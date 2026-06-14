package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

// newDomainSelector builds a PoolSelector with a fixed proxy set and a domain
// cooldown manager, bypassing the DB-backed Refresh.
func newDomainSelector(method string, sm *SessionManager, cd *DomainCooldownManager, ids ...int) *PoolSelector {
	ps := &PoolSelector{
		poolID:     1,
		method:     method,
		stick:      2,
		sessionTTL: time.Minute,
		sessionMgr: sm,
		domainCD:   cd,
	}
	for _, id := range ids {
		ps.proxies = append(ps.proxies, &models.Proxy{ID: id})
	}
	return ps
}

func ctxWithHost(host string) context.Context {
	return context.WithValue(context.Background(), TargetHostContextKey, host)
}

func TestPoolSelector_RoundRobinSkipsDomainCooled(t *testing.T) {
	cd := NewDomainCooldownManager()
	defer cd.Stop()
	ps := newDomainSelector("roundrobin", nil, cd, 1, 2, 3)

	cd.Set(1, "foo.com", time.Now().Add(time.Hour), "429")

	// foo.com never gets proxy 1...
	for i := 0; i < 6; i++ {
		p, err := ps.Select(ctxWithHost("foo.com"))
		if err != nil {
			t.Fatal(err)
		}
		if p.ID == 1 {
			t.Fatal("proxy 1 is on a foo.com cooldown and must not serve foo.com")
		}
	}

	// ...but bar.com still does.
	saw := map[int]bool{}
	for i := 0; i < 6; i++ {
		p, err := ps.Select(ctxWithHost("bar.com"))
		if err != nil {
			t.Fatal(err)
		}
		saw[p.ID] = true
	}
	if !saw[1] {
		t.Fatal("proxy 1 should still serve bar.com")
	}
}

func TestPoolSelector_AllCooledForHostErrors(t *testing.T) {
	cd := NewDomainCooldownManager()
	defer cd.Stop()
	ps := newDomainSelector("roundrobin", nil, cd, 1, 2)

	until := time.Now().Add(time.Hour)
	cd.Set(1, "foo.com", until, "")
	cd.Set(2, "foo.com", until, "")

	if _, err := ps.Select(ctxWithHost("foo.com")); err == nil {
		t.Fatal("expected error when every proxy is cooled for the host")
	}
	if _, err := ps.Select(ctxWithHost("bar.com")); err != nil {
		t.Fatalf("bar.com should still be served: %v", err)
	}
}

func TestPoolSelector_RandomSkipsDomainCooled(t *testing.T) {
	cd := NewDomainCooldownManager()
	defer cd.Stop()
	ps := newDomainSelector("random", nil, cd, 1, 2)

	cd.Set(1, "foo.com", time.Now().Add(time.Hour), "")

	for i := 0; i < 20; i++ {
		p, err := ps.Select(ctxWithHost("foo.com"))
		if err != nil {
			t.Fatal(err)
		}
		if p.ID != 2 {
			t.Fatalf("only proxy 2 is eligible for foo.com, got %d", p.ID)
		}
	}
}

func TestPoolSelector_StickSkipsDomainCooled(t *testing.T) {
	cd := NewDomainCooldownManager()
	defer cd.Stop()
	ps := newDomainSelector("stick", nil, cd, 1, 2)

	cd.Set(1, "foo.com", time.Now().Add(time.Hour), "")

	for i := 0; i < 6; i++ {
		p, err := ps.Select(ctxWithHost("foo.com"))
		if err != nil {
			t.Fatal(err)
		}
		if p.ID == 1 {
			t.Fatal("stick mode must skip the cooled proxy for foo.com")
		}
	}
}

// Bug: stick state (stickIdx/stickServed) is shared across all target hosts, so
// a foo.com request that detours around a foo.com-only cooldown must not disturb
// the sticky proxy bar.com is pinned to. Interleaving a foo.com request between
// bar.com requests previously advanced the shared pointer and shifted bar.com to
// a different proxy mid-stick.
func TestPoolSelector_StickDomainCooldownDoesNotDisruptOtherHosts(t *testing.T) {
	cd := NewDomainCooldownManager()
	defer cd.Stop()
	ps := newDomainSelector("stick", nil, cd, 1, 2)
	ps.stick = 3

	// Proxy 1 (the initial sticky proxy) is cooled for foo.com only.
	cd.Set(1, "foo.com", time.Now().Add(time.Hour), "429")

	// bar.com should pin to a single proxy for `stick` consecutive bar.com
	// requests, regardless of interleaved foo.com traffic that detours around
	// the cooled proxy.
	first, err := ps.Select(ctxWithHost("bar.com"))
	if err != nil {
		t.Fatal(err)
	}

	// Interleaved foo.com request: proxy 1 is cooled for foo.com, so it gets a
	// substitute — but this must not move bar.com's sticky proxy.
	if _, err := ps.Select(ctxWithHost("foo.com")); err != nil {
		t.Fatal(err)
	}

	second, err := ps.Select(ctxWithHost("bar.com"))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("bar.com stick broken by interleaved foo.com request: first=%d second=%d", first.ID, second.ID)
	}

	third, err := ps.Select(ctxWithHost("bar.com"))
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != first.ID {
		t.Fatalf("bar.com should stay on proxy %d for its full stick window, got %d", first.ID, third.ID)
	}
}

// The headline scenario: a sticky session keeps its proxy for bar.com after
// that proxy is invalidated for foo.com only; the first foo.com request
// rebinds the session to a fresh proxy.
func TestPoolSelector_SessionRebindsOnDomainCooldown(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	cd := NewDomainCooldownManager()
	defer cd.Stop()
	ps := newDomainSelector("session", sm, cd, 1, 2, 3)

	ctxFoo := context.WithValue(ctxWithHost("foo.com"), SessionTokenContextKey, "sess")
	ctxBar := context.WithValue(ctxWithHost("bar.com"), SessionTokenContextKey, "sess")

	first, err := ps.Select(ctxFoo)
	if err != nil {
		t.Fatal(err)
	}

	// Invalidate the bound proxy for foo.com only.
	cd.Set(first.ID, "foo.com", time.Now().Add(time.Hour), "429")

	// bar.com requests keep the binding.
	p, err := ps.Select(ctxBar)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != first.ID {
		t.Fatalf("session should keep proxy %d for bar.com, got %d", first.ID, p.ID)
	}

	// The next foo.com request rebinds the session to a fresh proxy.
	next, err := ps.Select(ctxFoo)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == first.ID {
		t.Fatal("session must rebind away from the proxy cooled for foo.com")
	}

	// The session sticks to the new proxy from here on (for all hosts).
	again, err := ps.Select(ctxBar)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != next.ID {
		t.Fatalf("session should stick to rebound proxy %d, got %d", next.ID, again.ID)
	}
}
