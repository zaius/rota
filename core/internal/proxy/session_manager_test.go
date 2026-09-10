package proxy

import (
	"errors"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
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

func TestSessionManager_ExclusiveWithinScope(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()
	first := sessionIdentity{poolID: 1, username: "alice", token: "a", scope: "example.com"}
	mustReserve(t, m, first, 42, time.Minute)
	for _, key := range []sessionIdentity{
		{poolID: 1, username: "alice", token: "b", scope: "example.com"},
		{poolID: 2, username: "alice", token: "b", scope: "example.com"},
		{poolID: 1, username: "bob", token: "a", scope: "example.com"},
		{poolID: 1, scope: "example.com"}, // requests without a token
	} {
		if err := reserveForTest(m, key, 42, time.Minute); !errors.Is(err, ErrNoProxyAvailable) {
			t.Fatalf("%+v must not take another session's proxy: %v", key, err)
		}
	}
	mustReserve(t, m, first, 42, time.Minute)
	other := first
	other.scope = "other.com"
	mustReserve(t, m, other, 42, time.Minute)
	if got := m.List(); len(got) != 2 {
		t.Fatalf("want two scoped bindings, got %v", got)
	}
}

func TestSessionManager_ReleaseAndExpiryFreeReservations(t *testing.T) {
	for _, action := range []string{"release", "release_token", "release_pools", "evict", "expire"} {
		t.Run(action, func(t *testing.T) {
			m := NewSessionManager()
			defer m.Stop()
			key := sessionIdentity{poolID: 1, token: "a", scope: "example.com"}
			mustReserve(t, m, key, 42, time.Minute)
			switch action {
			case "release":
				if !m.Release(1, "a") || m.Release(1, "a") {
					t.Fatal("release existence incorrect")
				}
			case "release_token":
				if n := m.ReleaseToken("a"); n != 1 {
					t.Fatalf("released %d", n)
				}
			case "release_pools":
				if n := m.ReleaseTokenInPools("a", []int{1}); n != 1 {
					t.Fatalf("released %d", n)
				}
			case "evict":
				if n := m.Evict(42); n != 1 {
					t.Fatalf("evicted %d", n)
				}
			case "expire":
				m.mu.Lock()
				m.sessions[key].lastUsed = time.Now().Add(-time.Hour)
				m.mu.Unlock()
			}
			key.token = "b"
			mustReserve(t, m, key, 42, time.Minute)
			if got := m.FindByToken("a"); len(got) != 0 {
				t.Fatalf("old binding remains: %v", got)
			}
		})
	}
}

func TestSessionManager_RebindAndRefresh(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()
	key := sessionIdentity{poolID: 1, token: "a", scope: "example.com"}
	mustReserve(t, m, key, 42, time.Minute)
	created := m.List()[0].CreatedAt
	m.mu.Lock()
	m.sessions[key].lastUsed = time.Now().Add(-30 * time.Second)
	m.mu.Unlock()
	mustReserve(t, m, key, 43, 2*time.Minute)
	got := m.FindByToken("a")[0]
	if got.ProxyID != 43 || got.CreatedAt != created || time.Since(got.LastUsed) > time.Second {
		t.Fatalf("rebind did not preserve creation and refresh use: %+v", got)
	}
	if got.ExpiresAt.Sub(got.LastUsed) != 2*time.Minute {
		t.Fatal("TTL not refreshed")
	}
	key.token = "b"
	mustReserve(t, m, key, 42, time.Minute) // old proxy was freed
}

func TestSessionManager_EmptyTokenDoesNotBind(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()
	mustReserve(t, m, sessionIdentity{poolID: 1}, 42, time.Minute)
	if len(m.List()) != 0 {
		t.Fatal("empty token must not bind")
	}
}

func TestSessionManager_ListAndFindSkipExpired(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()
	key := sessionIdentity{poolID: 1, token: "a"}
	mustReserve(t, m, key, 42, time.Nanosecond)
	time.Sleep(time.Millisecond)
	if len(m.FindByToken("a")) != 0 || len(m.List()) != 0 {
		t.Fatal("expired binding visible")
	}
	key.token = "b"
	mustReserve(t, m, key, 42, time.Minute)
}

func TestSessionManager_ReleaseScopeAndEvictAcrossScopes(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()
	for _, key := range []sessionIdentity{
		{poolID: 1, token: "a", scope: "one"},
		{poolID: 1, token: "a", scope: "two"},
		{poolID: 2, token: "a", scope: "three"},
		{poolID: 3, token: "b", scope: "four"},
	} {
		mustReserve(t, m, key, 42, time.Minute)
	}
	if len(m.FindByToken("a")) != 3 {
		t.Fatal("missing token bindings")
	}
	if n := m.ReleaseTokenInPools("a", []int{1}); n != 2 {
		t.Fatalf("released %d", n)
	}
	if len(m.FindByToken("a")) != 1 || len(m.FindByToken("b")) != 1 {
		t.Fatal("released outside scope")
	}
	if n := m.Evict(42); n != 2 {
		t.Fatalf("evicted %d", n)
	}
	if len(m.List()) != 0 {
		t.Fatal("eviction left bindings")
	}
}

func TestSplitSessionUsername(t *testing.T) {
	cases := []struct {
		raw   string
		user  string
		token string
	}{
		{"alice", "alice", ""},
		{"alice-session-abc123", "alice", "abc123"},
		{"my-user-session-xyz", "my-user", "xyz"},
		{"alice-session-", "alice", ""},
		{"-session-only", "", "only"},
	}
	for _, c := range cases {
		u, tok := splitSessionUsername(c.raw)
		if u != c.user || tok != c.token {
			t.Errorf("splitSessionUsername(%q) = (%q,%q), want (%q,%q)", c.raw, u, tok, c.user, c.token)
		}
	}
}

func TestSplitProfileUsername(t *testing.T) {
	cases := []struct {
		raw     string
		rest    string
		profile string
	}{
		{"alice", "alice", ""},
		{"alice-profile-ios", "alice", "ios"},
		{"my-user-profile-android", "my-user", "android"},
		{"alice-profile-", "alice", ""},
		// Profile is stripped first, so what remains still parses as a
		// session username — this is the composed form clients use.
		{"alice-session-abc123-profile-ios", "alice-session-abc123", "ios"},
		// Profile names containing the marker's own separator survive, because
		// only the last marker is honoured.
		{"alice-profile-android-okhttp", "alice", "android-okhttp"},
	}
	for _, c := range cases {
		rest, profile := splitProfileUsername(c.raw)
		if rest != c.rest || profile != c.profile {
			t.Errorf("splitProfileUsername(%q) = (%q,%q), want (%q,%q)",
				c.raw, rest, profile, c.rest, c.profile)
		}
	}
}

// TestSplitUsernameMarkersCompose checks the two markers together, which is
// how a client that wants both a sticky session and a fingerprint has to write
// the username.
func TestSplitUsernameMarkersCompose(t *testing.T) {
	rest, profile := splitProfileUsername("alice-session-tok9-profile-ios")
	user, token := splitSessionUsername(rest)

	if user != "alice" || token != "tok9" || profile != "ios" {
		t.Errorf("got user=%q token=%q profile=%q, want alice/tok9/ios", user, token, profile)
	}
}

func TestSessionManager_FilteredRelease(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()
	for i, key := range []sessionIdentity{
		{poolID: 1, username: "alice", token: "job", scope: "one"},
		{poolID: 1, username: "alice", token: "job", scope: "two"},
		{poolID: 1, username: "bob", token: "job", scope: "one"},
		{poolID: 2, username: "alice", token: "job", scope: "one"},
	} {
		mustReserve(t, m, key, i+1, time.Minute)
	}
	filter := SessionFilter{Token: "job", Username: "alice", Scope: "one", PoolIDs: []int{1}}
	if n := m.ReleaseSessions(filter); n != 1 {
		t.Fatalf("released %d", n)
	}
	if len(m.List()) != 3 {
		t.Fatal("released unrelated binding")
	}
	filter.PoolIDs = []int{}
	if n := m.ReleaseSessions(filter); n != 0 {
		t.Fatal("empty allowed pools released a binding")
	}
	mustReserve(t, m, sessionIdentity{poolID: 1, username: "carol", token: "new", scope: "one"}, 1, time.Minute)
}
