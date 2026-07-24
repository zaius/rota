package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/proxy"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// fakeProxyServer implements ProxyServer for handler tests.
type fakeProxyServer struct {
	sessions []proxy.SessionInfo

	releasedPool   []int    // pool IDs passed to ReleaseSession
	releasedTokens []string // tokens passed to ReleaseSessionToken
	releasedScoped [][]int  // pool scopes passed to ReleaseSessionTokenInPools
}

func (f *fakeProxyServer) ReloadSettings(ctx context.Context) error { return nil }
func (f *fakeProxyServer) EvictProxy(proxyID int)                   {}
func (f *fakeProxyServer) InvalidateUser(username string)           {}
func (f *fakeProxyServer) ListSessions() []proxy.SessionInfo        { return f.sessions }
func (f *fakeProxyServer) SessionsForToken(token string) []proxy.SessionInfo {
	var out []proxy.SessionInfo
	for _, s := range f.sessions {
		if s.Token == token {
			out = append(out, s)
		}
	}
	return out
}
func (f *fakeProxyServer) ReleaseSession(poolID int, token string) bool {
	f.releasedPool = append(f.releasedPool, poolID)
	return true
}
func (f *fakeProxyServer) ReleaseSessionToken(token string) int {
	f.releasedTokens = append(f.releasedTokens, token)
	return 1
}
func (f *fakeProxyServer) ReleaseSessionTokenInPools(token string, poolIDs []int) int {
	f.releasedScoped = append(f.releasedScoped, poolIDs)
	return 1
}
func (f *fakeProxyServer) SetDomainCooldown(proxyID int, domain string, until time.Time, reason string) {
}
func (f *fakeProxyServer) ClearDomainCooldown(proxyID int, domain string) bool { return false }
func (f *fakeProxyServer) ClearProxyDomainCooldowns(proxyID int) int           { return 0 }
func (f *fakeProxyServer) ListDomainCooldowns() []models.ProxyDomainCooldown   { return nil }

func newTestControlHandler(ps ProxyServer) *ProxyControlHandler {
	h := NewProxyControlHandler(nil, nil, logger.New("error"))
	h.SetProxyServer(ps)
	return h
}

func asProxyUser(r *http.Request, mainPool int, fallbacks ...int) *http.Request {
	u := &models.ProxyUser{ID: 1, Username: "alice", MainPoolID: &mainPool, FallbackPoolIDs: fallbacks}
	return r.WithContext(context.WithValue(r.Context(), models.ProxyUserContextKey, u))
}

func TestInvalidateSession_UnknownTokenNotFound(t *testing.T) {
	h := newTestControlHandler(&fakeProxyServer{})

	req := httptest.NewRequest("POST", "/sessions/invalidate", strings.NewReader(`{"token":"nope"}`))
	w := httptest.NewRecorder()
	h.InvalidateSession(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown token, got %d", w.Code)
	}
}

func TestInvalidateSession_TokenRequired(t *testing.T) {
	h := newTestControlHandler(&fakeProxyServer{})

	req := httptest.NewRequest("POST", "/sessions/invalidate", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.InvalidateSession(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without a token, got %d", w.Code)
	}
}

// A proxy user must not see (or invalidate) sessions bound in pools outside its
// own chain; out-of-scope bindings read as not found.
func TestInvalidateSession_ProxyUserOutOfScopeIsNotFound(t *testing.T) {
	ps := &fakeProxyServer{sessions: []proxy.SessionInfo{
		{PoolID: 99, Token: "job42", ProxyID: 7},
	}}
	h := newTestControlHandler(ps)

	req := httptest.NewRequest("POST", "/sessions/invalidate", strings.NewReader(`{"token":"job42"}`))
	req = asProxyUser(req, 1, 2)
	w := httptest.NewRecorder()
	h.InvalidateSession(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a session outside the user's pools, got %d", w.Code)
	}
}

func TestReleaseSession_ProxyUserForeignPoolForbidden(t *testing.T) {
	ps := &fakeProxyServer{}
	h := newTestControlHandler(ps)

	req := httptest.NewRequest("POST", "/sessions/release", strings.NewReader(`{"token":"job42","pool_id":99}`))
	req = asProxyUser(req, 1, 2)
	w := httptest.NewRecorder()
	h.ReleaseSession(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 releasing in a foreign pool, got %d", w.Code)
	}
	if len(ps.releasedPool) != 0 {
		t.Fatal("nothing must be released on a forbidden request")
	}
}

func TestReleaseSession_ProxyUserOwnPoolAllowed(t *testing.T) {
	ps := &fakeProxyServer{}
	h := newTestControlHandler(ps)

	req := httptest.NewRequest("POST", "/sessions/release", strings.NewReader(`{"token":"job42","pool_id":2}`))
	req = asProxyUser(req, 1, 2)
	w := httptest.NewRecorder()
	h.ReleaseSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 releasing in an own pool, got %d", w.Code)
	}
	if len(ps.releasedPool) != 1 || ps.releasedPool[0] != 2 {
		t.Fatalf("expected release in pool 2, got %v", ps.releasedPool)
	}
}

// Without a pool_id, a proxy user's release is scoped to its own pools rather
// than released globally.
func TestReleaseSession_ProxyUserWithoutPoolScopedToOwnPools(t *testing.T) {
	ps := &fakeProxyServer{}
	h := newTestControlHandler(ps)

	req := httptest.NewRequest("POST", "/sessions/release", strings.NewReader(`{"token":"job42"}`))
	req = asProxyUser(req, 1, 2)
	w := httptest.NewRecorder()
	h.ReleaseSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(ps.releasedTokens) != 0 {
		t.Fatal("a proxy user must not trigger a global release")
	}
	if len(ps.releasedScoped) != 1 || len(ps.releasedScoped[0]) != 2 {
		t.Fatalf("expected a release scoped to the user's two pools, got %v", ps.releasedScoped)
	}
}

// Admin (no proxy user in context) keeps the global release behaviour.
func TestReleaseSession_AdminReleasesGlobally(t *testing.T) {
	ps := &fakeProxyServer{}
	h := newTestControlHandler(ps)

	req := httptest.NewRequest("POST", "/sessions/release", strings.NewReader(`{"token":"job42"}`))
	w := httptest.NewRecorder()
	h.ReleaseSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(ps.releasedTokens) != 1 || ps.releasedTokens[0] != "job42" {
		t.Fatalf("expected a global release of the token, got %v", ps.releasedTokens)
	}
}
