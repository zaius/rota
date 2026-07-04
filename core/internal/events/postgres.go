package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
)

// PostgresStore implements Store on the primary Postgres database. It works on
// a plain Postgres server; when the TimescaleDB extension (with a TSL license)
// is present, retention and compression are delegated to its policies.
type PostgresStore struct {
	db *database.DB
}

// NewPostgresStore creates a Postgres-backed event store on the given pool.
func NewPostgresStore(db *database.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

var _ Store = (*PostgresStore)(nil)

// InsertLog records a system log event.
func (s *PostgresStore) InsertLog(ctx context.Context, entry LogEntry) error {
	query := `
		INSERT INTO logs (timestamp, level, message, details, metadata)
		VALUES ($1, $2, $3, $4, $5)
	`

	// Source is stored inside the metadata document, which is what the list
	// filters query. Copy before annotating so the caller's map is not mutated.
	metadata := entry.Metadata
	if entry.Source != "" {
		metadata = make(map[string]any, len(entry.Metadata)+1)
		for k, v := range entry.Metadata {
			metadata[k] = v
		}
		metadata["source"] = entry.Source
	}

	var metadataJSON []byte
	if metadata != nil {
		var err error
		metadataJSON, err = json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("failed to marshal metadata: %w", err)
		}
	}

	if _, err := s.db.Pool.Exec(ctx, query, time.Now(), entry.Level, entry.Message, entry.Details, metadataJSON); err != nil {
		return fmt.Errorf("failed to create log: %w", err)
	}

	return nil
}

// ListLogs returns one page of logs matching the filter, newest first, with
// the total match count.
func (s *PostgresStore) ListLogs(ctx context.Context, filter LogFilter, page, limit int) ([]models.Log, int, error) {
	whereClauses := []string{}
	args := []any{}
	argPos := 1

	if filter.Level != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("level = $%d", argPos))
		args = append(args, filter.Level)
		argPos++
	}

	if filter.Search != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("message ILIKE $%d", argPos))
		args = append(args, "%"+filter.Search+"%")
		argPos++
	}

	if filter.Source != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("metadata->>'source' = $%d", argPos))
		args = append(args, filter.Source)
		argPos++
	}

	if filter.StartTime != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("timestamp >= $%d", argPos))
		args = append(args, *filter.StartTime)
		argPos++
	}

	if filter.EndTime != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("timestamp <= $%d", argPos))
		args = append(args, *filter.EndTime)
		argPos++
	}

	whereClause := ""
	if len(whereClauses) > 0 {
		whereClause = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM logs %s", whereClause)
	var total int
	if err := s.db.Pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("failed to count logs: %w", err)
	}

	offset := (page - 1) * limit
	query := fmt.Sprintf(`
		SELECT id, timestamp, level, message, details, metadata
		FROM logs
		%s
		ORDER BY timestamp DESC
		LIMIT $%d OFFSET $%d
	`, whereClause, argPos, argPos+1)

	args = append(args, limit, offset)

	rows, err := s.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list logs: %w", err)
	}
	defer rows.Close()

	logs, err := scanLogs(rows)
	if err != nil {
		return nil, 0, err
	}
	return logs, total, nil
}

// LogsSince returns up to limit logs with ID greater than lastID in ascending
// ID order, optionally filtered by source.
func (s *PostgresStore) LogsSince(ctx context.Context, lastID int64, limit int, source string) ([]models.Log, error) {
	whereClauses := []string{"id > $1"}
	args := []any{lastID}
	argPos := 2

	if source != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("metadata->>'source' = $%d", argPos))
		args = append(args, source)
		argPos++
	}

	query := fmt.Sprintf(`
		SELECT id, timestamp, level, message, details, metadata
		FROM logs
		WHERE %s
		ORDER BY id ASC
		LIMIT $%d
	`, strings.Join(whereClauses, " AND "), argPos)

	args = append(args, limit)

	rows, err := s.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list logs: %w", err)
	}
	defer rows.Close()

	return scanLogs(rows)
}

