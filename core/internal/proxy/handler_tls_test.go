package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/tlsprofile"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

// Exercise auth suffixes, CONNECT diagnostics, and the certificate the client
// actually receives through an upstream proxy.
func TestProxyRouter_TLSInspectionDiagnostics(t *testing.T) {
	ca := testCertAuthority(t)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "origin response") //nolint:errcheck
	}))
	defer origin.Close()
	host := origin.Listener.Addr().String()
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		if r.Method != http.MethodConnect || r.Host != host {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.Host)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		target, err := net.DialTimeout("tcp", host, 5*time.Second)
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
		BidirectionalCopy(conn, target) //nolint:errcheck
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name          string
		inspector     string
		inspectTLS    bool
		profile       string
		override      string
		reason        string
		wantInspected bool
	}{
		{name: "default opaque tunnel"},
		{name: "CA alone does not opt in", inspector: "configured"},
		{name: "inspection without CA", inspectTLS: true, reason: "tls_inspection_ca_not_configured"},
		{name: "inspector without CA", inspector: "empty", inspectTLS: true, reason: "tls_inspection_ca_not_configured"},
		{name: "stored profile without CA", inspectTLS: true, profile: "chrome", reason: "tls_inspection_ca_not_configured"},
		{name: "override without CA", inspectTLS: true, profile: "chrome", override: "ios", reason: "tls_inspection_ca_not_configured"},
		{name: "override without opt-in", inspector: "configured", override: "ios", reason: "tls_inspection_disabled"},
		{name: "stored profile without opt-in stays dormant", inspector: "configured", profile: "chrome"},
		{name: "explicit Go without opt-in", inspector: "configured", profile: "chrome", override: "go", reason: "tls_inspection_disabled"},
		{name: "bypassed target", inspector: "bypass", inspectTLS: true, override: "ios", reason: "tls_inspection_bypassed"},
		{name: "inspect with Go", inspector: "configured", inspectTLS: true, wantInspected: true},
		{name: "inspect with stored profile", inspector: "configured", inspectTLS: true, profile: "chrome", wantInspected: true},
		{name: "inspect with override", inspector: "configured", inspectTLS: true, profile: "chrome", override: "ios", wantInspected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attemptsBefore := upstreamRequests.Load()
			var logs bytes.Buffer
			log := &logger.Logger{Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))}
			var inspector *TLSInspector
			switch tc.inspector {
			case "empty":
				inspector = &TLSInspector{}
			case "configured", "bypass":
				var bypass []string
				if tc.inspector == "bypass" {
					bypass = []string{"127.0.0.1"}
				}
				inspector = NewTLSInspector(ca, bypass, log)
				inspector.upstreamRoots = x509.NewCertPool()
				inspector.upstreamRoots.AddCert(origin.Certificate())
			}
			profile, err := tlsprofile.Lookup(tc.profile)
			if err != nil {
				t.Fatal(err)
			}
			chain := &PoolChain{
				username: "alice", inspectTLS: tc.inspectTLS, tlsProfile: profile,
				selectors: []*PoolSelector{newMethodSelector("roundrobin",
					&models.Proxy{ID: 42, Protocol: "http", Address: upstream.Listener.Addr().String()},
					&models.Proxy{ID: 43, Protocol: "http", Address: upstream.Listener.Addr().String()},
				)},
			}
			auth := newTestUserAuthMw()
			cacheTestUser(auth, chain)
			router := &proxyRouter{userAuthMw: auth, upstream: NewUpstreamProxyHandler(nil, inspector, log)}
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
			if tc.override != "" {
				username += "-profile-" + tc.override
			}
			basicProxyAuth(req, username, "secret")
			if err := req.Write(conn); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), req)
			if err != nil {
				t.Fatal(err)
			}
			if tc.reason != "" {
				if resp.StatusCode != 594 || resp.Header.Get(ProxyErrorHeader) != tc.reason {
					t.Fatalf("unexpected inspection error: %d %v", resp.StatusCode, resp.Header)
				}
				if resp.Header.Get(ProxyIDHeader) != "" || resp.Header.Get("Retry-After") != "" {
					t.Fatalf("inspection error claims a proxy or retry delay: %v", resp.Header)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("CONNECT handler did not finish")
				}
				if upstreamRequests.Load() != attemptsBefore || chain.selectors[0].rrIdx != 0 {
					t.Fatal("inspection failure selected or contacted an upstream proxy")
				}
				var entry struct {
					Level, Msg, Source, Username, Host, Profile, Reason, Action string
					InspectTLS                                                  bool `json:"inspect_tls"`
				}
				decoder := json.NewDecoder(&logs)
				if err := decoder.Decode(&entry); err != nil {
					t.Fatalf("missing error log: %v", err)
				}
				wantProfile := profile.Name
				if tc.override != "" {
					wantProfile = tc.override
				}
				if entry.Level != "ERROR" || entry.Source != "proxy" || entry.Username != "alice" || entry.Host != host || entry.Profile != wantProfile || entry.Reason != tc.reason || entry.InspectTLS != tc.inspectTLS || !strings.Contains(entry.Msg, "CONNECT rejected") {
					t.Fatalf("incorrect error log: %+v", entry)
				}
				for _, hint := range map[string][]string{
					"tls_inspection_ca_not_configured": {"TLS_INSPECT_CA_CERT", "TLS_INSPECT_CA_KEY", "mount", "restart"},
					"tls_inspection_disabled":          {"Enable inspect_tls", "-profile-"},
					"tls_inspection_bypassed":          {"TLS_INSPECT_BYPASS_DOMAINS", "127.0.0.1"},
				}[tc.reason] {
					if !strings.Contains(entry.Action, hint) || !strings.Contains(string(body), hint) {
						t.Errorf("error log or body omits %q: %+v, %s", hint, entry, body)
					}
				}
				if !strings.Contains(string(body), "profile "+wantProfile) {
					t.Errorf("error body omits effective profile: %s", body)
				}
				if err := decoder.Decode(&entry); err != io.EOF {
					t.Fatalf("expected exactly one error log, got another: %+v, %v", entry, err)
				}
				return
			}
			if resp.StatusCode != http.StatusOK || resp.Header.Get(ProxyIDHeader) != "42" || resp.Header.Get(ProxyErrorHeader) != "" {
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

			if logs.Len() != 0 {
				t.Fatalf("unexpected error log: %s", &logs)
			}
		})
	}
}
