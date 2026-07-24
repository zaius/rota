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
func (f *fakeProxyServer) SetDomainCooldown(proxyID int, domain string, until time.Time, reason string) {
}
func (f *fakeProxyServer) ClearDomainCooldown(proxyID int, domain string) bool { return false }
func (f *fakeProxyServer) ClearProxyDomainCooldowns(proxyID int) int           { return 0 }
func (f *fakeProxyServer) ListDomainCooldowns() []models.ProxyDomainCooldown   { return nil }

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

func TestReleaseSession_ReleasesGlobally(t *testing.T) {
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
