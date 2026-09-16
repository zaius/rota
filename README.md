<div align="center" style="margin-bottom: 20px;">
  <img src="static/rota_logo.png" alt="rota" width="100px">
  <h1 align="center">
  Rota - Proxy Rotation Platform
  </h1>
</div>

<p align="center">
<a href="https://opensource.org/licenses/Apache-2.0"><img src="https://img.shields.io/badge/License-Apache%202.0-blue.svg"></a>
<a href="https://golang.org"><img src="https://img.shields.io/badge/Go-1.25.3-00ADD8?logo=go"></a>
<a href="https://react.dev"><img src="https://img.shields.io/badge/React-19-61DAFB?logo=react"></a>
<a href="https://www.timescale.com/"><img src="https://img.shields.io/badge/TimescaleDB-2.22-FDB515?logo=timescale"></a>
<a href="https://github.com/zaius/rota/releases"><img src="https://img.shields.io/github/release/zaius/rota"></a>
<a href="https://github.com/zaius/rota/actions"><img src="https://img.shields.io/github/actions/workflow/status/zaius/rota/release.yaml"></a>
</p>

![Rota Dashboard](static/dashboard.png)

## 🎯 Overview

**Rota** manages rotating proxies through a Go server and React dashboard, with per-user routing, sticky sessions, health checks, and traffic analytics.

## ✨ Key Features

- **Proxy routing:** HTTP and SOCKS proxies, per-pool rotation, ordered fallback pools, retries, and per-IP rate limits.
- **Pools and sources:** Scheduled imports, GeoIP enrichment, geo/ISP/tag filters, automatic or manual membership, and exports.
- **Sessions:** Exclusive reservations per target or custom scope, idle expiry, and explicit release/invalidation.
- **Monitoring:** Scheduled health checks, webhook alerts, and request/tunnel history.
- **HTTPS inspection:** Optional per-user TLS interception and configurable TLS/HTTP fingerprint profiles.
- **Security:** Authenticated proxy users, admin JWTs, bcrypt credentials, and login brute-force protection.
- **Deployment:** Docker Compose, PostgreSQL/TimescaleDB, optional ClickHouse event storage, and Prometheus/OTLP metrics.

---

## 🚀 Quick Start

### Using Docker Compose (Recommended)

The fastest way to get Rota up and running:

```bash
# 1. Clone the repository
git clone https://github.com/zaius/rota.git
cd rota

# 2. Create your environment file
cp .env.example .env
# For local development the defaults work as-is.

# 3. Start all services
docker compose up -d

# 4. Check service status
docker compose ps
```

**Access the services:**

- 🌐 **Dashboard + API**: http://localhost:8001
- 🔄 **Proxy**: http://localhost:8000

**Default credentials for dashboard:**

- Username: `admin`
- Password: `admin`

### Configuration

All settings are controlled through a single `.env` file (see `.env.example` for all options with descriptions):

