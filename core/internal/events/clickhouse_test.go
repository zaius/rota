package events

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// chTestBackend runs the conformance suite against ClickHouseStore. Run it
// with EVENT_STORE_TEST=clickhouse against a local server:
//
//	docker run -d --name ch -e CLICKHOUSE_USER=rota -e CLICKHOUSE_PASSWORD=rota_password \
//	  -e CLICKHOUSE_DB=rota_test -p 59000:9000 clickhouse/clickhouse-server:25.3
//	ROTA_TEST_DB=1 EVENT_STORE_TEST=clickhouse go test ./internal/events/ -run Integration -v
type chTestBackend struct {
	store *ClickHouseStore

	// ClickHouse has no proxies table (control plane stays in Postgres), so
	// SeedProxy just fabricates stable per-address IDs.
	proxyIDs map[string]int
}

func newCHTestBackend(t *testing.T) storeBackend {
	t.Helper()
	port, _ := strconv.Atoi(getenv("TEST_CH_PORT", "59000"))
	cfg := &config.ClickHouseConfig{
		Host:     getenv("TEST_CH_HOST", "localhost"),
		Port:     port,
		User:     getenv("TEST_CH_USER", "rota"),
		Password: getenv("TEST_CH_PASSWORD", "rota_password"),
		Name:     getenv("TEST_CH_NAME", "rota_test"),
	}

	ctx := context.Background()
	store, err := NewClickHouseStore(ctx, cfg, logger.New("error"))
	if err != nil {
		t.Fatalf("connect clickhouse: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// Each test starts from empty tables, with TTLs back at their bootstrap
	// defaults — a previous test may have shortened them below the age of
	// rows this test plants.
	for _, stmt := range []string{
		"TRUNCATE TABLE logs",
		"TRUNCATE TABLE proxy_requests",
		"ALTER TABLE logs MODIFY TTL toDateTime(timestamp) + toIntervalDay(30)",
		"ALTER TABLE proxy_requests MODIFY TTL toDateTime(timestamp) + toIntervalDay(90)",
	} {
		if err := store.conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}

	return &chTestBackend{store: store, proxyIDs: map[string]int{}}
}

func (b *chTestBackend) Store() Store { return b.store }

func (b *chTestBackend) SeedProxy(t *testing.T, address string) int {
	t.Helper()
	if id, ok := b.proxyIDs[address]; ok {
		return id
	}
	id := len(b.proxyIDs) + 1
	b.proxyIDs[address] = id
	return id
}

func (b *chTestBackend) RequestDims(t *testing.T) []rawRequestDims {
	t.Helper()
	rows, err := b.store.conn.Query(context.Background(),
		`SELECT pool_id, username, domain FROM proxy_requests`)
	if err != nil {
		t.Fatalf("read request dims: %v", err)
	}
	defer rows.Close()

	dims := []rawRequestDims{}
	for rows.Next() {
		var poolID int32
		var username, domain string
		if err := rows.Scan(&poolID, &username, &domain); err != nil {
			t.Fatalf("scan request dims: %v", err)
		}
		// This backend stores "not applicable" as the zero value; translate
		// to the suite's nil convention.
		var d rawRequestDims
		if poolID != 0 {
			p := int(poolID)
			d.PoolID = &p
		}
		if username != "" {
			d.Username = &username
		}
		if domain != "" {
			d.Domain = &domain
		}
		dims = append(dims, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read request dims: %v", err)
	}
	return dims
}

func (b *chTestBackend) VerifyRetentionApplied(t *testing.T, cfg RetentionConfig, wantLogs, wantRequests int) {
	t.Helper()
	ctx := context.Background()

	// TTL-based backend: rows expire in background merges, so assert the
	// TTL expressions carry the configured periods rather than counting rows.
	assertTTL := func(table string, days int) {
		t.Helper()
		var createQuery string
		err := b.store.conn.QueryRow(ctx, `
			SELECT create_table_query FROM system.tables
			WHERE database = currentDatabase() AND name = ?
		`, table).Scan(&createQuery)
		if err != nil {
			t.Fatalf("read %s DDL: %v", table, err)
		}
		want := "toIntervalDay(" + strconv.Itoa(days) + ")"
		if !strings.Contains(createQuery, want) {
			t.Errorf("%s: TTL not updated to %s: %s", table, want, createQuery)
		}
	}
	assertTTL("logs", cfg.RetentionDays)
	assertTTL("proxy_requests", cfg.RequestRetentionDays)
}
