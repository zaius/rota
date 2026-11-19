# Proxy status codes

Rota answers on the proxy port with three statuses of its own, so a client can tell Rota's failures from the target's responses. Match on the status and the `X-Rota-Error` header, because reason phrases and bodies may change.

| Status | `X-Rota-Error` | What to do |
|---|---|---|
| `407` | `proxy_auth_required`: the password is missing or wrong | Fix the credentials |
| `407` | `invalid_proxy_options`: the username does not parse, and the body names the problem | Fix the username |
| `592` | Forwarding or tunnel failure, with one of the reasons below | The target may have received the request, so retry only if it is safe to repeat |
| `593` | `no_proxy_available`: every eligible proxy is reserved, on cooldown or filtered out | Wait `Retry-After` seconds (5) and retry with the same session and scope. A proxy may still not be free by then |
| `594` | The requested TLS profile cannot run, as described in [HTTPS inspection](tls-inspection.md#594-inspection-unavailable) | Fix the configuration before retrying |

Every other status and body comes from upstream, including `429`, `502` and `503`. Rota strips `X-Rota-Error` from upstream responses, so the header only ever comes from Rota.

## 592 reasons

| Reason | Meaning |
|---|---|
| `proxy_connect_failed` | DNS or TCP connection to the proxy failed |
| `proxy_handshake_failed` | Sending CONNECT to the proxy, or reading its reply, failed |
| `proxy_configuration_error` | The proxy has an invalid URL or an unsupported protocol |
| `upstream_proxy_auth_failed` | The proxy answered `407`. Check its stored credentials |
| `upstream_proxy_payment_required` | The proxy answered `402`: the provider account is out of bandwidth or unpaid |
| `upstream_connect_rejected` | The proxy refused the CONNECT with another 4xx or 5xx, and so did a second proxy if one was free. `X-Rota-Upstream-Status` carries the last status |
| `upstream_timeout` | The connection, tunnel or request timed out |
| `upstream_request_failed` | Any other forwarding failure |
| `target_invalid` | The CONNECT target has no host, or no port from 1 to 65535 |
| `target_dns_not_found` | The CONNECT host has neither an A nor an AAAA record |
| `target_no_ipv4` | The CONNECT host has no IPv4 address: an IPv6 literal, or a name without an A record |
| `client_request_aborted` | The client canceled the request, or its deadline passed |

Rota cannot always tell whether the proxy or the target caused a failure, so it counts a failure against a proxy only when the proxy is clearly at fault:

- Connection, handshake, authentication, payment and timeout failures count against the proxy, and Rota retries through another one.
- `target_*` failures come from a local check before Rota picks a proxy, so they use no attempt and no session reservation. Rota resolves the host itself for up to five seconds. If that lookup is inconclusive, Rota leaves resolution to the proxy.
- `upstream_connect_rejected` might be the target refusing, the proxy blocking the domain, or the target blocking the exit IP. Rota retries once through a different proxy and counts neither rejection against its proxy.
- `client_request_aborted` is not retried and does not count against the proxy.

History records the last two as failed requests, flagged `target_failure` so they stay out of per-proxy success rates and automatic cleanup.

## HTTPS

Errors before a tunnel is established arrive on the CONNECT response. After that the stream is encrypted and Rota cannot inject a response, so a failure closes the connection. Requests inside an [inspected](tls-inspection.md) tunnel can each return `592`.
