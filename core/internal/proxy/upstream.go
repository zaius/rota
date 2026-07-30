package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	utls "github.com/bogdanfinn/utls"

	"github.com/alpkeskin/rota/core/internal/tlsprofile"
)

// upstreamSession is one TLS session to the target, carrying requests in
// whichever protocol the handshake negotiated.
//
// The client side of an intercepted tunnel always speaks HTTP/1.1 to Rota — it
// is our own user, and terminating it is what makes per-request accounting
// possible. The target side is where the fingerprint lands, so it speaks
// whatever the selected profile's ClientHello advertised. Splitting the two
// legs is what lets Rota offer h2 upstream without implementing an h2 server
// for the client.
type upstreamSession interface {
	// RoundTrip sends one request and returns the response. The response body
	// belongs to the caller.
	RoundTrip(req *http.Request, host string, timeout time.Duration) (*http.Response, error)

	// Protocol names the negotiated protocol, for logging.
	Protocol() string

	// Upgradable returns the raw connection and read buffer when the protocol
	// can hand the connection to another one (a 101 response). HTTP/2 has no
	// such transition, so it returns false.
	Upgradable() (net.Conn, *bufio.Reader, bool)

	Close() error
}

// dialUpstream completes a TLS handshake to the target over an already
// established connection and returns a session speaking the negotiated
// protocol.
//
// The profile decides both halves of what the target sees: which ClientHello
// goes out, and — through its ALPN list — which protocol can come back.
func dialUpstream(
	conn net.Conn,
	serverName string,
	profile *tlsprofile.Profile,
	roots *x509.CertPool,
	timeout time.Duration,
) (upstreamSession, error) {
	if !profile.Impersonates() {
		return dialPassthrough(conn, serverName, roots, timeout)
	}

	alpn, err := profile.ALPN()
	if err != nil {
		return nil, err
	}

	// NextProtos is set from the profile's own spec rather than left unset:
	// both uTLS and this code then name the same protocol list, so there is no
	// way for the handshake to advertise something the session cannot speak.
	// The three flags are, in order: permute extensions, force HTTP/1.1, and
	// disable HTTP/3. Extension order is part of the fingerprint being
	// reproduced, so it is never permuted here — a profile whose real client
	// shuffles carries that in its own spec. HTTP/1.1 is not forced because
	// forcing it would strip h2 from the ALPN the profile advertises. HTTP/3
	// is disabled because Rota reaches the target over a TCP proxy hop, which
	// cannot carry QUIC.
	uconn := utls.UClient(conn, &utls.Config{
		ServerName: serverName,
		RootCAs:    roots,
		NextProtos: alpn,
	}, profile.HelloID(), false, false, true)

	if err := handshakeUTLS(uconn, timeout); err != nil {
		return nil, fmt.Errorf("upstream TLS handshake for %s: %w", serverName, err)
	}

	if uconn.ConnectionState().NegotiatedProtocol == "h2" {
		return newH2Session(uconn, profile)
	}
	return newH1Session(uconn, profile, "http/1.1"), nil
}

// dialPassthrough is the pre-profile path: Go's own TLS stack, HTTP/1.1 only.
func dialPassthrough(
	conn net.Conn,
	serverName string,
	roots *x509.CertPool,
	timeout time.Duration,
) (upstreamSession, error) {
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: serverName,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	if err := handshake(tlsConn, timeout); err != nil {
		return nil, fmt.Errorf("upstream TLS handshake for %s: %w", serverName, err)
	}
	return newH1Session(tlsConn, tlsprofile.Passthrough, "http/1.1"), nil
}

// handshakeUTLS mirrors handshake for a uTLS connection, which does not share
// an interface with *tls.Conn.
func handshakeUTLS(conn *utls.UConn, timeout time.Duration) error {
	if timeout > 0 {
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		defer conn.SetDeadline(time.Time{}) //nolint:errcheck
	}
	return conn.Handshake()
}

// h1Session carries requests over HTTP/1.1, one at a time, which is all an
// intercepted tunnel needs — the client feeding it is itself serial.
type h1Session struct {
	conn    net.Conn
	reader  *bufio.Reader
	profile *tlsprofile.Profile
	proto   string
}

