// Package events defines the event store: the single boundary through which
// the application reads and writes time-series event data (system logs and
// per-request proxy history).
//
// Everything behind this interface is an implementation detail of one storage
// backend. Two rules keep backends swappable (Postgres today, ClickHouse
// planned):
//
//  1. No SQL may join event tables with control-plane tables (proxies, pools,
//     settings, ...). Cross-store merges happen in Go, on the caller's side.
//  2. Methods are business questions ("success-rate chart for the last day"),
//     never raw SQL passthrough. If a caller needs a new shape of answer, the
//     interface grows a method and every backend implements it.
package events

import (
	"context"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

// LogEntry is a system log event to be recorded.
//
// Source identifies the subsystem that produced the log (e.g. "proxy") and is
// a first-class field so backends can index or column-ize it; how it is stored
// is the backend's business. Metadata carries free-form attributes for display.
type LogEntry struct {
	Level    string
	Message  string
	Details  *string
	Source   string
	Metadata map[string]any
}

// LogFilter narrows log listings. Zero values mean "no filter".
type LogFilter struct {
	Level     string
	Search    string // substring match on message, case-insensitive
	Source    string
	StartTime *time.Time
	EndTime   *time.Time
}

// RequestEvent is one proxied request outcome.
type RequestEvent struct {
	ProxyID      int
	ProxyAddress string
	Method       string
	URL          string
	StatusCode   int // 0 = no response
	ResponseTime int // milliseconds
	Success      bool
	Error        string
	Timestamp    time.Time
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

// RetentionConfig controls how long event data is kept. CompressionAfterDays
// is advisory: backends without a compression concept ignore it.
type RetentionConfig struct {
	RetentionDays        int
	CompressionAfterDays int
}

// Store is the event store. Implementations must be safe for concurrent use.
type Store interface {
	// InsertLog records a system log event.
	InsertLog(ctx context.Context, entry LogEntry) error

	// ListLogs returns one page of logs matching the filter, newest first,
	// along with the total match count.
	ListLogs(ctx context.Context, filter LogFilter, page, limit int) ([]models.Log, int, error)

	// LogsSince returns up to limit logs with ID greater than lastID in
	// ascending ID order, optionally filtered by source. It backs live log
	// streaming; IDs are monotonic per backend.
	LogsSince(ctx context.Context, lastID int64, limit int, source string) ([]models.Log, error)

	// DeleteLogsOlderThan removes logs older than the given age and reports
	// how many were deleted. Backends with native retention may prefer
	// ApplyRetention; this is the portable fallback.
	DeleteLogsOlderThan(ctx context.Context, age time.Duration) (int64, error)

	// InsertRequest records one proxied request outcome.
	InsertRequest(ctx context.Context, event RequestEvent) error

	// RequestStats returns today/yesterday request aggregates for the
	// dashboard.
	RequestStats(ctx context.Context) (*RequestStats, error)

	// ResponseTimeChart returns average response time of successful requests
	// bucketed over time. Interval is one of "1h", "4h", "1d".
	ResponseTimeChart(ctx context.Context, interval string) ([]models.ChartDataPoint, error)

	// SuccessRateChart returns success/failure percentages bucketed over
	// time. Interval is one of "1h", "4h", "1d".
	SuccessRateChart(ctx context.Context, interval string) ([]models.SuccessRateDataPoint, error)

	// ApplyRetention (re)applies the retention configuration to the backing
	// store. Implementations decide the mechanism (policies, TTLs, no-op).
	ApplyRetention(ctx context.Context, cfg RetentionConfig) error
}
