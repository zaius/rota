package events

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

// This file is the event-store conformance suite: every Store implementation
// must pass it. Test bodies talk only to the Store interface plus the small
// storeBackend hook set below — no backend-specific SQL — so a new backend
// (e.g. ClickHouse) joins by adding a case to newTestBackend, with zero new
// tests.
//
// The suite needs a real database and is skipped unless ROTA_TEST_DB is set,
// so `go test ./...` stays hermetic. To run it locally:
//
//	docker run -d --name pg -e POSTGRES_USER=rota -e POSTGRES_PASSWORD=rota_password \
//	  -e POSTGRES_DB=rota_test -p 55432:5432 timescale/timescaledb:2.22.1-pg17   # or postgres:17
//	ROTA_TEST_DB=1 TEST_DB_PORT=55432 go test ./internal/events/ -run Integration -v
//
// When running integration tests from multiple packages in one invocation,
// pass `-p 1`: the packages share the test database, and Go runs package
// tests in parallel by default (repository tests delete proxies, which
// cascades into rows these tests assert on).

// rawRequestDims is the stored dimension tuple of one request event. Nil
// means the backend stored "not applicable" (NULL or its equivalent).
type rawRequestDims struct {
	PoolID   *int
	Username *string
	Domain   *string
}

// storeBackend gives the conformance suite out-of-band access to the backend
// under test, for the few things the Store interface deliberately does not
// expose.
type storeBackend interface {
	// Store returns the store under test, with empty event tables.
	Store() Store

	// SeedProxy ensures a proxy with the given address exists for request
	// events to reference, returning its ID. Backends without referential
	// integrity may simply fabricate a unique ID.
	SeedProxy(t *testing.T, address string) int

	// RequestDims returns the stored (pool, username, domain) dimensions of
	// every request event, in any order.
	RequestDims(t *testing.T) []rawRequestDims

	// VerifyRetentionApplied asserts that cfg took effect after an
	// ApplyRetention call: delete-based backends check that only the
	// unexpired rows remain (wantRequests/wantTunnels), policy- or TTL-based
	// backends check their native mechanism is configured with cfg's
	// periods.
	VerifyRetentionApplied(t *testing.T, cfg RetentionConfig, wantRequests, wantTunnels int)
}

// newTestBackend returns the backend selected by EVENT_STORE_TEST (default
// "postgres"), skipping unless ROTA_TEST_DB is set.
func newTestBackend(t *testing.T) storeBackend {
	t.Helper()
	if os.Getenv("ROTA_TEST_DB") == "" {
		t.Skip("set ROTA_TEST_DB=1 (with a running database) to run event store conformance tests")
	}
	switch backend := os.Getenv("EVENT_STORE_TEST"); backend {
	case "", "postgres":
		return newPGTestBackend(t)
	case "clickhouse":
		return newCHTestBackend(t)
	default:
		t.Fatalf("unknown EVENT_STORE_TEST %q", backend)
		return nil
	}
}

func TestIntegration_Requests_InsertStatsCharts(t *testing.T) {
	backend := newTestBackend(t)
	store := backend.Store()
	ctx := context.Background()
	proxyID := backend.SeedProxy(t, "127.0.0.1:9999")

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

	// Dimensions round-trip; zero values are stored as "not applicable".
	dims := backend.RequestDims(t)
	var withDims, nullDims int
	for _, d := range dims {
		switch {
		case d.PoolID != nil && d.Username != nil && d.Domain != nil:
			withDims++
			if *d.PoolID != 7 || *d.Username != "alice" || *d.Domain != "example.com" {
				t.Errorf("dimensions: want (7, alice, example.com), got (%v, %v, %v)",
					*d.PoolID, *d.Username, *d.Domain)
			}
		case d.PoolID == nil && d.Username == nil && d.Domain == nil:
			nullDims++
		default:
			t.Errorf("mixed dimension tuple: %+v", d)
		}
	}
	if withDims != 1 || nullDims != 2 {
		t.Errorf("want 1 dimensioned + 2 null-dimension rows, got %d + %d", withDims, nullDims)
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
}

func TestIntegration_Tunnels_InsertAndStats(t *testing.T) {
	backend := newTestBackend(t)
	store := backend.Store()
	ctx := context.Background()
	proxyID := backend.SeedProxy(t, "127.0.0.1:9104")

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
}

func TestIntegration_ApplyRetention(t *testing.T) {
	backend := newTestBackend(t)
	store := backend.Store()
	ctx := context.Background()
	proxyID := backend.SeedProxy(t, "127.0.0.1:9999")

	// One fresh and one expired row in each event table.
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

	// Must succeed on any backend; how retention takes effect is the
	// backend's business, checked by VerifyRetentionApplied.
	cfg := RetentionConfig{
		RequestRetentionDays: 14,
	}
	if err := store.ApplyRetention(ctx, cfg); err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}

	backend.VerifyRetentionApplied(t, cfg, 1, 1)
}

func TestIntegration_ProxyRollupAndLowSuccess(t *testing.T) {
	backend := newTestBackend(t)
	store := backend.Store()
	ctx := context.Background()
	good := backend.SeedProxy(t, "127.0.0.1:9101")
	bad := backend.SeedProxy(t, "127.0.0.1:9102")

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
}

func TestIntegration_TrafficSeries(t *testing.T) {
	backend := newTestBackend(t)
	store := backend.Store()
	ctx := context.Background()
	proxyID := backend.SeedProxy(t, "127.0.0.1:9103")

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
	// local-zone timestamp (same instant as bucketA's window): backends must
	// store instants, not wall-clock digits, or it lands in the wrong bucket
	// on any non-UTC host.
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
	// few ms so backend rounding differences don't matter.
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
}
