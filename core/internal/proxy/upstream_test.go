package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/alpkeskin/rota/core/internal/tlsprofile"
)

// originServer is a TLS server standing in for a target site. It records the
// ClientHello it was offered, which is the only way to check from the outside
// that a profile reached the wire rather than stopping at the config struct.
type originServer struct {
	listener net.Listener
	roots    *x509.CertPool

	mu    sync.Mutex
	hello *tls.ClientHelloInfo
}

// newOriginServer starts a TLS origin. handler may be nil, in which case the
// connection is accepted and dropped after the handshake — enough to inspect a
// ClientHello without speaking any HTTP.
func newOriginServer(t *testing.T, alpn []string, handler http.Handler) *originServer {
	t.Helper()

	caFile, keyFile := writeTestCA(t, true)
	ca, err := LoadCertAuthority(caFile, keyFile)
	if err != nil {
		t.Fatalf("LoadCertAuthority: %v", err)
	}
	leaf, err := ca.CertFor("origin.test")
	if err != nil {
		t.Fatalf("CertFor: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)

	srv := &originServer{roots: roots}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   alpn,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			srv.mu.Lock()
			srv.hello = hello
			srv.mu.Unlock()
			return nil, nil
		},
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv.listener = ln
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck

	go func() {
		h2 := &http2.Server{}
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck
				tc, ok := c.(*tls.Conn)
				if !ok || tc.Handshake() != nil {
					return
				}
				if handler == nil {
					// Hold the connection open briefly so the client side can
					// finish inspecting the session before it is torn down.
					time.Sleep(50 * time.Millisecond)
					return
				}
				if tc.ConnectionState().NegotiatedProtocol == "h2" {
					h2.ServeConn(tc, &http2.ServeConnOpts{Handler: handler})
					return
				}
				serveHTTP1(c, handler)
			}(conn)
		}
	}()

	return srv
}

// serveHTTP1 answers requests on a raw connection without net/http's server,
// so a test can see exactly what arrived.
func serveHTTP1(conn net.Conn, handler http.Handler) {
	reader := bufio.NewReader(conn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		rec := &responseRecorder{header: http.Header{}, body: &bytes.Buffer{}}
		handler.ServeHTTP(rec, req)
		resp := &http.Response{
			StatusCode:    rec.status,
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        rec.header,
			Body:          io.NopCloser(rec.body),
			ContentLength: int64(rec.body.Len()),
		}
		if resp.StatusCode == 0 {
			resp.StatusCode = http.StatusOK
		}
		if resp.Write(conn) != nil {
			return
		}
	}
}

type responseRecorder struct {
	header http.Header
	body   *bytes.Buffer
	status int
}

func (r *responseRecorder) Header() http.Header  { return r.header }
func (r *responseRecorder) WriteHeader(code int) { r.status = code }
func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

func (s *originServer) addr() string { return s.listener.Addr().String() }

func (s *originServer) clientHello(t *testing.T) *tls.ClientHelloInfo {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hello == nil {
		t.Fatal("origin recorded no ClientHello")
	}
	return s.hello
}

// dialOrigin runs dialUpstream against the origin with the named profile.
func dialOrigin(t *testing.T, srv *originServer, profileName string) upstreamSession {
	t.Helper()

	profile, err := tlsprofile.Lookup(profileName)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", profileName, err)
	}
	raw, err := net.Dial("tcp", srv.addr())
	if err != nil {
		t.Fatalf("dial origin: %v", err)
	}
	t.Cleanup(func() { raw.Close() }) //nolint:errcheck

	session, err := dialUpstream(raw, "origin.test", profile, srv.roots, 10*time.Second)
	if err != nil {
		t.Fatalf("dialUpstream(%q): %v", profileName, err)
	}
	t.Cleanup(func() { session.Close() }) //nolint:errcheck
	return session
}

