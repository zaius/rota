package proxy

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/authlimit"
	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestUserAuth_DatabaseFailureIsNotBadCredentials(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://localhost/rota_test")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close() // Fail deterministically without connecting to a database.
	m := newTestUserAuthMw()
	m.userRepo = repository.NewUserRepository(&database.DB{Pool: pool})
	m.authLimiter = authlimit.New(1, time.Minute, time.Minute)
	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	basicProxyAuth(req, "alice", "secret")
	for range 3 {
		_, resp := m.HandleRequest(req)
		if resp == nil || resp.StatusCode != 500 || resp.Header.Get(ProxyErrorHeader) != "rota_internal_error" || resp.Header.Get("Proxy-Authenticate") != "" {
			t.Fatalf("database outage was misreported: %v", resp)
		}
	}
}

func TestUserAuthLimitsOnlyCredentialFailures(t *testing.T) {
	m := newTestUserAuthMw()
	m.authLimiter = authlimit.New(2, time.Minute, time.Minute)
	cacheTestUser(m, &PoolChain{})
	for i := range 1500 {
		r := httptest.NewRequest(http.MethodConnect, "http://example.com", nil)
		username := "alice-session-job"
		if i < 20 {
			username += "-profile-invalid-profile"
		}
		basicProxyAuth(r, username, "secret")
		_, resp := m.HandleRequest(r)
		if i < 20 {
			if resp == nil || resp.Header.Get(ProxyErrorHeader) != "invalid_tls_profile" {
				t.Fatalf("unexpected profile rejection: %v", resp)
			}
		} else if resp != nil {
			t.Fatalf("successful traffic exhausted limiter: %v", resp)
		}
	}
	// Challenge-response clients send their first request bare and add
	// credentials only after the 407, so challenges must not consume budget.
	for range 1500 {
		r := httptest.NewRequest(http.MethodConnect, "http://example.com", nil)
		_, resp := m.HandleRequest(r)
		if resp == nil || resp.StatusCode != 407 || resp.Header.Get(ProxyErrorHeader) != "proxy_auth_required" || resp.Header.Get("Retry-After") != "" {
			t.Fatalf("auth challenge counted as a failure: %v", resp)
		}
	}
	calls := 0
	m.userRepo = testUserAuthenticator(func(context.Context, string, string) (*models.ProxyUser, error) {
		calls++
		return nil, repository.ErrProxyAuthentication
	})
	for i, method := range []string{http.MethodGet, http.MethodConnect, http.MethodGet} {
		r := httptest.NewRequest(method, "http://example.com", nil)
		basicProxyAuth(r, "alice", "wrong")
		r.Header.Set("X-Forwarded-For", "203.0.113.99")
		_, resp := m.HandleRequest(r)
		if resp == nil || resp.StatusCode != 407 || resp.Header.Get("Proxy-Authenticate") == "" {
			t.Fatalf("missing auth challenge: %v", resp)
		}
		if i == 2 && (resp.Header.Get("Retry-After") != "60" || resp.Header.Get(ProxyErrorHeader) != "proxy_auth_rate_limited") {
			t.Fatalf("missing block details: %v", resp)
		}
	}
	if calls != 2 {
		t.Fatalf("blocked request reached credential verification: %d calls", calls)
	}
}

func timeInAnHour() time.Time {
	return time.Now().Add(time.Hour)
}

type testUserAuthenticator func(context.Context, string, string) (*models.ProxyUser, error)

func (f testUserAuthenticator) Authenticate(ctx context.Context, username, password string) (*models.ProxyUser, error) {
	return f(ctx, username, password)
}

func cacheTestUser(m *UserAuthMiddleware, chain *PoolChain) {
	m.userRepo = testUserAuthenticator(func(_ context.Context, username, password string) (*models.ProxyUser, error) {
		if username != "alice" || password != "secret" {
			return nil, repository.ErrProxyAuthentication
		}
		return &models.ProxyUser{ID: 1, Username: "alice", Enabled: true}, nil
	})
	m.cache["alice"] = userEntry{chain: chain, expiresAt: timeInAnHour(), userID: 1}
}

// Per-user credentials are the only proxy auth: any request that does not
// resolve to an enabled proxy user is rejected with 407 — including on a
// deployment with no proxy users at all, which blocks rather than running an
// open proxy. These tests cover that routing without a database.

func basicProxyAuth(req *http.Request, username, password string) {
	creds := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	req.Header.Set("Proxy-Authorization", "Basic "+creds)
}

func newTestUserAuthMw() *UserAuthMiddleware {
	return &UserAuthMiddleware{
		logger: logger.New("error"),
		cache:  map[string]userEntry{},
	}
}

