# Task 07A — Web UI, authentication and legacy compatibility: design plan

**Status:** design only. Nothing here is implemented, and nothing in this document is approved until the owner says so. The companion draft contract is [`docs/specs/web-v1.md`](../specs/web-v1.md).
**Branch:** `refactor/0.8.0-unify-and-new-storage`. `master` was read as a historical reference and was not modified.
**Not done, by design:** no web code, no schema change, no user migration, no new dependency, no database access, no DHT traffic.

Sources: the PHP/Symfony application on `master` (file paths below are `master:` paths), and the current Go tree (`opflags/`, `main.go`, `persistence/`, `codec/`, `internal/ingest/server/`). Statements about the legacy hash distribution in the production database are **not** known: nobody has queried it (see §3.3).

## 1. Legacy inventory

### 1.1 Routes and access control

From `config/routes.yaml` and `config/packages/security.yaml`:

| Route | Access | Behaviour |
|---|---|---|
| `/` | public | Page with a login button, or the search form when logged in |
| `/torrents` | user | Search results, 20 per page |
| `/torrents/{id}` | user | Details and file tree |
| `/magnet/{infoHash}` (40 hex) | **public** | 307 redirect to a magnet URI with the configured tracker list. No database lookup, no `dn` |
| `/auth/login`, `/auth/logout` | public | Symfony form login, optional remember-me (12 weeks) |
| `/auth/reset/request`, `/auth/reset/{code}` | public | Email reset link; reCAPTCHA v3 on the request form |
| `/register/{code}` | public | Invite-based registration; reCAPTCHA v3 |
| `/account`, `/account/password` | user | Profile, invites, tokens; password change |
| `/profile/api/token/create`, `/profile/api/token/revoke/{key}` | user | **GET** requests that change state |
| `/api/v1/login`, `/api/v1/logout` | public / token | Mint and delete an API token |
| `/api/v1/torrents`, `/api/v1/torrents/{id}` | token | JSON |
| `/api/v1/feed/rss/last` | token | RSS |

`/api/*` is a stateless firewall using `ApiTokenAuthenticator`, which reads the token from the `api-token` **header, cookie or query parameter**.

### 1.2 Search behaviour (`src/Search/TorrentSearcher.php`)

- The query is trimmed and split on whitespace. Words shorter than 3 characters (`mb_strlen`, so characters, not bytes) are discarded.
- Each remaining word becomes `name ILIKE '%word%'`, all ANDed. The word is lower-cased with `strtolower` (ASCII only in PHP 8) and `%` and `_` are **not escaped**, so they behave as wildcards.
- With no usable word the query returns every torrent.
- `order-by` accepts any Doctrine field of `Torrent` except `infoHash` (`name`, `totalSize`, `discoveredOn`, `id`), with `asc` or `desc`. An invalid `order` throws (a 500).
- There is no default sort, so unsorted results have no defined order.
- Pagination is 20 per page, `OFFSET` based, and `PagelessDoctrineORMAdapter` never counts. The UI shows only previous, current and next. In the JSON API `numberOfResults` and `numberOfPages` are therefore always `1`.

### 1.3 Details page and file tree

`FileTreeNode::createFromTorrent` builds the whole tree in PHP memory from all `files` rows and renders every directory as a nested `<details>`. `addChild` **throws `RuntimeException` on a duplicate name**, so a torrent with a duplicate path (which the new storage explicitly preserves, wire-format-v1 §1) would be a 500 page. Leading `/` is trimmed and `a//b` makes an empty-named directory. Nothing is paged.

### 1.4 Users and credentials (`master:src/Entity`, `src/Migrations`)

Schema `users`:

| Table | Columns | Notes |
|---|---|---|
| `users` | `id` identity, `username varchar(25)` unique, `password text`, `email varchar(254)` unique **not null**, `roles json`, `created_at` | The legacy registration form requires an email |
| `invites` | `id`, `user_id` (owner), `code varchar(32)` unique, `used_by_id` null | Code is plaintext `md5(random_bytes(100))` |
| `api_tokens` | `key varchar(32)` PK, `user_id`, `created_at` | Plaintext `md5(random_bytes(100))`; no label, no expiry |
| `password_reset_tokens` | `code text` PK, `user_id`, `valid_until` | Plaintext `sha3-384(uniqid())`, 1 day |

Password hashing, reconstructed from `git log -p -- config/packages/security.yaml`:

| Period | Config | Resulting hash |
|---|---|---|
| 2018-06 → 2020-01 | `argon2i`, memory 16384 KiB, time 2, threads 4 | `$argon2i$v=19$m=16384,t=2,p=4$…` |
| 2020-01 → 2022-07 | `auto` | Whatever PHP's `PASSWORD_DEFAULT` was then (bcrypt `$2y$` or argon2id, depending on the runtime); **not determinable from the repo** |
| 2022-07 → now | `sodium` | `$argon2id$v=19$m=65536,t=4,p=1$…` (libsodium defaults; **to be confirmed with vectors**) |

There is no `PasswordUpgraderInterface` implementation (`UserRepository` has no `upgradePassword`), so Symfony did not necessarily rehash on login. The production table may contain a **mix** of formats. Which formats exist, and how many users each, is unknown. §3.3 defines the audit the owner must run.

Sessions are native PHP sessions plus a remember-me cookie signed with `APP_SECRET`, lifetime 7,257,600 s (12 weeks). Neither can be carried over: every user must log in again after cutover.

Registration (`UserController::register`, `InviteManager`): an unused invite is required, and each new user gets `NEW_USER_INVITES` (default 10) invites. Password reset (`PasswordResetManager`) emails a link, with a `@todo add limits`, and the response tells the requester when the address does not exist ("User not found"). Resetting a password does not invalidate existing sessions or tokens.

### 1.5 API v1 (`src/Api/V1`)

Envelope on every response, `ApiResponse`: `{ "code": int, "status": "success"|"error"|"fail"|"unknown", "message": string|null, "data": … }`.

