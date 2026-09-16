package proxy

import (
	"context"
	"errors"
	"fmt"
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

type connectRequestStore struct {
	events.Store
	records chan events.RequestEvent
	err     error
}

func (s *connectRequestStore) InsertRequest(_ context.Context, event events.RequestEvent) error {
	s.records <- event
	return s.err
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

func TestConnectWithRetry_RejectionDoesNotWalkPoolOrChargeProxy(t *testing.T) {
	for _, status := range []int{301, 403, 407, 429, 500, 502, 503, 504, 592, 593, 594} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if r.Method != http.MethodConnect || r.Host != "target.example:443" {
					t.Errorf("upstream request changed: %s %s", r.Method, r.Host)
				}
				w.WriteHeader(status)
			}))
			defer upstream.Close()
			var proxies []*models.Proxy
			for id := 1; id <= 5; id++ {
				proxies = append(proxies, &models.Proxy{ID: id, Protocol: "http", Address: upstream.Listener.Addr().String()})
			}
			primary := newMethodSelector("roundrobin", proxies[:2]...)
			fallback := newMethodSelector("roundrobin", proxies[2:]...)
			primary.poolID, fallback.poolID = 10, 20
			store := &connectRequestStore{records: make(chan events.RequestEvent, 20)}
			chain := &PoolChain{
				selectors:      []*PoolSelector{primary, fallback},
				maxRetry:       5,
				username:       "alice",
				logger:         logger.New("error"),
				targetResolver: ipv4TargetResolver{},
				tracker:        NewUsageTracker(store, nil), // Target failures must never access the health DB.
				failCounts:     map[int]int{1: 2, 2: 2, 3: 2, 4: 2, 5: 2},
			}
			for attempt := 1; attempt <= 3; attempt++ {
				conn, binding, err := chain.ConnectWithRetry("target.example:443", context.Background(), nil, chain.logger)
				if conn != nil || binding != nil || !isTargetConnectFailure(err) {
					t.Fatalf("unexpected CONNECT result: %v %v %v", conn, binding, err)
				}
				if got := attempts.Load(); got != int32(attempt) {
					t.Fatalf("%d CONNECT requests consumed %d upstream attempts", attempt, got)
				}
				event := store.next(t)
				if event.Success || event.PoolID != 10 || event.ProxyID != (attempt-1)%2+1 || event.Username != "alice" || event.Method != "CONNECT" || event.URL != "CONNECT://target.example:443" || !strings.Contains(event.Error, fmt.Sprint(status)) {
					t.Fatalf("incorrect target rejection record: %+v", event)
				}
			}
			if proxyCount(primary) != 2 || proxyCount(fallback) != 3 || fallback.rrIdx != 0 {
				t.Fatal("CONNECT rejection evicted a proxy or consumed a fallback")
			}
			for id := 1; id <= 5; id++ {
				if chain.failCounts[id] != 2 {
					t.Fatalf("proxy %d failure streak changed: %d", id, chain.failCounts[id])
				}
			}
		})
	}
}

func TestConnectWithRetry_TransportFailureRetriesAndChargesProxy(t *testing.T) {
	for _, failure := range []string{"dial", "malformed reply", "truncated reply"} {
		t.Run(failure, func(t *testing.T) {
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			primary := newMethodSelector("roundrobin", &models.Proxy{ID: 1, Protocol: "http", Address: badAddress})
			fallback := newMethodSelector("roundrobin", &models.Proxy{ID: 2, Protocol: "http", Address: good.Listener.Addr().String()})
			fallback.poolID = 20
			// Stop after capturing the events: this test exercises retry accounting,
			// while database state updates for transport failures are unchanged.
			store := &connectRequestStore{records: make(chan events.RequestEvent, 2), err: errors.New("test: skip health DB")}
			chain := &PoolChain{
				selectors:  []*PoolSelector{primary, fallback},
				maxRetry:   5,
				logger:     logger.New("error"),
				tracker:    NewUsageTracker(store, nil),
				failCounts: map[int]int{1: 2, 2: 2},
			}
			conn, binding, err := chain.ConnectWithRetry("192.0.2.1:443", context.Background(), nil, chain.logger)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if binding.ProxyID != 2 || binding.PoolID != 20 || proxyCount(primary) != 0 || chain.failCounts[2] != 0 {
				t.Fatalf("transport failure did not evict/retry, or success did not reset streak: %+v", binding)
			}
			records := map[int]events.RequestEvent{}
			for range 2 {
				event := store.next(t)
				records[event.ProxyID] = event
			}
			if len(records) != 2 || records[1].Success || records[1].Error == "" || !records[2].Success {
				t.Fatalf("incorrect attempt records: %+v", records)
			}
		})
	}
}

func TestUsageTracker_TargetRejectionDoesNotUpdateHealth(t *testing.T) {
	store := &connectRequestStore{records: make(chan events.RequestEvent, 1)}
	tracker := NewUsageTracker(store, nil)
	// A nil health repository makes this fail if a target rejection either
	// increments the failure counter or resets it as a success.
	err := tracker.RecordRequest(context.Background(), RequestRecord{
		ProxyID: 42, Method: "CONNECT", RequestedURL: "CONNECT://target.example:443",
		TargetFailure: true, ErrorMessage: "502 Bad Gateway", Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if event := store.next(t); event.ProxyID != 42 || event.Success || event.Error != "502 Bad Gateway" {
		t.Fatalf("target failure missing from request history: %+v", event)
	}
}
