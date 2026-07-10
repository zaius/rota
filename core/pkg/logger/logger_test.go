package logger

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func TestAddHook_HookReceivesLogs(t *testing.T) {
	l := New("info")

	var mu sync.Mutex
	var got []string
	l.AddHook(func(level, message string, attrs map[string]any) {
		mu.Lock()
		got = append(got, level+":"+message)
		mu.Unlock()
	})

	l.Info("hello", "k", "v")
	l.Error("boom")

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 2
	})
}

func TestAddHook_AttrsArePassedThrough(t *testing.T) {
	l := New("info")

	var mu sync.Mutex
	var attrs map[string]any
	l.AddHook(func(_, _ string, a map[string]any) {
		mu.Lock()
		attrs = a
		mu.Unlock()
	})

	l.Info("msg", "proxy", "1.2.3.4", "count", 7)

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attrs != nil
	})

	mu.Lock()
	defer mu.Unlock()
	if attrs["proxy"] != "1.2.3.4" || attrs["count"] != 7 {
		t.Fatalf("unexpected attrs: %v", attrs)
	}
}

// A slow hook must not stall the caller: logging stays fast and excess events
// are dropped instead of queuing without bound.
func TestCallHooks_SlowHookDoesNotBlockLogging(t *testing.T) {
	l := New("info")

	release := make(chan struct{})
	var running atomic.Int64
	l.AddHook(func(_, _ string, _ map[string]any) {
		running.Add(1)
		<-release
	})

	// Fill well past the queue so the default branch is exercised.
	start := time.Now()
	for range hookQueueSize + hookWorkers + 500 {
		l.Info("flood")
	}
	elapsed := time.Since(start)
	close(release)

	if elapsed > 2*time.Second {
		t.Fatalf("logging blocked on a slow hook for %s", elapsed)
	}
	if l.dropped.Load() == 0 {
		t.Fatal("expected the full hook queue to drop events rather than grow without bound")
	}
}

// No hooks registered means no workers and no queueing work.
func TestCallHooks_NoHooksIsANoop(t *testing.T) {
	l := New("info")
	l.Info("nobody is listening")

	if got := len(l.hookCh); got != 0 {
		t.Fatalf("expected nothing queued with no hooks, got %d", got)
	}
	if l.dropped.Load() != 0 {
		t.Fatal("expected no drops with no hooks registered")
	}
}

// Registering hooks concurrently with logging must not race.
func TestAddHook_ConcurrentWithLogging(t *testing.T) {
	l := New("info")

	var fired atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for range 50 {
			l.AddHook(func(_, _ string, _ map[string]any) { fired.Add(1) })
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			l.Info("concurrent")
		}
	}()
	wg.Wait()
}
