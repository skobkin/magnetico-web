# Magnetico 0.8.0 — Web Service Specification (web-v1)

> **DRAFT — FOR OWNER REVIEW. NOT APPROVED. NOT IMPLEMENTED.**
> Nothing in this document is a contract until the owner approves it. Items marked **[OPEN]** are undecided owner questions; items marked **[PROPOSAL]** are recommendations, not decisions. Rationale and the legacy inventory are in [`docs/wip/07A-web-design-plan.md`](../wip/07A-web-design-plan.md).

Builds on [`postgresql-persistence-v2.md`](postgresql-persistence-v2.md) and [`wire-format-v1.md`](wire-format-v1.md). If this document and either of those disagree, they win and this document must be corrected. This document does not change the database schema, the wire format, or crawler/ingest behaviour.

Key words MUST, SHOULD, MAY are used in the RFC 2119 sense.

## 1. Scope and non-goals

1.1 `magnetico web` is a command of the single `magnetico` binary and an independently deployable service. It reads the 0.8.0 PostgreSQL storage and keeps its own account state in PostgreSQL.

1.2 The web service MUST NOT depend on the crawler, the crawler's SQLite queue, the ingest service, or the legacy PHP runtime. The web command's dependency graph MUST NOT contain `internal/crawler`, `internal/crawlqueue`, `internal/ingest` or `modernc.org/sqlite` (enforced by a test).

1.3 The web service never writes to `magneticod.*` tables.

1.4 Non-goals: torrent upload or editing, public browsing, search by file path (removed in 0.8.0, `postgresql-persistence-v2.md` §1), administration UI, OAuth, multi-tenant features, user migration (a separate task), CI/OCI packaging.

## 2. Process model

2.1 One HTTP listener, plain HTTP, TLS terminated by a reverse proxy (Traefik or equivalent). The default listen address MUST be loopback (`127.0.0.1:<port>`).

2.2 Configuration: command-line flags, then `MAGNETICO_WEB_*` environment variables, then defaults, in the manner of `opflags/ingest.go`. Unknown `MAGNETICO_WEB_*` variables MUST be rejected. Secrets (database URL containing a password, SMTP password) MUST be environment-only and MUST NOT be printed or logged. **[PROPOSAL]** no configuration file in the first version.

2.3 Required settings include: database URL; `--public-url` (the externally visible base URL); `--trusted-proxies` (CIDR list, default empty); listen address; optional SMTP settings; the default invite quota for new users (default `0`).

2.4 Startup MUST fail fast, with an actionable message and without modifying the database, if: PostgreSQL is unreachable; `pg_trgm` is missing; the torrent schema or its `schema_version` is not the supported one; the users schema is missing or has an unsupported version; the database role lacks the needed privileges. It MUST NOT run any automatic migration (`postgresql-persistence-v2.md` §2, §6).

2.5 Graceful shutdown on `SIGINT`/`SIGTERM`: stop accepting connections, let in-flight requests finish within a bounded grace period, then close pools. A stuck handler MUST NOT prevent exit beyond the bound; the exit status distinguishes an incomplete shutdown (as `ingest` does).

2.6 Health endpoints: `GET /healthz` (process alive) and `GET /readyz` (database reachable and schema verified). Both are unauthenticated and MUST NOT disclose versions, counts or configuration.

2.7 Database role **[PROPOSAL]**: `SELECT` on `magneticod.*`; `SELECT/INSERT/UPDATE/DELETE` on the users schema; no DDL.

2.8 Pool: explicit maximum connections **[PROPOSAL: 10]**; every request has a context deadline; search has a statement timeout.

## 3. Request pipeline

3.1 Order: request ID → panic recovery → access log (redacting) → security headers → client-IP resolution → cross-origin protection → authentication → rate limiting → handler.

3.2 **Client IP and forwarded headers.** `Forwarded` and `X-Forwarded-*` headers MUST be honoured **only if the direct TCP peer address is within `--trusted-proxies`**. Otherwise they are ignored and the peer address is the client address. When the peer is trusted, the client address is the right-most address in the `X-Forwarded-For` chain that is not itself trusted. A direct client MUST NOT be able to change the address used for rate limiting.