| Variable | Default | Description |
|---|---|---|
| `PROXY_PORT` | `8000` | Host port for the proxy server |
| `API_PORT` | `8001` | Host port for the REST API + dashboard UI |
| `ROTA_ADMIN_USER` | `admin` | Initial dashboard username (seeded once) |
| `ROTA_ADMIN_PASSWORD` | `admin` | Initial dashboard password (seeded once, min 6 chars) |
| `DB_PASSWORD` | `rota_password` | Database password |
| `EVENT_STORE` | `postgres` | Backend for request and tunnel history: `postgres` or `clickhouse` |
| `CLICKHOUSE_PASSWORD` | `rota_password` | ClickHouse password (when `EVENT_STORE=clickhouse`) |
| `LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `METRICS_ENABLED` | `true` | Prometheus `/metrics` endpoint + optional OTLP push — see [Metrics & Observability](#-metrics--observability) |
| `METRICS_BEARER_TOKEN` | *(empty)* | When set, `/metrics` requires `Authorization: Bearer <token>` |
| `AUTH_IP_MAX_ATTEMPTS` | `10` | Failed login attempts before an IP is blocked |
| `AUTH_IP_WINDOW_MINUTES` | `10` | Sliding window (minutes) to count per-IP failures |
| `AUTH_IP_BLOCK_MINUTES` | `30` | How long a blocked IP cannot attempt login |
| `AUTH_GLOBAL_MAX_PER_MINUTE` | `1000` | Max total login attempts/min across all IPs before global lockout |
| `AUTH_GLOBAL_LOCKOUT_MINUTES` | `1` | Duration of global login lockout |

> **Note**: `ROTA_ADMIN_USER` and `ROTA_ADMIN_PASSWORD` are only used when the database is empty (first start). After that, use the **Settings → Admin Account** page to change credentials.

### Production Deployment

For production, set at minimum:

```bash
# .env
DB_PASSWORD=a-strong-random-password
ROTA_ADMIN_PASSWORD=a-strong-password
JWT_SECRET=a-stable-random-secret  # so dashboard sessions survive restarts
```

Start with `docker compose up -d`; the Compose file already sets `restart: unless-stopped`.

### Using Docker

Prebuilt multi-arch images (linux/amd64 + linux/arm64) are published to the
GitHub Container Registry:

- `ghcr.io/zaius/rota` — all-in-one image: Go proxy (`:8000`) + REST API and dashboard UI (`:8001`)

The dashboard is a static build served by the Go server on the API port — same
origin as the API, so there is no separate frontend container and no API URL to
configure.

```bash
docker run -d \
  --name rota \
  -p 8000:8000 \
  -p 8001:8001 \
  -e DB_HOST=your-db-host \
  -e DB_USER=rota \
  -e DB_PASSWORD=your-password \
  ghcr.io/zaius/rota:latest
```

Or run the whole stack (rota + TimescaleDB) with `docker compose up -d`.

### From Source

```bash
# Prerequisites: Go 1.25.3+, Node.js 20+, pnpm, PostgreSQL 14+ (TimescaleDB optional)

# Clone the repository
git clone https://github.com/zaius/rota.git
cd rota

# Configure your environment (DB connection etc.)
cp .env.example .env

# Start the core (proxy :8000, API :8001). It reads plain environment
# variables, so export the ones from your .env first:
set -a; source .env; set +a
cd core
go run ./cmd/server/main.go

# Start the dashboard dev server (in a new terminal)
cd dashboard
pnpm install
pnpm run dev  # http://localhost:3000, proxies API calls to the core on :8001
```

### Testing the Proxy

The proxy only serves authenticated proxy users — create one first in the dashboard (**Proxy Users → Add User**), then:

```bash
# Route traffic through Rota with your proxy-user credentials
curl -x http://myuser:mypassword@localhost:8000 https://api.ipify.org?format=json

# Using environment variables
export HTTP_PROXY=http://myuser:mypassword@localhost:8000
export HTTPS_PROXY=http://myuser:mypassword@localhost:8000
curl https://api.ipify.org?format=json
```

---

## 📚 API Documentation

The API is served at `http://localhost:8001/api/v1`. See [API Authentication](#-api-authentication) for credentials and [route definitions](core/internal/api/server.go) for available endpoints.

## 🏗️ Architecture

The Go server serves the proxy on **:8000** and the API/React dashboard on **:8001**. PostgreSQL stores configuration; request and tunnel history use PostgreSQL or optional ClickHouse. Proxy requests follow each user's main and fallback pools.

### Rotation Strategies

Rotation is configured per pool via `rotation_method`:

