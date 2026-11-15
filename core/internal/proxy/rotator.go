package proxy

import (
	"cmp"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zaius/rota/core/internal/models"
	"github.com/zaius/rota/core/internal/repository"
	"github.com/zaius/rota/core/pkg/logger"
	"github.com/zaius/rota/core/pkg/safeworker"
)

const (
	// failureThreshold is how many consecutive failures take a proxy out of
	// rotation until a health check passes.
	failureThreshold = 3

	// maxConnectRejections is how many distinct proxies must reject a CONNECT
	// target before ConnectWithRetry returns upstream_connect_rejected.
	maxConnectRejections = 2

	// maxAttempts caps the upstream attempts for one client request.
	maxAttempts = 5
)

// Rotator holds the process-wide list of selectable proxies and forwards
// requests through them, retrying on another proxy when one fails.
//
// It owns request recording: it is the only place that knows how long each
// individual attempt took, so per-proxy stats are charged per attempt, not per
// retry loop.
type Rotator struct {
	proxyRepo *repository.ProxyRepository
	sessions  *SessionManager
	domainCD  *DomainCooldownManager
	tracker   *UsageTracker
	logger    *logger.Logger

	// settings is swapped by ReloadSettings while requests read it.
	settings atomic.Pointer[models.RotationSettings]

	mu       sync.Mutex
	proxies  []*models.Proxy
	lastUsed map[int]uint64 // proxy ID → value of uses when last picked
	uses     uint64
	// failCounts tracks consecutive failures, so a single transient error
	// doesn't evict an otherwise-healthy proxy.
	failCounts map[int]int
}

// NewRotator creates an empty Rotator; Refresh loads its proxies.
func NewRotator(proxyRepo *repository.ProxyRepository, sessions *SessionManager, domainCD *DomainCooldownManager, tracker *UsageTracker, settings *models.RotationSettings, log *logger.Logger) *Rotator {
	r := &Rotator{
		proxyRepo:  proxyRepo,
		sessions:   sessions,
		domainCD:   domainCD,
		tracker:    tracker,
		logger:     log,
		lastUsed:   map[int]uint64{},
		failCounts: map[int]int{},
	}
	r.SetSettings(settings)
	return r
}

// SetSettings publishes new rotation settings.
func (r *Rotator) SetSettings(s *models.RotationSettings) {
	if s == nil {
		s = &models.RotationSettings{}
	}
	r.settings.Store(s)
}

func (r *Rotator) timeout() time.Duration {
	return time.Duration(cmp.Or(r.settings.Load().Timeout, 90)) * time.Second
}

func (r *Rotator) sessionTTL() time.Duration {
	return time.Duration(cmp.Or(r.settings.Load().SessionTTLMinutes, 10)) * time.Minute
}

