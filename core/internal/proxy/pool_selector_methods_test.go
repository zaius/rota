package proxy

import (
	"context"
	"testing"
	"time"

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

func TestPoolSelector_LeastConnBalancesInMemoryUsage(t *testing.T) {
	// least_conn balances on this selector's own usage counts, not the
	// database request totals — px lifetime counts must be ignored.
	ps := newMethodSelector("least_conn", px(1, 100), px(2, 5), px(3, 42))
	counts := map[int]int{}
	for i := 0; i < 9; i++ {
		p, err := ps.Select(context.Background())
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		counts[p.ID]++
	}
	for id := 1; id <= 3; id++ {
		if counts[id] != 3 {
			t.Fatalf("least_conn: uneven distribution %v, want 3 each", counts)
		}
	}
}

func TestPoolSelector_LeastConnPrefersLeastUsed(t *testing.T) {
	ps := newMethodSelector("least_conn", px(1, 0), px(2, 0), px(3, 0))
	// Pre-load usage so proxy 2 is clearly the least used.
	ps.useCounts = map[int]int64{1: 10, 2: 1, 3: 7}

	p, err := ps.Select(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != 2 {
		t.Fatalf("least_conn: got proxy %d, want 2 (least used)", p.ID)
	}
}

func TestPoolSelector_LeastConnSkipsCooled(t *testing.T) {
	cd := NewDomainCooldownManager()
	defer cd.Stop()
	ps := newMethodSelector("least_conn", px(1, 0), px(2, 0), px(3, 0))
	ps.domainCD = cd
	// Proxy 1 is the least used but is cooled for foo.com → expect proxy 2.
	ps.useCounts = map[int]int64{1: 0, 2: 5, 3: 9}
	cd.Set(1, "foo.com", time.Now().Add(time.Hour), "429")

	p, err := ps.Select(ctxWithHost("foo.com"))
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != 2 {
		t.Fatalf("least_conn with proxy 1 cooled: got %d, want 2", p.ID)
	}
}

func TestPoolSelector_TimeBasedReturnsEligible(t *testing.T) {
	cd := NewDomainCooldownManager()
	defer cd.Stop()
	ps := newMethodSelector("time_based", px(1, 0), px(2, 0), px(3, 0))
	ps.timeInterval = time.Minute
	ps.domainCD = cd
	cd.Set(2, "foo.com", time.Now().Add(time.Hour), "429")

	for i := 0; i < 10; i++ {
		p, err := ps.Select(ctxWithHost("foo.com"))
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		if p.ID == 2 {
			t.Fatal("time_based returned proxy 2, which is cooled for foo.com")
		}
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