3.3 **Absolute URLs** (RSS, setup/reset/invite links, magnet short links shown to users) MUST be built from `--public-url`, never from the `Host` header.

3.4 **Response headers** for HTML: a CSP with `default-src 'self'`, no inline scripts, `frame-ancestors 'none'`; `X-Content-Type-Options: nosniff`; `Referrer-Policy` (`same-origin` by default, `no-referrer` where specified); `Cache-Control: private, no-store` for authenticated pages; `Permissions-Policy` denying unused features. The documentation pages (§9.4) MAY use a narrower relaxation limited to those routes.

3.5 **Panics** produce a generic 500 with a request ID; details only in the log.

## 4. Authentication and authorization

### 4.1 Policy

| Route class | Access |
|---|---|
| `/login`, `/register/{code}`, `/password/forgot`, `/password/reset/{token}`, `/password/setup/{token}` | public |
| `/magnet/{infohash}` | public (§8.3) |
| `/healthz`, `/readyz`, `/robots.txt`, static assets | public |
| `/api/v1/feed/rss/last` | API token (§6.3); public documentation pages **[OPEN]** (§9.4) |
| every other route | authenticated session (UI) or API token (`/api/v2/*`) |

Unauthenticated requests to UI pages redirect to `/login`; to API routes they receive `401`.

### 4.2 Accounts

4.2.1 States: `pending_setup` (no password set), `active`, `disabled`. A `pending_setup` or `disabled` account MUST NOT authenticate by any means.

4.2.2 Authentication MUST check the account's `active` state on **every** request that resolves a session or token, in the same lookup that resolves the credential. Removing session rows is not sufficient on its own.

4.2.3 Email is optional. **[OPEN]** username case sensitivity and collision policy; whether email addresses are verified; whether `roles` are kept (there is no administrative UI in this version).

### 4.3 Passwords

4.3.1 New hashes: **argon2id**, PHC string format. **[PROPOSAL]** parameters per RFC 9106 §4 second recommended option (memory 64 MiB, 3 passes, parallelism 4), confirmed by a benchmark on the target host before release.

4.3.2 New passwords: 8 to 1,024 bytes. Login accepts up to 4,096 bytes (the legacy limit). Longer input is rejected before hashing.

4.3.3 Legacy hashes are verified directly (they cannot be converted offline). A legacy format is **supported only if** it has independently verified test vectors generated by PHP (§12.2). Recognising a format prefix is not support. A stored hash in an unsupported format makes the account "needs reset": it cannot log in and the administrator is told via `web user list` and on reset.

4.3.4 On successful login with a legacy or outdated hash, the hash MUST be replaced with a current-parameter argon2id hash in the same transaction that records the login.

4.3.5 **Bounded concurrency.** All hash and verify operations pass through one process-wide limiter. **[PROPOSAL]** default `max(1, min(GOMAXPROCS, 2))` concurrent operations, a bounded queue wait (a few seconds); when the wait expires the request fails with a generic 503 and `Retry-After`, and a counter is incremented. The limit bounds memory (argon2 memory × concurrency).

4.3.6 Login for an unknown username MUST perform an equivalent dummy verification (through the same limiter), and return the same generic error as a wrong password or an unavailable account.

4.3.7 Dependencies: argon2/bcrypt come from `golang.org/x/crypto`. The repository currently carries an old indirect pin; **a maintained version MUST be selected in a dependency review and approved by the owner before use.** **[OPEN]**

### 4.4 Sessions

4.4.1 The session token is 32 random bytes from `crypto/rand`; only its SHA-256 is stored server-side (PostgreSQL). A new session is created at login; the session is rotated on password change.

4.4.2 Production cookie: name `__Host-magnetico_session`, `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`. This MUST NOT be weakened for local testing.

4.4.3 **Development mode** exists for loopback only: it is accepted only when `--public-url` is an `http://` URL whose host is a loopback address, it uses a different cookie name without the `__Host-` prefix and without `Secure`, and the service logs a warning at startup. It MUST be rejected for any other public URL.

