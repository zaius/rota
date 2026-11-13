package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// Exercise real auth parsing, target-host extraction, selection, and both
// response paths without opening any upstream connection on exhaustion.
func TestProxyRouter_CapacityResponse(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodConnect} {
		for _, scenario := range []string{"reserved", "explicit_scope", "empty", "cooled"} {
			t.Run(method+"/"+scenario, func(t *testing.T) {
				r := newTestRotator(t, ids(42)...)
				username := "alice-session-new"
				owner := Options{Session: "owner"}
				switch scenario {
				case "explicit_scope":
					username += "-scope-shared"
					owner.Scope = "shared"
					fallthrough
				case "reserved":
					if _, err := r.pick(requestCtx("example.com", owner), nil); err != nil {
						t.Fatal(err)
					}
				case "empty":
					r.proxies = nil
				case "cooled":
					r.domainCD.Set(42, "example.com", time.Now().Add(time.Hour), "")
				}
				router := &proxyRouter{upstream: NewUpstreamProxyHandler(r, nil, logger.New("error")), password: "secret"}
				target := "http://Example.COM/path"
				if method == http.MethodConnect {
					target = "Example.COM:443"
				}
				req := httptest.NewRequest(method, target, nil)
				basicProxyAuth(req, username, "secret")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if w.Code != 593 || w.Header().Get("Retry-After") != "5" || w.Header().Get("X-Rota-Error") != "no_proxy_available" {
					t.Fatalf("unexpected capacity response: %d %v %s", w.Code, w.Header(), w.Body.String())
				}
				if !strings.Contains(w.Body.String(), "wait and retry") {
					t.Fatal("missing retry instructions")
				}
				if w.Header().Get(ProxyIDHeader) != "" {
					t.Fatal("exhaustion must not report an upstream proxy")
				}
			})
		}
	}
}

func TestUpstreamHandler_ConfigurationFailureStatus(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodConnect} {
		// An unsupported protocol gives a deterministic upstream/configuration failure.
		r := newTestRotator(t, &models.Proxy{ID: 404, Protocol: "invalid"})
		target := "http://127.0.0.1"
		if method == http.MethodConnect {
			target = "127.0.0.1:443"
		}
		req := httptest.NewRequest(method, target, nil)
		h := NewUpstreamProxyHandler(r, nil, logger.New("error"))
		w := httptest.NewRecorder()
		if method == http.MethodConnect {
			h.HandleConnectRequest(w, req)
		} else {
			h.HandleHTTPRequest(w, req)
		}
		if w.Code != 592 || w.Header().Get(ProxyErrorHeader) != "proxy_configuration_error" || w.Header().Get("Retry-After") != "" {
			t.Fatalf("%s: %d %v", method, w.Code, w.Header())
		}
		if strings.Contains(w.Body.String(), "%!w") {
			t.Fatalf("malformed error: %s", w.Body.String())
		}
	}
}
