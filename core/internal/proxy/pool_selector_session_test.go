package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

// newSessionSelector builds a PoolSelector in "session" mode with a fixed proxy
// set, bypassing the DB-backed Refresh.
func newSessionSelector(sm *SessionManager, ids ...int) *PoolSelector {
	ps := &PoolSelector{
		poolID:     1,
		method:     "session",
		sessionTTL: time.Minute,
		sessionMgr: sm,
	}
	for _, id := range ids {
		ps.proxies = append(ps.proxies, &models.Proxy{ID: id})
	}
	return ps
}

func ctxWithToken(token string) context.Context {
	return context.WithValue(context.Background(), SessionTokenContextKey, token)
}

func TestPoolSelector_SessionSticksToSameProxy(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	ps := newSessionSelector(sm, 1, 2, 3)
	ctx := ctxWithToken("sess-a")

	first, err := ps.Select(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		p, err := ps.Select(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if p.ID != first.ID {
			t.Fatalf("session must stick: got %d, want %d", p.ID, first.ID)
		}
	}
}

func TestPoolSelector_SessionDistinctTokensDiffer(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	ps := newSessionSelector(sm, 1, 2, 3)

	a, _ := ps.Select(ctxWithToken("a"))
	b, _ := ps.Select(ctxWithToken("b"))
	// Round-robin assignment means two sequential sessions land on different proxies.
	if a.ID == b.ID {
		t.Fatalf("expected distinct proxies for distinct sessions, both got %d", a.ID)
	}
}

func TestPoolSelector_SessionRebindsAfterEvict(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	ps := newSessionSelector(sm, 1, 2, 3)
	ctx := ctxWithToken("sess")

	first, _ := ps.Select(ctx)

	// Simulate invalidation: evict the session and remove the proxy from the pool.
	sm.Evict(first.ID)
	ps.RemoveProxy(first.ID)

	next, err := ps.Select(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == first.ID {
		t.Fatalf("session should rebind away from evicted proxy %d", first.ID)
	}
}

func TestPoolSelector_SessionNoTokenFallsBackRoundRobin(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	ps := newSessionSelector(sm, 1, 2)
	ctx := context.Background() // no token

	a, _ := ps.Select(ctx)
	b, _ := ps.Select(ctx)
	if a.ID == b.ID {
		t.Fatal("without a token, session mode should round-robin")
	}
}
