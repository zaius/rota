package metrics

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/alpkeskin/rota/core/pkg/safeworker"
)

// FleetPoller periodically counts proxies by status in the primary database
// and exposes the counts as an observable gauge. Polling on its own clock keeps
// scrapes and OTLP exports free of database round-trips: a slow or down
// database makes the gauge stale, never the /metrics endpoint slow.
type FleetPoller struct {
	db       *database.DB
	interval time.Duration
	logger   *logger.Logger
	byStatus atomic.Pointer[map[string]int64]
}

// NewFleetPoller creates the poller and registers its observable gauge.
func NewFleetPoller(db *database.DB, interval time.Duration, log *logger.Logger) *FleetPoller {
	p := &FleetPoller{db: db, interval: interval, logger: log}

	proxiesGauge, _ := meter.Int64ObservableGauge("rota.proxies",
		metric.WithDescription("Proxies in the fleet, by status"))

	_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		byStatus := p.byStatus.Load()
		if byStatus == nil {
			return nil // no successful poll yet — observe nothing rather than zeros
		}
		for status, count := range *byStatus {
			o.ObserveInt64(proxiesGauge, count,
				metric.WithAttributes(attribute.String("status", status)))
		}
		return nil
	}, proxiesGauge)

	return p
}

// Name identifies the service for the lifecycle manager.
func (p *FleetPoller) Name() string { return "metrics-fleet-poller" }

// Run refreshes once at startup (so the gauge is warm right after boot), then
// on every interval tick until ctx is cancelled.
func (p *FleetPoller) Run(ctx context.Context) {
	p.refresh(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			safeworker.Call(p.logger, "fleet_metrics", func() { p.refresh(ctx) })
		}
	}
}

func (p *FleetPoller) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	rows, err := p.db.Pool.Query(ctx, `SELECT status, COUNT(*) FROM proxies GROUP BY status`)
	if err != nil {
		p.logger.Warn("metrics fleet poll: proxy counts failed", "error", err)
		return
	}
	defer rows.Close()
	byStatus := make(map[string]int64)
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			p.logger.Warn("metrics fleet poll: proxy counts scan failed", "error", err)
			return
		}
		byStatus[status] = count
	}
	if err := rows.Err(); err != nil {
		p.logger.Warn("metrics fleet poll: proxy counts failed", "error", err)
		return
	}
	// Publish only complete snapshots; a failed poll keeps the previous one.
	p.byStatus.Store(&byStatus)
}
