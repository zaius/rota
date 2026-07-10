package events

import (
	"sort"
	"sync"
	"testing"
)

// nextLogID must issue strictly increasing, unique IDs under concurrency —
// the LogsSince streaming cursor depends on it.
func TestNextLogID_MonotonicUnderConcurrency(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 1000

	var wg sync.WaitGroup
	results := make([][]int64, goroutines)
	for g := range results {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ids := make([]int64, perGoroutine)
			for i := range ids {
				ids[i] = nextLogID()
			}
			results[g] = ids
		}(g)
	}
	wg.Wait()

	var all []int64
	for g, ids := range results {
		for i := 1; i < len(ids); i++ {
			if ids[i] <= ids[i-1] {
				t.Fatalf("goroutine %d: id %d issued after %d is not greater", g, ids[i], ids[i-1])
			}
		}
		all = append(all, ids...)
	}

	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	for i := 1; i < len(all); i++ {
		if all[i] == all[i-1] {
			t.Fatalf("duplicate id issued: %d", all[i])
		}
	}
}