- `POST /api/v1/login` (form fields `username`, `password`): returns the new token as `data` (a 32-hex string). It creates one new token per login, unbounded. It answers "User not found" or "Invalid password", which enumerates users.
- `POST /api/v1/logout`: deletes the presented token. Errors are returned as HTTP 500 (even for "no token provided").
- `GET /api/v1/torrents?query=&page=&order-by=&order=`: `data` is `{numberOfPages, currentPage, numberOfResults, maxPerPage, items[]}` with `items` of `{id, infoHash (hex), name, totalSize, discoveredOn (unix s)}`.
- `GET /api/v1/torrents/{id}`: the same fields plus `files[{size, path}]`.
- `GET /api/v1/feed/rss/last?page=N`: see §1.6.

### 1.6 RSS (`RssController`, `RssGenerator`)

- Content-Type `application/rss+xml`. Generated with laminas-feed, cached 600 s in Redis per page.
- 1,000 items per page, `page` defaults to 1 and is clamped to 10.
- **Bug:** the offset is `$page * 1000` while `page` starts at 1, so the default feed **skips the newest 1,000 torrents** and page 10 starts at 10,000. Sorting is `id DESC`.
- Channel: title `Last`, description `Last Last torrents`, link `index`, atom `feedLink`, language `en-US`, author `Magnetico Web`.
- Item: `guid` = info hash, `title` = name, `description` = info hash, `link` = magnet URI (with trackers and `dn`), `comments` = absolute `/torrents/{id}` URL, `pubDate` and `lastModified` = `discovered_on`, `enclosure` = the magnet URI with `length` = `total_size` and `type` = `application/x-bittorrent`.
- The account page offers the RSS URL as `…/api/v1/feed/rss/last?api-token=<key>`, because torrent clients cannot send headers. This is the only reason the query parameter exists.

### 1.7 Things the legacy code gets wrong (not to be copied)

- State-changing GET routes: token create, token revoke, logout.
- No CSRF on API login; no rate limiting anywhere; user enumeration on login and reset.
- Tokens, invite codes and reset codes stored in plaintext; tokens accepted from the query string on every API endpoint (they end up in access logs and Referer headers).
- No session or token invalidation on password change or reset.
- reCAPTCHA (a third-party call) as the only abuse control.
- Duplicate-path crash, empty-directory-name tree, default RSS offset bug, 500 on an invalid `order`.
- Absolute URLs built from the request (trusted-proxy configuration dependent).

## 2. Compatibility classification

| # | Feature | Class | Decision (proposal) |
|---|---|---|---|
| 1 | Name search, AND of words, ILIKE | **Essential** | Keep, with the fixes in §6 |
| 2 | Details page with file hierarchy | **Essential** | Keep; redesign (paged, duplicate-safe) |
| 3 | Sort by name, size, discovered | **Essential** | Keep; add a deterministic default (newest first) |
| 4 | Private by default, local accounts | **Essential** | Keep; redesign sessions/CSRF/limits |
| 5 | Invite-based registration | **Essential** (owner decision) | Keep, plus an admin CLI |
| 6 | Email password reset | **Essential** (owner decision) | Keep, hardened, optional SMTP |
| 7 | API tokens (create, list, revoke) | **Essential** | Keep, hashed at rest; same tokens usable on v2 |
| 8 | RSS at `/api/v1/feed/rss/last` | **Essential** (known consumer) | Preserve URL and `api-token` auth |
| 9 | Public short magnet redirect | **Essential** | Keep, may move; must be linked from the UI |
| 10 | Account page, password change | **Essential** | Keep, redesign |
| 11 | JSON torrent list/detail API | Worth redesigning | New `/api/v2`; v1 JSON endpoints removed |
| 12 | RSS content | Worth redesigning | v1 stays byte-compatible in structure; new v2 feed optional |
| 13 | Pagination | Worth redesigning | Keyset cursors, no COUNT, no OFFSET |
| 14 | File tree rendering | Worth redesigning | Server-rendered, lazy per directory |
| 15 | Search rules help | Worth redesigning | Short inline hint |
| 16 | `/api/v1/login`, `/api/v1/logout` | Optional compatibility | **Removed**; tokens come from the account page |
| 17 | `/api/v1/torrents`, `/api/v1/torrents/{id}` | Optional compatibility | **Removed**; re-implementable on demand (§5) |
| 18 | `api-token` in query string for non-RSS endpoints | Legacy not to retain | Removed |
| 19 | Native PHP sessions, remember-me cookie | Legacy not to retain | New server-side sessions |
| 20 | reCAPTCHA | Legacy not to retain | Rate limiting instead; no third-party calls |
| 21 | GET state changes | Legacy not to retain | POST + CSRF |
| 22 | Plaintext tokens/codes at rest | Legacy not to retain | Hashed (invite storage open, §10) |
| 23 | `ROLE_*` `roles` column | Open | No admin UI exists; role migration is an owner decision |
| 24 | Font Awesome, PicoCSS, Redis, Sentry, Doctrine | Legacy not to retain | Not carried over |

## 3. Authentication and user model

### 3.1 Principles

- Everything except `/magnet/*`, `/login`, `/register/*`, password reset/setup pages, health endpoints, static assets and the RSS endpoint (token-authenticated) requires an authenticated session or API token.
- Only the Go web process authenticates. Sessions, tokens and CSRF state live in PostgreSQL (a future `magnetico_users` schema; not created here), so the web service can restart or be replicated without logging users out. The web service never contacts the crawler or ingest.
- No account lockout (a lockout is a denial-of-service lever); throttling instead.

### 3.2 Account lifecycle and onboarding (admin-created users)

`magnetico web user add <username> [--email E] [--invites N]` creates the row with `password_hash = NULL` and state `pending_setup`:

- A `pending_setup` account **cannot authenticate**: login code refuses any account whose `password_hash` is NULL, regardless of input. There is no empty or default password.
- The command prints a **single-use, expiring password-setup link** (`/password/setup/{token}`) built on the same hashed-token table as password reset (`purpose = setup`, ≈72 h by default; configurable).
- The user opens the link and sets their own password. The account becomes `active`, the token is consumed, and sessions start only after a normal login.
- **If the link expires**, the account stays `pending_setup` and nothing else changes. The administrator issues a new link with `web user reset-password <username>`; every new link invalidates earlier ones for that account.
- The administrator never learns the password, except in the explicit rare `set-password` case, where the password is read from a prompt or stdin (never from argv, never printed).

