package services

import (
	"context"
	"sync"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

// Service is a long-running background job managed by a Manager.
//
// Previously each background job open-coded its own ticker + goroutine +
// shutdown handling and was started with a never-cancelled context.Background(),
// so none of them actually stopped on shutdown. Implementing this interface lets
// the composition root own their lifecycle uniformly.
type Service interface {
	// Name identifies the service in logs.
	Name() string
	// Run executes the service until ctx is cancelled, then returns. It MUST
	// block: the Manager runs it in its own goroutine and waits for it to return
	// during shutdown.
	Run(ctx context.Context)
}

// Manager starts a set of background services against a single cancellable
// context and joins them on shutdown.
type Manager struct {
	logger   *logger.Logger
	services []Service
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewManager creates a Manager for the given services.
func NewManager(log *logger.Logger, svcs ...Service) *Manager {
	return &Manager{logger: log, services: svcs}
}

// Start launches every registered service in its own goroutine, bound to a
// context derived from ctx. It returns immediately.
func (m *Manager) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	for _, svc := range m.services {
		svc := svc
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.logger.Info("background service started", "service", svc.Name())
			svc.Run(runCtx)
			m.logger.Info("background service stopped", "service", svc.Name())
		}()
	}
}

// Stop cancels the services' context and waits for all of them to return,
// bounded by ctx. It returns ctx.Err() if the deadline is reached before every
// service has stopped.
func (m *Manager) Stop(ctx context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
