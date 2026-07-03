package services

import (
	"context"
	"fmt"
	"time"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// defaultCleanupInterval is used when the configured cleanup interval is unset
// or the settings cannot be read.
const defaultCleanupInterval = time.Hour

// LogCleanupService handles automatic log cleanup and retention
type LogCleanupService struct {
	db           *database.DB
	settingsRepo *repository.SettingsRepository
	logger       *logger.Logger
}

// NewLogCleanupService creates a new log cleanup service
func NewLogCleanupService(
	db *database.DB,
	settingsRepo *repository.SettingsRepository,
	log *logger.Logger,
) *LogCleanupService {
	return &LogCleanupService{
		db:           db,
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

	// Update retention policy
	if err := s.updateRetentionPolicy(ctx, settings.LogRetention); err != nil {
		s.logger.Error("failed to update retention policy", "error", err)
		// Don't return error, continue with other tasks
	}

	// Update compression policy
	if err := s.updateCompressionPolicy(ctx, settings.LogRetention); err != nil {
		s.logger.Error("failed to update compression policy", "error", err)
		// Don't return error, continue with other tasks
	}

	s.logger.Info("log cleanup completed",
		"retention_days", settings.LogRetention.RetentionDays,
		"compression_after_days", settings.LogRetention.CompressionAfterDays,
	)

	return nil
}

// updateRetentionPolicy updates the TimescaleDB retention policy.
//
// Retention policies are a TSL-licensed TimescaleDB feature, unavailable on
// Apache-only builds (e.g. Azure Flexible Server). The SQL is guarded on the
// license so it is a no-op there instead of erroring on every cleanup cycle —
// matching the guard the migrations use when creating the policies.
func (s *LogCleanupService) updateRetentionPolicy(ctx context.Context, config models.LogRetentionSettings) error {
	query := `
		DO $ts$
		BEGIN
			IF current_setting('timescaledb.license', true) = 'timescale' THEN
				PERFORM remove_retention_policy('logs', if_exists => true);
				PERFORM add_retention_policy('logs', INTERVAL '%d days', if_not_exists => true);
			END IF;
		END
		$ts$;
	`
	sql := fmt.Sprintf(query, config.RetentionDays)

	if _, err := s.db.Pool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("failed to update retention policy: %w", err)
	}

	s.logger.Info("updated retention policy", "retention_days", config.RetentionDays)
	return nil
}

// updateCompressionPolicy updates the TimescaleDB compression policy. Like the
// retention policy above it is TSL-licensed, so it is guarded on the license
// and becomes a no-op on Apache-only builds.
func (s *LogCleanupService) updateCompressionPolicy(ctx context.Context, config models.LogRetentionSettings) error {
	query := `
		DO $ts$
		BEGIN
			IF current_setting('timescaledb.license', true) = 'timescale' THEN
				PERFORM remove_compression_policy('logs', if_exists => true);
				PERFORM add_compression_policy('logs', INTERVAL '%d days', if_not_exists => true);
			END IF;
		END
		$ts$;
	`
	sql := fmt.Sprintf(query, config.CompressionAfterDays)

	if _, err := s.db.Pool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("failed to update compression policy: %w", err)
	}

	s.logger.Info("updated compression policy", "compression_after_days", config.CompressionAfterDays)
	return nil
}
