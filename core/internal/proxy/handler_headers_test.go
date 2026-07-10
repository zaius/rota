package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCopyResponse_StripsHopByHopHeaders(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("hello")),
	}
	resp.Header.Set("Content-Type", "text/plain")
	resp.Header.Set("Connection", "keep-alive")
	resp.Header.Set("Keep-Alive", "timeout=5")
	resp.Header.Set("Transfer-Encoding", "chunked")
	resp.Header.Set("Upgrade", "h2c")
	resp.Header.Set("Proxy-Authenticate", "Basic")

	rec := httptest.NewRecorder()
	copyResponse(rec, resp)

	for _, h := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "Proxy-Authenticate"} {
		if got := rec.Header().Get(h); got != "" {
			t.Errorf("expected %s to be stripped from the response, got %q", h, got)
		}
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("expected end-to-end headers to survive, Content-Type = %q", got)
	}
	if body := rec.Body.String(); body != "hello" {
		t.Errorf("expected the body to be copied, got %q", body)
	}
}

// Headers named by the Connection header are hop-by-hop too, and must not reach
// the client.
func TestCopyResponse_StripsHeadersNamedByConnection(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	resp.Header.Set("Connection", "X-Hop-One, X-Hop-Two")
	resp.Header.Set("X-Hop-One", "a")
	resp.Header.Set("X-Hop-Two", "b")
	resp.Header.Set("X-Keep", "c")

	rec := httptest.NewRecorder()
	copyResponse(rec, resp)

	if got := rec.Header().Get("X-Hop-One"); got != "" {
		t.Errorf("expected X-Hop-One to be stripped, got %q", got)
	}
	if got := rec.Header().Get("X-Hop-Two"); got != "" {
		t.Errorf("expected X-Hop-Two to be stripped, got %q", got)
	}
	if got := rec.Header().Get("X-Keep"); got != "c" {
		t.Errorf("expected X-Keep to survive, got %q", got)
	}
}

func TestRemoveHopByHopHeaders_StripsRequestHeaders(t *testing.T) {
	h := &UpstreamProxyHandler{}
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Proxy-Authorization", "Basic Zm9vOmJhcg==")
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("X-Keep", "yes")

	h.removeHopByHopHeaders(req)

	for _, name := range []string{"Connection", "Proxy-Authorization", "Proxy-Connection", "Upgrade"} {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("expected %s to be stripped from the request, got %q", name, got)
		}
	}
	if got := req.Header.Get("X-Keep"); got != "yes" {
		t.Errorf("expected end-to-end request headers to survive, got %q", got)
	}
}

// Connection must be read for its token list before it is itself deleted;
// otherwise the headers it names are silently forwarded.
func TestRemoveHopByHopHeaders_StripsHeadersNamedByConnection(t *testing.T) {
	h := &UpstreamProxyHandler{}
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("Connection", "X-Custom-Hop")
	req.Header.Set("X-Custom-Hop", "leaked")

	h.removeHopByHopHeaders(req)

	if got := req.Header.Get("X-Custom-Hop"); got != "" {
		t.Fatalf("expected the header named by Connection to be stripped, got %q", got)
	}
}