4.4.4 Lifetimes **[OPEN]**. **[PROPOSAL]** idle timeout 12 h; "keep me signed in" 90 days absolute, refreshed no more often than daily; `last_seen` updates throttled.

4.4.5 Logout is a `POST`, deletes the server-side session, and clears the cookie.

### 4.5 CSRF

4.5.1 Every state-changing browser action MUST be a `POST` (or other non-safe method) and MUST be protected, including actions that need no existing session: login, registration, password reset request, password reset, password setup, and every account action. No state change on `GET`.

4.5.2 Two layers: the standard library's cross-origin protection (Fetch-metadata/`Origin` based) and a synchronizer token in every form. For forms shown before a session exists, the token is bound to a short-lived pre-session cookie.

4.5.3 API endpoints authenticated only by a bearer token are not subject to cookie CSRF, and MUST NOT accept the session cookie as authentication.

### 4.6 Rate limiting

4.6.1 Throttling (not account lockout) with bounded in-memory state: login per client IP and per normalised username; reset requests per IP and per email; registration per IP; failed token authentications per IP; v1 RSS and v2 per token.

4.6.2 Responses: `429` with `Retry-After`; error text generic. State is per process, which assumes a **single** web instance. **[OPEN]** multi-instance deployments.

### 4.7 Revocation and account state

| Event | Sessions | API tokens | Invitations owned by the user |
|---|---|---|---|
| Disable | All revoked immediately; login refused | Rejected immediately (active check); recorded as revoked | **Suspended**: rows and codes unchanged; registration through them is refused while the owner is inactive |
| Enable | None restored; user must log in | **Not** restored; new tokens required | Usable again automatically, unless used, explicitly revoked, or expired |
| Self-service password change | Other sessions revoked; current rotated | Kept by default with a "revoke all tokens" option **[OPEN]** | Unchanged |
| Admin `reset-password` / `set-password`; completed email reset | All revoked | Revoked **[OPEN]** | Unchanged |
| Explicit invitation revocation | n/a | n/a | Permanent (`revoked_at`); `enable` never undoes it |

4.7.1 "Revoked" is stored state. Enabling an account MUST NOT make previously revoked sessions or tokens valid.

4.7.2 **Invitation suspension.** Disabling a user MUST NOT modify, delete or mark as revoked any invitation they own. The same applies to migrated legacy invitations.

4.7.3 **Registration is one transaction** that (a) locks the invitation row (`FOR UPDATE`) and its owner's user row (`FOR SHARE`); (b) verifies that the invitation exists, is unused, is not revoked, is not expired, and that its owner is `active`; (c) creates the account; (d) marks the invitation used. `disable` MUST take `FOR UPDATE` on the same user row. Consequently a registration MUST NOT succeed after a disable transaction has committed, and a rejected registration MUST NOT consume or mark the invitation.

4.7.4 The registration page MUST show one generic message for every non-usable invitation (unknown, used, revoked, expired, owner disabled): *"This invitation is invalid or no longer available."* It MUST NOT reveal that the owner is disabled.

4.7.5 A request authorised before a revocation commits may complete; the next request MUST observe the revocation. No authentication result is cached in the proposal; if a cache is later introduced its TTL is the documented revocation latency.

### 4.8 API tokens

4.8.1 New token format **[PROPOSAL]**: `mgt_` + base64url of 32 random bytes. Only the SHA-256 is stored, with label, creation time, throttled last-use time, optional expiry, and `revoked_at`. The plaintext is shown **once**, at creation.

4.8.2 **Legacy tokens** (32 hex characters) are stored as SHA-256 of that string, so tokens presented in existing URLs continue to verify. Their plaintext cannot be recovered: the account page MUST list them as masked entries (label such as "legacy token", creation date, revoke action) and MUST NOT imply the value can be displayed.

4.8.3 Presentation: `/api/v2/*` accepts `Authorization: Bearer` only. `/api/v1/feed/rss/last` additionally accepts the legacy `api-token` header, cookie and query parameter (§6.3).

