package repository

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// These tests exercise the real SQL against a Postgres/TimescaleDB instance.
// They are skipped unless ROTA_TEST_DB is set, so `go test ./...` stays
// hermetic. To run them locally:
//
//	docker run -d --name pg -e POSTGRES_USER=rota -e POSTGRES_PASSWORD=rota_password \
//	  -e POSTGRES_DB=rota_test -p 55432:5432 timescale/timescaledb:2.22.1-pg17
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
		`TRUNCATE pool_proxies, pool_geo_filters, pool_isp_filters, pool_tag_filters, proxy_pools, proxies RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("clean: %v", err)
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

func TestIntegration_AddProxies_BatchDedupes(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	proxyRepo := NewProxyRepository(db)
	poolRepo := NewPoolRepository(db)
	ctx := context.Background()

	pool, err := poolRepo.Create(ctx, models.CreatePoolRequest{Name: "p1", RotationMethod: "roundrobin", StickCount: 1})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	var ids []int
	for i := 0; i < 4; i++ {
		p, err := proxyRepo.Create(ctx, models.CreateProxyRequest{Address: "10.0.0." + strconv.Itoa(i) + ":80", Protocol: "http"})
		if err != nil {
			t.Fatalf("create proxy %d: %v", i, err)
		}
		ids = append(ids, p.ID)
	}

	if err := poolRepo.AddProxies(ctx, pool.ID, ids[:3]); err != nil {
		t.Fatalf("add first batch: %v", err)
	}
	// Overlapping batch must not error (ON CONFLICT DO NOTHING) and must dedupe.
	if err := poolRepo.AddProxies(ctx, pool.ID, ids[1:]); err != nil {
		t.Fatalf("add overlapping batch: %v", err)
	}

	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM pool_proxies WHERE pool_id=$1`, pool.ID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 4 {
		t.Fatalf("expected 4 members after dedupe, got %d", count)
	}
}

func TestIntegration_SyncPoolByFilters_RebuildsMembership(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	poolRepo := NewPoolRepository(db)
	ctx := context.Background()

	// Two US proxies, one DE proxy.
	mustProxy(t, db, "us1:80", "US")
	mustProxy(t, db, "us2:80", "US")
	deID := mustProxy(t, db, "de1:80", "DE")

	pool, err := poolRepo.Create(ctx, models.CreatePoolRequest{Name: "us-pool", RotationMethod: "roundrobin", StickCount: 1, SyncMode: "auto"})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if err := poolRepo.SetGeoFilters(ctx, pool.ID, []models.GeoFilter{{CountryCode: "US"}}); err != nil {
		t.Fatalf("set geo filters: %v", err)
	}

	total, newIDs, err := poolRepo.SyncPoolByFilters(ctx, *pool)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if total != 2 || len(newIDs) != 2 {
		t.Fatalf("expected 2 members (both new), got total=%d new=%d", total, len(newIDs))
	}

	// The DE proxy must not be a member.
	var isMember bool
	if err := db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pool_proxies WHERE pool_id=$1 AND proxy_id=$2)`, pool.ID, deID).Scan(&isMember); err != nil {
		t.Fatalf("member check: %v", err)
	}
	if isMember {
		t.Fatal("DE proxy should not be in the US pool")
	}

	// Re-sync is idempotent: same membership, and nothing reported as newly added.
	total2, newIDs2, err := poolRepo.SyncPoolByFilters(ctx, *pool)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if total2 != 2 || len(newIDs2) != 0 {
		t.Fatalf("re-sync should be idempotent, got total=%d new=%d", total2, len(newIDs2))
	}
}

func TestIntegration_SetGeoFilters_ReplacesAtomically(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	poolRepo := NewPoolRepository(db)
	ctx := context.Background()

	pool, err := poolRepo.Create(ctx, models.CreatePoolRequest{Name: "p", RotationMethod: "roundrobin", StickCount: 1})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	if err := poolRepo.SetGeoFilters(ctx, pool.ID, []models.GeoFilter{{CountryCode: "US"}, {CountryCode: "DE"}}); err != nil {
		t.Fatalf("set 1: %v", err)
	}
	if err := poolRepo.SetGeoFilters(ctx, pool.ID, []models.GeoFilter{{CountryCode: "FR"}}); err != nil {
		t.Fatalf("set 2: %v", err)
	}

	got, err := poolRepo.GetGeoFilters(ctx, pool.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 || got[0].CountryCode != "FR" {
		t.Fatalf("expected only FR after replace, got %+v", got)
	}
}

// Covers the queries moved out of the handler/service layer into the
// repository (ListAll, ListUngeotaggedAddresses, UpdateGeo, UpdateStatus, and
// the pool filter-builder's ListKnownISPs/ListKnownTags).
func TestIntegration_ProxyRepoMethods(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	proxyRepo := NewProxyRepository(db)
	poolRepo := NewPoolRepository(db)
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

	// ListKnownISPs (pool filter-builder) sees the ISP we just wrote.
	isps, err := poolRepo.ListKnownISPs(ctx, "cloud")
	if err != nil || len(isps) != 1 || isps[0] != "Cloudflare" {
		t.Fatalf("ListKnownISPs: got %v (err %v), want [Cloudflare]", isps, err)
	}

	// ListKnownTags returns the distinct tags.
	tags, err := poolRepo.ListKnownTags(ctx)
	if err != nil {
		t.Fatalf("ListKnownTags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("expected 2 distinct tags, got %d (%v)", len(tags), tags)
	}

	// UpdateStatus flips a proxy's status.
	if err := proxyRepo.UpdateStatus(ctx, taggedID, "failed"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	var status string
	db.Pool.QueryRow(ctx, `SELECT status FROM proxies WHERE id=$1`, taggedID).Scan(&status)
	if status != "failed" {
		t.Fatalf("expected status failed, got %q", status)
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

// mustPool inserts a bare pool row and returns its id.
func mustPool(t *testing.T, db *database.DB, name string) int {
	t.Helper()
	var id int
	err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO proxy_pools (name) VALUES ($1) RETURNING id`, name).Scan(&id)
	if err != nil {
		t.Fatalf("insert pool %s: %v", name, err)
	}
	return id
}

