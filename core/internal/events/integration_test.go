package events

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// These tests exercise the event store against a real Postgres instance and
// are the seed of the backend conformance suite: every Store implementation
// must pass them. Both plain Postgres and TimescaleDB images work — the store
// adapts at runtime. They are skipped unless ROTA_TEST_DB is set, so
// `go test ./...` stays hermetic. To run them locally:
//
//	docker run -d --name pg -e POSTGRES_USER=rota -e POSTGRES_PASSWORD=rota_password \
//	  -e POSTGRES_DB=rota_test -p 55432:5432 timescale/timescaledb:2.22.1-pg17   # or postgres:17
//	ROTA_TEST_DB=1 TEST_DB_PORT=55432 go test ./internal/events/ -run Integration -v
//
// When running integration tests from multiple packages in one invocation,
// pass `-p 1`: the packages share the test database, and Go runs package
// tests in parallel by default (repository tests delete proxies, which
// cascades into rows these tests assert on).

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func testStore(t *testing.T) (*PostgresStore, *database.DB) {
	t.Helper()
	if os.Getenv("ROTA_TEST_DB") == "" {
		t.Skip("set ROTA_TEST_DB=1 (with a running Postgres) to run event store integration tests")
	}
	port, _ := strconv.Atoi(getenv("TEST_DB_PORT", "55432"))
	cfg := &config.DatabaseConfig{
		Host:     getenv("TEST_DB_HOST", "localhost"),
		Port:     port,
		User:     getenv("TEST_DB_USER", "rota"),
		Password: getenv("TEST_DB_PASSWORD", "rota_password"),
		Name:     getenv("TEST_DB_NAME", "rota_test"),
		SSLMode:  "disable",
	}
	db, err := database.New(context.Background(), cfg, database.DefaultConfig(), logger.New("error"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Each test starts from empty event tables.
	ctx := context.Background()
	for _, table := range []string{"logs", "proxy_requests"} {
		if _, err := db.Pool.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}

	return NewPostgresStore(db, logger.New("error")), db
}

// testProxyID inserts a proxy row to satisfy the proxy_requests FK and
// returns its id.
func testProxyID(t *testing.T, db *database.DB) int {
	t.Helper()
	var id int
	err := db.Pool.QueryRow(context.Background(), `
		INSERT INTO proxies (address, protocol)
		VALUES ('127.0.0.1:9999', 'http')
		ON CONFLICT (address, protocol) DO UPDATE SET updated_at = NOW()
		RETURNING id
	`).Scan(&id)
	if err != nil {
		t.Fatalf("insert proxy: %v", err)
	}
	return id
}

func TestIntegration_Logs_InsertListSince(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()

	details := "some details"
	entries := []LogEntry{
		{Level: "info", Message: "plain app log"},
		{Level: "error", Message: "proxy request failed", Details: &details, Source: "proxy"},
		// Source only as a first-class field, absent from metadata: the store
		// must still find it via the source filter.
		{Level: "info", Message: "proxy request ok", Source: "proxy", Metadata: map[string]any{"method": "GET"}},
	}
	for _, e := range entries {
		if err := store.InsertLog(ctx, e); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}

	all, total, err := store.ListLogs(ctx, LogFilter{}, 1, 10)
	if err != nil {
		t.Fatalf("ListLogs: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("ListLogs: want 3 logs, got total=%d len=%d", total, len(all))
	}
	// Newest first.
	if all[0].Message != "proxy request ok" {
		t.Errorf("ListLogs order: want newest first, got %q", all[0].Message)
	}

	proxyLogs, total, err := store.ListLogs(ctx, LogFilter{Source: "proxy"}, 1, 10)
	if err != nil {
		t.Fatalf("ListLogs(source): %v", err)
	}
	if total != 2 || len(proxyLogs) != 2 {
		t.Fatalf("ListLogs(source): want 2 logs, got total=%d len=%d", total, len(proxyLogs))
	}

	errLogs, _, err := store.ListLogs(ctx, LogFilter{Level: "error", Search: "FAILED"}, 1, 10)
	if err != nil {
		t.Fatalf("ListLogs(level+search): %v", err)
	}
	if len(errLogs) != 1 || errLogs[0].Details == nil || *errLogs[0].Details != details {
		t.Fatalf("ListLogs(level+search): want the error log with details, got %+v", errLogs)
	}

	// Streaming cursor: ascending IDs, strictly after lastID.
	since, err := store.LogsSince(ctx, 0, 10, "")
	if err != nil {
		t.Fatalf("LogsSince: %v", err)
	}
	if len(since) != 3 {
		t.Fatalf("LogsSince(0): want 3 logs, got %d", len(since))
	}
	if since[0].ID >= since[2].ID {
		t.Errorf("LogsSince order: want ascending IDs, got %d..%d", since[0].ID, since[2].ID)
	}
	tail, err := store.LogsSince(ctx, since[0].ID, 10, "proxy")
	if err != nil {
		t.Fatalf("LogsSince(cursor): %v", err)
	}
	for _, l := range tail {
		if l.ID <= since[0].ID {
			t.Errorf("LogsSince(cursor): got ID %d <= cursor %d", l.ID, since[0].ID)
		}
	}
}

func TestIntegration_Logs_DeleteOlderThan(t *testing.T) {
	store, db := testStore(t)
	ctx := context.Background()

	if err := store.InsertLog(ctx, LogEntry{Level: "info", Message: "fresh"}); err != nil {
		t.Fatalf("InsertLog: %v", err)
	}
	// Backdate a second log beyond the cutoff.
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO logs (timestamp, level, message) VALUES (NOW() - INTERVAL '3 days', 'info', 'stale')
	`); err != nil {
		t.Fatalf("insert stale log: %v", err)
	}

	deleted, err := store.DeleteLogsOlderThan(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("DeleteLogsOlderThan: %v", err)
	}
	if deleted != 1 {
		t.Errorf("DeleteLogsOlderThan: want 1 deleted, got %d", deleted)
	}
	_, total, err := store.ListLogs(ctx, LogFilter{}, 1, 10)
	if err != nil {
		t.Fatalf("ListLogs: %v", err)
	}
	if total != 1 {
		t.Errorf("after delete: want 1 log remaining, got %d", total)
	}
}

func TestIntegration_Requests_InsertStatsCharts(t *testing.T) {
	store, db := testStore(t)
	ctx := context.Background()
	proxyID := testProxyID(t, db)

	now := time.Now()
	reqs := []RequestEvent{
		{ProxyID: proxyID, ProxyAddress: "127.0.0.1:9999", Method: "GET", URL: "http://example.com",
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
}

func TestIntegration_ApplyRetention(t *testing.T) {
	store, db := testStore(t)
	ctx := context.Background()
	proxyID := testProxyID(t, db)

	// One fresh and one expired row in each event table.
	if err := store.InsertLog(ctx, LogEntry{Level: "info", Message: "fresh"}); err != nil {
		t.Fatalf("InsertLog: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO logs (timestamp, level, message) VALUES (NOW() - INTERVAL '20 days', 'info', 'stale')
	`); err != nil {
		t.Fatalf("insert stale log: %v", err)
	}
	for _, age := range []string{"0 hours", "20 days"} {
		if _, err := db.Pool.Exec(ctx, `
			INSERT INTO proxy_requests (timestamp, proxy_id, proxy_address, method, success, response_time)
			VALUES (NOW() - $1::interval, $2, '127.0.0.1:9999', 'GET', true, 100)
		`, age, proxyID); err != nil {
			t.Fatalf("insert request: %v", err)
		}
	}

	// Must succeed on any backend: installs policies where supported,
	// deletes expired rows otherwise. Never errors just because the backend
	// lacks a feature.
	err := store.ApplyRetention(ctx, RetentionConfig{
		RetentionDays:        14,
		CompressionAfterDays: 3,
		RequestRetentionDays: 14,
	})
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}

	caps, err := store.capabilities(ctx)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if caps.tslPolicies {
		// Policy path: deletion is deferred to Timescale's background jobs,
		// so only assert the policies landed with the configured periods.
		var n int
		err := db.Pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM timescaledb_information.jobs
			WHERE proc_name = 'policy_retention'
			  AND hypertable_name IN ('logs', 'proxy_requests')
			  AND config->>'drop_after' = '14 days'
		`).Scan(&n)
		if err != nil {
			t.Fatalf("query policies: %v", err)
		}
		if n != 2 {
			t.Errorf("want retention policies on logs and proxy_requests with 14 days, got %d", n)
		}
		return
	}

	// Fallback path (plain Postgres / Apache-only builds): expired rows are
	// deleted immediately, fresh rows survive.
	assertRows := func(table string, want int) {
		t.Helper()
		var n int
		if err := db.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s: want %d rows after retention, got %d", table, want, n)
		}
	}
	assertRows("logs", 1)
	assertRows("proxy_requests", 1)
}
