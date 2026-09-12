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
			sm := NewSessionManager()
			defer sm.Stop()
			ps := newSessionSelector(sm, 1)
			ctx := context.WithValue(ctxWithToken("job42"), TargetHostContextKey, "foo.com")
			if scope == "shopping" {
				ctx = context.WithValue(ctx, SessionScopeContextKey, scope)
			}
			other := context.WithValue(ctxWithToken("job42"), TargetHostContextKey, "bar.com")
			for _, c := range []context.Context{ctx, other} {
				if _, err := ps.Select(c); err != nil {
					t.Fatal(err)
				}
			}
			sm.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 1, Scope: scope, CooldownUntil: time.Now().Add(time.Hour)})
			if _, err := ps.Select(ctx); !errors.Is(err, ErrNoProxyAvailable) {
				t.Fatalf("cooled scope should be exhausted: %v", err)
			}
			if p, err := ps.Select(other); err != nil || p.ID != 1 {
				t.Fatalf("unrelated binding changed: %v %v", p, err)
			}
			ps.proxies = append(ps.proxies, &models.Proxy{ID: 2})
			if p, err := ps.Select(ctx); err != nil || p.ID != 2 {
				t.Fatalf("session did not rebind: %v %v", p, err)
			}
			if scope == "shopping" {
				ctx = context.WithValue(ctx, TargetHostContextKey, "api.other.com")
				if p, err := ps.Select(ctx); err != nil || p.ID != 2 {
					t.Fatalf("custom scope cooldown must cover other target hosts: %v %v", p, err)
				}
			}
		})
	}
}

func TestScopeCooldown_AllModesAndPools(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	sm.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 1, Scope: "shopping", CooldownUntil: time.Now().Add(time.Hour)})
	ctx := context.WithValue(context.Background(), SessionScopeContextKey, "shopping")
	for _, method := range []string{"session", "roundrobin", "random", "stick"} {
		ps := newSessionSelector(sm, 1, 2)
		ps.poolID, ps.method = 99, method
		for range 5 {
			if p, err := ps.Select(ctx); err != nil || p.ID != 2 {
				t.Fatalf("%s selected cooled proxy in another pool: %v %v", method, p, err)
			}
		}
	}
}

func TestScopeCooldown_SessionFallsBack(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	primary := newSessionSelector(sm, 1)
	fallback := newSessionSelector(sm, 1, 2)
	fallback.poolID, fallback.method = 2, "roundrobin"
	chain := &PoolChain{username: "alice", selectors: []*PoolSelector{primary, fallback}}
	ctx := context.WithValue(ctxWithToken("job42"), SessionScopeContextKey, "shopping")
	if p, idx, err := chain.pickProxy(ctx, nil); err != nil || idx != 0 || p.ID != 1 {
		t.Fatalf("initial binding: %v %d %v", p, idx, err)
	}
	sm.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 1, Scope: "shopping", CooldownUntil: time.Now().Add(time.Hour)})
	for range 2 {
		if p, idx, err := chain.pickProxy(ctx, nil); err != nil || idx != 1 || p.ID != 2 {
			t.Fatalf("fallback selected cooled proxy or lost binding: %v %d %v", p, idx, err)
		}
	}
}

func TestScopeCooldown_ExpiryRefreshAndClear(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	ps := newSessionSelector(sm, 1)
	ctx := context.WithValue(context.Background(), SessionScopeContextKey, "shopping")
	sm.ReplaceScopeCooldowns([]models.ProxyScopeCooldown{
		{ProxyID: 1, Scope: "shopping", CooldownUntil: time.Now().Add(-time.Second)},
		{ProxyID: 1, Scope: "other", CooldownUntil: time.Now().Add(time.Hour)},
	})
	if p, err := ps.Select(ctx); err != nil || p.ID != 1 {
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
	if _, err := ps.Select(ctx); !errors.Is(err, ErrNoProxyAvailable) {
		t.Fatalf("restored cooldown not enforced: %v", err)
	}
}
