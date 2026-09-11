package services

import (
	"context"
	"time"

	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

const (
	historyCleanupInterval = 24 * time.Hour
	// Matches the bootstrap retention policies for requests and tunnels.
	requestRetentionDays = 90
)

// HistoryCleanupService enforces retention for request and tunnel history.
type HistoryCleanupService struct {
	events events.Store
	logger *logger.Logger
}

func NewHistoryCleanupService(eventStore events.Store, log *logger.Logger) *HistoryCleanupService {
	return &HistoryCleanupService{events: eventStore, logger: log}
}

func (s *HistoryCleanupService) Name() string { return "history-cleanup" }

// Run applies retention at startup and daily until the service is stopped.
func (s *HistoryCleanupService) Run(ctx context.Context) {
	ticker := time.NewTicker(historyCleanupInterval)
	defer ticker.Stop()

	for {
		if err := s.events.ApplyRetention(ctx, events.RetentionConfig{
			RequestRetentionDays: requestRetentionDays,
		}); err != nil {
			s.logger.Error("failed to apply history retention", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