- **`roundrobin`**: Cycle through the pool's proxies in order
- **`random`**: Pick a random proxy from the pool each request
- **`stick`**: Hold one proxy for `stick_count` requests, then advance
- **`session`**: Hold one proxy per **client session** until released, idle past `session_ttl_minutes`, or invalidated — see [Session Stickiness](#-session-stickiness--proxy-invalidation)

---

## 🗂️ Proxy Sources & Pools

### How Proxy Sources work

1. Go to **Proxy Sources** in the dashboard
2. Add a URL pointing to a plain-text proxy list (one `ip:port` per line)
3. Choose the protocol and refresh interval
4. Click **Fetch Now** or wait for the scheduler

Each import upserts proxies, enriches new entries with GeoIP data, and re-syncs automatic pools.

### Geo Distribution & Pools

After proxies are geolocated, open the **Proxy Pools → Geo Distribution** tab:

- Browse all proxy-holding countries; click a country to expand cities
- Check individual countries or cities; mix them freely
- Click **Create Pool from selection** — the pool is created and filled instantly

Pools also support **ISP filters** (substring match, OR logic) and **tag filters** (AND logic — proxy must carry all specified tags). Combine geo + ISP + tags in any combination.

#### Pool Sync Modes

| Mode | Behaviour |
|------|-----------|
| `auto` | Pool membership is rebuilt automatically after every proxy import or geo-enrichment |
| `manual` | Membership only changes when you press **Sync** — useful for curated pools |

#### Exporting a Pool

```bash
# Plain text — one protocol://ip:port per line
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8001/api/v1/pools/{id}/export?format=txt" -o pool.txt

# CSV — with status, geo, ISP, success rate
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8001/api/v1/pools/{id}/export?format=csv" -o pool.csv
```

#### Webhook Alerts

Add an alert rule to a pool to be notified when the active proxy count drops below a threshold:

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  "http://localhost:8001/api/v1/pools/{id}/alert-rules" \
  -d '{
    "enabled": true,
    "min_active_proxies": 10,
    "webhook_url": "https://hooks.slack.com/...",
    "cooldown_minutes": 30
  }'
```

Payload sent to the webhook:
```json
{
  "event": "pool.degraded",
  "pool_id": 1,
  "pool_name": "US Residential",
  "active_proxies": 3,
  "total_proxies": 50,
  "threshold": 10,
  "fired_at": "2026-04-02T04:30:00Z"
}
```

### Per-User Routing

1. Create pools for each location/use-case
2. Go to **Proxy Users**, click **Add User**
3. Set a main pool and optional fallback pools (in priority order)
4. Configure max retries across the chain

Users connect as:
```
http://username:password@your-proxy-host:8000
```

If the main pool has no live IPs the request automatically cascades to the next fallback pool.

---

## Custom HTTP Status Codes

Rota uses these custom codes on the **proxy listener**. Match the number and `X-Rota-Error`; reason phrases may vary.

| Code | Meaning | Action |
| --- | --- | --- |
| **592** | Forwarding/tunnel failure; reason in `X-Rota-Error` below. | Inspect the reason. The target may have received the request; retry only if safe to repeat. |
| **593** | `no_proxy_available`: eligible proxies are empty, reserved, or on cooldown. | Wait `Retry-After` seconds (currently 5), then retry with the same session/scope. |
| **594** | `proxy_rate_limited`: Rota's per-IP limiter rejected the request. | Reduce the rate and wait `Retry-After` seconds. |

`593` and `594` are returned before forwarding. `Retry-After` is a polling delay, not a guarantee of availability.

| `592` reason | Meaning |
| --- | --- |
| `proxy_connect_failed` | Proxy DNS/TCP connection failed. |
| `proxy_handshake_failed` | Sending CONNECT or reading/parsing its reply failed. |
| `proxy_connect_rejected` | CONNECT target has no IPv4 address (NXDOMAIN/NODATA), or the upstream returned a 4xx/5xx CONNECT response other than `407`. No pool retry or proxy-health strike. |
| `upstream_proxy_auth_failed` | Upstream HTTP proxy returned `407` during CONNECT or HTTP forwarding; check its stored credentials. Counts against proxy health and retries another proxy. |
| `upstream_timeout` | Connection, tunnel, or request timed out. |
| `client_request_aborted` | The HTTP client canceled the request or its request context expired. No pool retry or proxy-health strike. |
| `proxy_configuration_error` | Invalid proxy URL or unsupported transport/protocol. |
| `upstream_request_failed` | Other forwarding failures, including unclassified SOCKS errors. |

Rota cannot always identify whether the proxy or target caused a failure. Ordinary upstream statuses and bodies pass through, including `429`, `502`, and `503`; upstream HTTP proxy `407` is mapped to `592`. Upstream `X-Rota-Error` headers are removed from forwarded/inspected responses to prevent false attribution.

Before selecting a proxy for CONNECT, Rota checks the target for IPv4 addresses using its local resolver, with a five-second lookup limit (or the client's earlier deadline). IPv6-only targets, NXDOMAIN, and NODATA return `592` without consuming an upstream attempt or session reservation. Other resolver errors, including SERVFAIL and lookup timeouts, allow the upstream to resolve the original hostname. A canceled or expired client request stops before selection.

An upstream CONNECT target rejection records one failed request for that attempt but leaves proxy health unchanged and stops retries. Its persisted `target_failure` flag excludes it from per-proxy success-rate rollups and low-success cleanup; traffic history and charts still include the failed request. Existing records retain their previous classification. TCP dial, CONNECT handshake, and upstream authentication failures still count against the proxy and try the next one.

HTTP forwarding also stops when the client cancels or its request context expires. An in-flight abort records one failed attempt with `target_failure = true` and a `client request aborted` error; it neither adds nor resets a proxy strike and is excluded from proxy reliability statistics and cleanup. Cancellation before proxy selection records no attempt. An upstream timeout while the client context is still active remains a proxy failure and retries normally.

**Standard codes:** Client proxy authentication retains `407` with `Proxy-Authenticate` and reason `proxy_auth_required` or `invalid_tls_profile`. Internal proxy/authentication failures use `500` with `rota_internal_error`. The directly addressed control API uses standard codes:

| API case | Result |
| --- | --- |
| Malformed session request or missing `token` | `400` |
| Invalidate an unknown, expired, or inaccessible session | `404` |
| Release an unknown/already-released session | `200`, `count: 0` |
| Explicit release from an unassigned pool | `403` |
| Invalid credentials / auth rate limit | `401` / `429` (with `Retry-After`) |
| Internal failure / source-fetch failure / unavailable service | `500` / `502` / `503` |

**HTTPS:** Errors before tunnel establishment appear on the CONNECT response. After CONNECT succeeds, opaque-tunnel/TLS failures close the connection; Rota cannot inject HTTP into encrypted traffic. TLS inspection can return `592` for failed HTTP forwarding. Errors after response headers are sent cannot change the status.

## 📌 Session Stickiness & Proxy Invalidation

### Session-based rotation

Set the pool's `rotation_method` to `session`, then include a token in the proxy username:

```bash
# Reuse job42's proxy for this target
curl -x "http://alice-session-job42:password@localhost:8000" https://example.com

