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
	db         *database.DB
	poolID     int
	method     string // roundrobin | random | stick | session
	stick      int    // stick_count
	sessionTTL time.Duration
	sessionMgr *SessionManager
	domainCD   *DomainCooldownManager

	mu          sync.Mutex
	proxies     []*models.Proxy
	rrIdx       int
	stickIdx    int
	stickServed int
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

// Refresh reloads the pool's active/idle member proxies.
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
	ps.mu.Unlock()
	return nil
}

// refreshQuery returns the SQL (and args) used to load the pool's members.
func (ps *PoolSelector) refreshQuery() (string, []any) {
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
	return ps.selectExcluding(ctx, nil, false)
}

// selectExcluding skips proxies already attempted by the request. forceSession
// preserves reservations when a session chain falls back to another pool mode.
func (ps *PoolSelector) selectExcluding(ctx context.Context, tried map[int]bool, forceSession bool) (*models.Proxy, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if len(ps.proxies) == 0 {
		return nil, fmt.Errorf("pool %d: %w", ps.poolID, ErrNoProxyAvailable)
	}

	host, _ := ctx.Value(TargetHostContextKey).(string)
	scope, _ := ctx.Value(SessionScopeContextKey).(string)
	if scope == "" {
		scope = normalizeHost(host)
	}
	key := sessionIdentity{poolID: ps.poolID, scope: scope}
	if chain, ok := chainFromContext(ctx); ok {
		key.username = chain.username
	}
	method := ps.method
	if forceSession {
		method = "session"
	}
	if method == "session" {
		key.token, _ = ctx.Value(SessionTokenContextKey).(string)
	}
	choose := func(boundID int, available func(int) bool) (*models.Proxy, error) {
		eligible := func(id int) bool {
			return !tried[id] && !ps.cooledForHost(id, host) && available(id)
		}
		if boundID != 0 {
			for _, p := range ps.proxies {
				if p.ID == boundID && eligible(p.ID) {
					return p, nil
				}
			}
		}
		return ps.selectLocked(method, eligible)
	}
	if ps.sessionMgr != nil {
		return ps.sessionMgr.selectProxy(key, ps.sessionTTL, choose)
	}
	return choose(0, func(int) bool { return true })
}

// selectLocked applies rotation among eligible proxies. Caller holds ps.mu.
func (ps *PoolSelector) selectLocked(method string, eligible func(int) bool) (*models.Proxy, error) {
	switch method {
	case "random":
		p := ps.proxies[rand.IntN(len(ps.proxies))]
		if eligible(p.ID) {
			return p, nil
		}
		var candidates []*models.Proxy
		for _, p := range ps.proxies {
			if eligible(p.ID) {
				candidates = append(candidates, p)
			}
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("pool %d: %w", ps.poolID, ErrNoProxyAvailable)
		}
		return candidates[rand.IntN(len(candidates))], nil

	case "stick":
		if ps.stick <= 0 {
			ps.stick = 10
		}
		p := ps.proxies[ps.stickIdx]
		if eligible(p.ID) {
			ps.stickServed++
			if ps.stickServed >= ps.stick {
				// advance to next proxy round-robin style
				ps.stickIdx = (ps.stickIdx + 1) % len(ps.proxies)
				ps.stickServed = 0
			}
			return p, nil
		}
		// The sticky proxy is unavailable for this request. Serve a
		// substitute eligible proxy for this request alone, without touching the
		// shared stickIdx/stickServed — other hosts keep the sticky proxy and its
		// serve count, and this host resumes it once the cooldown expires.
		for i := 1; i < len(ps.proxies); i++ {
			cand := ps.proxies[(ps.stickIdx+i)%len(ps.proxies)]
			if eligible(cand.ID) {
				return cand, nil
			}
		}
		return nil, fmt.Errorf("pool %d: %w", ps.poolID, ErrNoProxyAvailable)

	default: // roundrobin, or a new/unbound session
		return ps.nextRoundRobinLocked(eligible)
	}
}

// nextRoundRobinLocked returns the next eligible proxy. Caller holds ps.mu.
func (ps *PoolSelector) nextRoundRobinLocked(eligible func(int) bool) (*models.Proxy, error) {
	for range ps.proxies {
		p := ps.proxies[ps.rrIdx]
		ps.rrIdx = (ps.rrIdx + 1) % len(ps.proxies)
		if eligible(p.ID) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("pool %d: %w", ps.poolID, ErrNoProxyAvailable)
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