Other commands: `web user reset-password`, `set-password`, `disable`, `enable`, plus `web user invite add|revoke`. Output must not contain secrets other than the one link being issued.

### 3.3 Existing credentials and migration (a separate, later task)

The migrator is **not** designed in detail here. What the web service must support, and what must be decided first:

1. **Hash formats.** Password hashes cannot be converted offline (the plaintext is unavailable). The web service therefore must **verify legacy PHC strings directly**, and rehash into the current format on successful login. A format counts as supported only when it has independently verified PHP-generated test vectors; recognising a prefix is not support. Candidates, to be confirmed by the audit: `$argon2i$`, `$argon2id$`, `$2y$` (bcrypt). Anything unrecognised makes that user "needs reset" rather than login-able.
2. **Audit query (owner runs it; this task does not touch production).** Aggregate only, no hashes or usernames in the output, for example:

```sql
SELECT split_part(password, '$', 2) AS algo,
       CASE WHEN password LIKE '$argon2%' THEN split_part(password, '$', 4) END AS params,
       count(*)
FROM users.users GROUP BY 1, 2 ORDER BY 3 DESC;
```

   The result decides which verifiers must be built and which test vectors are required. It is an open prerequisite for the migration task.
3. **Case collisions.** The legacy unique index on `username` is case-sensitive. Whether usernames become case-insensitive is an owner decision (§10); the migrator must report collisions rather than choose.
4. **API tokens.** Legacy tokens are 32-hex plaintext. Hashing them with SHA-256 keeps verification working (the same string presented in an old RSS URL hashes to the stored value) but **cannot recover the plaintext**. The account page must therefore show migrated tokens as masked entries ("legacy token, created …") and must not pretend to display them. New tokens are shown exactly once. Existing RSS URLs keep working.
5. **Invites.** Legacy invite codes are 32-hex plaintext. Whether the new storage hashes them (cannot re-display them) or keeps them readable is open (§10). The "suspension while the owner is disabled" rule in §3.5 applies to migrated invites too.
6. **Not migrated:** sessions, remember-me cookies, reset tokens, reCAPTCHA state. All users sign in once after cutover.
7. **Roles.** Legacy `roles` is `["ROLE_USER"]` for ordinary users; there is no admin UI in the PHP app. Whether `roles` is carried over or dropped is an owner decision.

### 3.4 Passwords

- New hashes: argon2id in PHC string format; parameters are fixed in the spec as a proposal (OWASP/RFC 9106 second recommended option) and benchmarked on the target host before release.
- New-password length: 8 to 1,024 bytes (bounded to avoid hashing DoS); login accepts up to the legacy 4,096.
- **Bounded global hashing concurrency.** A single process-wide semaphore limits concurrent hash/verify operations (default `max(1, min(GOMAXPROCS, 2))`), with a bounded wait. Argon2 at 64 MiB per operation means the cap also bounds memory. When the wait times out, the request fails with a generic error (HTTP 503 with `Retry-After`) and is counted.
- Unknown usernames still perform a dummy verification so response timing does not reveal account existence.
- Rehash on login when the algorithm or parameters are outdated, inside the same transaction as the `last_login_at` update.
- The `golang.org/x/crypto` module is currently an **old indirect** requirement (a 2022 pseudo-version in `go.mod`). Using argon2 or bcrypt means a direct dependency. Selecting a maintained version is part of a later dependency review; reusing the old version is not proposed.
- An optional breached-password check (HIBP-style) would leave the host; it is not proposed (§10).

### 3.5 Revocation and "active" semantics

Authentication middleware checks account state on **every** session or token request, in the same query that resolves the credential (session hash → user row → `active` and credential epoch). Removing session rows alone is not sufficient, because API tokens are not sessions.

| Event | Sessions | API tokens | Invites owned by the user |
|---|---|---|---|
| `disable` | All revoked immediately; login refused | Rejected (the middleware checks `users.active`); marked revoked | **Suspended, not revoked**: rows and codes unchanged; registration through them is refused while the owner is inactive |
| `enable` | None restored; the user logs in again | **Not** silently restored; new tokens must be created | Usable again automatically, unless used, explicitly revoked or expired |
| Self-service password change | Other sessions revoked, the current one rotated | Kept by default, with an "also revoke all tokens" option (proposal; open, §10) | Unchanged |
| Admin `reset-password` / `set-password`, email reset | All revoked | Revoked (proposal; open, §10) | Unchanged |
| Explicit invite revocation | n/a | n/a | Permanent (`revoked_at`); `enable` never undoes it |

Design rules:

- Disabling and re-enabling an account must not make previously revoked credentials work again. "Revoked" is a stored state (`revoked_at`), not a derived one.
- Invites are different on purpose: disabling the owner **suspends** them. The invite rows stay untouched, so re-enabling restores them without any bookkeeping. This applies equally to migrated legacy invites.
- **Registration is a single transaction.** It locks the invite row `FOR UPDATE` and its owner's `users` row `FOR SHARE`, verifies that the invite is unused, not revoked, not expired, and that the owner is `active`, then creates the user and marks the invite used. `disable` takes `FOR UPDATE` on the same user row, so a registration cannot succeed after a disable transaction has committed, and a rejected registration never consumes the invite.
- The registration page shows one generic message ("This invitation is invalid or no longer available") for unused-but-suspended, used, revoked, expired and unknown codes. It never reveals that the owner is disabled.
- **In-flight requests:** a request already authorised may complete; the next request sees the new state. There is no authentication cache in the proposal. If a cache is ever introduced, its TTL becomes the documented revocation latency.
- Tests (milestones W2, W3, W6): disable refuses registration; enable makes the same invite usable; concurrent disable versus registration (run both orders many times, assert registration never commits after disable commits and the invite is not consumed on refusal); concurrent revoke versus authenticated request; migrated invite behaves identically; tokens stay dead after disable → enable.

