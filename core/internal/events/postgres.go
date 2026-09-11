package events

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// PostgresStore implements Store on the primary Postgres database. It works on
// a plain Postgres 14+ server; when the TimescaleDB extension (with a TSL
// license) is present, retention and compression are delegated to its policies.
type PostgresStore struct {
	db     *database.DB
	logger *logger.Logger

	// capabilities probe result, cached after the first successful probe.
	capMu    sync.Mutex
	capKnown bool
	caps     pgCapabilities
}

// pgCapabilities describes which optional accelerators the connected Postgres
// server offers.
type pgCapabilities struct {
	// timescale: the TimescaleDB extension is installed.
	timescale bool
	// tslPolicies: TSL-licensed features (retention/compression policies) are
	// available. False on plain Postgres and on Apache-only TimescaleDB
	// builds (e.g. Azure Flexible Server).
	tslPolicies bool
}

// NewPostgresStore creates a Postgres-backed event store on the given pool.
func NewPostgresStore(db *database.DB, log *logger.Logger) *PostgresStore {
	return &PostgresStore{db: db, logger: log}
}

var _ Store = (*PostgresStore)(nil)

// pgTime prepares a timestamp for binding against the naive TIMESTAMP
// columns: pgx keeps the time's wall-clock digits and drops the zone, while
// the server compares against NOW() in UTC — so a local-zone time.Time would
// land offset by the host's UTC offset. Converting first makes the stored
// wall clock BE the UTC reading. (Docker deployments never noticed: the app
// container runs UTC, so local == UTC.)
func pgTime(t time.Time) time.Time { return t.UTC() }

// capabilities probes the server for optional TimescaleDB features, caching
// the result after the first success. Probe failures are returned (not
// cached) so a transient error cannot pin the wrong mode.
func (s *PostgresStore) capabilities(ctx context.Context) (pgCapabilities, error) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if s.capKnown {
		return s.caps, nil
	}

	var caps pgCapabilities
	err := s.db.Pool.QueryRow(ctx, `
		SELECT
			EXISTS (SELECT FROM pg_extension WHERE extname = 'timescaledb'),
			COALESCE(current_setting('timescaledb.license', true) = 'timescale', false)
	`).Scan(&caps.timescale, &caps.tslPolicies)
	if err != nil {
		return pgCapabilities{}, fmt.Errorf("failed to probe database capabilities: %w", err)
	}

	s.caps = caps
	s.capKnown = true
	s.logger.Info("event store capabilities probed",
		"timescaledb", caps.timescale,
		"timescaledb_policies", caps.tslPolicies,
	)
	return caps, nil
}

