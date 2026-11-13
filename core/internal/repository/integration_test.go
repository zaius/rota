package repository

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// These tests exercise the real SQL against a Postgres instance.
// They are skipped unless ROTA_TEST_DB is set, so `go test ./...` stays
// hermetic. To run them locally:
//
//	docker run -d --name pg -e POSTGRES_USER=rota -e POSTGRES_PASSWORD=rota_password \
//	  -e POSTGRES_DB=rota_test -p 55432:5432 postgres:17-alpine
//	ROTA_TEST_DB=1 TEST_DB_PORT=55432 go test ./internal/repository/ -run Integration -v

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func testDB(t *testing.T) *database.DB {
	t.Helper()
	if os.Getenv("ROTA_TEST_DB") == "" {
		t.Skip("set ROTA_TEST_DB=1 (with a running Postgres) to run repository integration tests")
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
	if err := db.Migrate(context.Background()); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// cleanTables wipes the tables the repository tests touch.
func cleanTables(t *testing.T, db *database.DB) {
	t.Helper()
	_, err := db.Pool.Exec(context.Background(),
		`TRUNCATE proxies RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
}

func TestIntegration_SeedDefaultsJSON(t *testing.T) {
	db := testDB(t)
	repo := NewSettingsRepository(db)
	ctx := context.Background()
	key := "healthcheck"
	if _, err := db.Pool.Exec(ctx, `DELETE FROM settings WHERE key = $1`, key); err != nil {
		t.Fatalf("clear default setting: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Pool.Exec(ctx, `DELETE FROM settings WHERE key = $1`, key); err != nil {
			t.Errorf("clean default setting: %v", err)
		}
	})

	if err := repo.SeedDefaults(ctx); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}
	value, err := repo.Get(ctx, key)
	if err != nil {
		t.Fatalf("get default setting: %v", err)
	}
	if value["url"] != "https://api.ipify.org" {
		t.Fatalf("unexpected setting: %#v", value)
	}
}

func TestIntegration_Upsert_CreatesThenUpdates(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	repo := NewProxyRepository(db)
	ctx := context.Background()

	user := "alice"
	id, status, err := repo.Upsert(ctx, models.CreateProxyRequest{Address: "1.2.3.4:8080", Protocol: "http", Username: &user})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if status != "created" || id == 0 {
		t.Fatalf("expected created with id, got status=%q id=%d", status, id)
	}

	user2 := "bob"
	id2, status2, err := repo.Upsert(ctx, models.CreateProxyRequest{Address: "1.2.3.4:8080", Protocol: "http", Username: &user2})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if status2 != "updated" {
		t.Fatalf("expected updated, got %q", status2)
	}
	if id2 != id {
		t.Fatalf("expected same id on update, got %d != %d", id2, id)
	}

	var got string
	if err := db.Pool.QueryRow(ctx, `SELECT username FROM proxies WHERE id=$1`, id).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != "bob" {
		t.Fatalf("expected username updated to bob, got %q", got)
	}
}

// Covers ListAll, ListUngeotaggedAddresses, UpdateGeo, ListTags and
// RecordCheckResult.
func TestIntegration_ProxyRepoMethods(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	proxyRepo := NewProxyRepository(db)
	ctx := context.Background()

	// Two ungeotagged proxies (one tagged), one already-geotagged.
	var taggedID int
	err := db.Pool.QueryRow(ctx,
		`INSERT INTO proxies (address, protocol, status, tags) VALUES ('1.1.1.1:80','http','active','{"fast","us"}') RETURNING id`).Scan(&taggedID)
	if err != nil {
		t.Fatalf("insert tagged: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO proxies (address, protocol, status) VALUES ('2.2.2.2:80','http','idle')`); err != nil {
		t.Fatalf("insert plain: %v", err)
	}
	mustProxy(t, db, "3.3.3.3:80", "DE") // already has country_code

	// ListAll returns every proxy.
	all, err := proxyRepo.ListAll(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("ListAll: got %d (err %v), want 3", len(all), err)
	}

	// ListUngeotaggedAddresses returns only the two without country_code.
	ungeo, err := proxyRepo.ListUngeotaggedAddresses(ctx, 100)
	if err != nil {
		t.Fatalf("ListUngeotagged: %v", err)
	}
	if len(ungeo) != 2 {
		t.Fatalf("expected 2 ungeotagged, got %d (%v)", len(ungeo), ungeo)
	}

	// UpdateGeo writes geo (incl. ISP) and moves a proxy out of the ungeotagged set.
	if err := proxyRepo.UpdateGeo(ctx, "1.1.1.1:80", models.GeoInfo{CountryCode: "US", CountryName: "United States", ISP: "Cloudflare"}); err != nil {
		t.Fatalf("UpdateGeo: %v", err)
	}
	ungeo2, _ := proxyRepo.ListUngeotaggedAddresses(ctx, 100)
	if len(ungeo2) != 1 {
		t.Fatalf("after UpdateGeo expected 1 ungeotagged, got %d", len(ungeo2))
	}

	// ListTags returns the distinct tags.
	tags, err := proxyRepo.ListTags(ctx)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("expected 2 distinct tags, got %d (%v)", len(tags), tags)
	}

	// A failed check marks the proxy failed at once and extends its streak;
	// a passing check reactivates it and clears the streak and error.
	var (
		status  string
		streak  int
		lastErr *string
	)
	readState := func() {
		t.Helper()
		if err := db.Pool.QueryRow(ctx,
			`SELECT status, failed_requests, last_error FROM proxies WHERE id=$1`, taggedID,
		).Scan(&status, &streak, &lastErr); err != nil {
			t.Fatalf("read proxy state: %v", err)
		}
	}
	for i := 1; i <= 2; i++ {
		if err := proxyRepo.RecordCheckResult(ctx, taggedID, false, "connection refused"); err != nil {
			t.Fatalf("RecordCheckResult(fail): %v", err)
		}
		readState()
		if status != "failed" || streak != i || lastErr == nil || *lastErr != "connection refused" {
			t.Fatalf("after failed check %d: status=%q streak=%d last_error=%v", i, status, streak, lastErr)
		}
	}
	if err := proxyRepo.RecordCheckResult(ctx, taggedID, true, ""); err != nil {
		t.Fatalf("RecordCheckResult(success): %v", err)
	}
	readState()
	if status != "active" || streak != 0 || lastErr != nil {
		t.Fatalf("after passing check: status=%q streak=%d last_error=%v", status, streak, lastErr)
	}
}

func TestIntegration_Migrate_Idempotent(t *testing.T) {
	db := testDB(t)
	// testDB already migrated once; a second run must be a clean no-op.
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	status, err := db.GetMigrationStatus(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, s := range status {
		if applied, _ := s["applied"].(bool); !applied {
			t.Fatalf("migration %v reported not applied after Migrate", s["version"])
		}
	}
}

// mustProxy inserts a proxy row with a country code and returns its id.
func mustProxy(t *testing.T, db *database.DB, address, country string) int {
	t.Helper()
	var id int
	err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO proxies (address, protocol, status, country_code) VALUES ($1,'http','active',$2) RETURNING id`,
		address, country).Scan(&id)
	if err != nil {
		t.Fatalf("insert proxy %s: %v", address, err)
	}
	return id
}

// ApplyRequestStats denormalizes event-derived aggregates onto proxies:
// listed proxies get the new numbers, unlisted proxies are zeroed (the
// columns mean "within the event window"), and a second identical apply
// writes nothing.
func TestIntegration_ApplyRequestStats(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	repo := NewProxyRepository(db)
	ctx := context.Background()

	p1 := mustProxy(t, db, "10.0.0.1:1", "US")
	p2 := mustProxy(t, db, "10.0.0.2:1", "US")

	// p2 has stale nonzero counters from a previous era; the rollup no
	// longer contains it, so they must be zeroed.
	if _, err := db.Pool.Exec(ctx,
		`UPDATE proxies SET requests = 500, successful_requests = 400, avg_response_time = 80 WHERE id = $1`,
		p2); err != nil {
		t.Fatalf("seed stale stats: %v", err)
	}

	stats := []events.ProxyRequestStats{
		{ProxyID: p1, Requests: 42, Successes: 40, AvgResponseTime: 123},
	}
	written, err := repo.ApplyRequestStats(ctx, stats)
	if err != nil {
		t.Fatalf("ApplyRequestStats: %v", err)
	}
	if written != 2 { // p1 updated + p2 zeroed
		t.Errorf("first apply: want 2 rows written, got %d", written)
	}

	assertStats := func(id int, wantReq, wantOK int64, wantAvg int) {
		t.Helper()
		var req, ok int64
		var avg int
		err := db.Pool.QueryRow(ctx,
			`SELECT requests, successful_requests, COALESCE(avg_response_time,0) FROM proxies WHERE id = $1`,
			id).Scan(&req, &ok, &avg)
		if err != nil {
			t.Fatalf("read stats: %v", err)
		}
		if req != wantReq || ok != wantOK || avg != wantAvg {
			t.Errorf("proxy %d: want (%d, %d, %d), got (%d, %d, %d)", id, wantReq, wantOK, wantAvg, req, ok, avg)
		}
	}
	assertStats(p1, 42, 40, 123)
	assertStats(p2, 0, 0, 0)

	// Idempotent: unchanged values write no rows.
	written, err = repo.ApplyRequestStats(ctx, stats)
	if err != nil {
		t.Fatalf("ApplyRequestStats (repeat): %v", err)
	}
	if written != 0 {
		t.Errorf("repeat apply: want 0 rows written, got %d", written)
	}
}
