# `magnetico web` — configuration, database and reverse-proxy notes (W1–W7)

Normative behaviour is in `docs/specs/web-v1.md`; this page is the operator's quick reference. Nothing here has been integration-tested against a real proxy yet (that belongs to a later milestone).

## 1. Configuration

Precedence: **command line > `MAGNETICO_WEB_*` environment > YAML file > defaults.** The recommended (container) deployment is **environment-based**; the YAML file is optional and is selected with the global `--config-file-path` (before the subcommand) or `MAGNETICO_CONFIG_FILE_PATH`. The file is decoded strictly: unknown keys are errors, and a missing or unreadable file is an error, never ignored. Existing files without a `web:` section are unaffected, and a `web:` section is accepted (and not used) by the other commands.

| Environment variable | `web:` YAML key | Default |
|---|---|---|
| `MAGNETICO_WEB_PUBLIC_URL` | `publicURL` | **required** (`https://…`, or loopback `http://` for development) |
| `MAGNETICO_WEB_LISTEN` | `listen` | `127.0.0.1:8081` |
| `MAGNETICO_WEB_SITE_TITLE` | `siteTitle` | `Magnetico` (site name in the header, page titles and footer; 1–40 characters) |
| `MAGNETICO_WEB_DATABASE` | `databaseURL` | development default as `ingest` |
| `MAGNETICO_WEB_POOL_MAX_CONNS` | `poolMaxConns` | `10` |
| `MAGNETICO_WEB_TRUSTED_PROXIES` | `trustedProxies` (string or list) | empty |
| `MAGNETICO_WEB_SESSION_IDLE_TIMEOUT` / `_ABSOLUTE_LIFETIME` / `_REMEMBER_LIFETIME` | `sessionIdleTimeout` / `sessionAbsoluteLifetime` / `sessionRememberLifetime` | `12h` / `168h` / `2160h` |
| `MAGNETICO_WEB_SEARCH_TIMEOUT` | `searchTimeout` | `60s` |
| `MAGNETICO_WEB_SEARCH_CONCURRENCY` | `searchConcurrency` | `4` |
| `MAGNETICO_WEB_REQUEST_TIMEOUT` | `requestTimeout` | `30s` |
| `MAGNETICO_WEB_SHUTDOWN_TIMEOUT` | `shutdownTimeout` | `30s` (1s–10m) |
| `MAGNETICO_WEB_INVITE_QUOTA` | `inviteQuota` | `0` (invitations a user receives when the account is created or registers; users cannot create more, §8) |
| `MAGNETICO_WEB_TRACKER_CACHE_TTL` | `trackerCacheTTL` | `60s` (how often the public tracker list is re-read, §9) |
| `MAGNETICO_WEB_SMTP_HOST` / `_PORT` / `_SECURITY` / `_USERNAME` / `_FROM` | `smtpHost` / `smtpPort` / `smtpSecurity` / `smtpUsername` / `smtpFrom` | unset (email features off; §8) |
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

## 6. Sign-in, sessions and rate limits (W3)

**Routes.** `GET/POST /login`, `POST /logout` and an interim page at the exact root `/` (replaced by the torrent pages in W4). Every other path, including legacy PHP routes, is 404. Sign-out is `POST` only.

**Cookies.** Production (https public URL): `__Host-magnetico_session` (`Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`). An ordinary session is a browser-session cookie; a "keep me signed in" session carries `Expires`/`Max-Age` equal to the server-side absolute expiry and is never extended by activity. Before sign-in the login form is bound to a one-hour `__Host-magnetico_presession` cookie (`SameSite=Strict`). **Development mode** (loopback `http://` public URL only) uses the names `magnetico_session` / `magnetico_presession` without the prefix and without `Secure`, and logs a warning at startup. The `__Host-` prefix requires the browser to see `https://`, so the reverse proxy must terminate TLS for the public URL.

**Session lifetimes** are `MAGNETICO_WEB_SESSION_IDLE_TIMEOUT` (12h), `…_ABSOLUTE_LIFETIME` (168h) and `…_REMEMBER_LIFETIME` (2160h), validated at startup. All expiry decisions use the **database clock**. To avoid a write per request, `last_seen_at` is updated at most every `min(5m, idle/10)`, so an ordinary session can expire up to that much earlier than the nominal idle timeout.

**Revocation visibility.** There is no authentication cache: every request on a private route resolves its session, the owner's `active` flag and the role in one query. `web user disable` therefore takes effect on the next request; `enable` never brings sessions back (the user signs in again).