# Use a shared scope to prevent overlap across different hostnames
curl -x "http://alice-session-job42-scope-shopping:password@localhost:8000" https://www.example.com
curl -x "http://alice-session-job43-scope-shopping:password@localhost:8000" https://api.example.com
```

Each `(user, pool, token, scope)` has a sticky binding. Proxies are exclusive within a scope across users and pools in one Rota process. The default scope is the lowercase target hostname without a port or trailing dot; subdomains are separate scopes. Custom scopes use the same normalization. Sessions sharing `shopping` above receive different proxies; different scopes may reuse them.

- **No/empty session token:** round-robin among unreserved proxies; no sticky reservation.
- **No/empty scope:** use the target hostname.
- **TLS profile override:** put it last: `alice-session-job42-scope-shopping-profile-ios`.
- **Lifetime:** release explicitly, expire after `session_ttl_minutes` idle (default 10), or rebind when the proxy becomes unavailable. Bindings reset on restart and do not coordinate across Rota processes.
- **Fallbacks:** a session main pool preserves reservations in fallback pools. Exhaustion returns [593 with Retry-After](#custom-http-status-codes).

Inspect bindings with `GET /api/v1/sessions`. Release with `POST /api/v1/sessions/release` and `{"token":"job42"}`. Release and invalidation accept optional `pool_id` and `scope` filters; admins may also filter by `username`. Proxy users can only control their own bindings in assigned pools.

### Knowing which proxy served a request

`X-Rota-Proxy-Id` identifies the serving proxy. For HTTPS, read it from the CONNECT response with `curl -v`; use the ID to invalidate that proxy.

### Invalidating a proxy mid-session

When you detect a proxy is rate-limited (or otherwise bad) while using it, pull it out of rotation immediately:

```bash
# Cooldown for a caller-selected duration (defaults to 30 minutes if omitted)
curl -X POST "http://localhost:8001/api/v1/proxies/123/invalidate" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"minutes": 30, "reason": "429 from target"}'

