package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alpkeskin/rota/core/internal/api/handlers"
	"github.com/go-chi/chi/v5"
)

// The Docker HEALTHCHECK and some load balancers probe /health with HEAD
// (e.g. wget --spider); chi only matches registered methods, so HEAD must be
// routed explicitly or the container reports unhealthy on a 405.
func TestHealthRouteAnswersGetAndHead(t *testing.T) {
	s := &Server{
		router:        chi.NewRouter(),
		healthHandler: handlers.NewHealthHandler(nil, nil, nil),
		authRL:        newAuthRateLimiter(5, 1, 1, 60, 1, false, nil),
	}
	s.setupRoutes()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, "/health", nil)
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s /health = %d, want %d", method, rec.Code, http.StatusOK)
		}
	}
}
