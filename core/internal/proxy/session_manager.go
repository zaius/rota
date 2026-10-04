package proxy

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

// ErrNoProxyAvailable signals temporary capacity exhaustion, including pools
// whose proxies are all reserved or on cooldown.
var ErrNoProxyAvailable = errors.New("no proxy available; wait and retry")

// SessionManager owns process-wide sticky bindings and exclusive reservations.
// A proxy can have only one session owner per scope, including when that proxy
// appears in several pools. Bindings survive per-user PoolChain rebuilds, and
// are released explicitly, on idle expiry, or when the proxy is evicted.
// Traffic through a binding's tunnels counts as use. A binding's tunnels end
// when it does, and when it moves off or cools the proxy they run through, so
// an open tunnel never carries a session over a proxy the session gave up.
type SessionManager struct {
	mu           sync.Mutex
	sessions     map[sessionIdentity]*sessionEntry
	reservations map[reservationKey]sessionIdentity
	cooldowns    map[reservationKey]models.ProxyScopeCooldown
	stop         chan struct{}
}

// Scope is an explicit client identifier or the normalized target hostname.
// Username prevents users reusing a token from sharing a binding accidentally.
type sessionIdentity struct {
	poolID   int
	username string
	token    string
	scope    string
}

type reservationKey struct {
	proxyID int
	scope   string
}

type sessionEntry struct {
	proxyID   int
	createdAt time.Time
	lastUsed  time.Time
	ttl       time.Duration
	tunnels   map[*sessionHold]struct{}
}

// lastActive returns the binding's latest selection or tunnel traffic.
func (e *sessionEntry) lastActive() time.Time {
	t := e.lastUsed
	for h := range e.tunnels {
		if a := time.Unix(0, h.lastTraffic.Load()); a.After(t) {
			t = a
		}
	}
	return t
}

// endTunnelsLocked ends the entry's tunnels that match.
func (e *sessionEntry) endTunnelsLocked(match func(*sessionHold) bool) {
	for h := range e.tunnels {
		if match(h) {
			h.endLocked()
		}
	}
}

// sessionHold ties one open tunnel to its binding. It records the proxy and
// normalized host the tunnel runs through, so a scope or domain cooldown on
// that proxy can end it.
type sessionHold struct {
	m           *SessionManager
	key         sessionIdentity
	entry       *sessionEntry
	proxyID     int
	host        string
	lastTraffic atomic.Int64 // UnixNano
	shutdown    func()       // guarded by m.mu
	ended       bool         // guarded by m.mu
}

