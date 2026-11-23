# Task 07A — Web UI, authentication and legacy compatibility: design plan

**Status:** design only. Nothing here is implemented. **Revision 4** (account lifecycle correction: *disable suspends, explicit revocation or password reset destroys*; §3.5) follows revision 3, which incorporates the owner decisions given in the reviews of commits `17b4c2f0` (revision 2) and `15b76d20` (revision 3) (see §0); settled choices are marked **owner-approved**, recommendations are marked *proposal*, and what is still undecided is listed in §10. The companion draft contract is [`docs/specs/web-v1.md`](../specs/web-v1.md), which stays **DRAFT** pending the owner's final review.
**Branch:** `refactor/0.8.0-unify-and-new-storage`. `master` was read as a historical reference and was not modified.
**Not done, by design:** no web code, no schema change, no user migration, no new dependency, no database access, no DHT traffic.

Sources: the PHP/Symfony application on `master` (file paths below are `master:` paths), and the current Go tree (`opflags/`, `main.go`, `persistence/`, `codec/`, `internal/ingest/server/`). Statements about the legacy hash distribution in the production database are **not** known: nobody has queried it (see §3.3).

## 0. Owner decisions incorporated in revision 2

| # | Decision | Where |
|---|---|---|
| 1 | Schema is **`magnetico_web`** (accounts, auth, sessions, invitations, API tokens, RSS tokens, trackers, future web settings). **Historical user and invitation IDs are preserved** (identity columns, sequences advanced); no UUIDs | §3.10, spec §5 |
| 2 | **`pending_setup` removed.** Independent `active`, nullable `password_hash`, and email verification | §3.2, §3.5 |
| 3 | Two roles `user` / `admin` with `web user set-role`; no RBAC; legacy roles are not used to infer admins | §3.3, §3.10 |
| 4 | Invitation codes stored in **plaintext**; suspension and transaction rules unchanged | §3.3, §3.5 |
| 5 | Sessions: 12 h idle, 90 d remember; configurable via `MAGNETICO_WEB_*`; finite absolute expiry always | §3.6 |
| 6 | Email optional; verified-only recovery; strict-TLS SMTP; email changes need the current password; reset/admin-reset revokes sessions, **API tokens and RSS tokens** (email and page say so; revision 4); password change keeps tokens by default with explicit revoke options | §3.5, §3.11 |
| 7 | Search accepted as proposed; **60 s** configurable timeout (`MAGNETICO_WEB_SEARCH_TIMEOUT`); no short default; HTTP/proxy deadlines must not defeat it; friendly timeout error; no new indexes without measured, approved gain | §6 |
| 8 | v1 RSS preserved incl. query token (RSS tokens only); offset fix recommended as an intentional change; **RSS credentials are separate from API credentials** (revision 3); new `/api/v2/feeds/latest.rss?feed_token=` | §3.13, §5.2 |
| 9 | Trackers in `magnetico_web`, idempotent verbatim import of `master:config/public_trackers.json`; `/magnet` still public and never checks the torrent exists, but reads cached tracker settings | §5.5 |
| 10 | **No legacy UI redirects**; only `/api/v1/feed/rss/last` and `/magnet/{infohash}` are retained | §5.1 |
| 11 | `GET /robots.txt` = `Disallow: /` plus `X-Robots-Tag: noindex, nofollow` everywhere; private by default without baking that assumption into the architecture | §3.12 |
| 12 | `log/slog`; public `/api` documentation; embedded OpenAPI; vendored client-side renderer; no CDN; latest OpenAPI version the chosen renderer supports | §4.4, §5.4 |
| 13 | v2 non-UTF-8 paths: `path: null` + `path_base64` | §5.3 |
| 14 | File-tree budgets are **benchmark targets, not hard release gates**; W4 measures and reports, the owner selects the solution | §6.4 |
| 15 | Remaining prerequisites kept separate (hash audit — does not block W1; `x/crypto` version; DDL; renderer; migration; CI/OCI) | §10 |
| 17 | **Revision 4:** `disable` suspends (not revokes) API/RSS tokens and invitations, `enable` restores them (never sessions); password reset revokes sessions, API and RSS tokens; `web user revoke-tokens` for explicit mass revocation | §3.5, §3.13, §10 |
| 16 | **Revision 3:** separate `rss_tokens` / `api_tokens`; legacy tokens → `rss_tokens` only; case-sensitive usernames; one instance; session lifetimes 12 h / 168 h / 2160 h; tracker CLI; seed at explicit schema init only; budgets are benchmark targets, not gates; `AGENTS.md` OpenAPI rule added | §3.13, §10 |

*`AGENTS.md`:* the restriction to the two documents was lifted for `AGENTS.md` in revision 3; the OpenAPI-maintenance rule has been added there (§5.4). No other file outside the three documents changed.

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
| 6 | Email password reset | **Essential** (owner decision) | Keep, hardened; optional SMTP; only to a **verified** address; strict-TLS transport |
| 7 | API tokens (create, list, revoke) | **Redesign** | Two separate credential types: **API tokens** (new, Bearer, JSON API only, created explicitly) and **RSS tokens** (feeds only); legacy tokens become RSS tokens (§3.13) |
| 8 | RSS at `/api/v1/feed/rss/last` | **Essential** (known consumer) | Preserve URL and `api-token` auth for RSS tokens (legacy migrated and new); add `/api/v2/feeds/latest.rss?feed_token=` |
| 9 | Public short magnet redirect | **Essential** | Keep at `/magnet/{infohash}`; linked from the UI; trackers now from PostgreSQL (cached), no torrent-existence lookup |
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
| 22 | Plaintext tokens/codes at rest | Legacy not to retain | Tokens, reset/setup links and sessions hashed. **Invitation codes stay plaintext (owner-approved)** so users can re-copy links |
| 23 | `ROLE_*` `roles` column | Redesign (owner-approved) | Two roles `user`/`admin`; migrated accounts are `user`; operator promotes via `set-role`; Admin UI is a future release |
| 25 | Legacy UI URLs (`/auth/*`, `/profile/api/*`, …) | Not retained | No redirects (owner-approved) |
| 26 | Tracker list in `config/public_trackers.json` | Redesign | Rows in `magnetico_web.trackers`, verbatim seed |
| 24 | Font Awesome, PicoCSS, Redis, Sentry, Doctrine | Legacy not to retain | Not carried over |

## 3. Authentication and user model

### 3.1 Principles

- Everything except `/magnet/*`, `/login`, `/register/*`, password reset/setup and email-verification pages, health endpoints, `/robots.txt`, the public `/api` documentation, static assets and the RSS endpoint (token-authenticated) requires an authenticated session or API token.
- Only the Go web process authenticates. Sessions, tokens and CSRF state live in PostgreSQL (the `magnetico_web` schema; not created here), so the web service can restart or be replicated without logging users out. The web service never contacts the crawler or ingest.
- No account lockout (a lockout is a denial-of-service lever); throttling instead.

### 3.2 Account model and onboarding (admin-created users) — owner-approved revision

There is **no `pending_setup` state**. Three independent attributes:

- `active` — the account is administratively enabled.
- `password_hash` — `NULL` until an initial password is established; `NULL` prevents password authentication (no empty or default password).
- Email verification — separate, relevant only when an address exists (§3.11).

