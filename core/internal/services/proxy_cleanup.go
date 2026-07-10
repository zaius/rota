package services

import (
	"context"
	"encoding/json"
	"time"

	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// Low-success cleanup judges proxies on their trailing week, not lifetime
// totals: a proxy that was bad a month ago but healthy now shouldn't be
// deleted, and one that just went bad shouldn't be shielded by a good past.
const (
	lowSuccessWindow      = 7 * 24 * time.Hour
	lowSuccessMinRequests = 10 // enough requests in-window for the rate to mean something
)

// defaultProxyCleanupInterval is used when settings are unavailable or unset.
const defaultProxyCleanupInterval = 24 * time.Hour

// ProxyCleanupService periodically deletes dead/low-quality proxies based on
// the proxy_cleanup settings row.
type ProxyCleanupService struct {
	proxyRepo    *repository.ProxyRepository
	settingsRepo *repository.SettingsRepository
	events       events.Store
	log          *logger.Logger
}

// NewProxyCleanupService creates a new ProxyCleanupService.
func NewProxyCleanupService(
	proxyRepo *repository.ProxyRepository,
	settingsRepo *repository.SettingsRepository,
	eventStore events.Store,
	log *logger.Logger,
) *ProxyCleanupService {
	return &ProxyCleanupService{
		proxyRepo:    proxyRepo,
		settingsRepo: settingsRepo,
		events:       eventStore,
		log:          log,
	}
}

// Name identifies the service for the lifecycle manager.
func (s *ProxyCleanupService) Name() string { return "proxy-cleanup" }

// Run deletes dead/low-quality proxies on the configured cleanup interval until
// ctx is cancelled. The interval is re-derived from settings each cycle, so a
// change made via the API takes effect without a restart.
func (s *ProxyCleanupService) Run(ctx context.Context) {
	interval := s.cleanupInterval(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.run(ctx)
			// Pick up interval changes made via the API since the last cycle.
			if next := s.cleanupInterval(ctx); next != interval {
				interval = next
				ticker.Reset(interval)
				s.log.Info("updated proxy cleanup interval", "interval", interval)
			}
		}
	}
}

// cleanupInterval reads the configured cleanup interval, falling back to a sane
// default when settings are unavailable or unset.
func (s *ProxyCleanupService) cleanupInterval(ctx context.Context) time.Duration {
	cfg, err := s.loadSettings(ctx)
	if err != nil || cfg.CleanupIntervalHours <= 0 {
		return defaultProxyCleanupInterval
	}
	return time.Duration(cfg.CleanupIntervalHours) * time.Hour
}

func (s *ProxyCleanupService) run(ctx context.Context) {
	cfg, err := s.loadSettings(ctx)
	if err != nil {
		s.log.Warn("proxy cleanup: failed to load settings", "error", err)
		return
	}
	if !cfg.Enabled {
		return
	}

	deleted, err := s.proxyRepo.DeleteDeadProxies(ctx, cfg.MaxFailedDays)
	if err != nil {
		s.log.Error("proxy cleanup: delete failed", "error", err)
		return
	}

	// Low-success cleanup: candidates come from the event store over a
	// trailing window, then are deleted by ID in the primary database.
	if cfg.MinSuccessRate > 0 {
		ids, err := s.events.LowSuccessProxies(ctx, lowSuccessWindow, cfg.MinSuccessRate, lowSuccessMinRequests)
		if err != nil {
			s.log.Error("proxy cleanup: low-success lookup failed", "error", err)
		} else if len(ids) > 0 {
			n, err := s.proxyRepo.BulkDelete(ctx, ids)
			if err != nil {
				s.log.Error("proxy cleanup: low-success delete failed", "error", err)
			} else {
				deleted += n
			}
		}
	}

	if deleted > 0 {
		s.log.Info("proxy cleanup: removed dead proxies", "count", deleted)
	}
}

func (s *ProxyCleanupService) loadSettings(ctx context.Context) (models.ProxyCleanupSettings, error) {
	var cfg models.ProxyCleanupSettings
	// Defaults
	cfg.Enabled = false
	cfg.MaxFailedDays = 7
	cfg.CleanupIntervalHours = 24

	m, err := s.settingsRepo.Get(ctx, "proxy_cleanup")
	if err != nil || m == nil {
		return cfg, nil
	}
	// Marshal map back to JSON then decode into struct
	b, _ := json.Marshal(m)
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}
