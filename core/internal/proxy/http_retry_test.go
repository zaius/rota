package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/events"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

func httpRetryTestChain(t *testing.T, address string) (*PoolChain, *connectRequestStore) {
	t.Helper()
	var proxies []*models.Proxy
	for id := 1; id <= 5; id++ {
		p := &models.Proxy{ID: id, Protocol: "http", Address: address}
		proxies = append(proxies, p)
		t.Cleanup(func() { InvalidateTransport(p) })
	}
	primary := newMethodSelector("roundrobin", proxies[:2]...)
	fallback := newMethodSelector("roundrobin", proxies[2:]...)
	primary.poolID, fallback.poolID = 10, 20
	store := &connectRequestStore{records: make(chan events.RequestEvent, 10)}
	return &PoolChain{
		selectors:  []*PoolSelector{primary, fallback},
		maxRetry:   5,
		username:   "alice",
		logger:     logger.New("error"),
		tracker:    NewUsageTracker(store, nil), // Client aborts must never access the health DB.
		failCounts: map[int]int{1: 2, 2: 2, 3: 2, 4: 2, 5: 2},
	}, store
}

func assertHTTPAbort(t *testing.T, chain *PoolChain, store *connectRequestStore, err, cause error) {
	t.Helper()
	if !errors.Is(err, cause) || forwardingReason(err) != "client_request_aborted" {
		t.Fatalf("incorrect abort classification: %v", err)
	}
	event := store.next(t)
	if event.ProxyID != 1 || event.PoolID != 10 || event.Username != "alice" || event.Method != "GET" || event.Success || !event.TargetFailure || !strings.Contains(event.Error, "client request aborted") {
		t.Fatalf("incorrect client-abort record: %+v", event)
	}
	for id := 1; id <= 5; id++ {
		if chain.failCounts[id] != 2 {
			t.Fatalf("client abort changed proxy %d's failure streak: %d", id, chain.failCounts[id])
		}
	}
	if proxyCount(chain.selectors[0]) != 2 || proxyCount(chain.selectors[1]) != 3 || chain.selectors[0].rrIdx != 1 || chain.selectors[1].rrIdx != 0 {
		t.Fatal("client abort retried or evicted a proxy")
	}
}

func TestSendWithRetry_ClientAbortStopsAfterCurrentAttempt(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "request context"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			}
			defer cancel()
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if mode != "deadline" {
					cancel()
				}
				if mode == "request context" {
					// The supplied forwarding context stays live. Return an EOF so
					// only the original request's canceled context identifies the abort.
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				<-r.Context().Done()
			}))
			defer upstream.Close()
			chain, store := httpRetryTestChain(t, upstream.Listener.Addr().String())
			req := httptest.NewRequest(http.MethodGet, "http://clients2.google.com/background", nil).WithContext(ctx)
			forwardCtx := ctx
			if mode == "request context" {
				forwardCtx = context.Background()
			}
			resp, proxyID, err := chain.SendWithRetry(req, forwardCtx, nil, chain.logger)
			if resp != nil || proxyID != 0 || attempts.Load() != 1 {
				t.Fatalf("abort result: response=%v proxy=%d attempts=%d", resp, proxyID, attempts.Load())
			}
			assertHTTPAbort(t, chain, store, err, ctx.Err())
		})
	}
}

func TestSendWithRetry_WrappedCancellationStopsRetry(t *testing.T) {
	chain, store := httpRetryTestChain(t, "127.0.0.1:9000")
	var attempts atomic.Int32
	transportCache.Store(transportCacheKey(chain.selectors[0].proxies[0]), &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			attempts.Add(1)
			return nil, fmt.Errorf("transport canceled: %w", context.Canceled)
		},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.gvt1.com/background", nil)
	_, _, err := chain.SendWithRetry(req, req.Context(), nil, chain.logger)
	if attempts.Load() != 1 || req.Context().Err() != nil {
		t.Fatalf("attempts=%d context=%v", attempts.Load(), req.Context().Err())
	}
	assertHTTPAbort(t, chain, store, err, context.Canceled)
}

func TestSendWithRetry_AlreadyCanceledDoesNotSelectProxy(t *testing.T) {
	for _, canceledContext := range []string{"request", "forwarding"} {
		t.Run(canceledContext, func(t *testing.T) {
			chain, store := httpRetryTestChain(t, "127.0.0.1:9000")
			sessions := NewSessionManager()
			defer sessions.Stop()
			chain.selectors[0].method = "session"
			chain.selectors[0].sessionMgr = sessions
			ctx, cancel := context.WithCancel(ctxWithToken("test"))
			cancel()
			req := httptest.NewRequest(http.MethodGet, "http://clients2.google.com/background", nil)
			forwardCtx := ctx
			if canceledContext == "request" {
				req = req.WithContext(ctx)
				forwardCtx = ctxWithToken("test")
			}
			_, _, err := chain.SendWithRetry(req, forwardCtx, nil, chain.logger)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation, got %v", err)
			}
			if len(sessions.List()) != 0 || chain.selectors[0].rrIdx != 0 || len(store.records) != 0 || chain.failCounts[1] != 2 {
				t.Fatal("already canceled request consumed a proxy attempt or reservation")
			}
		})
	}
}

func TestSendWithRetry_UpstreamTimeoutStillRetriesAndChargesProxy(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "healthy proxy")
	}))
	defer upstream.Close()
	chain, store := httpRetryTestChain(t, upstream.Listener.Addr().String())
	store.err = errors.New("test: skip health DB")
	req := httptest.NewRequest(http.MethodGet, "http://clients2.google.com/background", nil)
	resp, proxyID, err := chain.SendWithRetry(req, req.Context(), &models.RotationSettings{Timeout: 1}, chain.logger)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "healthy proxy" || proxyID != 2 || attempts.Load() != 2 || req.Context().Err() != nil {
		t.Fatalf("upstream timeout did not retry successfully: proxy=%d body=%q err=%v", proxyID, body, err)
	}
	if proxyCount(chain.selectors[0]) != 1 || chain.failCounts[2] != 0 {
		t.Fatal("upstream timeout did not evict failed proxy or reset successful proxy's streak")
	}
	records := map[int]events.RequestEvent{}
	for range 2 {
		event := store.next(t)
		records[event.ProxyID] = event
	}
	if len(records) != 2 || records[1].Success || records[1].TargetFailure || !strings.Contains(records[1].Error, "deadline exceeded") || !records[2].Success {
		t.Fatalf("incorrect timeout/retry accounting: %+v", records)
	}
}
