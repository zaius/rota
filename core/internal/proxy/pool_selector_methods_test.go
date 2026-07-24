package proxy

import (
	"context"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
)

// newMethodSelector builds a PoolSelector with a fixed proxy set (bypassing the
// DB-backed Refresh) for testing a rotation method in isolation.
func newMethodSelector(method string, proxies ...*models.Proxy) *PoolSelector {
	return &PoolSelector{
		poolID:  1,
		method:  method,
		stick:   2,
		proxies: proxies,
	}
}

func px(id int, requests int64) *models.Proxy {
	return &models.Proxy{ID: id, Requests: requests}
}

func TestPoolSelector_RoundRobinOrder(t *testing.T) {
	ps := newMethodSelector("roundrobin", px(1, 0), px(2, 0), px(3, 0))
	want := []int{1, 2, 3, 1, 2, 3}
	for i, w := range want {
		p, err := ps.Select(context.Background())
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		if p.ID != w {
			t.Fatalf("select %d: got proxy %d, want %d", i, p.ID, w)
		}
	}
}

func TestPoolSelector_RandomStaysInSet(t *testing.T) {
	ps := newMethodSelector("random", px(1, 0), px(2, 0), px(3, 0))
	seen := map[int]bool{}
	for i := 0; i < 60; i++ {
		p, err := ps.Select(context.Background())
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		if p.ID < 1 || p.ID > 3 {
			t.Fatalf("random returned out-of-set proxy %d", p.ID)
		}
		seen[p.ID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("random never varied across 60 draws, saw %v", seen)
	}
}

func TestPoolSelector_StickServesStickCountThenAdvances(t *testing.T) {
	ps := newMethodSelector("stick", px(1, 0), px(2, 0))
	ps.stick = 3
	// stick=3 → proxy 1 three times, then proxy 2 three times, then back.
	want := []int{1, 1, 1, 2, 2, 2, 1}
	for i, w := range want {
		p, err := ps.Select(context.Background())
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		if p.ID != w {
			t.Fatalf("stick select %d: got %d, want %d", i, p.ID, w)
		}
	}
}

func TestPoolSelector_EmptyReturnsError(t *testing.T) {
	ps := newMethodSelector("roundrobin")
	if _, err := ps.Select(context.Background()); err == nil {
		t.Fatal("expected error selecting from an empty pool")
	}
}
