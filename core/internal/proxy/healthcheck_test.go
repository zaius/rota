package proxy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// expiredCert returns an expired, self-signed certificate for 127.0.0.1.
func expiredCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "expired"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// startTLS serves h over TLS with cert, quietly: strict clients reject some
// of these handshakes by design.
func startTLS(t *testing.T, h http.Handler, cert tls.Certificate) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

// newExpiredTLSServer starts an HTTPS server with an expired, self-signed
// certificate — what a client sees through a proxy that intercepts TLS badly.
func newExpiredTLSServer(t *testing.T) *httptest.Server {
	return startTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), expiredCert(t))
}

// connectHandler is a minimal CONNECT proxy that counts the tunnels it opens.
func connectHandler(tunnels *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusBadRequest)
			return
		}
		target, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		tunnels.Add(1)
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			target.Close()
			return
		}
		client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() { io.Copy(target, client); target.Close(); client.Close() }()
		go func() { io.Copy(client, target); target.Close(); client.Close() }()
	})
}

// newCONNECTProxy starts a plain-HTTP CONNECT proxy, so the check's TLS
// handshake runs end to end against the target.
func newCONNECTProxy(t *testing.T) string {
	t.Helper()
	var tunnels atomic.Int64
	srv := httptest.NewServer(connectHandler(&tunnels))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestHealthCheckStrictTLS(t *testing.T) {
	target := newExpiredTLSServer(t)
	proxyAddr := newCONNECTProxy(t)
	h := &HealthChecker{logger: logger.New("error")}

	tests := []struct {
		name    string
		strict  bool
		want    string
		wantErr string
	}{
		{name: "lenient accepts an expired certificate", strict: false, want: "active"},
		{name: "strict rejects an expired certificate", strict: true, want: "failed", wantErr: "TLS/SSL error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h.setSettings(&models.HealthCheckSettings{
				Timeout: 5, Workers: 1, URL: target.URL, Status: http.StatusOK, StrictTLS: tc.strict,
			})
			result, err := h.CheckProxy(context.Background(), &models.Proxy{ID: 1, Address: proxyAddr, Protocol: "http"})
			if err != nil {
				t.Fatalf("CheckProxy: %v", err)
			}
			errMsg := ""
			if result.Error != nil {
				errMsg = *result.Error
			}
			if result.Status != tc.want {
				t.Fatalf("status = %q, want %q (error: %s)", result.Status, tc.want, errMsg)
			}
			if !strings.Contains(errMsg, tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", errMsg, tc.wantErr)
			}
		})
	}
}

func TestConfigureCheckTLSDropsLegacyMinimum(t *testing.T) {
	for _, strict := range []bool{false, true} {
		p := &models.Proxy{Address: "127.0.0.1:1", Protocol: "http"}
		transport, err := CreateProxyTransport(p)
		if err != nil {
			t.Fatal(err)
		}
		ConfigureCheckTLS(transport, p, strict)
		c := transport.TLSClientConfig
		if c.MinVersion != 0 || c.InsecureSkipVerify == strict || c.VerifyPeerCertificate != nil {
			t.Errorf("strict=%v: MinVersion=%x InsecureSkipVerify=%v, want Go's default minimum and verification=%v",
				strict, c.MinVersion, c.InsecureSkipVerify, strict)
		}
	}
}

// Strict checks verify the target, not the https proxy carrying them: the
// proxy's own certificate gets the leniency proxied traffic gives it.
func TestStrictCheckThroughHTTPSProxy(t *testing.T) {
	var tunnels atomic.Int64
	proxySrv := startTLS(t, connectHandler(&tunnels), expiredCert(t))
	p := &models.Proxy{Address: strings.TrimPrefix(proxySrv.URL, "https://"), Protocol: "https"}

	trusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer trusted.Close()
	untrusted := newExpiredTLSServer(t)

	get := func(url string) error {
		transport, err := CreateProxyTransport(p)
		if err != nil {
			t.Fatal(err)
		}
		ConfigureCheckTLS(transport, p, true)
		roots := x509.NewCertPool()
		roots.AddCert(trusted.Certificate())
		transport.TLSClientConfig.RootCAs = roots
		defer transport.CloseIdleConnections()

		resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Get(url)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}

	if err := get(trusted.URL); err != nil {
		t.Errorf("trusted target through an https proxy with an expired certificate: %v", err)
	}
	if err := get(untrusted.URL); err == nil || !strings.Contains(err.Error(), "x509") {
		t.Errorf("untrusted target = %v, want a certificate error", err)
	}
	if n := tunnels.Load(); n != 2 {
		t.Errorf("proxy opened %d tunnels, want 2", n)
	}
}
