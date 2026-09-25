package safeworker

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

// syncBuffer lets the logger write from another goroutine while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogger() (*logger.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return &logger.Logger{Logger: slog.New(slog.NewJSONHandler(buf, nil))}, buf
}

func TestCallLogsPanicAndReturns(t *testing.T) {
	log, buf := captureLogger()

	Call(log, "test_worker", func() { panic("boom") })

	var entry map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &entry); err != nil {
		t.Fatalf("log output %q: %v", buf.String(), err)
	}
	if entry["level"] != "ERROR" || entry["worker"] != "test_worker" || entry["panic"] != "boom" {
		t.Errorf("log entry = %v", entry)
	}
	if stack, _ := entry["stack"].(string); !strings.Contains(stack, "safeworker") {
		t.Errorf("stack = %q, want a goroutine trace", stack)
	}
}

func TestCallRunsFnWithoutLoggingOnSuccess(t *testing.T) {
	log, buf := captureLogger()
	ran := false

	Call(log, "ok_worker", func() { ran = true })

	if !ran {
		t.Fatal("fn did not run")
	}
	if buf.String() != "" {
		t.Errorf("unexpected log output: %s", buf.String())
	}
}

func TestGoContainsPanic(t *testing.T) {
	log, buf := captureLogger()
	done := make(chan struct{})

	Go(log, "go_worker", func() {
		defer close(done)
		panic(42)
	})
	<-done

	// The recover runs after fn's deferred close, so wait for the log line.
	for i := 0; i < 1000 && !strings.Contains(buf.String(), "go_worker"); i++ {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(buf.String(), `"panic":"42"`) {
		t.Errorf("log output = %q, want the panic value", buf.String())
	}
}
