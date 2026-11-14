package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zaius/rota/core/internal/models"
	"github.com/zaius/rota/core/pkg/logger"
)

// Drive a CONNECT through the router and an upstream proxy to a TLS origin,
// and check which certificate the client actually receives.
func TestProxyRouter_TLSInspection(t *testing.T) {
	ca := testCertAuthority(t)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "origin response") //nolint:errcheck
	}))
	defer origin.Close()
	host := origin.Listener.Addr().String()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer target.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		target.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		BidirectionalCopy(conn, target, nil) //nolint:errcheck
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name          string
		inspector     bool
		profile       string
		wantInspected bool
	}{
		{name: "default opaque tunnel"},
		{name: "CA alone does not opt in", inspector: true},
		{name: "inspect with Go", inspector: true, profile: "go", wantInspected: true},
		{name: "inspect with profile", inspector: true, profile: "ios", wantInspected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := logger.New("error")
			var inspector *TLSInspector
			if tc.inspector {
				inspector = NewTLSInspector(ca, nil, log)
				inspector.upstreamRoots = x509.NewCertPool()
				inspector.upstreamRoots.AddCert(origin.Certificate())
			}
			r := newTestRotator(t, &models.Proxy{ID: 42, Protocol: "http", Address: upstream.Listener.Addr().String()})
			router := &proxyRouter{upstream: NewUpstreamProxyHandler(r, inspector, log), password: "secret"}
			done := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				router.ServeHTTP(w, r)
			}))
			defer server.Close()
			conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			req := httptest.NewRequest(http.MethodConnect, host, nil)
			username := "alice"
			if tc.profile != "" {
				username += "-profile-" + tc.profile
			}
			basicProxyAuth(req, username, "secret")
			if err := req.Write(conn); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || resp.Header.Get(ProxyIDHeader) != "42" {
				t.Fatalf("unexpected CONNECT response: %d %v", resp.StatusCode, resp.Header)
			}

			roots := x509.NewCertPool()
			roots.AddCert(origin.Certificate())
			roots.AddCert(ca.cert)
			client := tls.Client(conn, &tls.Config{ServerName: "127.0.0.1", RootCAs: roots})
			defer client.Close()
			if err := client.Handshake(); err != nil {
				t.Fatal(err)
			}
			cert := client.ConnectionState().PeerCertificates[0]
			if tc.wantInspected {
				if err := cert.CheckSignatureFrom(ca.cert); err != nil {
					t.Fatalf("client did not receive the inspection CA certificate: %v", err)
				}
			} else if !cert.Equal(origin.Certificate()) {
				t.Fatal("opaque tunnel did not preserve the origin certificate")
			}
			get := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
			get.Close = true
			if err := get.Write(client); err != nil {
				t.Fatal(err)
			}
			resp, err = http.ReadResponse(bufio.NewReader(client), get)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(body) != "origin response" {
				t.Fatalf("tunnel response = %q, error %v", body, err)
			}
			client.Close()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("CONNECT handler did not finish")
			}
		})
	}
}
