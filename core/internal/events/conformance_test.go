package events

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

// testStores runs fn against a store that answers from the open day's log and
// against one that converts every day to Parquet before each read, so both
// read paths keep the Store contract. files is the ParquetStore behind store.
func testStores(t *testing.T, fn func(t *testing.T, store Store, files *ParquetStore)) {
	for _, sealed := range []bool{false, true} {
		name := "log"
		if sealed {
			name = "parquet"
		}
		t.Run(name, func(t *testing.T) {
			day := newFakeDay()
			files := newTestParquetStore(t, t.TempDir(), day)
			var store Store = files
			if sealed {
				store = sealingStore{ParquetStore: files, day: day}
			}
			fn(t, store, files)
		})
	}
}

func TestRequestStatsAndCharts(t *testing.T) {
	testStores(t, func(t *testing.T, store Store, files *ParquetStore) {
		ctx := context.Background()
		proxyID := 1

		now := time.Now()
		reqs := []RequestEvent{
			{ProxyID: proxyID, ProxyAddress: "127.0.0.1:9999", PoolID: 7, Username: "alice",
				Method: "GET", URL: "http://example.com", Domain: "example.com",
				StatusCode: 200, ResponseTime: 100, Success: true, Timestamp: now},
			{ProxyID: proxyID, ProxyAddress: "127.0.0.1:9999", Method: "GET", URL: "http://example.com",
				ResponseTime: 300, Success: false, Error: "connect timeout", Timestamp: now},
			// Yesterday's window: between 1 and 2 days ago.
			{ProxyID: proxyID, ProxyAddress: "127.0.0.1:9999", Method: "GET", URL: "http://example.com",
				StatusCode: 200, ResponseTime: 200, Success: true, Timestamp: now.Add(-36 * time.Hour)},
		}
		for _, r := range reqs {
			if err := store.InsertRequest(ctx, r); err != nil {
				t.Fatalf("InsertRequest: %v", err)
			}
		}

		stats, err := store.RequestStats(ctx)
		if err != nil {
			t.Fatalf("RequestStats: %v", err)
		}
		if stats.RequestsToday != 2 || stats.RequestsYesterday != 1 {
			t.Errorf("RequestStats counts: want today=2 yesterday=1, got today=%d yesterday=%d",
				stats.RequestsToday, stats.RequestsYesterday)
		}
		if stats.SuccessRateToday != 50 || stats.SuccessRateYesterday != 100 {
			t.Errorf("RequestStats rates: want today=50 yesterday=100, got today=%v yesterday=%v",
				stats.SuccessRateToday, stats.SuccessRateYesterday)
		}
		if stats.ResponseTimeToday != 200 || stats.ResponseTimeYesterday != 200 {
			t.Errorf("RequestStats response times: want today=200 yesterday=200, got today=%d yesterday=%d",
				stats.ResponseTimeToday, stats.ResponseTimeYesterday)
		}

		// Response-time chart averages successful requests only.
		rt, err := store.ResponseTimeChart(ctx, "1h")
		if err != nil {
			t.Fatalf("ResponseTimeChart: %v", err)
		}
		if len(rt) != 1 || rt[0].Value != 100 {
			t.Errorf("ResponseTimeChart: want one bucket of 100ms, got %+v", rt)
		}

		sr, err := store.SuccessRateChart(ctx, "1h")
		if err != nil {
			t.Fatalf("SuccessRateChart: %v", err)
		}
		if len(sr) != 1 || sr[0].Success != 50 || sr[0].Failure != 50 {
			t.Errorf("SuccessRateChart: want one 50/50 bucket, got %+v", sr)
		}

		// Dimensions round-trip; zero values mean "not applicable".
		var withDims, without int
		for _, row := range readAllRequests(t, files) {
			switch {
			case row.PoolID == 7 && row.Username == "alice" && row.Domain == "example.com":
				withDims++
			case row.PoolID == 0 && row.Username == "" && row.Domain == "":
				without++
			default:
				t.Errorf("dimensions did not round-trip: %+v", row)
			}
		}
		if withDims != 1 || without != 2 {
			t.Errorf("want 1 dimensioned + 2 dimensionless rows, got %d + %d", withDims, without)
		}
	})
}

