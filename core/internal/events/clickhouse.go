package events

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// ClickHouseStore implements Store on ClickHouse (native protocol).
//
// Storage conventions differ from the Postgres backend where ClickHouse
// idioms differ from SQL ones:
//   - dimension "not applicable" is the zero value (0 / ”), not NULL —
//     Nullable columns cost a null-mask per column and are avoided;
//   - retention is a table TTL instead of policies or DELETEs;
//   - inserts run with async_insert so the server batches the one-row-per-
//     request write pattern into sane parts. wait_for_async_insert=1 keeps
//     read-your-writes semantics (an insert returns once its batch is
//     committed, up to ~200ms); flip it to 0 for maximum throughput if
//     dashboard statistics may lag a beat.
type ClickHouseStore struct {
	conn   driver.Conn
	logger *logger.Logger
}

var _ Store = (*ClickHouseStore)(nil)

// chSchema is the idempotent bootstrap DDL. TTLs here are the initial
// defaults; ApplyRetention keeps them in sync with the retention config.
var chSchema = []string{
	`CREATE TABLE IF NOT EXISTS proxy_requests (
		timestamp     DateTime64(3),
		proxy_id      Int32,
		proxy_address String,
		pool_id       Int32,
		username      LowCardinality(String),
		method        LowCardinality(String),
		url           String,
		domain        String,
		status_code   UInt16,
		response_time Int32,
		success       Bool,
		error         String
	) ENGINE = MergeTree
	PARTITION BY toYYYYMMDD(timestamp)
	ORDER BY (proxy_id, timestamp)
	TTL toDateTime(timestamp) + toIntervalDay(90)`,

	`CREATE TABLE IF NOT EXISTS proxy_tunnels (
		timestamp     DateTime64(3),
		proxy_id      Int32,
		proxy_address String,
		pool_id       Int32,
		username      LowCardinality(String),
		host          String,
		domain        String,
		bytes_up      Int64,
		bytes_down    Int64,
		requests      Int32,
		duration_ms   Int32,
		error         String
	) ENGINE = MergeTree
	PARTITION BY toYYYYMMDD(timestamp)
	ORDER BY (proxy_id, timestamp)
	TTL toDateTime(timestamp) + toIntervalDay(90)`,
}

// NewClickHouseStore connects to ClickHouse, verifies the connection and
// bootstraps the schema.
func NewClickHouseStore(ctx context.Context, cfg *config.ClickHouseConfig, log *logger.Logger) (*ClickHouseStore, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)},
		Auth: clickhouse.Auth{
			Database: cfg.Name,
			Username: cfg.User,
			Password: cfg.Password,
		},
		Settings: clickhouse.Settings{
			"async_insert":          1,
			"wait_for_async_insert": 1,
		},
		Compression: &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open clickhouse connection: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping clickhouse: %w", err)
	}

	s := &ClickHouseStore{conn: conn, logger: log}
	for _, ddl := range chSchema {
		if err := conn.Exec(ctx, ddl); err != nil {
			return nil, fmt.Errorf("failed to bootstrap clickhouse schema: %w", err)
		}
	}

	log.Info("clickhouse event store ready", "host", cfg.Host, "database", cfg.Name)
	return s, nil
}

// Close closes the connection.
func (s *ClickHouseStore) Close() error { return s.conn.Close() }

