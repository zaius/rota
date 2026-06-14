package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// fakeSequenceSelector returns proxies from a fixed list in round-robin order,
// mimicking the non-pool legacy selectors (RandomSelector/RoundRobinSelector)
// which are not domain-cooldown aware.
type fakeSequenceSelector struct {
	proxies []*models.Proxy
	idx     int
}

func (f *fakeSequenceSelector) Select(ctx context.Context) (*models.Proxy, error) {
	if len(f.proxies) == 0 {
		return nil, fmt.Errorf("no proxies available")
	}
	p := f.proxies[f.idx%len(f.proxies)]
	f.idx++
	return p, nil
}

func (f *fakeSequenceSelector) Refresh(ctx context.Context) error { return nil }

// mockConnectProxy starts a throwaway HTTP CONNECT proxy that accepts every
// tunnel and echoes bytes back. Returns its address.
func mockConnectProxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimSpace(line) == "" {
						break
					}
				}
				conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
				io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String()
}

// Bug 1: skipping a domain-cooled proxy must NOT consume a fallback attempt.
// With two proxies cooled for the target domain and FallbackMaxRetries=2, the
// single eligible proxy must still be tried — the old code burned both fallback
// attempts on the cooled skips and failed without ever reaching the eligible
// proxy.
func TestConnectThroughProxy_DomainCooledSkipDoesNotBurnFallback(t *testing.T) {
	eligibleAddr := mockConnectProxy(t)

	cooled1 := &models.Proxy{ID: 1, Address: "127.0.0.1:9", Protocol: "http"}
	cooled2 := &models.Proxy{ID: 2, Address: "127.0.0.1:9", Protocol: "http"}
	eligible := &models.Proxy{ID: 3, Address: eligibleAddr, Protocol: "http"}

	cd := NewDomainCooldownManager()
	defer cd.Stop()
	until := time.Now().Add(time.Hour)
	cd.Set(1, "example.com", until, "429")
	cd.Set(2, "example.com", until, "429")

	sel := &fakeSequenceSelector{proxies: []*models.Proxy{cooled1, cooled2, eligible}}
	settings := &models.RotationSettings{Fallback: true, FallbackMaxRetries: 2, Timeout: 5, Retries: 1}
	h := NewUpstreamProxyHandler(sel, nil, settings, cd, logger.New("error"))

	conn, proxyID, err := h.connectThroughProxy("example.com:443", context.Background())
	if err != nil {
		t.Fatalf("eligible proxy (id 3) must be tried even though 2 cooled proxies precede it: %v", err)
	}
	defer conn.Close()
	if proxyID != 3 {
		t.Fatalf("expected eligible proxy 3 to serve the request, got %d", proxyID)
	}
}

// Same guarantee for the plain-HTTP forward path (sendWithRetry), which reads
// the target host from the request context.
func TestSendWithRetry_DomainCooledSkipDoesNotBurnFallback(t *testing.T) {
	// A mock origin server reached through the eligible HTTP proxy. For a
	// forward proxy we can point the proxy address at a plain HTTP server that
	// answers any absolute-form request with 200.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimSpace(line) == "" {
						break
					}
				}
				conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
			}()
		}
	}()

	cooled1 := &models.Proxy{ID: 1, Address: "127.0.0.1:9", Protocol: "http"}
	cooled2 := &models.Proxy{ID: 2, Address: "127.0.0.1:9", Protocol: "http"}
	eligible := &models.Proxy{ID: 3, Address: ln.Addr().String(), Protocol: "http"}

	cd := NewDomainCooldownManager()
	defer cd.Stop()
	until := time.Now().Add(time.Hour)
	cd.Set(1, "example.com", until, "429")
	cd.Set(2, "example.com", until, "429")

	sel := &fakeSequenceSelector{proxies: []*models.Proxy{cooled1, cooled2, eligible}}
	settings := &models.RotationSettings{Fallback: true, FallbackMaxRetries: 2, Timeout: 5, Retries: 1}
	h := NewUpstreamProxyHandler(sel, NewUsageTracker(nil), settings, cd, logger.New("error"))

	req, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), TargetHostContextKey, "example.com")
	resp, proxyID, err := h.sendWithRetry(req, ctx)
	if err != nil {
		t.Fatalf("eligible proxy (id 3) must be tried even though 2 cooled proxies precede it: %v", err)
	}
	resp.Body.Close()
	if proxyID != 3 {
		t.Fatalf("expected eligible proxy 3 to serve the request, got %d", proxyID)
	}
}