func newH1Session(conn net.Conn, profile *tlsprofile.Profile, proto string) *h1Session {
	return &h1Session{
		conn:    conn,
		reader:  bufio.NewReader(conn),
		profile: profile,
		proto:   proto,
	}
}

func (s *h1Session) Protocol() string { return s.proto }
func (s *h1Session) Close() error     { return s.conn.Close() }

func (s *h1Session) Upgradable() (net.Conn, *bufio.Reader, bool) {
	return s.conn, s.reader, true
}

// RoundTrip forwards one request and reads its response, absorbing any
// informational (1xx) responses that precede the real one — leaving those
// unread would desynchronize the next read from the message stream.
func (s *h1Session) RoundTrip(req *http.Request, host string, timeout time.Duration) (*http.Response, error) {
	if timeout > 0 {
		if err := s.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		defer s.conn.SetDeadline(time.Time{}) //nolint:errcheck
	}

	if err := writeOrderedRequest(s.conn, req, s.profile, host); err != nil {
		return nil, fmt.Errorf("forward request: %w", err)
	}
	if req.Body != nil {
		req.Body.Close() //nolint:errcheck
	}

	for {
		resp, err := http.ReadResponse(s.reader, req)
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		// 101 is an upgrade, not an interim response — hand it to the caller.
		if resp.StatusCode < 100 || resp.StatusCode > 199 || resp.StatusCode == http.StatusSwitchingProtocols {
			return resp, nil
		}
		resp.Body.Close() //nolint:errcheck
	}
}

// writeOrderedRequest writes an HTTP/1.1 request with its headers in the
// profile's order.
//
// net/http's Request.Write is not usable here: it emits headers sorted
// alphabetically, and alphabetical order is not something any real client
// produces — it is a fingerprint in its own right, and a recognizable one.
// Framing is taken from what ReadRequest already determined, so a chunked body
// is re-chunked and a counted one keeps its length.
func writeOrderedRequest(w io.Writer, req *http.Request, profile *tlsprofile.Profile, host string) error {
	var buf strings.Builder

	uri := req.URL.RequestURI()
	buf.WriteString(req.Method + " " + uri + " HTTP/1.1\r\n")

	// ReadRequest promotes Host out of the header map, so it has to be put
	// back to be ordered alongside everything else.
	headers := make(http.Header, len(req.Header)+1)
	for k, v := range req.Header {
		if _, hop := hopHeaders[k]; hop {
			continue
		}
		headers[k] = v
	}
	authority := req.Host
	if authority == "" {
		authority = host
	}
	headers.Set("Host", authority)

	// Framing headers are ours to set, not the client's to pass through: the
	// body was decoded on the way in and is re-encoded here.
	headers.Del("Content-Length")
	headers.Del("Transfer-Encoding")
	chunked := req.ContentLength < 0 && req.Body != nil && req.Body != http.NoBody
	switch {
	case chunked:
		headers.Set("Transfer-Encoding", "chunked")
	case req.ContentLength > 0:
		headers.Set("Content-Length", strconv.FormatInt(req.ContentLength, 10))
	}
	if req.Close {
		headers.Set("Connection", "close")
	}

	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	for _, k := range profile.OrderHeaders(keys) {
		for _, v := range headers[k] {
			buf.WriteString(k + ": " + v + "\r\n")
		}
	}
	buf.WriteString("\r\n")

	if _, err := io.WriteString(w, buf.String()); err != nil {
		return err
	}

	if req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	if chunked {
		cw := httputil.NewChunkedWriter(w)
		if _, err := io.Copy(cw, req.Body); err != nil {
			return err
		}
		if err := cw.Close(); err != nil {
			return err
		}
		// A chunked body ends with the terminating CRLF after the zero chunk;
		// NewChunkedWriter writes the zero chunk but not the trailer break.
		_, err := io.WriteString(w, "\r\n")
		return err
	}
	if req.ContentLength > 0 {
		_, err := io.CopyN(w, req.Body, req.ContentLength)
		return err
	}
	return nil
}

// h2Session carries requests over HTTP/2 with the profile's SETTINGS, window
// update, pseudo-header order and priority frames — the parts of an HTTP/2
// fingerprint that differ per client stack as much as the ClientHello does.
type h2Session struct {
	conn    net.Conn
	client  *fhttp.Client
	cc      h2ClientConn
	profile *tlsprofile.Profile
}