// Refresh reloads the selectable proxies. Use order survives the reload, so
// least-recently-used rotation stays fair across refreshes.
func (r *Rotator) Refresh(ctx context.Context) error {
	proxies, err := r.proxyRepo.ListSelectable(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lastUsed := make(map[int]uint64, len(proxies))
	for _, p := range proxies {
		if n, ok := r.lastUsed[p.ID]; ok {
			lastUsed[p.ID] = n
		}
	}
	r.proxies, r.lastUsed = proxies, lastUsed
	return nil
}

// pick chooses a proxy for the request among those it has not tried and marks
// it used. A session keeps its bound proxy while that stays eligible. Otherwise
// the least recently used eligible proxy wins, which is round-robin for a
// single filter and stays fair when requests with different filters
// interleave.
func (r *Rotator) pick(ctx context.Context, tried map[int]bool) (*models.Proxy, error) {
	rt := routeFrom(ctx)
	r.mu.Lock()
	p, err := r.sessions.selectProxy(rt.sessionKey(), r.sessionTTL(), func(boundID int, available func(int) bool) (*models.Proxy, error) {
		var chosen *models.Proxy
		eligible := 0
		for _, p := range r.proxies {
			if tried[p.ID] || !rt.matches(p) || r.domainCD.IsCooled(p.ID, rt.host) || !available(p.ID) {
				continue
			}
			if p.ID == boundID {
				return p, nil
			}
			eligible++
			if rt.Random {
				if rand.IntN(eligible) == 0 {
					chosen = p
				}
			} else if chosen == nil || r.lastUsed[p.ID] < r.lastUsed[chosen.ID] {
				chosen = p
			}
		}
		if chosen == nil {
			return nil, ErrNoProxyAvailable
		}
		return chosen, nil
	})
	if err == nil {
		r.uses++
		r.lastUsed[p.ID] = r.uses
	}
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r.startCooldownRecovery(ctx, p.ID)
	return p, nil
}

// markFailed evicts a proxy once it fails failureThreshold times in a row, so
// one transient timeout doesn't drain healthy proxies. The streak lives only
// here; marking the proxy failed in the database keeps it out of later
// refreshes and restarts until a health check passes.
func (r *Rotator) markFailed(proxyID int) {
	r.mu.Lock()
	r.failCounts[proxyID]++
	evicted := r.failCounts[proxyID] >= failureThreshold
	if evicted {
		delete(r.failCounts, proxyID)
		r.removeLocked(proxyID)
	}
	r.mu.Unlock()
	if !evicted || r.proxyRepo == nil {
		return
	}
	safeworker.Go(r.logger, "mark_proxy_failed", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.proxyRepo.SetHealthy(ctx, proxyID, false); err != nil {
			r.logger.Error("failed to mark proxy failed", "proxy_id", proxyID, "error", err)
		}
	})
}

func (r *Rotator) markSucceeded(proxyID int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failCounts, proxyID)
}

// Remove drops a proxy from the list until a refresh brings it back.
func (r *Rotator) Remove(proxyID int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeLocked(proxyID)
}

func (r *Rotator) removeLocked(proxyID int) {
	r.proxies = slices.DeleteFunc(r.proxies, func(p *models.Proxy) bool { return p.ID == proxyID })
}

// record records one upstream attempt outcome. It only appends to the event
// log, so it runs inline: a goroutine could still be writing when shutdown
// closes the event store, losing the event.
func (r *Rotator) record(record RequestRecord) {
	if r.tracker == nil || record.ProxyID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.tracker.RecordRequest(ctx, record); err != nil {
		r.logger.Error("failed to record proxy request", "error", err)
	}
}

// recordAttempt records an attempt through p in request history. A nil cause
// is a success.
func (r *Rotator) recordAttempt(client string, p *models.Proxy, url, method string, status int, start time.Time, cause error) {
	record := RequestRecord{
		ProxyID:      p.ID,
		ProxyAddress: p.Address,
		Client:       client,
		RequestedURL: url,
		Method:       method,
		Success:      cause == nil,
		StatusCode:   status,
		ResponseTime: int(time.Since(start).Milliseconds()),
		Timestamp:    start,
	}
	if cause != nil {
		record.TargetFailure = isTargetConnectFailure(cause) || isClientRequestAbort(cause)
		record.ErrorMessage = cause.Error()
	}
	r.record(record)
}

