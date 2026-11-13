<div align="center" style="margin-bottom: 20px;">
  <img src="static/rota_logo.png" alt="rota" width="100px">
  <h1 align="center">
  Rota - Proxy Rotation Platform
  </h1>
</div>

<p align="center">
<a href="https://opensource.org/licenses/Apache-2.0"><img src="https://img.shields.io/badge/License-Apache%202.0-blue.svg"></a>
<a href="https://golang.org"><img src="https://img.shields.io/badge/Go-1.27.1-00ADD8?logo=go"></a>
<a href="https://react.dev"><img src="https://img.shields.io/badge/React-19-61DAFB?logo=react"></a>
<a href="https://github.com/zaius/rota/releases"><img src="https://img.shields.io/github/release/zaius/rota"></a>
<a href="https://github.com/zaius/rota/actions"><img src="https://img.shields.io/github/actions/workflow/status/zaius/rota/release.yaml"></a>
</p>

![Rota Dashboard](static/dashboard.png)

## 🎯 Overview

**Rota** manages rotating proxies through a Go server and React dashboard, with per-request options, sticky sessions, health checks, and traffic analytics. It is meant for trusted clients on a private network: one shared password guards everything.

## ✨ Key Features

- **Proxy routing:** HTTP and SOCKS proxies, least-recently-used or random rotation, country and tag filters chosen per request, and retries.
- **Sources:** Scheduled imports, GeoIP enrichment, tags, and exports.
- **Sessions:** Exclusive reservations per target or custom scope, idle expiry, and explicit release/invalidation.
- **Monitoring:** Scheduled health checks and request/tunnel history.
- **HTTPS inspection:** Optional per-request TLS interception with configurable TLS/HTTP fingerprint profiles.
- **Deployment:** Docker Compose, PostgreSQL, file-based Parquet event storage, and Prometheus/OTLP metrics.

---

## 🚀 Quick Start

### Using Docker Compose (Recommended)

The fastest way to get Rota up and running:

```bash
# 1. Clone the repository
git clone https://github.com/zaius/rota.git
cd rota

# 2. Create your environment file and set ROTA_PASSWORD in it
cp .env.example .env

# 3. Start all services
docker compose up -d

# 4. Check service status
docker compose ps
```

**Access the services:**

- 🌐 **Dashboard + API**: http://localhost:8001
- 🔄 **Proxy**: http://localhost:8000

Sign in to the dashboard with `ROTA_PASSWORD`.

### Configuration

All settings are controlled through a single `.env` file (see `.env.example` for all options with descriptions):

| Variable | Default | Description |
|---|---|---|
| `PROXY_PORT` | `8000` | Host port for the proxy server |
| `API_PORT` | `8001` | Host port for the REST API + dashboard UI |
| `ROTA_PASSWORD` | *(required)* | Shared password for the proxy port, the API and the dashboard; Rota refuses to start without it |
| `DB_PASSWORD` | `rota_password` | Database password |
| `EVENT_STORE_DIR` | `data/events` | Directory for request and tunnel history |
| `EVENT_STORE_INSTANCE` | `rota` | Name in this instance's history file names; keep it fixed |
| `EVENT_STORE_SYNC` | `false` | Make every history append wait for the disk |
| `LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `METRICS_ENABLED` | `true` | Prometheus `/metrics` endpoint + optional OTLP push — see [Metrics & Observability](#-metrics--observability) |
| `METRICS_BEARER_TOKEN` | *(empty)* | When set, `/metrics` requires `Authorization: Bearer <token>` |

### Production Deployment

For production, set at minimum:

```bash
# .env
DB_PASSWORD=a-strong-random-password
ROTA_PASSWORD=a-strong-password
```

Rota has no accounts and no login rate limiting, so keep both ports on a private network or VPN.

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
  -e ROTA_PASSWORD=your-rota-password \
  ghcr.io/zaius/rota:latest
```

Or run the whole stack (rota + PostgreSQL) with `docker compose up -d`.

### From Source