`magnetico web user add <username> [--email E] [--invites N]` creates an **enabled** account (role `user`) with `password_hash = NULL` and prints a single-use, expiring **password-setup link** (`/password/setup/{token}`, hashed token, ≈72 h by default). Rules:

- Completing setup sets the password hash, revokes any sessions/tokens (none exist for a new account), consumes the token — and **never writes `active`**. A disabled account cannot be re-enabled this way: the link is refused with the generic invalid-link message and is **not consumed** (it works after `enable`, unless it expired).
- Disabling and re-enabling never set, change or clear the password hash.
- Expired links are re-issued by `web user reset-password <username>`; each new link invalidates earlier ones. This works for accounts without an email address.
- Email verification is independent: setting a password does not verify an address and verifying an address does not set a password.
- The administrator never learns the password, except in the explicit rare `set-password` case (prompt or stdin, never argv, never printed).

Other commands: `set-role <username> <user|admin>`, `reset-password`, `set-password`, `disable`, `enable`, `revoke-tokens <username>` (permanently revokes all API and RSS tokens; security incidents; separate from `disable`), `invite add|revoke`, `list`. Output never contains secrets other than the one link being issued.

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
3. **Usernames are case-sensitive (owner-approved)**, as in the legacy application: exact spelling and identity are preserved, no case folding, no case-insensitive uniqueness.
4. **Legacy API tokens → `rss_tokens` only (owner-approved, supersedes the earlier plan).** See §3.13. They are never copied into `api_tokens`; no general API token is created by the migration.
5. **Invites.** Legacy invite codes are 32-hex plaintext and **stay plaintext (owner-approved)**, so users keep seeing and copying their unused links; migrated invitations remain available and follow the suspension rule of §3.5. Mitigations accepted with that choice: codes never logged, visible only to the owner, CSRF on invitation management, single-use consumption and explicit revocation enforced in the database.
6. **Not migrated:** sessions, remember-me cookies, reset tokens, reCAPTCHA state. All users sign in once after cutover.
7. **Roles (owner-approved).** Historical PHP roles were not meaningfully used for administration. All migrated accounts become ordinary `user`s; **no administrator is inferred** from legacy role data. The operator becomes the first administrator with `web user set-role <username> admin`. Registration through an invitation always creates a `user`.
8. **Identifiers.** Legacy numeric user IDs need not be preserved. Recommendation (§3.10): keep them, since the mapping is then the identity and trivially verifiable. The hash audit and the migrator are separate prerequisites and do **not** block W1.

### 3.4 Passwords

- New hashes: argon2id in PHC string format; parameters are fixed in the spec as a proposal (OWASP/RFC 9106 second recommended option) and benchmarked on the target host before release.
- New-password length: 8 to 1,024 bytes (bounded to avoid hashing DoS); login accepts up to the legacy 4,096.
- **Bounded global hashing concurrency.** A single process-wide semaphore limits concurrent hash/verify operations (default `max(1, min(GOMAXPROCS, 2))`), with a bounded wait. Argon2 at 64 MiB per operation means the cap also bounds memory. When the wait times out, the request fails with a generic error (HTTP 503 with `Retry-After`) and is counted.
- Unknown usernames still perform a dummy verification so response timing does not reveal account existence.
- Rehash on login when the algorithm or parameters are outdated, inside the same transaction as the `last_login_at` update.
- The `golang.org/x/crypto` module is currently an **old indirect** requirement (a 2022 pseudo-version in `go.mod`). Using argon2 or bcrypt means a direct dependency. Selecting a maintained version is part of a later dependency review; reusing the old version is not proposed.
- An optional breached-password check (HIBP-style) would leave the host; it is not proposed (§10).

### 3.5 Revocation and "active" semantics

Authentication middleware checks account state on **every** session or token request, in the same query that resolves the credential (session hash → user row → `active` and `role`). Removing session rows alone is not sufficient, because API tokens are not sessions.

| Event | Sessions | API tokens | RSS tokens | Invites owned by the user |
|---|---|---|---|---|
| `disable` | **Permanently revoked**; login refused | **Suspended, not revoked**: unusable while the owner is inactive (per-request owner-active check); rows, hashes, `revoked_at`, expiry unchanged | Same (migrated legacy RSS tokens included) | **Suspended, not revoked**: rows and codes unchanged; registration through them is refused while the owner is inactive |
| `enable` | None restored; the user logs in again | **Usable again** (same tokens) unless expired or explicitly revoked | **Usable again**, legacy ones and their unchanged v1 URLs included; no client reconfiguration | Usable again automatically, unless used, explicitly revoked or expired |
| Self-service password change | Other sessions revoked, the current one rotated | **Kept by default**; explicit "also revoke all API tokens" option (owner-approved) | **Kept by default**; separate explicit "also revoke all RSS tokens" option | Unchanged |
| Reset by email, admin `reset-password` / `set-password`, completion of a setup link | **All revoked** | **All revoked** (owner-approved); email and page say so | **All revoked (owner-approved, revision 4)**, legacy tokens included: recovery may signal compromise and RSS URLs carry credentials; email and page say so | Unchanged |
| Explicit token revocation (account UI per token; admin `revoke-tokens`) | Unchanged | **Permanently revoked** | **Permanently revoked** | Unchanged |
| Explicit invite revocation | n/a | n/a | n/a | Permanent (`revoked_at`); `enable` never undoes it |

API tokens and RSS tokens are never conflated: separate stores, separate revocation actions, separate account sections, separate tests. A password reset revokes both (owner-approved), so its notification warns that **both API and RSS credentials stop working and need replacement**; `disable` suspends both without revoking them.

Design rules:

- **Disable suspends access; explicit revocation or a password reset destroys credential validity (owner-approved, revision 4).** Disabling sets `active = false`, destroys sessions, and leaves API-token, RSS-token and invitation rows untouched (no `revoked_at`, expiry or hash change, and no epoch/generation counter). Enabling restores every token and invitation that is neither expired nor explicitly revoked, and never a session. Explicitly revoked or expired credentials are never restored. "Revoked" is a stored state (`revoked_at`), not a derived one; "disabled" is derived from `users.active` on each request. Disabling or enabling never touches the password hash.
- **Per-request check** for every API/RSS request, server-side and without an unbounded cache: the credential exists, is not expired, is not revoked, and its owner is active. A disable takes effect for requests authorized after its transaction commits; one already authorized may finish.
- Invites are different on purpose: disabling the owner **suspends** them. The invite rows stay untouched, so re-enabling restores them without any bookkeeping. This applies equally to migrated legacy invites.
- **Registration is a single transaction.** It locks the invite row `FOR UPDATE` and its owner's `users` row `FOR SHARE`, verifies that the invite is unused, not revoked, not expired, and that the owner is `active`, then creates the user and marks the invite used. `disable` takes `FOR UPDATE` on the same user row, so a registration cannot succeed after a disable transaction has committed, and a rejected registration never consumes the invite.
- The registration page shows one generic message ("This invitation is invalid or no longer available") for unused-but-suspended, used, revoked, expired and unknown codes. It never reveals that the owner is disabled.
- **In-flight requests:** a request already authorised may complete; the next request sees the new state. There is no authentication cache in the proposal. If a cache is ever introduced, its TTL becomes the documented revocation latency.
- Tests (milestones W2, W3, W6): disable refuses registration; enable makes the same invite usable; concurrent disable versus registration (run both orders many times, assert registration never commits after disable commits and the invite is not consumed on refusal); concurrent disable versus token-authenticated requests; migrated invite behaves identically. Token tests (spec §12.4b): active → disabled → enabled with the same API token, the same new RSS token and a migrated legacy RSS token on the unchanged v1 URL; explicitly revoked and expired tokens stay dead after enable; sessions stay invalid.

