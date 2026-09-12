package events

import (
	"context"
	"fmt"

	"github.com/alpkeskin/rota/core/internal/models"
)

func (s *PostgresStore) DomainStats(ctx context.Context, rng, domain string, limit int) ([]models.DomainStats, error) {
	_, window := SeriesWindow(rng)
	rows, err := s.db.Pool.Query(ctx, `
		SELECT domain, SUM(requests), SUM(successes), SUM(rate_limited),
		       SUM(response_ms), MAX(p50), MAX(p95), SUM(tunnels),
		       SUM(tunnel_errors), SUM(bytes_up), SUM(bytes_down)
		FROM (
			SELECT domain, COUNT(*) AS requests,
			       COUNT(*) FILTER (WHERE success) AS successes,
			       COUNT(*) FILTER (WHERE status_code = 429) AS rate_limited,
			       COALESCE(SUM(response_time) FILTER (WHERE success), 0) AS response_ms,
			       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY response_time) FILTER (WHERE success), 0) AS p50,
			       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY response_time) FILTER (WHERE success), 0) AS p95,
			       0::bigint AS tunnels, 0::bigint AS tunnel_errors,
			       0::bigint AS bytes_up, 0::bigint AS bytes_down
			FROM proxy_requests
			WHERE timestamp >= NOW() - make_interval(secs => $1)
			  AND domain IS NOT NULL AND domain <> ''
			  AND ($2 = '' OR domain = $2 OR RIGHT(domain, LENGTH($2) + 1) = '.' || $2)
			GROUP BY domain
			UNION ALL
			SELECT domain, 0, 0, 0, 0, 0, 0, COUNT(*),
			       COUNT(*) FILTER (WHERE error IS NOT NULL AND error <> ''),
			       SUM(bytes_up), SUM(bytes_down)
			FROM proxy_tunnels
			WHERE timestamp + make_interval(secs => duration_ms / 1000.0) >= NOW() - make_interval(secs => $1)
			  AND domain IS NOT NULL AND domain <> ''
			  AND ($2 = '' OR domain = $2 OR RIGHT(domain, LENGTH($2) + 1) = '.' || $2)
			GROUP BY domain
		) traffic
		GROUP BY domain
		ORDER BY SUM(requests) + SUM(tunnels) DESC, domain
		LIMIT $3
	`, window.Seconds(), domain, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get domain stats: %w", err)
	}
	defer rows.Close()
	return readDomainStats(rows)
}

func (s *ClickHouseStore) DomainStats(ctx context.Context, rng, domain string, limit int) ([]models.DomainStats, error) {
	_, window := SeriesWindow(rng)
	rows, err := s.conn.Query(ctx, `
		SELECT domain, toInt64(sum(requests)), toInt64(sum(successes)),
		       toInt64(sum(rate_limited)), toInt64(sum(response_ms)),
		       max(p50), max(p95), toInt64(sum(tunnels)),
		       toInt64(sum(tunnel_errors)), toInt64(sum(bytes_up)), toInt64(sum(bytes_down))
		FROM (
			SELECT domain, count() AS requests, countIf(success) AS successes,
			       countIf(status_code = 429) AS rate_limited,
			       sumIf(response_time, success) AS response_ms,
			       if(countIf(success) > 0, quantilesExactInclusiveIf(0.5, 0.95)(response_time, success)[1], 0) AS p50,
			       if(countIf(success) > 0, quantilesExactInclusiveIf(0.5, 0.95)(response_time, success)[2], 0) AS p95,
			       toUInt64(0) AS tunnels, toUInt64(0) AS tunnel_errors,
			       toInt64(0) AS bytes_up, toInt64(0) AS bytes_down
			FROM proxy_requests
			WHERE timestamp >= now() - toIntervalSecond(?)
			  AND domain != '' AND (? = '' OR domain = ? OR endsWith(domain, concat('.', ?)))
			GROUP BY domain
			UNION ALL
			SELECT domain, 0, 0, 0, toInt64(0), toFloat64(0), toFloat64(0),
			       count(), countIf(error != ''), sum(bytes_up), sum(bytes_down)
			FROM proxy_tunnels
			WHERE timestamp + toIntervalMillisecond(duration_ms) >= now() - toIntervalSecond(?)
			  AND domain != '' AND (? = '' OR domain = ? OR endsWith(domain, concat('.', ?)))
			GROUP BY domain
		) traffic
		GROUP BY domain
		ORDER BY sum(requests) + sum(tunnels) DESC, domain
		LIMIT ?
	`, int64(window.Seconds()), domain, domain, domain,
		int64(window.Seconds()), domain, domain, domain, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get domain stats: %w", err)
	}
	defer rows.Close()
	return readDomainStats(rows)
}

func readDomainStats(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]models.DomainStats, error) {
	stats := []models.DomainStats{}
	for rows.Next() {
		var st models.DomainStats
		var responseMs int64
		var p50, p95 float64
		if err := rows.Scan(&st.Domain, &st.Requests, &st.Successes, &st.RateLimited,
			&responseMs, &p50, &p95, &st.Tunnels, &st.TunnelErrors, &st.BytesUp, &st.BytesDown); err != nil {
			return nil, fmt.Errorf("failed to scan domain stats: %w", err)
		}
		st.Failures = st.Requests - st.Successes
		if st.Successes > 0 {
			st.AvgResponseTime = int(responseMs / st.Successes)
			st.P50Ms, st.P95Ms = int(p50), int(p95)
		}
		stats = append(stats, st)
	}
	return stats, rows.Err()
}