// InsertRequest records one proxied request outcome.
func (s *ClickHouseStore) InsertRequest(ctx context.Context, event RequestEvent) error {
	statusCode := event.StatusCode
	if statusCode < 0 || statusCode > 65535 {
		statusCode = 0
	}

	err := s.conn.Exec(ctx, `
		INSERT INTO proxy_requests (
			timestamp, proxy_id, proxy_address, pool_id, username,
			method, url, domain, status_code, response_time, success, error
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		event.Timestamp,
		int32(event.ProxyID),
		event.ProxyAddress,
		int32(event.PoolID),
		event.Username,
		event.Method,
		event.URL,
		event.Domain,
		uint16(statusCode),
		int32(event.ResponseTime),
		event.Success,
		event.Error,
	)
	if err != nil {
		return fmt.Errorf("failed to insert proxy request: %w", err)
	}
	return nil
}

// RequestStats returns today/yesterday request aggregates for the dashboard.
func (s *ClickHouseStore) RequestStats(ctx context.Context) (*RequestStats, error) {
	// Rates and averages are derived in Go from counts and sums: aggregate
	// functions over empty sets return NaN in ClickHouse, which coalesce()
	// does not catch.
	var (
		reqToday, okToday, msToday uint64
		reqYday, okYday, msYday    uint64
	)
	err := s.conn.QueryRow(ctx, `
		SELECT
			countIf(timestamp >= now() - toIntervalDay(1)),
			countIf(success AND timestamp >= now() - toIntervalDay(1)),
			toUInt64(sumIf(response_time, timestamp >= now() - toIntervalDay(1))),
			countIf(timestamp < now() - toIntervalDay(1)),
			countIf(success AND timestamp < now() - toIntervalDay(1)),
			toUInt64(sumIf(response_time, timestamp < now() - toIntervalDay(1)))
		FROM proxy_requests
		WHERE timestamp >= now() - toIntervalDay(2)
	`).Scan(&reqToday, &okToday, &msToday, &reqYday, &okYday, &msYday)
	if err != nil {
		return nil, fmt.Errorf("failed to get request stats: %w", err)
	}

	stats := &RequestStats{
		RequestsToday:     int64(reqToday),
		RequestsYesterday: int64(reqYday),
	}
	if reqToday > 0 {
		stats.SuccessRateToday = float64(okToday) * 100 / float64(reqToday)
		stats.ResponseTimeToday = int(msToday / reqToday)
	}
	if reqYday > 0 {
		stats.SuccessRateYesterday = float64(okYday) * 100 / float64(reqYday)
		stats.ResponseTimeYesterday = int(msYday / reqYday)
	}
	return stats, nil
}

// InsertTunnel records one completed CONNECT tunnel.
func (s *ClickHouseStore) InsertTunnel(ctx context.Context, event TunnelEvent) error {
	err := s.conn.Exec(ctx, `
		INSERT INTO proxy_tunnels (
			timestamp, proxy_id, proxy_address, pool_id, username,
			host, domain, bytes_up, bytes_down, requests, duration_ms, error
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		event.OpenedAt,
		int32(event.ProxyID),
		event.ProxyAddress,
		int32(event.PoolID),
		event.Username,
		event.Host,
		event.Domain,
		event.BytesUp,
		event.BytesDown,
		int32(event.Requests),
		int32(event.DurationMs),
		event.Error,
	)
	if err != nil {
		return fmt.Errorf("failed to insert proxy tunnel: %w", err)
	}
	return nil
}

// TunnelStats aggregates tunnels that closed within the trailing window.
func (s *ClickHouseStore) TunnelStats(ctx context.Context, window time.Duration) (*TunnelSummary, error) {
	// Bucketed by close time, not open time: a tunnel opened before the
	// window but closed inside it did its work in the window.
	var (
		tunnels                       uint64
		bytesUp, bytesDown, durations int64
		requests                      int64
	)
	err := s.conn.QueryRow(ctx, `
		SELECT
			count(),
			toInt64(sum(bytes_up)),
			toInt64(sum(bytes_down)),
			toInt64(sum(requests)),
			toInt64(sum(duration_ms))
		FROM proxy_tunnels
		WHERE timestamp + toIntervalMillisecond(duration_ms) >= now() - toIntervalSecond(?)
	`, int64(window.Seconds())).Scan(&tunnels, &bytesUp, &bytesDown, &requests, &durations)
	if err != nil {
		return nil, fmt.Errorf("failed to get tunnel stats: %w", err)
	}

	return &TunnelSummary{
		Tunnels:         int64(tunnels),
		BytesUp:         bytesUp,
		BytesDown:       bytesDown,
		Requests:        requests,
		TotalDurationMs: durations,
	}, nil
}

// TrafficSeries returns request volume and latency percentiles bucketed over
// the trailing range, zero-filled to a dense series.
func (s *ClickHouseStore) TrafficSeries(ctx context.Context, rng string) ([]models.TrafficPoint, error) {
	bucket, lookback := SeriesWindow(rng)

	// quantilesExactInclusive matches Postgres' percentile_cont
	// interpolation, keeping the two backends chart-identical. Percentiles
	// cover successful requests only.
	query := `
		SELECT
			toStartOfInterval(timestamp, toIntervalSecond(?)) AS bucket,
			count(),
			countIf(success),
			quantilesExactInclusiveIf(0.5, 0.95)(response_time, success)
		FROM proxy_requests
		WHERE timestamp >= now() - toIntervalSecond(?)
		GROUP BY bucket
		ORDER BY bucket
	`

	rows, err := s.conn.Query(ctx, query, int64(bucket.Seconds()), int64(lookback.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("failed to get traffic series: %w", err)
	}
	defer rows.Close()

	points := []models.TrafficPoint{}
	for rows.Next() {
		var ts time.Time
		var requests, successes uint64
		var pcts []float64
		if err := rows.Scan(&ts, &requests, &successes, &pcts); err != nil {
			return nil, fmt.Errorf("failed to scan traffic series: %w", err)
		}
		p := models.TrafficPoint{
			Time:      ts.UTC(),
			Requests:  int64(requests),
			Successes: int64(successes),
		}
		// The quantiles are NaN when the bucket has no successful requests.
		if successes > 0 && len(pcts) == 2 {
			p.P50Ms = int(pcts[0])
			p.P95Ms = int(pcts[1])
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read traffic series: %w", err)
	}

	return fillTrafficGaps(points, bucket, lookback, time.Now()), nil
}

// chChartWindow maps an API interval to ClickHouse bucket/lookback interval
// expressions. Values are from a fixed set, never user input.
func chChartWindow(interval string) (bucket, lookback string) {
	switch interval {
	case "1h":
		return "toIntervalHour(1)", "toIntervalHour(24)"
	case "1d":
		return "toIntervalDay(1)", "toIntervalDay(7)"
	default: // "4h"
		return "toIntervalHour(4)", "toIntervalHour(24)"
	}
}

// ResponseTimeChart returns average response time of successful requests
// bucketed over time.
func (s *ClickHouseStore) ResponseTimeChart(ctx context.Context, interval string) ([]models.ChartDataPoint, error) {
	bucket, lookback := chChartWindow(interval)

	query := fmt.Sprintf(`
		SELECT toStartOfInterval(timestamp, %s) AS bucket,
		       toInt32(avg(response_time))
		FROM proxy_requests
		WHERE timestamp >= now() - %s
		  AND success = true
		GROUP BY bucket
		ORDER BY bucket
	`, bucket, lookback)

	rows, err := s.conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get response time chart: %w", err)
	}
	defer rows.Close()

	data := []models.ChartDataPoint{}
	for rows.Next() {
		var ts time.Time
		var value int32
		if err := rows.Scan(&ts, &value); err != nil {
			return nil, fmt.Errorf("failed to scan chart data: %w", err)
		}
		data = append(data, models.ChartDataPoint{
			Time:  ts.Local().Format("15:04"),
			Value: int(value),
		})
	}
	return data, rows.Err()
}

// SuccessRateChart returns success/failure percentages bucketed over time.
func (s *ClickHouseStore) SuccessRateChart(ctx context.Context, interval string) ([]models.SuccessRateDataPoint, error) {
	bucket, lookback := chChartWindow(interval)

	query := fmt.Sprintf(`
		SELECT toStartOfInterval(timestamp, %s) AS bucket,
		       toInt32(countIf(success) * 100 / greatest(count(), 1)),
		       toInt32(countIf(NOT success) * 100 / greatest(count(), 1))
		FROM proxy_requests
		WHERE timestamp >= now() - %s
		GROUP BY bucket
		ORDER BY bucket
	`, bucket, lookback)

	rows, err := s.conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get success rate chart: %w", err)
	}
	defer rows.Close()

	data := []models.SuccessRateDataPoint{}
	for rows.Next() {
		var ts time.Time
		var success, failure int32
		if err := rows.Scan(&ts, &success, &failure); err != nil {
			return nil, fmt.Errorf("failed to scan chart data: %w", err)
		}
		data = append(data, models.SuccessRateDataPoint{
			Time:    ts.Local().Format("15:04"),
			Success: int(success),
			Failure: int(failure),
		})
	}
	return data, rows.Err()
}

// ProxyRollup returns per-proxy request aggregates over the whole event
// window.
func (s *ClickHouseStore) ProxyRollup(ctx context.Context) ([]ProxyRequestStats, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT proxy_id,
		       count(),
		       countIf(success),
		       if(countIf(success) = 0, 0, toInt32(sumIf(response_time, success) / countIf(success)))
		FROM proxy_requests
		WHERE proxy_id > 0
		GROUP BY proxy_id
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to roll up proxy requests: %w", err)
	}
	defer rows.Close()

	stats := []ProxyRequestStats{}
	for rows.Next() {
		var proxyID int32
		var requests, successes uint64
		var avgMs int32
		if err := rows.Scan(&proxyID, &requests, &successes, &avgMs); err != nil {
			return nil, fmt.Errorf("failed to scan proxy rollup: %w", err)
		}
		stats = append(stats, ProxyRequestStats{
			ProxyID:         int(proxyID),
			Requests:        int64(requests),
			Successes:       int64(successes),
			AvgResponseTime: int(avgMs),
		})
	}
	return stats, rows.Err()
}

