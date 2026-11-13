package proxy

import (
	"testing"
	"time"
)

func TestRotator_SkipsDomainCooled(t *testing.T) {
	for _, opts := range []Options{{}, {Random: true}} {
		r := newTestRotator(t, ids(1, 2, 3)...)
		r.domainCD.Set(1, "foo.com", time.Now().Add(time.Hour), "429")

		// foo.com never gets proxy 1...
		for i := 0; i < 6; i++ {
			p, err := r.pick(requestCtx("foo.com", opts), nil)
			if err != nil {
				t.Fatal(err)
			}
			if p.ID == 1 {
				t.Fatalf("%+v: proxy 1 is on a foo.com cooldown and must not serve foo.com", opts)
			}
		}

		// ...but bar.com still does.
		saw := map[int]bool{}
		for i := 0; i < 30; i++ {
			p, err := r.pick(requestCtx("bar.com", opts), nil)
			if err != nil {
				t.Fatal(err)
			}
			saw[p.ID] = true
		}
		if !saw[1] {
			t.Fatalf("%+v: proxy 1 should still serve bar.com", opts)
		}
	}
}

func TestRotator_AllCooledForHostErrors(t *testing.T) {
	r := newTestRotator(t, ids(1, 2)...)
	until := time.Now().Add(time.Hour)
	r.domainCD.Set(1, "foo.com", until, "")
	r.domainCD.Set(2, "foo.com", until, "")

	if _, err := r.pick(ctxWithHost("foo.com"), nil); err == nil {
		t.Fatal("expected error when every proxy is cooled for the host")
	}
	if _, err := r.pick(ctxWithHost("bar.com"), nil); err != nil {
		t.Fatalf("bar.com should still be served: %v", err)
	}
}

// The headline scenario: a sticky session keeps its proxy for bar.com after
// that proxy is invalidated for foo.com only; the first foo.com request
// rebinds the session to a fresh proxy.
func TestRotator_SessionRebindsOnDomainCooldown(t *testing.T) {
	r := newTestRotator(t, ids(1, 2, 3)...)
	ctxFoo := requestCtx("foo.com", Options{Session: "sess", Scope: "shared"})
	ctxBar := requestCtx("bar.com", Options{Session: "sess", Scope: "shared"})

	first, err := r.pick(ctxFoo, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Invalidate the bound proxy for foo.com only.
	r.domainCD.Set(first.ID, "foo.com", time.Now().Add(time.Hour), "429")

	// bar.com requests keep the binding.
	p, err := r.pick(ctxBar, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != first.ID {
		t.Fatalf("session should keep proxy %d for bar.com, got %d", first.ID, p.ID)
	}

	// The next foo.com request rebinds the session to a fresh proxy.
	next, err := r.pick(ctxFoo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == first.ID {
		t.Fatal("session must rebind away from the proxy cooled for foo.com")
	}

	// The session sticks to the new proxy from here on (for all hosts).
	again, err := r.pick(ctxBar, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != next.ID {
		t.Fatalf("session should stick to rebound proxy %d, got %d", next.ID, again.ID)
	}
}
