package proxy

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

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

// A sticky session keeps its proxy for bar.com after that proxy is invalidated
// for foo.com only; the first foo.com request rebinds the session to a fresh
// proxy.
func TestRotator_SessionRebindsOnDomainCooldown(t *testing.T) {
	r := newTestRotator(t, ids(1, 2, 3)...)
	ctxFoo := requestCtx("foo.com", Options{Session: "sess", Scope: "shared"})
	ctxBar := requestCtx("bar.com", Options{Session: "sess", Scope: "shared"})

	first, err := r.pick(ctxFoo, nil)
	if err != nil {
		t.Fatal(err)
	}

	r.domainCD.Set(first.ID, "foo.com", time.Now().Add(time.Hour), "429")

	p, err := r.pick(ctxBar, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != first.ID {
		t.Fatalf("session should keep proxy %d for bar.com, got %d", first.ID, p.ID)
	}

	next, err := r.pick(ctxFoo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == first.ID {
		t.Fatal("session must rebind away from the proxy cooled for foo.com")
	}

	// The session sticks to the new proxy from here on, for every host.
	again, err := r.pick(ctxBar, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != next.ID {
		t.Fatalf("session should stick to rebound proxy %d, got %d", next.ID, again.ID)
	}
}