4.8.4 Token creation is a `POST` with CSRF; there is no token-minting-by-password endpoint.

### 4.9 Password reset and setup

4.9.1 Self-service email reset is available **only when SMTP is configured**. `POST /password/forgot` MUST return the same response and similar timing whether or not the address exists: *"If the address you entered is registered, a password reset link has been sent."* Requests are rate limited (§4.6).

4.9.2 Reset and setup tokens: 32 random bytes, stored as SHA-256, single use, expiring (**[PROPOSAL]** reset 1 h, setup 72 h), bound to one account; issuing a new token for an account invalidates earlier ones. Legacy reset tokens are not migrated.

4.9.3 Completing a reset follows §4.7. Users without an email address cannot self-serve and need an administrator.

4.9.4 Reset/setup/invite codes MUST NOT be logged, and the pages carrying them use `Referrer-Policy: no-referrer`.

### 4.10 Administrator CLI

`magnetico web user …` operates directly on the users store (database URL from the environment):

| Command | Behaviour |
|---|---|
| `add <username> [--email E] [--invites N]` | Creates a `pending_setup` account with **no usable password**, prints a single-use expiring setup link. `--invites` overrides the configured default quota (default 0). |
| `reset-password <username>` | Issues a reset/setup link bound to that account and prints it; the administrator learns no password. Revokes sessions per §4.7 when completed. |
| `set-password <username>` | Rare case. Reads the password from a prompt or stdin; never from argv; never echoed or logged. |
| `disable <username>` / `enable <username>` | Per §4.7. |
| `invite add\|revoke …` | Create or permanently revoke invitations. |
| `list` | Usernames, state, whether a password hash is in a supported format. No secrets. |

Commands MUST NOT print secrets other than the single link or token they were asked to issue.

## 5. Users schema (proposal; not applied)

5.1 **[OPEN]** Name and DDL. **[PROPOSAL]** a schema `magnetico_users` created by an explicit, non-destructive, idempotent command (like `migrate schema-init`), never implicitly by `web`, with its own version table.

5.2 **[PROPOSAL]** logical tables (types and indexes to be fixed in the DDL review):

- `users`: id (identity; legacy IDs preserved by the migrator), username, email (nullable, unique when present), password_hash (nullable), state, created_at, password_changed_at, last_login_at, a credential epoch.
- `sessions`: token_hash, user_id, created_at, last_seen_at, expires_at, remember flag.
- `api_tokens`: token_hash, user_id, label, created_at, last_used_at, expires_at, revoked_at, legacy flag.
- `invites`: id, owner user_id, code or code_hash **[OPEN]**, used_by, created_at, expires_at (nullable), revoked_at.
- `one_time_tokens`: token_hash, user_id, purpose (`reset` | `setup`), expires_at, used_at.

5.3 `roles` **[OPEN]**: dropped in the proposal.

5.4 Concurrency invariants (§4.7.3) require row-level locking semantics; schema and constraints MUST make an invite single-use (`used_by` set at most once) at the database level.

## 6. UI routes

### 6.1 Route table

| Method and path | Auth | Purpose |
|---|---|---|
| `GET /` | public | Redirect to `/torrents` (authenticated) or `/login` |
| `GET, POST /login`; `POST /logout` | public / session | Sign in / out |
| `GET /torrents` | session | Search and listing |
| `GET /torrents/{id}` | session | Details |
| `GET /torrents/{id}/files` | session | File browser view |
| `GET, POST /register/{code}` | public | Invite registration |
| `GET, POST /password/forgot`, `/password/reset/{token}`, `/password/setup/{token}` | public | Reset and setup |
| `GET /account`; `POST /account/password`, `/account/email`, `/account/tokens`, `/account/tokens/{id}/revoke`, `/account/invites` | session | Account management |
| `GET /magnet/{infohash}` | public | Short magnet redirect (§8.3) |
| `GET /api`, `/api/v1`, `/api/v2` and their OpenAPI documents | see §9.4 | API documentation |
| `GET /healthz`, `/readyz`, `/robots.txt` | public | Operations |

