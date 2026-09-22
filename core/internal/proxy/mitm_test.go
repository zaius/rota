package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/tlsprofile"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// inspectedTunnel wires an origin server to a TLSInspector over real sockets
// and returns the client end of the tunnel plus a channel carrying Serve's
// result once the tunnel closes.
type inspectedTunnel struct {
	clientConn net.Conn
	host       string
	ca         *CertAuthority
	done       chan serveResult
}

type serveResult struct {
	counts   TunnelCounts
	requests int
	err      error
}

// startInspectedTunnel stands up the full interception path against origin:
// a TCP connection standing in for the CONNECT tunnel, and the inspector
// terminating TLS on both sides of it.
func startInspectedTunnel(t *testing.T, origin *httptest.Server) *inspectedTunnel {
	t.Helper()
	return startInspectedTunnelReadingAhead(t, origin, 0, tlsprofile.Passthrough)
}

// startInspectedTunnelWithProfile does the same with an impersonating profile
// on the upstream leg, which is what lets the origin negotiate h2.
func startInspectedTunnelWithProfile(t *testing.T, origin *httptest.Server, profile *tlsprofile.Profile) *inspectedTunnel {
	t.Helper()
	return startInspectedTunnelReadingAhead(t, origin, 0, profile)
}

// startInspectedTunnelWithPrefix does the same, but first consumes the head of
// the client's TLS handshake and hands it back through a prefixConn — what the
// handler does for a client that pipelines its ClientHello behind the CONNECT.
func startInspectedTunnelWithPrefix(t *testing.T, origin *httptest.Server) *inspectedTunnel {
	t.Helper()
	return startInspectedTunnelReadingAhead(t, origin, 8, tlsprofile.Passthrough)
}

func startInspectedTunnelReadingAhead(t *testing.T, origin *httptest.Server, readAhead int, profile *tlsprofile.Profile) *inspectedTunnel {
	t.Helper()

	ca := testCertAuthority(t)
	inspector := NewTLSInspector(ca, nil, logger.New("error"))

	// The origin is a self-signed httptest server; production verifies against
	// the system pool, so point verification at the origin's own certificate.
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	inspector.upstreamRoots = roots

	clientConn, proxyClientSide := tcpPipe(t)

	// Stands in for the tunnel the pool chain would have opened through an
	// upstream proxy: a raw TCP connection to the target, TLS not yet started.
	upstreamConn, err := net.Dial("tcp", origin.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial origin: %v", err)
	}
	t.Cleanup(func() { upstreamConn.Close() })

	host := origin.Listener.Addr().String()
	done := make(chan serveResult, 1)
	go func() {
		stream := net.Conn(proxyClientSide)
		if readAhead > 0 {
			head := make([]byte, readAhead)
			if _, err := io.ReadFull(proxyClientSide, head); err != nil {
				done <- serveResult{err: err}
				return
			}
			stream = &prefixConn{Conn: proxyClientSide, prefix: head}
		}
		counts, requests, err := inspector.Serve(stream, upstreamConn, host, nil, profile, 10*time.Second)
		proxyClientSide.Close()
		done <- serveResult{counts: counts, requests: requests, err: err}
	}()

	return &inspectedTunnel{clientConn: clientConn, host: host, ca: ca, done: done}
}

// dialTLS completes the client side of the intercepted handshake, trusting the
// inspector's CA exactly as a configured client would.
func (t2 *inspectedTunnel) dialTLS(t *testing.T) *tls.Conn {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(t2.ca.cert)

	host, _, err := net.SplitHostPort(t2.host)
	if err != nil {
		t.Fatalf("split host: %v", err)
	}

	conn := tls.Client(t2.clientConn, &tls.Config{
		ServerName: host,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	})
	if err := conn.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	return conn
}

