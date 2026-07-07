package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// PoolChain holds an ordered list of pool selectors for a user:
// index 0 = main pool, index 1..N = fallback pools.
// It refreshes pool selectors periodically and provides the high-level
// SendWithRetry / ConnectWithRetry methods used by the proxy handler.
//
// The chain owns all request recording: it is the only place that knows which
// pool served an attempt, which user the chain belongs to, and how long each
// individual attempt took — so per-proxy stats are charged per attempt, not
// per retry loop.
type PoolChain struct {
	selectors []*PoolSelector
	tracker   *UsageTracker
	logger    *logger.Logger
	maxRetry  int    // total upstream attempts across all pools
	username  string // proxy user this chain serves; "" for the default chain
}

// NewPoolChain builds a PoolChain from an ordered list of ProxyPool objects
// for the named proxy user.
func NewPoolChain(db *database.DB, pools []models.ProxyPool, username string, maxRetry int, sessionMgr *SessionManager, domainCD *DomainCooldownManager, tracker *UsageTracker, log *logger.Logger) *PoolChain {
	selectors := make([]*PoolSelector, 0, len(pools))
	for _, p := range pools {
		selectors = append(selectors, NewPoolSelector(db, p, sessionMgr, domainCD))
	}
	return &PoolChain{
		selectors: selectors,
		tracker:   tracker,
		logger:    log,
		maxRetry:  maxRetry,
		username:  username,
	}
}

// poolID returns the pool ID behind selector index selIdx (0 for the default
// pool or an out-of-range index).
func (c *PoolChain) poolID(selIdx int) int {
	if selIdx >= 0 && selIdx < len(c.selectors) {
		return c.selectors[selIdx].poolID
	}
	return 0
}

// recordAttempt asynchronously records one upstream attempt outcome.
func (c *PoolChain) recordAttempt(record RequestRecord) {
	if c.tracker == nil || record.ProxyID <= 0 {
		return
	}
	record.Username = c.username
	go func() {
		recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.tracker.RecordRequest(recordCtx, record); err != nil {
			c.logger.Error("failed to record proxy request", "error", err)
		}
	}()
}

// recordFailure records a failed upstream attempt. Without this the pool path
// would only ever persist successes, so proxy failure stats and the
// consecutive-failure auto-disable threshold (see updateProxyStats) would
// never advance for pooled proxies — a dead proxy would be retried forever.
func (c *PoolChain) recordFailure(selIdx, proxyID int, address, url, method string, attemptStart time.Time, cause error) {
	record := RequestRecord{
		ProxyID:      proxyID,
		ProxyAddress: address,
		PoolID:       c.poolID(selIdx),
		RequestedURL: url,
		Method:       method,
		Success:      false,
		ResponseTime: int(time.Since(attemptStart).Milliseconds()),
		Timestamp:    attemptStart,
	}
	if cause != nil {
		record.ErrorMessage = cause.Error()
	}
	c.recordAttempt(record)
}

// recordSuccess records a successful upstream attempt.
func (c *PoolChain) recordSuccess(selIdx, proxyID int, address, url, method string, statusCode int, attemptStart time.Time) {
	c.recordAttempt(RequestRecord{
		ProxyID:      proxyID,
		ProxyAddress: address,
		PoolID:       c.poolID(selIdx),
		RequestedURL: url,
		Method:       method,
		Success:      true,
		StatusCode:   statusCode,
		ResponseTime: int(time.Since(attemptStart).Milliseconds()),
		Timestamp:    attemptStart,
	})
}

// defaultPoolMethod maps a global rotation method onto the selector methods the
// default pool supports, preserving the legacy global selector's fallbacks:
// session/stick have no global session context, so they degrade to round-robin,
// and an unknown method defaults to random.
func defaultPoolMethod(method string) string {
	switch method {
	case "random":
		return "random"
	case "roundrobin", "round-robin":
		return "roundrobin"
	case "least_conn", "least-conn", "least_connections":
		return "least_conn"
	case "time_based", "time-based":
		return "time_based"
	case "session", "stick", "sticky":
		return "roundrobin"
	default:
		return "random"
	}
}