The UI paths are a proposal and may be adjusted; the legacy UI URLs (`/torrents`, `/torrents/{id}`, `/register/{code}`, `/auth/*`, `/account`, `/profile/api/*`) have no compatibility promise. **[OPEN]** whether to add redirects from `/auth/login` and `/auth/reset/*`.

### 6.2 Search (draft decisions, **[OPEN]**)

6.2.1 Query text is split on whitespace. A word shorter than 3 **Unicode code points** is ignored (legacy `mb_strlen` semantics; the Go implementation counts runes, not bytes). All remaining words must match (AND), case-insensitively, as substrings of `torrents.name`.

6.2.2 **Difference from legacy:** the legacy code did not escape `%` and `_`, so users' typed `%`/`_` acted as SQL wildcards. This proposal treats the input as a **literal substring** and escapes LIKE metacharacters (the existing helper `escapeLikePattern`). This is a deliberate behaviour change.

6.2.3 A query that is exactly 40 hexadecimal characters is an exact info-hash lookup (addition). **[OPEN]**

6.2.4 An empty query (or one with no usable word) lists the newest torrents.

6.2.5 Search operates on `torrents` only and MUST NOT decode blobs (`postgresql-persistence-v2.md` §3, §5).

6.2.6 A statement timeout bounds search; on timeout the UI shows "query too broad" (not a 500). Timeout value **[OPEN]**.

### 6.3 Ordering and pagination

6.3.1 Sorts: `discovered` (default, descending), `size`, `name` (only with a query). Every sort has `id` as a tie-breaker so cursors are stable. Unknown values fall back to the default (never an error page).

6.3.2 **Keyset pagination**, opaque cursors, no `COUNT(*)`, no `OFFSET`; "newer/older" navigation; page sizes 20, 50, 100. The UI MUST NOT display a total result count.

6.3.3 No change to indexes is proposed. Whether `(discovered_on, id)` and equivalents need composite indexes is a performance question to settle by measurement (plan §6.3), and any resulting schema change needs the owner's approval under the persistence contract.

### 6.4 Details and file browser

6.4.1 `GET /torrents/{id}` reads the `torrents` row (no blob). Unknown ID → 404.

6.4.2 The file list is obtained through the persistence layer's `GetFiles` semantics (`postgresql-persistence-v2.md` §5): whole list or a typed error, never a partial list. Missing/corrupt blob → HTTP 500 page, `ERROR` log with `torrent_id`, no silent repair.

6.4.3 **Display limits and resource limits are separate requirements.**
- *Display:* at most **[PROPOSAL]** 500 entries per page; directories shown with file count and aggregated size.
- *Resources:* the work to decode the full list and build or look up its index is not reduced by the display limit. It MUST be bounded by: the codec limits (`codec.DefaultLimits`: 250,000 files, 64 MiB uncompressed), a process-wide weighted limit on concurrent decodes, per-request deadlines, and — if used — a size-bounded cache of decoded indexes. A directory navigation MUST NOT silently repeat an unbounded decode.

6.4.4 **Gate.** Before the approach is committed, benchmarks MUST demonstrate acceptable behaviour at the limits, against numeric criteria set and approved by the owner **[OPEN]**: a torrent near 250,000 files; concurrent detail requests; repeated directory navigation; process peak memory and per-request allocations; cold and cached latency. If blob-only storage cannot meet them, the alternatives (cache only, partial decode in the codec, a stored directory index, or limiting browsing) are presented to the owner (plan §6.4); the schema MUST NOT be changed silently.

6.4.5 **Fidelity.** Duplicate `(path, size)` entries are preserved and displayed. Paths are opaque byte strings (`wire-format-v1.md` §1): splitting on `/` is for display grouping only; empty components, leading `/`, invalid UTF-8 and very long components are displayed with explicit markers, never normalised, merged or dropped. Rendering escapes all output.

## 7. API v2 (draft)

7.1 Base `/api/v2`. Authentication: `Authorization: Bearer <token>`. Errors: RFC 9457 `application/problem+json`. Fields `snake_case`; timestamps RFC 3339 UTC; `Cache-Control: private`.

