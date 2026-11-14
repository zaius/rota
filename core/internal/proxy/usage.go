package proxy

import (
	"context"
	"fmt"
	"time"

	"github.com/zaius/rota/core/internal/events"
	"github.com/zaius/rota/core/internal/metrics"
	"github.com/zaius/rota/core/internal/repository"
)

// UsageTracker records request and tunnel outcomes in the event store and
// health-check verdicts on the proxy row.
type UsageTracker struct {
	events events.Store
	repo   *repository.ProxyRepository
}

// NewUsageTracker creates a new usage tracker
func NewUsageTracker(eventStore events.Store, repo *repository.ProxyRepository) *UsageTracker {
	return &UsageTracker{
		events: eventStore,
		repo:   repo,
	}
}

// RequestRecord represents a single proxy request
type RequestRecord struct {
	ProxyID       int
	ProxyAddress  string
	Client        string // client label from the proxy username
	RequestedURL  string
	Method        string
	Success       bool
	TargetFailure bool // target rejection or client abort, not the proxy's fault
	ResponseTime  int  // milliseconds
	StatusCode    int
	ErrorMessage  string
	Timestamp     time.Time
}

// RecordRequest records a proxy request in the event store.
func (t *UsageTracker) RecordRequest(ctx context.Context, record RequestRecord) error {
	metrics.RecordProxyRequest(ctx, record.Client, record.Success, record.StatusCode, record.ResponseTime)

	// Record the request outcome in the event store. The target domain is
	// derived from the URL with the same normalization as domain cooldowns,
	// so per-domain analytics line up with proxy_domain_cooldowns entries.
	err := t.events.InsertRequest(ctx, events.RequestEvent{
		ProxyID:       record.ProxyID,
		ProxyAddress:  record.ProxyAddress,
		Username:      record.Client,
		Method:        record.Method,
		URL:           record.RequestedURL,
		Domain:        NormalizeCooldownDomain(record.RequestedURL),
		StatusCode:    record.StatusCode,
		ResponseTime:  record.ResponseTime,
		Success:       record.Success,
		Error:         record.ErrorMessage,
		Timestamp:     record.Timestamp,
		TargetFailure: record.TargetFailure,
	})
	if err != nil {
		return fmt.Errorf("failed to insert proxy request: %w", err)
	}
	return nil
}

// TunnelRecord represents one completed CONNECT tunnel.
type TunnelRecord struct {
	ProxyID      int
	ProxyAddress string
	Client       string // client label from the proxy username
	Host         string // CONNECT authority, host:port
	BytesUp      int64  // client → target
	BytesDown    int64  // target → client
	Requests     int    // HTTP requests seen inside; 0 when not inspected
	Duration     time.Duration
	ErrorMessage string
	OpenedAt     time.Time
}

// RecordTunnel records a completed CONNECT tunnel in the event store.
func (t *UsageTracker) RecordTunnel(ctx context.Context, record TunnelRecord) error {
	metrics.RecordProxyTunnel(ctx, record.Client, record.ErrorMessage == "",
		record.BytesUp, record.BytesDown, int(record.Duration.Milliseconds()))

	err := t.events.InsertTunnel(ctx, events.TunnelEvent{
		ProxyID:      record.ProxyID,
		ProxyAddress: record.ProxyAddress,
		Username:     record.Client,
		Host:         record.Host,
		Domain:       NormalizeCooldownDomain(record.Host),
		BytesUp:      record.BytesUp,
		BytesDown:    record.BytesDown,
		Requests:     record.Requests,
		DurationMs:   int(record.Duration.Milliseconds()),
		Error:        record.ErrorMessage,
		OpenedAt:     record.OpenedAt,
	})
	if err != nil {
		return fmt.Errorf("failed to insert proxy tunnel: %w", err)
	}
	return nil
}

// RecordHealthCheck records a health check result. Checks are deliberate
// probes, so the result applies to the proxy's status immediately rather than
// waiting out the consecutive-failure threshold that request failures use.
func (t *UsageTracker) RecordHealthCheck(ctx context.Context, proxyID int, success bool, responseTime int) error {
	metrics.RecordHealthCheck(ctx, success, responseTime)
	return t.repo.SetHealthy(ctx, proxyID, success)
}