// PUT /proxy-users must be a partial update: fields absent from the JSON
// document keep their current values, explicit null clears. A bare
// {"enabled":...} toggle (as the dashboard sends) must not wipe pool
// assignments — it used to.
func TestIntegration_UserUpdate_PartialFields(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	repo := NewUserRepository(db)
	ctx := context.Background()

	p1 := mustPool(t, db, "main-pool")
	p2 := mustPool(t, db, "fallback-pool")

	created, err := repo.Create(ctx, models.CreateProxyUserRequest{
		Username:        "alice",
		Password:        "alicepw123",
		Enabled:         true,
		MainPoolID:      &p1,
		FallbackPoolIDs: []int{p2},
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// update decodes a wire document and applies it, mirroring the handler.
	update := func(doc string) *models.ProxyUser {
		t.Helper()
		var req models.UpdateProxyUserRequest
		if err := json.Unmarshal([]byte(doc), &req); err != nil {
			t.Fatalf("unmarshal %s: %v", doc, err)
		}
		u, err := repo.Update(ctx, created.ID, req)
		if err != nil || u == nil {
			t.Fatalf("update %s: %v", doc, err)
		}
		return u
	}

	// A bare toggle keeps every other field.
	u := update(`{"enabled":false}`)
	if u.Enabled {
		t.Error("toggle: enabled should be false")
	}
	if u.MainPoolID == nil || *u.MainPoolID != p1 {
		t.Errorf("toggle: main_pool_id wiped, got %v want %d", u.MainPoolID, p1)
	}
	if len(u.FallbackPoolIDs) != 1 || u.FallbackPoolIDs[0] != p2 {
		t.Errorf("toggle: fallback_pool_ids wiped, got %v want [%d]", u.FallbackPoolIDs, p2)
	}

	// Explicit null clears the main pool; omitted fallbacks stay.
	u = update(`{"main_pool_id":null}`)
	if u.MainPoolID != nil {
		t.Errorf("null: main_pool_id should be cleared, got %v", *u.MainPoolID)
	}
	if len(u.FallbackPoolIDs) != 1 {
		t.Errorf("null: fallback_pool_ids should be kept, got %v", u.FallbackPoolIDs)
	}

	// Explicit values apply; explicit empty list clears.
	u = update(`{"main_pool_id":` + strconv.Itoa(p2) + `,"fallback_pool_ids":[]}`)
	if u.MainPoolID == nil || *u.MainPoolID != p2 {
		t.Errorf("set: main_pool_id got %v want %d", u.MainPoolID, p2)
	}
	if len(u.FallbackPoolIDs) != 0 {
		t.Errorf("set: fallback_pool_ids should be empty, got %v", u.FallbackPoolIDs)
	}
}
