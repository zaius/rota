package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// mockHandler is a minimal UpstreamProxyHandler replacement for router tests.
type mockHandler struct {
	httpCalled    bool
	connectCalled bool
}

func (m *mockHandler) HandleHTTPRequest(w http.ResponseWriter, r *http.Request) {
	m.httpCalled = true
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("proxied"))
}

func (m *mockHandler) HandleConnectRequest(w http.ResponseWriter, r *http.Request) {
	m.connectCalled = true
	w.WriteHeader(http.StatusOK)
}

func TestProxyRouter_AuthReject(t *testing.T) {
	log := logger.New("error")

	rlMw := NewRateLimitMiddleware(models.RateLimitSettings{Enabled: false})

	// A request without resolvable proxy-user credentials must be rejected at
	// the router level — there is no unauthenticated path.
	router := &proxyRouter{
		userAuthMw:  NewTestUserAuthMiddleware(),
		rateLimitMw: rlMw,
		upstream:    nil, // won't be reached due to auth rejection
		logger:      log,
	}

	req := httptest.NewRequest("GET", "http://example.com/path", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusProxyAuthRequired {
		t.Fatalf("expected 407, got %d", w.Code)
	}
}

func TestProxyRouter_RateLimitReject(t *testing.T) {
	rlMw := NewRateLimitMiddleware(models.RateLimitSettings{
		Enabled:     true,
		Interval:    1,
		MaxRequests: 1,
	})

	// Test rate limiting at the middleware level directly (without full router)
	// to avoid nil pointer on selector/tracker.
	req1, _ := http.NewRequest("GET", "http://example.com/", nil)
	req1.RemoteAddr = "5.5.5.5:5555"

	// First request passes rate limit
	_, rlResp := rlMw.HandleRequest(req1)
	if rlResp != nil {
		t.Fatal("first request should pass rate limit")
	}

	// Second request should be blocked
	req2, _ := http.NewRequest("GET", "http://example.com/", nil)
	req2.RemoteAddr = "5.5.5.5:5555"
	_, rlResp2 := rlMw.HandleRequest(req2)
	if rlResp2 == nil || rlResp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %v", rlResp2)
	}
}

func TestWriteHTTPResponse(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       http.NoBody,
	}
	resp.Header.Set("X-Test", "value")

	w := httptest.NewRecorder()
	writeHTTPResponse(w, resp)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if w.Header().Get("X-Test") != "value" {
		t.Fatal("expected X-Test header")
	}
}

func TestWriteHTTPResponse_WithBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       io.NopCloser(http.NoBody),
	}

	w := httptest.NewRecorder()
	writeHTTPResponse(w, resp)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// NewTestUserAuthMiddleware creates a UserAuthMiddleware without a database,
// for testing the router dispatch. Every request without valid cached
// credentials is rejected.
func NewTestUserAuthMiddleware() *UserAuthMiddleware {
	return &UserAuthMiddleware{
		logger: logger.New("error"),
		cache:  make(map[string]userEntry),
	}
}
