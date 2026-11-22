# `magnetico web` — configuration, database and reverse-proxy notes (W1, W2)

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

## 4. Database schema and roles (W2)

Order of operations on a fresh database (all explicit; the web service never creates or migrates anything):

1. `magnetico migrate schema-init` — the torrent schema (`magneticod`).
2. `magnetico migrate web-schema-init` — the `magnetico_web` schema: accounts, sessions, invitations, API and RSS tokens, one-time links, trackers, settings, and the `web_version` table. It also seeds the approved historical tracker list **once** (never again, so later operator edits are kept). It is non-destructive and idempotent: an already initialized, structurally identical schema is accepted; **anything else (a different version, a missing constraint or index, an extra column or trigger, …) is refused without any change** — there is no automatic repair. The target database comes from `--database`, `MAGNETICO_MIGRATE_DATABASE` or the `migrateDatabaseURL` file key (the same as the other `migrate` commands) and needs a role that may create schemas.
3. Grant the minimum privileges below, then start `magnetico web` with `MAGNETICO_WEB_DATABASE` set to the runtime role.

At every start the service verifies the torrent schema, the exact `magnetico_web` structure and these privileges, and refuses to start (exit status 1, with the missing privilege named) if anything is off. The version table is deliberately named `web_version`: the torrent-schema verification treats a table called `schema_version` in *any* schema as one of its own markers.

### Minimum grants

The runtime role reads the torrent schema (including `magneticod.schema_version`, which the startup verification reads) and has DML — but no DDL — on `magnetico_web`. The operator role (used by the administrator commands) adds tracker writes. Replace the role names as needed.

<!-- grants:begin -->
```sql
-- runtime role (the web service)
GRANT USAGE ON SCHEMA magneticod TO magnetico_web_rt;
GRANT SELECT ON magneticod.torrents, magneticod.torrent_file_blobs, magneticod.schema_version TO magnetico_web_rt;
GRANT USAGE ON SCHEMA magnetico_web TO magnetico_web_rt;
GRANT SELECT ON magnetico_web.web_version, magnetico_web.settings, magnetico_web.trackers TO magnetico_web_rt;
GRANT SELECT, INSERT, UPDATE ON magnetico_web.users, magnetico_web.api_tokens,
      magnetico_web.rss_tokens, magnetico_web.invites TO magnetico_web_rt;
GRANT SELECT, INSERT, UPDATE, DELETE ON magnetico_web.sessions, magnetico_web.one_time_tokens TO magnetico_web_rt;

-- operator role (administrator commands): everything above plus tracker writes
GRANT magnetico_web_rt TO magnetico_web_op;
GRANT INSERT, UPDATE, DELETE ON magnetico_web.trackers TO magnetico_web_op;
```
<!-- grants:end -->

These statements are executed verbatim by the integration tests, which start the real service as the restricted role and check that revoking any of them (for example `SELECT` on `magneticod.schema_version`) makes startup fail with a controlled message.

## 5. Administrator commands (W2)

`magnetico web user …` and `magnetico web tracker …` administer accounts, invitations and the public tracker list. They use the same database settings as the service (`--database` without a password, or `MAGNETICO_WEB_DATABASE`, or the `web:` file section), open two connections, and refuse to run unless the `magnetico_web` schema is exactly the supported version and the role holds the operator grants of §4. Commands that print absolute links (`user add`, `user reset-password`, `user invite add|list`) also need the public URL (`MAGNETICO_WEB_PUBLIC_URL`); the other commands validate only what they use. Results and requested links go to stdout, failures to stderr with exit status 1; nothing secret is printed except the link you asked for.

| Command | Effect |
|---|---|
| `web user add <username> [--email E] [--invites N]` | Enabled account without a password; prints a single-use setup link (72 h). `--invites` defaults to `invite-quota`. |
| `web user list` | id, username, role, active, password state (`none`/`current`/`legacy`/`needs-reset`), email state, created, last login. |
| `web user set-role <username> <user\|admin>` | Sets the role. |
| `web user disable <username>` / `enable <username>` | Disable deletes the account's sessions and suspends its API/RSS tokens and invitations; enable restores them (revoked and expired ones stay dead, sessions do not return). |
| `web user reset-password <username>` | Issues a setup link (account has no password, 72 h) or a reset link (1 h) and replaces any earlier link. **Completing a reset signs the user out everywhere and permanently revokes all their API and RSS tokens**; clients must be updated. |
| `web user set-password <username>` | Reads the password from a no-echo terminal prompt (asked twice) or from one line of piped standard input — never from arguments. Same revocations as a completed reset. |
| `web user revoke-tokens <username>` | Permanently revokes all API and RSS tokens. |
| `web user invite add <username> [--count N] [--expires-in D]` / `invite list <username>` / `invite revoke <id>` | Invitation links; revocation is by id so codes stay out of shell history. |
| `web tracker list` / `add <url> [--position N]` / `remove <url\|id>` / `move <url\|id> <position>` | Edits the public tracker list (positions stay 1..n). Running services pick the change up within `tracker-cache-ttl`. |
