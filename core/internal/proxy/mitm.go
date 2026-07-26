package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

// TLSInspector terminates client TLS inside a CONNECT tunnel so the individual
// HTTPS requests flowing through it can be counted and recorded.
//
// Without it a tunnel is opaque: Rota sees one CONNECT and then a stream of
// encrypted bytes, however many thousands of requests the client multiplexes
// over it. With it, every request inside the tunnel produces a normal request
// event — method, URL, status code, latency — the same shape the plain-HTTP
// path produces.
//
// This is a man-in-the-middle by construction and is off unless two separate
// things are true: a CA keypair is configured on the server, and the proxy user
// has inspect_tls set. Clients that want to own their own TLS simply stay
// opted out and get the untouched tunnel they get today.
//
// Two consequences are worth stating plainly:
//
//   - The client must trust the configured CA, or every intercepted request
//     fails its certificate check. Hosts that pin certificates will fail
//     regardless of trust — put those in the bypass list.
//   - Interception forces HTTP/1.1 (see connectALPN), because per-request
//     accounting means reading discrete request/response messages. That changes
//     the protocol and TLS fingerprint the target sees, which matters if the
//     target is fingerprinting.
type TLSInspector struct {
	ca     *CertAuthority
	bypass []string
	logger *logger.Logger

	// upstreamRoots overrides the roots used to verify the target's own
	// certificate. nil means the system pool, which is what production uses;
	// tests point it at their origin's CA.
	upstreamRoots *x509.CertPool
}

// connectALPN is the protocol set offered in both directions on an intercepted
// tunnel. HTTP/2 is deliberately excluded: h2 multiplexes concurrent streams
// over one connection, and demultiplexing it to count requests would mean
// implementing an h2 server and client here. Restricting to http/1.1 keeps the
// message loop a simple read-forward-read.
var connectALPN = []string{"http/1.1"}

// NewTLSInspector builds an inspector over a loaded CA. bypassDomains are
// hosts (and their subdomains) that are never intercepted, for targets that
// pin certificates or that must see an untouched TLS handshake.
func NewTLSInspector(ca *CertAuthority, bypassDomains []string, log *logger.Logger) *TLSInspector {
	bypass := make([]string, 0, len(bypassDomains))
	for _, d := range bypassDomains {
		if norm := NormalizeCooldownDomain(d); norm != "" {
			bypass = append(bypass, norm)
		}
	}
	return &TLSInspector{ca: ca, bypass: bypass, logger: log}
}

// ShouldInspect reports whether host (a CONNECT authority, with or without a
// port) may be intercepted.
func (i *TLSInspector) ShouldInspect(host string) bool {
	if i == nil || i.ca == nil {
		return false
	}
	h := normalizeHost(host)
	if h == "" {
		return false
	}
	for _, d := range i.bypass {
		if hostMatchesDomain(h, d) {
			return false
		}
	}
	return true
}

// Serve intercepts an established tunnel: it terminates TLS toward the client
// with a minted certificate, opens its own TLS session to the target over the
// upstream connection, and shuttles HTTP messages between them, recording each
// one against the binding.
//
// It returns the wire bytes moved in each direction and the number of requests
// observed. Both connections belong to the caller and are not closed here.
func (i *TLSInspector) Serve(
	clientConn, upstreamConn net.Conn,
	host string,
	binding *TunnelBinding,
	timeout time.Duration,
) (TunnelCounts, int, error) {
	// Count at the wire, under TLS: the plaintext loop above cannot see record
	// framing or handshake volume, and bandwidth is a wire question.
	countedClient := newCountingConn(clientConn)
	countedUpstream := newCountingConn(upstreamConn)
	counts := func() TunnelCounts {
		return TunnelCounts{
			BytesUp:   countedClient.BytesRead(),
			BytesDown: countedClient.BytesWritten(),
		}
	}

	serverName := normalizeHost(host)

	clientTLS := tls.Server(countedClient, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: connectALPN,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// Prefer the SNI the client actually asked for; fall back to the
			// CONNECT authority when the client sends none (bare-IP targets).
			name := serverName
			if hello.ServerName != "" {
				name = normalizeHost(hello.ServerName)
			}
			return i.ca.CertFor(name)
		},
	})

	if err := handshake(clientTLS, timeout); err != nil {
		return counts(), 0, fmt.Errorf("client TLS handshake for %s: %w", host, err)
	}
	if negotiated := clientTLS.ConnectionState().ServerName; negotiated != "" {
		serverName = normalizeHost(negotiated)
	}

	upstreamTLS := tls.Client(countedUpstream, &tls.Config{
		ServerName: serverName,
		RootCAs:    i.upstreamRoots,
		MinVersion: tls.VersionTLS12,
		NextProtos: connectALPN,
	})
	if err := handshake(upstreamTLS, timeout); err != nil {
		return counts(), 0, fmt.Errorf("upstream TLS handshake for %s: %w", host, err)
	}

	requests := i.pump(clientTLS, upstreamTLS, host, serverName, binding, timeout)
	return counts(), requests, nil
}

