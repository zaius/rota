package events

import (
	"context"
	"testing"
	"time"
)

// testStores runs fn against a store that answers from the open day's log, one
// that sweeps the log into the open day's Parquet file before each read, and
// one that finishes the day before each read, so every read path keeps the
// Store contract.
func testStores(t *testing.T, fn func(t *testing.T, store Store)) {
	for _, name := range []string{"log", "swept", "finished"} {
		t.Run(name, func(t *testing.T) {
			day := newFakeDay()
			rows := int64(sweepRows)
			if name == "swept" {
				rows = 2
			}
			files := newTestParquetStoreAs(t, t.TempDir(), day, testInstance, rows)
			prepare := map[string]func(){
				"log":      func() {},
				"swept":    func() { files.sweepPending(context.Background()) },
				"finished": func() { midnight(t, files, day) },
			}[name]
			fn(t, preparedStore{ParquetStore: files, prepare: prepare})
		})
	}
}

// Regression: target failures counted against the proxies that carried them,
// dragging down their success rate.
func TestTargetFailuresExcludedFromProxyReliability(t *testing.T) {
	testStores(t, func(t *testing.T, store Store) {
		ctx := context.Background()
		good, bad, targetOnly := 1, 2, 3

		for _, group := range []struct {
			proxyID       int
			success       bool
			targetFailure bool
			count         int
		}{
			{good, true, false, 5},
			{good, false, true, 10},
			{bad, true, false, 1},
			{bad, false, false, 10},
			{targetOnly, false, true, 10},
		} {
			for range group.count {
				if err := store.InsertRequest(ctx, RequestEvent{
					ProxyID: group.proxyID, ProxyAddress: "x", Method: "CONNECT",
					URL: "CONNECT://target.example:443", Domain: "target.example",
					Success: group.success, TargetFailure: group.targetFailure,
					ResponseTime: 100, Timestamp: time.Now(),
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		rollup, err := store.ProxyRollup(ctx)
		if err != nil {
			t.Fatal(err)
		}
		byID := map[int]ProxyRequestStats{}
		for _, stats := range rollup {
			byID[stats.ProxyID] = stats
		}
		if len(byID) != 2 || byID[good].Requests != 5 || byID[good].Successes != 5 || byID[bad].Requests != 11 || byID[bad].Successes != 1 {
			t.Fatalf("target failures affected proxy reliability: %+v", byID)
		}
		// Target failures must affect neither the success rate nor the minimum
		// request threshold, and a proxy with only target failures is ineligible.
		for _, minimum := range []int{1, 10} {
			ids, err := store.LowSuccessProxies(ctx, 24*time.Hour, 50, minimum)
			if err != nil {
				t.Fatal(err)
			}
			if len(ids) != 1 || ids[0] != bad {
				t.Fatalf("cleanup with minimum %d: want [%d], got %v", minimum, bad, ids)
			}
		}
		traffic, err := store.RequestStats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if traffic.RequestsToday != 36 || traffic.SuccessRateToday < 16.66 || traffic.SuccessRateToday > 16.67 {
			t.Fatalf("target failures missing from traffic history: %+v", traffic)
		}
	})
}
