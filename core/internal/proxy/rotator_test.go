package proxy

import (
	"context"
	"encoding/base64"
	"errors"
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
	r.targetResolver = ipv4TargetResolver{}
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

func ctxWithHost(host string) context.Context { return requestCtx(host, Options{}) }

func basicProxyAuth(req *http.Request, username, password string) {
	creds := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	req.Header.Set("Proxy-Authorization", "Basic "+creds)
}

// A single transient failure must not drain the list.
func TestRotator_MarkFailedKeepsProxyBelowThreshold(t *testing.T) {
	r := newTestRotator(t, ids(7)...)
	for i := 1; i < failureThreshold; i++ {
		r.markFailed(7)
		if proxyCount(r) != 1 {
			t.Fatalf("proxy evicted after %d failure(s), threshold is %d", i, failureThreshold)
		}
	}
}

func TestRotator_MarkFailedEvictsAtThreshold(t *testing.T) {
	r := newTestRotator(t, ids(7)...)
	for range failureThreshold {
		r.markFailed(7)
	}
	if proxyCount(r) != 0 {
		t.Fatalf("expected the proxy to be evicted after %d consecutive failures", failureThreshold)
	}
}

// Intervening success resets the streak, so failures must not accumulate across it.
func TestRotator_MarkSucceededResetsFailureStreak(t *testing.T) {
	r := newTestRotator(t, ids(7)...)
	for i := 1; i < failureThreshold; i++ {
		r.markFailed(7)
	}
	r.markSucceeded(7)
	for i := 1; i < failureThreshold; i++ {
		r.markFailed(7)
		if proxyCount(r) != 1 {
			t.Fatalf("failure streak was not reset by an intervening success")
		}
	}
}

// Failures for one proxy must not evict another.
func TestRotator_FailureCountsArePerProxy(t *testing.T) {
	r := newTestRotator(t, ids(1, 2)...)
	for range failureThreshold {
		r.markFailed(1)
	}
	if len(r.proxies) != 1 || r.proxies[0].ID != 2 {
		t.Fatalf("expected only proxy 1 to be evicted, got %d proxies", len(r.proxies))
	}
}

func TestRotator_LeastRecentlyUsedRoundRobins(t *testing.T) {
	r := newTestRotator(t, ids(1, 2, 3)...)
	for i, want := range []int{1, 2, 3, 1, 2, 3} {
		p, err := r.pick(context.Background(), nil)
		if err != nil || p.ID != want {
			t.Fatalf("pick %d: got %v %v, want proxy %d", i, p, err, want)
		}
	}
}

func TestRotator_RandomStaysInSet(t *testing.T) {
	r := newTestRotator(t, ids(1, 2, 3)...)
	ctx := requestCtx("", Options{Random: true})
	seen := map[int]bool{}
	for i := 0; i < 60; i++ {
		p, err := r.pick(ctx, nil)
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		seen[p.ID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("random never varied across 60 draws, saw %v", seen)
	}
}

func TestRotator_EmptyReturnsError(t *testing.T) {
	r := newTestRotator(t)
	if _, err := r.pick(context.Background(), nil); !errors.Is(err, ErrNoProxyAvailable) {
		t.Fatalf("expected capacity error from an empty list, got %v", err)
	}
}

func TestRotator_SessionRetrySkipsTriedProxy(t *testing.T) {
	r := newTestRotator(t, ids(1, 2)...)
	first, err := r.pick(ctxWithToken("a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err := r.pick(ctxWithToken("a"), map[int]bool{first.ID: true})
	if err != nil || first.ID == next.ID {
		t.Fatalf("retry did not advance: %v %v", next, err)
	}
	if bindings := r.sessions.List(); len(bindings) != 1 || bindings[0].ProxyID != next.ID {
		t.Fatalf("wrong binding: %v", bindings)
	}
}