// SendWithRetry forwards an HTTP request, trying a fresh proxy on each attempt.
func (r *Rotator) SendWithRetry(req *http.Request, ctx context.Context) (*http.Response, int, error) {
	client := routeFrom(ctx).Client
	followRedirect := r.settings.Load().FollowRedirect
	tried := make(map[int]bool)

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := clientRequestAbort(req, ctx, nil); err != nil {
			return nil, 0, err
		}
		p, err := r.pick(ctx, tried)
		if err != nil {
			if lastErr != nil {
				return nil, 0, fmt.Errorf("proxy attempts failed: %w", lastErr)
			}
			return nil, 0, err
		}
		tried[p.ID] = true
		start := time.Now()
		url := req.URL.String()

		r.logger.Info("trying proxy", "attempt", attempt+1, "max", maxAttempts, "proxy", p.Address)

		// The shared per-proxy transport reuses keep-alive connections across
		// attempts.
		transport, err := GetOrCreateTransport(p)
		if err != nil {
			// Building a transport fails on local configuration problems (an
			// unsupported protocol, a malformed address), not because the upstream
			// proxy misbehaved, so it doesn't count toward eviction.
			lastErr = forwardingFailure("proxy_configuration_error", err)
			r.recordAttempt(client, p, url, req.Method, 0, start, err)
			continue
		}

		httpClient := &http.Client{
			Transport: transport,
			Timeout:   r.timeout(),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if !followRedirect {
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

		resp, err := httpClient.Do(cloned)
		if err != nil {
			// When CheckRedirect rejects — the redirect limit above, for instance —
			// client.Do returns both a response and an error. That body still owns
			// a connection, and nothing downstream will close it.
			if resp != nil {
				resp.Body.Close()
			}
			if abort := clientRequestAbort(req, ctx, err); abort != nil {
				r.recordAttempt(client, p, url, req.Method, 0, start, abort)
				r.logger.Debug("client request aborted", "proxy", p.Address, "err", abort)
				return nil, 0, abort
			}
			r.recordAttempt(client, p, url, req.Method, 0, start, err)
			lastErr = fmt.Errorf("proxy %s attempt %d: %w", p.Address, attempt+1, err)
			r.logger.Warn("proxy failed", "proxy", p.Address, "err", err)
			r.markFailed(p.ID)
			continue
		}

		if resp.StatusCode == http.StatusProxyAuthRequired {
			resp.Body.Close()
			lastErr = forwardingFailure("upstream_proxy_auth_failed", fmt.Errorf("upstream proxy %s rejected authentication", p.Address))
			r.recordAttempt(client, p, url, req.Method, 0, start, lastErr)
			r.markFailed(p.ID)
			continue
		}

		r.markSucceeded(p.ID)
		r.recordAttempt(client, p, url, req.Method, resp.StatusCode, start, nil)
		r.logger.Info("proxy succeeded", "proxy", p.Address, "status", resp.StatusCode)
		return resp, p.ID, nil
	}

	return nil, 0, fmt.Errorf("all %d attempts failed, last: %w", maxAttempts, lastErr)
}

// TunnelBinding identifies the upstream proxy serving an established CONNECT
// tunnel and carries the dimensions needed to account for it.
//
// The tunnel outlives ConnectWithRetry — it is handed to the caller to pump
// bytes through — so the binding is what lets the caller attribute what happens
// next (requests seen inside it, bytes moved, how long it lived) back to the
// proxy and client that it served. Its traffic also keeps the request's session
// binding alive, and the binding ends the tunnel when it expires, is released,
// or gives up or cools the tunnel's proxy.
type TunnelBinding struct {
	ProxyID  int
	Address  string
	Host     string
	OpenedAt time.Time

	client  string
	rotator *Rotator
	session *sessionHold
}

// touch records traffic through the tunnel against its session binding.
func (b *TunnelBinding) touch() {
	if b != nil {
		b.session.touch()
	}
}

// EndWithSession shuts down both sides of the tunnel when its session binding
// ends it. Without a session binding it does nothing.
func (b *TunnelBinding) EndWithSession(client, upstream net.Conn) {
	if b == nil || b.session == nil {
		return
	}
	b.session.onEnd(func() {
		shutdownConn(client)
		shutdownConn(upstream)
	})
}

// Close detaches the tunnel from its session binding, which then expires one
// session TTL after the tunnel's last traffic unless used again. Later calls
// do nothing.
func (b *TunnelBinding) Close() {
	if b == nil || b.session == nil {
		return
	}
	b.session.close()
	b.session = nil
}

// RecordRequest records one HTTP request observed inside the tunnel. It is
// only ever called on the interception path — an opaque tunnel has no requests
// to see.
func (b *TunnelBinding) RecordRequest(method, url string, statusCode int, success bool, errMsg string, start time.Time) {
	if b == nil || b.rotator == nil {
		return
	}
	b.rotator.record(RequestRecord{
		ProxyID:      b.ProxyID,
		ProxyAddress: b.Address,
		Client:       b.client,
		RequestedURL: url,
		Method:       method,
		Success:      success,
		StatusCode:   statusCode,
		ResponseTime: int(time.Since(start).Milliseconds()),
		ErrorMessage: errMsg,
		Timestamp:    start,
	})
}

// RecordClose records the tunnel's lifetime volume once it has torn down.
// requests is 0 for an uninspected tunnel, where the payload is opaque.
func (b *TunnelBinding) RecordClose(counts TunnelCounts, requests int, cause error) {
	if b == nil || b.rotator == nil || b.rotator.tracker == nil || b.ProxyID <= 0 {
		return
	}

	record := TunnelRecord{
		ProxyID:      b.ProxyID,
		ProxyAddress: b.Address,
		Client:       b.client,
		Host:         b.Host,
		BytesUp:      counts.BytesUp,
		BytesDown:    counts.BytesDown,
		Requests:     requests,
		Duration:     time.Since(b.OpenedAt),
		OpenedAt:     b.OpenedAt,
	}
	if cause != nil {
		record.ErrorMessage = cause.Error()
	}

	// Recording a tunnel only queues an event, so it runs inline on the
	// tunnel's own goroutine, which has finished its work.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.rotator.tracker.RecordTunnel(ctx, record); err != nil {
		b.rotator.logger.Error("failed to record proxy tunnel", "error", err)
	}
}

