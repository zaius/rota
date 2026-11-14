package repository

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/zaius/rota/core/internal/config"
	"github.com/zaius/rota/core/internal/database"
	"github.com/zaius/rota/core/pkg/logger"
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

// Regression: with the statement cache off, settings JSON passed as []byte
// did not encode and seeding defaults failed.
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

// Regression: a failed manual test left the proxy active.
func TestIntegration_SetHealthy(t *testing.T) {
	db := testDB(t)
	cleanTables(t, db)
	proxyRepo := NewProxyRepository(db)
	ctx := context.Background()

	var id int
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO proxies (address, protocol, status) VALUES ('1.1.1.1:80','http','active') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert proxy: %v", err)
	}

	for _, healthy := range []bool{false, true} {
		if err := proxyRepo.SetHealthy(ctx, id, healthy); err != nil {
			t.Fatalf("SetHealthy(%v): %v", healthy, err)
		}
		var status string
		if err := db.Pool.QueryRow(ctx, `SELECT status FROM proxies WHERE id=$1`, id).Scan(&status); err != nil {
			t.Fatalf("read proxy status: %v", err)
		}
		if want := map[bool]string{false: "failed", true: "active"}[healthy]; status != want {
			t.Fatalf("SetHealthy(%v): status=%q, want %q", healthy, status, want)
		}
	}
}
