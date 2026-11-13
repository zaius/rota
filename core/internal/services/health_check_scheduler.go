package services

import (
	"context"
	"time"

	"github.com/alpkeskin/rota/core/internal/proxy"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/alpkeskin/rota/core/pkg/safeworker"
)

// HealthCheckScheduler checks every proxy on the health-check interval. It is
// how a failed proxy returns to rotation, since rotation never selects one.
type HealthCheckScheduler struct {
	checker      *proxy.HealthChecker
	settingsRepo *repository.SettingsRepository
	log          *logger.Logger
}

func NewHealthCheckScheduler(checker *proxy.HealthChecker, settingsRepo *repository.SettingsRepository, log *logger.Logger) *HealthCheckScheduler {
	return &HealthCheckScheduler{checker: checker, settingsRepo: settingsRepo, log: log}
}

// Name identifies the service for the lifecycle manager.
func (s *HealthCheckScheduler) Name() string { return "health-check-scheduler" }

// Run reads the interval before each wait, so a settings change applies from
// the next check.
func (s *HealthCheckScheduler) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.interval(ctx)):
			safeworker.Call(s.log, "scheduled_health_check", func() {
				if _, err := s.checker.CheckAllProxies(ctx); err != nil {
					s.log.Error("scheduled health check failed", "error", err)
				}
			})
		}
	}
}

func (s *HealthCheckScheduler) interval(ctx context.Context) time.Duration {
	settings, err := s.settingsRepo.GetAll(ctx)
	if err != nil {
		s.log.Error("failed to load health check interval", "error", err)
		return 30 * time.Minute
	}
	return time.Duration(settings.HealthCheck.IntervalMinutes) * time.Minute
}