### 3.6 Sessions, cookies, CSRF

- Opaque 32-byte random session token; PostgreSQL stores only its SHA-256. A new session is issued at login (no fixation) and rotated on password change and privilege change.
- Production cookie: `__Host-magnetico_session`; `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`. The `__Host-` prefix and `Secure` are **never** weakened for local testing.
- Development on a loopback `http://` public URL uses an explicit dev mode with a different cookie name without the `__Host-` prefix and without `Secure`; the mode is rejected when the public URL is not a loopback `http://` URL.
- Lifetimes (owner-approved defaults, configurable): idle timeout **12 h** (`MAGNETICO_WEB_SESSION_IDLE_TIMEOUT`); remember-me **90 days absolute** from creation (`MAGNETICO_WEB_SESSION_REMEMBER_LIFETIME`, `2160h` — Go durations have no `d` unit). **Every session has a finite absolute expiry**: an ordinary session also gets `MAGNETICO_WEB_SESSION_ABSOLUTE_LIFETIME` (*proposal*: 168 h) so continuous activity cannot keep it alive indefinitely. Startup validation: positive durations, `idle ≥ 5m`, `idle ≤ absolute ≤ remember ≤ 8760h`; no "0 = unlimited". `last_seen` writes are throttled; an expired-session purge runs in the background.
- **CSRF covers every state-changing browser action**, including those that need no existing session: login, registration, forgot-password, password reset, password setup and account actions. Two layers: Go's `http.CrossOriginProtection` (Fetch metadata / `Origin` checks) and a synchronizer token in every form. Before a session exists the token is bound to a short-lived pre-session cookie. All state changes are `POST`; nothing changes state on `GET`.
- Logout is a `POST`.

### 3.7 Rate limiting and brute force

In-memory sliding windows per client IP and per normalised username (login), per IP and per email (reset requests), per IP (registration, token 401s), with bounded memory (LRU of keys) and generic error text. The initial deployment is **one web instance (owner-approved)**, so in-memory limiting is acceptable; coordinating limits across several instances is a future consideration, not a 0.8.0 blocker.

### 3.8 Proxy and HTTPS assumptions

- The web service listens on plain HTTP (same convention as ingest, `opflags/ingest.go` `Listen`) and is run behind Traefik, which terminates TLS.
- `--trusted-proxies` is a CIDR list. `X-Forwarded-For` (the only forwarding header supported; RFC 7239 `Forwarded` is not read) is honoured **only when the direct TCP peer is inside that set**; otherwise they are ignored and the peer address is the client. A direct client therefore cannot spoof its address to evade rate limits. With a trusted peer, the client IP is the right-most address in the `X-Forwarded-For` chain that is not itself a trusted proxy.
- A required `--public-url` determines every absolute URL (RSS, reset, setup and invite links). The `Host` header is never used for that.
- Strict response headers: CSP, `X-Content-Type-Options`, `Referrer-Policy`, `Cache-Control: private/no-store` on authenticated pages, `Permissions-Policy`.

### 3.9 Sensitive logging and errors

No passwords, session tokens, API tokens, RSS tokens, reset/setup/invite codes or full query strings are logged. The `api-token` / `feed_token` / `token` query values and `/register/{code}`, `/password/*/{token}` path segments are redacted in the access log. File paths and torrent names are not logged by default (matches `postgresql-persistence-v2.md` §8). Errors shown to users are generic with a request ID; details go to the log.

Both RSS endpoints, because they accept the credential in the URL, additionally get `Referrer-Policy: no-referrer` and `Cache-Control: private`. **Traefik access logs** record the query string by default; operators must drop or redact it (documented in the deployment notes). That is outside the application's control and must not be presented as solved.

### 3.10 Schema `magnetico_web` and identifier strategy (owner-approved name; strategy is a proposal)

`magnetico_web` holds users, sessions, invitations, API tokens, RSS tokens, one-time tokens (setup, reset, email verification), the public tracker list, a small `settings` table and a version table. It is created by an explicit, idempotent command, never by `web` at startup. Detailed DDL is a separate prerequisite; the logical tables are in the spec (§5).

Identifier strategy (**owner-approved**: historical numeric user and invitation IDs are preserved; the approved torrent/file identifiers are untouched):

- **Keep legacy `users.id` and `invites.id` verbatim** (`bigint` identity; `OVERRIDING SYSTEM VALUE` on import, each sequence advanced past the maximum with `setval`, foreign-key consistency verified). The old→new mapping is the identity, so verification is counts, foreign-key integrity and per-user invitation/token counts against the source, with no mapping table to carry or get wrong.
- `api_tokens` and `rss_tokens` get their own surrogate ids (the legacy primary key *is* the secret). For migrated legacy tokens the link to the source is verifiable: `rss_tokens.token_hash = sha256(legacy key)`.
- Sessions and one-time tokens are keyed by their hash; trackers get a new identity.
- **No UUID/ULID** without a concrete requirement: no identifier is public, there is no multi-writer case, and a type change would force a permanent mapping with no benefit.

### 3.11 Email, SMTP and password recovery (owner-approved)

- Email is **optional for every account**. A supplied address is *unverified* until its holder confirms a single-use link. Only a **verified** address is used for self-service recovery.
- **Changing an address:** the new address is verified first; the existing verified address stays in force until the replacement is confirmed (the pending address lives in the verification token, not in `users.email`). **Owner-approved:** the change requires the current password. *Proposal:* also notify the previous address when it completes.
- Administrator-generated reset links remain available for users without email.
- **SMTP:** authenticated submission over **encrypted, certificate-verified** transport only. Modes: strict STARTTLS and implicit TLS; **no plaintext fallback**, no `InsecureSkipVerify`, no `AUTH` or message content before TLS, not even to loopback. *Proposal:* standard library (`net`, `crypto/tls`, `net/smtp.NewClient` with explicit `StartTLS`); **not** `smtp.SendMail`, which falls back to plaintext. A library is justified only if a needed capability is missing, with owner approval. Forgot-password hands the message to a bounded in-process queue so latency cannot reveal whether an address exists.
- **Reset emails and the confirmation page state explicitly (owner-approved)** that existing API tokens are invalidated, that existing RSS tokens and RSS URLs stop working, and that the user must generate new credentials and update their torrent clients; the CLI output of `reset-password` says the same to the operator.
- Historical email addresses at migration remain a separate decision (they would enter as unverified unless decided otherwise).

### 3.12 Indexing policy (owner-approved)

Public `GET /robots.txt` returns `User-agent: *` / `Disallow: /`. Every response also carries `X-Robots-Tag: noindex, nofollow` (defence in depth; covers public routes such as `/magnet/*`, documentation and static assets). `robots.txt` is not access control: real content is protected by authentication. The policy is one isolated middleware plus one handler, so a later release can support public read-only access or open registration without re-architecting; those modes are **not designed or implemented now**.

### 3.13 Credentials: API tokens and RSS tokens (owner-approved, revision 3)

