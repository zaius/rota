package proxy

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// withConfiguredProxyUsers primes the hasProxyUsers TTL cache so the auth path
// can be exercised without a database behind userRepo.
func withConfiguredProxyUsers(m *UserAuthMiddleware) *UserAuthMiddleware {
	m.usersConfigured = true
	m.usersCheckedUntil = time.Now().Add(time.Hour)
	return m
}

func basicProxyAuth(req *http.Request, username, password string) {
	creds := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	req.Header.Set("Proxy-Authorization", "Basic "+creds)
}

// When proxy users are configured but legacy auth is off, an unauthenticated
// request must not be served by the default chain — that would bypass per-user
// auth entirely.
func TestUserAuth_NoCredsWithProxyUsersRejects(t *testing.T) {
	m := withConfiguredProxyUsers(&UserAuthMiddleware{
		legacy:       NewAuthMiddleware(models.AuthenticationSettings{Enabled: false}),
		defaultChain: &PoolChain{},
		logger:       logger.New("error"),
		cache:        map[string]userEntry{},
	})

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	_, resp := m.HandleRequest(req)
	if resp == nil || resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("expected 407 when proxy users are configured and no credentials are provided, got %v", resp)
	}
}

// Same bypass, reached with credentials that do not resolve to a proxy user.
func TestUserAuth_BadCredsWithProxyUsersRejects(t *testing.T) {
	m := withConfiguredProxyUsers(&UserAuthMiddleware{
		legacy:       NewAuthMiddleware(models.AuthenticationSettings{Enabled: false}),
		defaultChain: &PoolChain{},
		logger:       logger.New("error"),
		cache:        map[string]userEntry{},
	})

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	basicProxyAuth(req, "nosuchuser", "wrongpassword")
	_, resp := m.HandleRequest(req)
	if resp == nil || resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("expected 407 for credentials matching no proxy user, got %v", resp)
	}
}

// Legacy auth still vouches for requests when it is enforcing, even while proxy
// users exist: those requests are served by the default chain.
func TestUserAuth_LegacyCredsWithProxyUsersUsesDefaultChain(t *testing.T) {
	def := &PoolChain{}
	m := withConfiguredProxyUsers(&UserAuthMiddleware{
		legacy:       NewAuthMiddleware(models.AuthenticationSettings{Enabled: true, Username: "admin", Password: "secret"}),
		defaultChain: def,
		logger:       logger.New("error"),
		cache:        map[string]userEntry{},
	})

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	basicProxyAuth(req, "admin", "secret")
	got, resp := m.HandleRequest(req)
	if resp != nil {
		t.Fatalf("expected legacy credentials to be accepted, got status %d", resp.StatusCode)
	}
	chain, ok := chainFromContext(got.Context())
	if !ok || chain != def {
		t.Fatal("expected the default pool chain to be attached to the request")
	}
}