// handshake completes a TLS handshake under a deadline, clearing it afterwards
// so the long-lived session that follows is not cut off mid-stream.
func handshake(conn *tls.Conn, timeout time.Duration) error {
	if timeout > 0 {
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		defer conn.SetDeadline(time.Time{}) //nolint:errcheck
	}
	return conn.Handshake()
}

// pump shuttles HTTP messages between the two TLS sessions until either side
// closes, returning how many requests it carried.
func (i *TLSInspector) pump(
	clientTLS, upstreamTLS *tls.Conn,
	host, serverName string,
	binding *TunnelBinding,
	timeout time.Duration,
) int {
	clientReader := bufio.NewReader(clientTLS)
	upstreamReader := bufio.NewReader(upstreamTLS)

	requests := 0
	for {
		// No deadline on this read: a keep-alive connection may idle for a
		// long time between requests, exactly as it may on the pass-through
		// path, and tearing it down here would be a behaviour regression.
		req, err := http.ReadRequest(clientReader)
		if err != nil {
			if !isCleanClose(err) {
				i.logger.Debug("intercepted tunnel: read request failed",
					"source", "proxy", "host", host, "error", err)
			}
			return requests
		}

		requests++
		start := time.Now()
		url := "https://" + host + req.URL.RequestURI()
		clientWantsClose := req.Close

		resp, err := i.roundTrip(req, upstreamTLS, upstreamReader, timeout)
		if err != nil {
			binding.RecordRequest(req.Method, url, 0, false, err.Error(), start)
			i.logger.Warn("intercepted request failed",
				"source", "proxy", "host", host, "method", req.Method, "error", err)
			writeGatewayError(clientTLS, req)
			return requests
		}

		// Success here means the target answered, matching how the plain-HTTP
		// path scores an attempt. A 403 or 429 is an answer — the status code
		// is recorded alongside, which is what makes blocks visible.
		binding.RecordRequest(req.Method, url, resp.StatusCode, true, "", start)

		// A 101 hands the connection to another protocol (websockets), after
		// which there are no more HTTP messages to parse — relay the rest
		// verbatim.
		if resp.StatusCode == http.StatusSwitchingProtocols {
			i.relayUpgrade(clientTLS, upstreamTLS, clientReader, upstreamReader, resp, req)
			return requests
		}

		serverWantsClose := resp.Close
		if err := resp.Write(clientTLS); err != nil {
			resp.Body.Close() //nolint:errcheck
			i.logger.Debug("intercepted tunnel: write response failed",
				"source", "proxy", "host", host, "error", err)
			return requests
		}
		resp.Body.Close() //nolint:errcheck

		if clientWantsClose || serverWantsClose {
			return requests
		}
	}
}

// roundTrip forwards one request upstream and reads its response, absorbing
// any informational (1xx) responses that precede the real one — leaving those
// unread would desynchronize the next read from the message stream.
func (i *TLSInspector) roundTrip(
	req *http.Request,
	upstreamTLS *tls.Conn,
	upstreamReader *bufio.Reader,
	timeout time.Duration,
) (*http.Response, error) {
	if timeout > 0 {
		if err := upstreamTLS.SetDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		defer upstreamTLS.SetDeadline(time.Time{}) //nolint:errcheck
	}

	if err := req.Write(upstreamTLS); err != nil {
		return nil, fmt.Errorf("forward request: %w", err)
	}
	req.Body.Close() //nolint:errcheck

	for {
		resp, err := http.ReadResponse(upstreamReader, req)
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

// relayUpgrade forwards a 101 response and then copies the two connections
// verbatim, including whatever each bufio.Reader has already buffered.
func (i *TLSInspector) relayUpgrade(
	clientTLS, upstreamTLS *tls.Conn,
	clientReader, upstreamReader *bufio.Reader,
	resp *http.Response,
	req *http.Request,
) {
	if err := resp.Write(clientTLS); err != nil {
		i.logger.Debug("intercepted tunnel: write upgrade response failed",
			"source", "proxy", "error", err)
		return
	}
	resp.Body.Close() //nolint:errcheck

	// Bytes already pulled into the readers belong to the upgraded protocol
	// and would be lost if we copied straight from the connections.
	if n := upstreamReader.Buffered(); n > 0 {
		if _, err := io.CopyN(clientTLS, upstreamReader, int64(n)); err != nil {
			return
		}
	}
	if n := clientReader.Buffered(); n > 0 {
		if _, err := io.CopyN(upstreamTLS, clientReader, int64(n)); err != nil {
			return
		}
	}

	i.logger.Debug("intercepted tunnel: relaying protocol upgrade",
		"source", "proxy", "url", req.URL.String())
	BidirectionalCopy(clientTLS, upstreamTLS) //nolint:errcheck
}

// writeGatewayError reports an upstream failure to the client in-band, so a
// client inside the tunnel sees a 502 rather than a truncated connection.
func writeGatewayError(w io.Writer, req *http.Request) {
	resp := &http.Response{
		StatusCode:    http.StatusBadGateway,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		Body:          http.NoBody,
		ContentLength: 0,
		Close:         true,
		Request:       req,
	}
	resp.Write(w) //nolint:errcheck
}

// isCleanClose reports whether an error is an ordinary end-of-connection
// rather than something worth logging.
func isCleanClose(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed)
}
