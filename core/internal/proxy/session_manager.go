package proxy

import (
	"fmt"
	"sync"
	"time"
)

// SessionManager holds sticky session→proxy bindings for the "session" rotation
// method. It is a process-wide singleton so bindings survive the periodic
// rebuild of per-user PoolChains (which happens every ~60s on auth-cache expiry).
//
// A session is identified by a client-supplied token carried in the proxy
// username (e.g. "myuser-session-abc123" → token "abc123"). A binding is kept
// until one of the following happens:
//   - the client explicitly releases it (ReleaseToken / Release)
//   - it goes idle for longer than the pool's session TTL (reaped)
//   - the bound proxy is invalidated / fails (Evict, or rebind on next Select)
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*sessionEntry // key: "poolID:token"
	stop     chan struct{}
}

type sessionEntry struct {
	poolID    int
	token     string
	proxyID   int
	createdAt time.Time
	lastUsed  time.Time
	ttl       time.Duration
}

// SessionInfo is the externally-visible view of a live session binding.
type SessionInfo struct {
	PoolID    int       `json:"pool_id"`
	Token     string    `json:"token"`
	ProxyID   int       `json:"proxy_id"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewSessionManager creates a SessionManager and starts its idle-reaper.
func NewSessionManager() *SessionManager {
	m := &SessionManager{
		sessions: make(map[string]*sessionEntry),
		stop:     make(chan struct{}),
	}
	go m.reapLoop()
	return m
}

func sessionKey(poolID int, token string) string {
	return fmt.Sprintf("%d:%s", poolID, token)
}

// Get returns the proxy bound to (poolID, token) if a live binding exists,
// refreshing its idle timer. ok is false if there is no binding (or it expired).
func (m *SessionManager) Get(poolID int, token string) (proxyID int, ok bool) {
	if token == "" {
		return 0, false
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	e, exists := m.sessions[sessionKey(poolID, token)]
	if !exists {
		return 0, false
	}
	if now.Sub(e.lastUsed) > e.ttl {
		delete(m.sessions, sessionKey(poolID, token))
		return 0, false
	}
	e.lastUsed = now
	return e.proxyID, true
}

// Bind creates or replaces the binding for (poolID, token) → proxyID.
func (m *SessionManager) Bind(poolID int, token string, proxyID int, ttl time.Duration) {
	if token == "" {
		return
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	key := sessionKey(poolID, token)
	if e, ok := m.sessions[key]; ok {
		e.proxyID = proxyID
		e.lastUsed = now
		e.ttl = ttl
		return
	}
	m.sessions[key] = &sessionEntry{
		poolID:    poolID,
		token:     token,
		proxyID:   proxyID,
		createdAt: now,
		lastUsed:  now,
		ttl:       ttl,
	}
}

// Release drops the binding for (poolID, token). Returns true if one existed.
func (m *SessionManager) Release(poolID int, token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := sessionKey(poolID, token)
	if _, ok := m.sessions[key]; ok {
		delete(m.sessions, key)
		return true
	}
	return false
}

// ReleaseToken drops every binding matching token across all pools.
// Returns the number of bindings removed.
func (m *SessionManager) ReleaseToken(token string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key, e := range m.sessions {
		if e.token == token {
			delete(m.sessions, key)
			n++
		}
	}
	return n
}

// Evict drops every binding pointing at proxyID (used when a proxy is
// invalidated or fails). Bound sessions rebind to a fresh proxy on next use.
// Returns the number of bindings removed.
func (m *SessionManager) Evict(proxyID int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key, e := range m.sessions {
		if e.proxyID == proxyID {
			delete(m.sessions, key)
			n++
		}
	}
	return n
}

// List returns a snapshot of all live session bindings.
func (m *SessionManager) List() []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionInfo, 0, len(m.sessions))
	for _, e := range m.sessions {
		out = append(out, SessionInfo{
			PoolID:    e.poolID,
			Token:     e.token,
			ProxyID:   e.proxyID,
			CreatedAt: e.createdAt,
			LastUsed:  e.lastUsed,
			ExpiresAt: e.lastUsed.Add(e.ttl),
		})
	}
	return out
}

// reapLoop periodically removes idle (expired) sessions.
func (m *SessionManager) reapLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			m.mu.Lock()
			for key, e := range m.sessions {
				if now.Sub(e.lastUsed) > e.ttl {
					delete(m.sessions, key)
				}
			}
			m.mu.Unlock()
		case <-m.stop:
			return
		}
	}
}

// Stop terminates the reaper goroutine.
func (m *SessionManager) Stop() {
	close(m.stop)
}