### 3.6 Sessions, cookies, CSRF

- Opaque 32-byte random session token; PostgreSQL stores only its SHA-256. A new session is issued at login (no fixation) and rotated on password change and privilege change.
- Production cookie: `__Host-magnetico_session`; `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`. The `__Host-` prefix and `Secure` are **never** weakened for local testing.
- Development on a loopback `http://` public URL uses an explicit dev mode with a different cookie name without the `__Host-` prefix and without `Secure`; the mode is rejected when the public URL is not a loopback `http://` URL.
- Lifetimes are proposals (open, §10): idle 12 h; "remember me" 90 days absolute with sliding refresh capped to the absolute limit. `last_seen` writes are throttled.
- **CSRF covers every state-changing browser action**, including those that need no existing session: login, registration, forgot-password, password reset, password setup and account actions. Two layers: Go's `http.CrossOriginProtection` (Fetch metadata / `Origin` checks) and a synchronizer token in every form. Before a session exists the token is bound to a short-lived pre-session cookie. All state changes are `POST`; nothing changes state on `GET`.
- Logout is a `POST`.

### 3.7 Rate limiting and brute force

In-memory sliding windows per client IP and per normalised username (login), per IP and per email (reset requests), per IP (registration, token 401s), with bounded memory (LRU of keys) and generic error text. This assumes **one web instance**; multiple replicas would need shared state (open, §10).

### 3.8 Proxy and HTTPS assumptions

- The web service listens on plain HTTP (same convention as ingest, `opflags/ingest.go` `Listen`) and is run behind Traefik, which terminates TLS.
- `--trusted-proxies` is a CIDR list. `Forwarded` / `X-Forwarded-*` headers are honoured **only when the direct TCP peer is inside that set**; otherwise they are ignored and the peer address is the client. A direct client therefore cannot spoof its address to evade rate limits. With a trusted peer, the client IP is the right-most address in the `X-Forwarded-For` chain that is not itself a trusted proxy.
- A required `--public-url` determines every absolute URL (RSS, reset, setup and invite links). The `Host` header is never used for that.
- Strict response headers: CSP, `X-Content-Type-Options`, `Referrer-Policy`, `Cache-Control: private/no-store` on authenticated pages, `Permissions-Policy`.

### 3.9 Sensitive logging and errors

No passwords, session tokens, API tokens, reset/setup/invite codes or full query strings are logged. The `api-token` / `token` query values and `/register/{code}`, `/password/*/{token}` path segments are redacted in the access log. File paths and torrent names are not logged by default (matches `postgresql-persistence-v2.md` §8). Errors shown to users are generic with a request ID; details go to the log.

The legacy RSS endpoint, because it accepts the token in the query string, additionally gets `Referrer-Policy: no-referrer` and `Cache-Control: private`. **Traefik access logs** record the query string by default; operators must drop or redact it (documented in the deployment notes). That is outside the application's control and must not be presented as solved.

## 4. Architecture

### 4.1 Shape

`magnetico web` is its own command and its own container. It depends on PostgreSQL only. A test asserts that the web command's dependency graph does not include `internal/crawler`, `internal/crawlqueue`, `internal/ingest` or `modernc.org/sqlite`.

```
HTTP (net/http ServeMux, Go 1.22 patterns)
  └─ middleware chain
       request-id → recover → access-log(redacting) → security-headers
       → trusted-proxy client IP → CrossOriginProtection → auth → rate-limit
  └─ handlers (ui | api/v1 rss | api/v2)         internal/web/{ui,api}
  └─ application services (search, torrents,     internal/web/service
       accounts, tokens, invites)
  └─ stores:  persistence (torrents, blobs)      existing package, additive reads
             users store (sessions, tokens, …)    internal/users (new)
```

Handlers contain no SQL and no business rules; services contain no HTTP types; stores contain no policy. Services take small interfaces so they can be tested with fakes (the repo already does this for ingest, `server_test.go`).

### 4.2 Routing, rendering, assets

- Standard-library router; no third-party router.
- `html/template` with templates embedded via `embed.FS`; auto-escaping everywhere; every untrusted byte string (torrent names, file paths, which may be invalid UTF-8, `codec` wire-format §1) is converted with an explicit replacement-character policy **for display only**; raw bytes are never altered in storage or in API responses that carry them (see §6.4).
- One small hand-written CSS file with custom properties (light and dark themes via `prefers-color-scheme` plus a persisted override) and an inline SVG sprite instead of Font Awesome.
- Progressive enhancement through one small vanilla JS file (target < 6 KB): copy-to-clipboard, theme toggle, `/` to focus search, in-place directory expansion that falls back to a normal link. No JavaScript framework, no htmx, no CDN, no build step.
- Assets are embedded and served with content-hashed names and long-lived caching; CSP forbids inline scripts.
- The interactive API documentation page is the single place that needs a vendored renderer (§5.4).

**UI approach considered and rejected for now:** an SPA (React/Vue/Svelte), htmx, and Pico CSS. None is needed for six pages; each adds a toolchain, dependency or supply-chain surface. Re-evaluate only if a concrete interaction cannot be built with the above.

### 4.3 PostgreSQL

- A dedicated `pgxpool` for the web service, explicit `pool_max_conns` (default 10), health check, and per-request `context` deadlines. Existing `persistence.connectPool` uses an unconfigured pool; web should pass explicit settings.
- Startup runs the existing read-only `verifyStartup` (`pg_trgm`, schema objects, `schema_version`) and fails fast; a later users-schema version check is added. No automatic migration (postgresql-persistence-v2 §2).
- Recommended roles: a dedicated database role with `SELECT` on `magneticod.*` and DML on the users schema only. The web service never writes torrent data.
- **Persistence additions** (read-only, no schema change, signatures illustrative): get torrent by integer ID; get file list by ID; keyset `QueryTorrents`. Today `persistence` has `GetTorrent(infoHash)`, `GetFiles(infoHash)` and `QueryTorrents` with a `LIMIT` only (`persistence/interface.go`, `postgres.go:296-349`), no offset, no cursor, no by-ID lookup.
- Statement timeout for search; a timeout is reported as "query too broad", not a 500.
- Typed error mapping: `ErrTorrentNotFound` → 404; `ErrIntegrityMissingBlob`/`ErrCorruptBlob` → 500 with an `ERROR` log carrying `torrent_id` (postgresql-persistence-v2 §5).