func TestUserAuth_NoCredsRejects(t *testing.T) {
	m := newTestUserAuthMw()

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	_, resp := m.HandleRequest(req)
	if resp == nil || resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("expected 407 without credentials, got %v", resp)
	}
	if resp.Header.Get("Proxy-Authenticate") == "" {
		t.Fatal("expected a Proxy-Authenticate challenge on the 407")
	}
}

func TestUserAuth_UnresolvedCredsRejects(t *testing.T) {
	m := newTestUserAuthMw()

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	basicProxyAuth(req, "nosuchuser", "wrongpassword")
	_, resp := m.HandleRequest(req)
	if resp == nil || resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("expected 407 for credentials matching no proxy user, got %v", resp)
	}
}

// A resolved user's chain is attached to the request and the credentials are
// stripped before forwarding. The authenticator stands in for the repository.
func TestUserAuth_ResolvedUserGetsChain(t *testing.T) {
	m := newTestUserAuthMw()
	chain := &PoolChain{}
	cacheTestUser(m, chain)

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	basicProxyAuth(req, "alice-session-job42", "secret")
	got, resp := m.HandleRequest(req)
	if resp != nil {
		t.Fatalf("expected valid credentials to be accepted, got status %d", resp.StatusCode)
	}
	c, ok := chainFromContext(got.Context())
	if !ok || c != chain {
		t.Fatal("expected the user's pool chain to be attached to the request")
	}
	if tok, _ := got.Context().Value(SessionTokenContextKey).(string); tok != "job42" {
		t.Fatalf("expected the session token to be parsed from the username, got %q", tok)
	}
	if got.Header.Get("Proxy-Authorization") != "" {
		t.Fatal("expected Proxy-Authorization to be stripped before forwarding")
	}
}

func TestUserAuth_SessionScopeComposesWithProfile(t *testing.T) {
	m := newTestUserAuthMw()
	chain := &PoolChain{username: "alice"}
	cacheTestUser(m, chain)
	for _, method := range []string{http.MethodGet, http.MethodConnect} {
		req := httptest.NewRequest(method, "http://example.com/", nil)
		basicProxyAuth(req, "alice-session-job42-scope-SHARED-profile-ios", "secret")
		got, reject := m.HandleRequest(req)
		if reject != nil {
			t.Fatalf("%s rejected: %v", method, reject)
		}
		if got.Context().Value(SessionTokenContextKey) != "job42" || got.Context().Value(SessionScopeContextKey) != "shared" {
			t.Fatalf("missing session or scope: %v", got.Context())
		}
		if got.Context().Value(TLSProfileContextKey) == nil {
			t.Fatal("profile not parsed")
		}
		if got.Header.Get("Proxy-Authorization") != "" {
			t.Fatal("credentials leaked")
		}
	}
}

func TestSplitSessionScope(t *testing.T) {
	for _, tc := range []struct{ raw, rest, scope string }{
		{"alice-scope-team", "alice-scope-team", ""},
		{"alice-session-job", "alice-session-job", ""},
		{"alice-session-job-scope-team", "alice-session-job", "team"},
		{"alice-session-job-scope-", "alice-session-job", ""},
		{"alice-scope-team-session-job", "alice-scope-team-session-job", ""},
	} {
		rest, scope := splitSessionScope(tc.raw)
		if rest != tc.rest || scope != tc.scope {
			t.Fatalf("%q: got %q/%q", tc.raw, rest, scope)
		}
	}
}

func TestUserAuth_CachedChainStillRequiresAuthentication(t *testing.T) {
	m := newTestUserAuthMw()
	cacheTestUser(m, &PoolChain{})
	for _, method := range []string{http.MethodGet, http.MethodConnect} {
		req := httptest.NewRequest(method, "http://example.com/", nil)
		basicProxyAuth(req, "alice-session-new-job", "wrong")
		_, resp := m.HandleRequest(req)
		if resp == nil || resp.StatusCode != http.StatusProxyAuthRequired {
			t.Fatalf("%s: cached chain bypassed authentication: %v", method, resp)
		}
	}
}

func TestUserAuth_ChangedUserRebuildsCachedChain(t *testing.T) {
	m := newTestUserAuthMw()
	oldChain := &PoolChain{}
	cacheTestUser(m, oldChain)
	m.userRepo = testUserAuthenticator(func(context.Context, string, string) (*models.ProxyUser, error) {
		return &models.ProxyUser{ID: 1, Username: "alice", Enabled: true, UpdatedAt: time.Now(), MaxRetries: 7}, nil
	})
	chain, err := m.resolve(context.Background(), "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if chain == oldChain || chain.maxRetry != 7 {
		t.Fatal("reused a chain from an older user revision")
	}
}