7.2 Endpoints **[PROPOSAL]**:

| Endpoint | Result |
|---|---|
| `GET /torrents?q=&sort=&order=&limit=&cursor=` | `{items: [torrent], next_cursor}`; `limit` ≤ 100 |
| `GET /torrents?info_hash=<40 hex>` | zero or one torrent |
| `GET /torrents/{id}` | torrent, including `magnet_uri` |
| `GET /torrents/{id}/files?limit=&cursor=` | page of the canonical file list; `limit` ≤ 1,000 |
| `GET /feeds/latest.rss` | RSS of the newest torrents |

Torrent object: `id`, `info_hash` (hex), `name`, `total_size`, `file_count`, `discovered_at`.

7.3 File entries carry the path and size. **[OPEN]** representation of paths that are not valid UTF-8 (for example an additional base64 field only when needed). Responses MUST NOT alter path bytes.

7.4 Tokens are not accepted in query strings on v2 (**[OPEN]** for the v2 RSS endpoint).

7.5 No OAuth; no password-based token minting.

## 8. Compatibility surface

### 8.1 Retained in v1

| Endpoint | Decision |
|---|---|
| `GET /api/v1/feed/rss/last` | **Preserved** (§8.2) |

### 8.2 `GET /api/v1/feed/rss/last`

8.2.1 Content type `application/rss+xml`. Query `page` (integer, 1 to 10, values above 10 clamp to 10). Channel and item mapping as in the legacy implementation: item `guid` = info hash (hex), `title` = torrent name, `description` = info hash, `link` = magnet URI (with configured trackers and `dn`), `comments` = absolute URL of the torrent details page, `pubDate` = `discovered_on`, `enclosure` = magnet URI with `length` = `total_size` and `type` = `application/x-bittorrent`. Order: newest first, 1,000 items per page.

8.2.2 **Authentication** (compatibility): API token from header `api-token`, cookie `api-token`, or query parameter `api-token`. The legacy `401` JSON envelope (`{code, status, message, data}`) is retained for failures on this endpoint.

8.2.3 Because the token may appear in the URL, the endpoint MUST: redact the parameter in all logs; send `Referrer-Policy: no-referrer` and `Cache-Control: private`; rate limit per token; never echo the token. The token check MUST run before any cache lookup. Operators SHOULD configure the reverse proxy to omit or mask query strings in access logs; this is outside the application's control and is documented, not enforced.

8.2.4 The feed reads `torrents` only (no blob decoding).

8.2.5 **[OPEN]** page offset. The legacy offset is `page × 1000` with `page` defaulting to 1, so the default feed skips the newest 1,000 torrents. Options: preserve exactly, or make page 1 the newest 1,000. Not decided.

8.2.6 A structural golden test (element set, guid, enclosure) guards the mapping.

### 8.3 Magnet short link

`GET /magnet/{infohash}` (40 hexadecimal characters, case-insensitive) is **public**, performs **no database lookup** (so it cannot reveal whether a hash is stored), and responds `307` with a `magnet:?xt=urn:btih:<hash>` URI that includes the configured tracker list (no `dn`, as legacy). The path MAY change **[OPEN]**; whatever it becomes, the details page MUST show the short link and offer to copy it. Default tracker list **[OPEN]** (the legacy list contains hosts that may be defunct).

### 8.4 Removed

`POST /api/v1/login`, `POST /api/v1/logout`, `GET /api/v1/torrents`, `GET /api/v1/torrents/{id}` are removed. They MUST NOT appear in any v1 OpenAPI document, neither as implemented nor as deprecated. If a consumer is found after cutover they can be reintroduced as an adapter over v2; exact legacy fidelity (`numberOfResults`, `numberOfPages`) cannot be reproduced because no count is computed and the legacy values were constants.

## 9. OpenAPI and documentation

9.1 Each API prefix has an OpenAPI document embedded in the binary: `/api/v1/openapi.yaml`, `/api/v2/openapi.yaml`.