### 4.4 Configuration, logging, shutdown

- `WebOptions` modelled on `IngestOptions` (`opflags/ingest.go`): flags, then `MAGNETICO_WEB_*` environment variables, then defaults; unknown `MAGNETICO_WEB_*` variables are rejected; no config-file layer in the first version. `parseArgs` and `applyConfig` need a `web` case (today `opflags/config.go:183-195` returns `unknown command`).
- Secrets (database URL with password, SMTP password) are environment-only and never printed; the DSN is redacted with the existing helper pattern (`main.go` `redactDatabaseURL`).
- Logging: the repository uses stdlib `log` today. The proposal is structured `log/slog` for the web service only (open, §10).
- Graceful shutdown follows the ingest pattern (`signal.NotifyContext`, bounded grace, in-flight tracking, `internal/ingest/server/service.go`).
- Health: `/healthz` (process) and `/readyz` (database reachable and schema verified); neither is authenticated nor reveals versions or counts.
- The CLI subcommands for operators (`web user …`) share the users store and open the database directly; they run against a stopped or running service.

### 4.4a Dependencies

Proposed new direct dependencies are limited to: an argon2/bcrypt implementation (`golang.org/x/crypto`, version to be selected), an SMTP client if the standard `net/smtp` is judged insufficient (probably it is sufficient for plain submission with STARTTLS), and a vendored, pinned documentation renderer for `/api` (static files, not a Go module). Nothing is added in Task 07A. Each needs owner approval.

## 5. API, RSS and magnet compatibility

### 5.1 Per-endpoint assessment

| Endpoint | Historical behaviour | Auth | Data | Cost to keep | Recommendation |
|---|---|---|---|---|---|
| `GET /api/v1/feed/rss/last` | §1.6 | token (header/cookie/query) | `torrents` only (no blob decode) | Low: one indexed query per page; deterministic XML | **Preserve** URL, token transport and item mapping. Document that the default page skip is fixed or kept (open §10) |
| `GET /magnet/{40hex}` | public 307, no DB | none | none | Trivial | **Preserve** the path (Telegram-bot use). Linked from the details page and copy actions |
| `POST /api/v1/login` | mint token from password | public | `users`, `api_tokens` | Medium (login throttling, rehash, token policy) | **Remove**. Tokens are created on the account page |
| `POST /api/v1/logout` | delete presented token | token | `api_tokens` | Low | **Remove** (revocation lives on the account page) |
| `GET /api/v1/torrents` | JSON, OFFSET paging, `numberOfPages` always 1 | token | `torrents` | Medium: reproducing the envelope and fake counts honestly is not possible | **Remove**; replaced by v2. Re-implementable later if a consumer appears |
| `GET /api/v1/torrents/{id}` | JSON + `files[]` | token | `torrents` + blob decode | Medium | **Remove**; v2 pages the file list |

The owner stated that RSS readers are known consumers, that `/magnet` may move but must stay reachable, that the v1 JSON API may be redesigned as v2 with v1 re-implementable on demand, and that `api-token` in the query may be dropped except for the v1 RSS endpoint. The table follows those statements.

**Honesty about compatibility.** The new schema cannot reproduce `numberOfResults`/`numberOfPages` (no COUNT is run, and the legacy code returned constants), and `files` for very large torrents must be paged. A v1 JSON shim would therefore be an approximation, not a drop-in; that is one reason to remove it rather than promise it. If a consumer is discovered after cutover, an adapter can be written; its cost is small (≈ a day: envelope plus field mapping) but its fidelity limits must be documented.

### 5.2 RSS v1 details

- Same channel and item mapping as §1.6; `Content-Type: application/rss+xml`. A golden-file test compares structure (element set, guid, enclosure), not bytes (the XML serialiser changes).
- Authentication stays `api-token` from header, cookie or **query** for this one endpoint, so existing torrent-client URLs continue to work. Failed authentication returns the legacy JSON envelope with HTTP 401, as before.
- Added protections: token redaction in logs, `Referrer-Policy: no-referrer`, `Cache-Control: private`, per-token rate limit, no token echo in the body.
- The feed reads `torrents` only (`id DESC LIMIT 1000`), never blobs.
- Page numbering: **open decision.** The faithful reading of the legacy code skips the newest 1,000 items on `page=1`. Options: keep the offset (consumers see exactly what they saw), or fix it so `page=1` is the newest 1,000 (consumers may see more items on first poll; GUIDs are info hashes so readers de-duplicate). Not decided here.
- Cached in-process for a short TTL (legacy: 600 s). With revocation semantics in §3.5 the cache must be keyed on the page only, and the token check must run on every request, before the cache.

### 5.3 API v2 (proposal)

Deliberately small. Bearer tokens from the account page (the same tokens as v1, per the owner), no OAuth.

- `GET /api/v2/torrents?q=&sort=&order=&limit=&cursor=` returns `{items, next_cursor}`; `GET /api/v2/torrents?info_hash=<40 hex>`; `GET /api/v2/torrents/{id}` (includes `magnet_uri`); `GET /api/v2/torrents/{id}/files?limit=&cursor=` over the flat canonical list; `GET /api/v2/feeds/latest.rss`.
- Conventions: `Authorization: Bearer <token>`, `snake_case` JSON, RFC 3339 timestamps, RFC 9457 `application/problem+json` errors, cursor pagination, `Cache-Control: private`, no tokens in query strings (whether v2 RSS should accept one is open, §10).
- Path bytes in v2 file listings: paths are opaque byte strings (wire-format-v1 §1). JSON requires valid UTF-8, so the representation of non-UTF-8 paths needs a rule (for example an additional `path_b64` field only when needed). Draft in the spec; open.