```bash
# Prerequisites: Go 1.27.1+, Node.js 22+, pnpm, PostgreSQL 14+

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

### Standalone Binary (no Docker)

Every release attaches archives for Linux (amd64, arm64), macOS (arm64, amd64) and Windows (amd64), with `checksums.txt`. Each holds the `rota` binary and the dashboard in `web/`, which the binary serves on the API port. You still need a PostgreSQL 14+ database.

```bash
# Unpack the archive for your platform, then configure it
cp .env.example .env   # set DB_HOST, DB_USER, DB_PASSWORD, DB_NAME, ROTA_PASSWORD

# The binary reads .env from its working directory; real environment variables win
./rota                 # Windows: rota.exe
```

To build the archives yourself, run `scripts/release-binaries.sh <version>`; they land in `dist/`.

### Testing the Proxy

The proxy password is `ROTA_PASSWORD`. The username carries [request options](#-proxy-username-options) and may be a bare client label:

```bash
curl -x http://scraper:$ROTA_PASSWORD@localhost:8000 https://api.ipify.org?format=json

# Using environment variables
export HTTP_PROXY=http://scraper:$ROTA_PASSWORD@localhost:8000
export HTTPS_PROXY=http://scraper:$ROTA_PASSWORD@localhost:8000
curl https://api.ipify.org?format=json
```

---

## 📚 API Documentation

The API is served at `http://localhost:8001/api/v1`. See [API Authentication](#-api-authentication) for credentials and [route definitions](core/internal/api/server.go) for available endpoints.

## 🏗️ Architecture

The Go server serves the proxy on **:8000** and the API/React dashboard on **:8001**. PostgreSQL stores configuration; request and tunnel history live in local Parquet files. The proxy keeps one in-memory list of selectable proxies (active or idle, outside a global cooldown), refreshed from the database every 30 seconds.

Rota keeps history in `EVENT_STORE_DIR` with no extra server. Each table has a tab-separated log for the current UTC day, which Rota scans directly, and a Parquet file for each finished day, which tools like DuckDB can also read. Files are named `<day>.<instance>.log` and `<day>.<instance>.parquet` after `EVENT_STORE_INSTANCE`. Rota reads every file in the directory, including `<day>.log` and `<day>.parquet` files without an instance name, so a day can have more than one file. It converts only its own logs and unnamed ones, so keep `EVENT_STORE_INSTANCE` the same across restarts. A completed append survives a Rota crash; set `EVENT_STORE_SYNC=true` to survive power loss too, at the cost of write latency. Put the directory on persistent storage, and run one Rota process per directory.

### Rotation

Each request picks among the proxies it is eligible for: those matching its `country` and `tag` options, not yet tried by this request, not on a cooldown for the target domain or the request's scope, and not reserved by another session in that scope.

