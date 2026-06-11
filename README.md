<div align="center" style="margin-bottom: 20px;">
  <img src="static/rota_logo.png" alt="rota" width="100px">
  <h1 align="center">
  Rota - Proxy Rotation Platform
  </h1>
</div>

<p align="center">
<a href="https://opensource.org/licenses/Apache-2.0"><img src="https://img.shields.io/badge/License-Apache%202.0-blue.svg"></a>
<a href="https://golang.org"><img src="https://img.shields.io/badge/Go-1.25.3-00ADD8?logo=go"></a>
<a href="https://nextjs.org"><img src="https://img.shields.io/badge/Next.js-16-000000?logo=next.js"></a>
<a href="https://www.timescale.com/"><img src="https://img.shields.io/badge/TimescaleDB-2.22-FDB515?logo=timescale"></a>
<a href="https://github.com/alpkeskin/rota/releases"><img src="https://img.shields.io/github/release/alpkeskin/rota"></a>
<a href="https://github.com/alpkeskin/rota/actions"><img src="https://img.shields.io/github/actions/workflow/status/alpkeskin/rota/release.yaml"></a>
</p>


![Khipu Screenshot](static/dashboard.png)


## 🎯 Overview

**Rota** is a modern, full-stack proxy rotation platform that combines enterprise-grade proxy management with a beautiful, real-time web dashboard. Built with performance and scalability in mind, Rota handles thousands of requests per second while providing comprehensive monitoring, analytics, and control through an intuitive interface.

Whether you're conducting web scraping operations, performing security research, load testing, or need reliable proxy management at scale, Rota delivers a complete solution with:

- **High-Performance Core**: Lightning-fast Go-based proxy server with intelligent rotation strategies
- **Real-Time Dashboard**: Modern Next.js web interface with live metrics and monitoring
- **Time-Series Analytics**: TimescaleDB-powered storage for historical analysis and insights
- **Production-Ready**: Docker-based deployment with health checks, graceful shutdown, and monitoring

---

## ✨ Key Features

### Core Proxy Server
- 🚀 **High Performance**: Handle thousands of concurrent requests with minimal latency
- 🔄 **Smart Rotation**: Multiple rotation strategies (random, round-robin, least connections, time-based, plus per-pool sticky and session-based)
- 🤖 **Automatic Management**: Real-time proxy pool monitoring with automatic unhealthy proxy removal
- 🌍 **Multi-Protocol**: Full support for HTTP, HTTPS, SOCKS4, SOCKS4A, and SOCKS5
- ✅ **Health Checking**: Built-in proxy validation to maintain a healthy pool
- 🔒 **Authentication**: Basic auth support for proxy server
- ⚡ **Rate Limiting**: Configurable rate limiting to prevent abuse
- 🔗 **Proxy Chaining**: Compatible with upstream proxies (Burp Suite, OWASP ZAP, etc.)
- ⏱️ **Configurable Timeouts**: Fine-grained control over request timeouts and retries
- 🔁 **Redirect Support**: Optional HTTP redirect following

### Proxy Sources & Auto-Import
- 📥 **Remote TXT Lists**: Add URLs pointing to `ip:port` proxy lists — fetched automatically on schedule
- 🕐 **Per-Source Interval**: Each source has its own refresh interval (in minutes)
- 🔁 **Background Scheduler**: Overdue sources are fetched automatically every minute
- 🌍 **Protocol per Source**: Assign HTTP, HTTPS, SOCKS4, SOCKS4a, or SOCKS5 to each list

