package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// Exercise real auth parsing, target-host extraction, chain selection, and both
// response paths without opening any upstream connection on exhaustion.
func TestProxyRouter_CapacityResponse(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodConnect} {
		for _, scenario := range []string{"reserved", "explicit_scope", "empty", "cooled"} {
			t.Run(method+"/"+scenario, func(t *testing.T) {
				sm := NewSessionManager()
				defer sm.Stop()
				cd := NewDomainCooldownManager()
				defer cd.Stop()
				ps := newDomainSelector("session", sm, cd, 42)
				chain := &PoolChain{username: "alice", selectors: []*PoolSelector{ps}, targetResolver: ipv4TargetResolver{}}
				username := "alice-session-new"
				ctx := context.WithValue(ctxWithToken("owner"), TargetHostContextKey, "example.com")
				switch scenario {
				case "explicit_scope":
					username += "-scope-shared"
					ctx = context.WithValue(ctx, SessionScopeContextKey, "shared")
					fallthrough
				case "reserved":
					if _, _, err := chain.pickProxy(ctx, nil); err != nil {
						t.Fatal(err)
					}
				case "empty":
					ps.proxies = nil
				case "cooled":
					cd.Set(42, "example.com", time.Now().Add(time.Hour), "")
				}
				auth := newTestUserAuthMw()
				cacheTestUser(auth, chain)
				router := &proxyRouter{
					userAuthMw: auth,
					upstream:   NewUpstreamProxyHandler(nil, nil, logger.New("error")),
				}
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
		chain := &PoolChain{maxRetry: 2, selectors: []*PoolSelector{newMethodSelector("roundrobin", &models.Proxy{ID: 404, Protocol: "invalid"})}}
		target := "http://127.0.0.1"
		if method == http.MethodConnect {
			target = "127.0.0.1:443"
		}
		req := httptest.NewRequest(method, target, nil)
		req = req.WithContext(context.WithValue(req.Context(), UserChainContextKey, chain))
		h := NewUpstreamProxyHandler(nil, nil, logger.New("error"))
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
