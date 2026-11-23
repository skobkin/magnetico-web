# Magnetico 0.8.0 — Web Service Specification (web-v1)

> **DRAFT — FOR OWNER REVIEW. NOT APPROVED AS A WHOLE. W1–W7 are implemented (see §14).**
> Revision 7 (07G, W6 + W7 implemented: account management, SMTP and the mail queue, API and RSS credential management, `/api/v2`, both RSS feeds, the `/magnet` short link with the tracker snapshot, the vendored API documentation; the implementation record and every deliberate deviation are in §14) on top of revision 6 (W5: the file browser moves into the details page and is served as JSON in two backend-selected modes with deterministic positional directory paths; the HTML directory browser, percent-encoded directory links and the no-JavaScript requirement for file viewing are retired; §6.4.7–6.4.13, §7.2–7.6, §11.1) on top of revision 5 (W2 schema contract: DDL realized in `internal/web/webschema`, version table `web_version`, one-time links deleted on consumption, no triggers, structural verification, minimum privileges) on top of revision 4 (account lifecycle: `disable` suspends rather than revokes tokens and invitations, password reset revokes all credentials, `revoke-tokens`) on top of revision 3 (final owner corrections after review of `15b76d20`: separate RSS and API credentials, final answers on the open items, tracker CLI, `AGENTS.md` rule) and revision 2 (review of `17b4c2f0`). Tags used below:
> **[APPROVED]** — decided by the owner; **[PROPOSAL]** — a recommendation, not a decision; **[OPEN]** — an undecided owner question.
> Rationale and the legacy inventory are in [`docs/wip/07A-web-design-plan.md`](../wip/07A-web-design-plan.md). This document remains DRAFT until the owner's final review.

Builds on [`postgresql-persistence-v2.md`](postgresql-persistence-v2.md) and [`wire-format-v1.md`](wire-format-v1.md). If this document and either of those disagree, they win and this document must be corrected. This document does not change the torrent/file schema, the wire format, the codec, or crawler/ingest behaviour.

Key words MUST, SHOULD, MAY are used in the RFC 2119 sense.

## 1. Scope and non-goals

1.1 `magnetico web` is a command of the single `magnetico` binary and an independently deployable service. It reads the 0.8.0 PostgreSQL torrent storage and keeps its own state (accounts, sessions, invitations, API tokens, tracker list, web settings) in the PostgreSQL schema **`magnetico_web`** **[APPROVED]**.

1.2 The web service MUST NOT depend on the crawler, the crawler's SQLite queue, the ingest service, or the legacy PHP runtime. Its dependency graph MUST NOT contain `internal/crawler`, `internal/crawlqueue`, `internal/ingest` or `modernc.org/sqlite` (enforced by a test over the dependency graph of `./internal/web/...`; the unified executable's `main` package links the crawler and ingest by design and therefore cannot be the subject of this check).

1.3 The web service never writes to `magneticod.*` tables.

1.4 Non-goals of this version: torrent upload or editing; search by file path (removed in 0.8.0, `postgresql-persistence-v2.md` §1); **an administration UI** (a future release will offer statistics, user management, invitations and tracker management; this version provides CLI commands only); OAuth; general RBAC; multi-tenant features; public read-only access and open registration (**not designed here**, §3.6); user migration (a separate task); CI/OCI packaging.

## 2. Process model

2.1 One HTTP listener, plain HTTP, TLS terminated by a reverse proxy (Traefik or equivalent). The default listen address MUST be loopback.

2.2 Configuration **[APPROVED]**: precedence is command-line flags > `MAGNETICO_WEB_*` environment variables > the optional YAML file > defaults. The file is the repository-wide `--config-file-path` / `MAGNETICO_CONFIG_FILE_PATH` mechanism (`goccy/go-yaml`, strict); `web` settings live in a `web:` section whose keys are listed in `docs/web-deployment.md` (camelCase, e.g. `publicURL`, `requestTimeout`); unknown keys are rejected and existing files without a `web:` section keep working. The documented and recommended container deployment is ENV-based. Unknown `MAGNETICO_WEB_*` variables MUST be rejected. Values are never echoed in errors. Secrets are environment-only and MUST NOT be printed or logged: the SMTP password has no flag and no file key, and a database URL containing a password is refused on the command line and in the file (use `MAGNETICO_WEB_DATABASE`).

2.3 Configuration reference **[PROPOSAL]** except where tagged **[APPROVED]**. Durations use Go syntax (`ParseDuration`: `s`, `m`, `h`; **there is no `d` unit**, so 90 days is `2160h`). Invalid values abort startup with an actionable message.

| Variable | Default | Validation |
|---|---|---|
| `MAGNETICO_WEB_DATABASE` | as ingest | PostgreSQL URL |
| `MAGNETICO_WEB_LISTEN` | `127.0.0.1:8081` | host:port |
| `MAGNETICO_WEB_PUBLIC_URL` | **required** | absolute `https://` URL, or loopback `http://` (dev mode, §4.4.3) |
| `MAGNETICO_WEB_SITE_TITLE` | `Magnetico` | 1 to 40 characters, no control characters; the site name in the header, page titles and footer |
| `MAGNETICO_WEB_TRUSTED_PROXIES` | empty | CIDR list |
| `MAGNETICO_WEB_SESSION_IDLE_TIMEOUT` | `12h` **[APPROVED]** | ≥ `5m`; ≤ absolute lifetime |
| `MAGNETICO_WEB_SESSION_ABSOLUTE_LIFETIME` | `168h` (7 d) **[APPROVED]** | ≥ idle timeout; ≤ remember lifetime |
| `MAGNETICO_WEB_SESSION_REMEMBER_LIFETIME` | `2160h` (90 d) **[APPROVED]** | ≥ absolute lifetime; ≤ `8760h` |
| `MAGNETICO_WEB_SEARCH_TIMEOUT` | `60s` **[APPROVED]** | `1s` ≤ value ≤ `15m` |
| `MAGNETICO_WEB_SEARCH_CONCURRENCY` | `4` | 1 ≤ value ≤ pool size − 2 |
| `MAGNETICO_WEB_REQUEST_TIMEOUT` | `30s` | `5s` ≤ value; deadline for non-search requests |
| `MAGNETICO_WEB_SHUTDOWN_TIMEOUT` | `30s` **[APPROVED]** | `1s` ≤ value ≤ `10m`; grace period for in-flight requests on `SIGINT`/`SIGTERM` (§2.5) |
| `MAGNETICO_WEB_POOL_MAX_CONNS` | `10` | ≥ 4 |
| `MAGNETICO_WEB_INVITE_QUOTA` | `0` | ≥ 0; invitations given to a new user |
| `MAGNETICO_WEB_TRACKER_CACHE_TTL` | `60s` | `5s` ≤ value ≤ `1h` |
| `MAGNETICO_WEB_SMTP_HOST`, `_PORT`, `_SECURITY`, `_USERNAME`, `_FROM` | unset (email features disabled) | `_SECURITY` is `starttls` or `tls` only; there is **no** plaintext value |
| `MAGNETICO_WEB_SMTP_PASSWORD` | unset | environment only |
| `MAGNETICO_WEB_LOG_LEVEL`, `_LOG_FORMAT` | `info`, `text` | `text` or `json` |

2.4 Startup MUST fail fast, with an actionable message and without modifying the database, if: PostgreSQL is unreachable; `pg_trgm` is missing; the torrent schema or its `schema_version` is not the supported one; the `magnetico_web` schema is missing, has an unsupported version, **or differs structurally from version 1 (columns, constraints, indexes, unexpected triggers; §5.5)**; the tracker list cannot be loaded; the database role lacks the needed privileges; SMTP is partially configured or its `_SECURITY` is not `starttls`/`tls`. It MUST NOT run any automatic migration (`postgresql-persistence-v2.md` §2, §6).

2.5 Graceful shutdown on `SIGINT`/`SIGTERM`: stop accepting connections, let in-flight requests finish within a bounded grace period, then close pools. A stuck handler MUST NOT prevent exit beyond the bound; the exit status distinguishes an incomplete shutdown (as `ingest` does).

2.6 `GET /healthz` (process alive) and `GET /readyz` (database reachable and schema verified). Both unauthenticated; MUST NOT disclose versions, counts or configuration.

2.7 Database role **[APPROVED, W2]**: `USAGE` on `magneticod` and `SELECT` on `magneticod.torrents`, `magneticod.torrent_file_blobs` **and `magneticod.schema_version`** (the startup verification reads it); `USAGE` on `magnetico_web`, `SELECT/INSERT/UPDATE` on `users`, `api_tokens`, `rss_tokens`, `invites`, `SELECT/INSERT/UPDATE/DELETE` on `sessions` and `one_time_tokens`, `SELECT` on `trackers`, `settings`, `web_version`; no DDL. The administrator CLI additionally needs `INSERT/UPDATE/DELETE` on `trackers`. The exact `GRANT` statements are in `docs/web-deployment.md` §4 and are executed verbatim by the tests. A missing privilege is a controlled startup failure that names it.

2.8 Pools and deadlines. The pool has an explicit maximum. Every request has a context deadline. A search can run up to the search timeout (default 60 s), so **search concurrency is bounded separately** (a weighted semaphore of `SEARCH_CONCURRENCY` slots, bounded queue wait, then a `503` "busy") so that slow searches cannot starve authentication or other pages of connections.

2.9 **HTTP deadlines MUST NOT defeat the configured search timeout.** Go's `http.Server` is configured with `ReadHeaderTimeout`, `ReadTimeout` and `IdleTimeout`, but **no global `WriteTimeout` and no `http.TimeoutHandler`** shorter than the search route needs. Write deadlines are set per request with `http.ResponseController`: for search routes (UI and API) `SEARCH_TIMEOUT` plus a 10 s rendering margin; for all other routes `REQUEST_TIMEOUT` plus a 5 s margin **[APPROVED]**. The **handler context** deadline is exactly `SEARCH_TIMEOUT` / `REQUEST_TIMEOUT`: the margin exists only so that a timeout or error response can still be written and MUST NOT extend database or handler work after the context has expired. With the defaults the search write deadline is 70 s and the ordinary one 35 s. The effective values are logged at startup. Reverse-proxy and CDN deadlines (for example the proxy's response and idle timeouts) are outside the application's control: operators MUST keep them above `SEARCH_TIMEOUT` plus margin, and the deployment notes MUST say so. Whether a given proxy version's defaults satisfy this is **not verified** here; see `docs/web-deployment.md`.

## 3. Request pipeline

3.1 Order **[APPROVED]** (outermost first): request ID → access log (redacting) → panic recovery → security headers → indexing policy (§3.6) → client-IP resolution → cross-origin protection → authentication → rate limiting → handler. The access log deliberately wraps the recoverer so that it observes the final status (`500`) of a recovered panic; security headers are set before the handler runs, so recovered responses still carry them.

3.2 **Client IP and forwarded headers.** Only `X-Forwarded-For` is supported **[APPROVED]**; it MUST be honoured **only if the direct TCP peer address is within `--trusted-proxies`**. Otherwise it is ignored and the peer address is the client address. RFC 7239 `Forwarded`, `X-Real-IP`, `X-Forwarded-Proto` and `X-Forwarded-Host` are **not** read (absolute URLs come from `PUBLIC_URL`, §3.3); `Forwarded` support may be added only on a demonstrated need. A malformed, zoned or over-long (> 64 hops) chain falls back to the peer address. When the peer is trusted, the client address is the right-most address in the `X-Forwarded-For` chain that is not itself trusted. A direct client MUST NOT be able to change the address used for rate limiting.

3.3 **Absolute URLs** (RSS, setup/reset/verification/invite links, magnet short links shown to users) MUST be built from `--public-url`, never from the `Host` header.

3.4 **Response headers** for HTML: a CSP with `default-src 'self'`, no inline scripts, `frame-ancestors 'none'`; `X-Content-Type-Options: nosniff`; `Referrer-Policy` (`same-origin` by default, `no-referrer` where specified); `Cache-Control: private, no-store` for authenticated pages and for the session-authenticated JSON endpoints (§6.4.9); `Permissions-Policy` denying unused features. Static assets are served under content-hashed names with `Cache-Control: public, max-age=31536000, immutable`. The documentation pages (§9.4) MAY use a narrower relaxation limited to those routes.