# Put it back early
curl -X POST "http://localhost:8001/api/v1/proxies/123/reactivate" \
  -H "Authorization: Bearer $TOKEN"
```

Proxy-ID invalidation without `domain` immediately removes the proxy from all rotation and drops its bindings. It returns when the cooldown expires. Pass `domain` (for example `"example.com"`) to cool it only for that domain and its subdomains. `minutes` must be a positive integer; zero and negative durations are rejected. The dashboard also provides **Invalidate / Reactivate** actions.

### Invalidating by session token

When a session-bound client only knows its own session token — not the proxy ID behind it — invalidate through the session instead:

```bash
curl -X POST "http://localhost:8001/api/v1/sessions/invalidate" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"token": "job42", "scope": "example.com", "minutes": 15, "reason": "429 from target"}'
```

**Session invalidation defaults to the reservation's scope.** The bound proxy goes on cooldown only within that exact scope and the session rebinds on its next request. Other scopes keep their bindings and can continue using the same proxy. A default hostname scope such as `example.com` is separate from `api.example.com`; a custom service scope such as `shopping` applies across all hostnames using that scope. Cooldowns apply across pools and users sharing the scope, including non-session rotation, and persist across restarts.

The caller controls the **base duration** with `minutes` (positive integer, default **30**). Consecutive session invalidations for the same proxy and scope use **1×, 2×, then 4×** that duration. The **fourth invalidation** flags the proxy as invalid and excludes it from that scope until an admin reactivates it. For example, sending `minutes: 360` each time gives **6h → 12h → 24h → excluded**. This also applies when session invalidation explicitly supplies a `domain`. Counts belong to the proxy and scope/domain, survive cooldown expiry, session rebinding, and restarts, and do not affect the proxy's availability for unrelated targets.

Recovery is inferred from the client's lack of invalidation, independent of HTTP status codes. The first session request using that proxy after its cooldown starts a grace period equal to the serving session's `session_ttl_minutes`. An invalidation during that period advances the streak; once that period passes without invalidation, the streak resets. Further requests do not extend the grace period. **No resumed session traffic means no reset.** This works for opaque HTTPS tunnels too; it requires no success-reporting API call. A late invalidation after the grace period starts again at the base duration.

Optional `scope`, `pool_id`, and admin-only `username` filters select bindings. Without a `scope` filter, every matching binding is invalidated within its own scope, including when one proxy is bound in several scopes. Responses include `failure_count` and `invalid`; a permanent exclusion returns `invalid: true` and `cooldown_until: null` in the invalidation response.

Two explicit overrides are available:

- `"domain": "example.com"` cools each matched proxy for that domain and its subdomains, regardless of reservation scope. Include `scope` to select a particular binding when the token is used in multiple scopes.
- `"global": true` cools each matched proxy across all targets and drops all of its bindings. `global` and `domain` cannot be combined.

Inspect active cooldowns and permanent exclusions with admin-authenticated `GET /api/v1/proxies/scope-cooldowns` or `GET /api/v1/proxies/domain-cooldowns`. In these lists, `invalid: true` means the exclusion has no expiry, regardless of the stored `cooldown_until` timestamp. Reactivate an exact scope with `POST /api/v1/proxies/{id}/reactivate` and `{"scope":"shopping"}`; use `{"domain":"example.com"}` for an explicit domain exclusion, or omit both to clear all cooldowns and exclusions for that proxy. Reactivation also clears the failure history. Global session invalidation (`global: true`) and direct proxy-ID invalidation retain their fixed-duration behavior; only admin reactivation lifts a permanent scope/domain exclusion.

**Behavior change:** session invalidation without `domain` used to invalidate globally; callers that need that behavior must now send `global: true`. An omitted duration now means 30 minutes (the previous implementation used 24 hours despite the API comment saying 30). `minutes: 0` no longer selects that legacy 24-hour fallback.

### Invalidating with proxy-user credentials

The scraping client usually holds proxy-user credentials, not an admin JWT. Three control endpoints therefore also accept **HTTP Basic auth with proxy-user credentials**:

- `POST /api/v1/proxies/{id}/invalidate`
- `POST /api/v1/sessions/invalidate`
- `POST /api/v1/sessions/release`

```bash
# Same credentials the client already uses on the proxy port
curl -X POST "http://localhost:8001/api/v1/sessions/invalidate" \
  -u "myuser:mypassword" \
  -H "Content-Type: application/json" \
  -d '{"token": "job42", "minutes": 30}'