func TestTLSInspector_CountsEveryRequestOnOneTunnel(t *testing.T) {
	var originHits atomic.Int64
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		// Echo the path so we can prove responses are matched to requests.
		fmt.Fprintf(w, "hit %s", r.URL.Path)
	}))
	defer origin.Close()

	tunnel := startInspectedTunnel(t, origin)
	clientTLS := tunnel.dialTLS(t)

	// The whole point: many requests over ONE tunnel. Without interception
	// this is a single CONNECT event no matter how many requests ride it.
	const requestCount = 25
	reader := bufio.NewReader(clientTLS)
	for i := 0; i < requestCount; i++ {
		path := fmt.Sprintf("/listing/%d", i)
		req, err := http.NewRequest(http.MethodGet, "https://"+tunnel.host+path, nil)
		if err != nil {
			t.Fatalf("build request %d: %v", i, err)
		}
		if err := req.Write(clientTLS); err != nil {
			t.Fatalf("write request %d: %v", i, err)
		}

		resp, err := http.ReadResponse(reader, req)
		if err != nil {
			t.Fatalf("read response %d: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read body %d: %v", i, err)
		}
		if got, want := string(body), "hit "+path; got != want {
			t.Fatalf("response %d: got %q, want %q", i, got, want)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("response %d: status %d", i, resp.StatusCode)
		}
	}

	clientTLS.Close()

	select {
	case res := <-tunnel.done:
		if res.err != nil {
			t.Fatalf("Serve: %v", res.err)
		}
		if res.requests != requestCount {
			t.Errorf("recorded %d requests, want %d", res.requests, requestCount)
		}
		// Wire bytes must be counted under TLS, so they cover handshake and
		// record framing, not just the plaintext payload.
		if res.counts.BytesUp <= 0 || res.counts.BytesDown <= 0 {
			t.Errorf("byte counts not recorded: up=%d down=%d",
				res.counts.BytesUp, res.counts.BytesDown)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the client closed")
	}

	if got := originHits.Load(); got != requestCount {
		t.Errorf("origin saw %d requests, want %d", got, requestCount)
	}
}