3.5 **Panics** produce a generic 500 with a request ID; details only in the log.

3.6 **Indexing policy [APPROVED].**
- `GET /robots.txt` is public and returns, by default, exactly:
  ```text
  User-agent: *
  Disallow: /
  ```
- Every HTTP response (HTML, API, documentation, static assets, redirects, errors, `/robots.txt` itself) carries `X-Robots-Tag: noindex, nofollow` as defence in depth. HTML pages MAY also carry a matching `<meta name="robots">`.
- `robots.txt` is **not** an access-control mechanism. Private content is protected by authentication only; public routes (`/magnet/…`, documentation, RSS 401s) are discouraged from indexing by the above but are not secret.
- This release is private by default. The policy lives in one isolated middleware and one handler, with no deployment assumption elsewhere that every instance is private. Public read-only access and open registration are **not designed or implemented now**; a later release may add them and then change this policy by configuration.

## 4. Authentication and authorization

### 4.1 Policy

| Route class | Access |
|---|---|
| `/login`, `/register/{code}`, `/password/forgot`, `/password/reset/{token}`, `/password/setup/{token}`, `/email/verify/{token}` | public |
| `/magnet/{infohash}` | public (§8.3) |
| `/healthz`, `/readyz`, `/robots.txt`, static assets | public |
| `/api`, `/api/v1`, `/api/v2` documentation pages and the embedded OpenAPI files | **public [APPROVED]** |
| `/api/v1/feed/rss/last` | RSS token (§4.8.2, §8.2) |
| `/api/v2/feeds/latest.rss` | RSS token in the URL (§4.8.2, §8.2.8) |
| other `/api/v2/*` | bearer API token (§4.8.1) |
| `/torrents/{id}/files`, `/torrents/{id}/files/tree` (UI JSON endpoints, §6.4.9) | authenticated **session**; anonymous requests receive `401` `application/problem+json`, never a redirect |
| every other route | authenticated session |

Unauthenticated requests to UI pages redirect to `/login`; to the session-authenticated UI JSON endpoints and to API routes they receive `401`. Documentation pages contain no live tokens, credentials or user-specific data (§9.4).

### 4.2 Accounts and roles

4.2.1 **Account state is not an enum of lifecycle phases.** Three independent attributes **[APPROVED]**:
- `active` (boolean): administratively enabled. `active = false` MUST NOT authenticate by any means (session, password, token).
- `password_hash` (nullable): `NULL` until an initial password exists. `password_hash IS NULL` prevents **password** authentication (no empty or default password, no code path that compares against a missing hash).
- Email and its verification (§4.9), relevant only when an address is provided.

There is no `pending_setup` state.

4.2.2 Authentication MUST check `active` on **every** request that resolves a session or token, in the same lookup that resolves the credential (§4.7.5). Removing session rows alone is not sufficient, and there is no authentication cache beyond that lookup.

4.2.3 **Roles [APPROVED].** Exactly two: `user` and `admin`. This is a column with a check constraint, not an RBAC system. Registration through an invitation always creates a `user`. The operator assigns roles with the CLI (§4.10); there is no web UI for it. In this version `admin` grants no web capability (the Admin UI is a future release); it exists so that the operator can designate the first administrators. The role is read in the same lookup as `active`. The historical PHP `roles` data is **not** used to infer administrators: migrated accounts are ordinary `user`s.

4.2.4 Email is **optional for every account** **[APPROVED]**.

4.2.5 **Usernames are case-sensitive [APPROVED]**, as in the legacy application. Exact spelling and identity are preserved by migration; there is no case folding and no case-insensitive uniqueness.

### 4.3 Passwords

4.3.1 New hashes: **argon2id**, PHC string format. **[PROPOSAL]** parameters per RFC 9106 §4 second recommended option (memory 64 MiB, 3 passes, parallelism 4), confirmed by a benchmark on the target host before release.

4.3.2 New passwords: 8 to 1,024 bytes. Login accepts up to 4,096 bytes (the legacy limit). Longer input is rejected before hashing.

4.3.3 Legacy hashes are verified directly (they cannot be converted offline). A legacy format is **supported only if** it has independently verified PHP-generated test vectors (§12.2). Recognising a prefix is not support. A stored hash in an unsupported format makes the account "needs reset": it cannot log in and `web user list` flags it.

4.3.4 On successful login with a legacy or outdated hash, the hash MUST be replaced with a current-parameter argon2id hash in the same transaction that records the login, **conditionally**: the update applies only if the stored hash is still exactly the one that was verified and the account is still active, so a concurrent password change or reset is never overwritten. Hashing happens outside the transaction. **[APPROVED, W3 review] Exception:** if the limiter refuses the rehash computation (`ErrBusy`) after a successful verification, the login completes without rehash and the hash is upgraded at a later login; verification itself is never skipped.

4.3.5 **Bounded concurrency.** All hash and verify operations pass through one process-wide limiter **[PROPOSAL]** default `max(1, min(GOMAXPROCS, 2))`, a bounded queue wait (a few seconds); on expiry the request fails with a generic 503 and `Retry-After`, and a counter is incremented.

4.3.6 Login for an unknown username, for a `NULL` hash, or for an inactive account MUST perform an equivalent dummy verification (through the same limiter) and return the same generic error as a wrong password.

4.3.7 argon2/bcrypt come from `golang.org/x/crypto`. The repository carries an old indirect pin; **a maintained version MUST be selected in a dependency review and approved by the owner before use.** This is a separate prerequisite (§13.3) and does **not** block W1.

### 4.4 Sessions

4.4.1 The session token is 32 random bytes from `crypto/rand`; only its SHA-256 is stored server-side, in PostgreSQL **[APPROVED]**. A session is created at login; rotated on password change.

4.4.2 Production cookie: `__Host-magnetico_session`, `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`. MUST NOT be weakened for local testing.

4.4.3 **Development mode** exists for loopback only: accepted only when `--public-url` is an `http://` URL whose host is a loopback address; it uses a different cookie name without the `__Host-` prefix and without `Secure`; a warning is logged at startup. It MUST be rejected for any other public URL.

4.4.4 **Lifetimes [APPROVED defaults, configurable §2.3].**
- *Idle timeout* 12 h (`SESSION_IDLE_TIMEOUT`) applies to ordinary sessions: a session unused for longer is invalid.
- *Remember me* 90 days (`SESSION_REMEMBER_LIFETIME`) is an **absolute** lifetime from creation; it is not extended by activity. (The cookie carries a matching `Expires`; the server-side value is authoritative.)
- **Every session has a finite absolute expiry.** An ordinary (non-remember) session additionally has `SESSION_ABSOLUTE_LIFETIME` **[PROPOSAL: 168 h]** so that continuous activity cannot keep it alive forever; its cookie is a browser-session cookie.
- Validation (startup error otherwise): all durations positive; `idle ≥ 5m`; `idle ≤ absolute ≤ remember ≤ 8760h`. There is no "0 means unlimited".
- `last_seen_at` writes are throttled **[PROPOSAL]** to at most once per `min(5m, idle/10)`. A background purge deletes expired sessions.

4.4.4a **[APPROVED, W3 review]** Session rows are created only inside the login transaction; lookups and activity updates use the database clock. Because `last_seen_at` writes are throttled, an ordinary session's effective idle window is between `idle − throttle` and `idle` (the throttle is at most 10 % of the idle timeout). Roles are resolved on every lookup, so a role change needs no session rotation; rotation happens at login, password change and password reset only.

4.4.5 Logout is a `POST`, deletes the server-side session, and clears the cookie.

### 4.5 CSRF

4.5.1 Every state-changing browser action MUST be a `POST` (or other non-safe method) and MUST be protected, including actions that need no existing session: login, registration, password reset request, password reset, password setup, email verification confirmation, and every account and invitation action. No state change on `GET`; in particular legacy state-changing GET URLs are not carried over (§6.1).

4.5.2 Two layers: the standard library's cross-origin protection (Fetch-metadata/`Origin` based) and a synchronizer token in every form. For forms shown before a session exists, the token is bound to a short-lived pre-session cookie. **[APPROVED, W3 review]** The token is a stateless, cookie-bound construction (a domain-separated SHA-256 of the high-entropy HttpOnly cookie secret), not a server-stored value; it relies on the `__Host-` cookie prefix against cookie injection. Pre-session cookie lifetime is 1 h. After login, a validated local `next` path may be honoured; absolute URLs, `//`, `/\\` and encoded variants are rejected.

4.5.3 Bearer-token API endpoints are not subject to cookie CSRF and MUST NOT accept the session cookie as authentication. The session-authenticated UI JSON endpoints (§6.4.9) are a separate route class (`SessionJSON`): `GET`/`HEAD` only (enforced at route registration), they carry no state change and therefore need no CSRF token, they accept the session cookie only, and they are not part of `/api/v2`. The two classes share the file-view service (§6.4.7) but never an authentication mechanism.

### 4.6 Rate limiting

4.6.1 Throttling (not account lockout) with bounded in-memory state: login per client IP and per normalised username; reset and verification requests per IP and per account/email; registration per IP; failed token authentications per IP; v1 RSS and v2 per token.

4.6.1a **[APPROVED, W3 review]** Initial login limits (constants): per client IP burst 20, refill 1 per 30 s; per normalised (case-folded, hashed) username burst 5, refill 1 per 60 s; a successful login refunds the username unit; at most 10,000 keys per limiter, least-recently-used eviction.

4.6.2 Responses: `429` with `Retry-After`; error text generic. State is per process: the initial deployment is **one web instance [APPROVED]** and in-memory limiting is acceptable. Coordinating limits across several instances is a future consideration, not a 0.8.0 blocker.

### 4.7 Revocation and account state

| Event | Sessions | API tokens | RSS tokens | Invitations owned by the user |
|---|---|---|---|---|
| Disable **[APPROVED, revision 4]** | **Permanently revoked** (rows deleted/invalidated); login refused | **Temporarily unusable** (owner-active check); rows, hashes, `revoked_at` and expiry **unchanged** | **Temporarily unusable**; rows unchanged (migrated legacy RSS tokens included) | **Suspended**: rows and codes unchanged; registration through them is refused while the owner is inactive |
| Enable **[APPROVED, revision 4]** | None restored; user must log in again | **Usable again** (same tokens), unless expired or explicitly revoked | **Usable again** (same tokens and RSS URLs, migrated legacy tokens included), unless expired or explicitly revoked | Usable again automatically, unless used, explicitly revoked, or expired |
| Self-service password change | Other sessions revoked; current rotated | **Kept by default**; explicit "also revoke all API tokens" option **[APPROVED]** | **Kept by default**; a separate explicit "also revoke all RSS tokens" option | Unchanged |
| Password reset by email, **or** administrator reset/set-password, **or** completion of a setup link | **All permanently revoked** | **All permanently revoked [APPROVED]** | **All permanently revoked [APPROVED, revision 4]** (migrated legacy RSS tokens included); recovery may indicate compromise and RSS URLs carry credentials | Unchanged |
| Explicit token revocation (account UI per token; `web user revoke-tokens <username>` for all, §4.10) | unchanged | **Permanently revoked** (selected / all) | **Permanently revoked** (selected / all) | Unchanged |
| Explicit invitation revocation | n/a | n/a | n/a | Permanent (`revoked_at`); `enable` never undoes it |

4.7.0 API tokens and RSS tokens are **different credential types** and are never conflated: each is revoked by its own explicit action, listed in its own account section, and covered by its own tests. **Disable suspends access; explicit revocation or a password reset destroys credential validity.** A password reset (email or administrator, including a setup link) permanently revokes every session, API token and RSS token **[APPROVED]**; its notifications warn about both token types (§4.9.6).