// LowSuccessProxies returns the IDs of proxies below minRate percent success
// over the trailing window, with at least minRequests requests.
func (s *ClickHouseStore) LowSuccessProxies(ctx context.Context, window time.Duration, minRate float64, minRequests int) ([]int, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT proxy_id
		FROM proxy_requests
		WHERE proxy_id > 0
		  AND timestamp >= now() - toIntervalSecond(?)
		GROUP BY proxy_id
		HAVING count() >= ?
		   AND countIf(success) * 100 / count() < ?
	`, int64(window.Seconds()), int64(minRequests), minRate)
	if err != nil {
		return nil, fmt.Errorf("failed to query low-success proxies: %w", err)
	}
	defer rows.Close()

	ids := []int{}
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan low-success proxy id: %w", err)
		}
		ids = append(ids, int(id))
	}
	return ids, rows.Err()
}

// ApplyRetention keeps the table TTLs in sync with the configuration.
// ClickHouse expires rows in background merges, so this only (re)declares the
// TTL expressions; tables already at the configured periods are left alone.
func (s *ClickHouseStore) ApplyRetention(ctx context.Context, cfg RetentionConfig) error {
	apply := func(table string, days int) error {
		if days <= 0 {
			return nil
		}
		want := fmt.Sprintf("toIntervalDay(%d)", days)

		var createQuery string
		err := s.conn.QueryRow(ctx, `
			SELECT create_table_query FROM system.tables
			WHERE database = currentDatabase() AND name = ?
		`, table).Scan(&createQuery)
		if err != nil {
			return fmt.Errorf("failed to read %s TTL: %w", table, err)
		}
		if strings.Contains(createQuery, want) {
			return nil
		}

		alter := fmt.Sprintf("ALTER TABLE %s MODIFY TTL toDateTime(timestamp) + %s", table, want)
		if err := s.conn.Exec(ctx, alter); err != nil {
			return fmt.Errorf("failed to update %s TTL: %w", table, err)
		}
		s.logger.Info("updated event retention TTL", "table", table, "days", days)
		return nil
	}

	if err := apply("proxy_requests", cfg.RequestRetentionDays); err != nil {
		return err
	}
	return apply("proxy_tunnels", cfg.RequestRetentionDays)
}
