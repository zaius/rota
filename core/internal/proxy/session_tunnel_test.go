package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// echoConnectProxy accepts any CONNECT and echoes each tunneled line back
// prefixed with its proxy ID, so a client can tell which proxy carries it.
func echoConnectProxy(t *testing.T, id int) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		for {
			line, err := buf.ReadString('\n')
			if err != nil {
				return
			}
			fmt.Fprintf(conn, "%d:%s", id, line)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// Regression: a client holding its CONNECT tunnel open kept reaching the target
// through the invalidated proxy, while only fresh connections rebound.
func TestProxyRouter_SessionInvalidateEndsOpenTunnel(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()
	chain := &PoolChain{
		username: "alice",
		selectors: []*PoolSelector{{
			poolID: 1, method: "session", sessionTTL: time.Minute, sessionMgr: sm,
			proxies: []*models.Proxy{
				{ID: 42, Protocol: "http", Address: echoConnectProxy(t, 42).Listener.Addr().String()},
				{ID: 43, Protocol: "http", Address: echoConnectProxy(t, 43).Listener.Addr().String()},
			},
		}},
		logger:         logger.New("error"),
		targetResolver: ipv4TargetResolver{},
	}
	auth := newTestUserAuthMw()
	cacheTestUser(auth, chain)
	router := &proxyRouter{userAuthMw: auth, upstream: NewUpstreamProxyHandler(nil, nil, logger.New("error"))}
	server := httptest.NewServer(router)
	defer server.Close()

	connect := func() (net.Conn, *bufio.Reader, int) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		req := httptest.NewRequest(http.MethodConnect, "target.example:443", nil)
		basicProxyAuth(req, "alice-session-abc", "secret")
		if err := req.Write(conn); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT failed: %v %v", resp, err)
		}
		id, _ := strconv.Atoi(resp.Header.Get(ProxyIDHeader))
		return conn, br, id
	}
	exchange := func(conn net.Conn, br *bufio.Reader) (string, error) {
		if _, err := io.WriteString(conn, "ping\n"); err != nil {
			return "", err
		}
		return br.ReadString('\n')
	}

	held, heldReader, first := connect()
	if line, err := exchange(held, heldReader); err != nil || line != fmt.Sprintf("%d:ping\n", first) {
		t.Fatalf("tunnel did not reach proxy %d: %q %v", first, line, err)
	}

	sm.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: first, Scope: "target.example", CooldownUntil: time.Now().Add(time.Hour)})

	held.SetDeadline(time.Now().Add(5 * time.Second))
	if line, err := exchange(held, heldReader); err == nil {
		t.Fatalf("held tunnel still reaches the invalidated proxy: %q", line)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("held tunnel stayed open after invalidation")
	}

	fresh, freshReader, second := connect()
	if second == first {
		t.Fatalf("fresh CONNECT reused invalidated proxy %d", first)
	}
	if line, err := exchange(fresh, freshReader); err != nil || line != fmt.Sprintf("%d:ping\n", second) {
		t.Fatalf("fresh tunnel did not reach proxy %d: %q %v", second, line, err)
	}
}
