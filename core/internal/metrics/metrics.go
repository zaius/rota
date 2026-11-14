// Package metrics defines rota's OpenTelemetry instruments and the helpers the
// rest of the codebase calls to record them.
//
// Instruments are created against the global meter provider, so every recording
// call is a cheap no-op until the composition root installs the SDK via Setup —
// and stays a no-op forever when metrics are disabled. Callers therefore never
// need to check whether metrics are on.
//
// Label discipline: client labels come from a handful of trusted clients'
// proxy usernames, so they are labels. Proxy IDs, client IPs and target domains
// are unbounded and must never become labels — per-proxy and per-domain
// analytics stay in the event store, which is built for that cardinality.
package metrics

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const scopeName = "github.com/zaius/rota/core"

var meter = otel.Meter(scopeName)

// Attempt/request durations: proxied requests routinely take seconds, and the
// rotation timeout defaults to 90s, so the tail buckets stretch past a minute.
var requestBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// Tunnels are long-lived by design (browsing sessions, streaming), so buckets
// run from sub-second failures out to hours.
var tunnelBuckets = []float64{0.1, 0.5, 1, 5, 15, 60, 300, 900, 3600, 14400}

// API calls are mostly instant, but bulk health checks legitimately run for
// minutes (the server allows 10), hence the long tail.
var apiBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 30, 120, 600}

var (
	proxyRequests         metric.Int64Counter
	proxyRequestDuration  metric.Float64Histogram
	proxyTunnels          metric.Int64Counter
	proxyTunnelDuration   metric.Float64Histogram
	proxyTunnelBytes      metric.Int64Counter
	authRejections        metric.Int64Counter
	healthChecks          metric.Int64Counter
	healthCheckDuration   metric.Float64Histogram
	sourceFetches         metric.Int64Counter
	sourceProxiesImported metric.Int64Counter
	eventsWritten         metric.Int64Counter
	apiRequests           metric.Int64Counter
	apiRequestDuration    metric.Float64Histogram
)