- **Default:** the least recently used eligible proxy. With one filter this is round-robin, and it stays fair when requests with different filters interleave.
- **`rotation-random`:** a uniformly random eligible proxy.
- **`session-<token>`:** hold one proxy for the token until released, idle past the session TTL, or invalidated — see [Session Stickiness](#-session-stickiness--proxy-invalidation).

A request tries up to five proxies. A proxy that fails three times in a row leaves the in-memory list until the next refresh.

## 🧩 Proxy Username Options

The proxy username carries options, not an account. Split on `-`: with an odd number of parts the first is a free-form **client label**, used only to attribute events and metrics; the rest are `key-value` pairs. An empty username means no options.

| Option | Meaning |
|---|---|
| `session-<token>` | Hold one proxy for this token until released, idle past the session TTL, or invalidated |
| `scope-<name>` | Reservation scope; defaults to the target host |
| `profile-<name>` | Intercept TLS and present this fingerprint — see [TLS fingerprint profiles](#tls-fingerprint-profiles). Without it a tunnel stays opaque |
| `country-<cc>` | Only proxies whose country code matches (case-insensitive) |
| `tag-<tag>` | Only proxies carrying this tag; repeatable, all must match |
| `rotation-random` | Pick a random eligible proxy instead of the least recently used |

```bash
# Client "scraper", sticky session abc123, German proxies tagged "residential"
curl -x "http://scraper-session-abc123-country-de-tag-residential:$ROTA_PASSWORD@localhost:8000" https://example.com
```

Parsing is strict so a typo never silently downgrades a session to plain rotation: an empty part, an unknown key, a repeated non-`tag` key or an invalid value returns `407` with `X-Rota-Error: invalid_proxy_options` and a message naming the problem. Values cannot contain `-`.

---

## 🗂️ Proxy Sources

### How Proxy Sources work

1. Go to **Proxy Sources** in the dashboard
2. Add a URL pointing to a plain-text proxy list (one `ip:port` per line)
3. Choose the protocol and refresh interval
4. Click **Fetch Now** or wait for the scheduler

Each import upserts proxies and enriches new entries with GeoIP data. Tag proxies on the **Proxies** page, one at a time or in bulk; clients select them with `tag-<tag>`, which also reaches proxies without GeoIP data, such as local or VPN proxies.

#### GeoIP Source

Locations come from the free [ip-api.com](https://ip-api.com) web service unless local MaxMind databases are configured. Local lookups avoid ip-api.com's rate limit and keep proxy addresses on your server:

| Variable | Purpose |
|---|---|
| `MAXMIND_LICENSE_KEY` | Download GeoLite2-City and GeoLite2-ASN into `data/geoip/` and keep them current (free [MaxMind account](https://www.maxmind.com/en/geolite2/signup)) |
| `MAXMIND_ACCOUNT_ID` | Optional; downloads through MaxMind's account-authenticated endpoint |
| `GEOIP_CITY_DB` / `GEOIP_ASN_DB` | Paths to existing `.mmdb` files, e.g. kept current by `geoipupdate`; a replaced file is reloaded within the hour |
| `GEOIP_UPDATE_HOURS` | Re-download age for managed databases (default `168`) |

The ASN database supplies proxies' ISP names. Until the City database loads, lookups keep using ip-api.com, as do proxies given by hostname rather than IP address.

### Health checks

Every `interval_minutes` (Settings → Health check, default 30) Rota checks every proxy, including failed ones, against the configured URL. A passing check marks a proxy active, which is how a failed proxy returns to rotation.

---

## Custom HTTP Status Codes

Rota uses these custom codes on the **proxy listener**. Match the number and `X-Rota-Error`; reason phrases may vary.

| Code | Meaning | Action |
| --- | --- | --- |
| **592** | Forwarding/tunnel failure; reason in `X-Rota-Error` below. | Inspect the reason. The target may have received the request; retry only if safe to repeat. |
| **593** | `no_proxy_available`: eligible proxies are empty, reserved, or on cooldown. | Wait `Retry-After` seconds (currently 5), then retry with the same session/scope. |
| **594** | Requested TLS inspection/profile cannot run; see [TLS fingerprint profiles](#tls-fingerprint-profiles) for `X-Rota-Error` reasons. | Correct the inspection configuration before retrying. Rota rejects the CONNECT before selecting or contacting an upstream proxy. |

Rota returns `593` before forwarding. Its `Retry-After` is a polling delay, not a guarantee of availability.

| `592` reason | Meaning |
| --- | --- |
| `proxy_connect_failed` | Proxy DNS/TCP connection failed. |
| `proxy_handshake_failed` | Sending CONNECT or reading/parsing its reply failed. |
| `target_dns_not_found` | Rota's resolver found neither an A nor an AAAA record for the CONNECT host (NXDOMAIN/NODATA). No upstream attempt or proxy-health strike. |
| `target_no_ipv4` | The CONNECT host has no IPv4 address: an IPv6 literal, an IPv6-only name, or a name with no A record whose AAAA lookup was inconclusive. No upstream attempt or proxy-health strike. |
| `target_invalid` | Malformed CONNECT authority: missing host or port, or a port outside 1–65535. No upstream attempt or proxy-health strike. |
| `upstream_connect_rejected` | The upstream proxy returned a 4xx/5xx CONNECT response other than `407`, and so did one retry through a different proxy when another was available. The target may have refused or timed out, the upstream may block the domain, or the target may block both exit IPs. `X-Rota-Upstream-Status` carries the last upstream status code. No proxy-health strike. |
| `upstream_proxy_auth_failed` | Upstream HTTP proxy returned `407` during CONNECT or HTTP forwarding; check its stored credentials. Counts against proxy health and retries another proxy. |
| `upstream_timeout` | Connection, tunnel, or request timed out. |
| `client_request_aborted` | The HTTP client canceled the request or its request context expired. No retry or proxy-health strike. |
| `proxy_configuration_error` | Invalid proxy URL or unsupported transport/protocol. |
| `upstream_request_failed` | Other forwarding failures, including unclassified SOCKS errors. |

Rota cannot always identify whether the proxy or target caused a failure. Ordinary upstream statuses and bodies pass through, including `429`, `502`, and `503`; upstream HTTP proxy `407` is mapped to `592`. Upstream `X-Rota-Error` headers are removed from forwarded/inspected responses to prevent false attribution.

Before selecting a proxy for CONNECT, Rota checks the target for IPv4 addresses using its local resolver, with a five-second limit for its lookups (or the client's earlier deadline). When the A lookup finds nothing, an AAAA lookup separates a dead name (`target_dns_not_found`) from an IPv6-only one (`target_no_ipv4`). These reasons and `target_invalid` return `592` without consuming an upstream attempt or session reservation. Other A lookup errors, including SERVFAIL and timeouts, allow the upstream to resolve the original hostname. A canceled or expired client request stops before selection.

An upstream CONNECT rejection records one failed request per attempt but leaves proxy health unchanged. Rota retries it once through a proxy the request has not tried and returns `upstream_connect_rejected` only if that proxy also rejects, or if no other proxy is available. A session that selects the retry proxy rebinds to it for that scope, the same as after any failed attempt. Each rejection's persisted `target_failure` flag excludes it from per-proxy success-rate rollups and low-success cleanup; traffic history and charts still include the failed request. Existing records retain their previous classification. TCP dial, CONNECT handshake, and upstream authentication failures still count against the proxy and try the next one.

HTTP forwarding also stops when the client cancels or its request context expires. An in-flight abort records one failed attempt with `target_failure = true` and a `client request aborted` error; it neither adds nor resets a proxy strike and is excluded from proxy reliability statistics and cleanup. Cancellation before proxy selection records no attempt. An upstream timeout while the client context is still active remains a proxy failure and retries normally.

**Standard codes:** Client proxy authentication returns `407` with `Proxy-Authenticate` and reason `proxy_auth_required` (missing or wrong password) or `invalid_proxy_options` (an unparseable username, with the problem in the body). The directly addressed control API uses standard codes:

| API case | Result |
| --- | --- |
| Malformed session request, missing `token` or invalid `scope` | `400` |
| Invalidate an unknown or expired session | `409` |
| Release an unknown/already-released session | `200`, `count: 0` |
| Missing or wrong password | `401` |
| Internal failure / source-fetch failure / unavailable service | `500` / `502` / `503` |

**HTTPS:** Errors before tunnel establishment appear on the CONNECT response. After CONNECT succeeds, opaque-tunnel/TLS failures close the connection; Rota cannot inject HTTP into encrypted traffic. TLS inspection can return `592` for failed HTTP forwarding. Errors after response headers are sent cannot change the status.

## 📌 Session Stickiness & Proxy Invalidation

### Session-based rotation

Include a `session-<token>` option in the proxy username:

```bash
# Reuse job42's proxy for this target
curl -x "http://scraper-session-job42:$ROTA_PASSWORD@localhost:8000" https://example.com

# Use a shared scope to prevent overlap across different hostnames
curl -x "http://scraper-session-job42-scope-shopping:$ROTA_PASSWORD@localhost:8000" https://www.example.com
curl -x "http://scraper-session-job43-scope-shopping:$ROTA_PASSWORD@localhost:8000" https://api.example.com
```

Each `(token, scope)` has a sticky binding; the client label plays no part. Proxies are exclusive within a scope in one Rota process. The default scope is the lowercase target hostname without a port or trailing dot; subdomains are separate scopes. Custom scopes use the same normalization. Sessions sharing `shopping` above receive different proxies; different scopes may reuse them.

- **No session token:** rotation among proxies unreserved in the scope; no sticky reservation.
- **No scope:** use the target hostname.
- **Filters:** `country` and `tag` options apply to the binding too; a bound proxy that stops matching is replaced.
- **Lifetime:** release explicitly, expire after the session TTL idle (Settings → Upstream requests, default 10 minutes), or rebind when the proxy becomes unavailable. Traffic through the binding's CONNECT tunnels counts as use, so a busy kept-alive tunnel keeps its binding. Rota closes a binding's tunnels when it expires, is released, or leaves or cools their proxy (invalidation, eviction, or rebinding), so a client reusing a kept-alive connection reconnects through selection instead of staying on the old proxy. Tunnels without a session token have no idle limit and stay open through invalidation. Bindings reset on restart and do not coordinate across Rota processes.
- **Exhaustion** returns [593 with Retry-After](#custom-http-status-codes).

Inspect bindings with `GET /api/v1/sessions`. Release with `POST /api/v1/sessions/release` and `{"token":"job42"}`. Release and invalidation accept an optional `scope` filter.

### Knowing which proxy served a request

`X-Rota-Proxy-Id` identifies the serving proxy. For HTTPS, read it from the CONNECT response with `curl -v`; use the ID to invalidate that proxy.

### Invalidating a proxy mid-session

When you detect a proxy is rate-limited (or otherwise bad) while using it, pull it out of rotation immediately:

```bash
# Cooldown for a caller-selected duration (defaults to 30 minutes if omitted)
curl -X POST "http://localhost:8001/api/v1/proxies/123/invalidate" \
  -H "Authorization: Bearer $ROTA_PASSWORD" \
  -H "Content-Type: application/json" \
  -d '{"minutes": 30, "reason": "429 from target"}'

# Put it back early
curl -X POST "http://localhost:8001/api/v1/proxies/123/reactivate" \
  -H "Authorization: Bearer $ROTA_PASSWORD"
```

Proxy-ID invalidation without `domain` immediately removes the proxy from all rotation and drops its bindings. It returns when the cooldown expires. Pass `domain` (for example `"example.com"`) to cool it only for that domain and its subdomains. `minutes` must be a positive integer; zero and negative durations are rejected. The dashboard also provides **Invalidate / Reactivate** actions.

### Invalidating by session token

When a session-bound client only knows its own session token — not the proxy ID behind it — invalidate through the session instead:

```bash
curl -X POST "http://localhost:8001/api/v1/sessions/invalidate" \
  -H "Authorization: Bearer $ROTA_PASSWORD" \
  -H "Content-Type: application/json" \
  -d '{"token": "job42", "scope": "example.com", "minutes": 15, "reason": "429 from target"}'
```

**Session invalidation defaults to the reservation's scope.** The bound proxy goes on cooldown only within that exact scope, the session's open tunnels through it close, and the session rebinds on its next request. Other scopes keep their bindings and can continue using the same proxy. A default hostname scope such as `example.com` is separate from `api.example.com`; a custom service scope such as `shopping` applies across all hostnames using that scope. Cooldowns apply to every request in the scope, including non-session rotation, and persist across restarts.

The caller controls the **base duration** with `minutes` (positive integer, default **30**). Consecutive session invalidations for the same proxy and scope use **1×, 2×, then 4×** that duration. The **fourth invalidation** flags the proxy as invalid and excludes it from that scope until it is reactivated. For example, sending `minutes: 360` each time gives **6h → 12h → 24h → excluded**. This also applies when session invalidation explicitly supplies a `domain`. Counts belong to the proxy and scope/domain, survive cooldown expiry, session rebinding, and restarts, and do not affect the proxy's availability for unrelated targets.

Recovery is inferred from the client's lack of invalidation, independent of HTTP status codes. The first session request using that proxy after its cooldown starts a grace period equal to the session TTL. An invalidation during that period advances the streak; once that period passes without invalidation, the streak resets. Further requests do not extend the grace period. **No resumed session traffic means no reset.** This works for opaque HTTPS tunnels too; it requires no success-reporting API call. A late invalidation after the grace period starts again at the base duration.

An optional `scope` filter selects bindings. Without it, every matching binding is invalidated within its own scope, including when one proxy is bound in several scopes. Responses include `failure_count` and `invalid`; a permanent exclusion returns `invalid: true` and `cooldown_until: null` in the invalidation response.

Two explicit overrides are available:

- `"domain": "example.com"` cools each matched proxy for that domain and its subdomains, regardless of reservation scope. Include `scope` to select a particular binding when the token is used in multiple scopes.
- `"global": true` cools each matched proxy across all targets and drops all of its bindings. `global` and `domain` cannot be combined.

Inspect active cooldowns and permanent exclusions with `GET /api/v1/proxies/scope-cooldowns` or `GET /api/v1/proxies/domain-cooldowns`. In these lists, `invalid: true` means the exclusion has no expiry, regardless of the stored `cooldown_until` timestamp. Reactivate an exact scope with `POST /api/v1/proxies/{id}/reactivate` and `{"scope":"shopping"}`; use `{"domain":"example.com"}` for an explicit domain exclusion, or omit both to clear all cooldowns and exclusions for that proxy. Reactivation also clears the failure history. Global session invalidation (`global: true`) and direct proxy-ID invalidation retain their fixed-duration behavior; only reactivation lifts a permanent scope/domain exclusion.

A client can call these endpoints with the same credentials it uses on the proxy port, since the API accepts `ROTA_PASSWORD` as a Basic password with any username:

```bash
curl -X POST "http://localhost:8001/api/v1/sessions/invalidate" \
  -u "scraper:$ROTA_PASSWORD" \
  -H "Content-Type: application/json" \
  -d '{"token": "job42", "minutes": 30}'
```

### Exporting working proxies

A client that connects to upstream proxies directly, rather than through Rota, can download them from `GET /api/v1/proxies/working`, one per line with their credentials, fastest first:

```bash
# All working proxies as protocol://user:pass@host:port
curl -u "scraper:$ROTA_PASSWORD" "http://localhost:8001/api/v1/proxies/working"

# At most 20 German proxies tagged "residential", as host:port:user:pass
curl -u "scraper:$ROTA_PASSWORD" "http://localhost:8001/api/v1/proxies/working?country=de&tag=residential&limit=20&format=colon"
```

`country` and `tag` (repeatable; all must match) filter like the proxy username options. By default only active proxies outside a cooldown are listed; `status=all` includes the rest.

### Per-domain statistics

The dashboard's **Traffic by Domain** table follows the traffic time range, refreshes every 30 seconds, and can filter to a domain and its subdomains. It shows visible HTTP requests, success rate, failures, 429 responses, p50/p95 latency, completed HTTPS tunnels, tunnel errors, and bytes sent/received.

The same stats are available from the API:

```bash
curl "http://localhost:8001/api/v1/dashboard/domains?range=24h&domain=example.com&limit=100" \
  -H "Authorization: Bearer $ROTA_PASSWORD"
```

`range` accepts `1h`, `6h`, `24h` (default), `7d`, or `30d`. Omit `domain` for all hosts. `limit` is 1–1000 (default 100); rows are ranked by requests plus completed tunnels, then hostname. Each hostname has its own row; filtering to a parent includes subdomain rows without merging them.

Responses include `requests`, `successes`, `failures`, `rate_limited`, `avg_response_time`, `p50_ms`, `p95_ms`, `tunnels`, `tunnel_errors`, `bytes_up`, and `bytes_down` per domain. Latencies are milliseconds and cover successful requests only. Request outcomes inside opaque HTTPS tunnels are unknown; tunnels and their bytes are counted separately at close time. Historical events without a recorded domain are excluded.

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
| `proxy_id`, `username` | Which proxy served it, and the client label from the proxy username. The column keeps its historical name; `pool_id` is always 0 |

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

Use the actual paths to the generated files. For **Docker Compose**, put these
settings in `.env` with **absolute host paths** and include the inspection overlay
to mount both files read-only at those same paths inside the container:

```bash
docker compose -f docker-compose.yml -f docker-compose.tls-inspect.yml up -d --build rota
```

The container runs as UID 1000 and must be able to read both files. Include the
overlay on subsequent Compose commands so the mounts stay configured. For a
standalone server, export the settings into its environment and restart it.

**2. Request it per connection** with a `profile-<name>` option in the proxy username. Both the CA and the profile are required.

Once on, each request inside the tunnel produces a normal request event with its method, URL, latency and **status code** — which is what makes blocks visible. A `429` or a `403` block page is an answer, not a transport failure, so it still counts as a successful attempt (matching the plain-HTTP path); look at status codes to see blocking, such as the `rate_limited` count in Traffic by Domain or the `status_code` column of the request history, rather than the success rate.

> **Before enabling, understand the trade-offs:**
> - The client must **trust the CA**, or every intercepted request fails its certificate check.
> - **The target sees Rota's handshake, not the client's.** Which handshake that is depends on the requested profile — see below. `go` is Go's, which is recognizable.
> - **Certificate-pinning targets will fail** regardless of trust. `TLS_INSPECT_BYPASS_DOMAINS` prevents inspection of those targets; inspection requests to them return `594`. Connect without a profile for opaque access.
> - The CA key can mint a certificate for **any** host. Treat it like any other signing key.

### TLS fingerprint profiles

With HTTPS inspection enabled, profiles control Rota's TLS ClientHello and HTTP/2 settings, header order, and priority frames.

| Profile | Presents |
|---------|----------|
| `go` | Go's stdlib TLS, HTTP/1.1 only — no impersonation |
| `ios` | Safari on iOS 26 (iPhone) |
| `ios18` | Safari on iOS 18.5 (iPhone) |
| `android` | Chrome on Android (phone browser) |
| `okhttp` | OkHttp 4 on Android 13 — the stack behind most native Android apps |
| `chrome` | Chrome 146 desktop |
| `firefox` | Firefox 148 desktop |

Request a profile per connection with the `profile-<name>` option, which composes with the others:

```bash
# Inspect with an iPhone fingerprint
curl --cacert ca.crt -x http://scraper-profile-ios:$ROTA_PASSWORD@localhost:8000 https://example.com

# Sticky session and a fingerprint together
curl --cacert ca.crt -x http://scraper-session-abc123-profile-android:$ROTA_PASSWORD@localhost:8000 https://example.com
```

Unknown profile names return `407` with `X-Rota-Error: invalid_proxy_options`.

**Verify inspection before testing profiles:** startup logs must include
`HTTPS interception available for requests that name a profile`. Add `-v` to a
curl command above and check that the target certificate's issuer is your
inspection CA. If you still see the origin's issuer (such as Let's Encrypt), the
tunnel is opaque: check the username's `profile` option and
`TLS_INSPECT_BYPASS_DOMAINS`. With `LOG_LEVEL=debug`, an inspected connection
logs `intercepted tunnel established` with its profile and upstream protocol.

When a requested profile cannot run, Rota rejects CONNECT with **594** before
DNS lookup, proxy selection, or session reservation. This covers every profile,
including `go`. Rota logs an **error** with the client label, target host,
profile, reason, and corrective action. The response includes an
`X-Rota-Error` code and an actionable text body:

```text
HTTP/1.1 594
X-Rota-Error: tls_inspection_ca_not_configured

TLS inspection required for profile ios but unavailable: Set TLS_INSPECT_CA_CERT and TLS_INSPECT_CA_KEY, mount readable CA files, and restart Rota
```

| `594` reason | Corrective action |
|--------|-------------------|
| `tls_inspection_ca_not_configured` | Set both `TLS_INSPECT_CA_CERT` and `TLS_INSPECT_CA_KEY`, mount readable CA files, and restart Rota. |
| `tls_inspection_bypassed` | Check `TLS_INSPECT_BYPASS_DOMAINS`; the error identifies the matching entry. |
| `tls_inspection_invalid_host` | Use a valid CONNECT target host. |

Rota never silently falls back to an opaque tunnel for these requests, including
on bypass domains. Clients without a profile keep their opaque tunnels. A `594`
carries no `Retry-After`: waiting or rotating proxies does not fix the
configuration. TLS handshake failures after CONNECT succeeds close the
connection and log the client label, host, profile, and error.

For aiohttp, load the inspection CA into the SSL context used by the session:

```python
import asyncio
import os
import ssl
import aiohttp

ROTA_PASSWORD = os.environ["ROTA_PASSWORD"]

async def main():
    context = ssl.create_default_context()
    context.load_verify_locations(cafile="ca.crt")
    async with aiohttp.ClientSession(connector=aiohttp.TCPConnector(ssl=context)) as session:
        async with session.get(
            "https://example.com",
            proxy="http://localhost:8000",
            proxy_auth=aiohttp.BasicAuth("scraper-profile-chrome", ROTA_PASSWORD),
        ) as response:
            body = await response.read()
            print(response.status, len(body))

asyncio.run(main())
```

Profiles reorder headers but do not change their values, including `User-Agent`; configure the client to match. TCP/IP fingerprints come from the upstream proxy, and captured TLS/HTTP profiles can become outdated.

---

## 📡 Metrics & Observability

Rota instruments itself once with OpenTelemetry and exports through two paths — use either or both:

- **Pull (default)**: `GET http://<host>:8001/metrics` serves Prometheus exposition format. Zero configuration; point Prometheus, SigNoz's collector, Grafana Alloy, or any Prometheus-compatible scraper at it.
- **Push (opt-in)**: set the standard `OTEL_EXPORTER_OTLP_*` env vars and Rota pushes the same metrics over OTLP — no scraper or collector required.

### What's exported

| Metric | Type | Labels |
|---|---|---|
| `rota_proxy_requests_total` | counter | `client`, `outcome`, `status_class` — every upstream attempt (retries count separately) |
| `rota_proxy_request_duration_seconds` | histogram | `outcome` |
| `rota_proxy_tunnels_total` / `rota_proxy_tunnel_duration_seconds` | counter / histogram | completed CONNECT tunnels; the counter by `client`, `outcome` |
| `rota_proxy_tunnel_io_bytes_total` | counter | tunnel volume by `direction` (`up`/`down`), `client` |
| `rota_proxy_open_tunnels` | gauge | CONNECT tunnels live right now |
| `rota_proxy_sessions_active` | gauge | live sticky-session bindings |
| `rota_proxy_domain_cooldowns_active` | gauge | active domain-scoped cooldowns |
| `rota_proxy_auth_rejections_total` | counter | 407s by `reason` |
| `rota_proxies` | gauge | fleet size by `status` |
| `rota_healthcheck_checks_total` / `rota_healthcheck_duration_seconds` | counter / histogram | health-check probes by `outcome` |
| `rota_source_fetches_total` / `rota_source_proxies_imported_total` | counter | source list fetches and new proxies imported |
| `rota_events_written_total` | counter | request/tunnel history writes by `kind` and `outcome`; failures are lost history |
| `rota_api_requests_total` / `rota_api_request_duration_seconds` | counter / histogram | management API traffic by `route`, `method`, `status` |
| `go_*` / `process_*` | various | Go runtime: memory, GC, goroutines |

Labels stay bounded by design: client labels from proxy usernames are labels, individual proxy IDs and target domains are not — per-proxy and per-domain analytics live in the event store and dashboard instead. Keep client labels to a handful of fixed names.

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

Every `/api/v1` endpoint accepts `ROTA_PASSWORD` as a Basic-auth password (any username) or as `Authorization: Bearer <password>`. A missing or wrong password returns `401`.

```bash
curl -H "Authorization: Bearer $ROTA_PASSWORD" http://localhost:8001/api/v1/proxies
curl -u "scraper:$ROTA_PASSWORD" http://localhost:8001/api/v1/sessions
```

Public endpoints: `GET /health` and `HEAD /health`. `/metrics` is available when enabled and uses the optional `METRICS_BEARER_TOKEN`.

The dashboard stores the password in the browser and sends it as the Bearer token. When the API rejects it (the password changed), the dashboard returns to the sign-in page.

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
