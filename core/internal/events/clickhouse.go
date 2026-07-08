package events

import (
	"context"
	"encoding/json"
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
//     committed, up to ~200ms); flip it to 0 for maximum throughput if log
//     tailing may lag a beat.
type ClickHouseStore struct {
	conn   driver.Conn
	logger *logger.Logger
}

var _ Store = (*ClickHouseStore)(nil)

// chSchema is the idempotent bootstrap DDL. TTLs here are the initial
// defaults; ApplyRetention keeps them in sync with settings afterwards.
var chSchema = []string{
	`CREATE TABLE IF NOT EXISTS logs (
		id        Int64,
		timestamp DateTime64(3),
		level     LowCardinality(String),
		message   String,
		details   Nullable(String),
		source    LowCardinality(String),
		metadata  String
	) ENGINE = MergeTree
	PARTITION BY toYYYYMMDD(timestamp)
	ORDER BY (timestamp, id)
	TTL toDateTime(timestamp) + toIntervalDay(30)`,

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

// InsertLog records a system log event.
func (s *ClickHouseStore) InsertLog(ctx context.Context, entry LogEntry) error {
	ts := entry.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	// Source is stored both as its own column (for filtering) and folded
	// into the metadata document, which is what callers render — matching
	// what the Postgres backend returns.
	metadata := entry.Metadata
	if entry.Source != "" {
		metadata = make(map[string]any, len(entry.Metadata)+1)
		for k, v := range entry.Metadata {
			metadata[k] = v
		}
		metadata["source"] = entry.Source
	}
	metadataJSON := ""
	if metadata != nil {
		b, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("failed to marshal metadata: %w", err)
		}
		metadataJSON = string(b)
	}

	details := ""
	hasDetails := entry.Details != nil
	if hasDetails {
		details = *entry.Details
	}
	var detailsArg *string
	if hasDetails {
		detailsArg = &details
	}

	err := s.conn.Exec(ctx, `
		INSERT INTO logs (id, timestamp, level, message, details, source, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, nextLogID(), ts, entry.Level, entry.Message, detailsArg, entry.Source, metadataJSON)
	if err != nil {
		return fmt.Errorf("failed to create log: %w", err)
	}
	return nil
}

// logFilterWhere builds the WHERE clause for a log filter.
func logFilterWhere(filter LogFilter) (string, []any) {
	clauses := []string{}
	args := []any{}

	if filter.Level != "" {
		clauses = append(clauses, "level = ?")
		args = append(args, filter.Level)
	}
	if filter.Search != "" {
		clauses = append(clauses, "message ILIKE ?")
		args = append(args, "%"+filter.Search+"%")
	}
	if filter.Source != "" {
		clauses = append(clauses, "source = ?")
		args = append(args, filter.Source)
	}
	if filter.StartTime != nil {
		clauses = append(clauses, "timestamp >= ?")
		args = append(args, *filter.StartTime)
	}
	if filter.EndTime != nil {
		clauses = append(clauses, "timestamp <= ?")
		args = append(args, *filter.EndTime)
	}

	if len(clauses) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

// ListLogs returns one page of logs matching the filter, newest first, with
// the total match count.
func (s *ClickHouseStore) ListLogs(ctx context.Context, filter LogFilter, page, limit int) ([]models.Log, int, error) {
	where, args := logFilterWhere(filter)

	var total uint64
	if err := s.conn.QueryRow(ctx, "SELECT count() FROM logs "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("failed to count logs: %w", err)
	}

	offset := (page - 1) * limit
	query := fmt.Sprintf(`
		SELECT id, timestamp, level, message, details, metadata
		FROM logs
		%s
		ORDER BY timestamp DESC, id DESC
		LIMIT %d OFFSET %d
	`, where, limit, offset)

	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list logs: %w", err)
	}
	defer rows.Close()

	logs, err := scanCHLogs(rows)
	if err != nil {
		return nil, 0, err
	}
	return logs, int(total), nil
}

// LogsSince returns up to limit logs with ID greater than lastID in ascending
// ID order, optionally filtered by source.
func (s *ClickHouseStore) LogsSince(ctx context.Context, lastID int64, limit int, source string) ([]models.Log, error) {
	where := "WHERE id > ?"
	args := []any{lastID}
	if source != "" {
		where += " AND source = ?"
		args = append(args, source)
	}

	query := fmt.Sprintf(`
		SELECT id, timestamp, level, message, details, metadata
		FROM logs
		%s
		ORDER BY id ASC
		LIMIT %d
	`, where, limit)

	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list logs: %w", err)
	}
	defer rows.Close()

	return scanCHLogs(rows)
}

// DeleteLogsOlderThan removes logs older than the given age via a lightweight
// delete. The count is taken just before deletion, so it is approximate under
// concurrent inserts of backdated rows — acceptable for a maintenance
// operation.
func (s *ClickHouseStore) DeleteLogsOlderThan(ctx context.Context, age time.Duration) (int64, error) {
	cutoff := time.Now().Add(-age)

	var n uint64
	if err := s.conn.QueryRow(ctx, `SELECT count() FROM logs WHERE timestamp < ?`, cutoff).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count old logs: %w", err)
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.conn.Exec(ctx, `DELETE FROM logs WHERE timestamp < ?`, cutoff); err != nil {
		return 0, fmt.Errorf("failed to delete old logs: %w", err)
	}
	return int64(n), nil
}

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
// Compression is native to MergeTree — CompressionAfterDays is ignored as
// documented.
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

	if err := apply("logs", cfg.RetentionDays); err != nil {
		return err
	}
	return apply("proxy_requests", cfg.RequestRetentionDays)
}

// scanCHLogs reads (id, timestamp, level, message, details, metadata) rows.
func scanCHLogs(rows driver.Rows) ([]models.Log, error) {
	logs := []models.Log{}
	for rows.Next() {
		var l models.Log
		var metadataJSON string

		if err := rows.Scan(&l.ID, &l.Timestamp, &l.Level, &l.Message, &l.Details, &metadataJSON); err != nil {
			return nil, fmt.Errorf("failed to scan log: %w", err)
		}
		if metadataJSON != "" {
			if err := json.Unmarshal([]byte(metadataJSON), &l.Metadata); err != nil {
				return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
			}
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}
