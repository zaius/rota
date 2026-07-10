package proxy

import (
	"context"
	"fmt"
	"time"

	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/repository"
)

// UsageTracker tracks proxy usage and updates statistics: request outcomes are
// recorded as events in the event store, proxy state lives in the primary DB.
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
	ProxyID      int
	ProxyAddress string
	PoolID       int    // pool that served the request; 0 = default pool
	Username     string // proxy user the request was authenticated as
	RequestedURL string
	Method       string
	Success      bool
	ResponseTime int // milliseconds
	StatusCode   int
	ErrorMessage string
	Timestamp    time.Time
}

// RecordRequest records a proxy request and updates statistics
func (t *UsageTracker) RecordRequest(ctx context.Context, record RequestRecord) error {
	// Record the request outcome in the event store. The target domain is
	// derived from the URL with the same normalization as domain cooldowns,
	// so per-domain analytics line up with proxy_domain_cooldowns entries.
	err := t.events.InsertRequest(ctx, events.RequestEvent{
		ProxyID:      record.ProxyID,
		ProxyAddress: record.ProxyAddress,
		PoolID:       record.PoolID,
		Username:     record.Username,
		Method:       record.Method,
		URL:          record.RequestedURL,
		Domain:       NormalizeCooldownDomain(record.RequestedURL),
		StatusCode:   record.StatusCode,
		ResponseTime: record.ResponseTime,
		Success:      record.Success,
		Error:        record.ErrorMessage,
		Timestamp:    record.Timestamp,
	})
	if err != nil {
		return fmt.Errorf("failed to insert proxy request: %w", err)
	}

	// Update proxy statistics
	if err := t.updateProxyStats(ctx, record); err != nil {
		return fmt.Errorf("failed to update proxy stats: %w", err)
	}

	return nil
}

// updateProxyStats advances the proxy's control-plane state machine: status,
// the consecutive-failure counter (failed_requests), last_check and
// last_error. Metrics — request counts and response-time averages — are no
// longer written here; they are derived from the event store by the stats
// refresher (services.StatsRefresher).
//
// The success path only writes on an actual state transition (recovering
// status, resetting a failure streak, clearing an error), so the common case
// — a healthy proxy staying healthy — touches no row. A consequence is that
// last_check advances on transitions and health checks, not on every request.
func (t *UsageTracker) updateProxyStats(ctx context.Context, record RequestRecord) error {
	if record.Success {
		query := `
			UPDATE proxies
			SET
				failed_requests = 0,
				last_error      = NULL,
				status          = 'active',
				last_check      = $2,
				updated_at      = NOW()
			WHERE id = $1
			  AND (status <> 'active' OR failed_requests <> 0 OR last_error IS NOT NULL)
		`
		_, err := t.repo.GetDB().Pool.Exec(ctx, query, record.ProxyID, record.Timestamp)
		return err
	}

	var errorMsg *string
	if record.ErrorMessage != "" {
		errorMsg = &record.ErrorMessage
	}

	// failed_requests counts *consecutive* failures (it resets on success);
	// three in a row marks the proxy failed.
	query := `
		UPDATE proxies
		SET
			failed_requests = failed_requests + 1,
			last_error      = $2,
			last_check      = $3,
			status          = CASE WHEN failed_requests + 1 >= 3 THEN 'failed' ELSE status END,
			updated_at      = NOW()
		WHERE id = $1
	`
	_, err := t.repo.GetDB().Pool.Exec(ctx, query, record.ProxyID, errorMsg, record.Timestamp)
	return err
}

// RecordHealthCheck records a health check result
func (t *UsageTracker) RecordHealthCheck(ctx context.Context, proxyID int, success bool, responseTime int, errorMsg string) error {
	now := time.Now()

	status := "active"
	if !success {
		// Check how many consecutive failures
		var failedRequests int64
		query := `SELECT failed_requests FROM proxies WHERE id = $1`
		if err := t.repo.GetDB().Pool.QueryRow(ctx, query, proxyID).Scan(&failedRequests); err != nil {
			return err
		}

		// Mark as failed after 3 consecutive failures
		if failedRequests >= 2 {
			status = "failed"
		}
	}

	query := `
		UPDATE proxies
		SET
			last_check = $1,
			last_error = $2,
			status = $3,
			updated_at = NOW()
		WHERE id = $4
	`

	var lastError *string
	if errorMsg != "" {
		lastError = &errorMsg
	}

	_, err := t.repo.GetDB().Pool.Exec(ctx, query, now, lastError, status, proxyID)
	return err
}
