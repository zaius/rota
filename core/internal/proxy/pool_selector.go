package proxy

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
)

// PoolSelector selects a proxy from a specific pool using the pool's rotation strategy.
// It keeps in-memory state (round-robin index, stick counters) per pool instance.
type PoolSelector struct {
	db           *database.DB
	poolID       int
	method       string // roundrobin | random | stick | session | least_conn | time_based
	stick        int    // stick_count
	sessionTTL   time.Duration
	timeInterval time.Duration // time_based interval (0 → default 120s)
	sessionMgr   *SessionManager
	domainCD     *DomainCooldownManager

	// Default-pool mode: when loadAll is true the selector is not scoped to a
	// pool_proxies membership but draws from every active proxy, applying the
	// global rotation filters below. This backs the no-proxy-user request path.
	loadAll          bool
	allowedProtocols []string
	maxResponseTime  int
	minSuccessRate   float64

	mu          sync.Mutex
	proxies     []*models.Proxy
	rrIdx       int
	stickIdx    int
	stickServed int

	// useCounts tracks how often each proxy was selected by this selector
	// since process start. least_conn balances on it — an in-memory
	// approximation of current load, instead of the lifetime request totals
	// it used to read from the database (which are now event-window-derived
	// and refreshed out-of-band).
	useCounts map[int]int64
}

// NewPoolSelector creates a PoolSelector for the given pool.
func NewPoolSelector(db *database.DB, pool models.ProxyPool, sessionMgr *SessionManager, domainCD *DomainCooldownManager) *PoolSelector {
	ttl := pool.SessionTTLMinutes
	if ttl < 1 {
		ttl = 10
	}
	return &PoolSelector{
		db:         db,
		poolID:     pool.ID,
		method:     pool.RotationMethod,
		stick:      pool.StickCount,
		sessionTTL: time.Duration(ttl) * time.Minute,
		sessionMgr: sessionMgr,
		domainCD:   domainCD,
	}
}

// Refresh reloads the active/idle proxies this selector draws from — either the
// pool's members, or (in default-pool mode) every active proxy that passes the
// global rotation filters.
func (ps *PoolSelector) Refresh(ctx context.Context) error {
	query, args := ps.refreshQuery()
	rows, err := ps.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("pool selector refresh: %w", err)
	}
	defer rows.Close()

	var proxies []*models.Proxy
	for rows.Next() {
		var p models.Proxy
		err := rows.Scan(
			&p.ID, &p.Address, &p.Protocol, &p.Username, &p.Password,
			&p.Status, &p.Requests, &p.SuccessfulRequests, &p.FailedRequests,
			&p.AvgResponseTime, &p.LastCheck, &p.LastError, &p.CreatedAt, &p.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("pool selector scan: %w", err)
		}
		if ps.loadAll && !ps.passesFilters(&p) {
			continue
		}
		proxies = append(proxies, &p)
	}

	ps.mu.Lock()
	ps.proxies = proxies
	// fix out-of-bounds indices after refresh
	if ps.rrIdx >= len(proxies) {
		ps.rrIdx = 0
	}
	if ps.stickIdx >= len(proxies) {
		ps.stickIdx = 0
		ps.stickServed = 0
	}
	// drop usage counts for proxies that left the set, so the map doesn't
	// accumulate dead IDs across refreshes
	if ps.useCounts != nil {
		current := make(map[int]bool, len(proxies))
		for _, p := range proxies {
			current[p.ID] = true
		}
		for id := range ps.useCounts {
			if !current[id] {
				delete(ps.useCounts, id)
			}
		}
	}
	ps.mu.Unlock()
	return nil
}

