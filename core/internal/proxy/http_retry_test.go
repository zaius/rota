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

	"github.com/zaius/rota/core/internal/events"
	"github.com/zaius/rota/core/internal/models"
)

func httpRetryTestRotator(t *testing.T, address string) (*Rotator, *connectRequestStore) {
	t.Helper()
	var proxies []*models.Proxy
	for id := 1; id <= 5; id++ {
		p := &models.Proxy{ID: id, Protocol: "http", Address: address}
		proxies = append(proxies, p)
		t.Cleanup(func() { InvalidateTransport(p) })
	}
	store := &connectRequestStore{records: make(chan events.RequestEvent, 10)}
	r := newTestRotator(t, proxies...)
	r.tracker = NewUsageTracker(store, nil) // Client aborts must never access the health DB.
	r.failCounts = map[int]int{1: 2, 2: 2, 3: 2, 4: 2, 5: 2}
	return r, store
}

// withClient marks ctx as coming from the "alice" client.
func withClient(ctx context.Context) context.Context {
	return withRoute(ctx, route{Options: Options{Client: "alice"}})
}

func assertHTTPAbort(t *testing.T, r *Rotator, store *connectRequestStore, err, cause error) {
	t.Helper()
	if !errors.Is(err, cause) || forwardingReason(err) != "client_request_aborted" {
		t.Fatalf("incorrect abort classification: %v", err)
	}
	event := store.next(t)
	if event.ProxyID != 1 || event.PoolID != 0 || event.Username != "alice" || event.Method != "GET" || event.Success || !event.TargetFailure || !strings.Contains(event.Error, "client request aborted") {
		t.Fatalf("incorrect client-abort record: %+v", event)
	}
	for id := 1; id <= 5; id++ {
		if r.failCounts[id] != 2 {
			t.Fatalf("client abort changed proxy %d's failure streak: %d", id, r.failCounts[id])
		}
	}
	if proxyCount(r) != 5 || r.uses != 1 {
		t.Fatal("client abort retried or evicted a proxy")
	}
}

// Regression: a client hanging up mid-request retried on other proxies and
// struck each one as failed.
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
			r, store := httpRetryTestRotator(t, upstream.Listener.Addr().String())
			req := httptest.NewRequest(http.MethodGet, "http://clients2.google.com/background", nil).WithContext(ctx)
			forwardCtx := withClient(ctx)
			if mode == "request context" {
				forwardCtx = withClient(context.Background())
			}
			resp, proxyID, err := r.SendWithRetry(req, forwardCtx)
			if resp != nil || proxyID != 0 || attempts.Load() != 1 {
				t.Fatalf("abort result: response=%v proxy=%d attempts=%d", resp, proxyID, attempts.Load())
			}
			assertHTTPAbort(t, r, store, err, ctx.Err())
		})
	}
}

func TestSendWithRetry_WrappedCancellationStopsRetry(t *testing.T) {
	r, store := httpRetryTestRotator(t, "127.0.0.1:9000")
	var attempts atomic.Int32
	transportCache.Store(transportCacheKey(r.proxies[0]), &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			attempts.Add(1)
			return nil, fmt.Errorf("transport canceled: %w", context.Canceled)
		},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.gvt1.com/background", nil)
	_, _, err := r.SendWithRetry(req, withClient(req.Context()))
	if attempts.Load() != 1 || req.Context().Err() != nil {
		t.Fatalf("attempts=%d context=%v", attempts.Load(), req.Context().Err())
	}
	assertHTTPAbort(t, r, store, err, context.Canceled)
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
	r, store := httpRetryTestRotator(t, upstream.Listener.Addr().String())
	store.err = errors.New("test: skip health DB")
	r.SetSettings(&models.RotationSettings{Timeout: 1})
	req := httptest.NewRequest(http.MethodGet, "http://clients2.google.com/background", nil)
	resp, proxyID, err := r.SendWithRetry(req, req.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "healthy proxy" || proxyID != 2 || attempts.Load() != 2 || req.Context().Err() != nil {
		t.Fatalf("upstream timeout did not retry successfully: proxy=%d body=%q err=%v", proxyID, body, err)
	}
	if proxyCount(r) != 4 || r.failCounts[2] != 0 {
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
