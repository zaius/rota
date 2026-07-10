package events

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// pgTestBackend runs the conformance suite against PostgresStore, on either
// plain Postgres or TimescaleDB — the store adapts at runtime, and
// VerifyRetentionApplied checks whichever mechanism the server offers.
type pgTestBackend struct {
	db    *database.DB
	store *PostgresStore
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newPGTestBackend(t *testing.T) storeBackend {
	t.Helper()
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

	return &pgTestBackend{db: db, store: NewPostgresStore(db, logger.New("error"))}
}

func (b *pgTestBackend) Store() Store { return b.store }

func (b *pgTestBackend) SeedProxy(t *testing.T, address string) int {
	t.Helper()
	var id int
	err := b.db.Pool.QueryRow(context.Background(), `
		INSERT INTO proxies (address, protocol)
		VALUES ($1, 'http')
		ON CONFLICT (address, protocol) DO UPDATE SET updated_at = NOW()
		RETURNING id
	`, address).Scan(&id)
	if err != nil {
		t.Fatalf("insert proxy %s: %v", address, err)
	}
	return id
}

func (b *pgTestBackend) RequestDims(t *testing.T) []rawRequestDims {
	t.Helper()
	rows, err := b.db.Pool.Query(context.Background(),
		`SELECT pool_id, username, domain FROM proxy_requests`)
	if err != nil {
		t.Fatalf("read request dims: %v", err)
	}
	defer rows.Close()

	dims := []rawRequestDims{}
	for rows.Next() {
		var d rawRequestDims
		if err := rows.Scan(&d.PoolID, &d.Username, &d.Domain); err != nil {
			t.Fatalf("scan request dims: %v", err)
		}
		dims = append(dims, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read request dims: %v", err)
	}
	return dims
}

func (b *pgTestBackend) VerifyRetentionApplied(t *testing.T, cfg RetentionConfig, wantLogs, wantRequests int) {
	t.Helper()
	ctx := context.Background()

	caps, err := b.store.capabilities(ctx)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}

	if caps.tslPolicies {
		// Policy path: deletion is deferred to Timescale's background jobs,
		// so assert the policies landed with the configured periods.
		var n int
		err := b.db.Pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM timescaledb_information.jobs
			WHERE proc_name = 'policy_retention'
			  AND (
				(hypertable_name = 'logs'           AND config->>'drop_after' = $1) OR
				(hypertable_name = 'proxy_requests' AND config->>'drop_after' = $2)
			  )
		`, strconv.Itoa(cfg.RetentionDays)+" days", strconv.Itoa(cfg.RequestRetentionDays)+" days").Scan(&n)
		if err != nil {
			t.Fatalf("query policies: %v", err)
		}
		if n != 2 {
			t.Errorf("want retention policies on logs and proxy_requests with configured periods, got %d", n)
		}
		return
	}

	// Fallback path (plain Postgres / Apache-only builds): expired rows are
	// deleted immediately, fresh rows survive.
	assertRows := func(table string, want int) {
		t.Helper()
		var n int
		if err := b.db.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s: want %d rows after retention, got %d", table, want, n)
		}
	}
	assertRows("logs", wantLogs)
	assertRows("proxy_requests", wantRequests)
}