### GeoIP & Geo Distribution
- 🗺️ **Automatic GeoIP**: Proxies are geolocated via [ip-api.com](http://ip-api.com) (free, no API key required)
- 🏙️ **City-Level Data**: Country, region, city, ISP, latitude, longitude per proxy
- 🔍 **Geo Explorer**: Expandable country tree with city drill-down in the dashboard
- ♻️ **Auto-Enrich**: Geo data updated automatically after every source fetch

### Proxy Pools
- 🗂️ **Named Pools**: Group proxies by any combination of countries, cities, ISPs, or custom tags
- ☑️ **Multi-Filter Builder**: Pick geo locations, ISP substrings, or proxy tags — mix freely in one pool
- 🔄 **Auto / Manual Sync**: `sync_mode: auto` rebuilds membership on every import; `manual` keeps it frozen until you trigger sync explicitly
- 🔁 **Rotation Strategies**: Per-pool `roundrobin`, `random`, `sticky` (hold N requests per IP), or `session` (hold one proxy per client session until released or idle)
- 📌 **Session Stickiness**: Pin a proxy to a client-chosen session via the proxy username (`user-session-<id>`); released explicitly, on idle TTL, or when the proxy is invalidated
- 🚫 **Manual Invalidation**: Pull a single proxy out of rotation on demand (e.g. when you detect it's rate-limited) with a cooldown, then auto-recover or reactivate it
- ⚡ **Async Health Checks**: Run health checks against any URL; progress shown in real time
- ⏱️ **Scheduled Checks**: Cron-style schedule per pool (`*/30 * * * *`)
- 📤 **Export**: Download pool proxy list as `.txt` or `.csv` (`GET /api/v1/pools/{id}/export?format=txt|csv`)
- 🔔 **Webhook Alerts**: Per-pool alert rules — fire a POST/GET webhook when active proxy count drops below threshold, with configurable cooldown

### Per-User Pool Authentication
- 👤 **Proxy Users**: Create users with bcrypt passwords, each assigned a main pool + ordered fallbacks
- 🔗 **Usage**: `http://user:pass@host:8000` — the proxy routes through the user's pool chain
- 🔄 **Automatic Failover**: If a pool has no live IPs, requests cascade to fallback pools
- 🔁 **Retry Logic**: Each retry picks a fresh proxy; failed IPs are excluded for that request
- 📊 **Full Tracking**: All requests, success rates, and response times tracked per proxy
- ⚡ **Per-User Rate Limit**: Optional `requests_per_minute` cap per user (0 = unlimited)

### Security
- 🔐 **JWT Authentication**: All API endpoints require a valid JWT token; the browser auto-redirects to login on expiry with "Session expired" message
- 🔑 **Bcrypt Admin Credentials**: Dashboard password stored as bcrypt hash in database
- 🔄 **Change Password**: Update username/password via the Settings UI (requires current password)
- 🌐 **Public endpoints only**: `GET /health` and `POST /auth/login`
- 🛡️ **Auth Brute-Force Protection**: Per-IP block after N failed attempts + global lockout when request rate exceeds threshold (all configurable via `.env`)
- 🏷️ **Proxy Tags**: Label proxies with custom tags for fine-grained pool filtering
- 🧹 **Dead Proxy Cleanup**: Configurable automatic removal of long-failed or low-quality proxies

### Web Dashboard
- 📊 **Real-Time Metrics**: Live statistics, charts, and system monitoring
- 🔄 **Proxy Management**: Add, edit, delete, and test proxies through the UI
- 📝 **Live Logs**: WebSocket-based real-time log streaming
- 💻 **System Monitoring**: CPU, memory, disk, and runtime metrics
- ⚙️ **Configuration**: Manage settings through the web interface
- 🎨 **Modern UI**: Beautiful, responsive design with dark mode support
- 📱 **Mobile-Friendly**: Fully responsive across all devices

### Data & Analytics
- 📈 **Time-Series Storage**: TimescaleDB for efficient historical data storage
- 🔍 **Request History**: Track all proxy requests with detailed metadata
- 📉 **Performance Analytics**: Analyze proxy performance over time
- 🎯 **Usage Insights**: Understand traffic patterns and proxy utilization

### DevOps & Deployment
- 🐳 **Docker-Native**: Production-ready containerized deployment
- 🔧 **Easy Configuration**: All config via `.env` — see `.env.example` for all options
- 🏥 **Health Checks**: Built-in health endpoints for monitoring
- 🛑 **Graceful Shutdown**: Clean shutdown with connection draining
- 📊 **Observability**: Structured JSON logging and metrics endpoints

---

## 🚀 Quick Start

### Using Docker Compose (Recommended)

The fastest way to get Rota up and running:

```bash
# 1. Clone the repository
git clone https://github.com/alpkeskin/rota.git
cd rota

# 2. Create your environment file
cp .env.example .env
# For local development the defaults work as-is.
# For production: set NEXT_PUBLIC_API_URL to your public API URL.

# 3. Start all services
docker compose up -d

# 4. Check service status
docker compose ps
```

**Access the services:**
- 🌐 **Dashboard**: http://localhost:3000
- 🔧 **API**: http://localhost:8001
- 🔄 **Proxy**: http://localhost:8000
- 🗄️ **Database**: localhost:5432

**Default credentials for dashboard:**
- Username: `admin`
- Password: `admin`

### Configuration

All settings are controlled through a single `.env` file (see `.env.example` for all options with descriptions):

| Variable | Default | Description |
|---|---|---|
| `NEXT_PUBLIC_API_URL` | `http://localhost:8001` | Public URL of the API — used by the browser |
| `PROXY_PORT` | `8000` | Host port for the proxy server |
| `API_PORT` | `8001` | Host port for the REST API |
| `DASHBOARD_PORT` | `3000` | Host port for the web dashboard |
| `ROTA_ADMIN_USER` | `admin` | Initial dashboard username (seeded once) |
| `ROTA_ADMIN_PASSWORD` | `admin` | Initial dashboard password (seeded once, min 6 chars) |
| `DB_PASSWORD` | `rota_password` | TimescaleDB password |
| `LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
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
NEXT_PUBLIC_API_URL=https://api.yourdomain.com
DB_PASSWORD=a-strong-random-password
ROTA_ADMIN_PASSWORD=a-strong-password
```

Then rebuild the dashboard (required when changing `NEXT_PUBLIC_API_URL`, as it is baked into the Next.js bundle at build time):

```bash
docker compose up -d --build
```

### Using Docker

Pull and run the core service:

```bash
# Pull from GitHub Container Registry
docker pull ghcr.io/alpkeskin/rota:latest

# Run with basic configuration
docker run -d \
  --name rota-core \
  -p 8000:8000 \
  -p 8001:8001 \
  -e DB_HOST=your-db-host \
  -e DB_USER=rota \
  -e DB_PASSWORD=your-password \
  ghcr.io/alpkeskin/rota:latest
```

### From Source

```bash
# Prerequisites: Go 1.25.3+, Node.js 20+, PostgreSQL 16+ with TimescaleDB

# Clone the repository
git clone https://github.com/alpkeskin/rota.git
cd rota

# Start Core
cd core
cp .env .env.local  # Configure your environment
make install
make dev

# Start Dashboard (in new terminal)
cd dashboard
npm install
cp .env.local .env.local  # Configure API URL
npm run dev
```

### Testing the Proxy

```bash
# Route traffic through Rota proxy
curl -x http://localhost:8000 https://api.ipify.org?format=json

# Per-user pool routing (after creating a Proxy User in the dashboard)
curl -x http://myuser:mypassword@localhost:8000 https://api.ipify.org?format=json

# Using environment variables
export HTTP_PROXY=http://localhost:8000
export HTTPS_PROXY=http://localhost:8000
curl https://api.ipify.org?format=json
```

---

## 📚 API Documentation

### Interactive API Documentation (Swagger)

Rota provides interactive API documentation through Swagger UI. Once the core service is running, you can access it at:

```
http://localhost:8001/docs
```

The Swagger interface allows you to:
- 📖 Browse all available API endpoints
- 🧪 Test API requests directly from your browser
- 📝 View request/response schemas
- 🔍 Explore authentication requirements

**Quick Access:**
- **Swagger UI**: http://localhost:8001/docs
- **OpenAPI Spec**: http://localhost:8001/docs/swagger.json

---

## 🏗️ Architecture

Rota is built as a modern monorepo with three main components:

```
┌─────────────────────────────────────────────────────────────┐
│                        Rota Platform                        │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐   │
│  │   Dashboard  │───▶│  Core (API)  │───▶│ TimescaleDB  │   │
│  │   Next.js    │    │     Go       │    │  PostgreSQL  │   │
│  │  Port 3000   │    │  Port 8001   │    │  Port 5432   │   │
│  └──────────────┘    └──────────────┘    └──────────────┘   │
│         │                    │                              │
│         │                    ▼                              │
│         │            ┌──────────────┐                       │
│         └───────────▶│ Proxy Server │                       │
│                      │      Go      │                       │
│                      │  Port 8000   │                       │
│                      └──────────────┘                       │
│                              │                              │
└──────────────────────────────┼──────────────────────────────┘
                               ▼
                     ┌──────────────────┐
                     │   Proxy Pool     │
                     │  (External IPs)  │
                     └──────────────────┘
```

---

### Rotation Strategies

Global strategies (legacy single-pool mode):

- **Random**: Select a random proxy for each request
- **Round Robin**: Distribute requests evenly across all proxies
- **Least Connections**: Route to the proxy with fewest active connections
- **Time-Based**: Rotate proxies at fixed intervals

Per-pool strategies (set on each pool via `rotation_method`):

- **`roundrobin`**: Cycle through the pool's proxies in order
- **`random`**: Pick a random proxy from the pool each request
- **`sticky`**: Hold one proxy for `stick_count` requests, then advance
- **`session`**: Hold one proxy per **client session** until released, idle past `session_ttl_minutes`, or invalidated — see [Session Stickiness](#-session-stickiness--proxy-invalidation)

---

## 🐳 Deployment

### Production Deployment

#### Using Docker Compose

```bash
# Production configuration
docker compose -f docker-compose.yml up -d

# Enable auto-restart
docker compose up -d --restart=unless-stopped
```

---

## 🗂️ Proxy Sources & Pools

### How Proxy Sources work

1. Go to **Proxy Sources** in the dashboard
2. Add a URL pointing to a plain-text proxy list (one `ip:port` per line)
3. Choose the protocol and refresh interval
4. Click **Fetch Now** or wait for the scheduler

The system will:
- Download and parse the list
- Upsert proxies into the database (duplicates ignored)
- Automatically look up GeoIP data for every new proxy
- Re-sync all pools that have `Auto-sync` enabled

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
4. Configure max retries across the chain and an optional `requests_per_minute` cap

Users connect as:
```
http://username:password@your-proxy-host:8000
```

If the main pool has no live IPs the request automatically cascades to the next fallback pool.

---

## 📌 Session Stickiness & Proxy Invalidation

### Session-based rotation

Set a pool's `rotation_method` to `session` to keep the **same proxy** for a whole client session instead of rotating per request. A session is identified by a token you embed in the proxy **username**, using the common `user-session-<token>` convention:

```bash
# Every request with this username reuses the same upstream proxy
curl -x "http://alice-session-job42:password@your-proxy-host:8000" https://example.com
curl -x "http://alice-session-job42:password@your-proxy-host:8000" https://example.com/next

# A different token gets a different proxy
curl -x "http://alice-session-other:password@your-proxy-host:8000" https://example.com
```

A session binding is held until one of:

- **You release it** — `POST /api/v1/sessions/release` with `{"token":"job42"}` (add `"pool_id":<id>` to scope to one pool)
- **It goes idle** — no requests for `session_ttl_minutes` (default 10, configurable per pool)
- **Its proxy is invalidated or fails** — the session automatically rebinds to a fresh proxy on the next request

Inspect live bindings with `GET /api/v1/sessions`.

> Requests with no `-session-` token in the username fall back to round-robin, so a `session` pool stays safe to use without a token.

### Invalidating a proxy mid-session

When you detect a proxy is rate-limited (or otherwise bad) while using it, pull it out of rotation immediately:

```bash
# Cooldown for 30 minutes (omit "minutes" or pass 0 to keep it out until reactivated)
curl -X POST "http://localhost:8001/api/v1/proxies/123/invalidate" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"minutes": 30, "reason": "429 from target"}'

# Put it back early
curl -X POST "http://localhost:8001/api/v1/proxies/123/reactivate" \
  -H "Authorization: Bearer $TOKEN"
```

Invalidation sets a database cooldown **and** evicts the proxy from every active user's live rotation right away (no wait for the refresh cycle), rebinding any sessions that were using it. The proxy automatically returns to rotation when its cooldown expires. You can also do this from the dashboard via the **Invalidate / Reactivate** actions in the Proxies table row menu.

---

## 🔐 API Authentication

All API endpoints require a JWT bearer token obtained from `POST /api/v1/auth/login`.

```bash
# Login
TOKEN=$(curl -s -X POST http://localhost:8001/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"yourpassword"}' | jq -r '.token')

# Use token
curl -H "Authorization: Bearer $TOKEN" http://localhost:8001/api/v1/proxies
```

Public endpoints (no token required):
- `GET /health`
- `POST /api/v1/auth/login`

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

Contributions are welcome! We appreciate meaningful contributions that add value to the project.

### How to Contribute

1. **Fork the repository**
2. **Create a feature branch**: `git checkout -b feature/amazing-feature`
3. **Make your changes**
4. **Commit your changes**: `git commit -m 'Add amazing feature'`
5. **Push to the branch**: `git push origin feature/amazing-feature`
6. **Open a Pull Request**

### Contribution Guidelines

- Write clear, descriptive commit messages
- Add tests for new features
- Update documentation as needed
- Follow existing code style and conventions
- Ensure all tests pass before submitting PR
- One feature/fix per pull request

**Note**: Pull requests that do not contribute significant improvements or fixes will not be accepted.

### Development Workflow

```bash
# 1. Create feature branch
git checkout -b feature/my-feature

# 2. Make changes and test
make test

# 3. Commit changes
git add .
git commit -m "feat: add my feature"

# 4. Push and create PR
git push origin feature/my-feature
```

---

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
