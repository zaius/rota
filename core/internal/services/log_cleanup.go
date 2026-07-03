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

// LogCleanupService handles automatic log cleanup and retention
type LogCleanupService struct {
	db           *database.DB
	settingsRepo *repository.SettingsRepository
	logger       *logger.Logger
	stopChan     chan struct{}
	ticker       *time.Ticker
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
		stopChan:     make(chan struct{}),
	}
}

// Start starts the log cleanup service
func (s *LogCleanupService) Start(ctx context.Context) error {
	s.logger.Info("starting log cleanup service")

	// Get initial settings
	settings, err := s.settingsRepo.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("failed to get settings: %w", err)
	}

	if !settings.LogRetention.Enabled {
		s.logger.Info("log cleanup is disabled")
		return nil
	}

	// Set initial interval
	interval := time.Duration(settings.LogRetention.CleanupIntervalHours) * time.Hour
	s.ticker = time.NewTicker(interval)

	// Run cleanup immediately on start
	go func() {
		if err := s.runCleanup(ctx); err != nil {
			s.logger.Error("failed to run initial cleanup", "error", err)
		}
	}()

	// Start background worker
	go s.worker(ctx)

	return nil
}

// Stop stops the log cleanup service
func (s *LogCleanupService) Stop() {
	s.logger.Info("stopping log cleanup service")
	close(s.stopChan)
	if s.ticker != nil {
		s.ticker.Stop()
	}
}

// worker runs the cleanup job periodically
func (s *LogCleanupService) worker(ctx context.Context) {
	for {
		select {
		case <-s.ticker.C:
			if err := s.runCleanup(ctx); err != nil {
				s.logger.Error("cleanup job failed", "error", err)
			}
		case <-s.stopChan:
			s.logger.Info("log cleanup worker stopped")
			return
		case <-ctx.Done():
			s.logger.Info("log cleanup worker context cancelled")
			return
		}
	}
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

	// Update ticker if interval changed
	newInterval := time.Duration(settings.LogRetention.CleanupIntervalHours) * time.Hour
	if s.ticker != nil {
		currentInterval := time.Duration(settings.LogRetention.CleanupIntervalHours) * time.Hour
		if currentInterval != newInterval {
			s.ticker.Reset(newInterval)
			s.logger.Info("updated cleanup interval", "hours", settings.LogRetention.CleanupIntervalHours)
		}
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

// UpdateSettings updates the cleanup service with new settings
func (s *LogCleanupService) UpdateSettings(ctx context.Context) error {
	settings, err := s.settingsRepo.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("failed to get settings: %w", err)
	}

	if !settings.LogRetention.Enabled {
		s.logger.Info("log cleanup disabled")
		if s.ticker != nil {
			s.ticker.Stop()
		}
		return nil
	}

	// Restart ticker with new interval
	if s.ticker != nil {
		s.ticker.Stop()
	}
	interval := time.Duration(settings.LogRetention.CleanupIntervalHours) * time.Hour
	s.ticker = time.NewTicker(interval)

	// Run cleanup immediately
	go func() {
		if err := s.runCleanup(ctx); err != nil {
			s.logger.Error("failed to run cleanup after settings update", "error", err)
		}
	}()

	return nil
}
