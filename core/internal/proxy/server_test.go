package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A request without the shared password must be rejected at the router level —
// there is no unauthenticated path.
func TestProxyRouter_AuthReject(t *testing.T) {
	router := &proxyRouter{password: "secret"} // upstream is never reached
	for _, password := range []string{"", "wrong"} {
		req := httptest.NewRequest("GET", "http://example.com/path", nil)
		if password != "" {
			basicProxyAuth(req, "scraper", password)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusProxyAuthRequired || w.Header().Get("Proxy-Authenticate") == "" || w.Header().Get(ProxyErrorHeader) != "proxy_auth_required" {
			t.Fatalf("password %q: got %d %v", password, w.Code, w.Header())
		}
	}
}
