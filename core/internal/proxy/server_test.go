package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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

	// A request without resolvable proxy-user credentials must be rejected at
	// the router level — there is no unauthenticated path.
	router := &proxyRouter{
		userAuthMw: NewTestUserAuthMiddleware(),
		upstream:   nil, // won't be reached due to auth rejection
		logger:     log,
	}

	req := httptest.NewRequest("GET", "http://example.com/path", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusProxyAuthRequired {
		t.Fatalf("expected 407, got %d", w.Code)
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
