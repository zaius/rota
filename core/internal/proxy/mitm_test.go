package proxy

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zaius/rota/core/internal/tlsprofile"
	"github.com/zaius/rota/core/pkg/logger"
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

	// Stands in for the tunnel the rotator would have opened through an
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

// testCertAuthority writes a fresh self-signed CA to disk and loads it the way
// the server does.
func testCertAuthority(t *testing.T) *CertAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Rota Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	ca, err := LoadCertAuthority(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}
