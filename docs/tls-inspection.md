# HTTPS inspection and TLS fingerprints

By default an HTTPS request travels through an opaque CONNECT tunnel. Rota records each tunnel's target host, bytes and duration, and cannot see the requests inside it. One tunnel can carry many requests, so HTTPS request counts look low next to plain HTTP. The dashboard reports tunnels and tunnel data separately for that reason.

Inspection terminates the client's TLS at Rota, records every request with its method, URL, status and latency, and opens Rota's own TLS connection to the target. The profile you ask for decides which client that connection looks like.

It takes two opt-ins: a CA on the server, which makes inspection possible, and a `profile-<name>` option in the proxy username, which requests it for one connection.

## Trade-offs

- The client must trust the CA, or every inspected request fails its certificate check.
- The target sees Rota's TLS handshake in place of the client's.
- Targets that pin certificates always fail under inspection. List them in `TLS_INSPECT_BYPASS_DOMAINS` and connect to them without a profile.
- The CA key can mint a certificate for any host. Guard it like any other signing key.

## Setup

Generate a CA:

```bash
openssl ecparam -genkey -name prime256v1 -out ca.key
openssl req -x509 -new -key ca.key -sha256 -days 825 -out ca.crt \
  -subj "/CN=Rota Inspection CA" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign"
```

Point Rota at it and restart:

```bash
TLS_INSPECT_CA_CERT=/etc/rota/ca.crt
TLS_INSPECT_CA_KEY=/etc/rota/ca.key
```

Under Compose, use absolute host paths and add the overlay that mounts both files, on every Compose command:

```bash
docker compose -f docker-compose.yml -f docker-compose.tls-inspect.yml up -d
```

The container runs as UID 1000 and must be able to read both files.

Then request a profile and trust the CA in the client:

```bash
curl --cacert ca.crt -x "http://scraper-profile-ios:$ROTA_PASSWORD@localhost:8000" https://example.com
```

To check that it worked, add `-v` and look at the certificate issuer. If it is your CA, the connection is inspected. If it is the target's real issuer, the tunnel is still opaque. Rota also logs `HTTPS interception available for requests that name a profile` at startup.

## Profiles

A profile sets the TLS ClientHello and, over HTTP/2, the settings, priority frames and header order.

| Profile | Presents |
|---|---|
| `go` | Go's own TLS over HTTP/1.1, with no impersonation |
| `ios`, `ios18` | Safari on an iPhone |
| `android` | Chrome on an Android phone |
| `okhttp` | OkHttp, the stack behind most native Android apps |
| `chrome`, `firefox` | The desktop browser |

[`profile.go`](../core/internal/tlsprofile/profile.go) records the exact version and User-Agent of each.

- A profile reorders headers and leaves their values alone, so set the client's `User-Agent` to match the profile. An iPhone handshake with a Python User-Agent is easy to spot.
- The TCP/IP fingerprint comes from the upstream proxy, which no profile can change.
- The captured fingerprints go stale as browsers update.

## Reading the results

Each inspected request becomes a normal request event. A `429` or a `403` block page counts as a success, because the request got a response. To see blocking, check status codes, such as the 429s column of the dashboard's domain table.

## 594: inspection unavailable

When a requested profile cannot run, Rota rejects the CONNECT with `594` before it picks a proxy or reserves a session. Falling back to an opaque tunnel would send the client's own handshake without telling it, so Rota refuses the connection. The response body says what to fix.

| `X-Rota-Error` | Fix |
|---|---|
| `tls_inspection_ca_not_configured` | Set `TLS_INSPECT_CA_CERT` and `TLS_INSPECT_CA_KEY` to readable files and restart |
| `tls_inspection_bypassed` | The target matches the `TLS_INSPECT_BYPASS_DOMAINS` entry named in the body |
| `tls_inspection_invalid_host` | Use a valid CONNECT target host |

An unknown profile name is a `407` with `invalid_proxy_options`. A TLS handshake that fails after the CONNECT succeeds closes the connection, and Rota logs the client label, host, profile and error.