func TestTunnelStats(t *testing.T) {
	testStores(t, func(t *testing.T, store Store, _ *ParquetStore) {
		ctx := context.Background()
		proxyID := 1

		now := time.Now()
		tunnels := []TunnelEvent{
			// Opened and closed inside the window.
			{ProxyID: proxyID, ProxyAddress: "127.0.0.1:9104", PoolID: 3, Username: "scraper",
				Host: "www.airbnb.com:443", Domain: "www.airbnb.com",
				BytesUp: 1_000, BytesDown: 50_000, Requests: 420,
				DurationMs: 600_000, OpenedAt: now.Add(-30 * time.Minute)},
			// Opened before the window but closed inside it: its work counts.
			{ProxyID: proxyID, ProxyAddress: "127.0.0.1:9104",
				Host: "www.booking.com:443", Domain: "www.booking.com",
				BytesUp: 500, BytesDown: 10_000,
				DurationMs: 3 * 60 * 60 * 1000, OpenedAt: now.Add(-150 * time.Minute)},
			// Opened and closed entirely before the window.
			{ProxyID: proxyID, ProxyAddress: "127.0.0.1:9104",
				Host: "www.vrbo.com:443", Domain: "www.vrbo.com",
				BytesUp: 9_999, BytesDown: 9_999,
				DurationMs: 1_000, OpenedAt: now.Add(-10 * time.Hour)},
		}
		for _, tn := range tunnels {
			if err := store.InsertTunnel(ctx, tn); err != nil {
				t.Fatalf("InsertTunnel: %v", err)
			}
		}

		stats, err := store.TunnelStats(ctx, time.Hour)
		if err != nil {
			t.Fatalf("TunnelStats: %v", err)
		}
		if stats.Tunnels != 2 {
			t.Errorf("Tunnels: want 2 in the trailing hour, got %d", stats.Tunnels)
		}
		if stats.BytesUp != 1_500 || stats.BytesDown != 60_000 {
			t.Errorf("bytes: want (1500, 60000), got (%d, %d)", stats.BytesUp, stats.BytesDown)
		}
		if stats.Requests != 420 {
			t.Errorf("Requests: want 420, got %d", stats.Requests)
		}

		// 10min + 3h of tunnel lifetime over a 1h window: tunnels were open for
		// more wall-clock than the window itself, which is exactly the signal a
		// bare tunnel count throws away.
		wantMs := int64(600_000 + 3*60*60*1000)
		if stats.TotalDurationMs != wantMs {
			t.Errorf("TotalDurationMs: want %d, got %d", wantMs, stats.TotalDurationMs)
		}
		if got := stats.MeanConcurrency(time.Hour); got < 3.1 || got > 3.2 {
			t.Errorf("MeanConcurrency: want ~3.17, got %v", got)
		}

		// Tunnel volume must never leak into request counts — that conflation is
		// the whole reason tunnels are a separate stream.
		reqStats, err := store.RequestStats(ctx)
		if err != nil {
			t.Fatalf("RequestStats: %v", err)
		}
		if reqStats.RequestsToday != 0 {
			t.Errorf("tunnels leaked into request stats: RequestsToday=%d", reqStats.RequestsToday)
		}
	})
}

