# `magnetico web` — configuration and reverse-proxy notes (W1)

Normative behaviour is in `docs/specs/web-v1.md`; this page is the operator's quick reference. Nothing here has been integration-tested against a real proxy yet (that belongs to a later milestone).

## 1. Configuration

Precedence: **command line > `MAGNETICO_WEB_*` environment > YAML file > defaults.** The recommended (container) deployment is **environment-based**; the YAML file is optional and is selected with the global `--config-file-path` (before the subcommand) or `MAGNETICO_CONFIG_FILE_PATH`. The file is decoded strictly: unknown keys are errors, and a missing or unreadable file is an error, never ignored. Existing files without a `web:` section are unaffected, and a `web:` section is accepted (and not used) by the other commands.

| Environment variable | `web:` YAML key | Default |
|---|---|---|
| `MAGNETICO_WEB_PUBLIC_URL` | `publicURL` | **required** (`https://…`, or loopback `http://` for development) |
| `MAGNETICO_WEB_LISTEN` | `listen` | `127.0.0.1:8081` |
| `MAGNETICO_WEB_DATABASE` | `databaseURL` | development default as `ingest` |
| `MAGNETICO_WEB_POOL_MAX_CONNS` | `poolMaxConns` | `10` |
| `MAGNETICO_WEB_TRUSTED_PROXIES` | `trustedProxies` (string or list) | empty |
| `MAGNETICO_WEB_SESSION_IDLE_TIMEOUT` / `_ABSOLUTE_LIFETIME` / `_REMEMBER_LIFETIME` | `sessionIdleTimeout` / `sessionAbsoluteLifetime` / `sessionRememberLifetime` | `12h` / `168h` / `2160h` |
| `MAGNETICO_WEB_SEARCH_TIMEOUT` | `searchTimeout` | `60s` |
| `MAGNETICO_WEB_SEARCH_CONCURRENCY` | `searchConcurrency` | `4` |
| `MAGNETICO_WEB_REQUEST_TIMEOUT` | `requestTimeout` | `30s` |
| `MAGNETICO_WEB_SHUTDOWN_TIMEOUT` | `shutdownTimeout` | `30s` (1s–10m) |
| `MAGNETICO_WEB_INVITE_QUOTA` | `inviteQuota` | `0` |
| `MAGNETICO_WEB_TRACKER_CACHE_TTL` | `trackerCacheTTL` | `60s` |
| `MAGNETICO_WEB_SMTP_HOST` / `_PORT` / `_SECURITY` / `_USERNAME` / `_FROM` | `smtpHost` / `smtpPort` / `smtpSecurity` / `smtpUsername` / `smtpFrom` | unset |
| `MAGNETICO_WEB_LOG_LEVEL` / `_LOG_FORMAT` | `logLevel` / `logFormat` | `info` / `text` |

Durations use Go syntax (`30s`, `12h`; there is no `d` unit). Each setting also has a long flag (`--request-timeout`, …).

**Secrets are environment-only.** The SMTP password is `MAGNETICO_WEB_SMTP_PASSWORD` (no flag, no file key). A database URL **containing a password** is refused on `--database` (process listings) and in the file (`databaseURL`); put it in `MAGNETICO_WEB_DATABASE`.

```yaml
# optional; ENV remains the recommended way in containers
web:
  publicURL: https://magnet.example.org
  listen: 0.0.0.0:8081
  trustedProxies: [172.18.0.0/16]
  requestTimeout: 30s
```

## 2. Trusted proxies and client addresses

The service listens on plain HTTP behind a TLS-terminating proxy. Only **`X-Forwarded-For`** is read, and **only when the direct TCP peer is inside `trustedProxies`**; otherwise the header is ignored and the peer address is the client (so a client cannot choose its rate-limit identity). With a trusted peer the chain is walked from the right, skipping trusted hops; the first untrusted address is the client. A malformed or over-long chain falls back to the peer address. `Forwarded`, `X-Real-IP`, `X-Forwarded-Proto` and `X-Forwarded-Host` are not used; absolute URLs always come from `PUBLIC_URL`.

Configure `trustedProxies` with the **narrowest** set of addresses your proxy connects from (for example the proxy container's network or a single address), never `0.0.0.0/0`. Make sure the proxy **appends** the real peer to `X-Forwarded-For` (Traefik, Nginx with `$proxy_add_x_forwarded_for`, and Caddy's `reverse_proxy` do so), and that the service port is reachable **only** from the proxy: a client that can reach the port directly is simply treated by its own address. If you leave `trustedProxies` empty every request is attributed to the proxy's address, which is safe but merges all clients into one rate-limit bucket.

## 3. Reverse-proxy timeouts

Searches may legitimately run for `SEARCH_TIMEOUT` (60 s) and the service may still write the response for 10 s after that, so the search **write deadline is 70 s** with the defaults (ordinary requests: `REQUEST_TIMEOUT` + 5 s = 35 s). The proxy must not give up on the upstream before that, and must not cut an idle keep-alive earlier than it can recover from. **Recommendation: allow at least 90 s (about 20 s above 70 s) for the upstream response/idle timeouts**, and recompute as `SEARCH_TIMEOUT + 10 s + 20 s` if you change the search timeout. The handler context is cancelled exactly at `SEARCH_TIMEOUT`, so a stuck query is always stopped by the application, not by the proxy.

The option names below are taken from the upstream projects' documentation as I know it; **they and the proxies' defaults were not verified against specific versions here** — check your version's documentation. Note that proxy defaults (for example a 30 s upstream response limit in some products) may be lower than 70 s, so set the values explicitly.

- **Traefik (default deployment).** Static configuration: the entry point's `transport.respondingTimeouts` (`readTimeout`, `writeTimeout`, `idleTimeout`) govern the client side; set `writeTimeout` (or leave it unlimited) and `idleTimeout` above 90 s. For the upstream side, a `serversTransport` with `forwardingTimeouts.responseHeaderTimeout` above 90 s (and a sensible `idleConnTimeout`) applies to the web service. Client-IP trust is configured on the entry point (`forwardedHeaders.trustedIPs`).
- **Nginx.** `proxy_read_timeout 90s;` (or more) and `send_timeout`/`proxy_send_timeout` accordingly; `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`.
- **Caddy.** In `reverse_proxy`, `transport http { response_header_timeout 90s }` (and a read timeout if you set one); trusted proxies for Caddy's own client-IP handling are configured with `trusted_proxies` in the server options.

The service logs the effective deadlines and this requirement at startup. Full deployment integration testing is out of scope for W1.