// SessionInfo is the externally-visible view of a live session binding.
type SessionInfo struct {
	PoolID    int       `json:"pool_id"`
	Username  string    `json:"username"`
	Token     string    `json:"token"`
	Scope     string    `json:"scope"`
	ProxyID   int       `json:"proxy_id"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SessionFilter restricts control operations. Nil PoolIDs means all pools;
// an empty non-nil list matches none. Empty Username/Scope mean all values.
type SessionFilter struct {
	Token    string
	Username string
	Scope    string
	PoolIDs  []int
}

func (f SessionFilter) matches(key sessionIdentity) bool {
	if key.token != f.Token || (f.Username != "" && key.username != f.Username) || (f.Scope != "" && key.scope != f.Scope) {
		return false
	}
	if f.PoolIDs == nil {
		return true
	}
	for _, id := range f.PoolIDs {
		if id == key.poolID {
			return true
		}
	}
	return false
}

func (m *SessionManager) ReleaseSessions(filter SessionFilter) int {
	return m.releaseMatching(filter.matches)
}

func NewSessionManager() *SessionManager {
	m := &SessionManager{
		sessions:     make(map[sessionIdentity]*sessionEntry),
		reservations: make(map[reservationKey]sessionIdentity),
		cooldowns:    make(map[reservationKey]models.ProxyScopeCooldown),
		stop:         make(chan struct{}),
	}
	go m.reapLoop()
	return m
}

// selectProxy checks ownership, selects, and reserves under one lock shared by
// all pool selectors. choose must not call back into the manager. An empty
// token selects an unreserved proxy without creating a sticky binding.
func (m *SessionManager) selectProxy(key sessionIdentity, ttl time.Duration, choose func(boundID int, available func(int) bool) (*models.Proxy, error)) (*models.Proxy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	boundID := 0
	if e := m.liveLocked(key, now); e != nil && key.token != "" {
		boundID = e.proxyID
	}
	available := func(proxyID int) bool {
		if c, ok := m.cooldowns[reservationKey{proxyID, key.scope}]; ok && (c.Invalid || c.CooldownUntil.After(now)) {
			return false
		}
		owner, reserved := m.reservations[reservationKey{proxyID, key.scope}]
		if !reserved || m.liveLocked(owner, now) == nil {
			return true
		}
		return key.token != "" && owner == key
	}
	p, err := choose(boundID, available)
	if err != nil {
		return nil, err
	}
	if key.token == "" {
		return p, nil
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	e := m.sessions[key]
	if e == nil {
		e = &sessionEntry{createdAt: now}
	} else {
		delete(m.reservations, reservationKey{e.proxyID, key.scope})
		e.endTunnelsLocked(func(h *sessionHold) bool { return h.proxyID != p.ID })
	}
	e.proxyID, e.lastUsed, e.ttl = p.ID, now, ttl
	m.sessions[key] = e
	m.reservations[reservationKey{p.ID, key.scope}] = key
	return p, nil
}

func (m *SessionManager) liveLocked(key sessionIdentity, now time.Time) *sessionEntry {
	e := m.sessions[key]
	if e != nil && now.Sub(e.lastActive()) > e.ttl {
		m.deleteLocked(key)
		return nil
	}
	return e
}

// openTunnel attaches a tunnel through proxyID to key's live binding until the
// returned hold closes. It returns nil when key has no live binding, and a
// hold that has already ended when the binding moved off proxyID after
// selecting it.
func (m *SessionManager) openTunnel(key sessionIdentity, proxyID int, host string) *sessionHold {
	if key.token == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	e := m.liveLocked(key, now)
	if e == nil {
		return nil
	}
	h := &sessionHold{m: m, key: key, entry: e, proxyID: proxyID, host: host}
	h.lastTraffic.Store(now.UnixNano())
	if e.proxyID != proxyID {
		h.ended = true
		return h
	}
	if e.tunnels == nil {
		e.tunnels = make(map[*sessionHold]struct{})
	}
	e.tunnels[h] = struct{}{}
	return h
}

// touch records traffic through the tunnel. It does nothing on a nil hold.
func (h *sessionHold) touch() {
	if h != nil {
		h.lastTraffic.Store(time.Now().UnixNano())
	}
}

// onEnd sets how to shut the tunnel down when its binding ends it, and runs
// shutdown right away if that already happened.
func (h *sessionHold) onEnd(shutdown func()) {
	h.m.mu.Lock()
	h.shutdown = shutdown
	ended := h.ended
	h.m.mu.Unlock()
	if ended {
		shutdown()
	}
}

// endLocked detaches the tunnel from its binding and shuts it down. A tunnel
// without a shutdown yet ends as soon as onEnd sets one.
func (h *sessionHold) endLocked() {
	if h.ended {
		return
	}
	h.ended = true
	delete(h.entry.tunnels, h)
	if h.shutdown != nil {
		go h.shutdown()
	}
}

// close detaches the tunnel from its binding, whose idle clock then runs from
// the tunnel's last traffic. It does nothing if the binding already ended the
// tunnel.
func (h *sessionHold) close() {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if h.ended {
		return
	}
	delete(h.entry.tunnels, h)
	if t := time.Unix(0, h.lastTraffic.Load()); t.After(h.entry.lastUsed) {
		h.entry.lastUsed = t
	}
}

// deleteLocked drops the binding and ends its tunnels, which send the client
// back through selection instead of leaving it on a proxy that another session
// may now reserve.
func (m *SessionManager) deleteLocked(key sessionIdentity) {
	if e := m.sessions[key]; e != nil {
		delete(m.reservations, reservationKey{e.proxyID, key.scope})
		delete(m.sessions, key)
		e.endTunnelsLocked(func(*sessionHold) bool { return true })
	}
}

// Release drops the token's bindings in this pool, across users and scopes.
func (m *SessionManager) Release(poolID int, token string) bool {
	return m.releaseMatching(func(key sessionIdentity) bool {
		return key.poolID == poolID && key.token == token
	}) > 0
}

// ReleaseToken drops every binding matching token across all pools and scopes.
func (m *SessionManager) ReleaseToken(token string) int {
	return m.releaseMatching(func(key sessionIdentity) bool { return key.token == token })
}

// ReleaseTokenInPools restricts release to the proxy user's allowed pools.
func (m *SessionManager) ReleaseTokenInPools(token string, poolIDs []int) int {
	allowed := make(map[int]bool, len(poolIDs))
	for _, id := range poolIDs {
		allowed[id] = true
	}
	return m.releaseMatching(func(key sessionIdentity) bool {
		return key.token == token && allowed[key.poolID]
	})
}

func (m *SessionManager) releaseMatching(matches func(sessionIdentity) bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key := range m.sessions {
		if matches(key) {
			m.deleteLocked(key)
			n++
		}
	}
	return n
}

// FindByToken returns live bindings for the token across all pools and scopes.
func (m *SessionManager) FindByToken(token string) []SessionInfo {
	return m.listMatching(func(key sessionIdentity) bool { return key.token == token })
}

// Evict drops every binding pointing at proxyID. Sessions rebind on next use.
func (m *SessionManager) Evict(proxyID int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key, e := range m.sessions {
		if e.proxyID == proxyID {
			m.deleteLocked(key)
			n++
		}
	}
	return n
}

// EndDomainTunnels ends session tunnels through proxyID to domain or its
// subdomains. Their bindings stay and reselect on the next request there.
func (m *SessionManager) EndDomainTunnels(proxyID int, domain string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.sessions {
		e.endTunnelsLocked(func(h *sessionHold) bool {
			return h.proxyID == proxyID && hostMatchesDomain(h.host, domain)
		})
	}
}

func (m *SessionManager) List() []SessionInfo {
	return m.listMatching(func(sessionIdentity) bool { return true })
}

func (m *SessionManager) listMatching(matches func(sessionIdentity) bool) []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]SessionInfo, 0)
	for key := range m.sessions {
		e := m.liveLocked(key, now)
		if e == nil || !matches(key) {
			continue
		}
		lastActive := e.lastActive()
		out = append(out, SessionInfo{
			PoolID: key.poolID, Username: key.username, Token: key.token, Scope: key.scope,
			ProxyID: e.proxyID, CreatedAt: e.createdAt, LastUsed: lastActive,
			ExpiresAt: lastActive.Add(e.ttl),
		})
	}
	return out
}

func (m *SessionManager) reapLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			now := time.Now()
			for key := range m.sessions {
				m.liveLocked(key, now)
			}
			for key, c := range m.cooldowns {
				if !c.Invalid && c.FailureCount == 0 && !c.CooldownUntil.After(now) {
					delete(m.cooldowns, key)
				}
			}
			m.mu.Unlock()
		case <-m.stop:
			return
		}
	}
}

func (m *SessionManager) Stop() { close(m.stop) }