### 5.4 OpenAPI documents and `/api`

- Specs are written by hand, embedded in the binary, one per prefix: `/api/v1/openapi.yaml`, `/api/v2/openapi.yaml`, with a landing page at `/api` linking to both, and an interactive documentation page for each prefix.
- Target version: **OpenAPI 3.2.1** (the latest published version, per the owner). Renderer compatibility must be evaluated separately at implementation time; only if no lightweight client-side renderer supports 3.2.1 is the spec downgraded to the latest minor that is supported, and the evaluation is recorded. This design makes no claim about which renderers support which version.
- The **v1 specification describes only the retained RSS endpoint**: its parameters, the `api-token` header/cookie/query transport, the 401 envelope. The removed v1 JSON endpoints and login/logout do not appear in it, neither as implemented nor as deprecated. The v1 prefix is documented as deprecated as a whole.
- Documentation pages are served with a relaxed CSP limited to those routes; whether they require login is open (§10). The renderer is vendored and pinned (no CDN).

## 6. Search, listing, file browsing

### 6.1 Search semantics (draft decisions, not approved)

| Aspect | Legacy | Proposed |
|---|---|---|
| Word minimum | 3 characters, `mb_strlen` | 3 **Unicode characters**, counted as code points (runes) of the user's text without normalisation; this matches `mb_strlen` for UTF-8 input. Grapheme-cluster differences are not addressed |
| Matching | `name ILIKE '%w%'`, AND | Same, case-insensitive, AND |
| Case folding | PHP `strtolower` (ASCII) then `ILIKE` | PostgreSQL `ILIKE` only (locale-dependent for non-ASCII; to be documented and tested) |
| Wildcards | `%` and `_` typed by the user are **wildcards** | Escaped: user text is a **literal** substring (reuses `escapeLikePattern`, `persistence/postgres.go:353`). Behaviour change, documented |
| Empty query | every torrent, no defined order | latest torrents |
| 40-hex query | name search | exact info-hash lookup (addition) |
| Timeout | none | statement timeout, shown as "query too broad" |

The word-length rule, literal matching, the 40-hex shortcut and the timeout are **draft decisions** for the owner.

### 6.2 Ordering and pagination

- Sorts: `discovered` (default, descending), `size`, `name` (only with a query). Each sort has an `id` tie-breaker so keyset cursors are stable.
- **Keyset pagination** (`WHERE (sort_col, id) < ($1, $2) … LIMIT n+1`), opaque cursor in the URL, no `COUNT(*)`, no `OFFSET`. "Previous" is implemented by a reversed query. Page sizes 20/50/100.
- Existing indexes (`postgresql-persistence-v2.md` §2): `idx_torrents_total_size`, `idx_torrents_discovered_on`, `idx_torrents_name_gin_trgm`. A keyset on `(discovered_on, id)` can use the single-column index but must still sort ties; whether a composite index is required is a **performance question for validation (§8)**, not a schema change proposed here.

### 6.3 Costs to validate (not changed now)

- Trigram AND of several common words on a very large table: the planner may choose a bitmap of the most selective term; very short common terms can scan widely. Needs EXPLAIN (ANALYZE) on a realistic disposable dataset.
- Sorting a trigram result by `discovered_on`/`total_size`: requires sorting all matches unless the planner walks the sort index and filters. For broad terms this can be seconds; the statement timeout bounds it.
- `name` sort has no supporting index.
- Latest-first listing and the RSS query use `idx_torrents_discovered_on` (or `id`); both should be an index scan with `LIMIT`.

### 6.4 File tree: display limits are not resource limits

`codec.Decode` returns the **entire** file list as a `[]File` (no streaming/iterator API; `codec/codec.go:97-110`), with limits up to 250,000 files and 64 MiB uncompressed. Even if the page shows 500 entries, the server decodes and holds the full list to build or index it. Two limits must be kept apart:

1. **Display limit:** entries per page (proposal ≤ 500), and a hard cap on rendered rows.
2. **Resource limit:** CPU and memory to decode, index and drop the list, which is *independent* of how many rows are shown. Bounded by the codec limits, a global weighted semaphore on concurrent decodes (weighted by the stored blob size), per-request deadlines, and optionally a small LRU of decoded/indexed lists.

Consequence: "directory-by-directory browsing" reduces HTML size and browser cost, but **each directory click would repeat the full decode** unless a cache holds the list or tree. That cost is not acceptable to assume away.

Proposed approach, to be validated before commitment (gate in W4): on first request for a torrent build a compact index over the canonical sorted list (the list is already sorted by path bytes, so every directory is a contiguous range: directory = `[lo, hi)` plus a count and size sum; no per-node allocation needed), keep it in a size-bounded LRU keyed by torrent ID and blob identity, and serve subsequent directory views from the index.

Measurable acceptance criteria to set **before** choosing this approach (numbers are proposals for the owner to confirm, and have not been measured):

- Torrent at ≈ 250,000 files (the codec limit): decode + index build memory per request and its peak RSS delta; p95 latency of the first view; cold versus cached.
- N = 8 and N = 32 concurrent detail requests for large torrents, process peak RSS, and no request starved beyond a bounded wait.
- 100 successive directory navigations on one large torrent: cache hit rate, per-navigation allocations (target: no full decode after the first).
- Heap behaviour under `GOMEMLIMIT` settings like the ones used for ingest (256 MiB was recommended there; the web service needs its own recommendation).

If blob-only storage makes efficient browsing too expensive, the alternatives for owner review (none is applied here, the schema is not changed) are:

| Alternative | Cost | Needs approval of |
|---|---|---|
| A. Bounded decoded-index cache in the web process | Memory; cold first view stays expensive | Nothing beyond W4 results |
| B. Partial/streaming decode in `codec` (directory-trie serializer could allow skipping subtrees) | Codec work; wire-format interplay | Owner (wire-format/codec scope) |
| C. Precomputed per-torrent directory index stored in the database | Schema change, migrator and ingest changes | Owner (schema contract) |
| D. Hard-limit browsing to torrents up to N files, flat paged list above | UX limitation | Owner |