**CSRF.** In addition to the standard library's cross-origin check, every POST carries a `csrf_token` form field derived from an HttpOnly cookie (the session cookie, or the pre-session cookie for forms shown before sign-in). A missing or wrong token is a 403 page.

**Rate limiting (per process).** Failed or attempted sign-ins are limited per client address (burst 20, one more every 30 s; IPv6 per /64) and per case-folded username (burst 5, one more every 60 s; a successful sign-in refunds its unit). Exhaustion returns `429` with `Retry-After` and the same page whether or not the username exists; there is no lockout. The client address comes from `X-Forwarded-For` only when the TCP peer is inside `--trusted-proxies` (§2). With the default of one web instance this is sufficient; several instances would each keep their own counters.

**Password hashing capacity.** Verification and hashing share the process-wide limiter (§W2). When it stays saturated beyond its wait, sign-in answers `503` with `Retry-After: 5` (never a wrong-password message). A correct legacy password whose upgrade hash cannot be computed because of that saturation still signs in; the hash is upgraded at a later sign-in.

**Housekeeping.** A background task deletes expired sessions and expired one-time links about every 15 minutes (first run one minute after start). It is stopped, and awaited, before the database pool is closed at shutdown. Expired rows are ignored by every lookup whether or not they were purged yet.

**Privileges.** The documented runtime role (§4) is sufficient; sign-in uses `UPDATE users`, `INSERT/DELETE sessions` and the purge uses `DELETE one_time_tokens`.

## 7. Search, torrent details and the file browser (W4, reworked in W5)