```

Proxy-user calls are scoped to the user's own pools: only proxies that belong to the user's main/fallback pools can be invalidated, and session operations only match that user's own bindings in those pools. The endpoints share the same brute-force protection as the login endpoint. Reactivation stays admin-only. Temporary cooldowns expire automatically; permanent exclusions after repeated session invalidations require admin reactivation.

### Per-domain statistics

The dashboard's **Traffic by Domain** table follows the traffic time range, refreshes every 30 seconds, and can filter to a domain and its subdomains. It shows visible HTTP requests, success rate, failures, 429 responses, p50/p95 latency, completed HTTPS tunnels, tunnel errors, and bytes sent/received.

The same stats are available with an admin JWT:

```bash
curl "http://localhost:8001/api/v1/dashboard/domains?range=24h&domain=example.com&limit=100" \
  -H "Authorization: Bearer $TOKEN"
```

`range` accepts `1h`, `6h`, `24h` (default), `7d`, or `30d`. Omit `domain` for all hosts. `limit` is 1–1000 (default 100); rows are ranked by requests plus completed tunnels, then hostname. Each hostname has its own row; filtering to a parent includes subdomain rows without merging them.

Responses include `requests`, `successes`, `failures`, `rate_limited`, `avg_response_time`, `p50_ms`, `p95_ms`, `tunnels`, `tunnel_errors`, `bytes_up`, and `bytes_down` per domain. Latencies are milliseconds and cover successful requests only. Request outcomes inside opaque HTTPS tunnels are unknown; tunnels and their bytes are counted separately at close time. Historical events without a recorded domain are excluded. Both PostgreSQL and ClickHouse event stores support these queries.

---

## 📊 HTTPS Traffic Accounting

### Why HTTPS request counts look low

Without inspection, Rota counts HTTPS CONNECT tunnels, which can each carry many requests through keep-alive or HTTP/2. Plain HTTP requests are counted individually.

### What is always recorded

Every tunnel writes a record when it closes, whether or not inspection is enabled:

| Field | Meaning |
|-------|---------|
| `bytes_up` / `bytes_down` | Wire bytes each way — real volume even when the payload is opaque |
| `duration_ms` | How long the tunnel lived |
| `host` / `domain` | The CONNECT target |
| `proxy_id`, `pool_id`, `username` | Which proxy, pool and user served it |

The dashboard turns these into **Open Tunnels**, **Tunnels (24h)** and **Tunnel Data (24h)**, plus mean concurrency — the number that distinguishes "3 short tunnels" from "3 tunnels held open all day moving 2 GB".

### Optional: inspecting HTTPS requests

To count individual requests inside tunnels, Rota can terminate TLS, record each request, and re-encrypt to the target. This is **off by default** and requires two independent opt-ins.

**1. Configure a CA on the server** (makes interception possible, never automatic):

```bash
openssl ecparam -genkey -name prime256v1 -out ca.key
openssl req -x509 -new -key ca.key -sha256 -days 825 -out ca.crt \
  -subj "/CN=Rota Inspection CA" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign"