func TestTLSInspector_ForwardsRequestBodiesAndStatusCodes(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// A blocked scrape looks exactly like this, and the status code is the
		// signal an opaque tunnel throws away.
		if strings.Contains(string(body), "bot") {
			w.Header().Set(ProxyErrorHeader, "spoofed")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "rate limited")
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "received %d bytes", len(body))
	}))
	defer origin.Close()

	tunnel := startInspectedTunnel(t, origin)
	clientTLS := tunnel.dialTLS(t)
	reader := bufio.NewReader(clientTLS)

	send := func(payload string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "https://"+tunnel.host+"/search",
			strings.NewReader(payload))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if err := req.Write(clientTLS); err != nil {
			t.Fatalf("write request: %v", err)
		}
		resp, err := http.ReadResponse(reader, req)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		return resp
	}

	resp := send("normal query")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if got, want := string(body), "received 12 bytes"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	resp = send("i am a bot")
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	if resp.Header.Get(ProxyErrorHeader) != "" {
		t.Error("target spoofed a Rota error inside the tunnel")
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}

	clientTLS.Close()
	select {
	case res := <-tunnel.done:
		if res.requests != 2 {
			t.Errorf("recorded %d requests, want 2", res.requests)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// TestTLSInspector_ChunksUnknownLengthH2Responses pins how a response without
// a content-length crosses from the h2 upstream leg to the HTTP/1.1 client
// leg. HTTP/2 needs no length, END_STREAM ends the body, so most dynamic
// pages arrive this way. HTTP/1.1 has to either chunk such a body or close
// the connection after it, and the tunnel keeps its connection open for the
// next request, so it must chunk. A client told Connection: close instead
// reads the whole body and then waits forever for a close that never comes.
func TestTLSInspector_ChunksUnknownLengthH2Responses(t *testing.T) {
	payload := strings.Repeat("dynamic page\n", 4096)
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Proto", r.Proto)
		// Flushing the headers before the body is what leaves the length
		// unknown; net/http would otherwise buffer and count the body.
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		io.WriteString(w, payload) //nolint:errcheck
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()

	profile, err := tlsprofile.Lookup("chrome")
	if err != nil {
		t.Fatal(err)
	}
	tunnel := startInspectedTunnelWithProfile(t, origin, profile)
	clientTLS := tunnel.dialTLS(t)
	// The regression is a body that never ends; a deadline turns that into a
	// failure instead of a stuck test.
	if err := clientTLS.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(clientTLS)

	// All three ride one connection. HEAD is the case where an unknown length
	// comes with no body at all, and the GET after it proves the connection
	// is still in sync.
	for i, method := range []string{http.MethodGet, http.MethodHead, http.MethodGet} {
		req, err := http.NewRequest(method, fmt.Sprintf("https://%s/page/%d", tunnel.host, i), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := req.Write(clientTLS); err != nil {
			t.Fatalf("write request %d: %v", i, err)
		}
		resp, err := http.ReadResponse(reader, req)
		if err != nil {
			t.Fatalf("read response %d: %v", i, err)
		}
		if got := resp.Header.Get("X-Proto"); got != "HTTP/2.0" {
			t.Fatalf("origin spoke %q, want HTTP/2.0: the test is not exercising the h2 path", got)
		}
		if resp.Close {
			t.Errorf("response %d (%s) announced Connection: close on a connection the tunnel keeps open", i, method)
		}
		if len(resp.TransferEncoding) == 0 || resp.TransferEncoding[0] != "chunked" {
			t.Errorf("response %d (%s) transfer encoding = %v, want chunked", i, method, resp.TransferEncoding)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read body %d (%s): %v", i, method, err)
		}
		want := payload
		if method == http.MethodHead {
			want = ""
		}
		if string(body) != want {
			t.Fatalf("response %d (%s): body length %d, want %d", i, method, len(body), len(want))
		}
	}

	clientTLS.Close()
	select {
	case res := <-tunnel.done:
		if res.err != nil {
			t.Fatalf("Serve: %v", res.err)
		}
		if res.requests != 3 {
			t.Errorf("recorded %d requests, want 3", res.requests)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the client closed")
	}
}

// TestWriteResponse_FramesForKeepAlive checks the framing writeResponse puts
// on the wire for each way an upstream response can arrive. The invariant:
// Connection: close appears only when resp.Close asked for it, because that is
// the only case in which the pump closes.
func TestWriteResponse_FramesForKeepAlive(t *testing.T) {
	cases := []struct {
		name          string
		method        string
		contentLength int64
		close         bool
		wantChunked   bool
		wantClose     bool
	}{
		{name: "unknown length", method: http.MethodGet, contentLength: -1, wantChunked: true},
		{name: "known length", method: http.MethodGet, contentLength: 4},
		{name: "unknown length, server closes", method: http.MethodGet, contentLength: -1, close: true, wantClose: true},
		{name: "HEAD without length", method: http.MethodHead, contentLength: -1, wantChunked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httpRequest(t, tc.method, "/", nil)
			body := io.NopCloser(strings.NewReader("body"))
			if tc.method == http.MethodHead {
				body = http.NoBody
			}
			resp := &http.Response{
				StatusCode:    http.StatusOK,
				ProtoMajor:    1,
				ProtoMinor:    1,
				Header:        http.Header{"X-Test": {"1"}},
				Body:          body,
				ContentLength: tc.contentLength,
				Close:         tc.close,
				Request:       req,
			}

			var wire bytes.Buffer
			if err := writeResponse(&wire, resp); err != nil {
				t.Fatalf("writeResponse: %v", err)
			}
			if resp.Close != tc.close {
				t.Errorf("writeResponse changed resp.Close to %v", resp.Close)
			}

			reader := bufio.NewReader(bytes.NewReader(wire.Bytes()))
			got, err := http.ReadResponse(reader, req)
			if err != nil {
				t.Fatalf("parse written response: %v\n%s", err, wire.String())
			}
			if got.Close != tc.wantClose {
				t.Errorf("Connection: close = %v, want %v\n%s", got.Close, tc.wantClose, wire.String())
			}
			chunked := len(got.TransferEncoding) > 0 && got.TransferEncoding[0] == "chunked"
			if chunked != tc.wantChunked {
				t.Errorf("chunked = %v, want %v\n%s", chunked, tc.wantChunked, wire.String())
			}
			parsed, err := io.ReadAll(got.Body)
			if err != nil {
				t.Fatalf("read parsed body: %v", err)
			}
			want := "body"
			if tc.method == http.MethodHead {
				want = ""
			}
			if string(parsed) != want {
				t.Errorf("body = %q, want %q", parsed, want)
			}
			// Anything left over would be read as the start of the next
			// response and desynchronize the connection.
			if rest, _ := io.ReadAll(reader); len(rest) != 0 {
				t.Errorf("%d stray bytes after the response: %q", len(rest), rest)
			}
		})
	}
}

func TestPrefixConn_ReplaysReadAheadBytes(t *testing.T) {
	// The HTTP server can read past a CONNECT while filling its buffer. Those
	// bytes are the start of the client's TLS handshake, so they must come back
	// out of the stream first and in order, or interception sees a truncated
	// ClientHello.
	client, server := tcpPipe(t)
	defer client.Close()
	defer server.Close()

	go func() {
		client.Write([]byte("-tail")) //nolint:errcheck
	}()

	conn := &prefixConn{Conn: server, prefix: []byte("head-")}
	got := make([]byte, 0, 10)
	buf := make([]byte, 4)
	for len(got) < 10 {
		n, err := conn.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if string(got) != "head--tail" {
		t.Errorf("got %q, want %q", got, "head--tail")
	}
}

func TestTLSInspector_InterceptsAfterReadAhead(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer origin.Close()

	tunnel := startInspectedTunnelWithPrefix(t, origin)
	clientTLS := tunnel.dialTLS(t)
	reader := bufio.NewReader(clientTLS)

	req, err := http.NewRequest(http.MethodGet, "https://"+tunnel.host+"/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := req.Write(clientTLS); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	clientTLS.Close()
	select {
	case res := <-tunnel.done:
		// A client that pipelines its ClientHello behind the CONNECT must still
		// be intercepted, not silently downgraded to an opaque tunnel.
		if res.requests != 1 {
			t.Errorf("recorded %d requests, want 1", res.requests)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestTLSInspector_ShouldInspect(t *testing.T) {
	ca := testCertAuthority(t)
	inspector := NewTLSInspector(ca, []string{"pinned.example.com", "https://*.bank.test/"}, logger.New("error"))

	cases := []struct {
		host string
		want bool
	}{
		{"www.airbnb.com:443", true},
		{"pinned.example.com:443", false},
		{"api.pinned.example.com:443", false}, // subdomains inherit the bypass
		{"notpinned.example.com:443", true},   // suffix match must not be substring match
		{"bank.test:443", false},              // bypass entries are normalized
		{"login.bank.test:443", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := inspector.ShouldInspect(tc.host); got != tc.want {
			t.Errorf("ShouldInspect(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestTLSInspector_DisabledWithoutCA(t *testing.T) {
	// A nil inspector is the no-CA case, and it must never claim a tunnel:
	// interception has to be impossible unless it was explicitly configured.
	var inspector *TLSInspector
	if inspector.ShouldInspect("www.airbnb.com:443") {
		t.Error("nil inspector claimed a tunnel")
	}

	if (&TLSInspector{}).ShouldInspect("www.airbnb.com:443") {
		t.Error("inspector without a CA claimed a tunnel")
	}
}