func TestApplyRetention(t *testing.T) {
	testStores(t, func(t *testing.T, store Store, files *ParquetStore) {
		ctx := context.Background()
		proxyID := 1

		// Retention goes by the day an event arrived, so a fresh and a
		// backdated event in each table both stay until today expires.
		for _, age := range []time.Duration{0, 20 * 24 * time.Hour} {
			err := store.InsertRequest(ctx, RequestEvent{
				ProxyID: proxyID, ProxyAddress: "127.0.0.1:9999", Method: "GET",
				Success: true, ResponseTime: 100, Timestamp: time.Now().Add(-age),
			})
			if err != nil {
				t.Fatalf("InsertRequest: %v", err)
			}
			err = store.InsertTunnel(ctx, TunnelEvent{
				ProxyID: proxyID, ProxyAddress: "127.0.0.1:9999", Host: "example.com:443",
				OpenedAt: time.Now().Add(-age), DurationMs: 100,
			})
			if err != nil {
				t.Fatalf("InsertTunnel: %v", err)
			}
		}

		cfg := RetentionConfig{RequestRetentionDays: 14}
		if err := store.ApplyRetention(ctx, cfg); err != nil {
			t.Fatalf("ApplyRetention: %v", err)
		}
		if rollup, err := store.ProxyRollup(ctx); err != nil || len(rollup) != 1 || rollup[0].Requests != 2 {
			t.Fatalf("retention removed today's events: %+v %v", rollup, err)
		}
		tunnels, err := store.TunnelStats(ctx, 30*24*time.Hour)
		if err != nil || tunnels.Tunnels != 2 {
			t.Fatalf("retention removed today's tunnels: %+v %v", tunnels, err)
		}

		// Plant files from this store, another instance and an unnamed writer
		// just outside and just inside the period. The kept day gets no log of
		// this store's, which a conversion could replace.
		today := files.partitionDay()
		expired := today - int64(cfg.RequestRetentionDays) - 1
		kept := today - int64(cfg.RequestRetentionDays)
		var expiredPaths, keptPaths []string
		for _, table := range files.tables() {
			for _, instance := range []string{testInstance, "other", ""} {
				expiredPaths = append(expiredPaths,
					table.path(fileKey{expired, instance}, logSuffix), table.path(fileKey{expired, instance}, parquetSuffix))
				keptPaths = append(keptPaths, table.path(fileKey{kept, instance}, parquetSuffix))
			}
			keptPaths = append(keptPaths, table.path(fileKey{kept, "other"}, logSuffix))
		}
		for _, path := range append(expiredPaths, keptPaths...) {
			if err := os.WriteFile(path, nil, 0o640); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.ApplyRetention(ctx, cfg); err != nil {
			t.Fatalf("ApplyRetention: %v", err)
		}
		for _, path := range expiredPaths {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s survived retention: %v", path, err)
			}
		}
		for _, path := range keptPaths {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("retention removed a day inside the period: %v", err)
			}
		}
	})
}

func TestProxyRollupAndLowSuccess(t *testing.T) {
	testStores(t, func(t *testing.T, store Store, _ *ParquetStore) {
		ctx := context.Background()
		good, bad := 1, 2

		now := time.Now()
		insert := func(proxyID int, success bool, respMs int, age time.Duration) {
			t.Helper()
			err := store.InsertRequest(ctx, RequestEvent{
				ProxyID: proxyID, ProxyAddress: "x", Method: "GET",
				Success: success, ResponseTime: respMs, Timestamp: now.Add(-age),
			})
			if err != nil {
				t.Fatalf("insert request: %v", err)
			}
		}

		// good: 3 requests, 2 successes (100ms, 200ms), 1 failure (900ms —
		// must not pollute the success-only average).
		insert(good, true, 100, 0)
		insert(good, true, 200, time.Hour)
		insert(good, false, 900, time.Hour)

		// bad: 12 in-window requests with 2 successes (~17%), plus an ancient
		// all-success streak outside the 7-day window that must not shield it.
		for i := 0; i < 10; i++ {
			insert(bad, false, 50, time.Duration(i)*time.Minute)
		}
		insert(bad, true, 50, time.Hour)
		insert(bad, true, 50, 2*time.Hour)
		for i := 0; i < 20; i++ {
			insert(bad, true, 50, 8*24*time.Hour)
		}

		rollup, err := store.ProxyRollup(ctx)
		if err != nil {
			t.Fatalf("ProxyRollup: %v", err)
		}
		byID := map[int]ProxyRequestStats{}
		for _, st := range rollup {
			byID[st.ProxyID] = st
		}
		g := byID[good]
		if g.Requests != 3 || g.Successes != 2 || g.AvgResponseTime != 150 {
			t.Errorf("good rollup: want (3, 2, 150ms), got (%d, %d, %dms)", g.Requests, g.Successes, g.AvgResponseTime)
		}
		b := byID[bad]
		if b.Requests != 32 || b.Successes != 22 {
			t.Errorf("bad rollup: want (32, 22), got (%d, %d)", b.Requests, b.Successes)
		}

		// Low-success over 7 days at 50% minimum: bad qualifies (12 requests,
		// ~17%), good does not (only 3 in-window requests, below minRequests).
		ids, err := store.LowSuccessProxies(ctx, 7*24*time.Hour, 50, 10)
		if err != nil {
			t.Fatalf("LowSuccessProxies: %v", err)
		}
		if len(ids) != 1 || ids[0] != bad {
			t.Errorf("LowSuccessProxies: want [%d], got %v", bad, ids)
		}
	})
}