```

```bash
TLS_INSPECT_CA_CERT=/etc/rota/ca.crt
TLS_INSPECT_CA_KEY=/etc/rota/ca.key
# Never intercepted (certificate pinning, or targets that must see a clean handshake)
TLS_INSPECT_BYPASS_DOMAINS=accounts.google.com,api.pinned-service.com
```

**2. Enable it per proxy user** with **Inspect HTTPS** or `inspect_tls` via the API. Both the CA and user opt-in are required.

Once on, each request inside the tunnel produces a normal request event with its method, URL, latency and **status code** — which is what makes blocks visible. A `429` or a `403` block page is an answer, not a transport failure, so it still counts as a successful attempt (matching the plain-HTTP path); query `proxy_requests.status_code` to see blocking, rather than the success rate.

> **Before enabling, understand the trade-offs:**
> - The client must **trust the CA**, or every intercepted request fails its certificate check.
> - **The target sees Rota's handshake, not the client's.** Which handshake that is depends on the user's TLS fingerprint profile — see below. The default is Go's, which is recognizable.
> - **Certificate-pinning targets will fail** regardless of trust. Add them to `TLS_INSPECT_BYPASS_DOMAINS`.
> - The CA key can mint a certificate for **any** host. Treat it like any other signing key.

### TLS fingerprint profiles

With HTTPS inspection enabled, profiles control Rota's TLS ClientHello and HTTP/2 settings, header order, and priority frames.

| Profile | Presents |
|---------|----------|
| `go` (default) | Go's stdlib TLS, HTTP/1.1 only — no impersonation |
| `ios` | Safari on iOS 26 (iPhone) |
| `ios-18` | Safari on iOS 18.5 (iPhone) |
| `android` | Chrome on Android (phone browser) |
| `android-okhttp` | OkHttp 4 on Android 13 — the stack behind most native Android apps |
| `chrome` | Chrome 146 desktop |
| `firefox` | Firefox 148 desktop |

**Set a default per user** with the **TLS fingerprint** dropdown, or `tls_profile` via the API.

**Override per connection** with a `-profile-<name>` suffix on the proxy username, which composes with the existing session marker:

```bash
# Use the user's configured default
curl -x http://alice:pass@localhost:8000 https://example.com

# Override for this connection
curl -x http://alice-profile-ios:pass@localhost:8000 https://example.com

# Sticky session and a fingerprint together — profile goes last
curl -x http://alice-session-abc123-profile-android:pass@localhost:8000 https://example.com
```

Unknown profile names return `407` with `X-Rota-Error: invalid_tls_profile`.

Profiles reorder headers but do not change their values, including `User-Agent`; configure the client to match. TCP/IP fingerprints come from the upstream proxy, and captured TLS/HTTP profiles can become outdated.

---

## 📡 Metrics & Observability

Rota instruments itself once with OpenTelemetry and exports through two paths — use either or both:

- **Pull (default)**: `GET http://<host>:8001/metrics` serves Prometheus exposition format. Zero configuration; point Prometheus, SigNoz's collector, Grafana Alloy, or any Prometheus-compatible scraper at it.
- **Push (opt-in)**: set the standard `OTEL_EXPORTER_OTLP_*` env vars and Rota pushes the same metrics over OTLP — no scraper or collector required.

### What's exported

| Metric | Type | Labels |
|---|---|---|
| `rota_proxy_requests_total` | counter | `pool`, `user`, `outcome`, `status_class` — every upstream attempt (retries count separately) |
| `rota_proxy_request_duration_seconds` | histogram | `pool`, `outcome` |
| `rota_proxy_tunnels_total` / `rota_proxy_tunnel_duration_seconds` | counter / histogram | completed CONNECT tunnels by `pool`, `user`, `outcome` |
| `rota_proxy_tunnel_io_bytes_total` | counter | tunnel volume by `direction` (`up`/`down`), `pool`, `user` |
| `rota_proxy_open_tunnels` | gauge | CONNECT tunnels live right now |
| `rota_proxy_sessions_active` | gauge | live sticky-session bindings |
| `rota_proxy_domain_cooldowns_active` | gauge | active domain-scoped cooldowns |
| `rota_proxy_auth_rejections_total` | counter | 407s by `reason` |
| `rota_proxy_ratelimit_rejections_total` | counter | 594s from the proxy's per-IP limiter |
| `rota_proxies` | gauge | fleet size by `status` |
| `rota_pool_proxies` | gauge | per-pool proxy counts by `pool`, `status` |
| `rota_proxy_users` | gauge | configured proxy users by `enabled` |
| `rota_healthcheck_checks_total` / `rota_healthcheck_duration_seconds` | counter / histogram | health-check probes by `outcome` |
| `rota_source_fetches_total` / `rota_source_proxies_imported_total` | counter | source list fetches and new proxies imported |
| `rota_pool_alerts_total` | counter | alert webhooks by delivery `outcome` |
| `rota_api_requests_total` / `rota_api_request_duration_seconds` | counter / histogram | management API traffic by `route`, `method`, `status` |
| `go_*` / `process_*` | various | Go runtime: memory, GC, goroutines |

