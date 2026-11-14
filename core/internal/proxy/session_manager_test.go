package proxy

import (
	"testing"
	"time"

	"github.com/zaius/rota/core/internal/models"
)

func reserveForTest(m *SessionManager, key sessionIdentity, proxyID int, ttl time.Duration) error {
	_, err := m.selectProxy(key, ttl, func(_ int, available func(int) bool) (*models.Proxy, error) {
		if !available(proxyID) {
			return nil, ErrNoProxyAvailable
		}
		return &models.Proxy{ID: proxyID}, nil
	})
	return err
}

func mustReserve(t *testing.T, m *SessionManager, key sessionIdentity, proxyID int, ttl time.Duration) {
	t.Helper()
	if err := reserveForTest(m, key, proxyID, ttl); err != nil {
		t.Fatal(err)
	}
}

// Regression: requests over a kept-alive CONNECT tunnel never refreshed the
// binding, so it expired one TTL after the CONNECT with traffic still flowing,
// and the client's invalidate then found no session.
func TestSessionManager_TunnelTrafficHoldsBinding(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()
	key := sessionIdentity{token: "a", scope: "example.com"}
	mustReserve(t, m, key, 42, time.Minute)
	hold := m.openTunnel(key, 42, "example.com")
	idle := func(d time.Duration) {
		m.mu.Lock()
		m.sessions[key].lastUsed = time.Now().Add(-d)
		m.mu.Unlock()
		hold.lastTraffic.Store(time.Now().Add(-d).UnixNano())
	}

	idle(time.Hour)
	hold.touch()
	if got := m.FindSessions(SessionFilter{Token: "a"}); len(got) != 1 || time.Until(got[0].ExpiresAt) < 59*time.Second {
		t.Fatalf("binding expired with traffic on its tunnel: %+v", got)
	}
	idle(30 * time.Second)
	hold.close()
	if got := m.FindSessions(SessionFilter{Token: "a"}); len(got) != 1 || time.Since(got[0].LastUsed) < 29*time.Second {
		t.Fatalf("closed tunnel's idle clock did not run from its last traffic: %+v", got)
	}
	m.mu.Lock()
	m.sessions[key].lastUsed = time.Now().Add(-time.Minute - time.Second)
	m.mu.Unlock()
	if got := m.FindSessions(SessionFilter{Token: "a"}); len(got) != 0 {
		t.Fatalf("binding outlived its TTL after the tunnel closed: %+v", got)
	}
}

// A tunnel idle for the whole TTL must not keep its proxy: the binding expires
// and the tunnel ends.
func TestSessionManager_IdleTunnelEndsWithBinding(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()
	key := sessionIdentity{token: "a", scope: "example.com"}
	mustReserve(t, m, key, 42, time.Minute)
	hold := m.openTunnel(key, 42, "example.com")
	ended := make(chan struct{})
	hold.onEnd(func() { close(ended) })

	m.mu.Lock()
	m.sessions[key].lastUsed = time.Now().Add(-2 * time.Minute)
	m.mu.Unlock()
	hold.lastTraffic.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	if got := m.FindSessions(SessionFilter{Token: "a"}); len(got) != 0 {
		t.Fatalf("idle tunnel kept its binding alive: %+v", got)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("binding expiry did not end its idle tunnel")
	}
	hold.close()
	key.token = "b"
	mustReserve(t, m, key, 42, time.Minute) // the proxy was freed
}

