package proxy

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRotator_SessionSticksToSameProxy(t *testing.T) {
	r := newTestRotator(t, ids(1, 2, 3)...)
	ctx := ctxWithToken("sess-a")

	first, err := r.pick(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		p, err := r.pick(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p.ID != first.ID {
			t.Fatalf("session must stick: got %d, want %d", p.ID, first.ID)
		}
	}
}

func TestRotator_SessionDistinctTokensDiffer(t *testing.T) {
	r := newTestRotator(t, ids(1, 2, 3)...)
	a, _ := r.pick(ctxWithToken("a"), nil)
	b, _ := r.pick(ctxWithToken("b"), nil)
	if a.ID == b.ID {
		t.Fatalf("expected distinct proxies for distinct sessions, both got %d", a.ID)
	}
}

func TestRotator_SessionRebindsAfterEvict(t *testing.T) {
	r := newTestRotator(t, ids(1, 2, 3)...)
	ctx := ctxWithToken("sess")

	first, _ := r.pick(ctx, nil)

	// Simulate invalidation: evict the session and remove the proxy.
	r.sessions.Evict(first.ID)
	r.Remove(first.ID)

	next, err := r.pick(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == first.ID {
		t.Fatalf("session should rebind away from evicted proxy %d", first.ID)
	}
}

func TestRotator_NoTokenRotates(t *testing.T) {
	r := newTestRotator(t, ids(1, 2)...)
	a, _ := r.pick(context.Background(), nil)
	b, _ := r.pick(context.Background(), nil)
	if a.ID == b.ID {
		t.Fatal("without a token, selection should rotate")
	}
}

func TestRotator_SessionExhaustionAndRelease(t *testing.T) {
	r := newTestRotator(t, ids(1, 2)...)
	a, err := r.pick(ctxWithToken("a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pick(ctxWithToken("b"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pick(ctxWithToken("c"), nil); !errors.Is(err, ErrNoProxyAvailable) {
		t.Fatalf("want capacity exhaustion, got %v", err)
	}
	if again, err := r.pick(ctxWithToken("a"), nil); err != nil || again.ID != a.ID {
		t.Fatalf("existing owner lost its proxy: %v %v", again, err)
	}
	r.sessions.ReleaseSessions(SessionFilter{Token: "a"})
	if c, err := r.pick(ctxWithToken("c"), nil); err != nil || c.ID != a.ID {
		t.Fatalf("released proxy not reused: %v %v", c, err)
	}
}

func TestRotator_SessionScopes(t *testing.T) {
	r := newTestRotator(t, ids(1)...)
	request := func(token, host, scope string) context.Context {
		return requestCtx(host, Options{Session: token, Scope: scope})
	}
	if _, err := r.pick(request("a", "Example.COM.:443", ""), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pick(request("b", "example.com:80", ""), nil); !errors.Is(err, ErrNoProxyAvailable) {
		t.Fatalf("normalized host should share scope: %v", err)
	}
	if _, err := r.pick(request("b", "other.com", ""), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pick(request("c", "third.com", "example.com"), nil); !errors.Is(err, ErrNoProxyAvailable) {
		t.Fatalf("explicit scope should override host: %v", err)
	}
	if _, err := r.pick(request("c", "example.com", "custom"), nil); err != nil {
		t.Fatal(err)
	}
}

func TestRotator_ConcurrentSessionReservations(t *testing.T) {
	for _, sameToken := range []bool{false, true} {
		t.Run(fmt.Sprint("same_token=", sameToken), func(t *testing.T) {
			r := newTestRotator(t, ids(42)...)
			const count = 32
			start := make(chan struct{})
			results := make(chan error, count)
			for i := 0; i < count; i++ {
				token := "shared"
				if !sameToken {
					token = fmt.Sprint(i)
				}
				go func() {
					<-start
					_, err := r.pick(ctxWithToken(token), nil)
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
			if successes != want || len(r.sessions.List()) != 1 {
				t.Fatalf("got %d successes and %d bindings", successes, len(r.sessions.List()))
			}
		})
	}
}

func TestRotator_RotationRespectsReservations(t *testing.T) {
	r := newTestRotator(t, ids(1, 2)...)
	if p, err := r.pick(ctxWithToken("owner"), nil); err != nil || p.ID != 1 {
		t.Fatalf("owner did not reserve proxy 1: %v %v", p, err)
	}
	for _, opts := range []Options{{}, {Random: true}} {
		for i := 0; i < 5; i++ {
			p, err := r.pick(requestCtx("", opts), nil)
			if err != nil || p.ID != 2 {
				t.Fatalf("%+v took reserved proxy: %v %v", opts, p, err)
			}
		}
	}
}

func TestRotator_RebindReleasesOnlyItsDomain(t *testing.T) {
	r := newTestRotator(t, ids(1, 2)...)
	foo := requestCtx("foo.com", Options{Session: "a"})
	bar := requestCtx("bar.com", Options{Session: "a"})
	first, err := r.pick(foo, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := r.pick(bar, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.domainCD.Set(first.ID, "foo.com", time.Now().Add(time.Hour), "")
	rebound, err := r.pick(foo, nil)
	if err != nil || rebound.ID == first.ID {
		t.Fatalf("did not rebind: %v %v", rebound, err)
	}
	again, err := r.pick(bar, nil)
	if err != nil || again.ID != other.ID {
		t.Fatalf("changed unrelated domain binding: %v %v", again, err)
	}
}