// DeleteLogsOlderThan removes logs older than the given age.
func (s *PostgresStore) DeleteLogsOlderThan(ctx context.Context, age time.Duration) (int64, error) {
	query := `DELETE FROM logs WHERE timestamp < $1`
	cutoff := time.Now().Add(-age)

	result, err := s.db.Pool.Exec(ctx, query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to delete old logs: %w", err)
	}

	return result.RowsAffected(), nil
}

// InsertRequest records one proxied request outcome.
func (s *PostgresStore) InsertRequest(ctx context.Context, event RequestEvent) error {
	query := `
		INSERT INTO proxy_requests (
			proxy_id, proxy_address, method, url, status_code, success, response_time, error, timestamp
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`

	var errorMsg *string
	if event.Error != "" {
		errorMsg = &event.Error
	}

	var statusCode *int
	if event.StatusCode > 0 {
		statusCode = &event.StatusCode
	}

	_, err := s.db.Pool.Exec(
		ctx,
		query,
		event.ProxyID,
		event.ProxyAddress,
		event.Method,
		event.URL,
		statusCode,
		event.Success,
		event.ResponseTime,
		errorMsg,
		event.Timestamp,
	)

	return err
}

// RequestStats returns today/yesterday request aggregates for the dashboard.
func (s *PostgresStore) RequestStats(ctx context.Context) (*RequestStats, error) {
	query := `
		WITH yesterday_stats AS (
			SELECT
				COUNT(*) as requests,
				COALESCE(AVG(CASE WHEN success THEN 1.0 ELSE 0.0 END) * 100, 0) as success_rate,
				COALESCE(AVG(response_time), 0)::int as response_time
			FROM proxy_requests
			WHERE timestamp >= NOW() - INTERVAL '2 days'
			  AND timestamp < NOW() - INTERVAL '1 day'
		),
		today_stats AS (
			SELECT
				COUNT(*) as requests,
				COALESCE(AVG(CASE WHEN success THEN 1.0 ELSE 0.0 END) * 100, 0) as success_rate,
				COALESCE(AVG(response_time), 0)::int as response_time
			FROM proxy_requests
			WHERE timestamp >= NOW() - INTERVAL '1 day'
		)
		SELECT
			t.requests, t.success_rate, t.response_time,
			y.requests, y.success_rate, y.response_time
		FROM today_stats t, yesterday_stats y
	`

	var stats RequestStats
	err := s.db.Pool.QueryRow(ctx, query).Scan(
		&stats.RequestsToday,
		&stats.SuccessRateToday,
		&stats.ResponseTimeToday,
		&stats.RequestsYesterday,
		&stats.SuccessRateYesterday,
		&stats.ResponseTimeYesterday,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get request stats: %w", err)
	}

	return &stats, nil
}

// chartWindow maps an API interval to a bucket size and lookback period.
func chartWindow(interval string) (bucketSize, lookback string) {
	switch interval {
	case "1h":
		return "1 hour", "24 hours"
	case "1d":
		return "1 day", "7 days"
	default: // "4h"
		return "4 hours", "24 hours"
	}
}

// ResponseTimeChart returns average response time of successful requests
// bucketed over time.
func (s *PostgresStore) ResponseTimeChart(ctx context.Context, interval string) ([]models.ChartDataPoint, error) {
	bucketSize, lookback := chartWindow(interval)

	query := fmt.Sprintf(`
		SELECT
			time_bucket('%s', timestamp) as bucket,
			COALESCE(AVG(response_time), 0)::int as avg_response_time
		FROM proxy_requests
		WHERE timestamp >= NOW() - INTERVAL '%s'
		  AND success = true
		GROUP BY bucket
		ORDER BY bucket
	`, bucketSize, lookback)

	rows, err := s.db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get response time chart: %w", err)
	}
	defer rows.Close()

	data := []models.ChartDataPoint{}
	for rows.Next() {
		var bucket time.Time
		var value int

		if err := rows.Scan(&bucket, &value); err != nil {
			return nil, fmt.Errorf("failed to scan chart data: %w", err)
		}

		data = append(data, models.ChartDataPoint{
			Time:  bucket.Format("15:04"),
			Value: value,
		})
	}

	return data, rows.Err()
}