// InsertRequest records one proxied request outcome.
func (s *PostgresStore) InsertRequest(ctx context.Context, event RequestEvent) error {
	query := `
		INSERT INTO proxy_requests (
			proxy_id, proxy_address, pool_id, username, method, url, domain,
			status_code, success, response_time, error, timestamp
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`

	var errorMsg *string
	if event.Error != "" {
		errorMsg = &event.Error
	}

	var statusCode *int
	if event.StatusCode > 0 {
		statusCode = &event.StatusCode
	}

	// Zero-value dimensions are stored as NULL: "not applicable", not "".
	var poolID *int
	if event.PoolID > 0 {
		poolID = &event.PoolID
	}
	var username *string
	if event.Username != "" {
		username = &event.Username
	}
	var domain *string
	if event.Domain != "" {
		domain = &event.Domain
	}

	_, err := s.db.Pool.Exec(
		ctx,
		query,
		event.ProxyID,
		event.ProxyAddress,
		poolID,
		username,
		event.Method,
		event.URL,
		domain,
		statusCode,
		event.Success,
		event.ResponseTime,
		errorMsg,
		pgTime(event.Timestamp),
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

// InsertTunnel records one completed CONNECT tunnel.
func (s *PostgresStore) InsertTunnel(ctx context.Context, event TunnelEvent) error {
	query := `
		INSERT INTO proxy_tunnels (
			proxy_id, proxy_address, pool_id, username, host, domain,
			bytes_up, bytes_down, requests, duration_ms, error, timestamp
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`

	// Zero-value dimensions are stored as NULL: "not applicable", not "".
	var poolID *int
	if event.PoolID > 0 {
		poolID = &event.PoolID
	}
	var username *string
	if event.Username != "" {
		username = &event.Username
	}
	var domain *string
	if event.Domain != "" {
		domain = &event.Domain
	}
	var errorMsg *string
	if event.Error != "" {
		errorMsg = &event.Error
	}

	_, err := s.db.Pool.Exec(
		ctx,
		query,
		event.ProxyID,
		event.ProxyAddress,
		poolID,
		username,
		event.Host,
		domain,
		event.BytesUp,
		event.BytesDown,
		event.Requests,
		event.DurationMs,
		errorMsg,
		pgTime(event.OpenedAt),
	)
	if err != nil {
		return fmt.Errorf("failed to insert proxy tunnel: %w", err)
	}
	return nil
}

// TunnelStats aggregates tunnels that closed within the trailing window.
func (s *PostgresStore) TunnelStats(ctx context.Context, window time.Duration) (*TunnelSummary, error) {
	// Tunnels are bucketed by close time, not open time: a tunnel opened
	// before the window but closed inside it did its work in the window.
	query := `
		SELECT
			COUNT(*),
			COALESCE(SUM(bytes_up), 0),
			COALESCE(SUM(bytes_down), 0),
			COALESCE(SUM(requests), 0),
			COALESCE(SUM(duration_ms), 0)
		FROM proxy_tunnels
		WHERE timestamp + make_interval(secs => duration_ms / 1000.0) >= NOW() - $1::interval
	`

	var stats TunnelSummary
	err := s.db.Pool.QueryRow(ctx, query, window).Scan(
		&stats.Tunnels,
		&stats.BytesUp,
		&stats.BytesDown,
		&stats.Requests,
		&stats.TotalDurationMs,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get tunnel stats: %w", err)
	}
	return &stats, nil
}

// ProxyRollup returns per-proxy request aggregates over the whole event
// window.
func (s *PostgresStore) ProxyRollup(ctx context.Context) ([]ProxyRequestStats, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT proxy_id,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE success),
		       COALESCE((AVG(response_time) FILTER (WHERE success))::int, 0)
		FROM proxy_requests
		WHERE proxy_id IS NOT NULL
		GROUP BY proxy_id
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to roll up proxy requests: %w", err)
	}
	defer rows.Close()

	stats := []ProxyRequestStats{}
	for rows.Next() {
		var st ProxyRequestStats
		if err := rows.Scan(&st.ProxyID, &st.Requests, &st.Successes, &st.AvgResponseTime); err != nil {
			return nil, fmt.Errorf("failed to scan proxy rollup: %w", err)
		}
		stats = append(stats, st)
	}
	return stats, rows.Err()
}