**Why two stores.** RSS URLs must carry their credential (torrent clients cannot send headers), while the JSON API must stay strictly Bearer-header. One credential type cannot be both without putting a full-power token in URLs, so there are two, **logically and physically separate**, in `magnetico_web`:

| | `api_tokens` | `rss_tokens` |
|---|---|---|
| Purpose | JSON REST API (`/api/v2/*`) | RSS feeds only |
| Format | `mgt_` + base64url(32 random bytes) (*proposal*) | `mgf_` + base64url(32 random bytes); migrated legacy 32-hex tokens have no prefix |
| Stored | SHA-256 only | SHA-256 only |
| Transport | `Authorization: Bearer` only | v2 feed: `?feed_token=`; v1 feed: legacy `api-token` query/header/cookie |
| Creation | Explicit, on the account page; **never** automatic at login or migration | Explicit, on the account page; many per user; legacy ones come from migration |
| Metadata | label, created, last used (throttled), optional expiry, `revoked_at` | same, plus `legacy` flag |

No role/scope/permission framework: two tables with their own creation, revocation, metadata and authentication code. **An RSS token never authenticates against the general API; an API token never authenticates against RSS.** Tests assert both directions.

**Legacy migration** (for the later migration task). Every existing PHP API token goes **only into `rss_tokens`**: owner kept; `sha256(legacy_token)` stored (the migrator reads the plaintext from the source database during the run and keeps no plaintext in the target); `legacy = true`; original `created_at` kept; no replacement tokens generated; **no `api_tokens` created**; values never shown in the account UI (masked entries). Existing `…/api/v1/feed/rss/last?api-token=<legacy-token>` URLs keep working unchanged, and the old token must **not** authenticate against `/api/v2/*`. Legacy RSS tokens work until explicitly revoked, revoked by a password reset, or until a future, separately approved removal; `disable` only suspends them (they work again after `enable`). This supersedes the earlier idea of migrating legacy tokens into a general token store.

**New RSS endpoint.** `GET /api/v2/feeds/latest.rss?feed_token=<rss-token>` works in header-less clients. It is the one documented exception to header-only authentication in v2 and accepts only the separate RSS credential. The parameter name is adjustable only for a concrete compatibility or design problem; none was identified. Supporting new RSS tokens at v1 is allowed because authentication there remains RSS-only (the spec makes it so).

**Account page.** Two sections: *RSS feeds* (create/revoke RSS tokens, metadata, masked legacy entries, a ready-to-copy v2 URL shown once at creation, a note that v1 RSS is deprecated and that existing URLs keep working) and *API access* (create/revoke API tokens, Bearer explanation). A user wanting API access must create a token explicitly after logging in.

**Security.** RSS URLs contain credentials: redaction of `api-token`/`feed_token` and cookie in logs, `Referrer-Policy: no-referrer`, `Cache-Control: private, no-store`, reverse-proxy log-redaction guidance, constant-time digest comparison, per-token and per-IP rate limiting, independent revocation checked on the next request, token check before any feed cache.

**Resolved (revision 4).** Migrated legacy RSS tokens survive a disable/enable cycle exactly like new RSS and API tokens (suspension, not revocation); only a password reset or explicit revocation destroys them.

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
- One hand-written CSS file with custom properties (light and dark themes via `prefers-color-scheme` plus a persisted override) and an inline SVG sprite instead of Font Awesome. **[W5, implemented]** The stylesheet is the owner-approved Demo 3.1 design trimmed to the implemented pages (≈ 26 KB).
- Vanilla JavaScript, first-party only. **[W5, supersedes the original “< 6 KB, in-place directory expansion falling back to a link”]** `app.js` (≈ 33 KB, unminified, no build step) provides the theme toggle, clipboard copy, `/` to focus search, selector auto-submit and the **file browser**, which is JSON-driven and requires JavaScript (no HTML tree fallback; see `web-v1.md` §6.4.11–6.4.12). A 0.5 KB blocking `theme.js` applies the stored theme before first paint. No JavaScript framework, no htmx, no CDN, no build step.
- Assets are embedded and served with content-hashed names and long-lived caching; CSP forbids inline scripts.
- The interactive API documentation page is the single place that needs a vendored renderer (§5.4).

**UI approach considered and rejected for now:** an SPA (React/Vue/Svelte), htmx, and Pico CSS. None is needed for six pages; each adds a toolchain, dependency or supply-chain surface. Re-evaluate only if a concrete interaction cannot be built with the above.

### 4.3 PostgreSQL

- A dedicated `pgxpool` for the web service, explicit `pool_max_conns` (default 10), health check, and per-request `context` deadlines. Existing `persistence.connectPool` uses an unconfigured pool; web should pass explicit settings.
- Startup runs the existing read-only `verifyStartup` (`pg_trgm`, schema objects, `schema_version`) and fails fast; an exact `magnetico_web` structure/version and privilege check (`web_version`) is added (W2) (fails fast if missing or unsupported). No automatic migration (postgresql-persistence-v2 §2).
- Recommended roles: a dedicated database role with `SELECT` on `magneticod.*` and DML on `magnetico_web` only. The web service never writes torrent data.
- **Persistence additions** (read-only, no schema change, signatures illustrative): get torrent by integer ID; get file list by ID; keyset `QueryTorrents`. Today `persistence` has `GetTorrent(infoHash)`, `GetFiles(infoHash)` and `QueryTorrents` with a `LIMIT` only (`persistence/interface.go`, `postgres.go:296-349`), no offset, no cursor, no by-ID lookup.
- **Search timeout: default 60 s** (`MAGNETICO_WEB_SEARCH_TIMEOUT`, Go duration, validated 1 s–15 m), enforced by context plus a statement timeout. A timeout produces a friendly "the search did not finish in time" response (v2: problem+json 503 with `Retry-After`) and never claims the query is invalid or too broad. Because a search can hold a connection for a minute, search concurrency has its own bounded semaphore so slow searches cannot starve login and other pages.
- **HTTP deadlines must not defeat the SQL timeout:** no global `WriteTimeout` and no short `TimeoutHandler`; per-request write deadlines via `http.ResponseController` (search routes: search timeout + 10 s; others: request timeout + 5 s; handler contexts are exactly the timeouts). Reverse-proxy deadlines (e.g. Traefik responding/idle timeouts) are outside the application: the deployment notes must require them above the search timeout. Whether Traefik defaults satisfy this was **not verified**.
- Typed error mapping: `ErrTorrentNotFound` → 404; `ErrIntegrityMissingBlob`/`ErrCorruptBlob` → 500 with an `ERROR` log carrying `torrent_id` (postgresql-persistence-v2 §5).

### 4.4 Configuration, logging, shutdown

- `WebOptions` modelled on `IngestOptions` (`opflags/ingest.go`): flags, then `MAGNETICO_WEB_*` environment variables, then defaults; unknown `MAGNETICO_WEB_*` variables are rejected; no config-file layer in the first version. `parseArgs` and `applyConfig` need a `web` case (today `opflags/config.go:183-195` returns `unknown command`).
- Secrets (database URL with password, SMTP password) are environment-only and never printed; the DSN is redacted with the existing helper pattern (`main.go` `redactDatabaseURL`).
- Logging: **`log/slog`** for the web service (owner-approved; the rest of the repository keeps stdlib `log`). Text or JSON, configurable level.
- Graceful shutdown follows the ingest pattern (`signal.NotifyContext`, bounded grace, in-flight tracking, `internal/ingest/server/service.go`).
- Health: `/healthz` (process) and `/readyz` (database reachable and schema verified); neither is authenticated nor reveals versions or counts.
- The CLI subcommands for operators (`web user …`) share the users store and open the database directly; they run against a stopped or running service.

