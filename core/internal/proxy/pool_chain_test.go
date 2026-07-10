package proxy

import (
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
)

// chainWithProxy builds a chain over a single selector holding one proxy, with
// no database behind it — enough to observe eviction.
func chainWithProxy(proxyID int) (*PoolChain, *PoolSelector) {
	sel := &PoolSelector{
		poolID:  1,
		method:  "roundrobin",
		proxies: []*models.Proxy{{ID: proxyID, Address: "127.0.0.1:9000", Protocol: "http"}},
	}
	return &PoolChain{
		selectors:  []*PoolSelector{sel},
		failCounts: make(map[int]int),
	}, sel
}

func proxyCount(sel *PoolSelector) int {
	sel.mu.Lock()
	defer sel.mu.Unlock()
	return len(sel.proxies)
}

// A single transient failure must not drain the pool.
func TestPoolChain_MarkFailedKeepsProxyBelowThreshold(t *testing.T) {
	c, sel := chainWithProxy(7)

	for i := 1; i < chainFailureThreshold; i++ {
		c.markFailed(0, 7)
		if proxyCount(sel) != 1 {
			t.Fatalf("proxy evicted after %d failure(s), threshold is %d", i, chainFailureThreshold)
		}
	}
}

func TestPoolChain_MarkFailedEvictsAtThreshold(t *testing.T) {
	c, sel := chainWithProxy(7)

	for range chainFailureThreshold {
		c.markFailed(0, 7)
	}
	if proxyCount(sel) != 0 {
		t.Fatalf("expected the proxy to be evicted after %d consecutive failures", chainFailureThreshold)
	}
}

// Intervening success resets the streak, so failures must not accumulate across it.
func TestPoolChain_MarkSucceededResetsFailureStreak(t *testing.T) {
	c, sel := chainWithProxy(7)

	for i := 1; i < chainFailureThreshold; i++ {
		c.markFailed(0, 7)
	}
	c.markSucceeded(7)

	for i := 1; i < chainFailureThreshold; i++ {
		c.markFailed(0, 7)
		if proxyCount(sel) != 1 {
			t.Fatalf("failure streak was not reset by an intervening success")
		}
	}
}

// Failures for one proxy must not evict another.
func TestPoolChain_FailureCountsArePerProxy(t *testing.T) {
	sel := &PoolSelector{
		poolID: 1,
		method: "roundrobin",
		proxies: []*models.Proxy{
			{ID: 1, Address: "127.0.0.1:9001", Protocol: "http"},
			{ID: 2, Address: "127.0.0.1:9002", Protocol: "http"},
		},
	}
	c := &PoolChain{selectors: []*PoolSelector{sel}, failCounts: make(map[int]int)}

	for range chainFailureThreshold {
		c.markFailed(0, 1)
	}

	sel.mu.Lock()
	defer sel.mu.Unlock()
	if len(sel.proxies) != 1 || sel.proxies[0].ID != 2 {
		t.Fatalf("expected only proxy 1 to be evicted, got %d proxies", len(sel.proxies))
	}
}

// A chain built without the constructor (as tests and zero values do) must not
// panic on the first failure.
func TestPoolChain_MarkFailedWithNilCounts(t *testing.T) {
	c := &PoolChain{}
	c.markFailed(-1, 7)
	c.markSucceeded(7)
}