// LowSuccessProxies returns the IDs of proxies below minRate percent success
// over the trailing window, with at least minRequests requests.
func (s *PostgresStore) LowSuccessProxies(ctx context.Context, window time.Duration, minRate float64, minRequests int) ([]int, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT proxy_id
		FROM proxy_requests
		WHERE proxy_id IS NOT NULL
		  AND timestamp >= NOW() - $1::interval
		GROUP BY proxy_id
		HAVING COUNT(*) >= $2
		   AND (COUNT(*) FILTER (WHERE success))::float / COUNT(*)::float * 100 < $3
	`, window, minRequests, minRate)
	if err != nil {
		return nil, fmt.Errorf("failed to query low-success proxies: %w", err)
	}
	defer rows.Close()

	ids := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan low-success proxy id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// TrafficSeries returns request volume and latency percentiles bucketed over
// the trailing range, zero-filled to a dense series.
func (s *PostgresStore) TrafficSeries(ctx context.Context, rng string) ([]models.TrafficPoint, error) {
	bucket, lookback := SeriesWindow(rng)

	// Percentiles cover successful requests only — failure latencies say
	// more about timeouts than about the proxy. The date_bin origin is
	// midnight-aligned so buckets match fillTrafficGaps' epoch-aligned grid.
	query := `
		SELECT
			date_bin(make_interval(secs => $1), timestamp, TIMESTAMP '2000-01-03') AS bucket,
			COUNT(*),
			COUNT(*) FILTER (WHERE success),
			COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY response_time) FILTER (WHERE success), 0),
			COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY response_time) FILTER (WHERE success), 0)
		FROM proxy_requests
		WHERE timestamp >= NOW() - make_interval(secs => $2)
		GROUP BY bucket
		ORDER BY bucket
	`

	rows, err := s.db.Pool.Query(ctx, query, bucket.Seconds(), lookback.Seconds())
	if err != nil {
		return nil, fmt.Errorf("failed to get traffic series: %w", err)
	}
	defer rows.Close()

	points := []models.TrafficPoint{}
	for rows.Next() {
		var p models.TrafficPoint
		var p50, p95 float64
		if err := rows.Scan(&p.Time, &p.Requests, &p.Successes, &p50, &p95); err != nil {
			return nil, fmt.Errorf("failed to scan traffic series: %w", err)
		}
		p.Time = p.Time.UTC()
		p.P50Ms = int(p50)
		p.P95Ms = int(p95)
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read traffic series: %w", err)
	}

	return fillTrafficGaps(points, bucket, lookback, time.Now()), nil
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
//
// date_bin is vanilla Postgres 14+ and behaves like Timescale's time_bucket
// for these strides; the origin is time_bucket's default so buckets align
// either way.
func (s *PostgresStore) ResponseTimeChart(ctx context.Context, interval string) ([]models.ChartDataPoint, error) {
	bucketSize, lookback := chartWindow(interval)

	query := `
		SELECT
			date_bin($1::interval, timestamp, TIMESTAMP '2000-01-03') as bucket,
			COALESCE(AVG(response_time), 0)::int as avg_response_time
		FROM proxy_requests
		WHERE timestamp >= NOW() - $2::interval
		  AND success = true
		GROUP BY bucket
		ORDER BY bucket
	`

	rows, err := s.db.Pool.Query(ctx, query, bucketSize, lookback)
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

	query := `
		SELECT
			date_bin($1::interval, timestamp, TIMESTAMP '2000-01-03') as bucket,
			(COUNT(*) FILTER (WHERE success = true) * 100 / GREATEST(COUNT(*), 1))::int as success_rate,
			(COUNT(*) FILTER (WHERE success = false) * 100 / GREATEST(COUNT(*), 1))::int as failure_rate
		FROM proxy_requests
		WHERE timestamp >= NOW() - $2::interval
		GROUP BY bucket
		ORDER BY bucket
	`

	rows, err := s.db.Pool.Query(ctx, query, bucketSize, lookback)
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

// ApplyRetention makes the retention configuration effective. Where
// TimescaleDB's TSL-licensed policies are available (self-hosted community
// builds) it (re)installs them and lets their background jobs do the work;
// everywhere else — plain Postgres, Apache-only TimescaleDB builds (e.g.
// Azure Flexible Server) — it enforces retention directly by deleting expired
// rows.
func (s *PostgresStore) ApplyRetention(ctx context.Context, cfg RetentionConfig) error {
	caps, err := s.capabilities(ctx)
	if err != nil {
		return err
	}

	if caps.tslPolicies {
		return s.applyRetentionPolicies(ctx, cfg)
	}
	return s.applyRetentionDeletes(ctx, cfg)
}

// applyRetentionPolicies (re)installs TimescaleDB retention
// policies. Caller has verified they are available.
func (s *PostgresStore) applyRetentionPolicies(ctx context.Context, cfg RetentionConfig) error {
	// Values are integers formatted into DDL because policy intervals cannot
	// be bind parameters; remove+add so period changes take effect.
	statements := []string{}
	if cfg.RequestRetentionDays > 0 {
		statements = append(statements, fmt.Sprintf(`
			SELECT remove_retention_policy('proxy_requests', if_exists => true);
			SELECT add_retention_policy('proxy_requests', INTERVAL '%d days', if_not_exists => true);
		`, cfg.RequestRetentionDays))
		statements = append(statements, fmt.Sprintf(`
			SELECT remove_retention_policy('proxy_tunnels', if_exists => true);
			SELECT add_retention_policy('proxy_tunnels', INTERVAL '%d days', if_not_exists => true);
		`, cfg.RequestRetentionDays))
	}

	for _, sql := range statements {
		if _, err := s.db.Pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("failed to update retention policies: %w", err)
		}
	}
	return nil
}

// applyRetentionDeletes enforces retention by deleting expired rows — the
// portable fallback for servers without policy support. Non-positive periods
// are skipped so a zero-value config can never delete everything.
func (s *PostgresStore) applyRetentionDeletes(ctx context.Context, cfg RetentionConfig) error {
	var requestsDeleted, tunnelsDeleted int64

	if cfg.RequestRetentionDays > 0 {
		res, err := s.db.Pool.Exec(ctx,
			`DELETE FROM proxy_requests WHERE timestamp < NOW() - make_interval(days => $1)`,
			cfg.RequestRetentionDays)
		if err != nil {
			return fmt.Errorf("failed to delete expired proxy requests: %w", err)
		}
		requestsDeleted = res.RowsAffected()

		res, err = s.db.Pool.Exec(ctx,
			`DELETE FROM proxy_tunnels WHERE timestamp < NOW() - make_interval(days => $1)`,
			cfg.RequestRetentionDays)
		if err != nil {
			return fmt.Errorf("failed to delete expired proxy tunnels: %w", err)
		}
		tunnelsDeleted = res.RowsAffected()
	}

	if requestsDeleted > 0 || tunnelsDeleted > 0 {
		s.logger.Info("applied event retention by deletion",
			"requests_deleted", requestsDeleted,
			"tunnels_deleted", tunnelsDeleted,
		)
	}
	return nil
}
