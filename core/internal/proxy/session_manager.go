package proxy

import (
	"errors"
	"sync"
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
	}
	e.proxyID, e.lastUsed, e.ttl = p.ID, now, ttl
	m.sessions[key] = e
	m.reservations[reservationKey{p.ID, key.scope}] = key
	return p, nil
}

func (m *SessionManager) liveLocked(key sessionIdentity, now time.Time) *sessionEntry {
	e := m.sessions[key]
	if e != nil && now.Sub(e.lastUsed) > e.ttl {
		m.deleteLocked(key)
		return nil
	}
	return e
}

func (m *SessionManager) deleteLocked(key sessionIdentity) {
	if e := m.sessions[key]; e != nil {
		delete(m.reservations, reservationKey{e.proxyID, key.scope})
		delete(m.sessions, key)
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
		out = append(out, SessionInfo{
			PoolID: key.poolID, Username: key.username, Token: key.token, Scope: key.scope,
			ProxyID: e.proxyID, CreatedAt: e.createdAt, LastUsed: e.lastUsed,
			ExpiresAt: e.lastUsed.Add(e.ttl),
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