// SuccessRateChart returns success/failure percentages bucketed over time.
func (s *PostgresStore) SuccessRateChart(ctx context.Context, interval string) ([]models.SuccessRateDataPoint, error) {
	bucketSize, lookback := chartWindow(interval)

	query := fmt.Sprintf(`
		SELECT
			time_bucket('%s', timestamp) as bucket,
			(COUNT(*) FILTER (WHERE success = true) * 100 / GREATEST(COUNT(*), 1))::int as success_rate,
			(COUNT(*) FILTER (WHERE success = false) * 100 / GREATEST(COUNT(*), 1))::int as failure_rate
		FROM proxy_requests
		WHERE timestamp >= NOW() - INTERVAL '%s'
		GROUP BY bucket
		ORDER BY bucket
	`, bucketSize, lookback)

	rows, err := s.db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get success rate chart: %w", err)
	}
	defer rows.Close()

	data := []models.SuccessRateDataPoint{}
	for rows.Next() {
		var bucket time.Time
		var success, failure int

		if err := rows.Scan(&bucket, &success, &failure); err != nil {
			return nil, fmt.Errorf("failed to scan chart data: %w", err)
		}

		data = append(data, models.SuccessRateDataPoint{
			Time:    bucket.Format("15:04"),
			Success: success,
			Failure: failure,
		})
	}

	return data, rows.Err()
}

// ApplyRetention (re)applies retention and compression policies.
//
// Policies are a TSL-licensed TimescaleDB feature, unavailable on plain
// Postgres and on Apache-only builds (e.g. Azure Flexible Server). The SQL is
// guarded on the license so it is a no-op there instead of erroring on every
// cleanup cycle — matching the guard the migrations use when creating the
// policies.
func (s *PostgresStore) ApplyRetention(ctx context.Context, cfg RetentionConfig) error {
	retention := fmt.Sprintf(`
		DO $ts$
		BEGIN
			IF current_setting('timescaledb.license', true) = 'timescale' THEN
				PERFORM remove_retention_policy('logs', if_exists => true);
				PERFORM add_retention_policy('logs', INTERVAL '%d days', if_not_exists => true);
			END IF;
		END
		$ts$;
	`, cfg.RetentionDays)

	if _, err := s.db.Pool.Exec(ctx, retention); err != nil {
		return fmt.Errorf("failed to update retention policy: %w", err)
	}

	compression := fmt.Sprintf(`
		DO $ts$
		BEGIN
			IF current_setting('timescaledb.license', true) = 'timescale' THEN
				PERFORM remove_compression_policy('logs', if_exists => true);
				PERFORM add_compression_policy('logs', INTERVAL '%d days', if_not_exists => true);
			END IF;
		END
		$ts$;
	`, cfg.CompressionAfterDays)

	if _, err := s.db.Pool.Exec(ctx, compression); err != nil {
		return fmt.Errorf("failed to update compression policy: %w", err)
	}

	return nil
}

// logRows is the subset of pgx.Rows scanLogs needs.
type logRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// scanLogs reads (id, timestamp, level, message, details, metadata) rows.
func scanLogs(rows logRows) ([]models.Log, error) {
	logs := []models.Log{}
	for rows.Next() {
		var l models.Log
		var metadataJSON []byte

		if err := rows.Scan(&l.ID, &l.Timestamp, &l.Level, &l.Message, &l.Details, &metadataJSON); err != nil {
			return nil, fmt.Errorf("failed to scan log: %w", err)
		}

		if metadataJSON != nil {
			if err := json.Unmarshal(metadataJSON, &l.Metadata); err != nil {
				return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
			}
		}

		logs = append(logs, l)
	}

	return logs, rows.Err()
}
