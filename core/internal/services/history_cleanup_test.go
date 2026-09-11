package services

import (
	"context"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

type retentionStore struct {
	events.Store
	applied chan events.RetentionConfig
}

func (s *retentionStore) ApplyRetention(ctx context.Context, cfg events.RetentionConfig) error {
	select {
	case s.applied <- cfg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestHistoryCleanupAppliesRetentionAndStops(t *testing.T) {
	store := &retentionStore{applied: make(chan events.RetentionConfig, 1)}
	service := NewHistoryCleanupService(store, logger.New("error"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Run(ctx)
	}()

	select {
	case cfg := <-store.applied:
		if cfg.RequestRetentionDays != 90 {
			t.Fatalf("history retention: want 90 days, got %d", cfg.RequestRetentionDays)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("history retention was not applied at startup")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("history cleanup did not stop on cancellation")
	}
}
