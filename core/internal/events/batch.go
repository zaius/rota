package events

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/alpkeskin/rota/core/pkg/safeworker"
)

const (
	defaultBatchSize     = 500
	defaultFlushInterval = time.Second
	defaultBufferSize    = 10_000
	flushTimeout         = 15 * time.Second
)

// FlushObserver hears the outcome of every batch write. kind is "request" or
// "tunnel"; a failed write loses its events.
type FlushObserver func(ctx context.Context, kind string, count int, success bool)

// BatchWriter buffers request and tunnel events and writes them to the
// wrapped store in batches. Recording an event on the proxy's hot path costs a
// channel send rather than a database round trip, and the store takes one
// multi-row insert per flush. Every other Store method passes straight through.
//
// The writer flushes a batch when it fills or when the flush interval elapses,
// so history lags real time by at most the interval. When the buffer is full,
// inserts wait for room until their context expires. Close flushes whatever is
// still buffered; inserts after Close write through to the store directly.
type BatchWriter struct {
	Store
	logger  *logger.Logger
	observe FlushObserver

	batchSize int
	interval  time.Duration

	// mu makes Close exclusive with in-flight sends: once Close holds it and
	// sets closed, no event can reach the channels, so draining them empties
	// them for good.
	mu       sync.RWMutex
	closed   bool
	requests chan RequestEvent
	tunnels  chan TunnelEvent
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

var _ Store = (*BatchWriter)(nil)

// NewBatchWriter starts a writer that batches inserts into store. observe may
// be nil.
func NewBatchWriter(store Store, log *logger.Logger, observe FlushObserver) *BatchWriter {
	return newBatchWriter(store, log, observe, defaultBatchSize, defaultFlushInterval, defaultBufferSize)
}

func newBatchWriter(store Store, log *logger.Logger, observe FlushObserver, batchSize int, interval time.Duration, bufferSize int) *BatchWriter {
	w := &BatchWriter{
		Store:     store,
		logger:    log,
		observe:   observe,
		batchSize: batchSize,
		interval:  interval,
		requests:  make(chan RequestEvent, bufferSize),
		tunnels:   make(chan TunnelEvent, bufferSize),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go w.run()
	return w
}

// InsertRequest queues one request event for the next batch.
func (w *BatchWriter) InsertRequest(ctx context.Context, event RequestEvent) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return w.Store.InsertRequest(ctx, event)
	}
	select {
	case w.requests <- event:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("event buffer full: %w", ctx.Err())
	}
}

// InsertTunnel queues one tunnel event for the next batch.
func (w *BatchWriter) InsertTunnel(ctx context.Context, event TunnelEvent) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return w.Store.InsertTunnel(ctx, event)
	}
	select {
	case w.tunnels <- event:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("event buffer full: %w", ctx.Err())
	}
}

// Close stops batching and waits, bounded by ctx, for the writer to flush the
// buffered events.
func (w *BatchWriter) Close(ctx context.Context) error {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		close(w.stop)
	})
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("event batches not flushed: %w", ctx.Err())
	}
}

func (w *BatchWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	var (
		requests []RequestEvent
		tunnels  []TunnelEvent
	)
	flushRequests := func() {
		if len(requests) > 0 {
			w.flush("request", len(requests),
				func(ctx context.Context) error { return w.Store.InsertRequests(ctx, requests) },
				func(ctx context.Context, i int) error { return w.Store.InsertRequest(ctx, requests[i]) })
			requests = requests[:0]
		}
	}
	flushTunnels := func() {
		if len(tunnels) > 0 {
			w.flush("tunnel", len(tunnels),
				func(ctx context.Context) error { return w.Store.InsertTunnels(ctx, tunnels) },
				func(ctx context.Context, i int) error { return w.Store.InsertTunnel(ctx, tunnels[i]) })
			tunnels = tunnels[:0]
		}
	}
	addRequest := func(e RequestEvent) {
		requests = append(requests, e)
		if len(requests) >= w.batchSize {
			flushRequests()
		}
	}
	addTunnel := func(e TunnelEvent) {
		tunnels = append(tunnels, e)
		if len(tunnels) >= w.batchSize {
			flushTunnels()
		}
	}

	for {
		select {
		case e := <-w.requests:
			addRequest(e)
		case e := <-w.tunnels:
			addTunnel(e)
		case <-ticker.C:
			flushRequests()
			flushTunnels()
		case <-w.stop:
			for {
				select {
				case e := <-w.requests:
					addRequest(e)
				case e := <-w.tunnels:
					addTunnel(e)
				default:
					flushRequests()
					flushTunnels()
					return
				}
			}
		}
	}
}

// flush writes one batch of count events with writeAll. If the batch fails it
// retries the events one at a time with writeOne, so one event the store
// rejects does not take the rest of its batch with it. It logs and reports
// events that still fail rather than retrying them again: holding them would
// stall every event behind them.
func (w *BatchWriter) flush(kind string, count int, writeAll func(context.Context) error, writeOne func(ctx context.Context, i int) error) {
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()

	written, failed := 0, count
	safeworker.Call(w.logger, "event_batch_flush", func() {
		err := writeAll(ctx)
		if err == nil {
			written, failed = count, 0
			return
		}
		if count > 1 {
			w.logger.Warn("event batch failed; writing its events one at a time", "kind", kind, "events", count, "error", err)
			written = 0
			for i := 0; i < count && ctx.Err() == nil; i++ {
				if err = writeOne(ctx, i); err == nil {
					written++
				}
			}
			failed = count - written
			if failed > 0 && err == nil {
				err = ctx.Err() // the deadline cut the retries short
			}
		}
		if failed > 0 {
			w.logger.Error("failed to write events", "kind", kind, "events", failed, "error", err)
		}
	})

	if w.observe != nil {
		if written > 0 {
			w.observe(ctx, kind, written, true)
		}
		if failed > 0 {
			w.observe(ctx, kind, failed, false)
		}
	}
}