Labels stay bounded by design: pools and proxy users are labels, individual proxy IDs and target domains are not — per-proxy and per-domain analytics live in the event store and dashboard instead.

### Scraping (Prometheus, SigNoz collector, ...)

```yaml
# prometheus.yml — or the prometheus receiver in SigNoz's otel-collector config
scrape_configs:
  - job_name: rota
    static_configs:
      - targets: ["rota:8001"]
    # Only needed when METRICS_BEARER_TOKEN is set:
    authorization:
      credentials: <token>
```

### Pushing over OTLP (SigNoz direct, no collector)

```bash
# .env — SigNoz Cloud
OTEL_EXPORTER_OTLP_ENDPOINT=https://ingest.<region>.signoz.cloud:443
OTEL_EXPORTER_OTLP_HEADERS=signoz-ingestion-key=<your-key>

# .env — self-hosted SigNoz (or any OTLP backend)
OTEL_EXPORTER_OTLP_ENDPOINT=http://signoz-otel-collector:4318
```

All standard OpenTelemetry variables are honored (`OTEL_EXPORTER_OTLP_PROTOCOL` for `grpc` vs `http/protobuf`, `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, ...); Rota adds nothing vendor-specific. Set `METRICS_ENABLED=false` to disable the pipeline entirely.

---

## 🔐 API Authentication

Management endpoints require an admin JWT. The three [client-control endpoints](#invalidating-with-proxy-user-credentials) also accept proxy-user Basic credentials.

```bash
# Login
TOKEN=$(curl -s -X POST http://localhost:8001/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"yourpassword"}' | jq -r '.token')

# Use token
curl -H "Authorization: Bearer $TOKEN" http://localhost:8001/api/v1/proxies
```

Public endpoints: `GET /health`, `HEAD /health`, and `POST /api/v1/auth/login`. `/metrics` is available when enabled and uses the optional `METRICS_BEARER_TOKEN`.

### Brute-Force Protection

The login endpoint has two independent rate-limit mechanisms:

| Mechanism | Trigger | Response |
|-----------|---------|----------|
| **Per-IP block** | ≥ `AUTH_IP_MAX_ATTEMPTS` failed attempts from one IP within `AUTH_IP_WINDOW_MINUTES` minutes | `429` — IP blocked for `AUTH_IP_BLOCK_MINUTES` minutes |
| **Global lockout** | ≥ `AUTH_GLOBAL_MAX_PER_MINUTE` total attempts per minute across all IPs | `429` — login disabled for everyone for `AUTH_GLOBAL_LOCKOUT_MINUTES` minute(s) |

Both responses include a `Retry-After` header. All thresholds are configurable via `.env`.

The dashboard automatically redirects to the login page with a *"Session expired"* message when a `401` response is received.

---

## 🤝 Contributing

Open a focused pull request with tests and documentation for the change. Follow the existing code style and run:

```bash
# From the repository root
(cd core && go test ./...)
(cd dashboard && pnpm install && pnpm build)
```

## 📝 License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

---

<div align="center">
  <p>
    <sub>Built with ❤️ by <a href="https://github.com/alpkeskin">Alp Keskin</a></sub>
  </p>
  <p>
    <sub>⭐ Star this repository if you find it useful!</sub>
  </p>
</div>