### 4.4a Dependencies

Proposed new direct dependencies are limited to: an argon2/bcrypt implementation (`golang.org/x/crypto`, version to be selected), an SMTP library only if the standard library cannot provide a required capability without weakening TLS enforcement, and a vendored, pinned, small client-side documentation renderer for `/api` (static files, not a Go module; selection is a separate prerequisite). Nothing is added in Task 07A. Each needs owner approval. SMTP: see §3.11 (standard library with enforced TLS preferred).

## 5. API, RSS and magnet compatibility

### 5.1 Per-endpoint assessment

| Endpoint | Historical behaviour | Auth | Data | Cost to keep | Recommendation |
|---|---|---|---|---|---|
| `GET /api/v1/feed/rss/last` | §1.6 | **RSS token** (header/cookie/query) | `torrents` only (no blob decode) | Low: one indexed query per page; deterministic XML | **Preserve** URL, token transport, item identity and structure for migrated legacy and new RSS tokens. Page-1 offset fix recommended as an **intentional behaviour change** (§5.2) |
| `GET /api/v2/feeds/latest.rss?feed_token=` (new) | n/a | RSS token (query) | `torrents` only | Low | **Add**; the recommended URL for new RSS clients (§3.13) |
| `GET /magnet/{40hex}` | public 307, no DB | none | tracker list only (`magnetico_web.trackers`, cached); **never** the torrent index | Trivial | **Preserve** the path (owner-approved; Telegram-bot use). Linked from the details page and copy actions (§5.5) |
| `POST /api/v1/login` | mint token from password | public | `users`, `api_tokens` | Medium (login throttling, rehash, token policy) | **Remove**. Tokens are created on the account page |
| `POST /api/v1/logout` | delete presented token | token | `api_tokens` | Low | **Remove** (revocation lives on the account page) |
| `GET /api/v1/torrents` | JSON, OFFSET paging, `numberOfPages` always 1 | token | `torrents` | Medium: reproducing the envelope and fake counts honestly is not possible | **Remove**; replaced by v2. Re-implementable later if a consumer appears |
| `GET /api/v1/torrents/{id}` | JSON + `files[]` | token | `torrents` + blob decode | Medium | **Remove**; v2 pages the file list |

The owner stated that RSS readers are known consumers, that the v1 JSON API may be redesigned as v2 with v1 re-implementable on demand, that `api-token` in the query is kept only for v1 RSS (revision 3: RSS-only credentials, plus a new v2 feed URL with a separate RSS token), and (revision 2) that `/magnet/{infohash}` must keep working at its path. Legacy UI routes are **not** redirected (owner-approved): the homepage `/` is the entry point and the new UI may rename routes freely. Retained external routes are exactly `/api/v1/feed/rss/last` and `/magnet/{infohash}`; state-changing legacy GET routes are not served or redirected at all.

**Honesty about compatibility.** The new schema cannot reproduce `numberOfResults`/`numberOfPages` (no COUNT is run, and the legacy code returned constants), and `files` for very large torrents must be paged. A v1 JSON shim would therefore be an approximation, not a drop-in; that is one reason to remove it rather than promise it. If a consumer is discovered after cutover, an adapter can be written; its cost is small (≈ a day: envelope plus field mapping) but its fidelity limits must be documented.

### 5.2 RSS v1 details

- Item identity (`guid` = info hash, magnet `link`/`enclosure`) and the essential XML structure are preserved; `Content-Type: application/rss+xml`. A golden-file test compares structure (element set, guid, enclosure), not bytes. `comments` points at the new details route.
- Authentication stays `api-token` from header, cookie or **query** for this endpoint (owner-approved), so existing torrent-client URLs continue to work. It accepts **RSS tokens only** (legacy migrated and new `mgf_`); a general API token is rejected. Failed authentication returns the legacy JSON envelope with HTTP 401.
- Added protections: token redaction in logs, `Referrer-Policy: no-referrer`, `Cache-Control: private`, per-token rate limit, no token echo.
- The feed reads `torrents` only (`id DESC LIMIT 1000`), never blobs.
- **Page offset — recommended intentional change** (the owner does not consider it critical): the legacy `page=1` skips the newest 1,000 items. The recommendation is to correct it so `page=1` returns the newest 1,000. This is **not** an exact-compatibility guarantee. Release-note consequence: a client's first poll after cutover may show up to 1,000 newest items it never saw (GUIDs de-duplicate the rest).
- Cached in-process for a short TTL (legacy: 600 s), keyed on the page only; the token check runs on every request, **before** the cache (§3.5).
- The new account UI states that **v1 RSS is deprecated and may eventually be removed**, recommends the v2 URL, and does not force users to replace existing URLs.

**RSS URL decision (owner-approved, revision 3).** Torrent clients cannot send `Authorization` headers, so the options were compared (v1-only URL token, a v2 feed taking the general token in the query, a dedicated feed credential, Basic credentials in the URL). The owner chose the **dedicated feed credential**: separate `rss_tokens`, the new `GET /api/v2/feeds/latest.rss?feed_token=` endpoint recommended by the UI, v1 kept for migrated legacy tokens. The full-scope-token-in-URL risk of the other options is thereby avoided: a leaked RSS URL grants read access to the feed only. Details in §3.13.

### 5.3 API v2 (proposal)

Deliberately small. Bearer **API tokens** created explicitly on the account page (separate from RSS tokens, §3.13), no OAuth, **Bearer header only** (owner-approved) — with the single documented exception of the RSS feed endpoint, which accepts only an RSS token via `feed_token`.

- `GET /api/v2/torrents?q=&sort=&order=&limit=&cursor=` returns `{items, next_cursor}`; `GET /api/v2/torrents?info_hash=<40 hex>`; `GET /api/v2/torrents/{id}` (includes `magnet_uri`); `GET /api/v2/torrents/{id}/files?limit=&cursor=` over the flat canonical list; `GET /api/v2/feeds/latest.rss?feed_token=` (RSS-token authentication).
- Conventions: `snake_case` JSON, RFC 3339 timestamps, RFC 9457 `application/problem+json` errors, cursor pagination, `Cache-Control: private`.
- **File paths (owner-approved contract).** Paths are opaque bytes (wire-format-v1 §1). Valid UTF-8 → `path` is the original string. Invalid UTF-8 → `"path": null` plus `path_base64` carrying the **exact bytes** (standard base64 with padding). Validity is per whole path; invalid bytes are never turned into U+FFFD; storage and codec are unchanged. The HTML UI shows `\xNN` escapes in a visibly marked span (lossless). Tests cover valid, invalid, mixed encodings and duplicate paths, including across cursor boundaries.

### 5.4 OpenAPI documents and `/api`

