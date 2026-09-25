package events

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

// recordingStore captures what reaches the backend, and in which calls.
type recordingStore struct {
	Store

	mu            sync.Mutex
	requestCalls  [][]RequestEvent
	tunnelCalls   [][]TunnelEvent
	singleInserts int
	block         chan struct{} // when non-nil, batch writes wait on it
	fail          bool          // batch writes fail
	rejectProxy   int           // single writes fail for this proxy ID
}

func (s *recordingStore) InsertRequests(_ context.Context, events []RequestEvent) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestCalls = append(s.requestCalls, append([]RequestEvent(nil), events...))
	if s.fail {
		return errors.New("store down")
	}
	return nil
}

func (s *recordingStore) InsertTunnels(_ context.Context, events []TunnelEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tunnelCalls = append(s.tunnelCalls, append([]TunnelEvent(nil), events...))
	return nil
}

func (s *recordingStore) InsertRequest(_ context.Context, e RequestEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ProxyID == s.rejectProxy {
		return errors.New("rejected")
	}
	s.singleInserts++
	return nil
}

func (s *recordingStore) batches() (requests [][]RequestEvent, tunnels [][]TunnelEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]RequestEvent(nil), s.requestCalls...), append([][]TunnelEvent(nil), s.tunnelCalls...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBatchWriter_FlushesFullBatchesAndOnInterval(t *testing.T) {
	store := &recordingStore{}
	w := newBatchWriter(store, logger.New("error"), nil, 3, 50*time.Millisecond, 100)
	defer w.Close(context.Background())
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if err := w.InsertRequest(ctx, RequestEvent{ProxyID: i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.InsertTunnel(ctx, TunnelEvent{ProxyID: 9}); err != nil {
		t.Fatal(err)
	}

	// One full batch of 3, then the interval flushes the leftover request and
	// the tunnel.
	waitFor(t, func() bool {
		reqs, tuns := store.batches()
		return len(reqs) == 2 && len(tuns) == 1
	})
	reqs, tuns := store.batches()
	if len(reqs[0]) != 3 || len(reqs[1]) != 1 || reqs[1][0].ProxyID != 3 || tuns[0][0].ProxyID != 9 {
		t.Errorf("batches = %v / %v", reqs, tuns)
	}
}

func TestBatchWriter_CloseFlushesThenWritesThrough(t *testing.T) {
	store := &recordingStore{}
	w := newBatchWriter(store, logger.New("error"), nil, 100, time.Hour, 100)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := w.InsertRequest(ctx, RequestEvent{ProxyID: i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if reqs, _ := store.batches(); len(reqs) != 1 || len(reqs[0]) != 5 {
		t.Fatalf("Close flushed %v, want one batch of 5", reqs)
	}

	if err := w.InsertRequest(ctx, RequestEvent{ProxyID: 99}); err != nil {
		t.Fatal(err)
	}
	if store.singleInserts != 1 {
		t.Errorf("insert after Close: %d direct writes, want 1", store.singleInserts)
	}
	if err := w.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestBatchWriter_FullBufferWaitsForContext(t *testing.T) {
	store := &recordingStore{block: make(chan struct{})}
	w := newBatchWriter(store, logger.New("error"), nil, 1, time.Hour, 1)

	// The writer takes the first event into a batch whose write blocks; the
	// second fills the one-slot buffer; the third has nowhere to go.
	ctx := context.Background()
	if err := w.InsertRequest(ctx, RequestEvent{ProxyID: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(w.requests) == 0 })
	if err := w.InsertRequest(ctx, RequestEvent{ProxyID: 2}); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := w.InsertRequest(short, RequestEvent{ProxyID: 3}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("insert into a full buffer = %v, want the context's deadline error", err)
	}

	close(store.block)
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if reqs, _ := store.batches(); len(reqs) != 2 {
		t.Errorf("batches = %v, want the two accepted events", reqs)
	}
}

func TestBatchWriter_RetriesAFailedBatchOneByOne(t *testing.T) {
	store := &recordingStore{fail: true, rejectProxy: 2}
	var (
		mu      sync.Mutex
		reports []string
	)
	observe := func(_ context.Context, kind string, count int, success bool) {
		mu.Lock()
		defer mu.Unlock()
		outcome := "ok"
		if !success {
			outcome = "failed"
		}
		reports = append(reports, fmt.Sprintf("%s:%s:%d", kind, outcome, count))
	}
	w := newBatchWriter(store, logger.New("error"), observe, 10, time.Hour, 10)
	ctx := context.Background()
	for id := 1; id <= 3; id++ {
		w.InsertRequest(ctx, RequestEvent{ProxyID: id})
	}
	w.InsertTunnel(ctx, TunnelEvent{})
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// The writer retries the failed batch of three one at a time: two land,
	// and it reports the rejected one lost.
	mu.Lock()
	defer mu.Unlock()
	want := []string{"request:ok:2", "request:failed:1", "tunnel:ok:1"}
	if !reflect.DeepEqual(reports, want) || store.singleInserts != 2 {
		t.Errorf("reports = %v (single writes %d), want %v", reports, store.singleInserts, want)
	}
}
