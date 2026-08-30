package metrics

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// FleetPoller periodically snapshots fleet-wide aggregates from the primary
// database — proxies by status, per-pool counts, proxy users — and exposes
// them as observable gauges. Polling on its own clock keeps scrapes and OTLP
// exports free of database round-trips: a slow or down database makes the
// gauges stale, never the /metrics endpoint slow.
//
// It also maintains the pool ID → name mapping the hot-path labels resolve
// through (see SetPoolNames).
type FleetPoller struct {
	db       *database.DB
	poolRepo *repository.PoolRepository
	interval time.Duration
	logger   *logger.Logger
	snapshot atomic.Pointer[fleetSnapshot]
}

type poolCounts struct {
	name   string
	active int64
	failed int64
	other  int64
}

type fleetSnapshot struct {
	proxiesByStatus map[string]int64
	pools           []poolCounts
	usersEnabled    int64
	usersDisabled   int64
}

// NewFleetPoller creates the poller and registers its observable gauges.
func NewFleetPoller(db *database.DB, poolRepo *repository.PoolRepository, interval time.Duration, log *logger.Logger) *FleetPoller {
	p := &FleetPoller{
		db:       db,
		poolRepo: poolRepo,
		interval: interval,
		logger:   log,
	}

	proxiesGauge, _ := meter.Int64ObservableGauge("rota.proxies",
		metric.WithDescription("Proxies in the fleet, by status"))
	poolProxiesGauge, _ := meter.Int64ObservableGauge("rota.pool.proxies",
		metric.WithDescription("Proxies per pool, by status (other = neither active nor failed)"))
	usersGauge, _ := meter.Int64ObservableGauge("rota.proxy_users",
		metric.WithDescription("Configured proxy users, by enabled state"))

	_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		snap := p.snapshot.Load()
		if snap == nil {
			return nil // no successful poll yet — observe nothing rather than zeros
		}
		for status, count := range snap.proxiesByStatus {
			o.ObserveInt64(proxiesGauge, count,
				metric.WithAttributes(attribute.String("status", status)))
		}
		for _, pool := range snap.pools {
			for status, count := range map[string]int64{
				"active": pool.active, "failed": pool.failed, "other": pool.other,
			} {
				o.ObserveInt64(poolProxiesGauge, count, metric.WithAttributes(
					attribute.String("pool", pool.name),
					attribute.String("status", status),
				))
			}
		}
		o.ObserveInt64(usersGauge, snap.usersEnabled,
			metric.WithAttributes(attribute.Bool("enabled", true)))
		o.ObserveInt64(usersGauge, snap.usersDisabled,
			metric.WithAttributes(attribute.Bool("enabled", false)))
		return nil
	}, proxiesGauge, poolProxiesGauge, usersGauge)

	return p
}

// Name identifies the service for the lifecycle manager.
func (p *FleetPoller) Name() string { return "metrics-fleet-poller" }

// Run refreshes once at startup (so gauges and pool-name labels are warm right
// after boot), then on every interval tick until ctx is cancelled.
func (p *FleetPoller) Run(ctx context.Context) {
	p.refresh(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refresh(ctx)
		}
	}
}

func (p *FleetPoller) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	snap := &fleetSnapshot{proxiesByStatus: make(map[string]int64)}

	rows, err := p.db.Pool.Query(ctx, `SELECT status, COUNT(*) FROM proxies GROUP BY status`)
	if err != nil {
		p.logger.Warn("metrics fleet poll: proxy counts failed", "error", err)
		return
	}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			p.logger.Warn("metrics fleet poll: proxy counts scan failed", "error", err)
			return
		}
		snap.proxiesByStatus[status] = count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		p.logger.Warn("metrics fleet poll: proxy counts failed", "error", err)
		return
	}

	pools, err := p.poolRepo.List(ctx)
	if err != nil {
		p.logger.Warn("metrics fleet poll: pool counts failed", "error", err)
		return
	}
	names := make(map[int]string, len(pools))
	for _, pool := range pools {
		names[pool.ID] = pool.Name
		snap.pools = append(snap.pools, poolCounts{
			name:   pool.Name,
			active: int64(pool.ActiveProxies),
			failed: int64(pool.FailedProxies),
			other:  int64(pool.TotalProxies - pool.ActiveProxies - pool.FailedProxies),
		})
	}

	err = p.db.Pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE enabled),
			COUNT(*) FILTER (WHERE NOT enabled)
		FROM proxy_users
	`).Scan(&snap.usersEnabled, &snap.usersDisabled)
	if err != nil {
		p.logger.Warn("metrics fleet poll: proxy user counts failed", "error", err)
		return
	}

	// Publish only complete snapshots; a partial poll keeps the previous one.
	p.snapshot.Store(snap)
	SetPoolNames(names)
}