func TestTargetFailuresExcludedFromProxyReliability(t *testing.T) {
	testStores(t, func(t *testing.T, store Store, _ *ParquetStore) {
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

func TestTrafficSeries(t *testing.T) {
	testStores(t, func(t *testing.T, store Store, _ *ParquetStore) {
		ctx := context.Background()
		proxyID := 1

		// Plant deterministic traffic inside two known 30m buckets ("24h" range).
		// Offsets keep every event inside its bucket regardless of when the test
		// runs.
		bucketA := time.Now().UTC().Truncate(30 * time.Minute)
		bucketB := bucketA.Add(-2 * time.Hour)

		insert := func(at time.Time, success bool, respMs int) {
			t.Helper()
			err := store.InsertRequest(ctx, RequestEvent{
				ProxyID: proxyID, ProxyAddress: "x", Method: "GET",
				Success: success, ResponseTime: respMs, Timestamp: at,
			})
			if err != nil {
				t.Fatalf("insert request: %v", err)
			}
		}

		// Bucket A: four successes (100/200/200/300ms) and one failure whose huge
		// latency must not leak into the percentiles. One success carries a
		// local-zone timestamp (same instant as bucketA's window): the store
		// must keep instants, not wall-clock digits, or it lands in the wrong
		// bucket on any non-UTC host.
		insert(bucketA.Add(1*time.Minute), true, 100)
		insert(bucketA.Add(2*time.Minute), true, 200)
		insert(time.Now(), true, 200)
		insert(bucketA.Add(3*time.Minute), true, 300)
		insert(bucketA.Add(4*time.Minute), false, 9000)
		// Bucket B: failures only — volume without percentiles.
		insert(bucketB.Add(1*time.Minute), false, 50)
		insert(bucketB.Add(2*time.Minute), false, 50)

		series, err := store.TrafficSeries(ctx, "24h")
		if err != nil {
			t.Fatalf("TrafficSeries: %v", err)
		}

		// Dense series: ~49 buckets of 30m across 24h, strictly ascending, no gaps.
		if len(series) < 47 {
			t.Fatalf("series not dense: got %d points", len(series))
		}
		for i := 1; i < len(series); i++ {
			if got := series[i].Time.Sub(series[i-1].Time); got != 30*time.Minute {
				t.Fatalf("gap between points %d and %d: %v", i-1, i, got)
			}
		}

		byTime := map[int64]models.TrafficPoint{}
		var zeroes int
		for _, p := range series {
			byTime[p.Time.Unix()] = p
			if p.Requests == 0 {
				zeroes++
			}
		}
		if zeroes == 0 {
			t.Error("expected zero-filled quiet buckets in the series")
		}

		a, ok := byTime[bucketA.Unix()]
		if !ok {
			t.Fatalf("bucket A (%v) missing from series", bucketA)
		}
		if a.Requests != 5 || a.Successes != 4 {
			t.Errorf("bucket A volume: want (5, 4), got (%d, %d)", a.Requests, a.Successes)
		}
		if a.P50Ms != 200 {
			t.Errorf("bucket A p50: want 200, got %d", a.P50Ms)
		}
		// Inclusive interpolation puts p95 of [100,200,200,300] at 285; allow a
		// few ms for rounding.
		if a.P95Ms < 280 || a.P95Ms > 300 {
			t.Errorf("bucket A p95: want ~285, got %d", a.P95Ms)
		}

		b, ok := byTime[bucketB.Unix()]
		if !ok {
			t.Fatalf("bucket B (%v) missing from series", bucketB)
		}
		if b.Requests != 2 || b.Successes != 0 {
			t.Errorf("bucket B volume: want (2, 0), got (%d, %d)", b.Requests, b.Successes)
		}
		if b.P50Ms != 0 || b.P95Ms != 0 {
			t.Errorf("bucket B percentiles: want zeros (no successes), got (%d, %d)", b.P50Ms, b.P95Ms)
		}
	})
}
