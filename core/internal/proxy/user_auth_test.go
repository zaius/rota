package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// After the engine collapse, a request that does not map to a proxy user is no
// longer served by a separate legacy engine: the legacy single-user auth gate
// decides whether it is allowed, and if so it is served by the default pool
// chain. These tests cover that routing without a database.

func TestUserAuth_NoCredsAuthDisabledUsesDefaultChain(t *testing.T) {
	def := &PoolChain{}
	m := &UserAuthMiddleware{
		legacy:       NewAuthMiddleware(models.AuthenticationSettings{Enabled: false}),
		defaultChain: def,
		logger:       logger.New("error"),
		cache:        map[string]userEntry{},
	}

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	got, resp := m.HandleRequest(req)
	if resp != nil {
		t.Fatalf("expected no rejection, got status %d", resp.StatusCode)
	}
	chain, ok := chainFromContext(got.Context())
	if !ok || chain != def {
		t.Fatal("expected the default pool chain to be attached to the request")
	}
}

func TestUserAuth_NoCredsAuthEnabledRejects(t *testing.T) {
	m := &UserAuthMiddleware{
		legacy:       NewAuthMiddleware(models.AuthenticationSettings{Enabled: true, Username: "admin", Password: "secret"}),
		defaultChain: &PoolChain{},
		logger:       logger.New("error"),
		cache:        map[string]userEntry{},
	}

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	_, resp := m.HandleRequest(req)
	if resp == nil || resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("expected 407 when auth is enabled and no credentials are provided, got %v", resp)
	}
}
