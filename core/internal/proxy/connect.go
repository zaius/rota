package proxy

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	proxyDialer "golang.org/x/net/proxy"
)

// connectViaSocks5 dials host through a SOCKS5 proxy.
func connectViaSocks5(p *models.Proxy, host string) (net.Conn, error) {
	var auth *proxyDialer.Auth
	if p.Username != nil && *p.Username != "" {
		pw := ""
		if p.Password != nil {
			pw = *p.Password
		}
		auth = &proxyDialer.Auth{User: *p.Username, Password: pw}
	}
	dialer, err := proxyDialer.SOCKS5("tcp", p.Address, auth, proxyDialer.Direct)
	if err != nil {
		return nil, fmt.Errorf("socks5 dialer: %w", err)
	}
	conn, err := dialer.Dial("tcp", host)
	if err != nil {
		return nil, fmt.Errorf("socks5 dial %s via %s: %w", host, p.Address, err)
	}
	return conn, nil
}

// connectViaHTTPStandalone sends a CONNECT request to an HTTP proxy.
func connectViaHTTPStandalone(p *models.Proxy, host string, timeout time.Duration) (net.Conn, error) {
	if timeout < 30*time.Second {
		timeout = 30 * time.Second
	}

	conn, err := net.DialTimeout("tcp", p.Address, timeout)
	if err != nil {
		return nil, forwardingFailure("proxy_connect_failed", fmt.Errorf("dial proxy %s: %w", p.Address, err))
	}

	_ = conn.SetDeadline(time.Now().Add(timeout))

	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", host, host)
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
		conn.Close()
		return nil, forwardingFailure("proxy_handshake_failed", fmt.Errorf("send CONNECT to %s: %w", p.Address, err))
	}

	line, err := readCONNECTResponse(conn)
	if err != nil {
		conn.Close()
		return nil, forwardingFailure("proxy_handshake_failed", fmt.Errorf("read CONNECT response from %s: %w", p.Address, err))
	}
	status, err := connectResponseStatus(line)
	if err != nil {
		conn.Close()
		return nil, forwardingFailure("proxy_handshake_failed", err)
	}
	if status < 200 || status >= 300 {
		conn.Close()
		reason := "proxy_connect_rejected"
		if status == http.StatusProxyAuthRequired {
			reason = "upstream_proxy_auth_failed"
		}
		return nil, forwardingFailure(reason, fmt.Errorf("CONNECT to %s rejected: %s", p.Address, line))
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
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
