package proxy

import (
	"testing"
	"time"
)

func TestSessionManager_BindGet(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()

	if _, ok := m.Get(1, "tok"); ok {
		t.Fatal("expected no binding before Bind")
	}

	m.Bind(1, "tok", 42, time.Minute)
	pid, ok := m.Get(1, "tok")
	if !ok || pid != 42 {
		t.Fatalf("expected proxy 42, got %d ok=%v", pid, ok)
	}

	// Different pool, same token → independent binding.
	if _, ok := m.Get(2, "tok"); ok {
		t.Fatal("token must be scoped per pool")
	}
}

func TestSessionManager_EmptyTokenIgnored(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()

	m.Bind(1, "", 7, time.Minute)
	if _, ok := m.Get(1, ""); ok {
		t.Fatal("empty token must never bind")
	}
}

func TestSessionManager_IdleExpiry(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()

	// Bind with an already-elapsed TTL so the next Get treats it as idle.
	m.Bind(1, "tok", 9, time.Nanosecond)
	time.Sleep(time.Millisecond)

	if _, ok := m.Get(1, "tok"); ok {
		t.Fatal("expected binding to be expired after idle TTL")
	}
}

func TestSessionManager_Release(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()

	m.Bind(1, "tok", 5, time.Minute)
	if !m.Release(1, "tok") {
		t.Fatal("Release should report an existing binding")
	}
	if _, ok := m.Get(1, "tok"); ok {
		t.Fatal("binding should be gone after Release")
	}
	if m.Release(1, "tok") {
		t.Fatal("Release of missing binding should report false")
	}
}

func TestSessionManager_ReleaseToken(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()

	m.Bind(1, "tok", 5, time.Minute)
	m.Bind(2, "tok", 6, time.Minute)
	m.Bind(3, "other", 7, time.Minute)

	if n := m.ReleaseToken("tok"); n != 2 {
		t.Fatalf("expected 2 bindings released, got %d", n)
	}
	if _, ok := m.Get(3, "other"); !ok {
		t.Fatal("unrelated token should survive")
	}
}

func TestSessionManager_Evict(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()

	m.Bind(1, "a", 42, time.Minute)
	m.Bind(2, "b", 42, time.Minute)
	m.Bind(3, "c", 99, time.Minute)

	if n := m.Evict(42); n != 2 {
		t.Fatalf("expected 2 bindings evicted for proxy 42, got %d", n)
	}
	if _, ok := m.Get(3, "c"); !ok {
		t.Fatal("binding to a different proxy should survive eviction")
	}
}

func TestSessionManager_RebindRefreshesIdle(t *testing.T) {
	m := NewSessionManager()
	defer m.Stop()

	m.Bind(1, "tok", 1, time.Minute)
	// Re-Get should refresh lastUsed; binding stays alive.
	if _, ok := m.Get(1, "tok"); !ok {
		t.Fatal("binding should be live")
	}
	// Rebind to a new proxy keeps the session key but points elsewhere.
	m.Bind(1, "tok", 2, time.Minute)
	if pid, ok := m.Get(1, "tok"); !ok || pid != 2 {
		t.Fatalf("expected rebind to proxy 2, got %d ok=%v", pid, ok)
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
