package services

import (
	"context"
	"time"

	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// StatsRefresher periodically derives per-proxy request aggregates from the
// event store and denormalizes them onto the proxies table. It is the only
// writer of the metric columns (requests, successful_requests,
// avg_response_time) — the request hot path stopped maintaining them — so
// list sorting and filtering keep working from plain SQL at the cost of the
// numbers being at most one interval stale.
type StatsRefresher struct {
	events    events.Store
	proxyRepo *repository.ProxyRepository
	logger    *logger.Logger
	interval  time.Duration
}

// NewStatsRefresher creates a stats refresher running at the given interval.
func NewStatsRefresher(
	eventStore events.Store,
	proxyRepo *repository.ProxyRepository,
	interval time.Duration,
	log *logger.Logger,
) *StatsRefresher {
	return &StatsRefresher{
		events:    eventStore,
		proxyRepo: proxyRepo,
		logger:    log,
		interval:  interval,
	}
}

// Name identifies the service for the lifecycle manager.
func (s *StatsRefresher) Name() string { return "stats-refresher" }

// Run refreshes once at startup (so counters are current right after boot),
// then on every interval tick until ctx is cancelled.
func (s *StatsRefresher) Run(ctx context.Context) {
	s.refresh(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(ctx)
		}
	}
}

func (s *StatsRefresher) refresh(ctx context.Context) {
	stats, err := s.events.ProxyRollup(ctx)
	if err != nil {
		s.logger.Error("stats refresher: rollup failed", "error", err)
		return
	}

	written, err := s.proxyRepo.ApplyRequestStats(ctx, stats)
	if err != nil {
		s.logger.Error("stats refresher: apply failed", "error", err)
		return
	}
	if written > 0 {
		s.logger.Debug("stats refresher: updated proxy stats", "rows", written)
	}
}