- Specs are written by hand and **embedded in the binary as static assets**, one per prefix: `/api/v1/openapi.yaml`, `/api/v2/openapi.yaml`. `/api` and the version-specific documentation pages are **public** (owner-approved; they also carry `noindex`, §3.12).
- A **lightweight client-side renderer**, vendored and pinned, loads the relevant specification in the browser. The server has no dynamic documentation service. **No CDN, no runtime third-party requests**, no live tokens, credentials or user-specific data in the pages.
- **OpenAPI version:** the latest version supported by the chosen renderer. A small, maintainable renderer matters more than the newest minor version, and no heavyweight dependency is added merely to reach 3.2.1. (This supersedes the earlier "target 3.2.1" wording.) The renderer evaluation — size, licence, maintenance, CSP compatibility without inline script/`eval`, supported versions, no external requests, whether "try it out" can avoid persisting credentials — is a separate prerequisite; nothing is claimed here about specific renderers.
- The **v1 specification contains only endpoints the new application implements**: the RSS endpoint with its `api-token` header/cookie/query transport (RSS tokens only) and the 401 envelope; the prefix is marked deprecated. Removed endpoints do not appear.
- **Maintenance rule:** any API change updates the embedded document in the same change, enforced by a route-versus-spec test; the rule is now in `AGENTS.md` (owner-approved, revision 3): any change to a supported API endpoint, its authentication, parameters, response schema, error behaviour or other documented contract MUST update the corresponding embedded OpenAPI specification in the same change (v1 and v2, including the RSS authentication contract), and tests keep implemented routes and OpenAPI descriptions consistent where feasible.

### 5.5 Magnet short link and trackers in PostgreSQL

- `GET /magnet/{infohash}` stays public and at this path. It must **not** query whether the torrent exists in the index. It does read tracker settings, loaded from PostgreSQL and cached (the earlier "no database lookup at all" wording is withdrawn).
- Table `magnetico_web.trackers`: `id`, `url` (unique), `position`, `created_at`, `updated_at`; order is `position, id`. Allows future additions, edits, removals and reordering; leaves room for the future Admin UI (not implemented now). A tiny `settings` table holds the seed marker.
- **Initial import:** `master:config/public_trackers.json` (12 URLs), **verbatim and in order**, embedded as seed data (hash recorded). It runs inside the explicit web-schema initialization (a separate data migration is an acceptable alternative), in **one transaction under an advisory lock**: insert only if the table is empty **and** the `trackers.seed` marker is absent, write the marker in the same transaction. Re-running does nothing, a later operator edit is never undone, and a non-empty unmarked table is left alone and reported. **Nothing is silently filtered or replaced**: some listed hosts are probably defunct (not verified), and are kept for the owner to prune deliberately.
- **Caching (proposal):** an immutable in-memory snapshot loaded at startup (startup fails if it cannot be loaded), lazily refreshed when older than `MAGNETICO_WEB_TRACKER_CACHE_TTL` (default 60 s) by a single-flight background refresh. If refresh fails, the last good snapshot continues to be served and the failure is logged and counted; it is never replaced by an empty list. Local changes invalidate it immediately; other instances see changes within the TTL. No `LISTEN/NOTIFY`, Redis or other infrastructure. The same snapshot feeds magnet URIs in the UI, RSS and v2.
- Interim administration (before the Admin UI): SQL, or optional CLI `web tracker list|add|remove|move` (proposal, optional).

## 6. Search, listing, file browsing

### 6.1 Search semantics (owner-approved)

| Aspect | Legacy | Proposed |
|---|---|---|
| Word minimum | 3 characters, `mb_strlen` | 3 **Unicode characters**, counted as code points (runes) of the user's text without normalisation; this matches `mb_strlen` for UTF-8 input. Grapheme-cluster differences are not addressed |
| Matching | `name ILIKE '%w%'`, AND | Same, case-insensitive, AND |
| Case folding | PHP `strtolower` (ASCII) then `ILIKE` | PostgreSQL `ILIKE` only (locale-dependent for non-ASCII; to be documented and tested) |
| Wildcards | `%` and `_` typed by the user are **wildcards** | Escaped: user text is a **literal** substring (reuses `escapeLikePattern`, `persistence/postgres.go:353`). Behaviour change, documented |
| Empty query | every torrent, no defined order | latest torrents |
| 40-hex query | name search | exact info-hash lookup (addition) |
| AND of usable words | AND | AND (usable = ≥ 3 code points) |
| Timeout | none | **60 s default**, `MAGNETICO_WEB_SEARCH_TIMEOUT` (duration, validated 1 s–15 m); friendly "search timed out" response, never "invalid" or "too broad" |

The owner accepted this search architecture (3-code-point minimum, AND, case-insensitive substring, literal `%`/`_`, 40-hex exact lookup, empty query = newest, deterministic order, keyset pagination, no `COUNT(*)`). **The default timeout is deliberately long**: sorting very large result sets can be expensive, and the owner does not want a short default. Consequences handled in the architecture (§4.3): search concurrency is bounded separately so a handful of slow searches cannot exhaust the pool, and HTTP write deadlines and proxy timeouts must not cut the SQL timeout short.

The legacy PHP application avoided `COUNT()` with a pageless paginator to save database load. That property is **kept**: `LIMIT n+1` tells whether a next page exists and no total is ever shown.

### 6.2 Ordering and pagination

- Sorts: `discovered` (default, descending), `size`, `name` (only with a query). Each sort has an `id` tie-breaker so keyset cursors are stable.
- **Keyset pagination** (`WHERE (sort_col, id) < ($1, $2) … LIMIT n+1`), opaque cursor in the URL, no `COUNT(*)`, no `OFFSET`. "Previous" is implemented by a reversed query. Page sizes 20/50/100.
- Existing indexes (`postgresql-persistence-v2.md` §2): `idx_torrents_total_size`, `idx_torrents_discovered_on`, `idx_torrents_name_gin_trgm`. A keyset on `(discovered_on, id)` can use the single-column index but must still sort ties; whether a composite index is required is a **performance question for validation (§8)**, not a schema change proposed here. **Index policy (owner-approved):** no new index unless query-plan and load measurements prove a real gain *and* the owner approves it, to save storage; the PHP version did not have them.

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

**Numeric budgets — accepted by the owner as benchmark targets, not hard release gates (nothing has been measured).** To be benchmarked in W4 on **disposable synthetic data** generated by the tests, on a stated reference host (*proposal*: 2 vCPU, 1 GiB container), single process, idle database:

| Metric | Provisional budget |
|---|---|
| Cold decode + index build, ≈ 250,000 files (≈ 64 MiB uncompressed) | p95 ≤ **2 s**; bytes allocated ≤ **3×** the uncompressed list |
| Cached directory navigation (≤ 500 entries), server time | p95 ≤ **50 ms**, p99 ≤ **150 ms**; **0** decodes |
| 100 consecutive navigations in one large torrent | exactly **1** decode, 99 cache hits |
| 8 concurrent requests (mixed cold/cached, large torrents) | no errors; p95 ≤ **5 s** |
| 32 concurrent requests | no OOM/crash; successful p95 ≤ **15 s**; excess gets a bounded-wait (≤ **5 s**) `503` "busy", not unbounded memory growth |
| Peak process RSS in the 32-concurrent run | ≤ **640 MiB** (headroom in a 1 GiB container); decode working set ≤ **256 MiB** via the weighted semaphore; cache ≤ **128 MiB** |
| Allocations per cached navigation | ≤ **10,000** allocations and ≤ **1 MiB** |

