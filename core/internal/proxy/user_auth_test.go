package proxy

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func TestUserAuth_DatabaseFailureIsNotBadCredentials(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://localhost/rota_test")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close() // Fail deterministically without connecting to a database.
	m := newTestUserAuthMw()
	m.userRepo = repository.NewUserRepository(&database.DB{Pool: pool})
	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	basicProxyAuth(req, "alice", "secret")
	_, resp := m.HandleRequest(req)
	if resp == nil || resp.StatusCode != 500 || resp.Header.Get(ProxyErrorHeader) != "rota_internal_error" || resp.Header.Get("Proxy-Authenticate") != "" {
		t.Fatalf("database outage was misreported: %v", resp)
	}
}

func timeInAnHour() time.Time {
	return time.Now().Add(time.Hour)
}

func bcryptHashForTest(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
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
// stripped before forwarding. The cache stands in for the DB lookup.
func TestUserAuth_ResolvedUserGetsChain(t *testing.T) {
	m := newTestUserAuthMw()
	chain := &PoolChain{}
	m.cache["alice"] = userEntry{
		chain:          chain,
		expiresAt:      timeInAnHour(),
		verifiedPwHash: bcryptHashForTest(t, "secret"),
	}

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
	m.cache["alice"] = userEntry{chain: chain, expiresAt: timeInAnHour(), verifiedPwHash: bcryptHashForTest(t, "secret")}
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
