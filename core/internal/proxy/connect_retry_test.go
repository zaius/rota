package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zaius/rota/core/internal/events"
	"github.com/zaius/rota/core/internal/models"
)

type connectRequestStore struct {
	events.Store
	records chan events.RequestEvent
}

func (s *connectRequestStore) InsertRequest(_ context.Context, event events.RequestEvent) error {
	s.records <- event
	return nil
}

func (s *connectRequestStore) next(t *testing.T) events.RequestEvent {
	t.Helper()
	select {
	case event := <-s.records:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("request outcome was not recorded")
		return events.RequestEvent{}
	}
}

func TestConnectWithRetry_RejectionRetriesOnceWithoutChargingProxy(t *testing.T) {
	for _, status := range []int{403, 429, 500, 502, 503, 504, 592, 593, 594} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if r.Method != http.MethodConnect || r.Host != "192.0.2.1:443" {
					t.Errorf("upstream request changed: %s %s", r.Method, r.Host)
				}
				w.WriteHeader(status)
			}))
			defer upstream.Close()
			var proxies []*models.Proxy
			for id := 1; id <= 5; id++ {
				proxies = append(proxies, &models.Proxy{ID: id, Protocol: "http", Address: upstream.Listener.Addr().String()})
			}
			store := &connectRequestStore{records: make(chan events.RequestEvent, 20)}
			r := newTestRotator(t, proxies...)
			r.tracker = NewUsageTracker(store, nil)
			r.failCounts = map[int]int{1: 2, 2: 2, 3: 2, 4: 2, 5: 2}
			for request := 1; request <= 3; request++ {
				conn, binding, err := r.ConnectWithRetry("192.0.2.1:443", requestCtx("", Options{Client: "alice"}))
				if conn != nil || binding != nil || forwardingReason(err) != "upstream_connect_rejected" {
					t.Fatalf("unexpected CONNECT result: %v %v %v", conn, binding, err)
				}
				w := httptest.NewRecorder()
				writeProxyError(w, err)
				if w.Header().Get(UpstreamStatusHeader) != fmt.Sprint(status) {
					t.Fatalf("upstream status not reported: %v", w.Header())
				}
				if got := attempts.Load(); got != int32(2*request) {
					t.Fatalf("%d CONNECT requests consumed %d upstream attempts", request, got)
				}
				seen := map[int]bool{}
				for range 2 {
					event := store.next(t)
					if event.Success || !event.TargetFailure || event.PoolID != 0 || event.Username != "alice" || event.Method != "CONNECT" || event.URL != "CONNECT://192.0.2.1:443" || !strings.Contains(event.Error, fmt.Sprint(status)) {
						t.Fatalf("incorrect target rejection record: %+v", event)
					}
					seen[event.ProxyID] = true
				}
				if len(seen) != 2 {
					t.Fatalf("retry reused a proxy: %v", seen)
				}
			}
			if proxyCount(r) != 5 {
				t.Fatal("CONNECT rejection evicted a proxy")
			}
			for id := 1; id <= 5; id++ {
				if r.failCounts[id] != 2 {
					t.Fatalf("proxy %d failure streak changed: %d", id, r.failCounts[id])
				}
			}
		})
	}
}

// A rejection followed by a successful retry serves the tunnel from the second
// proxy, moves the session binding there, and leaves the first proxy's health
// untouched.
func TestConnectWithRetry_RejectionRetryRebindsSession(t *testing.T) {
	var rejected atomic.Int32
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rejected.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer rejecting.Close()
	accepting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer accepting.Close()

	r := newTestRotator(t,
		&models.Proxy{ID: 1, Protocol: "http", Address: rejecting.Listener.Addr().String()},
		&models.Proxy{ID: 2, Protocol: "http", Address: accepting.Listener.Addr().String()},
	)
	store := &connectRequestStore{records: make(chan events.RequestEvent, 3)}
	r.tracker = NewUsageTracker(store, nil)
	r.failCounts[1] = 2
	ctx := requestCtx("192.0.2.1", Options{Client: "etl", Session: "abc"})

	// Bind the session to the rejecting proxy first.
	if p, err := r.pick(ctx, nil); err != nil || p.ID != 1 {
		t.Fatalf("session did not bind proxy 1: %v %v", p, err)
	}

	conn, binding, err := r.ConnectWithRetry("192.0.2.1:443", ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if binding.ProxyID != 2 || rejected.Load() != 1 {
		t.Fatalf("retry did not serve from the second proxy: %+v, %d rejections", binding, rejected.Load())
	}
	if binding.session == nil {
		t.Fatal("tunnel does not hold its session binding")
	}
	binding.Close()
	sessions := r.sessions.List()
	if len(sessions) != 1 || sessions[0].ProxyID != 2 || sessions[0].Scope != "192.0.2.1" {
		t.Fatalf("session binding did not move to the serving proxy: %+v", sessions)
	}
	if proxyCount(r) != 2 || r.failCounts[1] != 2 {
		t.Fatal("rejection struck the first proxy")
	}
	records := map[int]events.RequestEvent{}
	for range 2 {
		event := store.next(t)
		records[event.ProxyID] = event
	}
	if records[1].Success || !records[1].TargetFailure || !records[2].Success || records[2].TargetFailure {
		t.Fatalf("incorrect attempt records: %+v", records)
	}

	// The session now goes straight to the proxy that served it.
	conn, binding, err = r.ConnectWithRetry("192.0.2.1:443", ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if binding.ProxyID != 2 || rejected.Load() != 1 {
		t.Fatalf("session left the rebound proxy: %+v, %d rejections", binding, rejected.Load())
	}
}

func TestConnectWithRetry_ProxyFailureRetriesAndChargesProxy(t *testing.T) {
	for _, failure := range []string{"dial", "malformed reply", "truncated reply", "407", "301"} {
		t.Run(failure, func(t *testing.T) {
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if failure == "407" {
					w.WriteHeader(http.StatusProxyAuthRequired)
					return
				}
				if failure == "301" {
					w.WriteHeader(http.StatusMovedPermanently)
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				if failure == "malformed reply" {
					fmt.Fprint(conn, "garbage 502 Bad Gateway\r\n\r\n")
				} else {
					fmt.Fprint(conn, "HTTP/1.1 200 OK\r\n")
				}
			}))
			defer bad.Close()
			badAddress := bad.Listener.Addr().String()
			if failure == "dial" {
				bad.Close()
			}
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer good.Close()
			r := newTestRotator(t,
				&models.Proxy{ID: 1, Protocol: "http", Address: badAddress},
				&models.Proxy{ID: 2, Protocol: "http", Address: good.Listener.Addr().String()},
			)
			store := &connectRequestStore{records: make(chan events.RequestEvent, 2)}
			r.tracker = NewUsageTracker(store, nil)
			r.failCounts = map[int]int{1: 2, 2: 2}
			conn, binding, err := r.ConnectWithRetry("192.0.2.1:443", context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if binding.ProxyID != 2 || proxyCount(r) != 1 || r.failCounts[2] != 0 {
				t.Fatalf("transport failure did not evict/retry, or success did not reset streak: %+v", binding)
			}
			records := map[int]events.RequestEvent{}
			for range 2 {
				event := store.next(t)
				records[event.ProxyID] = event
			}
			if len(records) != 2 || records[1].Success || records[1].TargetFailure || records[1].Error == "" || !records[2].Success {
				t.Fatalf("incorrect attempt records: %+v", records)
			}
		})
	}
}