// ConnectWithRetry establishes a TCP tunnel (HTTPS CONNECT) through a proxy.
// The returned binding attributes everything that subsequently happens in the
// tunnel back to the proxy that served it.
func (r *Rotator) ConnectWithRetry(host string, ctx context.Context) (net.Conn, *TunnelBinding, error) {
	if err := r.validateConnectTarget(ctx, host); err != nil {
		return nil, nil, err
	}
	rt := routeFrom(ctx)
	client := rt.Client
	tried := make(map[int]bool)

	var lastErr error
	rejections := 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, clientAbort(err)
		}
		p, err := r.pick(ctx, tried)
		if err != nil {
			if lastErr != nil {
				return nil, nil, fmt.Errorf("CONNECT attempts failed: %w", lastErr)
			}
			return nil, nil, err
		}
		tried[p.ID] = true
		start := time.Now()

		r.logger.Info("CONNECT: trying proxy", "attempt", attempt+1, "proxy", p.Address, "host", host)

		attemptCtx, cancel := context.WithTimeout(ctx, r.timeout())
		conn, err := dialTarget(attemptCtx, p, host)
		cancel()
		if err != nil && ctx.Err() != nil {
			// The client has gone: nobody waits for another attempt, and the
			// failure says nothing about the proxy.
			abort := clientAbort(ctx.Err())
			r.recordAttempt(client, p, "CONNECT://"+host, "CONNECT", 0, start, abort)
			return nil, nil, abort
		}
		if err != nil {
			r.recordAttempt(client, p, "CONNECT://"+host, "CONNECT", 0, start, err)
			lastErr = fmt.Errorf("CONNECT proxy %s attempt %d: %w", p.Address, attempt+1, err)
			if isTargetConnectFailure(err) {
				// One exit's rejection can mean the target blocks that IP or the
				// upstream blocks the domain, so a second exit confirms it. Neither
				// proxy takes a strike, and a session selecting the second proxy
				// rebinds to it.
				rejections++
				r.logger.Warn("CONNECT: target rejected", "proxy", p.Address, "host", host, "rejections", rejections, "err", err)
				if rejections >= maxConnectRejections {
					return nil, nil, lastErr
				}
				continue
			}
			r.logger.Warn("CONNECT: failed", "proxy", p.Address, "err", err)
			r.markFailed(p.ID)
			continue
		}

		r.markSucceeded(p.ID)
		r.recordAttempt(client, p, "CONNECT://"+host, "CONNECT", 200, start, nil)
		r.logger.Info("CONNECT: succeeded", "proxy", p.Address, "host", host)
		return conn, &TunnelBinding{
			ProxyID:  p.ID,
			Address:  p.Address,
			Host:     host,
			OpenedAt: start,
			client:   client,
			rotator:  r,
			session:  r.sessions.openTunnel(rt.sessionKey(), p.ID, rt.host),
		}, nil
	}

	return nil, nil, fmt.Errorf("all %d CONNECT attempts failed, last: %w", maxAttempts, lastErr)
}
