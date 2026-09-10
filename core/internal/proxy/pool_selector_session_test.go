package proxy

import (
	"context"
	"errors"
	"fmt"
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

func TestPoolSelector_SessionExhaustionAndRelease(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	ps := newSessionSelector(sm, 1, 2)
	a, err := ps.Select(ctxWithToken("a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Select(ctxWithToken("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Select(ctxWithToken("c")); !errors.Is(err, ErrNoProxyAvailable) {
		t.Fatalf("want capacity exhaustion, got %v", err)
	}
	if again, err := ps.Select(ctxWithToken("a")); err != nil || again.ID != a.ID {
		t.Fatalf("existing owner lost its proxy: %v %v", again, err)
	}
	sm.Release(1, "a")
	if c, err := ps.Select(ctxWithToken("c")); err != nil || c.ID != a.ID {
		t.Fatalf("released proxy not reused: %v %v", c, err)
	}
}

func TestPoolSelector_SessionScopes(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	ps := newSessionSelector(sm, 1)
	request := func(token, host, scope string) context.Context {
		ctx := context.WithValue(ctxWithToken(token), TargetHostContextKey, host)
		return context.WithValue(ctx, SessionScopeContextKey, scope)
	}
	if _, err := ps.Select(request("a", "Example.COM.:443", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Select(request("b", "example.com:80", "")); !errors.Is(err, ErrNoProxyAvailable) {
		t.Fatalf("normalized host should share scope: %v", err)
	}
	if _, err := ps.Select(request("b", "other.com", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Select(request("c", "third.com", "example.com")); !errors.Is(err, ErrNoProxyAvailable) {
		t.Fatalf("explicit scope should override host: %v", err)
	}
	if _, err := ps.Select(request("c", "example.com", "custom")); err != nil {
		t.Fatal(err)
	}
}

func TestPoolSelector_ConcurrentSessionReservations(t *testing.T) {
	for _, sameToken := range []bool{false, true} {
		t.Run(fmt.Sprint("same_token=", sameToken), func(t *testing.T) {
			sm := NewSessionManager()
			defer sm.Stop()
			const count = 32
			start := make(chan struct{})
			results := make(chan error, count)
			for i := 0; i < count; i++ {
				// Independent selectors model simultaneous chains and cache rebuilds.
				ps := newSessionSelector(sm, 42)
				token := "shared"
				if !sameToken {
					token = fmt.Sprint(i)
				}
				go func() {
					<-start
					_, err := ps.Select(ctxWithToken(token))
					results <- err
				}()
			}
			close(start)
			successes := 0
			for i := 0; i < count; i++ {
				if err := <-results; err == nil {
					successes++
				} else if !errors.Is(err, ErrNoProxyAvailable) {
					t.Fatal(err)
				}
			}
			want := 1
			if sameToken {
				want = count
			}
			if successes != want || len(sm.List()) != 1 {
				t.Fatalf("got %d successes and %d bindings", successes, len(sm.List()))
			}
		})
	}
}

func TestPoolSelector_AllMethodsRespectReservations(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	owner := newSessionSelector(sm, 1)
	if _, err := owner.Select(ctxWithToken("owner")); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"session", "roundrobin", "random", "stick"} {
		ps := newSessionSelector(sm, 1, 2)
		ps.method = method
		for i := 0; i < 5; i++ {
			p, err := ps.Select(context.Background())
			if err != nil || p.ID != 2 {
				t.Fatalf("%s took reserved proxy: %v %v", method, p, err)
			}
		}
	}
}

func TestPoolSelector_RebindReleasesOnlyItsDomain(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	cd := NewDomainCooldownManager()
	defer cd.Stop()
	ps := newDomainSelector("session", sm, cd, 1, 2)
	foo := context.WithValue(ctxWithToken("a"), TargetHostContextKey, "foo.com")
	bar := context.WithValue(ctxWithToken("a"), TargetHostContextKey, "bar.com")
	first, err := ps.Select(foo)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ps.Select(bar)
	if err != nil {
		t.Fatal(err)
	}
	cd.Set(first.ID, "foo.com", time.Now().Add(time.Hour), "")
	rebound, err := ps.Select(foo)
	if err != nil || rebound.ID == first.ID {
		t.Fatalf("did not rebind: %v %v", rebound, err)
	}
	again, err := ps.Select(bar)
	if err != nil || again.ID != other.ID {
		t.Fatalf("changed unrelated domain binding: %v %v", again, err)
	}
}
