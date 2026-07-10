package services

import (
	"context"
	"fmt"
	"time"

	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// defaultCleanupInterval is used when the configured cleanup interval is unset
// or the settings cannot be read.
const defaultCleanupInterval = time.Hour

// requestRetentionDays is how long proxy request history is kept. It matches
// the 90-day default the migrations use for the proxy_requests retention
// policy; it becomes a user setting when request retention grows a UI knob.
const requestRetentionDays = 90

// LogCleanupService handles automatic log cleanup and retention
type LogCleanupService struct {
	events       events.Store
	settingsRepo *repository.SettingsRepository
	logger       *logger.Logger
}

// NewLogCleanupService creates a new log cleanup service
func NewLogCleanupService(
	eventStore events.Store,
	settingsRepo *repository.SettingsRepository,
	log *logger.Logger,
) *LogCleanupService {
	return &LogCleanupService{
		events:       eventStore,
		settingsRepo: settingsRepo,
		logger:       log,
	}
}

// Name identifies the service for the lifecycle manager.
func (s *LogCleanupService) Name() string { return "log-cleanup" }

// Run applies retention/compression policies on the configured interval until
// ctx is cancelled. The interval is re-derived from settings each cycle, so
// changing it (or enabling/disabling cleanup) via the API takes effect without
// a restart — runCleanup itself is a no-op while retention is disabled.
func (s *LogCleanupService) Run(ctx context.Context) {
	interval := s.cleanupInterval(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run once on startup.
	if err := s.runCleanup(ctx); err != nil {
		s.logger.Error("failed to run initial cleanup", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.runCleanup(ctx); err != nil {
				s.logger.Error("cleanup job failed", "error", err)
			}
			// Pick up interval changes made via the API since the last cycle.
			if next := s.cleanupInterval(ctx); next != interval {
				interval = next
				ticker.Reset(interval)
				s.logger.Info("updated cleanup interval", "interval", interval)
			}
		}
	}
}

// cleanupInterval reads the configured cleanup interval, falling back to a sane
// default when settings are unavailable or unset.
func (s *LogCleanupService) cleanupInterval(ctx context.Context) time.Duration {
	settings, err := s.settingsRepo.GetAll(ctx)
	if err != nil || settings.LogRetention.CleanupIntervalHours <= 0 {
		return defaultCleanupInterval
	}
	return time.Duration(settings.LogRetention.CleanupIntervalHours) * time.Hour
}

// runCleanup performs the actual cleanup
func (s *LogCleanupService) runCleanup(ctx context.Context) error {
	s.logger.Info("running log cleanup")

	// Get current settings
	settings, err := s.settingsRepo.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("failed to get settings: %w", err)
	}

	if !settings.LogRetention.Enabled {
		s.logger.Info("log cleanup is disabled, skipping")
		return nil
	}

	// (Re)apply retention config; the event store decides the mechanism.
	err = s.events.ApplyRetention(ctx, events.RetentionConfig{
		RetentionDays:        settings.LogRetention.RetentionDays,
		CompressionAfterDays: settings.LogRetention.CompressionAfterDays,
		RequestRetentionDays: requestRetentionDays,
	})
	if err != nil {
		s.logger.Error("failed to apply retention config", "error", err)
		return nil // logged, not fatal — retried next cycle
	}

	s.logger.Info("log cleanup completed",
		"retention_days", settings.LogRetention.RetentionDays,
		"compression_after_days", settings.LogRetention.CompressionAfterDays,
	)

	return nil
}
