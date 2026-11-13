package proxy

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// newTestRotator builds a Rotator over a fixed proxy list, with no database.
func newTestRotator(t *testing.T, proxies ...*models.Proxy) *Rotator {
	t.Helper()
	sm := NewSessionManager()
	t.Cleanup(sm.Stop)
	cd := NewDomainCooldownManager()
	t.Cleanup(cd.Stop)
	r := NewRotator(nil, sm, cd, nil, nil, logger.New("error"))
	r.proxies = proxies
	return r
}

func ids(ids ...int) []*models.Proxy {
	var proxies []*models.Proxy
	for _, id := range ids {
		proxies = append(proxies, &models.Proxy{ID: id})
	}
	return proxies
}

func proxyCount(r *Rotator) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.proxies)
}

// requestCtx carries what the router attaches to a request.
func requestCtx(host string, opts Options) context.Context {
	return withRoute(context.Background(), route{Options: opts, host: normalizeHost(host)})
}

func ctxWithToken(token string) context.Context { return requestCtx("", Options{Session: token}) }

func basicProxyAuth(req *http.Request, username, password string) {
	creds := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	req.Header.Set("Proxy-Authorization", "Basic "+creds)
}