4.7.1 **"Revoked" is permanent stored state; "disabled" is not.** Sessions are destroyed by `disable` and never come back: the user logs in again after `enable`. API and RSS tokens are **not** touched by `disable`: it MUST NOT write their `revoked_at`, `expires_at` or `token_hash`, and MUST NOT use a per-user epoch/generation counter or any other mechanism that would invalidate otherwise valid persistent credentials (the schema has no credential-epoch column, §5.3). While `users.active = false` every API/RSS token of that user fails authentication because of the owner-active check; after `enable` the same tokens work again unless they are expired or were explicitly revoked (or revoked by a password reset). Enabling an account MUST NOT make any explicitly revoked or expired credential, or any revoked session, valid. Disabling and re-enabling MUST NOT set, change or clear `password_hash`, email verification or `last_login_at`. A user who is disabled and re-enabled keeps working RSS URLs and need not reconfigure torrent clients.

4.7.2 **Invitation suspension** (the same suspend-not-destroy model as tokens). Disabling a user MUST NOT modify, delete or mark as revoked any invitation they own. Applies equally to migrated legacy invitations.

4.7.3 **Registration is one transaction** that (a) locks the invitation row (`FOR UPDATE`) and its owner's user row (`FOR SHARE`); (b) verifies that the invitation exists, is unused, is not revoked, is not expired, and that its owner is `active`; (c) creates the account (role `user`); (d) marks the invitation used. `disable` MUST take `FOR UPDATE` on the same user row. Consequently a registration MUST NOT succeed after a disable has committed, and a rejected registration MUST NOT consume or mark the invitation.

4.7.4 The registration page MUST show one generic message for every non-usable invitation (unknown, used, revoked, expired, owner disabled): *"This invitation is invalid or no longer available."* It MUST NOT reveal that the owner is disabled.

4.7.4a A request authorised before a revocation commits may complete; the next request MUST observe the revocation. No authentication result is cached.

4.7.5 **Per-request credential checks [APPROVED, revision 4].** Every authenticated API and RSS request MUST, in one server-side lookup, verify: (1) the credential exists (by SHA-256 digest); (2) it is not expired; (3) `revoked_at` is null; (4) its owner is currently `active`. There is no authentication cache that could outlive a disable; if one is ever proposed its lifetime must be bounded and approved separately. `disable` takes effect for requests authorized after its transaction commits; a request authorized before the commit may complete (§4.2.2). Tests: §12.3, §12.4b.

### 4.8 Credentials: API tokens and RSS tokens **[APPROVED]**

The service has two logically **and physically separate** credential stores in `magnetico_web`, each with its own creation, revocation, metadata and authentication code. There is no generic role/scope/permission framework: two tables, two verifiers. **An RSS token never authenticates against the general API and an API token never authenticates against RSS.**

**4.8.1 API tokens (`api_tokens`)** — credentials for the JSON REST API.
- Format **[PROPOSAL]** `mgt_` + base64url of 32 random bytes; only the SHA-256 is stored, with label, creation time, throttled last-use time, optional expiry, `revoked_at`; the plaintext is shown **once**.
- Presented as `Authorization: Bearer <token>` **only**; never in query strings or cookies. Accepted by `/api/v2/*` except the feed endpoint.
- Created only by an explicit user action on the account page after signing in to the new application. **Never** created automatically at login or by the migration.
- **[IMPLEMENTED W6/W7]** At most 50 unrevoked tokens per type per user (counted under the user row lock; revoked rows do not count), creation limited per account and type to a burst of 10 and one more every 6 minutes. There is no lifetime cap on revoked rows (§14.5).

**4.8.2 RSS tokens (`rss_tokens`)** — credentials exclusively for RSS feed access.
- New tokens: `mgf_` + base64url of 32 random bytes (**[APPROVED]** recommended format); only the SHA-256 is stored; **read-only**; shown in plaintext **once**, immediately after creation, together with a ready-to-copy RSS URL (§8.2.8). Per-token label, `created_at`, throttled `last_used_at`, optional `expires_at`, `revoked_at`. A user may hold many. Created and revoked by `POST` with CSRF.
- Accepted only by the two RSS endpoints (§8.2); never by any other route.
- Authentication looks the credential up by its SHA-256 digest and compares digests in constant time (`crypto/subtle`) wherever the application compares them; the secret itself is never compared or stored.

**4.8.3 Legacy token migration (specified here for the later migration task).** Every legacy PHP API token migrates **exclusively into `rss_tokens`**; the migrator creates **no** `api_tokens`. For each token:
1. keep the owner relationship;
2. keep it valid on existing RSS URLs;
3. store only `SHA-256(legacy_token)` — the migrator reads the plaintext from the source database during the run and keeps no plaintext in the target;
4. set `legacy = true` and preserve the original creation timestamp;
5. generate no replacement token;
6. never expose the value: the account page lists these entries **masked** ("legacy token", creation date, revoke action) and MUST NOT imply the value can be displayed.

Existing clients using `/api/v1/feed/rss/last?api-token=<legacy-token>` keep working unchanged. A legacy token **MUST NOT** authenticate against `/api/v2/*`; a test asserts both the positive RSS case and the negative API case. Legacy RSS tokens work until explicitly revoked, revoked by a password reset, or until a future, separately approved compatibility removal. They are subject to the same rules as any RSS token (§4.7): `disable` suspends them (they and the unchanged v1 URL work again after `enable`), whereas a password reset or explicit revocation destroys them.

**4.8.4** Token creation and revocation are `POST`s with CSRF; there is no token-minting-by-password endpoint. Rate limiting per token and per client IP on failures (§4.6).

### 4.9 Invitations, password links, email and SMTP

**Invitations [APPROVED].**

4.9.1 Invitation codes are stored in **plaintext** so that the owner can see and copy their unused invitation links at any time; migrated legacy invitations remain available. The threat model accepts plaintext codes at rest for this purpose. Therefore: codes MUST NOT be logged (the register path segment is redacted); an invitation is visible only to its owner (and, through the CLI, to the operator) — never to other users; creation, copy-view, and revocation are authenticated `POST`/`GET` pages protected by CSRF for the state changes; one-time consumption (§4.7.3) and explicit revocation are enforced in the database (`used_by` settable once; `revoked_at`). Pages that display codes are `Cache-Control: no-store`, and `/register/{code}` uses `Referrer-Policy: no-referrer`.

4.9.2 An invitation owner who is disabled has their unused invitations suspended and restored as in §4.7. New codes: 16 random bytes or more, URL-safe; legacy 32-hex codes keep working.

**Password links (setup and reset).**

4.9.3 `web user add <username>` creates an **enabled** account with `password_hash = NULL` and prints a single-use, expiring **password-setup link**. The administrator never learns the password. Expired links are re-issued with `web user reset-password <username>` (§4.10); each new link invalidates earlier ones for that account. Administrator-issued links work for accounts **without an email address**.

4.9.4 Link tokens: 32 random bytes, stored as SHA-256, single use, expiring (**[PROPOSAL]** setup 72 h, reset 1 h, email verification 24 h), bound to one account, purpose `setup` | `reset` | `email_verify`.

