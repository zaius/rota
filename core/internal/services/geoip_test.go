package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

// geoTestService builds a GeoIPService pointed at a stub endpoint, with the
// outbound throttle effectively disabled so tests stay fast.
func geoTestService(endpoint string) *GeoIPService {
	return &GeoIPService{
		client:      &http.Client{Timeout: 5 * time.Second},
		cache:       make(map[string]cacheEntry),
		logger:      logger.New("error"),
		cacheTTL:    time.Hour,
		minInterval: 0,
		endpoint:    endpoint,
	}
}

// stubGeoAPI echoes a successful response for every queried IP and records how
// many requests and queries it saw.
func stubGeoAPI(t *testing.T, requests *atomic.Int64, queries *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
			return
		}
		var items []struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(body, &items); err != nil {
			t.Errorf("decoding request body: %v", err)
			return
		}
		queries.Add(int64(len(items)))

		out := make([]ipAPIResponse, 0, len(items))
		for _, it := range items {
			out = append(out, ipAPIResponse{Status: "success", CountryCode: "US", Query: it.Query})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}))
}

// Each IP must be requested exactly once — the old code ran a batch loop whose
// results it threw away, then re-fetched every IP in a single oversized call.
func TestLookupBatch_QueriesEachIPOnce(t *testing.T) {
	var requests, queries atomic.Int64
	srv := stubGeoAPI(t, &requests, &queries)
	defer srv.Close()

	g := geoTestService(srv.URL)
	addrs := []string{"1.1.1.1:80", "2.2.2.2:80", "3.3.3.3:80"}

	got := g.LookupBatch(context.Background(), addrs)

	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d", len(got))
	}
	if q := queries.Load(); q != 3 {
		t.Fatalf("expected each of the 3 IPs to be queried once, saw %d queries", q)
	}
	if r := requests.Load(); r != 1 {
		t.Fatalf("expected a single batch request, saw %d", r)
	}
}

// Addresses sharing an IP must not produce duplicate queries.
func TestLookupBatch_DeduplicatesSharedIPs(t *testing.T) {
	var requests, queries atomic.Int64
	srv := stubGeoAPI(t, &requests, &queries)
	defer srv.Close()

	g := geoTestService(srv.URL)
	got := g.LookupBatch(context.Background(), []string{"1.1.1.1:80", "1.1.1.1:9090", "1.1.1.1:1080"})

	if q := queries.Load(); q != 1 {
		t.Fatalf("expected the shared IP to be queried once, saw %d queries", q)
	}
	if len(got) == 0 {
		t.Fatal("expected a result for the shared IP")
	}
}

// More than 100 IPs must be split across requests: the API rejects larger batches.
func TestLookupBatch_ChunksToAPIBatchLimit(t *testing.T) {
	var requests, queries atomic.Int64
	srv := stubGeoAPI(t, &requests, &queries)
	defer srv.Close()

	g := geoTestService(srv.URL)

	addrs := make([]string, 0, 250)
	for i := range 250 {
		addrs = append(addrs, formatTestIP(i)+":80")
	}

	got := g.LookupBatch(context.Background(), addrs)

	if len(got) != 250 {
		t.Fatalf("expected 250 results, got %d", len(got))
	}
	if r := requests.Load(); r != 3 {
		t.Fatalf("expected 250 IPs to be split into 3 requests of at most %d, saw %d", ipAPIBatchSize, r)
	}
	if q := queries.Load(); q != 250 {
		t.Fatalf("expected 250 total queries, saw %d", q)
	}
}

// A second lookup for the same IP is served from the cache.
func TestLookupBatch_UsesCacheOnRepeatLookup(t *testing.T) {
	var requests, queries atomic.Int64
	srv := stubGeoAPI(t, &requests, &queries)
	defer srv.Close()

	g := geoTestService(srv.URL)
	g.LookupBatch(context.Background(), []string{"1.1.1.1:80"})
	g.LookupBatch(context.Background(), []string{"1.1.1.1:80"})

	if r := requests.Load(); r != 1 {
		t.Fatalf("expected the second lookup to hit the cache, saw %d requests", r)
	}
}

func TestSweep_EvictsExpiredEntries(t *testing.T) {
	g := geoTestService("")
	now := time.Now()
	g.cache["1.1.1.1"] = cacheEntry{cachedAt: now.Add(-2 * time.Hour)} // past the 1h TTL
	g.cache["2.2.2.2"] = cacheEntry{cachedAt: now}

	g.sweep(now)

	if _, ok := g.cache["1.1.1.1"]; ok {
		t.Error("expected the expired entry to be evicted")
	}
	if _, ok := g.cache["2.2.2.2"]; !ok {
		t.Error("expected the fresh entry to be retained")
	}
}

// A 429 must be retried after the Retry-After delay rather than failing outright.
func TestDoBatchRequest_BacksOffOn429(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode([]ipAPIResponse{{Status: "success", Query: "1.1.1.1", CountryCode: "US"}})
	}))
	defer srv.Close()

	g := geoTestService(srv.URL)
	g.minInterval = time.Millisecond

	raw, err := g.lookupBatchRaw(context.Background(), []string{"1.1.1.1"})
	if err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if _, ok := raw["1.1.1.1"]; !ok {
		t.Fatal("expected a result after the backoff retry")
	}
	if a := attempts.Load(); a != 2 {
		t.Fatalf("expected exactly one retry, saw %d attempts", a)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("5", time.Second); got != 5*time.Second {
		t.Errorf("expected 5s, got %s", got)
	}
	if got := parseRetryAfter(" 12 ", time.Second); got != 12*time.Second {
		t.Errorf("expected whitespace to be trimmed, got %s", got)
	}
	if got := parseRetryAfter("", time.Second); got != time.Second {
		t.Errorf("expected the fallback for an empty header, got %s", got)
	}
	if got := parseRetryAfter("Wed, 21 Oct 2015 07:28:00 GMT", time.Second); got != time.Second {
		t.Errorf("expected the fallback for an HTTP-date, got %s", got)
	}
	if got := parseRetryAfter("-3", time.Second); got != time.Second {
		t.Errorf("expected the fallback for a negative value, got %s", got)
	}
}

// throttle must space consecutive requests by at least minInterval.
func TestThrottle_SpacesRequests(t *testing.T) {
	g := geoTestService("")
	g.minInterval = 40 * time.Millisecond

	start := time.Now()
	for range 3 {
		if err := g.throttle(context.Background()); err != nil {
			t.Fatalf("throttle: %v", err)
		}
	}
	// First call is free; the next two each wait one interval.
	if elapsed := time.Since(start); elapsed < 2*g.minInterval {
		t.Fatalf("expected requests to be spaced by %s, took only %s", g.minInterval, elapsed)
	}
}

func TestThrottle_RespectsContextCancellation(t *testing.T) {
	g := geoTestService("")
	g.minInterval = 10 * time.Second

	if err := g.throttle(context.Background()); err != nil {
		t.Fatalf("priming throttle: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.throttle(ctx); err == nil {
		t.Fatal("expected throttle to return the context error rather than block")
	}
}

func formatTestIP(i int) string {
	return fmt.Sprintf("10.0.%d.%d", i/256, i%256)
}
