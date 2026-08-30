package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// GET /metrics must be public (scrapers don't do login flows) and mounted only
// when a metrics handler was injected — nil means metrics are disabled and the
// route must not exist.
func TestMetricsRouteRegistration(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	withMetrics := &Server{
		router:      chi.NewRouter(),
		metricsHTTP: stub,
		authRL:      newAuthRateLimiter(5, 1, 1, 60, 1, false, nil),
	}
	withMetrics.setupRoutes()

	rec := httptest.NewRecorder()
	withMetrics.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /metrics with handler = %d, want %d", rec.Code, http.StatusOK)
	}

	withoutMetrics := &Server{
		router: chi.NewRouter(),
		authRL: newAuthRateLimiter(5, 1, 1, 60, 1, false, nil),
	}
	withoutMetrics.setupRoutes()

	rec = httptest.NewRecorder()
	withoutMetrics.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("GET /metrics without handler = %d, want a non-200", rec.Code)
	}
}
