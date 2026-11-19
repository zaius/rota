package proxy

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zaius/rota/core/internal/models"
	proxyDialer "golang.org/x/net/proxy"
	"h12.io/socks"
)

const connectTargetLookupTimeout = 5 * time.Second

// validateConnectTarget checks for an IPv4 address before selection can consume a
// rotation slot or session reservation. Keep the hostname in the CONNECT request
// so the upstream still resolves it using its own DNS view.
//
// unresolved is a lookup failure that proves nothing alone, such as a SERVFAIL
// or a timeout, so the target still goes to an upstream whose DNS may work.
func (r *Rotator) validateConnectTarget(ctx context.Context, authority string) (unresolved, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	host, err := connectTargetHost(authority)
	if err != nil {
		return nil, forwardingFailure("target_invalid", fmt.Errorf("invalid CONNECT target %q: %w", authority, err))
	}
	noIPv4 := func() error {
		return forwardingFailure("target_no_ipv4", &net.DNSError{
			Err: "no IPv4 address for CONNECT target", Name: host, IsNotFound: true,
		})
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return nil, noIPv4()
		}
		return nil, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, connectTargetLookupTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(lookupCtx, "ip4", host)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil && !isDNSNotFound(err) {
		return err, nil
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			return nil, nil
		}
	}

	// An IPv6-only name fails the A lookup exactly like NXDOMAIN does, so only a
	// missing AAAA record as well proves the name is dead. Any other AAAA
	// outcome still leaves the target without IPv4.
	_, err = net.DefaultResolver.LookupIP(lookupCtx, "ip6", host)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isDNSNotFound(err) {
		return nil, forwardingFailure("target_dns_not_found", fmt.Errorf("resolve CONNECT target %q: %w", host, err))
	}
	return nil, noIPv4()
}

// connectTargetHost returns the host of a CONNECT authority that an upstream
// could dial: a non-empty host and a port from 1 to 65535.
func connectTargetHost(authority string) (string, error) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return "", err
	}
	if host == "" {
		return "", errors.New("missing host")
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return "", fmt.Errorf("invalid port %q", port)
	}
	return host, nil
}

func isDNSNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// proxyDial connects to upstream proxies. Its timeout fails an unreachable
// proxy fast, leaving the rest of the attempt's time for the next proxy.
var proxyDial = &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}

// dialProxy connects to p itself, completing the TLS handshake to an https
// proxy. Like proxied traffic, it accepts any certificate and TLS 1.0.
func dialProxy(ctx context.Context, p *models.Proxy) (net.Conn, error) {
	conn, err := proxyDial.DialContext(ctx, "tcp", p.Address)
	if err != nil || p.Protocol != "https" {
		return conn, err
	}
	serverName, _, _ := net.SplitHostPort(p.Address)
	tlsConn := tls.Client(conn, &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS10})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy TLS handshake: %w", err)
	}
	return tlsConn, nil
}

// dialTarget opens a connection to target through p. ctx bounds the whole
// handshake, and cancelling it abandons the handshake.
func dialTarget(ctx context.Context, p *models.Proxy, target string) (net.Conn, error) {
	switch p.Protocol {
	case "socks5":
		return dialSocks5(ctx, p, target)
	case "socks4", "socks4a":
		return dialSocks4(p, target)
	case "http", "https":
		return connectViaHTTP(ctx, p, target)
	default:
		return nil, forwardingFailure("proxy_configuration_error", fmt.Errorf("unsupported protocol for CONNECT: %s", p.Protocol))
	}
}

func dialSocks5(ctx context.Context, p *models.Proxy, target string) (net.Conn, error) {
	var auth *proxyDialer.Auth
	if p.Username != nil && *p.Username != "" {
		pw := ""
		if p.Password != nil {
			pw = *p.Password
		}
		auth = &proxyDialer.Auth{User: *p.Username, Password: pw}
	}
	dialer, err := proxyDialer.SOCKS5("tcp", p.Address, auth, proxyDial)
	if err != nil {
		return nil, fmt.Errorf("socks5 dialer: %w", err)
	}
	conn, err := dialer.(proxyDialer.ContextDialer).DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, fmt.Errorf("socks5 dial %s via %s: %w", target, p.Address, err)
	}
	return conn, nil
}

// dialSocks4 takes no context because h12.io/socks doesn't: the dial timeout
// bounds each step of the handshake instead.
func dialSocks4(p *models.Proxy, target string) (net.Conn, error) {
	u := proxyURL(p)
	u.RawQuery = "timeout=" + proxyDial.Timeout.String()
	conn, err := socks.Dial(u.String())("tcp", target)
	if err != nil {
		return nil, fmt.Errorf("%s dial %s via %s: %w", p.Protocol, target, p.Address, err)
	}
	return conn, nil
}