// NewDefaultPoolChain builds the chain used for requests that do not map to a
// proxy user: a single selector over every active proxy, honouring the global
// rotation method and filters. It replaces the legacy global selector engine.
func NewDefaultPoolChain(db *database.DB, settings *models.RotationSettings, sessionMgr *SessionManager, domainCD *DomainCooldownManager, tracker *UsageTracker, log *logger.Logger) *PoolChain {
	sel := &PoolSelector{
		db:               db,
		poolID:           0,
		method:           defaultPoolMethod(settings.Method),
		timeInterval:     time.Duration(settings.TimeBased.Interval) * time.Second,
		sessionMgr:       sessionMgr,
		domainCD:         domainCD,
		loadAll:          true,
		allowedProtocols: settings.AllowedProtocols,
		maxResponseTime:  settings.MaxResponseTime,
		minSuccessRate:   settings.MinSuccessRate,
	}
	maxRetry := 5
	if settings.FallbackMaxRetries > 0 {
		maxRetry = settings.FallbackMaxRetries
	}
	if !settings.Fallback {
		maxRetry = 1
	}
	return &PoolChain{
		selectors: []*PoolSelector{sel},
		tracker:   tracker,
		logger:    log,
		maxRetry:  maxRetry,
	}
}

// Refresh reloads all pool selectors (non-blocking goroutine).
func (c *PoolChain) Refresh(ctx context.Context) {
	var wg sync.WaitGroup
	for _, sel := range c.selectors {
		sel := sel
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sel.Refresh(ctx); err != nil {
				c.logger.Warn("pool selector refresh failed", "pool_id", sel.poolID, "error", err)
			}
		}()
	}
	wg.Wait()
}

// pickProxy iterates through pool selectors until it finds an active proxy
// that hasn't been tried yet. Returns (proxy, selectorIndex).
func (c *PoolChain) pickProxy(ctx context.Context, tried map[int]bool) (*models.Proxy, int, error) {
	for i, sel := range c.selectors {
		if !sel.HasActive() {
			continue
		}
		// Try up to len(proxies) times to find an untried one in this pool
		for attempt := 0; attempt < 10; attempt++ {
			p, err := sel.Select(ctx)
			if err != nil {
				break
			}
			if !tried[p.ID] {
				return p, i, nil
			}
		}
	}
	return nil, -1, fmt.Errorf("no untried proxies available across all pools")
}

// markFailed removes the proxy from its pool's in-memory list so it won't be
// re-selected in this chain's lifecycle (until next Refresh).
func (c *PoolChain) markFailed(selIdx int, proxyID int) {
	if selIdx >= 0 && selIdx < len(c.selectors) {
		c.selectors[selIdx].RemoveProxy(proxyID)
	}
}

// EvictProxy removes a proxy from every pool selector in this chain.
func (c *PoolChain) EvictProxy(proxyID int) {
	for _, sel := range c.selectors {
		sel.RemoveProxy(proxyID)
	}
}