The web service needs its own `GOMEMLIMIT` recommendation (ingest's 256 MiB is not transferable). W4 measures cold decode and index construction, cached navigation, 8/32 concurrent requests, peak memory and allocations. If a target is missed, W4 **reports the measurements and identifies the bottleneck** — it does not automatically redesign the codec or the persistence schema. The owner selects the final solution from the measurements and practical resource needs; any schema or codec change requires **separate owner approval**.

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

`⧉` copies the magnet URI; column headers are links with `aria-sort`; the name opens the details page. No total count is shown (none is computed); the empty state and the "search timed out" state have their own messages (the latter says the search did not finish in the configured time and may be retried).

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

**[W5, supersedes the mock-up above]** The file list is not part of the details HTML. Metadata renders at once; the Contents card then loads asynchronously in one of two backend-selected modes — a complete tree held in the browser (folders collapsed, local filter, chunked rendering) or directory-by-directory browsing with breadcrumbs and pagination — over small session-authenticated JSON endpoints with positional directory paths (`web-v1.md` §6.4.7–6.4.13; the approved visual design is Demo 3.1, see `07F-web-ui-report.md`). The old `?path=…` links and the HTML directory pages no longer exist. Aggregates (count, size) come from the index (§6.4). Duplicates are visibly marked, odd paths shown with a marker, and invalid UTF-8 bytes shown as lossless `\xNN` escapes in a marked span (never U+FFFD); a corrupt blob yields a clear error page, never a partial list.

### 7.6 Account

```
┌─ Account — alice ────────────────────────────────────────┐
│ Profile   username alice · role user                      │
│ Email     a@example.org  ✔ verified          [Change]     │
│           (optional; verified address enables reset)      │
│ Password  ••••••••                           [Change]     │
│           [ ] also revoke all my API tokens               │
│           [ ] also revoke all my RSS tokens               │
│                                                            │
│ RSS feeds                                  [ New RSS token ]│
│ ┌────────────┬───────────────┬──────────┬──────────────┐ │
│ │ Label      │ Created       │ Last used│              │ │
│ │ my-client  │ 2026-11-22    │ today    │ [Revoke]     │ │
│ │ legacy ····│ 2019-03-04    │ —        │ [Revoke]     │ │
│ └────────────┴───────────────┴──────────┴──────────────┘ │
│ A new RSS token is shown once with a ready-to-copy URL:   │
│  https://…/api/v2/feeds/latest.rss?feed_token=mgf_…       │
│ Legacy tokens are masked; existing v1 URLs keep working.  │
│ ⚠ The v1 feed (/api/v1/feed/rss/last) is deprecated and   │
│   may be removed in a future release.                     │
│                                                            │
│ API access                                  [ New API token]│
│ Bearer-header authentication for the JSON API (/api/v2).  │
│ Tokens are shown once; none is created automatically.     │
│ ┌────────────┬───────────────┬──────────┬──────────────┐ │
│ │ ci-script  │ 2026-11-22    │ today    │ [Revoke]     │ │
│ └────────────┴───────────────┴──────────┴──────────────┘ │
│                                                            │
│ Invitations   3 unused                                      │
│  https://…/register/9f3c…  [Copy] [Revoke]                 │
│  https://…/register/a71e…  [Copy] [Revoke]                 │
│ Used: bob (2026-10-02)                                      │
└────────────────────────────────────────────────────────────┘
```

The email field is optional. An unverified address shows a "send verification" action when SMTP is configured; it is never used for recovery until verified. A user without a verified address cannot self-serve a reset and needs the administrator. Invitation links are plaintext by owner decision, visible only to their owner; copy, revoke, API/RSS token creation/revocation and password change are POST forms with CSRF. The password-reset page states that completing a reset revokes all API tokens and all RSS tokens (RSS URLs stop working; new credentials must be generated and torrent clients updated), naming each type.

## 8. Implementation milestones

Each milestone ends in a reviewable change set and a stop. No milestone starts without owner approval. Database schema steps need the owner's approval of the DDL first. **W1 does not depend on the legacy hash audit, the `x/crypto` selection, the DDL, the renderer selection or the migration** (it needs no password hashing and no `magnetico_web` tables beyond the startup version check).

| # | Milestone | Scope | Acceptance criteria | Test strategy |
|---|---|---|---|---|
| W1 | Foundation | `WebOptions` with the §2.3 variables and validation (durations, session ordering, search timeout), `web` dispatch, `log/slog`, server with per-request deadlines (no global `WriteTimeout`), middleware chain, trusted-proxy handling, `/healthz`/`/readyz`, `/robots.txt` and `X-Robots-Tag` middleware, embedded static assets, startup verification, graceful shutdown, import-boundary test | Starts only with a verified schema; refuses bad config (each invalid duration/ordering rejected); graceful stop within the bound; forwarded headers ignored from untrusted peers; `robots.txt` body exact and `X-Robots-Tag` on every response class; the dependency graph of `./internal/web/...` (not of the unified executable, whose `main` package links the crawler and ingest by design) excludes `internal/crawler`, `internal/crawlqueue`, `internal/ingest`, `modernc.org/sqlite` (and the DHT stack) | Unit tests for config and client-IP logic; `httptest`; ephemeral PostgreSQL via `internal/pgtest`; a `go list -deps` test |
| W2 | Web schema, store and CLI | `magnetico_web` DDL (approved; realized in `internal/web/webschema`, version table `web_version`, created by `magnetico migrate web-schema-init`) incl. users (`active`, nullable `password_hash`, `role`, email verification), invites (plaintext), tokens, one-time tokens, trackers + idempotent seed; store; hashing with bounded concurrency; legacy verifiers (only those with vectors); `web user add/set-role/disable/enable/reset-password/set-password/list`, `invite add/revoke`, **`web tracker list/add/remove/move`** (owner-approved), `api_tokens` and `rss_tokens` stores as separate tables, tracker seed in the explicit schema-initialization command only | NULL hash cannot log in; completing a setup/reset link never writes `active` (disabled: refused, link not consumed); disable/enable never touch the hash; expired setup links re-issuable; invite suspension and the registration race hold; seed idempotent and non-clobbering; hashing concurrency bounded | Table-driven tests, **PHP-generated vector fixtures** per supported format, concurrency tests with the race detector, ephemeral PostgreSQL |
| W3 | Authentication and sessions | Login/logout, sessions with configurable idle/remember/absolute lifetimes, CSRF, rate limits, rehash on login, owner-active/expiry/revoked checks on every request | Sessions do not survive disable; API/RSS tokens fail while disabled and work again after enable;  every session row has a finite expiry; fixation covered; CSRF on all POSTs incl. unauthenticated; generic errors and equal timing for unknown/NULL-hash/inactive accounts | Handler tests, expiry tests with a fake clock, concurrency (disable/revoke vs request), timing-shape test, header tests |
| W4 | Search and details | Persistence read additions, keyset search with the 60 s configurable timeout and separate search concurrency, details, file index/cache, decode semaphore, **benchmark measurement** against the §6.4 targets (report, not gate), invalid-UTF-8 path display | Measurements and bottleneck analysis reported against the targets (no automatic codec/schema redesign; the owner picks the solution); search timeout friendly and not cut by HTTP deadlines; duplicates, invalid and mixed paths round-trip; 404 vs 500 mapping; no new indexes unless proven and approved | Unit + integration; benchmarks near 250,000 files at 1, 8 and 32 concurrency with RSS and allocation measurement; slow-query timeout test; EXPLAIN review on a synthetic disposable database |
| W5 | UI and styling | Templates, CSS, light/dark, responsive layouts, small JS, icons. **Implemented per the approved Demo 3.1** (`07F-web-ui-report.md`): the JSON file browser on the details page (tree and directory modes, positional paths, complexity limits, shared `fileview` service) replaces the HTML directory browser | Search, details and sign-in work without JS (the file browser requires it, an approved exception); 360 px layout; keyboard and contrast checks; CSP without inline scripts | Template tests, source-invariant tests, in-page JS self-tests, contrast computation, manual accessibility checklist (WCAG 2.1 AA) and screenshot review by the owner (pending) |
| W6 | Account management and email | Password change (keep tokens by default, revoke-all option), email add/change/verify, SMTP with enforced TLS and a bounded send queue, forgot/reset (verified address only), **separate API-token and RSS-token management** (create/list/revoke; masked legacy RSS entries; ready-to-copy v2 RSS URL shown once; no API token auto-created), plaintext invitation list/copy/revoke, registration | Reset responses identical for known/unknown/unverified/disabled; reset and admin reset revoke sessions, **API tokens and RSS tokens** and the email and page name each revoked type; `web user revoke-tokens`; email change requires the current password; old verified email stays until the new one is confirmed; STARTTLS/implicit-TLS enforced, no plaintext fallback; registration atomic with the owner-active check; other users' invitations never visible | Handler and store tests, **fake SMTP server** (no-STARTTLS, bad certificate, header injection), concurrency race tests |
| W7 | API, RSS, docs, magnet | `/api/v1` RSS (RSS-token `api-token` query/header/cookie kept, item identity, page-1 fix as documented change), **`/api/v2/feeds/latest.rss?feed_token=`**, `/api/v2` incl. `path`/`path_base64` and the **file-view parity endpoints** (`/torrents/{id}/files/tree`, `/torrents/{id}/files`, `full_tree_available`; contract in `web-v1.md` §7.5–7.6, implemented by wiring the existing `fileview` service behind bearer authentication), embedded OpenAPI at the latest version the chosen renderer supports, public `/api` docs with vendored renderer, `/magnet` with cached DB trackers, account RSS guidance (recommends the v2 URL, explains v1 deprecation), route-vs-OpenAPI consistency tests (the `AGENTS.md` rule itself is already in place) | RSS golden test; **credential-separation tests (legacy/new RSS token accepted on feeds and rejected by `/api/v2/*`; API token rejected by feeds)**; token redaction verified; v1 spec lists only implemented endpoints; v2 validates against its spec; route-vs-spec test; no external requests from docs pages; `/magnet` issues no torrent query and survives tracker-refresh failure | Contract tests, OpenAPI validation, log-capture test, tracker-cache tests |
| W8 | User migration | **Separate approved task**: `migrate users` (identity-preserving, case-sensitive usernames kept verbatim, handling of historical emails, legacy invitation import, **legacy tokens → `rss_tokens` only**: `sha256`, `legacy = true`, original timestamps, zero `api_tokens`) | Specified separately; depends on the hash audit, DDL approval and W2 verifiers | Specified separately |
| W9 | Integration, security, regression | Real-binary e2e (like `internal/e2e`), security checklist, load test of search, soak of the web service with realistic data, deployment notes (Traefik log redaction, proxy timeouts above the search timeout) | No P0/P1 findings open; memory recommendation for the container | e2e with disposable PostgreSQL, scripted abuse cases (brute force, CSRF, enumeration, spoofed `X-Forwarded-For`), accessibility review |

**Status (task 07G, 2026-11-23).** W6 and W7 were implemented together as one coordinated assignment (stages S1–S5): [`07G-web-account-api-report.md`](07G-web-account-api-report.md). The acceptance criteria above are covered by the tests listed in that report; the implementation record and the deliberate deviations (no self-service invitation creation, Swagger UI with GET-only "Try it out", OpenAPI 3.2.0, token caps and creation limits, mail queue sizing and delivery rules) are in `web-v1.md` §14. Not done and not claimed: any real-browser check of the new pages and of the vendored documentation renderer; W8 and W9 are not started.

The Task 06 e2e harness (`internal/e2e`) and `internal/pgtest` are reused; they run synthetic data only, and no step in this plan sends DHT traffic.

## 9. Documents and README

This task adds this document, `docs/specs/web-v1.md` (draft) and updates `docs/wip/README.md` (06A status, 07A row). Revision 3 changes this document, the spec and `AGENTS.md` (OpenAPI-maintenance rule).

## 10. Decisions

### 10.1 Owner-approved

*Revision 2:* see §0 (schema `magnetico_web`; no `pending_setup`; roles; plaintext invitation codes; configurable sessions; optional/verified email and strict-TLS SMTP; API-token revocation on reset; search architecture and the 60 s timeout; no new indexes without approval; RSS v1 preserved; trackers in PostgreSQL; no legacy redirects; `robots.txt`/`X-Robots-Tag`; `slog`; public embedded API docs with a client-side renderer; the `path`/`path_base64` contract).

*Revision 4:* **disable suspends rather than revokes** API tokens, RSS tokens and invitations; enable restores them, never sessions; password reset revokes sessions, API and RSS tokens with explicit notices; admin `revoke-tokens`.

*Revision 3:* **separate RSS and API credentials** (§3.13) with legacy tokens migrated only to `rss_tokens`, the new v2 RSS endpoint and account sections; **case-sensitive usernames**; **one web instance** initially; sessions 12 h / 168 h / 2160 h; **current password required for email changes**; **tracker CLI**; **tracker seed only during explicit schema initialization**, never at service start, never overwriting later edits; **preserved legacy user and invitation IDs**; file-tree budgets as targets, not gates; the `AGENTS.md` OpenAPI rule.

### 10.2 Still open

1. **File-tree final solution**, chosen by the owner after the W4 measurements.
2. **Breached-password lookup** (not proposed; it would send hash prefixes off the host).

Minor proposals, not blockers: a notice to the previous address after an email change, the default link lifetimes, the standard-library SMTP implementation.

### 10.3 Remaining implementation prerequisites (kept separate from the design)

- Legacy password-hash distribution audit (the owner runs the §3.3 aggregate query) — **does not block W1**.
- Selection of a maintained `golang.org/x/crypto` version.
- Detailed `magnetico_web` DDL.
- Selection of the API-documentation renderer and the OpenAPI version it supports.
- User-migration implementation (including historical email handling).
- CI/OCI images and release-candidate testing.

No live DHT activity is permitted on the development VM at any stage.

## 11. What was and was not verified

- Verified by reading: all legacy controllers, entities, migrations, security configuration, RSS/search/file-tree code, and the hash-algorithm history in Git; the current CLI, options, persistence API and error types, codec decode API, the ingest HTTP server patterns and shutdown, the PostgreSQL and wire-format specs, and Task 06's results.
- **Not** verified: any behaviour of the running legacy application; the legacy hash distribution; what `PASSWORD_DEFAULT` was on the legacy runtime in 2020–2022; renderer support for any OpenAPI version; whether Traefik's default response/idle timeouts exceed a 60 s search; which hosts in the historical tracker list are still alive; query plans or timings of any proposed query; memory of decoding a 250,000-file blob in the web process. These are explicit gates above.
- No code was built or run for this design other than the repository's own `go build` / `go vet` sanity check recorded in the commit report.