// Regression: a client that keeps its CONNECT tunnel open went on reusing the
// old proxy after invalidating or releasing its session, while fresh
// connections rebound to a new one.
func TestSessionManager_TunnelEndsWhenSessionLeavesProxy(t *testing.T) {
	future := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name   string
		action func(t *testing.T, m *SessionManager, key sessionIdentity)
		ends   bool
	}{
		{"release", func(_ *testing.T, m *SessionManager, _ sessionIdentity) { m.ReleaseSessions(SessionFilter{Token: "a"}) }, true},
		{"evict", func(_ *testing.T, m *SessionManager, _ sessionIdentity) { m.Evict(42) }, true},
		{"scope_cooldown", func(_ *testing.T, m *SessionManager, _ sessionIdentity) {
			m.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 42, Scope: "example.com", CooldownUntil: future})
		}, true},
		{"scope_exclusion", func(_ *testing.T, m *SessionManager, _ sessionIdentity) {
			m.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 42, Scope: "example.com", Invalid: true})
		}, true},
		{"domain_cooldown", func(_ *testing.T, m *SessionManager, _ sessionIdentity) { m.EndDomainTunnels(42, "example.com") }, true},
		{"parent_domain_cooldown", func(_ *testing.T, m *SessionManager, _ sessionIdentity) { m.EndDomainTunnels(42, "com") }, true},
		{"rebind", func(t *testing.T, m *SessionManager, key sessionIdentity) { mustReserve(t, m, key, 43, time.Minute) }, true},
		{"reselect_same_proxy", func(t *testing.T, m *SessionManager, key sessionIdentity) { mustReserve(t, m, key, 42, time.Minute) }, false},
		{"other_scope_cooldown", func(_ *testing.T, m *SessionManager, _ sessionIdentity) {
			m.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 42, Scope: "other.com", CooldownUntil: future})
		}, false},
		{"other_proxy_cooldown", func(_ *testing.T, m *SessionManager, _ sessionIdentity) {
			m.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 43, Scope: "example.com", CooldownUntil: future})
		}, false},
		{"expired_scope_cooldown", func(_ *testing.T, m *SessionManager, _ sessionIdentity) {
			m.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 42, Scope: "example.com", CooldownUntil: time.Now().Add(-time.Second)})
		}, false},
		{"other_domain_cooldown", func(_ *testing.T, m *SessionManager, _ sessionIdentity) { m.EndDomainTunnels(42, "other.com") }, false},
		{"other_proxy_domain_cooldown", func(_ *testing.T, m *SessionManager, _ sessionIdentity) { m.EndDomainTunnels(43, "example.com") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewSessionManager()
			defer m.Stop()
			key := sessionIdentity{token: "a", scope: "example.com"}
			mustReserve(t, m, key, 42, time.Minute)
			hold := m.openTunnel(key, 42, "example.com")
			ended := make(chan struct{})
			hold.onEnd(func() { close(ended) })

			tc.action(t, m, key)
			wait := 5 * time.Second
			if !tc.ends {
				wait = 100 * time.Millisecond
			}
			select {
			case <-ended:
				if !tc.ends {
					t.Fatal("tunnel ended though its session kept the proxy")
				}
			case <-time.After(wait):
				if tc.ends {
					t.Fatal("tunnel kept running through the proxy its session left")
				}
			}
			m.mu.Lock()
			attached := 0
			if e := m.sessions[key]; e != nil {
				attached = len(e.tunnels)
			}
			m.mu.Unlock()
			if want := map[bool]int{true: 0, false: 1}[tc.ends]; attached != want {
				t.Fatalf("binding holds %d tunnels, want %d", attached, want)
			}
			hold.close()
		})
	}
}

// A tunnel ended before its connections are set shuts down as soon as they are.
func TestSessionManager_TunnelEndedBeforeShutdownIsSet(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()
	key := sessionIdentity{token: "a", scope: "example.com"}
	mustReserve(t, m, key, 42, time.Minute)
	early := m.openTunnel(key, 42, "example.com")
	m.ReleaseSessions(SessionFilter{Token: "a"})

	// The binding moved off proxy 42 between selection and the tunnel opening.
	mustReserve(t, m, key, 43, time.Minute)
	moved := m.openTunnel(key, 42, "example.com")

	for name, hold := range map[string]*sessionHold{"released": early, "moved": moved} {
		ran := false
		hold.onEnd(func() { ran = true })
		if !ran {
			t.Fatalf("%s tunnel did not shut down once its shutdown was set", name)
		}
		hold.close()
	}
	if got := m.FindSessions(SessionFilter{Token: "a"}); len(got) != 1 || got[0].ProxyID != 43 {
		t.Fatalf("ended tunnels changed the live binding: %+v", got)
	}
}

// A tunnel that outlives its binding must not touch the binding that replaces it.
func TestSessionManager_StaleTunnelCloseIgnoresNewBinding(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()
	key := sessionIdentity{token: "a", scope: "example.com"}
	mustReserve(t, m, key, 42, time.Minute)
	stale := m.openTunnel(key, 42, "example.com")
	m.ReleaseSessions(SessionFilter{Token: "a"})
	mustReserve(t, m, key, 42, time.Minute)
	current := m.openTunnel(key, 42, "example.com")

	stale.close()
	m.mu.Lock()
	_, attached := m.sessions[key].tunnels[current]
	tunnels := len(m.sessions[key].tunnels)
	m.mu.Unlock()
	if !attached || tunnels != 1 {
		t.Fatalf("stale close changed the new binding's tunnels: %d attached", tunnels)
	}
	current.close()
}
