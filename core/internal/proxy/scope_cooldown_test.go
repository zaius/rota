package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

func TestScopeCooldown_IsolationAndRebind(t *testing.T) {
	for _, scope := range []string{"foo.com", "shopping"} {
		t.Run(scope, func(t *testing.T) {
			r := newTestRotator(t, ids(1)...)
			opts := Options{Session: "job42"}
			if scope == "shopping" {
				opts.Scope = scope
			}
			ctx := requestCtx("foo.com", opts)
			other := requestCtx("bar.com", Options{Session: "job42"})
			for _, c := range []context.Context{ctx, other} {
				if _, err := r.pick(c, nil); err != nil {
					t.Fatal(err)
				}
			}
			r.sessions.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 1, Scope: scope, CooldownUntil: time.Now().Add(time.Hour)})
			if _, err := r.pick(ctx, nil); !errors.Is(err, ErrNoProxyAvailable) {
				t.Fatalf("cooled scope should be exhausted: %v", err)
			}
			if p, err := r.pick(other, nil); err != nil || p.ID != 1 {
				t.Fatalf("unrelated binding changed: %v %v", p, err)
			}
			r.proxies = append(r.proxies, &models.Proxy{ID: 2})
			if p, err := r.pick(ctx, nil); err != nil || p.ID != 2 {
				t.Fatalf("session did not rebind: %v %v", p, err)
			}
			if scope == "shopping" {
				if p, err := r.pick(requestCtx("api.other.com", opts), nil); err != nil || p.ID != 2 {
					t.Fatalf("custom scope cooldown must cover other target hosts: %v %v", p, err)
				}
			}
		})
	}
}

func TestScopeCooldown_AppliesWithoutSession(t *testing.T) {
	for _, opts := range []Options{{Scope: "shopping"}, {Scope: "shopping", Random: true}} {
		r := newTestRotator(t, ids(1, 2)...)
		r.sessions.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 1, Scope: "shopping", CooldownUntil: time.Now().Add(time.Hour)})
		for range 5 {
			if p, err := r.pick(requestCtx("", opts), nil); err != nil || p.ID != 2 {
				t.Fatalf("%+v selected cooled proxy: %v %v", opts, p, err)
			}
		}
	}
}

func TestScopeCooldown_ExpiryRefreshAndClear(t *testing.T) {
	r := newTestRotator(t, ids(1)...)
	sm := r.sessions
	ctx := requestCtx("", Options{Scope: "shopping"})
	sm.ReplaceScopeCooldowns([]models.ProxyScopeCooldown{
		{ProxyID: 1, Scope: "shopping", CooldownUntil: time.Now().Add(-time.Second)},
		{ProxyID: 1, Scope: "other", CooldownUntil: time.Now().Add(time.Hour)},
	})
	if p, err := r.pick(ctx, nil); err != nil || p.ID != 1 {
		t.Fatalf("expired cooldown still active: %v %v", p, err)
	}
	if len(sm.ListScopeCooldowns()) != 1 {
		t.Fatal("listing includes expired cooldown")
	}
	sm.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 1, Scope: "shopping", CooldownUntil: time.Now().Add(time.Hour)})
	if sm.ClearScopeCooldowns(1, "shopping") != 1 || len(sm.ListScopeCooldowns()) != 1 {
		t.Fatal("scoped reactivation affected another scope")
	}
	if sm.ClearScopeCooldowns(1, "") != 1 || len(sm.ListScopeCooldowns()) != 0 {
		t.Fatal("full reactivation left a cooldown")
	}
	sm.ReplaceScopeCooldowns([]models.ProxyScopeCooldown{{ProxyID: 1, Scope: "shopping", CooldownUntil: time.Now().Add(time.Hour)}})
	if _, err := r.pick(ctx, nil); !errors.Is(err, ErrNoProxyAvailable) {
		t.Fatalf("restored cooldown not enforced: %v", err)
	}
}
