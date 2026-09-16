package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

func TestWriteProxyError_Classification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{"capacity", fmt.Errorf("wrapped: %w", ErrNoProxyAvailable), 593, "no_proxy_available"},
		{"request", io.ErrUnexpectedEOF, 592, "upstream_request_failed"},
		{"timeout", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), 592, "upstream_timeout"},
		{"dial", fmt.Errorf("wrapped: %w", &net.OpError{Op: "dial", Err: errors.New("refused")}), 592, "proxy_connect_failed"},
		{"proxy_timeout", forwardingFailure("proxy_connect_failed", context.DeadlineExceeded), 592, "upstream_timeout"},
		{"handshake", forwardingFailure("proxy_handshake_failed", io.EOF), 592, "proxy_handshake_failed"},
		{"rejected", forwardingFailure("proxy_connect_rejected", errors.New("403")), 592, "proxy_connect_rejected"},
		{"target_dns_timeout", forwardingFailure("proxy_connect_rejected", &net.DNSError{Name: "target.invalid", Err: "timeout", IsTimeout: true}), 592, "proxy_connect_rejected"},
		{"proxy_dns", forwardingFailure("proxy_connect_failed", &net.DNSError{Name: "proxy.invalid", Err: "no such host", IsNotFound: true}), 592, "proxy_connect_failed"},
		{"auth", forwardingFailure("upstream_proxy_auth_failed", errors.New("407")), 592, "upstream_proxy_auth_failed"},
		{"configuration", forwardingFailure("proxy_configuration_error", errors.New("http://user:secret@proxy")), 592, "proxy_configuration_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeProxyError(w, tc.err)
			if w.Code != tc.status || w.Header().Get(ProxyErrorHeader) != tc.reason {
				t.Fatalf("got %d %v", w.Code, w.Header())
			}
			if (w.Header().Get("Retry-After") != "") != (tc.status == 593) {
				t.Fatal("incorrect retry promise")
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("raw proxy credentials exposed")
			}
		})
	}
}

// HTTP errors returned upstream remain answers. CONNECT rejection and upstream
// proxy authentication are failures, regardless of the upstream status number.
func TestProxyHandler_UpstreamStatuses(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodConnect} {
		for _, status := range []int{403, 407, 429, 500, 502, 503, 504, 592, 593, 594} {
			t.Run(fmt.Sprintf("%s/%d", method, status), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set(ProxyErrorHeader, "spoofed")
					w.Header().Set("Retry-After", "21")
					w.WriteHeader(status)
					fmt.Fprint(w, "upstream answer")
				}))
				defer upstream.Close()
				p := &models.Proxy{ID: 42, Protocol: "http", Address: upstream.Listener.Addr().String()}
				t.Cleanup(func() { InvalidateTransport(p) })
				chain := &PoolChain{maxRetry: 1, selectors: []*PoolSelector{newMethodSelector("roundrobin", p)}, targetResolver: ipv4TargetResolver{}}
				target := "http://example.com/path"
				if method == http.MethodConnect {
					target = "example.com:443"
				}
				req := httptest.NewRequest(method, target, nil)
				req = req.WithContext(context.WithValue(req.Context(), UserChainContextKey, chain))
				h := NewUpstreamProxyHandler(nil, nil, logger.New("error"))
				w := httptest.NewRecorder()
				if method == http.MethodConnect {
					h.HandleConnectRequest(w, req)
				} else {
					h.HandleHTTPRequest(w, req)
				}
				switch {
				case status == 407 && method != http.MethodConnect:
					if w.Code != 592 || w.Header().Get(ProxyErrorHeader) != "upstream_proxy_auth_failed" {
						t.Fatalf("upstream auth misreported: %d %v", w.Code, w.Header())
					}
				case method == http.MethodConnect:
					if w.Code != 592 || w.Header().Get(ProxyErrorHeader) != "proxy_connect_rejected" {
						t.Fatalf("CONNECT rejection misreported: %d %v", w.Code, w.Header())
					}
				default:
					if w.Code != status || w.Header().Get(ProxyErrorHeader) != "" || w.Body.String() != "upstream answer" || w.Header().Get("Retry-After") != "21" {
						t.Fatalf("upstream answer changed: %d %v %s", w.Code, w.Header(), w.Body.String())
					}
				}
			})
		}
	}
}

func TestWriteGatewayError_InsideInspectedTunnel(t *testing.T) {
	for _, cause := range []error{io.ErrUnexpectedEOF, context.DeadlineExceeded} {
		var wire bytes.Buffer
		req := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
		writeGatewayError(&wire, req, cause)
		resp, err := http.ReadResponse(bufio.NewReader(&wire), req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 592 || resp.Header.Get(ProxyErrorHeader) != forwardingReason(cause) || !resp.Close {
			t.Fatalf("incorrect tunneled error: %+v", resp)
		}
	}
}

func TestProxyHandler_MissingChainIsInternalError(t *testing.T) {
	h := NewUpstreamProxyHandler(nil, nil, logger.New("error"))
	for _, method := range []string{http.MethodGet, http.MethodConnect} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "example.com:443", nil)
		if method == http.MethodConnect {
			h.HandleConnectRequest(w, req)
		} else {
			h.HandleHTTPRequest(w, req)
		}
		if w.Code != 500 || w.Header().Get(ProxyErrorHeader) != "rota_internal_error" {
			t.Fatalf("got %d %v", w.Code, w.Header())
		}
	}
}