Routes: `GET /torrents` (search, a *search* route with the search deadline), `GET /torrents/{id}` (metadata; reads at most the blob's first 12 bytes to choose the browsing mode, never decodes it) and the two JSON endpoints behind the details page's file browser, `GET /torrents/{id}/files/tree` and `GET /torrents/{id}/files?dir=&offset=&limit=` (W5; they replace the W4 HTML directory pages). `GET /` redirects to `/torrents`. All need a session; the JSON endpoints answer `401` `application/problem+json` instead of redirecting. `/api/v2` is described in §9.

**Fixed limits (constants, not configuration):** file-list decode admission **192 MiB** of estimated working set (bounded queue: 32 waiters, 5 s), file-index cache **128 MiB** (LRU, entries revalidated against the database on every request and expired after 10 min), directory pages of at most 1,000 entries and 1 MiB (the UI asks for 160), search queue 4 × `SEARCH_CONCURRENCY` waiters / 5 s. A saturated decode queue or search queue answers **503 with `Retry-After`**, never a partial or empty list.

**File-view limits (W5, constants, not configuration; rationale in `docs/wip/07F-web-ui-report.md`):** a complete tree is offered for torrents of at most **4,000 files** whose estimated JSON size is at most **1 MiB** and is refused (`422`, the browser then pages directory by directory) if the exact document would exceed **4 MiB**; serialization has its own budget of **8 concurrent responses × 4 MiB** (up to 32 MiB of buffers, separate from the 192 MiB decode gate), with a bounded 32-request queue and a 2 s wait, then `503`; the index refuses hierarchies of more than **250,000 directories or 256 levels** (`422 index-too-complex`; such torrents stay searchable and their details render; the refusal is remembered for at most 5 minutes, 1,024 entries, per blob version, so repeated requests do not repeat the decode). Neither the tree nor directory pages may be cached by a proxy (`Cache-Control: private, no-store`); only `/static/*` is immutable.

**Reverse proxy:** unchanged from §3 — the proxy's upstream timeouts must exceed the search write deadline (`SEARCH_TIMEOUT` + 10 s; with the default 60 s, allow ≥ 90 s). A cold file-list build takes 0.2–1.1 s uncontended and up to a few seconds when queued behind other builds, well below `REQUEST_TIMEOUT` (30 s), so file pages need no special proxy settings. Do not enable proxy response caching for these routes: pages and file-view JSON are per-user. `/static/*` files carry content-hashed names and `immutable` caching and may be cached freely. The UI makes no requests to any third-party host (no fonts, scripts or analytics); the Content-Security-Policy is unchanged (`default-src 'self'`, no inline scripts or styles). The file browser needs JavaScript in the browser; everything else works without it.

**Memory — recommendation for a container of about 1 GiB with 2 vCPU** (measured, see `docs/wip/07E-web-search-details-report.md` §7; verified in a real 1 GiB cgroup with swap disabled and a 200 % CPU quota):

- Set a hard container memory limit of **1 GiB** and `GOMEMLIMIT=768MiB` (about 75 % of the limit). Under a deliberately cold-heavy mixed workload (32 file clients + 4 searchers + logins) the process peaked at **≈ 750 MiB cgroup memory** (RSS ≤ 740 MiB), with no OOM kill, in every `GOMEMLIMIT` setting tested (unset, 640, 768, 896 MiB). `GOMEMLIMIT` changed the peak by only tens of MiB because the live heap is already bounded by the decode gate and the cache; its job is to make the collector work harder before the hard limit, not to shrink the budgets. Treat it as a safety margin, not as a proof against OOM: pathological blobs can exceed the weight estimate (measured up to 2.1× for trie-encoded long paths), and one process-wide limit cannot protect against that.
- Allow for PostgreSQL connections: `POOL_MAX_CONNS` (default 10) with `SEARCH_CONCURRENCY` ≤ pool − 2 keeps login and details responsive during heavy searches and decodes.
- The target "peak RSS ≤ 640 MiB at 32 concurrent clients" was **not met** in the worst mixed workload (≤ 740 MiB); plan for ≥ 1 GiB, not 768 MiB.
- On shutdown the file-index cache stops after in-flight requests drain and before the database pool closes; the stop is logged with the cache counters (`file index cache stopped`).
- **Directory addressing (W5).** Directories are addressed by short positional paths (at most 256 numeric components, under 2 KB), so the W4 limitation for very long directory URLs no longer exists and the 16 KiB request-header limit is no longer a concern for the file browser. A hierarchy beyond the index limits is the only case in which a valid torrent cannot be browsed.

## 8. Accounts, email and credentials (W6)

**Routes.** `GET /account?tab=profile|invitations|rss|api` and the `POST` actions below it (password, email, email/resend, API and RSS tokens, token and invitation revocation); public `/register/{code}`, `/password/forgot`, `/password/setup/{token}`, `/password/reset/{token}`, `/email/verify/{token}`. Every `POST` carries the CSRF token (session-bound on the account page, pre-session-bound on public forms). Pages that show or are reached through a credential send `Referrer-Policy: no-referrer` and `Cache-Control: private, no-store`.

**Invitations.** A user sees and revokes only their own pre-minted invitations (`INVITE_QUOTA` at account creation or registration); there is no self-service creation. Further invitations: `web user invite add`.

**Tokens.** At most 50 unrevoked API tokens and 50 unrevoked RSS tokens per user; creation is limited to a burst of 10 and one more every 6 minutes per account and type. Plaintext is shown once; only SHA-256 is stored. **Revoked rows are kept** (no database role may delete token rows) and there is no lifetime cap on them: with the creation limit this grows slowly, but operators who care should watch `magnetico_web.api_tokens` and `rss_tokens`, and a future operator command may prune old revoked rows.

**Email is optional.** Without `MAGNETICO_WEB_SMTP_HOST` the profile page says email is unavailable, `/password/forgot` says recovery is not available, and sign-in, tokens, password change and administrator-issued links work as before. With SMTP configured:

* `MAGNETICO_WEB_SMTP_SECURITY` is `starttls` (submission, usually port 587) or `tls` (implicit TLS, usually 465). There is **no plaintext mode and no fallback, loopback included**: if STARTTLS is not offered, or the certificate does not verify (system root certificates, the configured host name as the expected name, TLS 1.2 or newer), nothing is sent and no credential is transmitted. There is no option to skip verification. To trust a private CA, point `SSL_CERT_FILE` (or `SSL_CERT_DIR`) at it in the service's environment; that replaces the system roots for the whole process.
* The username and `MAGNETICO_WEB_SMTP_PASSWORD` are optional but must be given together; the password is environment-only and is never logged.
* Mail is queued in memory (capacity 32 messages queued or in flight, two workers, 30 s per message, jobs older than 15 minutes are dropped unsent). Nothing is persisted: a restart loses queued mail, and a user can request a verification email again from the account page. A password-recovery message that cannot be queued is silently dropped (the page answer never depends on it). The logs carry only the failure category (for example `tls`, `starttls-missing`, `auth`, `rejected`, `timeout`), never an address or a link.
* Password-reset and setup emails state that completing them signs the user out everywhere and revokes **all API tokens and all RSS tokens and feed URLs**.

**Privileges.** The documented runtime role (§4) is sufficient: token creation and `last_used_at` use `INSERT`/`UPDATE`, one-time links and sessions use `DELETE`, the tracker list uses `SELECT` on `trackers`. The real-binary test runs the whole feature set under exactly those grants.

## 9. API v2, RSS feeds, magnet links and documentation (W7)

| Path | Credential | Notes |
|---|---|---|
| `GET /api/v2/torrents`, `/torrents/{id}`, `/torrents/{id}/files/tree`, `/torrents/{id}/files` | API token (`mgt_…`) in `Authorization: Bearer` only | JSON, `private, no-store`; same limits as the web file browser (§7) |
| `GET /api/v2/feeds/latest.rss?feed_token=` | RSS token (`mgf_…`, or a migrated legacy token) in `feed_token` only | RSS 2.0, newest 1,000 per page, `page` 1–10 |
| `GET /api/v1/feed/rss/last` (deprecated) | RSS token in the `api-token` header, cookie or query | same document; failures use the legacy JSON envelope; `page=1` is now the newest 1,000 (clients may see up to 1,000 recent items they have never seen once after the upgrade; GUIDs are info hashes) |
| `GET /magnet/{infohash}` | none | `307` to `magnet:?xt=urn:btih:…&tr=…` with the configured trackers, no name; never looks the torrent up |
| `GET /api`, `/api/v1`, `/api/v2`, `/api/{v1,v2}/openapi.yaml` | session cookie (anonymous requests are redirected to `/login`) | `noindex`, `private`; interactive documentation (Swagger UI, vendored; "Try it out" is `GET` only and uses a token the user pastes in) |

* **Credentials are separate.** API tokens never open a feed and RSS tokens never open the JSON API; a revoked, expired or suspended (owner disabled) token fails on its very next request, and `disable`/`enable` leave token rows untouched. A password reset revokes both types.
* **Feed URLs contain a credential.** The service never logs query strings, never echoes the token, sends `Referrer-Policy: no-referrer` and `Cache-Control: private, no-store`. **Configure the reverse proxy to omit or mask query strings in its own access logs** and do not enable response caching for these paths (a cache key that includes the query would store a per-user URL; the pages are cheap and the service caches the rendered page for 60 s itself, after checking the token). Failures are rate limited per client address (burst 20, one more every 10 s) and successes per token.
* **Tracker list.** Loaded from `magnetico_web.trackers` at startup (startup fails if it cannot be read), refreshed at most every `TRACKER_CACHE_TTL` by one background task, last good list kept if a refresh fails (a warning is logged). Edits made with `web tracker …` appear within the TTL. `/magnet` and every magnet link use this snapshot and never wait for the database.
* **Documentation pages and CSP.** The interactive pages use the normal policy plus `img-src 'self' data:` (the renderer's stylesheet uses inline `data:` icons) — no inline script or style, no `eval`, no other origin; the relaxation applies to `/api/v1` and `/api/v2` only. Whether the renderer runs cleanly under this policy in real browsers has **not** been verified on the development VM.
* **Reverse proxy timeouts** are unchanged (§3): `/api/v2/torrents` is a search route (`SEARCH_TIMEOUT` + 10 s write deadline); every other new route is an ordinary request (`REQUEST_TIMEOUT` + 5 s).


## 10. Migrating the legacy PHP accounts (W8)

`magnetico migrate users` imports the accounts of the legacy PHP/Symfony application into `magnetico_web` in **one target transaction**: a failed, cancelled or interrupted run changes nothing. The design, decisions and cutover rationale are in `docs/wip/07H-user-migration-plan.md`; the implementation record is `docs/wip/07H-user-migration-report.md`. Nothing here connects to production by itself: you point the command at databases you choose.

**What it does.** Users and invitations keep their numeric ids and relationships; usernames are copied byte for byte (case-sensitive); password hashes are copied unchanged (the web service verifies them directly and rehashes on the first successful login); every migrated account is an active `user`; historical email addresses are copied as written and are **unverified** (use the account page to verify; until then they are not used for self-service recovery, and an administrator-issued reset link is the fallback). Every legacy API token becomes a **legacy RSS token** (`rss_tokens`, SHA-256 only, original creation time, `legacy = true`) so existing `…/api/v1/feed/rss/last?api-token=…` URLs keep working; **no** `api_tokens`, sessions or one-time links are created. Not migrated: PHP sessions, remember-me cookies, password-reset tokens, reCAPTCHA state.

**Databases and configuration.**

| | Setting |
|---|---|
| Target (has `magnetico_web`) | `--database`, `MAGNETICO_MIGRATE_DATABASE`, or the `migrateDatabaseURL` file key (the other `migrate` commands' convention) |
| Source (legacy PHP database, schema `users`) | `MAGNETICO_MIGRATION_USERS_SOURCE_DSN` or the `migrateUsersSourceDatabaseURL` file key **only**: no flag (it can never appear in `ps`), no default, never the target or the torrent source (`MAGNETICO_MIGRATION_SOURCE_DSN`) |
| `--source-timezone` | **required**, an IANA zone (`php -r 'echo date_default_timezone_get();'` in the legacy container); legacy timestamps carry no zone |

DSNs, hashes, token keys, invitation codes, emails and usernames are never printed or written to a report: records are identified by numeric id and finding code, and database errors are reduced to the SQLSTATE and constraint name.

**Privileges.** The source role needs `CONNECT`, `USAGE` on schema `users`, and `SELECT` on `users.users`, `users.invites`, `users.api_tokens` and the Doctrine `migration_versions` table; every source transaction is `READ ONLY`, and a role that can write a legacy table is reported as a warning. A `--dry-run` needs only `SELECT` on the six user-owned `magnetico_web` tables and on their three identity sequences (`users`, `invites`, `rss_tokens`). A **final run uses the owner of the `magnetico_web` tables** (the identity `RESTART` is an `ALTER TABLE`): it needs `INSERT` on `users`, `invites`, `rss_tokens`, and with `--replace-users` also `DELETE` on all six. It is never run as the runtime or operator role, and no grant or schema change is made.

**Commands.**

```text
magnetico migrate users --source-timezone ZONE --dry-run [--accept-needs-reset N]
        [--replace-users [--delete-unmatched-user-ids ID,ID]] [--report-json FILE]
magnetico migrate users --source-timezone ZONE --source-writes-stopped [--accept-needs-reset N]
        [--replace-users --confirm-plan DIGEST [--delete-unmatched-user-ids ID,ID]] [--report-json FILE]
magnetico migrate users verify --source-timezone ZONE [--report-json FILE]
```

* **`--dry-run`** is read-only on both databases (a `READ ONLY` target transaction, no locks, no sequence calls: sequence state is untouched). It checks the source schema fingerprint (exactly the five known Doctrine migrations and the legacy columns), validates every row against the target constraints, classifies the target and prints the **plan digest**. It may run during normal operation. It exits 0 only when a final run would be accepted; otherwise it names the reasons (`would_refuse`) and exits 1.
* **A final run is an offline operation.** Stop the legacy application and prevent legacy writes first, then pass `--source-writes-stopped`: it is an operator assertion, not a proof. The migrator reports what it can observe (other client backends, a database-level read-only default) and re-reads the source before committing, but that is defence in depth only: there is no cross-database atomic snapshot. The `ACCESS EXCLUSIVE` table locks it takes serialise against conflicting database operations only; they do **not** prove the web service is stopped, so stop the service and operator commands yourself.
* **Refusals** (exit 1, nothing written): `config`, `source-schema`, `target-schema`, `privileges`, `source-empty`, `data-blocked` (unstorable values, orphans, duplicates; nothing is transformed or dropped), `unvetted-hash-format`, `needs-reset-unacknowledged`, `target-not-empty`, `unmatched-target-accounts`, `plan-mismatch`, `source-writes-not-asserted`, `lock-timeout`, `source-changed`, `verify-failed`, `storage`.
* **Password hashes** fall in three categories. *Verified*: exact parameters with PHP-generated vectors. *Unvetted*: the verifier parses them but no exact vectors exist; a final run refuses until vectors are added in a reviewed change. *Unsupported*: they cannot log in; acknowledge the exact number with `--accept-needs-reset N` and issue administrator reset links afterwards (completing a reset permanently revokes that user's legacy RSS URLs). The aggregate-only audit for the legacy database is `docs/wip/07H-audit.sql`.
* **Re-running** a completed, untouched import is a no-op (`already-migrated`). A populated target that is not exactly the expected import is refused unless `--replace-users` is given.
* **`--replace-users` is destructive**: it deletes every account and all user-owned data in the target (sessions, one-time links, API and RSS tokens, invitations, users) and imports again, in the same transaction. It never touches torrents, trackers, settings or schema versions, and never moves an id sequence backward. A target account that is not in the source (same id **and** username) is protected: the run refuses unless `--delete-unmatched-user-ids` equals that set exactly. A real run requires `--confirm-plan` with the digest of a previous `--dry-run --replace-users` with identical options (the preview needs none and prints what would be discarded). The digest binds the source data, the time zone, the target state, the destructive operation, the deletion scope and the acknowledgement, so changing any of them invalidates the confirmation.
* **Reports.** `--report-json FILE` writes a secret-free JSON report (counts, ids, classes, digests) with mode 0600, created exclusively: an existing file is never overwritten.

**Cutover.** Run the audit, rehearse on copies, then: stop the legacy application and prevent its writes, back up both databases, stop the web service, `--dry-run`, run for real, `verify`, promote the first administrator with `magnetico web user set-role <username> admin`, start the web service. The full checklist is §9 of the plan.
