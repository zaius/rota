// Package events defines the event store: the single boundary through which
// the application reads and writes request and tunnel history.
//
// ParquetStore keeps history in local files, apart from the control-plane
// tables (proxies, pools, settings, ...) in Postgres, so callers merge the two
// in Go. Methods are business questions ("success-rate chart for the last
// day"), never raw query passthrough: a caller that needs a new shape of
// answer adds a method.
package events

import (
	"context"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

// RequestEvent is one proxied request outcome.
//
// PoolID, Username and Domain are dimensions, not relations: they capture who
// and what the request was for at the time it happened, survive deletion of
// the referenced pool/user, and zero values mean "not applicable" (e.g. the
// default pool has ID 0 and no user).
type RequestEvent struct {
	ProxyID      int
	ProxyAddress string
	PoolID       int    // pool that served the request; 0 = default pool
	Username     string // proxy user the request was authenticated as
	Method       string
	URL          string
	Domain       string // normalized target host (see NormalizeCooldownDomain)
	StatusCode   int    // 0 = no response
	ResponseTime int    // milliseconds
	Success      bool
	Error        string
	Timestamp    time.Time

	TargetFailure bool // target rejection or client abort; excluded from proxy reliability
}

// RequestStats aggregates request outcomes over the trailing day, with the
// prior day for growth comparisons. Rates are percentages (0-100).
type RequestStats struct {
	RequestsToday         int64
	SuccessRateToday      float64
	ResponseTimeToday     int
	RequestsYesterday     int64
	SuccessRateYesterday  float64
	ResponseTimeYesterday int
}

// TunnelEvent is one completed CONNECT tunnel, recorded when the tunnel
// closes.
//
// A tunnel is not a request. One tunnel carries every HTTPS request the client
// sends over that connection — with keep-alive, potentially thousands — and the
// bytes are opaque to the proxy unless TLS interception is enabled. Tunnels are
// therefore a separate event stream from proxied requests: counting them as
// requests would undercount real traffic by whatever the client's connection
// reuse factor happens to be.
//
// BytesUp/BytesDown are wire bytes in each direction, so they measure real
// volume even when the payload cannot be read. Requests is 0 unless TLS
// interception counted the messages inside the tunnel.
type TunnelEvent struct {
	ProxyID      int
	ProxyAddress string
	PoolID       int    // pool that served the tunnel; 0 = default pool
	Username     string // proxy user the tunnel was authenticated as
	Host         string // CONNECT authority, host:port
	Domain       string // normalized target host (see NormalizeCooldownDomain)
	BytesUp      int64  // client → target
	BytesDown    int64  // target → client
	Requests     int    // HTTP requests seen inside; 0 when not inspected
	DurationMs   int
	Error        string
	OpenedAt     time.Time
}

// TunnelSummary aggregates completed tunnels over a trailing window.
//
// TotalDurationMs is the summed lifetime of every tunnel in the window, which
// divided by the window length gives mean concurrency — the number that turns
// "3 tunnels" into "3 tunnels held open the whole hour".
type TunnelSummary struct {
	Tunnels         int64
	BytesUp         int64
	BytesDown       int64
	Requests        int64
	TotalDurationMs int64
}

// MeanConcurrency returns the average number of tunnels open simultaneously
// across a window of the given length. Zero for a non-positive window.
func (s *TunnelSummary) MeanConcurrency(window time.Duration) float64 {
	if s == nil || window <= 0 {
		return 0
	}
	return float64(s.TotalDurationMs) / float64(window.Milliseconds())
}

// RetentionConfig controls how long event data is kept. Non-positive periods
// disable retention.
//
// RequestRetentionDays governs both request and tunnel history: they are the
// same class of per-connection record and there is no reason to age them out
// on different schedules.
type RetentionConfig struct {
	RequestRetentionDays int // proxy request and tunnel history
}

// ProxyRequestStats aggregates request outcomes for one proxy over the event
// retention window, excluding target failures. AvgResponseTime covers successful
// requests only; failed attempts do not measure the proxy's normal latency.
type ProxyRequestStats struct {
	ProxyID         int
	Requests        int64
	Successes       int64
	AvgResponseTime int // milliseconds
}

// Store is the event store. Implementations must be safe for concurrent use.
type Store interface {
	// InsertRequest records one proxied request outcome.
	InsertRequest(ctx context.Context, event RequestEvent) error

	// RequestStats returns today/yesterday request aggregates for the
	// dashboard.
	RequestStats(ctx context.Context) (*RequestStats, error)

	// InsertTunnel records one completed CONNECT tunnel.
	InsertTunnel(ctx context.Context, event TunnelEvent) error

	// TunnelStats aggregates tunnels that closed within the trailing window.
	// Long-lived tunnels still open are not counted — they have no duration
	// or byte total yet.
	TunnelStats(ctx context.Context, window time.Duration) (*TunnelSummary, error)

	// ProxyRollup returns per-proxy request aggregates over the whole event
	// window. It feeds the stats refresher, which denormalizes these numbers
	// onto the proxies table for list sorting and filtering. Target failures
	// are excluded from these proxy reliability statistics.
	ProxyRollup(ctx context.Context) ([]ProxyRequestStats, error)

	// LowSuccessProxies returns the IDs of proxies whose success rate over
	// the trailing window is below minRate percent, counting only proxies
	// with at least minRequests requests in that window, excluding target failures.
	LowSuccessProxies(ctx context.Context, window time.Duration, minRate float64, minRequests int) ([]int, error)

	// TrafficSeries returns request volume and latency percentiles bucketed
	// over the trailing range (one of "1h", "6h", "24h", "7d", "30d" — see
	// SeriesWindow). Buckets with no traffic are zero-filled, so the series
	// is dense and chart-ready.
	TrafficSeries(ctx context.Context, rng string) ([]models.TrafficPoint, error)

	// DomainStats groups traffic by target hostname over the trailing range.
	// domain optionally filters to that hostname and its subdomains. Results
	// are ordered by requests + completed tunnels descending, then hostname.
	DomainStats(ctx context.Context, rng, domain string, limit int) ([]models.DomainStats, error)

	// ApplyRetention removes the history that cfg has expired.
	ApplyRetention(ctx context.Context, cfg RetentionConfig) error
}