**Fidelity requirements:**

- Duplicate `(path, size)` entries are preserved and shown (no merging, no error).
- Paths are split on `/` for **display grouping only**; unusual components (empty segments from `a//b`, leading `/`, invalid UTF-8, very long components) are shown as-is, with explicit visual markers, and not normalised, deduplicated or "fixed".
- Sizes are summed with overflow checks; mismatch with `torrents.total_size` or `file_count` is already an `ErrCorruptBlob` from `persistence`.
- Never a partial list on error.

## 7. UI/UX design

### 7.1 Principles

- A search-first, dense, quiet interface for a private tool. Light and dark. Works at 360 px width.
- Everything functional without JavaScript; JS only enhances.
- Accessibility: semantic landmarks, one `h1` per page, visible focus, keyboard-operable tables and file tree (native `<details>`/links), contrast ≥ 4.5:1 in both themes, `prefers-reduced-motion`, sort state exposed with `aria-sort`, results announced, labels on all inputs, no colour-only status.
- Large result sets: cursor pagination, stable sort, "load more"/prev-next, no infinite scroll; huge file lists are paged.

### 7.2 Login

```
┌──────────────────────────────────────────────┐
│ ◎ Magnetico                          ☾ theme │
├──────────────────────────────────────────────┤
│                                              │
│            Sign in                           │
│   Username  [______________________]         │
│   Password  [______________________]         │
│   [x] Keep me signed in on this device       │
│   [ Sign in ]        Forgot password?        │
│                                              │
│   (generic error: "Wrong username or         │
│    password, or the account is unavailable") │
└──────────────────────────────────────────────┘
```

"Forgot password?" appears only when SMTP is configured.

### 7.3 Search results, desktop

```
┌─ Magnetico ──────────────────────────────── [☾] alice ▾ ─┐
│ [ 🔍 ubuntu 24.04 desktop                    ] [Search]   │
│ Sort: ( Newest ▾ )   Page size: ( 50 ▾ )   hint: ≥3 chars │
├───────────────────────────────────────────────────────────┤
│   Name                           Size  Files  Discovered  │
│ ⧉ ubuntu-24.04-desktop-amd64.iso 5.7 GiB   1  2026-11-21  │
│ ⧉ Ubuntu 24.04 Server (pack)     3.1 GiB  14  2026-11-20  │
│ ⧉ …                                                       │
├───────────────────────────────────────────────────────────┤
│                         [‹ Newer]   [Older ›]             │
└───────────────────────────────────────────────────────────┘
```

`⧉` copies the magnet URI; column headers are links with `aria-sort`; the name opens the details page. No total count is shown (none is computed); empty and "query too broad" states have their own messages.

### 7.4 Search results, mobile

```
┌────────────────────────────┐
│ ◎ Magnetico        ☰ ☾     │
│ [ 🔍 search…       ] [Go]  │
│ Newest ▾                   │
├────────────────────────────┤
│ ubuntu-24.04-desktop-…iso  │
│ 5.7 GiB · 1 file · 11-21 ⧉│
├────────────────────────────┤
│ Ubuntu 24.04 Server (pack) │
│ 3.1 GiB · 14 files · 11-20⧉│
├────────────────────────────┤
│ [‹ Newer]        [Older ›] │
└────────────────────────────┘
```

### 7.5 Torrent details and file browser

```
┌─ ‹ Results ────────────────────────────────────────────┐
│ ubuntu-24.04-desktop-amd64.iso                          │
│ 5.7 GiB (6,123,456,789 B) · 1 file · found 2026-11-21   │
│ Info hash  a1b2…(40 hex)  [Copy]                        │
│ [ Magnet link ]  [ Copy magnet ]  [ Share short link ]  │
├─────────────────────────────────────────────────────────┤
│ Files            /  docs / en /          [sort: name ▾] │
│ 📁 images/            1,204 files   2.1 GiB             │
│ 📁 docs/                 38 files   14 MiB              │
│ 📄 README                           3 KiB               │
│ 📄 README  (duplicate path)         3 KiB               │
│                       [‹ Prev]   showing 1–500   [Next ›]│
└─────────────────────────────────────────────────────────┘
```

Breadcrumbs and directory rows are ordinary links (`?path=…`); with JS a directory expands in place. Aggregates (count, size) come from the index (§6.4). Duplicates are visibly marked, odd paths shown with a marker; a corrupt blob yields a clear error page, never a partial list.

### 7.6 Account

```
┌─ Account — alice ────────────────────────────────────────┐
│ Profile   username alice · email a@… [Change]            │
│ Password  ••••••••                       [Change]          │
│                                                            │
│ API tokens                                  [ New token ]  │
│ ┌────────────┬───────────────┬──────────┬──────────────┐ │
│ │ Label      │ Created       │ Last used│              │ │
│ │ rss-reader │ 2026-11-22    │ today    │ [Revoke]     │ │
│ │ legacy ····│ 2019-03-04    │ —        │ [Revoke]     │ │
│ └────────────┴───────────────┴──────────┴──────────────┘ │
│ A new token is shown once, in a banner, with a ready-made │
│ RSS URL. Legacy tokens are masked: their value can't be   │
│ shown, but existing RSS URLs keep working.                │
│                                                            │
│ Invitations   3 available                    [ Copy link ] │
│ Used: bob (2026-10-02)                                      │
└────────────────────────────────────────────────────────────┘
```

The email field is optional (a user without an email cannot use self-service reset; they need the administrator). Revocation and token creation are POST forms with CSRF.

## 8. Implementation milestones

Each milestone ends in a reviewable change set and a stop. No milestone starts without owner approval. Database schema steps need the owner's approval of the DDL first.