// TestDialUpstreamPresentsProfileClientHello is the test the whole feature
// rests on: it proves the profile changes what the target actually sees, not
// just what Rota configured. Comparing against the pass-through profile also
// pins the thing that made profiles necessary — that "go" is distinguishable.
func TestDialUpstreamPresentsProfileClientHello(t *testing.T) {
	goSrv := newOriginServer(t, []string{"h2", "http/1.1"}, nil)
	dialOrigin(t, goSrv, "go")
	goHello := goSrv.clientHello(t)

	iosSrv := newOriginServer(t, []string{"h2", "http/1.1"}, nil)
	dialOrigin(t, iosSrv, "ios")
	iosHello := iosSrv.clientHello(t)

	t.Run("ALPN comes from the profile", func(t *testing.T) {
		if got := goHello.SupportedProtos; len(got) != 1 || got[0] != "http/1.1" {
			t.Errorf("pass-through ALPN = %v, want [http/1.1]", got)
		}
		want := []string{"h2", "http/1.1"}
		if len(iosHello.SupportedProtos) != 2 ||
			iosHello.SupportedProtos[0] != want[0] || iosHello.SupportedProtos[1] != want[1] {
			t.Errorf("iOS ALPN = %v, want %v", iosHello.SupportedProtos, want)
		}
	})

	t.Run("cipher list differs from Go's", func(t *testing.T) {
		if len(iosHello.CipherSuites) == len(goHello.CipherSuites) {
			t.Errorf("iOS offered %d ciphers, same count as Go — profile may not have applied",
				len(iosHello.CipherSuites))
		}
		// 20 real suites plus the GREASE value the spec leads with.
		if got := len(iosHello.CipherSuites); got != 21 {
			t.Errorf("iOS cipher count = %d, want 21", got)
		}
	})

	t.Run("iOS leads with AES_128, which macOS Safari does not", func(t *testing.T) {
		// [0] is GREASE; the first real suite is what distinguishes the
		// platforms. See tlsprofile/specs.go.
		if len(iosHello.CipherSuites) < 2 {
			t.Fatalf("cipher list too short: %v", iosHello.CipherSuites)
		}
		if got := iosHello.CipherSuites[1]; got != tls.TLS_AES_128_GCM_SHA256 {
			t.Errorf("first real cipher = %#x, want TLS_AES_128_GCM_SHA256 (%#x)",
				got, tls.TLS_AES_128_GCM_SHA256)
		}
	})
}

// TestDialUpstreamNegotiatesH2 covers the path that makes an impersonated
// fingerprint coherent. A profile advertising h2 that then fell back to
// HTTP/1.1 would contradict its own ClientHello.
func TestDialUpstreamNegotiatesH2(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Proto", r.Proto)
		w.Write([]byte("hello from origin")) //nolint:errcheck
	})
	srv := newOriginServer(t, []string{"h2", "http/1.1"}, handler)

	session := dialOrigin(t, srv, "ios")
	if got := session.Protocol(); got != "h2" {
		t.Fatalf("negotiated protocol = %q, want h2", got)
	}
	if _, _, ok := session.Upgradable(); ok {
		t.Error("h2 session reports it can hand off a raw connection")
	}

	req, err := http.NewRequest(http.MethodGet, "https://origin.test/path?q=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "test-agent")

	resp, err := session.RoundTrip(req, "origin.test", 10*time.Second)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Proto"); got != "HTTP/2.0" {
		t.Errorf("origin saw %q, want HTTP/2.0", got)
	}
	// The response is relabelled for the HTTP/1.1 client leg.
	if resp.ProtoMajor != 1 {
		t.Errorf("response Proto = %s, want HTTP/1.1 for the client leg", resp.Proto)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello from origin" {
		t.Errorf("body = %q", body)
	}
}

// TestPassthroughStaysOnHTTP1 pins the pre-profile behaviour: a user who never
// picked a profile must keep the exact tunnel they had before.
func TestPassthroughStaysOnHTTP1(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok")) //nolint:errcheck
	})
	srv := newOriginServer(t, []string{"h2", "http/1.1"}, handler)

	session := dialOrigin(t, srv, "go")
	if got := session.Protocol(); got != "http/1.1" {
		t.Errorf("pass-through negotiated %q, want http/1.1", got)
	}
	if _, _, ok := session.Upgradable(); !ok {
		t.Error("http/1.1 session should be able to hand off for a 101")
	}
}

