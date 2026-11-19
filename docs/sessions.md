# Sticky sessions and invalidation

## Sessions

Add `session-<token>` to the proxy username to keep one proxy across requests:

```bash
# Reuse job42's proxy for this target
curl -x "http://scraper-session-job42:$ROTA_PASSWORD@localhost:8000" https://example.com

# Share one scope across hostnames, so job42 and job43 never get the same proxy
curl -x "http://scraper-session-job42-scope-shopping:$ROTA_PASSWORD@localhost:8000" https://www.example.com
curl -x "http://scraper-session-job43-scope-shopping:$ROTA_PASSWORD@localhost:8000" https://api.example.com
```

- Each `(token, scope)` pair holds one proxy. The client label is ignored.
- The default scope is the target hostname, lowercased and without its port, so each subdomain is its own scope. Set one explicitly with `scope-<name>`.
- Within a scope, a proxy serves one session at a time. Sessions in different scopes can share a proxy. Requests without a session only use proxies that no session holds in their scope.
- `country` and `tag` filters apply to the binding. If the bound proxy stops matching, Rota replaces it.
- A binding lasts until it is released, sits idle past the session TTL (Settings → Upstream requests, default 10 minutes), or loses its proxy. After losing its proxy, the session rebinds on its next request. Traffic through an open tunnel counts as use.
- When a binding ends or moves to another proxy, Rota closes its tunnels. A kept-alive connection then reconnects and goes through selection again. Tunnels without a session have no idle limit and stay open through invalidation.
- Bindings are held in memory. They reset on restart and are not shared between Rota processes.

When every eligible proxy is taken, the request fails with [`593`](errors.md).

`GET /api/v1/sessions` lists the bindings. Release one when the job is done:

```bash
curl -X POST http://localhost:8001/api/v1/sessions/release \
  -u "scraper:$ROTA_PASSWORD" -d '{"token": "job42"}'
```

Releasing a session that does not exist returns `200` with `count: 0`. Release and invalidation both take an optional `scope` to pick one binding of a token used in several scopes.

## Invalidation

When a target starts blocking a proxy, invalidate the proxy to put it on a cooldown. A client usually has its session token and no proxy ID, so it can invalidate by token:

```bash
curl -X POST http://localhost:8001/api/v1/sessions/invalidate \
  -u "scraper:$ROTA_PASSWORD" \
  -d '{"token": "job42", "minutes": 15, "reason": "429 from target"}'
```

`minutes` defaults to 30 and must be a positive integer. An unknown or expired token returns `409`.

| Request | The proxy cools down for | Duration |
|---|---|---|
| `POST /sessions/invalidate` | each matched binding's scope | backoff |
| … with `"domain": "example.com"` | that domain and its subdomains, whatever the scope | backoff |
| … with `"global": true` | every target, dropping all its bindings | fixed |
| `POST /proxies/{id}/invalidate` | every target, dropping all its bindings | fixed |
| … with `"domain": "example.com"` | that domain and its subdomains | fixed |

A cooldown applies to every request for that scope or domain, with or without a session, and survives restarts. The session's tunnels through the proxy close, and it rebinds on its next request. `X-Rota-Proxy-Id` on a proxied response, or on the CONNECT response for HTTPS, gives the ID for the last two rows.

A session on a residential gateway sets no cooldown, whatever the options. Its exit is dropped, and its next request gets a new IP. Invalidating a gateway by ID fails with `400`, since that would cool every exit behind it.

### Backoff

Repeated session invalidations of the same proxy for the same scope or domain last 1×, 2×, then 4× `minutes`. The fourth excludes the proxy from that scope or domain until it is reactivated. Sending `minutes: 360` each time gives 6h, 12h, 24h, then excluded. The response reports `failure_count` and `invalid`. For an exclusion, `invalid` is `true` and `cooldown_until` is `null`.

A streak resets once the client stops invalidating. The first session request through the proxy after its cooldown starts a grace period of one session TTL. An invalidation during the grace period extends the streak, and the streak resets if the period ends without one. A proxy that gets no session traffic after its cooldown keeps its streak. Rota ignores status codes here, so this works through opaque HTTPS tunnels too.

### Reactivation

```bash
curl -X POST http://localhost:8001/api/v1/proxies/123/reactivate -u "scraper:$ROTA_PASSWORD"
```

With no body, this clears every cooldown and exclusion on the proxy along with its backoff history. Send `{"scope": "shopping"}` or `{"domain": "example.com"}` to clear only that one.
