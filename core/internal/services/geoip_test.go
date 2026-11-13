package services

import (
	"context"
	"encoding/json"
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