// Instrument creation only fails on malformed names, so errors are ignored: a
// nil instrument would panic on first use, while these constants are exercised
// by every test run.
func init() {
	proxyRequests, _ = meter.Int64Counter("rota.proxy.requests",
		metric.WithDescription("Upstream proxy attempts (each retry counts separately), by client and outcome"))
	proxyRequestDuration, _ = meter.Float64Histogram("rota.proxy.request.duration",
		metric.WithDescription("Duration of upstream proxy attempts"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(requestBuckets...))
	proxyTunnels, _ = meter.Int64Counter("rota.proxy.tunnels",
		metric.WithDescription("Completed CONNECT tunnels, by client and outcome"))
	proxyTunnelDuration, _ = meter.Float64Histogram("rota.proxy.tunnel.duration",
		metric.WithDescription("Lifetime of completed CONNECT tunnels"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(tunnelBuckets...))
	proxyTunnelBytes, _ = meter.Int64Counter("rota.proxy.tunnel.io",
		metric.WithDescription("Bytes moved through CONNECT tunnels, by direction (up = client to target)"),
		metric.WithUnit("By"))
	authRejections, _ = meter.Int64Counter("rota.proxy.auth.rejections",
		metric.WithDescription("Proxy requests rejected with 407, by reason"))
	healthChecks, _ = meter.Int64Counter("rota.healthcheck.checks",
		metric.WithDescription("Proxy health checks performed, by outcome"))
	healthCheckDuration, _ = meter.Float64Histogram("rota.healthcheck.duration",
		metric.WithDescription("Duration of proxy health checks"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(requestBuckets...))
	sourceFetches, _ = meter.Int64Counter("rota.source.fetches",
		metric.WithDescription("Proxy source list fetches, by outcome"))
	sourceProxiesImported, _ = meter.Int64Counter("rota.source.proxies_imported",
		metric.WithDescription("New proxies imported from source fetches"))
	eventsWritten, _ = meter.Int64Counter("rota.events.written",
		metric.WithDescription("Request and tunnel history events written to the event store, by kind and outcome"))
	apiRequests, _ = meter.Int64Counter("rota.api.requests",
		metric.WithDescription("Management API requests, by route, method and status"))
	apiRequestDuration, _ = meter.Float64Histogram("rota.api.request.duration",
		metric.WithDescription("Duration of management API requests"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(apiBuckets...))
}

func clientLabel(client string) string {
	if client == "" {
		return "default"
	}
	return client
}

func outcomeLabel(success bool) string {
	if success {
		return "success"
	}
	return "failure"
}

// statusClass buckets an HTTP status into 1xx..5xx. Failed attempts usually
// carry no status at all (dial errors, timeouts), which lands in "none".
func statusClass(statusCode int) string {
	if statusCode >= 100 && statusCode < 600 {
		return fmt.Sprintf("%dxx", statusCode/100)
	}
	return "none"
}

// RecordProxyRequest records one upstream proxy attempt: a forwarded HTTP
// request, a CONNECT establishment, or a request observed inside an inspected
// tunnel. Each retry is its own attempt, mirroring how the event store counts.
func RecordProxyRequest(ctx context.Context, client string, success bool, statusCode, elapsedMs int) {
	attrs := metric.WithAttributes(
		attribute.String("client", clientLabel(client)),
		attribute.String("outcome", outcomeLabel(success)),
		attribute.String("status_class", statusClass(statusCode)),
	)
	proxyRequests.Add(ctx, 1, attrs)
	proxyRequestDuration.Record(ctx, float64(elapsedMs)/1000,
		metric.WithAttributes(attribute.String("outcome", outcomeLabel(success))))
}

// RecordProxyTunnel records one completed CONNECT tunnel. clean is false when
// the tunnel tore down with an error.
func RecordProxyTunnel(ctx context.Context, client string, clean bool, bytesUp, bytesDown int64, durationMs int) {
	label := attribute.String("client", clientLabel(client))
	outcome := "clean"
	if !clean {
		outcome = "error"
	}
	proxyTunnels.Add(ctx, 1, metric.WithAttributes(label, attribute.String("outcome", outcome)))
	proxyTunnelDuration.Record(ctx, float64(durationMs)/1000)
	ioAttrs := func(direction string) metric.MeasurementOption {
		return metric.WithAttributes(label, attribute.String("direction", direction))
	}
	if bytesUp > 0 {
		proxyTunnelBytes.Add(ctx, bytesUp, ioAttrs("up"))
	}
	if bytesDown > 0 {
		proxyTunnelBytes.Add(ctx, bytesDown, ioAttrs("down"))
	}
}

// RecordAuthRejection records a proxy request rejected with 407. reason is one
// of "missing_credentials", "bad_credentials", "invalid_options".
func RecordAuthRejection(ctx context.Context, reason string) {
	authRejections.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// RecordHealthCheck records one proxy health-check probe.
func RecordHealthCheck(ctx context.Context, success bool, elapsedMs int) {
	outcome := metric.WithAttributes(attribute.String("outcome", outcomeLabel(success)))
	healthChecks.Add(ctx, 1, outcome)
	healthCheckDuration.Record(ctx, float64(elapsedMs)/1000, outcome)
}

// RecordSourceFetch records one proxy-source list fetch and how many new
// proxies it imported.
func RecordSourceFetch(ctx context.Context, success bool, imported int) {
	sourceFetches.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcomeLabel(success))))
	if imported > 0 {
		sourceProxiesImported.Add(ctx, int64(imported))
	}
}

// RecordEventsWritten records a write of history events; kind is "request" or
// "tunnel". A failed write loses its events.
func RecordEventsWritten(ctx context.Context, kind string, count int, success bool) {
	eventsWritten.Add(ctx, int64(count), metric.WithAttributes(
		attribute.String("kind", kind),
		attribute.String("outcome", outcomeLabel(success)),
	))
}

// RecordAPIRequest records one management API request. route is the matched
// chi pattern (bounded), never the raw path.
func RecordAPIRequest(ctx context.Context, route, method string, status int, elapsed time.Duration) {
	if route == "" {
		route = "(unmatched)"
	}
	attrs := metric.WithAttributes(
		attribute.String("route", route),
		attribute.String("method", method),
		attribute.Int("status", status),
	)
	apiRequests.Add(ctx, 1, attrs)
	apiRequestDuration.Record(ctx, elapsed.Seconds(),
		metric.WithAttributes(attribute.String("route", route), attribute.String("method", method)))
}

// RegisterProxyObservables registers the proxy server's live gauges: open
// CONNECT tunnels, active sticky sessions and active domain cooldowns. Called
// once by proxy.New; before the SDK is installed the registration lands on the
// no-op provider and observes nothing.
func RegisterProxyObservables(openTunnels, activeSessions, activeDomainCooldowns func() int64) {
	tunnelsGauge, _ := meter.Int64ObservableGauge("rota.proxy.open_tunnels",
		metric.WithDescription("CONNECT tunnels currently established"))
	sessionsGauge, _ := meter.Int64ObservableGauge("rota.proxy.sessions.active",
		metric.WithDescription("Live sticky-session bindings"))
	cooldownsGauge, _ := meter.Int64ObservableGauge("rota.proxy.domain_cooldowns.active",
		metric.WithDescription("Active domain-scoped proxy cooldowns"))

	_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(tunnelsGauge, openTunnels())
		o.ObserveInt64(sessionsGauge, activeSessions())
		o.ObserveInt64(cooldownsGauge, activeDomainCooldowns())
		return nil
	}, tunnelsGauge, sessionsGauge, cooldownsGauge)
}
