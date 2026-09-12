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

	released        []proxy.SessionFilter
	cooldowns       []models.ProxyScopeCooldown
	domainCooldowns []models.ProxyDomainCooldown
	evicted         []int
	clearedScopes   []models.ProxyScopeCooldown
}

func (f *fakeProxyServer) ReloadSettings(ctx context.Context) error { return nil }
func (f *fakeProxyServer) EvictProxy(proxyID int)                   { f.evicted = append(f.evicted, proxyID) }
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
func (f *fakeProxyServer) ReleaseSessions(filter proxy.SessionFilter) int {
	f.released = append(f.released, filter)
	return 1
}
func (f *fakeProxyServer) SetDomainCooldown(proxyID int, domain string, until time.Time, reason string) {
	f.domainCooldowns = append(f.domainCooldowns, models.ProxyDomainCooldown{ProxyID: proxyID, Domain: domain, CooldownUntil: until})
}
func (f *fakeProxyServer) SetScopeCooldown(c models.ProxyScopeCooldown) {
	f.cooldowns = append(f.cooldowns, c)
}
func (f *fakeProxyServer) ClearScopeCooldowns(proxyID int, scope string) int {
	f.clearedScopes = append(f.clearedScopes, models.ProxyScopeCooldown{ProxyID: proxyID, Scope: scope})
	return 1
}
func (f *fakeProxyServer) ListScopeCooldowns() []models.ProxyScopeCooldown     { return f.cooldowns }
func (f *fakeProxyServer) ClearDomainCooldown(proxyID int, domain string) bool { return false }
func (f *fakeProxyServer) ClearProxyDomainCooldowns(proxyID int) int           { return 0 }
func (f *fakeProxyServer) ListDomainCooldowns() []models.ProxyDomainCooldown   { return nil }
func (f *fakeProxyServer) OpenTunnels() int64                                  { return 0 }

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
	if len(ps.released) != 0 {
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
	if len(ps.released) != 1 || len(ps.released[0].PoolIDs) != 1 || ps.released[0].PoolIDs[0] != 2 || ps.released[0].Username != "alice" {
		t.Fatalf("expected release in pool 2, got %v", ps.released)
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
	if len(ps.released) != 1 || ps.released[0].Username != "alice" || len(ps.released[0].PoolIDs) != 2 {
		t.Fatalf("expected a release scoped to the user's two pools, got %v", ps.released)
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
	if len(ps.released) != 1 || ps.released[0].Token != "job42" || ps.released[0].Username != "" || ps.released[0].PoolIDs != nil {
		t.Fatalf("expected a global release of the token, got %v", ps.released)
	}
}

func TestReleaseSession_ScopeAndOwner(t *testing.T) {
	ps := &fakeProxyServer{}
	h := newTestControlHandler(ps)
	req := httptest.NewRequest("POST", "/sessions/release", strings.NewReader(`{"token":"job42","scope":"Example.COM.","username":"bob"}`))
	req = asProxyUser(req, 1, 2)
	w := httptest.NewRecorder()
	h.ReleaseSession(w, req)
	if w.Code != http.StatusOK || len(ps.released) != 1 {
		t.Fatalf("release failed: %d", w.Code)
	}
	f := ps.released[0]
	if f.Username != "alice" || f.Scope != "example.com" || f.Token != "job42" || len(f.PoolIDs) != 2 {
		t.Fatalf("incorrect filter: %+v", f)
	}
}

func TestInvalidateSession_RestrictsScopeAndOwner(t *testing.T) {
	for _, body := range []string{
		`{"token":"job42"}`,
		`{"token":"job42","username":"bob"}`,
		`{"token":"job42","scope":"other.com"}`,
	} {
		ps := &fakeProxyServer{sessions: []proxy.SessionInfo{
			{PoolID: 1, Username: "bob", Token: "job42", Scope: "example.com", ProxyID: 42},
		}}
		h := newTestControlHandler(ps)
		req := asProxyUser(httptest.NewRequest("POST", "/sessions/invalidate", strings.NewReader(body)), 1)
		w := httptest.NewRecorder()
		h.InvalidateSession(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("foreign session visible: %d", w.Code)
		}
	}
}

func TestInvalidateSession_UnknownScopeNotFound(t *testing.T) {
	ps := &fakeProxyServer{sessions: []proxy.SessionInfo{
		{PoolID: 1, Username: "alice", Token: "job42", Scope: "example.com", ProxyID: 42},
	}}
	h := newTestControlHandler(ps)
	req := asProxyUser(httptest.NewRequest("POST", "/sessions/invalidate", strings.NewReader(`{"token":"job42","scope":"other.com"}`)), 1)
	w := httptest.NewRecorder()
	h.InvalidateSession(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unrelated scope matched: %d", w.Code)
	}
}
