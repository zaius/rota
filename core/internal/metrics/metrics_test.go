package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

func TestStatusClass(t *testing.T) {
	cases := map[int]string{
		0:   "none",
		99:  "none",
		200: "2xx",
		301: "3xx",
		404: "4xx",
		503: "5xx",
		600: "none",
	}
	for code, want := range cases {
		if got := statusClass(code); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestPoolLabel(t *testing.T) {
	SetPoolNames(map[int]string{7: "eu-residential"})

	if got := PoolLabel(0); got != "default" {
		t.Errorf("PoolLabel(0) = %q, want %q", got, "default")
	}
	if got := PoolLabel(7); got != "eu-residential" {
		t.Errorf("PoolLabel(7) = %q, want %q", got, "eu-residential")
	}
	if got := PoolLabel(42); got != "pool-42" {
		t.Errorf("PoolLabel(42) = %q, want %q", got, "pool-42")
	}
}

// Setup installs the SDK on the process-global meter provider, and the OTel
// global binds instruments to the first real provider it sees — so everything
// that depends on a live pipeline runs in this one test, in order.
func TestProviderEndToEnd(t *testing.T) {
	log := logger.New("error")

	provider, err := Setup(context.Background(), "", log)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer provider.Shutdown(context.Background())

	SetPoolNames(map[int]string{1: "test-pool"})
	RecordProxyRequest(context.Background(), 1, "alice", true, 200, 42)
	RecordProxyTunnel(context.Background(), 1, "alice", true, 10, 20, 1500)
	RecordAuthRejection(context.Background(), "bad_credentials")
	RecordRateLimitRejection(context.Background())
	RecordHealthCheck(context.Background(), false, 950)
	RecordSourceFetch(context.Background(), true, 12)
	RecordPoolAlert(context.Background(), true)
	RecordAPIRequest(context.Background(), "/api/v1/pools", http.MethodGet, 200, 3*time.Millisecond)
	RegisterProxyObservables(
		func() int64 { return 3 },
		func() int64 { return 2 },
		func() int64 { return 1 },
	)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	provider.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	// One entry per exposition-format name the README documents, so the docs
	// can't drift from what the exporter actually emits.
	for _, want := range []string{
		"rota_proxy_requests_total",
		`pool="test-pool"`,
		`user="alice"`,
		`status_class="2xx"`,
		"rota_proxy_request_duration_seconds",
		"rota_proxy_tunnels_total",
		"rota_proxy_tunnel_duration_seconds",
		"rota_proxy_tunnel_io_bytes_total",
		"rota_proxy_open_tunnels 3",
		"rota_proxy_sessions_active 2",
		"rota_proxy_domain_cooldowns_active 1",
		"rota_proxy_auth_rejections_total",
		"rota_proxy_ratelimit_rejections_total",
		"rota_healthcheck_checks_total",
		"rota_healthcheck_duration_seconds",
		"rota_source_fetches_total",
		"rota_source_proxies_imported_total",
		"rota_pool_alerts_total",
		"rota_api_requests_total",
		"rota_api_request_duration_seconds",
		// contrib runtime instrumentation must flow through the same registry
		"go_memory_used_bytes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics output missing %q", want)
		}
	}

	// A bearer-token provider must reject unauthenticated scrapes. This
	// second provider serves its own registry; the global instrument binding
	// doesn't matter for the auth check.
	authed, err := Setup(context.Background(), "s3cret", log)
	if err != nil {
		t.Fatalf("Setup with token: %v", err)
	}
	defer authed.Shutdown(context.Background())

	rec = httptest.NewRecorder()
	authed.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated scrape = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	authed.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("authenticated scrape = %d, want %d", rec.Code, http.StatusOK)
	}
}
