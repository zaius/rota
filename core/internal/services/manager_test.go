package services

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

// fakeService records that it ran and returns when its context is cancelled,
// unless ignoreCancel is set (used to exercise the Stop timeout path).
type fakeService struct {
	name         string
	started      atomic.Bool
	stopped      atomic.Bool
	ignoreCancel bool
	release      chan struct{}
}

func (f *fakeService) Name() string { return f.name }

func (f *fakeService) Run(ctx context.Context) {
	f.started.Store(true)
	if f.ignoreCancel {
		<-f.release // only returns when the test explicitly releases it
	} else {
		<-ctx.Done()
	}
	f.stopped.Store(true)
}

func testLogger() *logger.Logger { return logger.New("error") }

func TestManagerStartsAndStopsAllServices(t *testing.T) {
	a := &fakeService{name: "a"}
	b := &fakeService{name: "b"}
	m := NewManager(testLogger(), a, b)

	m.Start(context.Background())

	// Wait for both services to have started.
	waitFor(t, func() bool { return a.started.Load() && b.started.Load() }, time.Second)

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Stop(stopCtx); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	if !a.stopped.Load() || !b.stopped.Load() {
		t.Fatalf("expected both services stopped, got a=%v b=%v", a.stopped.Load(), b.stopped.Load())
	}
}

func TestManagerStopTimesOutOnStuckService(t *testing.T) {
	stuck := &fakeService{name: "stuck", ignoreCancel: true, release: make(chan struct{})}
	m := NewManager(testLogger(), stuck)
	m.Start(context.Background())
	waitFor(t, func() bool { return stuck.started.Load() }, time.Second)

	stopCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := m.Stop(stopCtx); err == nil {
		t.Fatal("expected Stop to return a deadline error for a stuck service")
	}

	// Release the service so the goroutine doesn't leak into other tests.
	close(stuck.release)
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
