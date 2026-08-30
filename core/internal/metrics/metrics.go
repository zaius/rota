// Package metrics defines rota's OpenTelemetry instruments and the helpers the
// rest of the codebase calls to record them.
//
// Instruments are created against the global meter provider, so every recording
// call is a cheap no-op until the composition root installs the SDK via Setup —
// and stays a no-op forever when metrics are disabled. Callers therefore never
// need to check whether metrics are on.
//
// Label discipline: pool and proxy-user names are admin-defined and bounded, so
// they are labels. Proxy IDs, client IPs and target domains are unbounded and
// must never become labels — per-proxy and per-domain analytics stay in the
// event store, which is built for that cardinality.
package metrics

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const scopeName = "github.com/alpkeskin/rota/core"

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
	rateLimitRejections   metric.Int64Counter
	healthChecks          metric.Int64Counter
	healthCheckDuration   metric.Float64Histogram
	sourceFetches         metric.Int64Counter
	sourceProxiesImported metric.Int64Counter
	poolAlerts            metric.Int64Counter
	apiRequests           metric.Int64Counter
	apiRequestDuration    metric.Float64Histogram
)

// Instrument creation only fails on malformed names, so errors are ignored: a
// nil instrument would panic on first use, while these constants are exercised
// by every test run.
func init() {
	proxyRequests, _ = meter.Int64Counter("rota.proxy.requests",
		metric.WithDescription("Upstream proxy attempts (each retry counts separately), by pool, user and outcome"))
	proxyRequestDuration, _ = meter.Float64Histogram("rota.proxy.request.duration",
		metric.WithDescription("Duration of upstream proxy attempts"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(requestBuckets...))
	proxyTunnels, _ = meter.Int64Counter("rota.proxy.tunnels",
		metric.WithDescription("Completed CONNECT tunnels, by pool, user and outcome"))
	proxyTunnelDuration, _ = meter.Float64Histogram("rota.proxy.tunnel.duration",
		metric.WithDescription("Lifetime of completed CONNECT tunnels"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(tunnelBuckets...))
	proxyTunnelBytes, _ = meter.Int64Counter("rota.proxy.tunnel.io",
		metric.WithDescription("Bytes moved through CONNECT tunnels, by direction (up = client to target)"),
		metric.WithUnit("By"))
	authRejections, _ = meter.Int64Counter("rota.proxy.auth.rejections",
		metric.WithDescription("Proxy requests rejected with 407, by reason"))
	rateLimitRejections, _ = meter.Int64Counter("rota.proxy.ratelimit.rejections",
		metric.WithDescription("Proxy requests rejected with 429 by the per-IP rate limiter"))
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
	poolAlerts, _ = meter.Int64Counter("rota.pool.alerts",
		metric.WithDescription("Pool alert webhooks fired, by delivery outcome"))
	apiRequests, _ = meter.Int64Counter("rota.api.requests",
		metric.WithDescription("Management API requests, by route, method and status"))
	apiRequestDuration, _ = meter.Float64Histogram("rota.api.request.duration",
		metric.WithDescription("Duration of management API requests"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(apiBuckets...))
}

// poolNames maps pool ID to pool name for hot-path label resolution. It is
// maintained by the FleetPoller; until the first refresh completes, labels fall
// back to "pool-<id>".
var poolNames atomic.Pointer[map[int]string]

// SetPoolNames publishes the pool ID → name mapping used for metric labels.
func SetPoolNames(names map[int]string) {
	poolNames.Store(&names)
}

// PoolLabel resolves a pool ID to its metric label. Pool 0 is the default pool
// (a proxy user's requests before any named pool matched).
func PoolLabel(poolID int) string {
	if poolID == 0 {
		return "default"
	}
	if m := poolNames.Load(); m != nil {
		if name, ok := (*m)[poolID]; ok {
			return name
		}
	}
	return fmt.Sprintf("pool-%d", poolID)
}

func userLabel(username string) string {
	if username == "" {
		return "default"
	}
	return username
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
func RecordProxyRequest(ctx context.Context, poolID int, username string, success bool, statusCode, elapsedMs int) {
	attrs := metric.WithAttributes(
		attribute.String("pool", PoolLabel(poolID)),
		attribute.String("user", userLabel(username)),
		attribute.String("outcome", outcomeLabel(success)),
		attribute.String("status_class", statusClass(statusCode)),
	)
	proxyRequests.Add(ctx, 1, attrs)
	proxyRequestDuration.Record(ctx, float64(elapsedMs)/1000,
		metric.WithAttributes(
			attribute.String("pool", PoolLabel(poolID)),
			attribute.String("outcome", outcomeLabel(success)),
		))
}

// RecordProxyTunnel records one completed CONNECT tunnel. clean is false when
// the tunnel tore down with an error.
func RecordProxyTunnel(ctx context.Context, poolID int, username string, clean bool, bytesUp, bytesDown int64, durationMs int) {
	pool := PoolLabel(poolID)
	user := userLabel(username)
	outcome := "clean"
	if !clean {
		outcome = "error"
	}
	proxyTunnels.Add(ctx, 1, metric.WithAttributes(
		attribute.String("pool", pool),
		attribute.String("user", user),
		attribute.String("outcome", outcome),
	))
	proxyTunnelDuration.Record(ctx, float64(durationMs)/1000,
		metric.WithAttributes(attribute.String("pool", pool)))
	ioAttrs := func(direction string) metric.MeasurementOption {
		return metric.WithAttributes(
			attribute.String("pool", pool),
			attribute.String("user", user),
			attribute.String("direction", direction),
		)
	}
	if bytesUp > 0 {
		proxyTunnelBytes.Add(ctx, bytesUp, ioAttrs("up"))
	}
	if bytesDown > 0 {
		proxyTunnelBytes.Add(ctx, bytesDown, ioAttrs("down"))
	}
}

// RecordAuthRejection records a proxy request rejected with 407. reason is one
// of "missing_credentials", "bad_credentials", "bad_profile".
func RecordAuthRejection(ctx context.Context, reason string) {
	authRejections.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// RecordRateLimitRejection records a proxy request rejected with 429. The
// client IP is deliberately not a label (unbounded).
func RecordRateLimitRejection(ctx context.Context) {
	rateLimitRejections.Add(ctx, 1)
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

// RecordPoolAlert records one fired pool alert webhook.
func RecordPoolAlert(ctx context.Context, delivered bool) {
	outcome := "delivered"
	if !delivered {
		outcome = "failed"
	}
	poolAlerts.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
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