func TestWriteOrderedRequest(t *testing.T) {
	ios, err := tlsprofile.Lookup("ios")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("headers follow the profile order", func(t *testing.T) {
		req := httpRequest(t, http.MethodGet, "/search?q=go", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("User-Agent", "test-agent")
		req.Header.Set("Accept", "*/*")

		var buf bytes.Buffer
		if err := writeOrderedRequest(&buf, req, ios, "origin.test"); err != nil {
			t.Fatalf("writeOrderedRequest: %v", err)
		}

		got := headerNames(buf.String())
		want := []string{"Host", "User-Agent", "Accept", "Accept-Encoding"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("header order = %v, want %v", got, want)
		}
	})

	t.Run("pass-through does not reorder", func(t *testing.T) {
		req := httpRequest(t, http.MethodGet, "/", nil)
		req.Header.Set("Zeta", "1")
		req.Header.Set("Alpha", "2")

		var buf bytes.Buffer
		if err := writeOrderedRequest(&buf, req, tlsprofile.Passthrough, "origin.test"); err != nil {
			t.Fatalf("writeOrderedRequest: %v", err)
		}
		// Every header still has to be present even when nothing reorders it.
		got := headerNames(buf.String())
		if len(got) != 3 {
			t.Errorf("got %v, want Host plus both custom headers", got)
		}
	})

	t.Run("hop-by-hop headers are dropped", func(t *testing.T) {
		req := httpRequest(t, http.MethodGet, "/", nil)
		req.Header.Set("Proxy-Authorization", "Basic secret")
		req.Header.Set("User-Agent", "test-agent")

		var buf bytes.Buffer
		if err := writeOrderedRequest(&buf, req, ios, "origin.test"); err != nil {
			t.Fatalf("writeOrderedRequest: %v", err)
		}
		if strings.Contains(buf.String(), "secret") {
			t.Errorf("Proxy-Authorization leaked upstream:\n%s", buf.String())
		}
	})

	t.Run("counted body keeps its length", func(t *testing.T) {
		req := httpRequest(t, http.MethodPost, "/submit", strings.NewReader("payload"))
		req.ContentLength = 7

		var buf bytes.Buffer
		if err := writeOrderedRequest(&buf, req, ios, "origin.test"); err != nil {
			t.Fatalf("writeOrderedRequest: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "Content-Length: 7\r\n") {
			t.Errorf("missing Content-Length:\n%s", out)
		}
		if !strings.HasSuffix(out, "payload") {
			t.Errorf("body not written:\n%s", out)
		}
	})

	// A body of unknown length has to be re-chunked, and httputil's chunked
	// writer deliberately omits the terminating CRLF — getting that wrong
	// desynchronizes every subsequent request on a keep-alive connection.
	t.Run("unknown-length body is re-chunked and terminated", func(t *testing.T) {
		req := httpRequest(t, http.MethodPost, "/stream", strings.NewReader("abc"))
		req.ContentLength = -1

		var buf bytes.Buffer
		if err := writeOrderedRequest(&buf, req, ios, "origin.test"); err != nil {
			t.Fatalf("writeOrderedRequest: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "Transfer-Encoding: chunked\r\n") {
			t.Errorf("missing Transfer-Encoding:\n%s", out)
		}
		if strings.Contains(out, "Content-Length") {
			t.Errorf("chunked request must not also carry Content-Length:\n%s", out)
		}
		if !strings.HasSuffix(out, "3\r\nabc\r\n0\r\n\r\n") {
			t.Errorf("bad chunk framing, want ...3\\r\\nabc\\r\\n0\\r\\n\\r\\n, got:\n%q", out)
		}

		// The strongest check available: the bytes must parse back as the same
		// request.
		parsed, err := http.ReadRequest(bufio.NewReader(strings.NewReader(out)))
		if err != nil {
			t.Fatalf("re-parse: %v", err)
		}
		body, err := io.ReadAll(parsed.Body)
		if err != nil {
			t.Fatalf("read re-parsed body: %v", err)
		}
		if string(body) != "abc" {
			t.Errorf("round-tripped body = %q, want abc", body)
		}
	})

	t.Run("request line carries the origin form", func(t *testing.T) {
		req := httpRequest(t, http.MethodGet, "/a/b?c=d", nil)
		var buf bytes.Buffer
		if err := writeOrderedRequest(&buf, req, ios, "origin.test"); err != nil {
			t.Fatalf("writeOrderedRequest: %v", err)
		}
		if !strings.HasPrefix(buf.String(), "GET /a/b?c=d HTTP/1.1\r\n") {
			t.Errorf("bad request line:\n%s", buf.String())
		}
	})
}

// httpRequest builds a request shaped like one http.ReadRequest would produce
// inside a tunnel: origin-form URL, Host lifted out of the header map.
func httpRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, "https://origin.test"+target, body)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.Host = "origin.test"
	return req
}

// headerNames returns the header field names of a written request, in order.
func headerNames(raw string) []string {
	lines := strings.Split(raw, "\r\n")
	var names []string
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			break
		}
		names = append(names, name)
	}
	return names
}
