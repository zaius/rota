package metrics

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

// Provider owns the installed metrics SDK: the Prometheus registry backing the
// /metrics endpoint and, when OTLP is configured, the periodic push exporter.
type Provider struct {
	meterProvider *sdkmetric.MeterProvider
	registry      *prometheus.Registry
	bearerToken   string
}

// Setup builds the metrics pipeline and installs it as the global OTel meter
// provider, which activates every instrument in this package.
//
// Two exporters hang off one instrumentation layer:
//   - a Prometheus reader, always on, served by Handler() — the pull path any
//     Prometheus-compatible scraper (including SigNoz's collector) consumes;
//   - an OTLP periodic push exporter, enabled only when the standard
//     OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_METRICS_ENDPOINT env
//     vars are set — the direct path to OTLP-native backends such as SigNoz.
//
// OTLP transport, headers, TLS, push interval and resource attributes all come
// from the standard OTEL_* env vars, read by the exporters and SDK themselves;
// rota adds no vendor-specific configuration on top.
func Setup(ctx context.Context, bearerToken string, log *logger.Logger) (*Provider, error) {
	res, err := buildResource()
	if err != nil {
		return nil, fmt.Errorf("failed to build telemetry resource: %w", err)
	}

	registry := prometheus.NewRegistry()
	// process_* (CPU, RSS, open fds) on the scrape path — with the contrib
	// runtime instrumentation below, this replaces the removed gopsutil
	// system-metrics endpoint.
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	promExporter, err := otelprom.New(
		otelprom.WithRegisterer(registry),
		otelprom.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create prometheus exporter: %w", err)
	}

	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExporter),
	}

	if endpoint := otlpEndpoint(); endpoint != "" {
		exporter, protocol, err := newOTLPExporter(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create OTLP metrics exporter: %w", err)
		}
		// The periodic reader honors OTEL_METRIC_EXPORT_INTERVAL (default 60s).
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
		log.Info("OTLP metrics push enabled", "endpoint", endpoint, "protocol", protocol)
	}

	meterProvider := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(meterProvider)

	// Go runtime metrics (memory, GC, goroutines) — the replacement for the
	// removed gopsutil system-metrics endpoint, flowing through both exporters.
	if err := runtime.Start(runtime.WithMeterProvider(meterProvider)); err != nil {
		log.Warn("failed to start runtime metrics instrumentation", "error", err)
	}

	return &Provider{
		meterProvider: meterProvider,
		registry:      registry,
		bearerToken:   bearerToken,
	}, nil
}

// Handler returns the HTTP handler for the Prometheus /metrics endpoint. When
// a bearer token is configured, requests must carry it in the Authorization
// header — Prometheus-compatible scrapers support this natively.
func (p *Provider) Handler() http.Handler {
	h := promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{})
	if p.bearerToken == "" {
		return h
	}
	expected := []byte("Bearer " + p.bearerToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), expected) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Shutdown flushes and stops the exporters.
func (p *Provider) Shutdown(ctx context.Context) error {
	return p.meterProvider.Shutdown(ctx)
}

// buildResource describes this process to metrics backends. The env detector
// in resource.Default honors OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES; the
// service name only falls back to "rota" when the deployer set neither.
func buildResource() (*resource.Resource, error) {
	res := resource.Default()
	if os.Getenv("OTEL_SERVICE_NAME") == "" &&
		!strings.Contains(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"), "service.name=") {
		merged, err := resource.Merge(res, resource.NewSchemaless(semconv.ServiceName("rota")))
		if err != nil {
			return nil, err
		}
		res = merged
	}
	return res, nil
}

// otlpEndpoint returns the configured OTLP metrics endpoint, or "" when OTLP
// push is not configured.
func otlpEndpoint() string {
	if v := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"); v != "" {
		return v
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
}

// newOTLPExporter builds the OTLP exporter for the configured protocol. Both
// constructors read the standard OTEL_EXPORTER_OTLP_* env vars themselves
// (endpoint, headers, TLS, compression, timeout).
func newOTLPExporter(ctx context.Context) (sdkmetric.Exporter, string, error) {
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol == "" {
		protocol = "http/protobuf"
	}
	switch protocol {
	case "grpc":
		exp, err := otlpmetricgrpc.New(ctx)
		return exp, protocol, err
	case "http/protobuf", "http/json":
		exp, err := otlpmetrichttp.New(ctx)
		return exp, protocol, err
	default:
		return nil, protocol, fmt.Errorf("unsupported OTEL_EXPORTER_OTLP_PROTOCOL: %q (use grpc or http/protobuf)", protocol)
	}
}