| # | Milestone | Scope | Acceptance criteria | Test strategy |
|---|---|---|---|---|
| W1 | Foundation | `WebOptions`, `web` dispatch, server, middleware chain, trusted-proxy handling, `/healthz`/`/readyz`, embedded static assets, startup verification, graceful shutdown, import-boundary test | Starts only with a verified schema; refuses bad config; graceful stop within the bound; forwarded headers ignored from untrusted peers; web binary graph excludes crawler/ingest/SQLite | Unit tests for config and client-IP logic; `httptest`; ephemeral PostgreSQL via `internal/pgtest`; a `go list -deps` test |
| W2 | Users store and CLI | Users schema DDL (approved first), store, password hashing with bounded concurrency, legacy verifiers (only those with vectors), `web user add/disable/enable/reset-password/set-password`, invites, pending-setup, setup/reset tokens | `pending_setup` cannot log in; disable/enable and invite suspension semantics hold, including the registration transaction race; hashing concurrency bounded | Table-driven tests, **PHP-generated vector fixtures** per supported format (generated independently and committed with the generating script and PHP version), concurrency tests with the race detector, ephemeral PostgreSQL |
| W3 | Authentication and sessions | Login/logout, sessions, CSRF, rate limits, rehash on login, revocation checks on every request | No auth state survives disable; session fixation covered; CSRF enforced on all POSTs incl. unauthenticated; generic errors and equal timing for unknown users | Handler tests, concurrency (revoke vs request), timing-shape test, header tests |
| W4 | Search and details | Persistence read additions, keyset search, details, file index and cache, decode semaphore, benchmark gate | Meets the §6.4 criteria **as confirmed by the owner** (or the owner picks an alternative); duplicates and odd paths round-trip; 404 vs 500 mapping | Unit + integration; benchmarks near 250,000 files, concurrent and repeated-navigation runs; EXPLAIN review on a synthetic disposable database |
| W5 | UI and styling | Templates, CSS, light/dark, responsive layouts, small JS, icons | Works without JS; 360 px layout; keyboard and contrast checks; CSP without inline scripts | Template tests, manual accessibility checklist (WCAG 2.1 AA), screenshot review by the owner |
| W6 | Account management | Password change, optional email change policy, tokens (create/list/revoke, legacy masking), invites, registration, SMTP reset, anti-enumeration | Reset responses identical for known/unknown email; reset revokes sessions per §3.5; registration is atomic with the owner-active check; tokens shown once | Handler and store tests, SMTP fake, concurrency race tests |
| W7 | API and RSS | `/api/v1` RSS, `/api/v2`, embedded OpenAPI (3.2.1 unless renderer evaluation forces a downgrade), `/api` docs, `/magnet` | RSS structure golden test; token redaction in logs verified; v1 spec lists only RSS; v2 validates against its OpenAPI document | Contract tests, OpenAPI validation, log-capture test |
| W8 | User migration | **Separate approved task**: `migrate users` | Specified separately; depends on the hash audit, schema approval and W2 verifiers | Specified separately |
| W9 | Integration, security, regression | Real-binary e2e (like `internal/e2e`), security checklist, load test of search, soak of the web service with realistic data, deployment notes (Traefik, log redaction) | No P0/P1 findings open; memory recommendation for the container | e2e with disposable PostgreSQL, scripted abuse cases (brute force, CSRF, enumeration, spoofed `X-Forwarded-For`), accessibility review |

The Task 06 e2e harness (`internal/e2e`) and `internal/pgtest` are reused; they run synthetic data only, and no step in this plan sends DHT traffic.

## 9. Documents and README

This task adds this document, `docs/specs/web-v1.md` (draft), and updates `docs/wip/README.md` (06A status, 07A row). `AGENTS.md` still lists only tasks 1–3 in its task order; updating it is not part of this task.

## 10. Open owner decisions

None of these is decided by the design; where a proposal appears it is labelled as such.

1. **Users schema:** name (`magnetico_users` proposed), DDL, preserving legacy user IDs.
2. **Dependencies:** which `golang.org/x/crypto` version (the current indirect pin is old), SMTP library vs stdlib, the API-documentation renderer.
3. **Hash audit:** owner runs the aggregate query in §3.3 on the legacy database and reports the result; this gates the migration task and the set of legacy verifiers.
4. **Username case sensitivity** and the collision policy for migrated users.
5. **Invite code storage:** plaintext (can be re-displayed, also for migrated ones) vs hashed (cannot).
6. **Session lifetimes:** idle and remember-me durations.
7. **Roles:** drop the legacy `roles` column or migrate it.
8. **RSS pagination:** keep the legacy offset (skips the newest 1,000 on `page=1`) or fix it.
9. **v2 RSS** query-string tokens; feed-scoped tokens.
10. **Email:** verification of addresses, and the email-change policy.
11. **API-token policy** on self-service password change (keep, with an opt-in revoke-all, is the proposal) and on admin reset (revoke is the proposal).
12. **Search semantics and timeout** (§6.1): all draft.
13. **File-tree approach** after the W4 benchmark, and the acceptance numbers in §6.4.
14. **Logging library** (`log/slog` proposed vs existing `log`).
15. **Documentation pages** at `/api`: require login or public.
16. **Single versus multiple web instances** (affects rate-limit state).
17. **Default tracker list** for magnet links (the legacy list contains hosts that may no longer exist; to be reviewed).
18. **Breached-password lookup** (not proposed; it would send hash prefixes off the host).
19. **Non-UTF-8 paths** in v2 JSON (representation rule).

## 11. What was and was not verified

- Verified by reading: all legacy controllers, entities, migrations, security configuration, RSS/search/file-tree code, and the hash-algorithm history in Git; the current CLI, options, persistence API and error types, codec decode API, the ingest HTTP server patterns and shutdown, the PostgreSQL and wire-format specs, and Task 06's results.
- **Not** verified: any behaviour of the running legacy application; the legacy hash distribution; what `PASSWORD_DEFAULT` was on the legacy runtime in 2020–2022; renderer support for OpenAPI 3.2.1; query plans or timings of any proposed query; memory of decoding a 250,000-file blob in the web process. These are explicit gates above.
- No code was built or run for this design other than the repository's own `go build` / `go vet` sanity check recorded in the commit report.
