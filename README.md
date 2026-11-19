<img src="static/rota_logo.png" alt="" width="80" align="right">

# Rota

[![CI](https://github.com/zaius/rota/actions/workflows/ci.yaml/badge.svg)](https://github.com/zaius/rota/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/v/release/zaius/rota)](https://github.com/zaius/rota/releases)

Rota is a rotating proxy server. Clients connect to one address, and Rota forwards each request through a pool of upstream HTTP and SOCKS proxies. A Go server does the proxying and serves a React dashboard for managing the pool.

Rota is built for trusted clients on a private network. One shared password guards the proxy, the API and the dashboard. It has no accounts and no login rate limiting, so keep both ports off the public internet.

[Quick start](#quick-start) · [Documentation](#documentation) · [Using the proxy](#using-the-proxy) · [API](#api) · [Operating](#operating) · [Running without Compose](#running-without-compose) · [Development](#development)

![Rota dashboard](static/dashboard.png)

## Quick start

```bash
git clone https://github.com/zaius/rota.git && cd rota
cp .env.example .env   # set ROTA_PASSWORD
docker compose up -d
```

The proxy listens on `:8000`, and the dashboard and API on `:8001`. Sign in to the dashboard with `ROTA_PASSWORD`, add some proxies, then send a request through them:

```bash
curl -x "http://scraper:$ROTA_PASSWORD@localhost:8000" https://api.ipify.org
```

Every setting is an environment variable, described in [`.env.example`](.env.example) and parsed in [`config.go`](core/internal/config/config.go).

## Documentation

- [Sticky sessions and invalidation](docs/sessions.md): holding one proxy for a job, releasing it, and cooling down a proxy the target has blocked.
- [Proxy status codes](docs/errors.md): the `407` and `592` to `594` responses Rota sends, and what a client should do with each.
- [HTTPS inspection and TLS fingerprints](docs/tls-inspection.md): recording the requests inside HTTPS tunnels and choosing the fingerprint the target sees.

## Using the proxy

Authenticate to the proxy with `ROTA_PASSWORD` as the password. The username holds the options for the request, as `key-value` pairs joined by `-`. If it has an odd number of parts, the first is a client label that tags the request in history and metrics.

| Option | Meaning |
|---|---|
| `session-<token>` | Hold one proxy for this token ([sticky sessions](docs/sessions.md)) |
| `scope-<name>` | Reserve the session's proxy across this scope instead of the target host |
| `country-<cc>` | Only proxies in this country |
| `tag-<tag>` | Only proxies with this tag. Repeat it to require several |
| `rotation-random` | Pick a random proxy instead of the least recently used |
| `profile-<name>` | Intercept TLS and present this fingerprint ([HTTPS inspection](docs/tls-inspection.md)) |

```bash
# Client "scraper", sticky session abc123, German proxies tagged "residential"
curl -x "http://scraper-session-abc123-country-de-tag-residential:$ROTA_PASSWORD@localhost:8000" https://example.com
```

An unknown option or a bad value fails the request with `407`. Values cannot contain `-`.

A request tries up to five proxies. Rota's own failures use statuses `592` to `594` with an `X-Rota-Error` header, listed in [status codes](docs/errors.md). The `X-Rota-Proxy-Id` response header names the proxy that served a request.

## API

The API is at `http://localhost:8001/api/v1`. It accepts `ROTA_PASSWORD` as a Bearer token, or as a Basic password with any username, so a client can reuse its proxy credentials:

```bash
curl -u "scraper:$ROTA_PASSWORD" http://localhost:8001/api/v1/proxies
```

[`server.go`](core/internal/api/server.go) lists the routes. The ones clients call most:

- `POST /sessions/release` and `POST /sessions/invalidate` end a sticky session or cool down its proxy. [Sticky sessions](docs/sessions.md) covers both.
- `GET /proxies/working` exports the working proxies with their credentials, for clients that connect to them directly.

`GET /health` needs no password.

## Operating

- PostgreSQL holds the configuration. Request and tunnel history goes to Parquet files under `EVENT_STORE_DIR`, one per day, which tools like DuckDB can read. Keep that directory on persistent storage and run one Rota process per directory.
- Add proxies in the dashboard, or point a source at a URL serving one `ip:port` per line and Rota imports it on a schedule. A health check every 30 minutes returns failed proxies to rotation.
- A residential gateway is one proxy whose username holds `{session}`, such as Webshare's `p.webshare.io:80` with `user-residential-{country}-{session}`. Rota fills `{session}` with a new exit for each sticky binding, and `{country}` from the `country-` option, dropping `-{country}` without one. Give it a tag: a gateway only serves requests that name one. A failed or [invalidated](docs/sessions.md#invalidation) exit gets replaced, and only an unreachable gateway or rejected credentials count against the gateway itself.
- `GET :8001/metrics` serves Prometheus metrics, each described by its `# HELP` line. Set the standard `OTEL_EXPORTER_OTLP_*` variables to push the same metrics over OTLP.
- HTTPS passes through as opaque tunnels, so history counts tunnels and bytes for it. Opt in to [HTTPS inspection](docs/tls-inspection.md) to record each request and choose the TLS fingerprint the target sees.

## Running without Compose

Each release publishes a multi-arch image, `ghcr.io/zaius/rota:<version>`, and standalone archives for Linux, macOS and Windows. Both hold the server and the dashboard, and both need a PostgreSQL 14+ database, set through the `DB_*` variables. The standalone binary reads `.env` from its working directory.

## Development

```bash
# Core: proxy on :8000, API on :8001. Needs Go and the PostgreSQL that .env points at.
set -a; source .env; set +a
(cd core && go run ./cmd/server)

# Dashboard dev server on :3000, proxying API calls to the core. Needs Node 22 and pnpm.
(cd dashboard && pnpm install && pnpm dev)

# Checks
(cd core && go test ./...)
(cd dashboard && pnpm lint && pnpm build)
```

## Credits

Forked from [alpkeskin/rota](https://github.com/alpkeskin/rota).

Proxy countries come from the free [IP geolocation by DB-IP](https://db-ip.com) database (CC BY 4.0), which Rota downloads each month.

Licensed under [Apache 2.0](LICENSE).