4.9.5 **Completing a setup or reset link** runs in one transaction that locks the user row, checks the token, sets `password_hash`, records `password_changed_at`, consumes the token by deleting it (and deletes the user's other outstanding password links), and applies the revocations of §4.7 (all sessions, all API tokens and all RSS tokens). It MUST NOT write `active`. If the account is inactive, the link is refused with the same generic message as an invalid link and **is not consumed** (it works again after `enable` unless it expired). Completing a link for an account whose `active` is false therefore can never reactivate it. Setting a password MUST NOT affect email verification, and email verification MUST NOT set a password.

4.9.6 **[APPROVED]** The reset email and the confirmation page MUST each state explicitly that completing the reset (a) **invalidates all existing API tokens**, (b) **stops all existing RSS tokens and RSS URLs from working**, and (c) means the user **must generate new credentials and update their torrent clients**; both credential types are named separately. The CLI output of `reset-password` states the same to the operator.

**Email [APPROVED].**

4.9.7 An address is **unverified** until its holder confirms a single-use link sent to it (`/email/verify/{token}`, `POST` to confirm). Only a **verified** address is used for self-service password recovery. An address supplied by the administrator or migrated is stored unverified; the owner may request verification from the account page when SMTP is configured. The handling of historical email addresses during migration is a separate decision (§13.3).

4.9.8 **Changing an address**: the new address is held as *pending* (in the verification token), a link is sent to it, and only its confirmation replaces `users.email` and sets `email_verified_at`. The existing verified address stays in force until then; abandoning or letting the link expire changes nothing. **[APPROVED]** the change requires the current password; **[PROPOSAL]** a notice is sent to the previous verified address when the change completes. At most one account may hold a given verified address (partial unique index on `lower(email)` where verified).

4.9.9 `POST /password/forgot` is available **only when SMTP is configured**, MUST return the same response and similar timing whether or not the address exists, is verified, or the account is active: *"If the address you entered is registered and verified, a password reset link has been sent."* Mail is handed to a bounded in-process queue so that SMTP latency cannot reveal existence; the lookup-and-enqueue path has the same shape in all cases. Requests are rate limited (§4.6). Users without a verified address cannot self-serve and need an administrator.

4.9.9a **[IMPLEMENTED W6]** The forgot-password handler touches neither the database nor SMTP: it validates the address syntax, applies the per-IP and per-address limits, reserves a slot in the mail queue without blocking and hands over a job that carries only the normalised address. The worker looks the address up (verified address, active account), issues the link (reset, or setup for an account without a password) and sends. A full or closed queue, an exhausted per-address limit and a malformed address all end in the same page; only the per-IP limiter answers differently (`429`), independently of the address. Measured handler time by account class: `docs/wip/07G-web-account-api-report.md`.

4.9.10 **SMTP transport [APPROVED].** Standard authenticated submission; **encrypted, certificate-verified transport is mandatory before any credential, reset link or sensitive content is sent.** Supported modes: `starttls` (explicit STARTTLS on submission, e.g. port 587) and `tls` (implicit TLS, e.g. port 465). There is no plaintext mode and no opportunistic fallback: if STARTTLS is not offered, or the TLS handshake or certificate verification fails (`ServerName` = configured host, system roots, TLS ≥ 1.2, no `InsecureSkipVerify` option), the send fails and nothing sensitive is transmitted; `AUTH` is never issued before TLS is established, including to loopback hosts. **[PROPOSAL]** implement with the standard library (`net` / `crypto/tls` / `net/smtp.NewClient`, explicit `StartTLS`); `net/smtp.SendMail` MUST NOT be used because it falls back to plaintext when STARTTLS is absent. A third-party SMTP library is added only if a concrete required capability is missing, with owner approval. Headers are built from validated values (no CR/LF injection), plain-text bodies, connection and total timeouts, no secrets in logs.

### 4.10 Administrator CLI

`magnetico web user …` operates directly on the web store (database URL from the environment):

| Command | Behaviour |
|---|---|
| `add <username> [--email E] [--invites N]` | Creates an **enabled** `user` with `password_hash NULL` and prints a single-use expiring setup link. `--invites` overrides the configured default quota. `--email` is stored unverified. |
| `set-role <username> <user\|admin>` | Assigns or changes the role **[APPROVED]**. |
| `reset-password <username>` | Issues a link (purpose `setup` if no password exists, otherwise `reset`), invalidating earlier links; prints it; works without an email address; completion permanently revokes sessions, API tokens and RSS tokens (§4.7). |
| `set-password <username>` | Rare case. Password from a prompt or stdin; never argv; never echoed or logged. Permanently revokes sessions, API tokens and RSS tokens (§4.7). |
| `disable <username>` / `enable <username>` | Per §4.7. `disable` sets `active = false`, destroys sessions and **suspends** (does not revoke) API tokens, RSS tokens and unused invitations; `enable` sets `active = true` and restores those that are not expired or explicitly revoked, never sessions. Neither touches `password_hash`, token rows or invitation rows. |
| `revoke-tokens <username>` | Permanently revokes **all** API and RSS tokens of the user (sets `revoked_at`; irreversible, never undone by `enable`), for security incidents. Separate from `disable`; does not touch sessions, the password or `active`. Prints the number revoked per type. |
| `invite add\|revoke …` | Create or permanently revoke invitations. |
| `list` | Username, role, `active`, password present/legacy-format/needs-reset, email present/verified. No secrets. |

Commands MUST NOT print secrets other than the single link or token they were asked to issue. **Tracker management [APPROVED]** — a minimal interim operator CLI until the Admin UI exists: `web tracker list`, `web tracker add <url> [--position N]`, `web tracker remove <url|id>`, `web tracker move <url|id> <position>`; details are specified in W2. It validates URLs (§8.3.2) and invalidates the tracker cache. No Admin UI is implemented now.

## 5. Web schema (`magnetico_web`; proposal, not applied)

5.1 **[APPROVED]** schema name `magnetico_web`. It holds accounts, authentication state, sessions, invitations, API tokens, RSS tokens, the public tracker list and future web-specific settings. It is created by the explicit, non-destructive, idempotent command **`magnetico migrate web-schema-init`** (never implicitly by `web`; database from `--database` / `MAGNETICO_MIGRATE_DATABASE` / `migrateDatabaseURL`) with its own version table **`web_version`**. The DDL (version 1) is `internal/web/webschema/ddl.go`. The version table is not called `schema_version` because the torrent-schema verification treats a table of that name in any schema as one of its own markers. The torrent/file identifiers of `magneticod` are unchanged.

5.2 **Identifier strategy [APPROVED].** Historical numeric user and invitation IDs are **preserved**; no UUID/ULID without a concrete requirement.
- `users.id`, `invites.id`: `bigint GENERATED ALWAYS AS IDENTITY`; **legacy values are preserved verbatim** (the migrator inserts with `OVERRIDING SYSTEM VALUE`, then advances each sequence past the maximum, e.g. with `setval`; foreign-key consistency is verified after the run). The mapping is the identity, so no mapping table exists and the check is simple: row counts per table, foreign-key integrity, per-user invitation and token counts equal to the source.
- `api_tokens.id` and `rss_tokens.id`: new surrogate identities (the legacy primary key is the secret itself and must not be reused as an identifier). For migrated legacy tokens the link to the source is the verifiable relation `token_hash = sha256(legacy key)` in `rss_tokens`.
- `sessions` and one-time tokens are keyed by their token hash; `trackers.id` is a new identity.
- **UUIDs are not used**: identifiers are not exposed publicly (the only identifier-bearing routes are owner-checked `POST`s), there is no multi-writer or merge scenario, and a changed key type would force a persistent old-to-new mapping with no benefit.

5.3 Logical tables (types and indexes fixed in the DDL review):
- `users`: id; username; email (nullable); email_verified_at (nullable); password_hash (nullable); `active` boolean not null default true; `role` text not null default `'user'` with `CHECK (role IN ('user','admin'))`; created_at; password_changed_at; last_login_at. **No `pending_setup` state and no credential-epoch column.**
- `sessions`: token_hash (PK), user_id, created_at, last_seen_at, expires_at (**not null**, absolute), remember.
- `api_tokens`: id, token_hash (unique), user_id, label, created_at, last_used_at, expires_at, revoked_at. JSON REST API only.
- `rss_tokens`: id, token_hash (unique), user_id, label, created_at, last_used_at, expires_at, revoked_at, `legacy` boolean. RSS feeds only. Physically separate from `api_tokens`; no shared scope column.
- `invites`: id, owner_id, code (plaintext, unique **[APPROVED]**), used_by, used_at, created_at (`NOT NULL DEFAULT now()`; a migrated legacy invitation, which has no creation time, takes its owner's `users.created_at` — an intentional approximation, not an exact reconstruction), expires_at (nullable), revoked_at.
- `one_time_tokens`: token_hash (PK), user_id, purpose (`setup` | `reset` | `email_verify`), email (target address, for `email_verify`), created_at, expires_at. **No `used_at` and no history:** a row exists only while the link is outstanding; successful consumption, replacement and revocation all `DELETE` it, in the transaction of the operation they authorize. A plain UNIQUE index allows at most one outstanding password link (`setup`|`reset`) and one outstanding email link per user, so issuance deletes the user's older link of that kind (even if expired) first, with the user row locked.
- `trackers`: id, url (unique), position, created_at, updated_at (§8.3).
- `settings`: key, value, updated_at (holds, among other future values, the tracker-seed marker).
- `web_version` (exactly one row, the supported version `1`).

5.5 **Structural verification and lifecycle enforcement [APPROVED, W2].** `web-schema-init` and the startup verification apply the same targeted catalog check: exact relation set, columns (type, nullability, identity, defaults), primary/unique/foreign-key/check constraints and indexes by name and definition, validity (no `NOT VALID`, no deferrable constraint), no row-level security, no unexpected triggers, and exactly one `web_version` row `{1}`. Any deviation fails closed with an error naming the objects; an existing incompatible schema is **never repaired or modified**. The schema contains **no triggers and no PL/pgSQL**: permanent revocation (`revoked_at` set only by a conditional `UPDATE … WHERE revoked_at IS NULL`), set-once invitation consumption (row lock plus `used_by IS NULL`) and one-time link consumption (row lock plus `DELETE … RETURNING`) are enforced by the stores and proven by concurrency tests; the database enforces the declarative invariants (keys, foreign keys, uniqueness, checks, NOT NULL). Foreign keys are `NO ACTION`.

5.4 Database-level invariants: an invitation is used at most once; `role` is one of two values; at most one account per verified email; session `expires_at` not null. Registration and disable semantics (§4.7.3) rely on row locking.

## 6. UI routes

### 6.1 Route table

| Method and path | Auth | Purpose |
|---|---|---|
| `GET /{$}` | session | Homepage and entry point: redirect to `/torrents` when signed in, otherwise to `/login`. Exact root only; unknown paths are 404. **Interim (W3, until W4):** a signed-in user sees a minimal page with the logout form |
| `GET, POST /login`; `POST /logout` | public / session | Sign in / out |
| `GET /torrents` | session | Search and listing |
| `GET /torrents/{id}` | session | Details; hosts the file browser (§6.4) |
| `GET /torrents/{id}/files/tree` | session (JSON) | Complete file tree, only for eligible torrents (§6.4.9) |
| `GET /torrents/{id}/files?dir=&offset=&limit=` | session (JSON) | One page of one directory, for every torrent (§6.4.9) |
| `GET, POST /register/{code}` | public | Invite registration |
| `GET, POST /password/forgot`, `/password/reset/{token}`, `/password/setup/{token}` | public | Reset and setup |
| `GET, POST /email/verify/{token}` | public | Email verification |
| `GET /account?tab=profile\|invitations\|rss\|api`; `POST /account/password`, `/account/email`, `/account/email/resend`, `/account/api-tokens`, `/account/api-tokens/{id}/revoke`, `/account/rss-tokens`, `/account/rss-tokens/{id}/revoke`, `/account/invites/{id}/revoke` | session | Account management **[IMPLEMENTED W6]** (there is no invitation-creation route, §14.2) |
| `GET /magnet/{infohash}` | public | Short magnet redirect (§8.3) |
| `GET /api`, `/api/v1`, `/api/v2`, `/api/v1/openapi.yaml`, `/api/v2/openapi.yaml` | public | API documentation |
| `GET /healthz`, `/readyz`, `/robots.txt` | public | Operations |

6.1.1 **No legacy UI redirects [APPROVED].** The legacy site was private and not indexed and has no known scraping consumers. The new UI may reorganise or rename routes freely; internal navigation starts at `/`. The only routes retained for external compatibility or functionality are **`/api/v1/feed/rss/last`** and **`/magnet/{infohash}`**. No redirects are provided for `/auth/login`, `/auth/reset/*`, `/profile/api/*`, or any other old route, and state-changing legacy `GET` routes are not served or redirected (they return 404).

### 6.2 Search

6.2.1 **[APPROVED]** Query text is split on whitespace. A word shorter than 3 **Unicode code points** is ignored (legacy `mb_strlen` semantics; the Go implementation counts runes). All remaining words must match (AND), case-insensitively, as substrings of `torrents.name`.

6.2.2 **[APPROVED]** `%` and `_` typed by the user are escaped and matched **literally** (helper `escapeLikePattern`). This is a deliberate difference from the legacy code, where they acted as wildcards.

6.2.3 **[APPROVED]** A query that is exactly 40 hexadecimal characters is an exact info-hash lookup. An empty query (or one with no usable word) lists the newest torrents.

6.2.4 Search operates on `torrents` only and MUST NOT decode blobs (`postgresql-persistence-v2.md` §3, §5).

6.2.5 **Timeout [APPROVED].** The SQL search timeout defaults to **60 s** and is configurable via `MAGNETICO_WEB_SEARCH_TIMEOUT` (§2.3). It is enforced by the request context and a server-side statement timeout, and §2.9 guarantees HTTP deadlines do not cut it short. The default is deliberately long because sorting very large result sets can be expensive. On expiry the UI and API report a friendly **search timeout** (UI page; v2 `application/problem+json`, status 503, `Retry-After`) that says the search did not finish in the configured time and may be retried. The message MUST NOT claim the query is invalid or "too broad". A cancelled request (client disconnect) is logged at debug level, not as a timeout. Saturated search slots produce a different "busy" response.

6.2.6 **Indexes.** No new index is added to support search or listing without (a) query-plan and load measurements on a disposable realistic dataset showing a proven gain, and (b) owner approval, because indexes cost storage and the PHP version did not have them **[APPROVED policy]**. Existing indexes are used as they are.

### 6.3 Ordering and pagination

6.3.1 Sorts: `discovered` (default, descending), `size`, `name` (only with a query). Every sort uses `id` as a tie-breaker so ordering is deterministic. Unknown values fall back to the default.

6.3.2 **Keyset pagination [APPROVED]**: opaque cursors, `LIMIT n+1` to learn whether another page exists, **no `COUNT(*)`, no `OFFSET`** — this preserves the legacy property (the PHP version used a pageless paginator precisely to avoid counting). The UI MUST NOT display a total result count; page sizes 20, 50, 100.

### 6.4 Details and file browser

6.4.1 `GET /torrents/{id}` renders the torrent metadata immediately from the `torrents` row. It additionally asks the file-view service for the browsing mode (§6.4.7), which costs at most one bounded read of the blob's first 12 bytes (one TOAST page, `FileHeader`); it never decodes a list, builds an index or serializes a tree. If that peek fails or times out (sub-deadline 1 s) the page still renders in full with mode `dirs` and the failure is logged by category. File data is loaded afterwards by the browser, asynchronously and independently of the metadata. Unknown ID → 404.

6.4.2 The file list comes from the persistence layer's `GetFiles` semantics (`postgresql-persistence-v2.md` §5): whole list or a typed error, never partial. Missing/corrupt blob → the JSON endpoints answer `500` `application/problem+json` (and the file browser shows an error with no partial list), `ERROR` log with `torrent_id`, no silent repair; the details page itself keeps working.

6.4.3 **Display limits and resource limits are separate.** *Display:* directory pages return at most 1,000 entries (the UI asks for 160) and never more than 1 MiB of JSON; directories carry their recursive file count and size. *Resources:* decoding the full list and building or looking up its index is bounded by the codec limits (`codec.DefaultLimits`: 250,000 files, 64 MiB uncompressed), the process-wide weighted decode gate, per-request deadlines, the size-bounded index cache, and the **web index complexity limits** of §6.4.8. A directory navigation never repeats an unbounded decode: all pages of a torrent are served from one cached index.

6.4.4 **Benchmark targets [APPROVED as targets, not release gates; unmeasured].** To be measured in W4 on **disposable synthetic** data (generated by the test, no real or network data), on a stated reference host (**[PROPOSAL]** 2 vCPU, 1 GiB container), single web process, idle database:

| Metric | Provisional budget |
|---|---|
| Cold decode + index build, one torrent of ≈ 250,000 files (≈ 64 MiB uncompressed), idle process | p95 ≤ **2 s** wall time; bytes allocated ≤ **3×** the uncompressed list size |
| Cached directory navigation (page of ≤ 500 entries), server time | p95 ≤ **50 ms**, p99 ≤ **150 ms**; **zero** decodes |
| 100 consecutive navigations in one large torrent | exactly **1** decode (99 cache hits) |
| 8 concurrent requests (mixed cold/cached, large torrents) | no errors; p95 ≤ **5 s** |
| 32 concurrent requests (mixed cold/cached, large torrents) | no OOM or crash; successful-request p95 ≤ **15 s**; excess requests receive a bounded-wait `503` "busy" (queue wait ≤ **5 s**) rather than unbounded memory growth |
| Peak process RSS during the 32-concurrent run | ≤ **640 MiB** total (leaving headroom in a 1 GiB container); decode working set bounded by the weighted semaphore (**[PROPOSAL]** ≤ 256 MiB) and cache ≤ **[PROPOSAL]** 128 MiB |
| Allocations per cached navigation request | ≤ **10,000** allocations and ≤ **1 MiB** |

These numbers are accepted by the owner as **benchmark targets, not hard release gates [APPROVED]**; they are not measured achievements. W4 MUST measure cold decode and index construction, cached navigation, 8/32 concurrent requests, peak memory and allocations. If a target is missed, W4 reports the measurements and identifies the bottleneck; it does **not** automatically redesign the codec or the persistence schema. The owner selects the final solution (plan §6.4 lists alternatives) from the measurements and practical resource needs; any schema or codec change requires separate owner approval.

6.4.5 **Fidelity.** Duplicate `(path, size)` entries are preserved as distinct entries, in canonical order, each with its canonical position (`file_index`). Paths are opaque byte strings (`wire-format-v1.md` §1): splitting on `/` is for grouping only; empty components, leading `/`, `.`/`..`, invalid UTF-8 and very long components are carried and shown with explicit markers, never normalised, merged or dropped. **Directories are addressed by deterministic positional paths (§6.4.8), not by path bytes**: no percent-encoded byte path, temporary identifier, server-side registry, session mapping or per-client state exists, so no byte sequence needs to survive a URL and the W4 request-line limitation for long directory links is gone. Names travel in JSON losslessly (§6.4.9) and are rendered with `textContent` only.

6.4.6 **Invalid UTF-8 in the page [APPROVED].** The page shows a clearly identified, lossless representation: each invalid byte as an escaped sequence (`\xNN`) in a visibly marked span, valid parts as text; control characters, bidi formatting characters and a literal U+FFFD as `\u{XXXX}` markers. It never substitutes U+FFFD. The rendering rule is implemented twice (Go `pathfmt.Display` for server-rendered names, `app.js` for file names) and both are pinned to the same vectors (`internal/web/pathfmt/testdata/name-vectors.json`).

6.4.7 **One file-view service, two operations [APPROVED W5].** The package `internal/web/fileview` is the single implementation of file browsing. The session-authenticated UI endpoints (§6.4.9) and the future bearer-authenticated `/api/v2` endpoints (§7.5) are thin authentication adapters over it; both reuse the persistence reader, the Arena index, the LRU cache and weighted decode gate (`filecache`), eligibility, directory resolution, pagination and serialization limits. There are two independent operations:
- **Tree**: the entire hierarchy in one JSON document, only for torrents that qualify.
- **Directory page**: a bounded page of one directory, for **any** torrent, with no minimum file count.

*Mode selection is the backend's decision.* The details page carries it as `data-mode="tree"` or `"dirs"`; the browser never estimates a size or chooses a mode. A client may always choose directory pages regardless of the mode.

*Eligibility has two stages.* (1) A cheap, conservative **pre-check** (`Decide`): single-file torrents are eligible; torrents with more than `MaxTreeFiles` files are not; otherwise the blob's declared payload length is read from its 12-byte header and `estimate = 96 × files + declared_bytes` must not exceed `MaxTreeEstimate`. It never decodes. The estimate is **not** an upper bound: prefix-delta and trie encodings and directory-heavy hierarchies can expand well beyond it (measured ratio actual/estimate: 0.68–0.83 for ordinary shapes, 1.54 for a directory-heavy adversarial list; `docs/wip/07F-w5-measurements/tree-size.txt`). (2) The **exact serialized size** is enforced while building the document in a bounded buffer; if the next byte would exceed `MaxTreeBytes` the request fails with a typed error **before any byte or status is sent**. `full_tree_available` / `data-mode` is the outcome of stage 1; stage 2 is authoritative. A rare false-positive estimate therefore yields a typed `422` that makes the client switch to directory pages, never an oversized response. Endpoints re-run stage 1 themselves, so a caller cannot bypass the thresholds by calling the endpoint directly.

*Thresholds [OWNER-SET after W5 review] (defaults, `fileview.Default*`):* `MaxTreeFiles` = 4,000; `MaxTreeEstimate` = 1 MiB; `MaxTreeBytes` (hard safety cap, unchanged) = 4 MiB; `MaxPageBytes` = 1 MiB; encode budget = 8 concurrent responses × `MaxTreeBytes` (unchanged: with the lower file limit an ordinary tree is ≈ 0.3–0.5 MB, the worst measured 4,000-file shape 0.7 MB, so the budget is already generous and no increase is justified). A 4,000-file torrent is ≈ 0.45–0.5 MB of JSON; 12,000 files ≈ 1.4 MB; 250,000 files ≈ 31 MB (`docs/wip/07F-w5-measurements/tree-size.txt`). The conservative values are those of the approved UI design; torrents above them use directory mode.

*Encode admission.* Serialization has its own weighted gate, separate from the 192 MiB decode gate (which keeps protecting decoding and index builds only); a response holds `MaxTreeBytes` of that budget until its bytes were written. Waiting respects the request context; saturation gives `503` with `Retry-After`. Large pooled buffers are not retained. No intermediate object graph is built.

6.4.8 **Positional directory paths [APPROVED W5].** A directory is addressed by its position in the index's deterministic hierarchy: `""` is the root; otherwise decimal components joined by `_` (e.g. `2_5_6`). Component *i* is the index of the subdirectory within its parent's listing, where subdirectories are listed first, sorted by name bytes (hence stable for an immutable blob). Grammar: `^(0|[1-9][0-9]{0,5})(_(0|[1-9][0-9]{0,5}))*$`, at most `MaxDepth` (256) components; only directories can be addressed (a component ≥ the parent's subdirectory count is `404`; a malformed path or one deeper than the limit is `400`). Directory positions and canonical file positions (`file_index`) are separate namespaces. Torrent file metadata is immutable under the normal ingest contract, so no client-side blob-version protocol and no `409` recovery exist; if a blob were replaced, positions are interpreted against the new list.

*Index complexity limits (web resource limits, not codec validity):* the web index refuses a hierarchy with more than `MaxDirs` = 250,000 directories or deeper than `MaxDepth` = 256, checked incrementally during the scan, before the index arrays grow (measurements: ≤ 17 MiB index at the limit; `docs/wip/07F-w5-measurements/index-complexity.txt`). A valid torrent beyond the limits stays visible in search and its details remain accessible; **both** file operations answer `422 index-too-complex` (§6.4.13). This is an intentional resource-safety limitation, not a corruption classification, and it cannot be bypassed by switching modes because both modes use the same index. Because the refusal is deterministic for an immutable blob version, `filecache` remembers it for at most 5 minutes (at most 1,024 entries, keyed by torrent id, file count, total size and blob version, so a replaced blob is examined again); transient, cancelled, storage and corruption errors are never remembered.

6.4.9 **JSON contracts (UI endpoints) [APPROVED W5].** Both routes are `GET`/`HEAD` session routes (§4.1, §4.5.3); responses are `application/json` or `application/problem+json`, `Cache-Control: private, no-store`, `X-Content-Type-Options: nosniff`, `X-Robots-Tag: noindex, nofollow`, with a `Content-Length`. Documents are RFC 8259 JSON produced by a dedicated writer that follows the `encoding/json` string-escaping rules (no Go-literal escapes) and are verified with `json.Valid` and round trips. Unknown query parameters are ignored; repeated `dir`/`offset`/`limit` parameters, malformed escapes and non-canonical numbers are `400`.

*Names.* Every named entry carries `"name"` (a string) when the bytes are valid UTF-8, otherwise `"name": null` and `"name_base64"` (RFC 4648 §4 standard alphabet, with padding) holding the exact bytes. An empty component is `"name": ""`. Nothing is converted to U+FFFD. (This is the §7.3 `path`/`path_base64` convention applied per component.)

*file-tree v1* — `GET /torrents/{id}/files/tree`:
```json
{"version":1,"file_count":N,"size":S,
 "root":{"file_count":N,"size":S,
         "dirs":[{"name":…,"file_count":n,"size":s,"dirs":[…],"files":[…]}],
         "files":[{"name":…,"size":n,"file_index":p,"duplicate":false}]}}
```
`dirs[i]` has positional component *i*; its path is derived (parent path + `_i`) and not repeated. `files` are in canonical order; `file_index` is the canonical position (the identity of duplicates); `duplicate` is true when the name equals the previous file entry's name in the same directory. Aggregates are recursive.

*file-dir-page v1* — `GET /torrents/{id}/files?dir=<positional>&offset=<n>&limit=<1..1000>` (defaults: root, 0, 160):
```json
{"version":1,"path":"2_5","ancestors":[{"name":…,"path":"2"},{"name":…,"path":"2_5"}],
 "dir":{"file_count":n,"size":s,"dir_count":d,"entry_count":e},
 "offset":o,"limit":l,"next_offset":k,
 "entries":[{"type":"dir","name":…,"path":"2_5_0","file_count":n,"size":s,"dir_count":d,"entry_count":e},
            {"type":"file","name":…,"size":n,"file_index":p,"duplicate":false}]}
```
Entries are subdirectories (by name bytes) then files (canonical order). `entry_count` is the number of listing rows of a directory (`dir_count` + immediate files). A page never exceeds `MaxPageBytes`: when the size bound of the requested rows would, the page is cut (never below one entry) and **`next_offset`** is the offset of the first row not returned, or `null` at the end. Clients MUST continue with `next_offset`, not `offset + limit`. An offset beyond the end is a valid empty page.

6.4.10 **Logging and caching.** The JSON endpoints log only the request ID, torrent ID, operation, outcome category and response size; never names, paths, `dir` values or tokens. Responses are never cacheable by proxies (`private, no-store`); the immutable-asset policy applies to static files only. No proxy response caching may be enabled for these routes.

6.4.11 **Front end [APPROVED W5].** `app.js` (one first-party file, no framework, CSP-compatible) renders the browser into `#file-browser` on the details page:
- *Tree mode:* fetch the tree once; folders collapsed unless the torrent has at most 20 files in total; expand/collapse and local filtering (case-insensitive, file names, matches keep their ancestors, the manual expansion state survives a filter session) make no request; at most 120 rows are in the DOM, extended 120 at a time; names via `textContent`.
- *Directory mode:* breadcrumbs, up, previous/next, explicit loading/error/retry states, requests are aborted when superseded, a stalled request times out after 30 s; directory changes never navigate the page and never add history entries (`history.replaceState` edits only `dir` and `offset` and keeps other parameters, the fragment and unrelated history state; a reload restores a valid location — including an offset of the root directory — and an invalid or stale one falls back to the root with a notice). *Previous:* the page returns to the exact earlier page while the view remembers it; after a reload the earlier offsets are unknown, so the client requests `max(0, offset − limit)` and, because byte-limited pages may hold fewer entries than `limit`, moves the start closer until the returned page reaches the offset it came from (no entry between the two pages is ever skipped; the page may overlap the one the user left).
- A `422 full-tree-unavailable` from the tree endpoint switches to directory mode; `422 index-too-complex` is final (no retry, no mode switch); `401` offers sign-in; `503` and timeouts offer retry.
Limitations: no virtualised scrolling (chunked rendering), no filter in directory mode (listing is server-side and search never scans paths), deep trees are not exposed to assistive technology as an ARIA tree (a list of rows with `aria-expanded` folder buttons), and phone performance with thousands of rows is unmeasured.

6.4.12 **No-JavaScript behaviour [APPROVED W5 exception].** Search, details, sign-in and sign-out work without JavaScript. The file browser requires it: without JavaScript the Contents panel shows a clear notice and no HTML file tree is rendered (the old `/torrents/{id}/files` HTML directory pages were removed). This is an explicit exception to §11.1.

6.4.13 **Errors** (`application/problem+json`, RFC 9457; `type` is a stable URN `urn:magnetico:problem:<slug>`):

| Condition | Status | Slug |
|---|---|---|
| anonymous request | 401 | `unauthenticated` |
| unknown torrent or directory | 404 | `not-found` |
| malformed `dir`, `offset`, `limit` or query | 400 | `invalid-parameter` |
| tree not available (file count, estimate or exact size) | 422 | `full-tree-unavailable` (+ `alternate`: the directory-page URL) |
| hierarchy beyond the web index limits | 422 | `index-too-complex` |
| decode or encode capacity saturated, deadline, storage I/O, blob replaced mid-request | 503 + `Retry-After` | `temporarily-unavailable` |
| missing or corrupt blob, list too large to index | 500 | `internal-error` |
| too many requests (API v2 and the v2 feed; **added in 07G**) | 429 + `Retry-After` | `rate-limited` |

Bodies never carry names, paths or internal detail.

## 7. API v2 (draft)

7.1 Base `/api/v2`. Authentication: `Authorization: Bearer <token>` only **[APPROVED]**. Errors: RFC 9457 `application/problem+json`. Fields `snake_case`; timestamps RFC 3339 UTC; `Cache-Control: private`.

7.2 Endpoints **[PROPOSAL]**:

| Endpoint | Result |
|---|---|
| `GET /torrents?q=&sort=&order=&limit=&cursor=` | `{items: [torrent], next_cursor}`; `limit` ≤ 100 |
| `GET /torrents?info_hash=<40 hex>` | zero or one torrent |
| `GET /torrents/{id}` | torrent, including `magnet_uri` and `full_tree_available` (§7.5) |
| `GET /torrents/{id}/files/tree` | the complete hierarchy (file-tree v1, §6.4.9), only if eligible; else `422` (§7.5) |
| `GET /torrents/{id}/files?dir=&offset=&limit=` | one page of one directory (file-dir-page v1, §6.4.9); `limit` ≤ 1,000; available for every torrent |
| `GET /feeds/latest.rss?feed_token=` | RSS of the newest torrents; **RSS-token** authentication through the URL (§8.2.8). The one documented exception to header-only authentication |

Torrent object: `id`, `info_hash` (hex), `name`, `total_size`, `file_count`, `discovered_at`; the single-torrent response adds `magnet_uri` and `full_tree_available` (boolean).

The W4-era proposal of `/files` as a flat canonical list with positional cursors is **replaced** by the directory-navigation contract above; none of these endpoints has been released.

7.3 **File paths [APPROVED contract].** A file entry is `{ "path": <string|null>, "path_base64": <string, present only when path is null>, "size": <integer> }`:
- Valid UTF-8 path → `path` is the original string; `path_base64` is absent.
- Invalid UTF-8 path → `"path": null` and `path_base64` holds the **exact original bytes** (RFC 4648 §4 standard alphabet, with padding).
- Validity is decided per whole path (Go `utf8.Valid`); a path mixing valid and invalid segments is invalid as a whole.
- Invalid byte sequences are **never** converted to replacement characters. Storage and the codec format are unchanged.
- The OpenAPI schema expresses exactly-one-of `path` / `path_base64`. Duplicate paths are returned as separate entries in canonical order.
- For the hierarchical file views (§6.4.9) the same rule applies per component with the member names `name` / `name_base64` (exactly one of them carries the bytes: `name` is a string, or `null` together with `name_base64`); the schema documents this.

7.4 No OAuth; no password-based token minting. Every other `/api/v2/*` endpoint accepts API tokens in the `Authorization` header only; the feed endpoint accepts RSS tokens only and rejects API tokens.

7.5 **File-view parity with the web UI [APPROVED W5 addendum; implemented in W7].** `/api/v2` offers the same file-browsing capabilities as the UI through the same service (§6.4.7), as two independent operations under `/api/v2/torrents/{id}`:
- `GET /files/tree` returns file-tree v1 only when the torrent is eligible. Otherwise it answers **`422 Unprocessable Content`** `application/problem+json` with `type` `urn:magnetico:problem:full-tree-unavailable`, a `detail` stating that full-tree delivery is unavailable for this torrent, and `alternate` pointing at `/api/v2/torrents/{id}/files`. `413` is not used. The limit is applied before any response byte is committed, so a `200` is never begun for a tree that then proves too large.
- `GET /files?dir=&offset=&limit=` returns file-dir-page v1 for **any** torrent, including one with 100 files; there is no minimum file count.
- `full_tree_available: boolean` in `GET /torrents/{id}` is the service's stage-1 decision (§6.4.7): `true` means the tree is eligible for one-response delivery; `false` means use `/files`. Clients may always choose `/files`, and are not required to fetch the tree when it is available.
- Hard limits (file count, estimated size, exact serialized size, page byte cap, encode budget) are enforced by the service, not by the adapter, so no request can bypass them. Errors are those of §6.4.13 under the API's problem+json rules (`503` carries `Retry-After`, per §6.2.5).
- Authentication: bearer API token only (§4.8.1, §7.1); the adapters share nothing with the session adapters except the service. API v2 routes MUST NOT be exposed before the bearer authentication exists (W7); W5 deliberately registers none, and a test asserts it.
- Fidelity and tests required of W7: duplicates as distinct entries, exact ordering, hierarchy and positional paths, sizes and directory aggregates, empty components, arbitrary bytes via `name`/`name_base64`; a small torrent usable through either endpoint; a 100-file torrent paginated; a large torrent with `full_tree_available = false`, a `422` from `/files/tree` and a working `/files`; every directory reachable without downloading the tree; malformed positional paths and page values rejected; resource limits not bypassable by direct requests; authentication enforced independently for the UI and the API.

7.6 **OpenAPI planning.** `/api/v2/openapi.yaml` (W7) MUST describe: the `Torrent` schema with `full_tree_available`; `FileTree` (file-tree v1: `FileTreeDir`, `FileTreeFile`), `FileDirPage` (file-dir-page v1: `DirEntry`, `FileEntry`, `Ancestor`, `DirSummary`) with the exactly-one-of `name`/`name_base64` rule; the query parameters `dir` (pattern `^(0|[1-9][0-9]{0,5})(_(0|[1-9][0-9]{0,5}))*$`, at most 256 components), `offset` (integer ≥ 0) and `limit` (1–1,000, default 160); the `Problem` schema and the `full-tree-unavailable`, `index-too-complex`, `invalid-parameter`, `not-found`, `temporarily-unavailable` problem types; and the `next_offset` continuation rule. The route-versus-OpenAPI consistency test (§9.1) covers both new routes. The UI JSON endpoints (§6.4.9) are internal and are **not** part of the public OpenAPI documents.

## 8. Compatibility surface

### 8.1 Retained in v1

| Endpoint | Decision |
|---|---|
| `GET /api/v1/feed/rss/last` | **Preserved** (§8.2) for migrated legacy RSS tokens; documented as **deprecated** |

### 8.2 `GET /api/v1/feed/rss/last`

8.2.1 Content type `application/rss+xml`. Query `page` (integer, 1 to 10; values above 10 clamp to 10). **Item identity and structure are preserved**: `guid` = info hash (hex), `title` = torrent name, `description` = info hash, `link` = magnet URI (configured trackers and `dn`), `comments` = absolute URL of the torrent's details page, `pubDate` = `discovered_on`, `enclosure` = magnet URI with `length` = `total_size` and `type` = `application/x-bittorrent`; the channel element set is kept, including the legacy feed author, emitted as in laminas-feed 2.26.3 both as `<author>Magnetico Web</author>` and as Dublin Core `<dc:creator>Magnetico Web</dc:creator>` (`xmlns:dc`); the legacy item `setDateModified()` is rendered by laminas as `<pubDate>`, which is the item `pubDate` above. Order: newest first, 1,000 items per page. The `comments` URL targets the new details route.

8.2.2 **Authentication [APPROVED]**: an **RSS token** (legacy migrated or new `mgf_`) from the header `api-token`, the cookie `api-token`, or the **query parameter** `api-token`, so existing torrent clients keep working unchanged. The endpoint accepts RSS tokens **only**; a general API token (`mgt_`) presented here is rejected. Failures return the legacy JSON `401` envelope (`{code, status, message, data}`).

8.2.3 RSS URLs contain credentials, so both feed endpoints MUST: redact the token parameters (`api-token`, `feed_token`) and the cookie in all logs; send `Referrer-Policy: no-referrer` and `Cache-Control: private, no-store`; rate limit per token and per client IP on failures; never echo the token; compare token digests in constant time; check the token **before** any cache lookup; and honour independent revocation on the next request. Operators SHOULD configure the reverse proxy to omit or mask query strings in access logs (documented, not enforced).

8.2.4 The feed reads `torrents` only (no blob decoding).

8.2.5 **Page offset — intentional behaviour change [RECOMMENDED; owner: not critical].** The legacy offset is `page × 1000` with `page` defaulting to 1, so the default feed skips the newest 1,000 torrents. The recommendation is to correct it so `page=1` is the newest 1,000. This is **not** an exact-compatibility guarantee. Consequence to document in release notes: on its first poll after cutover a client may see up to 1,000 newest items it never saw before (GUIDs are info hashes, so already-seen items de-duplicate).

8.2.6 A structural golden test (element set, guid, enclosure) guards the mapping.

8.2.7 **Account UI [APPROVED].** Two distinct sections:
- **RSS feeds:** create and revoke RSS tokens; metadata (label, created, last used, expiry); masked legacy entries; a ready-to-copy **v2 RSS URL** shown once when a token is created, which is the recommended URL; an explanation that the **v1 RSS endpoint is deprecated and may eventually be removed**; existing legacy URLs keep working and users are not required to replace them immediately.
- **API access:** create and revoke general API tokens; an explanation of Bearer-header authentication. API tokens are never created automatically at login or by migration; a user who wants API access creates one explicitly after logging in.

8.2.8 **`GET /api/v2/feeds/latest.rss?feed_token=<rss-token>` [APPROVED].** Works in torrent clients that cannot send custom authorization headers. It is an explicitly documented exception in the v2 OpenAPI document and uses only the separate RSS credential; it rejects general API tokens. The parameter name `feed_token` is the recommended one and may change only for a concrete compatibility or design problem (none identified). Same item mapping, ordering, page semantics and protections as v1 (§8.2.1, §8.2.3). The general v2 JSON API stays strictly Bearer-header authenticated; RSS tokens do not work there.

### 8.3 Magnet short link and the tracker list

8.3.1 `GET /magnet/{infohash}` (40 hexadecimal characters, case-insensitive) stays **public** and at this path **[APPROVED]**. It responds `307` with a `magnet:?xt=urn:btih:<hash>` URI carrying the configured trackers (no `dn`, as legacy). It MUST NOT query whether the torrent exists in the index, so it cannot reveal whether a hash is stored. (It does read the tracker settings, §8.3.3.) The details page MUST show the short link and offer to copy it.

8.3.2 **Trackers live in PostgreSQL [APPROVED]**: table `magnetico_web.trackers` (§5.3), ordered by `position` then `id`, supporting future additions, edits, removals and reordering (and later an Admin UI, which is not implemented now). Tracker URLs are validated on write (allowed schemes `udp`, `http`, `https`, `ws`, `wss`; URL-encoded into `tr=` parameters).

8.3.3 **Caching and refresh [PROPOSAL].** The web process keeps an immutable in-memory snapshot of the ordered list, loaded at startup (startup fails if it cannot be loaded, §2.4) and refreshed lazily when older than `TRACKER_CACHE_TTL` (default 60 s) by one background refresh at a time (single-flight). If a refresh fails, the last good snapshot keeps being served and the failure is logged and counted; the snapshot is never replaced by an empty list on error. Changes made through the CLI or a future Admin UI take effect within the TTL (immediately in the same process via an explicit invalidation). No `LISTEN/NOTIFY`, Redis or other infrastructure. Magnet URIs in the UI, RSS and v2 use the same snapshot.

8.3.4 **Initial list and idempotent import [APPROVED].** The initial content is `master:config/public_trackers.json`, **verbatim and in order**, embedded in the binary as seed data (SHA-256 of the file recorded in the seed definition and its test):

```text
udp://tracker.opentrackr.org:1337/announce
udp://tracker.open-internet.nl:6969/announce
udp://tracker.piratepublic.com:1337/announce
udp://9.rarbg.to:2710/announce
udp://tracker.zer0day.to:1337/announce
http://nyaa.tracker.wf:7777/announce
http://t.nyaatracker.com:80/announce
udp://open.stealth.si:80/announce
udp://bt.xxx-tracker.com:2710/announce
udp://exodus.desync.com:6969/announce
udp://open.demonii.si:1337/announce
udp://tracker.torrent.eu.org:451/announce
```

The import runs as a step of the **explicit web-schema initialization** command and **never automatically at ordinary web-service startup**, in **one transaction under an advisory lock**: it inserts the rows with positions `1..n` **only if** the table is empty **and** the settings marker `trackers.seed` is absent, then writes the marker (seed version and hash) in the same transaction. Re-running is a no-op; in particular, an operator who later removes or reorders trackers is never overridden, and a non-empty table without a marker is left untouched and reported. **No tracker is filtered, replaced or "repaired"**: some hosts in this historical list are probably defunct (this is **not verified**), and they are kept so that the owner can prune them deliberately.

8.3.5 Tracker administration before the Admin UI exists uses the `web tracker` CLI of §4.10 **[APPROVED]** (or SQL).

### 8.4 Removed

`POST /api/v1/login`, `POST /api/v1/logout`, `GET /api/v1/torrents`, `GET /api/v1/torrents/{id}` are removed and MUST NOT appear in the v1 OpenAPI document, neither as implemented nor as deprecated. If a consumer is found after cutover they can be reintroduced as an adapter over v2; exact legacy fidelity (`numberOfResults`, `numberOfPages`) cannot be reproduced.

## 9. OpenAPI and documentation

9.1 Each API prefix has an OpenAPI document embedded in the binary as a static asset: `/api/v1/openapi.yaml`, `/api/v2/openapi.yaml` **[APPROVED]**. **Maintenance rule [APPROVED; now in `AGENTS.md`]:** any change to a supported API endpoint, its authentication requirements, request parameters, response schema, error behaviour or other documented contract MUST update the corresponding embedded OpenAPI specification in the same change, for v1 and v2, including the RSS authentication contract. Tests keep implemented routes and OpenAPI descriptions consistent where feasible (every implemented route is described and every described route exists).

9.2 **Rendering [APPROVED approach].** A lightweight client-side renderer, vendored and pinned, loads the relevant embedded specification in the browser and renders interactive documentation. The server provides no dynamic documentation service. **No CDN and no runtime third-party requests.** Documentation never embeds live tokens, credentials or user-specific data.

9.2a **[IMPLEMENTED W7]** Renderer: Swagger UI (`swagger-ui-dist` 5.34.1, Apache-2.0), vendored unmodified, started by the first-party `docs.js` with the validator service and telemetry off, `persistAuthorization: false`, requests that omit cookies and "Try it out" limited to `GET`; the user pastes a token into the Authorize dialog by hand and the pages never contain one. Selection record, hashes and the bundle scan: `third_party/swagger-ui-dist-5.34.1/VENDOR.md`. The interactive pages use the application's CSP plus `img-src 'self' data:` (the stylesheet's icons) and nothing else (§14.4).

9.3 **Version selection.** The choice of a small, maintainable renderer matters more than the newest OpenAPI minor version. The specification MUST use **the latest OpenAPI version supported by the chosen renderer**; heavyweight dependencies MUST NOT be added merely to support OpenAPI 3.2.1. The renderer evaluation (candidates, size, licence, maintenance, CSP compatibility without inline scripts or `eval`, supported OpenAPI versions, no external requests) is a separate prerequisite (§13.3) and is recorded when done. This draft claims nothing about specific renderers.

9.4 `GET /api` is a public landing page linking to `/api/v1` and `/api/v2`, each of which serves the interactive documentation and a link to download its specification. All of them are public **[APPROVED]**, are covered by §3.6 (`noindex`), and may use a CSP relaxation limited to these routes only if the renderer requires one. A "try it out" feature MUST NOT persist or log credentials; whether to enable it is part of the renderer evaluation.

9.5 The v1 document describes **only** endpoints the new application implements: the RSS endpoint, its query/header/cookie token transport, the `401` envelope, and the deprecation of the prefix.

## 10. Logging, errors, privacy

10.1 The web application uses **`log/slog`** **[APPROVED]** (text or JSON, level configurable). Fields: request ID, method, route pattern, status, duration, user ID (not name) when authenticated.

10.2 MUST NOT be logged: passwords, session tokens, API tokens, RSS tokens, reset/setup/verification/invite codes, full query strings, torrent names or file paths (by default; `postgresql-persistence-v2.md` §8), SMTP credentials. Path segments carrying codes and the `api-token`/`token` parameters are redacted.

10.3 User-visible errors are generic with a request ID. Cases that would reveal account, email or invitation existence (§4.3.6, §4.7.4, §4.9.9) return identical content.

10.4 Typed storage errors map as: `ErrTorrentNotFound` → 404; `ErrIntegrityMissingBlob` and `ErrCorruptBlob` → 500 with an `ERROR` log carrying `torrent_id`; I/O errors → 503 or 500; none is reported as an empty file list. A search timeout is §6.2.5.

## 11. UI requirements

11.1 Server-rendered HTML with auto-escaping; search, details, sign-in and sign-out are usable without JavaScript. JavaScript is first-party only (`app.js` plus a tiny blocking `theme.js`; no framework, no CDN, no inline scripts) and provides enhancements (theme switch, clipboard copy, `/` to focus search, selector auto-submit) **and the file browser**, which requires it (§6.4.12, an approved exception: no duplicate HTML tree renderer is kept).

11.2 Light and dark themes (system preference with an explicit override), usable at 360 px width, keyboard operable, visible focus, labelled inputs, sort state exposed to assistive technology, contrast of at least 4.5:1 for text and 3:1 for control boundaries in both themes (`docs/wip/07F-w5-measurements/contrast.txt`), respect for reduced motion, a skip link.

11.3 No third-party requests from the browser (fonts, icons, scripts, analytics).

11.4 The account page (§8.2.7) has an **RSS feeds** section and a separate **API access** section; it explains that **v1 RSS is deprecated and may eventually be removed**, lists tokens of each type separately (legacy RSS entries masked), shows the user's unused invitation links with copy buttons, the email address with its verification state, and the password-change form with separate explicit "also revoke all API tokens" and "also revoke all RSS tokens" options.

## 12. Acceptance and tests (by milestone)

12.1 Milestones W1–W9 and their acceptance criteria are in the plan (§8); this specification is satisfied when those pass and the owner has resolved the open items.

12.2 **Password compatibility tests.** For each supported legacy hash format: PHP-generated vectors (valid password, wrong password, edge lengths) produced by a script with the PHP version recorded alongside, **independently verified** (generated outside the Go code under test); mixed-format databases; transparent rehash on login; unsupported format handled as "needs reset"; bounded concurrency under load.

12.3 **Account-state and revocation tests.** `password_hash IS NULL` cannot log in; completing a setup/reset link never changes `active` (disabled account: refused, link not consumed, works after `enable` unless expired); disable → enable does not touch `password_hash` and never restores sessions (the user must log in again) — token behaviour is §12.4b; expired setup links are re-issuable; email verification is independent of setup; `set-role` changes the role and invitation registration always yields `user`; disable refuses registration through the owner's invitations and enable restores them; concurrent disable versus registration (both orders, repeated, race detector) never commits a registration after the disable commit and never consumes the invitation on refusal; concurrent revoke versus authenticated request; migrated legacy invitations behave identically.

12.4 **Credential-revocation tests.** Email reset, admin reset and set-password revoke all sessions, API tokens **and RSS tokens** (API and RSS expectations asserted separately); self-service password change keeps both token types by default and revokes each with its own option; the reset email and page contain the notice for each revoked credential type.

12.4b **Disable/enable credential tests (revision 4).** (1) active → disabled → enabled with the same API token: works, fails while disabled, works again, and the stored row (`token_hash`, `revoked_at`, `expires_at`) is byte-identical throughout; (2) the same with a new `mgf_` RSS token on `/api/v2/feeds/latest.rss`; (3) the same with a **migrated legacy RSS token** on the unchanged v1 URL; (4) an explicitly revoked token stays revoked after enable; (5) an expired token stays expired after enable; (6) sessions stay invalid after disable/enable; (7) concurrent disable versus token-authenticated requests (both orders, repeated, race detector): no request authorized after the disable commit succeeds, none fails after enable; (8) `revoke-tokens` permanently revokes all tokens of both types and enable does not restore them; (9) a password reset revokes sessions, API tokens and RSS tokens (legacy included) and the email/page carry the §4.9.6 notices; (10) `disable` does not use or advance any epoch counter.

12.4a **Credential-separation and migration tests.** Positive: a migrated legacy RSS token authenticates `/api/v1/feed/rss/last` via query, header and cookie, unchanged; a new `mgf_` token authenticates `/api/v2/feeds/latest.rss?feed_token=` and v1 RSS. Negative: the same legacy token and any RSS token are rejected by `/api/v2/torrents` and every other JSON endpoint; an API token is rejected by both feed endpoints; the migration creates zero `api_tokens` rows, all `rss_tokens` rows carry `legacy = true` with preserved `created_at` and owner, only `sha256` is stored, and no plaintext appears in the target or in logs; the account UI never shows legacy values; independent revocation takes effect on the next request; token redaction in logs; `Referrer-Policy` and cache headers on both feeds.

12.5 **Session tests.** Idle expiry; remember-me absolute expiry; the ordinary-session absolute cap; every session row has a non-null `expires_at`; configuration validation rejects each invalid combination of §4.4.4; durations without a `d` unit.

12.6 **Email tests** with a fake SMTP server: STARTTLS enforced; server without STARTTLS → no `AUTH`, no message; invalid/untrusted certificate → failure; implicit TLS mode; no plaintext to loopback either; header-injection attempts rejected; unverified address never receives a reset; changing an address leaves the old verified address in force until confirmation; forgot-password responses and timing shape identical for known, unknown, unverified and disabled.

12.7 **Search tests.** Three-code-point minimum with multi-byte text; AND semantics; literal `%` and `_`; 40-hex lookup; empty query; deterministic ordering and keyset pagination without `COUNT`/`OFFSET` (assert on the generated SQL); the default timeout is 60 s and is configurable and validated; a deliberately slow query hits the timeout with a friendly response, **not** claiming the query is invalid; the HTTP request/write deadlines do not fire before the search timeout (run with a short general timeout and a longer search timeout); search slot saturation does not block login.

12.8 **Tracker and magnet tests.** The seed inserts the 12 historical URLs in order, is idempotent, and never resurrects removed trackers or overrides reordering; a non-empty unmarked table is untouched; the redirect works without any `torrents` lookup (assert no query is issued) and survives a tracker-refresh failure using the last good snapshot; changes are visible within the TTL.

12.9 **Policy and compatibility tests.** `/robots.txt` body is exactly the default; `X-Robots-Tag: noindex, nofollow` on all response classes; documentation and OpenAPI routes are public and contain no credentials; no external requests from documentation pages; old UI routes return 404 and old state-changing GETs change nothing; v1 RSS accepts RSS tokens by query, header and cookie, keeps item identity, and `page=1` returns the newest 1,000 (documented change); v1 OpenAPI lists only implemented endpoints; v2 rejects query-string API tokens everywhere except the RSS-token feed endpoint; the OpenAPI documents describe the RSS authentication contract; a route-versus-OpenAPI consistency test passes (§9.1).

12.10 **API path tests.** Valid UTF-8, invalid UTF-8 (`path: null`, `path_base64` round-trips to the exact bytes), mixed segments, duplicate paths, mixed encodings within one page and across continuation (`next_offset`) boundaries; the file browser shows lossless escapes and no U+FFFD (shared Go/JS name vectors); JSON validity and byte round trips for tree and page documents (NUL, controls, quotes, backslash, U+2028/2029, U+FFFD, invalid UTF-8, random bytes, fuzzing against `encoding/json`).

12.11 **Security tests.** Spoofed forwarded headers from an untrusted peer do not change rate-limit identity; CSRF enforced on every POST including unauthenticated ones; equal responses for known/unknown usernames, emails and invitations; log capture shows no secrets or invitation codes; headers present; dev-mode cookie rules rejected for non-loopback URLs; invitations of other users are never visible.

12.12 **File-tree benchmarks (W4)** per §6.4.4, on synthetic disposable data; **file-view measurements (W5):** serialized size and time per corpus shape, estimate calibration, index memory at the complexity limits, 8/32 concurrent full-tree clients, tree serialization concurrent with cold index builds, directory navigation under cache eviction churn (`docs/wip/07F-w5-measurements/`).

12.13 **Operational constraints for all testing:** disposable databases only (`internal/pgtest`); synthetic data; no production access; **no DHT or other public BitTorrent traffic from the development VM, under any circumstances.**

## 13. Decisions summary

### 13.1 Owner-approved

Revision 2 (review of `17b4c2f0`): schema `magnetico_web`; `pending_setup` removed in favour of `active` / nullable `password_hash` / independent email verification; roles `user` and `admin` with `set-role`; plaintext invitation codes with the suspension and transaction rules; configurable session lifetimes with finite absolute expiry; optional email with verification, verified-address-only recovery, strict-TLS SMTP, API-token revocation on reset with explicit email notice, optional revoke-all on password change; search architecture and the 60 s configurable timeout; no new indexes without measured, approved gain; trackers in PostgreSQL; a public `/magnet` that reads cached tracker settings but never checks that the torrent exists; no legacy UI redirects; `robots.txt` and `X-Robots-Tag`; public API documentation with embedded OpenAPI and a vendored client-side renderer; `log/slog`; the non-UTF-8 path contract.

Revision 4 (account lifecycle correction): **`disable` suspends and does not revoke** API tokens, RSS tokens (migrated legacy ones included) and unused invitations; `enable` restores them unless expired or explicitly revoked, and never restores sessions; no epoch counter; every API/RSS request checks existence, expiry, `revoked_at` and owner `active`; a separate admin `revoke-tokens <username>` permanently revokes all tokens; **password reset (email or admin) permanently revokes sessions, API tokens and RSS tokens** with explicit notices on the email and confirmation page; ordinary password change retains both token types by default. These replace the earlier revoke-on-disable rule and the "proposed" RSS-reset policy.

Revision 3 (review of `15b76d20`): **separate `rss_tokens` and `api_tokens`**, legacy tokens migrated only into `rss_tokens` as `sha256`, a new `/api/v2/feeds/latest.rss?feed_token=` endpoint with RSS-only credentials, `mgf_` format, account UI sections; **case-sensitive usernames**; **one web instance** initially; session lifetimes 12 h / 168 h / 2160 h; **current password required for email change**; **tracker CLI**; **tracker seed during explicit schema initialization only** (never at service start, never overwriting later changes); **preserved legacy user and invitation IDs**; file-tree budgets as benchmark targets, not hard gates; the `AGENTS.md` OpenAPI rule.

### 13.2 Still open

1. ~~File-tree final solution~~ **Resolved in W5**: JSON file browser on the details page, tree mode and directory mode chosen by the backend, positional directory paths (§6.4.7–6.4.13). The W4 memory budgets remain benchmark targets (§6.4.4).
2. Breached-password lookup (not proposed).

Minor proposals still unconfirmed, not blockers: the notice to the previous address after an email change (§4.9.8), the placement of the SMTP implementation (standard library preferred, §4.9.10), and the default reset/setup/verification link lifetimes (§4.9.4).

### 13.3 Remaining implementation prerequisites (separate from this design)

Legacy password-hash distribution audit (**does not block W1**); selection of a maintained `golang.org/x/crypto` version; ~~selection of the API-documentation renderer (and the OpenAPI version it supports)~~ **resolved in 07G: Swagger UI 5.34.1, OpenAPI 3.2.0**; the user-migration implementation, including the handling of historical email addresses; CI/OCI images and release-candidate testing.

## 14. Implementation record and deviations (revision 7, task 07G: W6 + W7)

This section records what 07G implemented and every place where the implementation deliberately differs from, or makes concrete, the text above. Test evidence is in `docs/wip/07G-web-account-api-report.md`.

### 14.1 Decisions of the owner during 07G

1. Users cannot create invitations; they list, copy and revoke the pre-minted ones (§14.2).
2. API documentation renderer: Swagger UI, vendored and pinned; "Try it out" is enabled but limited to `GET`; telemetry and the external validator are off; credentials are typed by the user and never persisted.
3. A JSON Schema 2020-12 validator is allowed as a **test-only** dependency (`github.com/santhosh-tekuri/jsonschema/v6` v6.0.3, with its dependency `github.com/dlclark/regexp2`); the official OpenAPI 3.2 schema documents are vendored under `internal/web/apidocs/testdata/oas/` and loaded offline.
4. No lifetime cap on revoked token rows in 0.8.0 (§14.5).

### 14.2 Invitations
`INVITE_QUOTA` keeps its meaning: the number of invitations a user receives when the account is created or registered. The account page lists the user's invitations (unused ones with the absolute link and a copy button, used ones with the registrant's name), revokes unused ones (owner-scoped; another user's id is a `404`) and offers no creation. New invitations come from `web user invite add` or a future administration UI.

### 14.3 Account, email and SMTP
- **Account page** `GET /account?tab=…` (server-side tabs, no JavaScript needed); every other action is a `POST` behind the session CSRF guard; success redirects to `?done=<slug>` (fixed texts), failures re-render the page with a status; secrets appear only in the response that creates them.
- **Password change** verifies the current password through the shared hashing limiter (and is limited per account), then commits conditionally on the verified hash: other sessions are deleted, the current session is replaced by a rotated one **with the same absolute expiry and `remember` flag** (a rotation never extends a lifetime), setup/reset links are deleted, and each of "revoke all API tokens" / "revoke all RSS tokens" applies only when ticked.
- **Setup, reset, registration and email links** use the existing atomic store operations. After completing a link or registering the user is sent to `/login` with a fixed notice; no session is created. The confirmation page and the reset email name API tokens and RSS tokens/URLs separately (§4.9.6).
- **Email verification** consumes its link in one transaction. If another account already holds the address verified, the unique index aborts the transaction, so the link is **not** consumed and the previous address is unchanged; the visitor sees the same message as for any invalid link. A disabled owner's link is not consumed either.
- **Mail queue** (non-durable, in process): capacity 32 (queued plus running), two workers, 30 s per message, a worker drops a job older than 15 minutes without sending it (shortest link lifetime: 1 h). Handlers `Reserve` a slot before they write anything: an authenticated verification or change request that finds the queue full answers `503` and issues no link; registration stays committed and tells the user to request the email later; forgot-password answers as always. At shutdown the queue stops accepting, drains within the hook's bound and counts what it abandoned.
- **Sender**: implicit TLS or STARTTLS only; system roots (`SSL_CERT_FILE` on Linux), `ServerName` = host, TLS ≥ 1.2, no verification bypass and no use of the standard library's one-call sender (a source test forbids both); `AUTH` (PLAIN, or LOGIN when only that is offered) only after a completed handshake; recipients are bare addresses and every header value is validated before any connection is made; errors carry a category only.
- **Without SMTP** every email-dependent control is shown as unavailable; `/password/forgot` stays registered and says so; sign-in, tokens, password change and administrator links are unaffected.
- **Email change notice** (§4.9.8 PROPOSAL): implemented, sent best-effort to the replaced verified address.

### 14.4 API v2, feeds, magnet, documentation
- **`/api/v2`** (access class `APIToken`, `GET`/`HEAD` only): the five endpoints of §7.2. `limit` must be an integer 1–100; a repeated parameter, a malformed `limit` or `info_hash`, or `info_hash` combined with `q`, `sort`, `order` or `cursor` is `400 invalid-parameter`; unknown `sort`/`order` values keep the §6.3.1 fallback. Responses use `Cache-Control: private, no-store`. A new problem slug `rate-limited` (`429`, `Retry-After`) is added to the table of §6.4.13. Unknown paths under `/api/v2/` answer `404` problem+json after authentication. The file-view endpoints are the **same adapter** as the UI's (`torrents.FileAPI`): parity is structural and a test compares responses byte for byte.
- **Bearer guard**: `Authorization: Bearer mgt_…` only (one header, one space); a failure limiter per client address (burst 20, one every 10 s) is applied before the lookup and refunded on success, a use limiter per token (burst 120, 2 per second); one lookup per request checks existence, expiry, revocation and owner-active; `last_used_at` is written at most every 5 minutes.
- **RSS feeds** (access class `Feed`, which carries no credential logic; each route binds its own fixed policy): v1 reads `api-token` from the header, then the cookie, then the query, and answers failures with the legacy envelope `{"code":401,"status":"error","message":"Invalid credentials.","data":null}` (for a missing and an invalid token alike, a simplification of the old application, whose body for a missing token differed); v2 reads `feed_token` from the query only and answers `application/problem+json`. Both use `rss_tokens` only (new `mgf_` and migrated legacy tokens) and never the Authorization header or an API token; the credential is checked on every request before the page cache; every response carries `Referrer-Policy: no-referrer` and `Cache-Control: private, no-store`; the document never contains the token (the self link has no query). Pages: `page` 1–10, invalid or below 1 → 1, above 10 → 10; `page=1` is the newest 1,000 (§8.2.5). Order: `id DESC`, the legacy order, through the bounded `persistence.Reader.LatestTorrents` (offset ≤ 9,000, limit ≤ 1,000). The rendered page is cached for 60 s with single-flight builds (two concurrent builds at most, bounded wait, then `503`). XML 1.0 cannot carry control characters; a name containing one is written with U+FFFD, the only normalisation.
- **Magnet links** are built as `xt`, optional `dn`, then one `tr` per tracker in stored order, form-encoded (`url.QueryEscape`: a space is `+`; PHP's `urlencode` additionally writes `~` as `%7E`, which is the only byte difference). The tracker snapshot is loaded in a startup hook (startup fails if the list cannot be read), refreshed lazily by a single background task, kept on failure, and read without blocking. `GET /magnet/{infohash}` does no database work at all. Search results, the details page, the API and the feeds use the same snapshot; the details page offers "Copy short link".
- **Documentation**: `/api`, `/api/v1`, `/api/v2` and the two YAML documents are public and static. Both documents are OpenAPI **3.2.0** (the highest version the vendored renderer supports) and are validated in tests against the official 3.2 schema; a cross-package test keeps router and documents equal in both directions; real responses are validated against the component schemas. The interactive pages relax the CSP by `img-src 'self' data:` only.

### 14.5 Credential storage growth
The 50-unrevoked-tokens cap bounds usable credentials, and creation is rate limited per account and type, but revoked rows are retained and there is no cleanup: no database role has `DELETE` on the token tables and adding it is a grant change that needs approval. Revoked-row retention is an operational concern (`docs/web-deployment.md`); a future operator command may prune old revoked rows.

### 14.6 Not changed
The `magnetico_web` schema, its structural verification, the roles and grants, the wire format and codec, the persistence schema, the administrator CLI, and the revocation semantics of §4.7 are unchanged. No W8 or W9 work was done.

### 14.7 Known limitations
- No page was rendered in a real browser (none exists on the development VM). The Swagger UI pages' behaviour under the CSP (no `unsafe-eval`, no `unsafe-inline`), "Try it out", light/dark presentation and 360 px layout of the new pages are owner items.
- Rate limits and the mail queue are per process (one instance, §4.6.2).
- The v1 feed's exact legacy XML bytes could not be reproduced or compared (the legacy generator library is not available); the structure follows §8.2.1 and is guarded by a structural golden test.