// refreshQuery returns the SQL (and args) used to load this selector's proxies:
// all active proxies in default-pool mode, otherwise the pool's members.
func (ps *PoolSelector) refreshQuery() (string, []any) {
	if ps.loadAll {
		return `
			SELECT id, address, protocol, username, password,
			       status, requests, successful_requests, failed_requests,
			       avg_response_time, last_check, last_error, created_at, updated_at
			FROM proxies
			WHERE status IN ('active', 'idle')
			  AND (cooldown_until IS NULL OR cooldown_until < NOW())
			ORDER BY address
		`, nil
	}
	return `
		SELECT p.id, p.address, p.protocol, p.username, p.password,
		       p.status, p.requests, p.successful_requests, p.failed_requests,
		       p.avg_response_time, p.last_check, p.last_error, p.created_at, p.updated_at
		FROM proxies p
		JOIN pool_proxies pp ON pp.proxy_id = p.id
		WHERE pp.pool_id = $1
		  AND p.status IN ('active', 'idle')
		  AND (p.cooldown_until IS NULL OR p.cooldown_until < NOW())
		ORDER BY p.id
	`, []any{ps.poolID}
}

// passesFilters applies the global rotation filters (default-pool mode only).
func (ps *PoolSelector) passesFilters(p *models.Proxy) bool {
	if len(ps.allowedProtocols) > 0 {
		allowed := false
		for _, proto := range ps.allowedProtocols {
			if p.Protocol == proto {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	if ps.maxResponseTime > 0 && p.AvgResponseTime > ps.maxResponseTime {
		return false
	}
	if ps.minSuccessRate > 0 && p.Requests > 0 {
		rate := float64(p.SuccessfulRequests) / float64(p.Requests) * 100
		if rate < ps.minSuccessRate {
			return false
		}
	}
	return true
}

// HasActive returns true if the pool currently has at least one active/idle proxy.
func (ps *PoolSelector) HasActive() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(ps.proxies) > 0
}

// Select picks the next proxy according to the pool's rotation method.
// When the context carries the request's target host (TargetHostContextKey),
// proxies on a domain cooldown for that host are skipped — they remain
// selectable for other targets.
func (ps *PoolSelector) Select(ctx context.Context) (*models.Proxy, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if len(ps.proxies) == 0 {
		return nil, fmt.Errorf("pool %d has no active proxies", ps.poolID)
	}

	host, _ := ctx.Value(TargetHostContextKey).(string)

	p, err := ps.selectLocked(ctx, host)
	if err != nil {
		return nil, err
	}
	if ps.useCounts == nil {
		ps.useCounts = make(map[int]int64)
	}
	ps.useCounts[p.ID]++
	return p, nil
}

// selectLocked dispatches to the pool's rotation method. Caller must hold
// ps.mu.
func (ps *PoolSelector) selectLocked(ctx context.Context, host string) (*models.Proxy, error) {
	switch ps.method {
	case "random":
		// Load balancing is not a security decision, so the non-cryptographic
		// generator is the right tool here: it needs no syscall, cannot fail, and
		// keeps the selection path allocation-free. Select() guarantees the slice
		// is non-empty, so IntN cannot be called with zero.
		p := ps.proxies[rand.IntN(len(ps.proxies))]
		if !ps.cooledForHost(p.ID, host) {
			return p, nil
		}
		// Re-draw among the proxies still eligible for this host.
		var eligible []*models.Proxy
		for _, c := range ps.proxies {
			if !ps.cooledForHost(c.ID, host) {
				eligible = append(eligible, c)
			}
		}
		if len(eligible) == 0 {
			return nil, fmt.Errorf("pool %d has no proxies available for host %q", ps.poolID, host)
		}
		return eligible[rand.IntN(len(eligible))], nil

	case "session":
		// Bind a proxy to the client's session token and hold it until the
		// session is released, goes idle (TTL), or its proxy disappears.
		token, _ := ctx.Value(SessionTokenContextKey).(string)
		if token == "" || ps.sessionMgr == nil {
			// No session token supplied → behave like round-robin.
			return ps.nextRoundRobinLocked(host)
		}
		if boundID, ok := ps.sessionMgr.Get(ps.poolID, token); ok {
			for _, p := range ps.proxies {
				if p.ID == boundID {
					if !ps.cooledForHost(p.ID, host) {
						return p, nil
					}
					// Bound proxy is on a domain cooldown for this host —
					// rebind below; the whole session moves to a fresh proxy.
					break
				}
			}
			// Bound proxy no longer available (failed/invalidated/cooled down) —
			// fall through to rebind to a fresh one.
		}
		p, err := ps.nextRoundRobinLocked(host)
		if err != nil {
			return nil, err
		}
		ps.sessionMgr.Bind(ps.poolID, token, p.ID, ps.sessionTTL)
		return p, nil

	case "stick":
		if ps.stick <= 0 {
			ps.stick = 10
		}
		p := ps.proxies[ps.stickIdx]
		if !ps.cooledForHost(p.ID, host) {
			ps.stickServed++
			if ps.stickServed >= ps.stick {
				// advance to next proxy round-robin style
				ps.stickIdx = (ps.stickIdx + 1) % len(ps.proxies)
				ps.stickServed = 0
			}
			return p, nil
		}
		// The sticky proxy is on a domain cooldown for this host only. Serve a
		// substitute eligible proxy for this request alone, WITHOUT touching the
		// shared stickIdx/stickServed — other hosts keep the sticky proxy and its
		// serve count, and this host resumes it once the cooldown expires.
		for i := 1; i < len(ps.proxies); i++ {
			cand := ps.proxies[(ps.stickIdx+i)%len(ps.proxies)]
			if !ps.cooledForHost(cand.ID, host) {
				return cand, nil
			}
		}
		return nil, fmt.Errorf("pool %d has no proxies available for host %q", ps.poolID, host)

	case "least_conn", "least-conn", "least_connections":
		// Pick the eligible proxy this selector has used least (see
		// useCounts) — current-process load, not lifetime totals.
		var best *models.Proxy
		var bestCount int64
		for _, p := range ps.proxies {
			if ps.cooledForHost(p.ID, host) {
				continue
			}
			if c := ps.useCounts[p.ID]; best == nil || c < bestCount {
				best, bestCount = p, c
			}
		}
		if best == nil {
			return nil, fmt.Errorf("pool %d has no proxies available for host %q", ps.poolID, host)
		}
		return best, nil

	case "time_based", "time-based":
		// Rotate to a new proxy every interval; all requests in the same window
		// map to the same proxy. Cooled proxies are excluded from the window set.
		interval := ps.timeInterval
		if interval <= 0 {
			interval = 120 * time.Second
		}
		var eligible []*models.Proxy
		for _, p := range ps.proxies {
			if !ps.cooledForHost(p.ID, host) {
				eligible = append(eligible, p)
			}
		}
		if len(eligible) == 0 {
			return nil, fmt.Errorf("pool %d has no proxies available for host %q", ps.poolID, host)
		}
		idx := int(time.Now().Unix()/int64(interval.Seconds())) % len(eligible)
		return eligible[idx], nil

	default: // roundrobin
		return ps.nextRoundRobinLocked(host)
	}
}

// nextRoundRobinLocked returns the next proxy in round-robin order, skipping
// proxies on a domain cooldown for host. Caller must hold ps.mu.
func (ps *PoolSelector) nextRoundRobinLocked(host string) (*models.Proxy, error) {
	for range ps.proxies {
		p := ps.proxies[ps.rrIdx]
		ps.rrIdx = (ps.rrIdx + 1) % len(ps.proxies)
		if !ps.cooledForHost(p.ID, host) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("pool %d has no proxies available for host %q", ps.poolID, host)
}

// cooledForHost reports whether a proxy is on a domain cooldown covering host.
func (ps *PoolSelector) cooledForHost(proxyID int, host string) bool {
	return host != "" && ps.domainCD != nil && ps.domainCD.IsCooled(proxyID, host)
}

// RemoveProxy removes a specific proxy from the in-memory list (called after failure).
func (ps *PoolSelector) RemoveProxy(proxyID int) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	filtered := ps.proxies[:0]
	for _, p := range ps.proxies {
		if p.ID != proxyID {
			filtered = append(filtered, p)
		}
	}
	ps.proxies = filtered

	// fix indices
	n := len(ps.proxies)
	if n == 0 {
		ps.rrIdx = 0
		ps.stickIdx = 0
		ps.stickServed = 0
	} else {
		if ps.rrIdx >= n {
			ps.rrIdx = 0
		}
		if ps.stickIdx >= n {
			ps.stickIdx = 0
			ps.stickServed = 0
		}
	}
}