// h2ClientConn is the slice of *http2.ClientConn this file uses, named so the
// session can be exercised without a live HTTP/2 server.
type h2ClientConn interface {
	RoundTrip(*fhttp.Request) (*fhttp.Response, error)
	Close() error
}

func newH2Session(conn net.Conn, profile *tlsprofile.Profile) (*h2Session, error) {
	tr := profile.NewH2Transport()
	// Compression stays off so the response body reaches the client exactly as
	// the target sent it. The transport would otherwise add its own
	// Accept-Encoding and transparently decode, which both alters the request
	// the target sees and hands the client something it did not ask for.
	tr.DisableCompression = true

	cc, err := tr.NewClientConn(conn)
	if err != nil {
		return nil, fmt.Errorf("start HTTP/2 session: %w", err)
	}
	return &h2Session{conn: conn, cc: cc, profile: profile}, nil
}

func (s *h2Session) Protocol() string { return "h2" }
func (s *h2Session) Close() error     { return s.cc.Close() }

// Upgradable reports false: HTTP/2 has no 101 transition, so there is never a
// raw connection to hand over.
func (s *h2Session) Upgradable() (net.Conn, *bufio.Reader, bool) { return nil, nil, false }

func (s *h2Session) RoundTrip(req *http.Request, host string, timeout time.Duration) (*http.Response, error) {
	// A deadline on the underlying connection would tear down the whole
	// multiplexed session rather than the one request, so the timeout rides on
	// the request context instead.
	ctx := req.Context()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	fReq, err := toFrameworkRequest(req, host, s.profile, ctx)
	if err != nil {
		return nil, err
	}

	fResp, err := s.cc.RoundTrip(fReq)
	if err != nil {
		return nil, fmt.Errorf("http/2 round trip: %w", err)
	}
	return fromFrameworkResponse(fResp, req), nil
}

// toFrameworkRequest converts a request parsed off the client into the fork's
// type, carrying the profile's header and pseudo-header order with it.
func toFrameworkRequest(
	req *http.Request,
	host string,
	profile *tlsprofile.Profile,
	ctx context.Context,
) (*fhttp.Request, error) {
	authority := req.Host
	if authority == "" {
		authority = host
	}

	target := &url.URL{
		Scheme:   "https",
		Host:     authority,
		Path:     req.URL.Path,
		RawQuery: req.URL.RawQuery,
		Opaque:   req.URL.Opaque,
	}

	out := &fhttp.Request{
		Method:        req.Method,
		URL:           target,
		Host:          authority,
		Body:          req.Body,
		ContentLength: req.ContentLength,
		Header:        make(fhttp.Header, len(req.Header)+2),
	}

	keys := make([]string, 0, len(req.Header))
	for k, v := range req.Header {
		if _, hop := hopHeaders[k]; hop {
			continue
		}
		out.Header[k] = v
		keys = append(keys, k)
	}

	// The fork reads these two magic keys to order the header block and the
	// pseudo-header block; it strips them before anything goes on the wire.
	ordered := profile.OrderHeaders(keys)
	lower := make([]string, len(ordered))
	for i, k := range ordered {
		lower[i] = strings.ToLower(k)
	}
	out.Header[fhttp.HeaderOrderKey] = lower
	out.Header[fhttp.PHeaderOrderKey] = profile.PseudoHeaderOrder()

	return out.WithContext(ctx), nil
}

// fromFrameworkResponse converts the fork's response back so it can be written
// to the HTTP/1.1 client.
func fromFrameworkResponse(resp *fhttp.Response, req *http.Request) *http.Response {
	header := make(http.Header, len(resp.Header))
	for k, v := range resp.Header {
		if _, hop := hopHeaders[k]; hop {
			continue
		}
		header[k] = v
	}

	// The client connection is HTTP/1.1, so the response is relabelled as
	// such. An unknown length becomes a chunked body on the way out, which is
	// how net/http frames ContentLength -1 for a 1.1 client.
	return &http.Response{
		Status:        resp.Status,
		StatusCode:    resp.StatusCode,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
		Trailer:       http.Header(resp.Trailer),
		Request:       req,
	}
}