// connectViaHTTP opens a CONNECT tunnel through an HTTP or HTTPS proxy.
func connectViaHTTP(ctx context.Context, p *models.Proxy, target string) (net.Conn, error) {
	conn, err := dialProxy(ctx, p)
	if err != nil {
		return nil, forwardingFailure("proxy_connect_failed", fmt.Errorf("dial proxy %s: %w", p.Address, err))
	}
	// The handshake's reads and writes take no context, so its deadline and
	// cancellation reach them through the connection.
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	err = sendCONNECT(conn, p, target)
	if !stop() && err == nil {
		err = ctx.Err() // ctx ended just as the handshake finished
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// sendCONNECT asks the proxy for a tunnel to target and reads its answer.
func sendCONNECT(conn net.Conn, p *models.Proxy, target string) error {
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if p.Username != nil && *p.Username != "" {
		pw := ""
		if p.Password != nil {
			pw = *p.Password
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(*p.Username + ":" + pw))
		req += "Proxy-Authorization: Basic " + encoded + "\r\n"
	}
	req += "User-Agent: Rota-Proxy/1.0\r\nProxy-Connection: Keep-Alive\r\n\r\n"

	if _, err := conn.Write([]byte(req)); err != nil {
		return forwardingFailure("proxy_handshake_failed", fmt.Errorf("send CONNECT to %s: %w", p.Address, err))
	}

	line, err := readCONNECTResponse(conn)
	if err != nil {
		return forwardingFailure("proxy_handshake_failed", fmt.Errorf("read CONNECT response from %s: %w", p.Address, err))
	}
	status, err := connectResponseStatus(line)
	if err != nil {
		return forwardingFailure("proxy_handshake_failed", err)
	}
	if status >= 200 && status < 300 {
		return nil
	}
	cause := fmt.Errorf("CONNECT target %s via %s rejected: %s", target, p.Address, line)
	switch {
	case status == http.StatusProxyAuthRequired:
		return forwardingFailure("upstream_proxy_auth_failed", cause)
	// A 402 is the proxy provider refusing an account that is out of bandwidth
	// or unpaid, so it counts against the proxy. A target rejection would not,
	// and would leave a dead list failing requests until the next health check.
	case status == http.StatusPaymentRequired:
		return forwardingFailure("upstream_proxy_payment_required", cause)
	case status < 400:
		return forwardingFailure("proxy_handshake_failed", cause)
	}
	return &upstreamFailure{reason: "upstream_connect_rejected", upstreamStatus: status, cause: cause}
}

func connectResponseStatus(line string) (int, error) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) >= 2 {
		_, _, versionOK := http.ParseHTTPVersion(parts[0])
		status, err := strconv.Atoi(parts[1])
		if versionOK && err == nil && len(parts[1]) == 3 && status >= 100 && status <= 599 {
			return status, nil
		}
	}
	return 0, fmt.Errorf("invalid CONNECT response: %s", line)
}

// readCONNECTResponse reads the proxy's CONNECT reply up to the end of the
// header block and returns its status line, without consuming any byte that
// belongs to the tunnelled stream.
//
// Reading into a large buffer swallows the first bytes of the target server's
// TLS ServerHello whenever the proxy pipelines them straight after "200
// Connection established": those bytes are then dropped on the floor and the
// client's handshake stalls. A CONNECT response carries no body, so the header
// terminator is a hard boundary — read one byte at a time and stop exactly on
// it. The response is a few hundred bytes, so the extra syscalls do not matter.
func readCONNECTResponse(conn net.Conn) (string, error) {
	var buf []byte
	b := make([]byte, 1)
	for {
		n, err := conn.Read(b)
		if n > 0 {
			buf = append(buf, b[0])
			if l := len(buf); l >= 4 &&
				buf[l-4] == '\r' && buf[l-3] == '\n' &&
				buf[l-2] == '\r' && buf[l-1] == '\n' {
				break
			}
			if len(buf) > 8192 {
				return "", fmt.Errorf("CONNECT response headers too large")
			}
		}
		if err != nil {
			return "", err
		}
	}
	statusLine := string(buf)
	if idx := strings.Index(statusLine, "\r\n"); idx >= 0 {
		statusLine = statusLine[:idx]
	}
	return statusLine, nil
}