9.2 **Target version: OpenAPI 3.2.1** (the latest published version). Compatibility of a lightweight, vendorable, client-side renderer MUST be evaluated separately at implementation; only if none supports 3.2.1 may the document be downgraded to the latest minor version a renderer supports, and the evaluation recorded. This draft makes no claim about specific renderers.

9.3 The v1 document describes **only** the retained RSS endpoint, including the query/header/cookie token transport and the 401 envelope, and marks the v1 prefix deprecated. It states that the other legacy endpoints do not exist.

9.4 `GET /api` is a landing page linking to `/api/v1` and `/api/v2`, each of which serves interactive documentation and a link to download its specification. The renderer is vendored and pinned (no CDN, no runtime fetch from third parties). Whether these pages require authentication **[OPEN]**.

## 10. Logging, errors, privacy

10.1 Structured logs with request ID, method, route pattern, status, duration, user ID (not name) when authenticated. **[OPEN]** `log/slog` versus the existing `log` package.

10.2 MUST NOT be logged: passwords, session tokens, API tokens, reset/setup/invite codes, full query strings, torrent names or file paths (by default; `postgresql-persistence-v2.md` §8). Path segments carrying codes and the `api-token`/`token` parameters are redacted.

10.3 User-visible errors are generic with a request ID. Failure cases that would reveal account or invitation existence (§4.3.6, §4.7.4, §4.9.1) return identical content.

10.4 Typed storage errors map as: `ErrTorrentNotFound` → 404; `ErrIntegrityMissingBlob` and `ErrCorruptBlob` → 500 with an `ERROR` log carrying `torrent_id`; I/O errors → 503 or 500; none is reported as an empty file list.

## 11. UI requirements

11.1 Server-rendered HTML with auto-escaping; all pages usable without JavaScript. JavaScript, if any, is a single small first-party file (no framework, no CDN, no inline scripts) providing enhancements only.

11.2 Light and dark themes (system preference with an explicit override), usable at 360 px width, keyboard operable, visible focus, labelled inputs, sort state exposed to assistive technology, contrast of at least 4.5:1 for text in both themes, respect for reduced motion.

11.3 No third-party requests from the browser (fonts, icons, scripts, analytics).

## 12. Acceptance and tests (by milestone)

12.1 Milestones W1–W9 and their acceptance criteria are in the plan (§8); this specification is satisfied when those pass and the owner has resolved the open items.

12.2 **Password compatibility tests.** For each supported legacy hash format: PHP-generated vectors (valid password, wrong password, edge lengths), produced by a script and PHP version recorded alongside the fixtures, **independently verified** (generated outside the Go code under test); mixed-format databases; transparent rehash on login; unsupported format handled as "needs reset"; bounded concurrency under load.

12.3 **Revocation tests.** Disable refuses registration through the owner's invitations; enable restores them; concurrent disable versus registration (both orders, repeated, with the race detector) never commits a registration after the disable commit and never consumes the invitation on refusal; tokens stay dead after disable → enable; concurrent revoke versus authenticated request; migrated legacy invitations behave identically.

12.4 **Security tests.** Spoofed forwarded headers from an untrusted peer do not change rate-limit identity; CSRF is enforced on every POST including unauthenticated ones; equal responses for known and unknown usernames, emails and invitations; log capture shows no secrets; headers present; dev-mode cookie rules rejected for non-loopback URLs.

12.5 **Operational constraints for all testing:** disposable databases only (`internal/pgtest`); synthetic data; no production access; **no DHT or other public BitTorrent traffic from the development VM.**

## 13. Open decisions summary

Users schema name/DDL and legacy ID preservation; `golang.org/x/crypto` version and other dependencies; legacy hash audit results; username case policy; invitation code storage; session lifetimes; `roles`; RSS v1 page offset; token-in-query for v2 RSS; email verification and change policy; API-token policy on password change and admin reset; search semantics and timeout; file-tree approach and its numeric acceptance criteria; logging library; documentation pages' access; single versus multiple instances; default tracker list; non-UTF-8 paths in v2 JSON; UI paths and legacy-redirect policy. See the plan, §10, for context.