// SendWithRetry attempts to forward an HTTP request through the chain.
// On each attempt it picks the next fresh proxy. If a pool has no active proxies
// it moves to the next pool automatically.
func (c *PoolChain) SendWithRetry(
	req *http.Request,
	ctx context.Context,
	rotationSettings *models.RotationSettings,
	log *logger.Logger,
) (*http.Response, int, error) {
	tried := make(map[int]bool)
	maxAttempts := c.maxRetry
	if maxAttempts <= 0 {
		maxAttempts = 5
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		selectedProxy, selIdx, err := c.pickProxy(ctx, tried)
		if err != nil {
			return nil, 0, fmt.Errorf("no proxy available: %w", lastErr)
		}
		tried[selectedProxy.ID] = true
		attemptStart := time.Now()

		log.Info("pool chain: trying proxy",
			"attempt", attempt+1,
			"max", maxAttempts,
			"pool_idx", selIdx,
			"proxy", selectedProxy.Address,
		)

		// Use the shared per-proxy transport cache so keep-alive connections are
		// reused across attempts instead of building a fresh connection pool each
		// time (the legacy path already did this).
		transport, err := GetOrCreateTransport(selectedProxy)
		if err != nil {
			lastErr = err
			c.recordFailure(selIdx, selectedProxy.ID, selectedProxy.Address, req.URL.String(), req.Method, attemptStart, err)
			c.markFailed(selIdx, selectedProxy.ID)
			continue
		}

		timeout := 90
		if rotationSettings != nil && rotationSettings.Timeout > 0 {
			timeout = rotationSettings.Timeout
		}

		client := &http.Client{
			Transport: transport,
			Timeout:   time.Duration(timeout) * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if rotationSettings != nil && !rotationSettings.FollowRedirect {
					return http.ErrUseLastResponse
				}
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				return nil
			},
		}

		cloned := req.Clone(ctx)
		cloned.RequestURI = ""

		resp, err := client.Do(cloned)
		if err != nil {
			c.recordFailure(selIdx, selectedProxy.ID, selectedProxy.Address, req.URL.String(), req.Method, attemptStart, err)
			lastErr = fmt.Errorf("proxy %s attempt %d: %w", selectedProxy.Address, attempt+1, err)
			log.Warn("pool chain: proxy failed", "proxy", selectedProxy.Address, "err", err)
			c.markFailed(selIdx, selectedProxy.ID)
			continue
		}

		c.recordSuccess(selIdx, selectedProxy.ID, selectedProxy.Address, req.URL.String(), req.Method, resp.StatusCode, attemptStart)
		log.Info("pool chain: success",
			"proxy", selectedProxy.Address,
			"status", resp.StatusCode,
		)
		return resp, selectedProxy.ID, nil
	}

	return nil, 0, fmt.Errorf("all %d attempts failed, last: %w", maxAttempts, lastErr)
}

// ConnectWithRetry establishes a TCP tunnel (HTTPS CONNECT) through the chain.
func (c *PoolChain) ConnectWithRetry(
	host string,
	ctx context.Context,
	rotationSettings *models.RotationSettings,
	log *logger.Logger,
) (net.Conn, int, error) {
	tried := make(map[int]bool)
	maxAttempts := c.maxRetry
	if maxAttempts <= 0 {
		maxAttempts = 5
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		selectedProxy, selIdx, err := c.pickProxy(ctx, tried)
		if err != nil {
			return nil, 0, fmt.Errorf("no proxy available: %w", lastErr)
		}
		tried[selectedProxy.ID] = true
		attemptStart := time.Now()

		log.Info("pool chain CONNECT: trying proxy",
			"attempt", attempt+1,
			"proxy", selectedProxy.Address,
			"host", host,
		)

		conn, err := connectViaProxyStandalone(selectedProxy, host, rotationSettings)
		if err != nil {
			c.recordFailure(selIdx, selectedProxy.ID, selectedProxy.Address, "CONNECT://"+host, "CONNECT", attemptStart, err)
			lastErr = fmt.Errorf("CONNECT proxy %s attempt %d: %w", selectedProxy.Address, attempt+1, err)
			log.Warn("pool chain CONNECT: failed", "proxy", selectedProxy.Address, "err", err)
			c.markFailed(selIdx, selectedProxy.ID)
			continue
		}

		c.recordSuccess(selIdx, selectedProxy.ID, selectedProxy.Address, "CONNECT://"+host, "CONNECT", 200, attemptStart)
		log.Info("pool chain CONNECT: success", "proxy", selectedProxy.Address, "host", host)
		return conn, selectedProxy.ID, nil
	}

	return nil, 0, fmt.Errorf("all %d CONNECT attempts failed, last: %w", maxAttempts, lastErr)
}

// connectViaProxyStandalone dials host through the given proxy for a CONNECT
// tunnel, dispatching by protocol. It is the sole CONNECT dial path.
func connectViaProxyStandalone(p *models.Proxy, host string, settings *models.RotationSettings) (net.Conn, error) {
	timeout := 90 * time.Second
	if settings != nil && settings.Timeout > 0 {
		timeout = time.Duration(settings.Timeout) * time.Second
	}
	if timeout < 30*time.Second {
		timeout = 30 * time.Second
	}

	switch p.Protocol {
	case "socks5":
		return connectViaSocks5(p, host)
	case "socks4", "socks4a":
		return connectViaSocks5(p, host) // h12.io/socks handles socks4 too; close enough
	case "http", "https":
		return connectViaHTTPStandalone(p, host, timeout)
	default:
		return nil, fmt.Errorf("unsupported protocol for CONNECT: %s", p.Protocol)
	}
}
