package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
func (f *fakeProxyServer) ListSessions() []proxy.SessionInfo        { return f.sessions }
func (f *fakeProxyServer) FindSessions(filter proxy.SessionFilter) []proxy.SessionInfo {
	var out []proxy.SessionInfo
	for _, s := range f.sessions {
		if s.Token == filter.Token && (filter.Scope == "" || s.Scope == filter.Scope) {
			out = append(out, s)
		}
	}
	return out
}
func (f *fakeProxyServer) ReleaseSessions(filter proxy.SessionFilter) int {
	f.released = append(f.released, filter)
	return 1
}
func (f *fakeProxyServer) SetDomainCooldown(c models.ProxyDomainCooldown) {
	f.domainCooldowns = append(f.domainCooldowns, c)
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
	h := NewProxyControlHandler(nil, logger.New("error"))
	h.SetProxyServer(ps)
	return h
}

func TestInvalidateSession_UnknownTokenNotFound(t *testing.T) {
	h := newTestControlHandler(&fakeProxyServer{})

	req := httptest.NewRequest("POST", "/sessions/invalidate", strings.NewReader(`{"token":"nope"}`))
	w := httptest.NewRecorder()
	h.InvalidateSession(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for an unknown token, got %d", w.Code)
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

func TestReleaseSession_Filter(t *testing.T) {
	for body, want := range map[string]proxy.SessionFilter{
		`{"token":"job42"}`:                        {Token: "job42"},
		`{"token":"job42","scope":"Example.COM."}`: {Token: "job42", Scope: "example.com"},
	} {
		ps := &fakeProxyServer{}
		h := newTestControlHandler(ps)
		w := httptest.NewRecorder()
		h.ReleaseSession(w, httptest.NewRequest("POST", "/sessions/release", strings.NewReader(body)))
		if w.Code != http.StatusOK || len(ps.released) != 1 || ps.released[0] != want {
			t.Fatalf("%s: got %d %+v", body, w.Code, ps.released)
		}
	}
}

func TestInvalidateSession_UnknownScopeNotFound(t *testing.T) {
	ps := &fakeProxyServer{sessions: []proxy.SessionInfo{
		{Token: "job42", Scope: "example.com", ProxyID: 42},
	}}
	h := newTestControlHandler(ps)
	req := httptest.NewRequest("POST", "/sessions/invalidate", strings.NewReader(`{"token":"job42","scope":"other.com"}`))
	w := httptest.NewRecorder()
	h.InvalidateSession(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("unrelated scope matched: %d", w.Code)
	}
}
