# Task 07D / W3 — HTTP authentication, sessions, CSRF, rate limiting: implementation plan

> **S0 review applied (owner).** D1–D5 approved, and R1, R3 and R4 approved. Corrections:
> 1. **Exact root.** The interim homepage is `GET /{$}`, not `GET /`, because `GET /` is a ServeMux subtree. Unknown paths, `/torrents` before W4 and legacy UI routes all stay 404. Routing tests cover anonymous and authenticated callers.
> 2. **Purger lifecycle.** The purger is owned by `Server.Run`, not by the `baseCtx` side effect (§6.4 revised below):
>    - It starts only after verification, the startup hooks and `Listen` all succeed.
>    - It has its own `purgeCtx`/`purgeDone`.
>    - Every exit path stops it and waits for it before `backend.Close()`: listen failure (it never started), an unexpected `Serve` failure, a normal SIGTERM and a forced shutdown.
>    - Shutdown order: `srv.Shutdown`, and on grace expiry the forced cancel/close plus the existing in-flight wait; then purge cancel plus a bounded wait (the purger's own 30 s per-run context is shortened to respect `closeWait`); then `backend.Close` with the existing bound.
>    - A purger that does not stop in time sets `incomplete` (keeping `ErrShutdownIncomplete`, exit 3).
>    - No generic job framework: one `Background func(ctx)` field in `server.Config`.
> 3. **Test expectations.**
>    - Generic-failure tests compare status, the generic message, structure, cookie behaviour and verification count. Full bodies are compared only with identical username input and the same pre-session state.
>    - "No state on GET" means no application-level change. Throttled `sessions.last_seen_at` bookkeeping and pre-session cookie issuance are allowed.
> 4. **D1 approved:** if the second limiter acquisition (`Rehash`) returns `ErrBusy`, login succeeds without rehash. This is documented as an exception to §4.3.4. Verification is never skipped and other failures are never suppressed. **D2 approved:** documented as a stateless cookie-bound construction, not a server-stored synchronizer token. **D3, D4 approved**: D4 adds tests for external, `//`, `/\`, `%2F%2F`, `%5C` and similar bypasses. **D5 approved.**
> 5. **Proceed with steps 1–3 only and stop at S1.**

**Base:** `49daf675` on `refactor/0.8.0-unify-and-new-storage` (W2 complete, awaiting final review).
**Inputs:** `docs/specs/web-v1.md` rev. 5 (§§2–4, 6, 10, 12), `docs/wip/07A-web-design-plan.md` rev. 4 (§3.6–3.9, §4.1, §7.2, W3 row), `07B` and `07C` reports, and the W1/W2 code.
**Deliverable:** once this plan is approved, it goes verbatim into `docs/wip/07D-web-auth-session-plan.md`, with an index row in `docs/wip/README.md`. Nothing is implemented, installed or committed before then.

## Context

W1 built the HTTP foundation: middleware chain, cross-origin protection, client-IP resolution, deadlines, renderer. W2 built the schema, the account and credential stores, and password hashing with the shared limiter. Neither can sign a user in. W3 adds:

- browser login and logout;
- database-backed sessions;
- the second CSRF layer (synchronizer tokens);
- login rate limiting;
- rehash-on-login.

The aim is that every later browser milestone (W4 pages, W6 account flows) inherits authentication and CSRF structurally rather than remembering them per handler.

---

## 1. Scope and non-goals

**In scope (W3):**

- **Routes:** `GET /login`, `POST /login` and `POST /logout`.
- **Sessions:**
  - session creation, lookup, throttled activity update, logout deletion and background purge;
  - cookie issuance, validation, expiry and clearing in production and dev mode;
  - fixation-safe session creation.
- **Authentication enforcement** for private UI routes:
  - `303 → /login` for unauthenticated session routes;
  - `503` when the database is unavailable.
- **CSRF:**
  - an automatic per-route synchronizer-token check for every non-safe method, using session-bound tokens on private routes and tokens tied to a pre-session cookie on public forms;
  - the existing stdlib cross-origin layer stays.
- **Login:**
  - `password.Default.Prepare` before the listener opens;
  - dummy verification for unknown, inactive, NULL-hash and unsupported-hash accounts;
  - `ErrBusy` → 503 with `Retry-After`;
  - rehash-on-login with optimistic concurrency;
  - `last_login_at` recorded in the same transaction that creates the session.
- **Rate limiting:**
  - a bounded in-process package;
  - wired to login per client IP and per normalised username;
  - reusable by W6 and W7.
- **Minimal templates:** a login page and an interim signed-in page at `/` with a logout button (§6).
- **Small additions to W1 code:** route access classes, the user ID in the access log, the session settings passed into `server.Config`, and a `Pool()` accessor on `webdb.Backend`.
- **Small additions to W2 code:**
  - `password.Service.Rehash`, a new-hash path without the new-password length policy;
  - session and login SQL in `accounts`;
  - an optional expired one-time-link purge (decision D3).

**Non-goals:**

| Milestone | Not built in W3 |
|---|---|
| W4 | `/torrents`, details, search |
| W5 | Real styling and UX |
| W6 | Password change and session rotation on it, setup/reset/forgot handlers, registration, email, token management UI |
| W7 | API and RSS HTTP endpoints, token-authenticated routes, token `last_used_at` throttling, the tracker cache |
| W8, W9 | Migration; production integration |

W3 adds no new configuration variables, no new dependencies and no schema change.

## 2. Findings from the existing implementation

1. **Middleware chain** (`internal/web/server/server.go:139`): track → request ID → access log → recoverer → security headers → client IP → `http.CrossOriginProtection` (public origin trusted) → `limitBody` → mux. This is the approved spec §3.1 order with two documented deviations (the log wraps the recoverer; `limitBody`). W3 keeps it. Authentication becomes a **per-route wrapper inside the mux** (§6.1), so it runs after cross-origin protection as §3.1 requires, without a database lookup for `/healthz`, `/static/*` and similar routes.
2. **`Router.Handle(pattern, Kind, h)`** is "the single place new milestones add handlers". W3 extends it with an access class, so authentication and CSRF cannot be forgotten.
3. **`securityHeaders`** already sets `Cache-Control: no-store`, `Referrer-Policy: same-origin`, a CSP with `form-action 'self'`, and HSTS on https. That satisfies §3.4 for authenticated pages, so no change is needed.
4. **The W1 cross-origin comment** says the synchronizer token is "added with authentication in W3", which confirms the scope.
5. **`WebOptions`** (`opflags/web.go`):
   - already validates `SessionIdleTimeout`, `SessionAbsoluteLifetime` and `SessionRememberLifetime` per §4.4.4, and derives `DevMode`;
   - refuses dev mode with a non-loopback listen address.
   - **Gap:** none of these reach `server.Config` today (`main.go:317`). W3 passes them through.
6. **`accounts`:**
   - It has no session SQL. Its package doc says "creation and lookup are W3".
   - `Disable` and `resetRevocationsTx` already delete sessions under the user lock.
   - Lock order is invites → users → dependents, and expiry inside transactions uses `statement_timestamp()`.
   - W3 session and login SQL follow the same rules.
7. **`password.Service`:**
   - `Verify` returns `ErrUnsupportedHash` **without hashing**, which is a timing oracle if the login path stops there; W3 must dummy-verify in that case.
   - `Hash` enforces the 8–1,024-byte new-password policy, but legacy passwords may be 1–7 or 1,025–4,096 bytes, so rehash needs a separate entry point.
   - `NeedsRehash` is the exact `ClassLegacy` check.
8. **Schema (v1, fixed):** `sessions(token_hash PK, user_id, created_at, last_seen_at, expires_at NOT NULL, remember)` with `CHECK expires_at > created_at` and no CSRF column, so CSRF tokens must be derived (§4.3). The runtime role has `SELECT/INSERT/UPDATE/DELETE` on `sessions` and `one_time_tokens`, and `UPDATE` on `users`, which covers everything W3 needs without new grants.
9. **Client IP:** `server.ClientIP(ctx)` returns the trusted-proxy-resolved address and zero only for an unparsable peer. W3 rate limiting uses only this value.
10. **Spec discrepancies:**
    - 07A §3.6 rotates sessions on "privilege change" as well; spec §4.4.1 says password change only. See R1.
    - §4.7.5 appears twice (editorial).
    - §6.1 says `/` redirects signed-in users to `/torrents`, which does not exist until W4. See R3.

## 3. Packages and interfaces

| Package | Content | Dependencies |
|---|---|---|
| `internal/web/ratelimit` (new) | Generic keyed limiter (§7). No HTTP or SQL. | stdlib |
| `internal/web/accounts` (extended) | `sessions.go` (create/lookup/touch/delete/purge), `login.go` (credential load, conditional login commit), optional `PurgeExpiredLinks` | pgx |
| `internal/web/password` (extended) | `Service.Rehash(ctx, pw)`: argon2id at current parameters for any input of 1..`MaxLoginLength` bytes, through the limiter | — |
| `internal/web/auth` (new) | Cookie codec, CSRF derivation and verification, pre-session cookie, `Principal` in context, access-class wrappers, login/logout handlers, purger loop | accounts, password, ratelimit, server (Router, `ClientIP`, `RequestID`), ui |
| `internal/web/server` (extended) | Access classes on `Router.Handle`; a user ID slot in `reqState`; `Config` startup hooks (§6.4) | — |
| `internal/web/ui` (extended) | `login.html`, `home.html` (interim), shared `csrf` hidden-field partial | — |

Interfaces inside `auth`, so handler tests use fakes and store tests use real PostgreSQL:

```go
type SessionStore interface {
    CreateLogin(ctx, LoginCommit) (Session, error)              // accounts.CompleteLogin
    LookupSession(ctx, tokenHash []byte, idle time.Duration) (Session, error)
    TouchSession(ctx, tokenHash []byte, idle, throttle time.Duration) error
    DeleteSession(ctx, tokenHash []byte) error
    LoginCredential(ctx, username string) (Credential, error)  // id, hash (nullable), active
}
type Verifier interface {        // *password.Service
    Verify(ctx, pw, hash string) (bool, error)
    DummyVerify(ctx, pw string) error
    NeedsRehash(hash string) bool
    Rehash(ctx, pw string) (string, error)
}
```

`Principal{UserID int64; Username, Role string; Remember bool}` is read with `auth.Current(ctx)`.

`webdb.Backend` gains `Pool() *pgxpool.Pool`. `main.runWeb` wires `accounts.New(backend.Pool())` into `auth.New(...)`.

## 4. Session and CSRF design

### 4.1 Tokens and cookies

- **Session token:** 32 bytes from `crypto/rand`, sent as unpadded base64url (43 chars). The DB stores `sha256(raw 32 bytes)`.
  - A cookie value that is not exactly 43 valid base64url characters is treated as absent: no DB query is made and the cookie is cleared.
  - If several cookies carry the session name, the first well-formed one is used. With `__Host-`, nothing can shadow it from a subdomain.
- **Cookie names:**

  | Cookie | Production | Dev mode (loopback http) |
  |---|---|---|
  | Session | `__Host-magnetico_session` | `magnetico_session` |
  | Pre-session | `__Host-magnetico_presession` | `magnetico_presession` |

- **Cookie attributes:**

  | Cookie | Attributes |
  |---|---|
  | Session | `HttpOnly; SameSite=Lax; Path=/`, no Domain; `Secure` in production only |
  | Pre-session | `HttpOnly; SameSite=Strict; Path=/; Max-Age=3600`, no Domain; `Secure` in production only |

  - Dev mode is already gated by `checkPublicURL` and `checkListen`. The dev-mode warning is logged at startup (§4.4.3; W3 adds it).
- **Cookie lifetime:**

  | Session type | Client-side cookie | Server-side (authoritative) |
  |---|---|---|
  | Ordinary | Browser-session cookie (no `Expires`/`Max-Age`) | idle **and** absolute expiry |
  | Remember | `Expires` = the DB-returned `expires_at`, `Max-Age` = `expires_at − db_now` | absolute expiry only |

  - The cookie is never re-issued on activity, so "remember" is never extended.
  - Any request on a session route that presents an invalid, expired or deleted session gets a deletion `Set-Cookie` (`Max-Age=0`, same name and attributes).

### 4.2 Session SQL (all in `accounts`; one clock: PostgreSQL `statement_timestamp()`)

**Create** (only inside the login transaction, §5):
```sql
INSERT INTO magnetico_web.sessions (token_hash, user_id, created_at, last_seen_at, expires_at, remember)
VALUES ($1, $2, statement_timestamp(), statement_timestamp(), statement_timestamp() + $3::interval, $4)
RETURNING expires_at, statement_timestamp()
```
`$3` is `REMEMBER_LIFETIME` when `$4` is true, otherwise `ABSOLUTE_LIFETIME`. Both are positive (validated at startup), so the `expires_at > created_at` CHECK always holds.

**Lookup** (every request on a session route; one statement, no cache):
```sql
SELECT s.user_id, u.username, u.role, s.remember, s.expires_at,
       s.last_seen_at <= statement_timestamp() - $3::interval AS touch_due
  FROM magnetico_web.sessions s
  JOIN magnetico_web.users u ON u.id = s.user_id
 WHERE s.token_hash = $1
   AND s.expires_at > statement_timestamp()
   AND (s.remember OR s.last_seen_at + $2::interval > statement_timestamp())
   AND u.active
```
- `$2` is the idle timeout; `$3` is the throttle `min(5m, idle/10)` (§4.4.4).
- `u.active` is read in the same lookup (§4.2.2, §4.7.5), and the role is read there too.
- Zero rows (unknown, expired, idle, disabled owner) gives `ErrUnauthenticated`.

**Touch** (only when `touch_due`; failure is logged and does not fail the request):
```sql
UPDATE magnetico_web.sessions SET last_seen_at = statement_timestamp()
 WHERE token_hash = $1
   AND last_seen_at <= statement_timestamp() - $3::interval
   AND expires_at > statement_timestamp()
   AND (remember OR last_seen_at + $2::interval > statement_timestamp())
```
- **Conditional:**
  - it never revives an expired or idle session;
  - it never undoes a concurrent delete, because an `UPDATE` cannot insert;
  - concurrent requests that both see `touch_due` write at most once usefully, since the second matches 0 rows.
- Remember sessions also get `last_seen_at` updated. The column is informational for them and does not extend `expires_at`.
- **Effective idle window:** because writes are throttled, an ordinary session expires between `idle − throttle` and `idle` after its last request (at most 10% early). Documented.

**Logout:** `DELETE FROM magnetico_web.sessions WHERE token_hash = $1`. Deleting 0 rows is not an error (idempotent replay).

**Purge** (background, §6.5), in batches until a batch is short:
```sql
DELETE FROM magnetico_web.sessions WHERE token_hash IN (
  SELECT token_hash FROM magnetico_web.sessions
   WHERE expires_at <= statement_timestamp()
      OR (NOT remember AND last_seen_at + $1::interval <= statement_timestamp())
   LIMIT 1000)
```
No new index: the table is small, and §6.2.6 forbids speculative indexes. Seq-scan cost is a documented risk (R6).

**Clock semantics:**

- Every expiry decision is made in SQL on the DB clock. The only app-clock value is the pre-session cookie's issue time (§4.3).
- Lookup and touch are single autocommit statements, so `statement_timestamp()` equals `now()` there. It is used anyway, for uniformity with W2.
- Inside the login transaction, which can wait on the user row lock, `statement_timestamp()` is mandatory, as in W2.
- `created_at`/`last_seen_at` are written explicitly, not via `DEFAULT now()`, so they share the INSERT's statement time.

### 4.3 CSRF: two layers

**Layer 1 (unchanged):** `http.CrossOriginProtection` with the public origin trusted. It rejects cross-origin browser requests using `Sec-Fetch-Site`/`Origin`, but deliberately lets through requests that carry neither header. That gap is why layer 2 is required (spec §4.5.2).

**Layer 2: synchronizer token** (form field `csrf_token`, 43-char base64url, compared with `subtle.ConstantTimeCompare`):

| Context | Bound to | Token |
|---|---|---|
| Session forms (private routes) | the session cookie | `b64url(SHA-256("magnetico/csrf/session/v1\x00" ‖ raw session token))` |
| Pre-session forms (public routes) | the pre-session cookie | `b64url(SHA-256("magnetico/csrf/pre/v1\x00" ‖ pre-session cookie value))` |

- **Pre-session cookie value:** `v1.<b64url(32 random bytes)>.<unix issue time>`. It is reused while younger than 1 h and otherwise reissued on the next GET of a public form. A POST with a missing, malformed or expired (> 1 h, checked server-side) cookie fails.
- **Why derived and not stored:**
  - `sessions` has no CSRF column, and the schema is fixed at v1.
  - A domain-separated hash of a 32-byte HttpOnly secret cannot be computed without that secret.
  - It differs from the DB-stored `sha256(token)`.
  - It survives restarts and needs no new configuration.
  - Cookie injection, which defeats unsigned double-submit, is blocked by the `__Host-` prefix plus `Secure`/HSTS in production. Dev mode is loopback-only. See decision **D2** for the HMAC alternative.
- **Rotation:**
  - Session tokens change at every login (new session) and, in W6, at session rotation; the CSRF token follows automatically.
  - Successful login deletes the pre-session cookie, so the login form's token cannot be replayed.
  - Logout deletes the session, so its token is dead.
  - Tokens are per session, not per request, which is the standard synchronizer pattern; replay is only possible by someone who already holds the cookie.
- **Enforcement is structural (§6.1):**
  - The router wraps every non-safe-method route of class `Session` with the session-token check and of class `PublicForm` with the pre-session check.
  - Registering a non-safe method under class `Public` panics at startup.
  - Safe methods (GET/HEAD) never change state. Each W3 handler is reviewed for this, and the W3 GET handlers write nothing except the pre-session cookie.
- **Failure:** `403` with a generic "This form has expired or is invalid. Go back, reload the page and try again." page that links to the route's GET form. No form data is echoed. A failed CSRF check happens before the handler, so it is not counted by the login rate limiter.
- **Reuse in W6:** a W6 public form (`/register/{code}`, `/password/*`, `/email/verify/*`) registers as `PublicForm`. Its GET handler calls `auth.FormToken(w, r)` and passes the token to the template's `csrf` partial. Private POSTs register as `Session`, and the template uses `auth.SessionFormToken(r)`. No handler validates tokens itself.
- **W7 note:** bearer routes get their own classes, with no cookie authentication and no cookie CSRF (§4.5.3). Whether cross-origin protection needs `AddInsecureBypassPattern` for them is decided in W7.

### 4.4 Session fixation, rotation and ownership

- **Fixation:**
  - A session token is only ever generated by the server at successful login.
  - Client-supplied session values are never adopted.
  - If the login POST presented a session cookie (valid or not), its row is deleted **in the login transaction** before the new one is inserted, by `token_hash`, which proves possession.
  - The pre-session cookie is never promoted to a session; it is deleted.
- **Rotation:**

  | Trigger | Owner | Behaviour |
  |---|---|---|
  | Login | W3 | always a new session |
  | Self-service password change | W6 | other sessions deleted, current row replaced by a new token in the same transaction; W3 exposes the session-creation statement as `accounts.createSessionTx` for it |
  | Password reset, link completion, `set-password` | W2 | all sessions deleted (already implemented) |
  | Role change | — | no rotation: the role is read in every lookup and grants no web capability in v1 (§4.2.3); see R1 |

### 4.5 Concurrency (READ COMMITTED; lock order users → sessions)

- **Lookup vs `Disable`:**
  - A lookup that runs after `Disable` commits sees `active = false` or no row, and fails.
  - A request authorized before the commit may complete (§4.7.5).
  - No cache exists.
- **Lookup vs logout or reset:** the same reasoning applies; a deleted row cannot be resolved again.
- **Touch vs delete:** the conditional `UPDATE` affects 0 rows, so nothing is resurrected.
- **Login vs `Disable`, reset or `set-password`:** see §5.3.
- **Enable:** sessions do not come back, because the rows were deleted (W2) and enable inserts nothing. The user must log in again.

### 4.6 Database unavailable

| Situation | Response |
|---|---|
| Session lookup error (not "no rows") | `503` generic page with request ID. The user is never treated as anonymous (no redirect loop, no login form claiming signed-out) and never as authenticated. |
| `GET /login` with a session cookie whose lookup errors | `503` |
| `POST /login` credential load or commit error | `503`, no cookie set |
| `POST /logout` when the delete fails | `503` with "sign-out could not be completed, try again"; the cookie is **not** cleared, so the retry can still invalidate the server-side session |
| Touch or purge errors | logged with `error_type` only; the request continues |
| `/readyz` | unchanged |

## 5. Login and rehash transaction design

### 5.1 `POST /login` (class `PublicForm`; the router has already checked the pre-session CSRF token)

1. Parse the form fields `username`, `password`, `remember` (checkbox) and the optional `next` (D4). The body is already capped by `limitBody`, and credentials in the query string are ignored.
2. **Rate limit** (§7): reserve one unit from the IP bucket and one from the normalised-username bucket. If either refuses, return `429` with the generic message and `Retry-After`. No DB access or hashing happens.
3. **Input shape** (account-independent):
   - Empty password, or a password over 4,096 bytes: generic failure without hashing (§4.3.2). The timing depends only on the input.
   - A username that is not valid UTF-8, contains NUL, or is over 100 bytes (25 code points × 4 bytes): treated as an unknown account (dummy verification), without querying.
   - **Why validate first:** PostgreSQL rejects invalid UTF-8 or NUL in `text` with an error, which would otherwise surface as a `503` and act as an oracle.
4. `accounts.LoginCredential`:
   ```sql
   SELECT id, password_hash, active FROM magnetico_web.users WHERE username = $1
   ```
   Exact, case-sensitive match; no lock.
5. **Verify** (outside any transaction, through the shared limiter):

   | Case | Action | Result |
   |---|---|---|
   | no row / `password_hash IS NULL` / `NOT active` | `DummyVerify(pw)` | generic failure |
   | `Verify` → `ErrUnsupportedHash` | `DummyVerify(pw)` (closes the oracle in §2.7); WARN log with `user_id` and `needs_reset` | generic failure |
   | `Verify` → `(false, nil)` | — | generic failure |
   | `Verify` → `(true, nil)` | continue | — |
   | any `ErrBusy` | `BusyCount` already incremented | `503` + `Retry-After: 5` |
   | context error | — | `503` |
   | `ErrNotPrepared` | cannot happen after startup (§6.4) | `503` + ERROR log |

   - **Generic failure:** `401`, the re-rendered login form, and the single message "Wrong username or password, or the account is unavailable." (07A §7.2). The username field is echoed (auto-escaped); the password never is. The pre-session token is unchanged.
6. **Rehash** if `NeedsRehash(hash)`: `newHash = Rehash(ctx, pw)`, still outside the transaction.
   - If `Rehash` returns `ErrBusy`, see decision **D1**. Recommended: log, skip the rehash, and continue the login with `newHash = NULL`. The account rehashes on a later login.
7. **Commit** with `accounts.CompleteLogin` (one transaction), §5.2.
8. **Success:**
   - `Set-Cookie` for the session (attributes per §4.1) and a deletion `Set-Cookie` for the pre-session cookie;
   - username-bucket reservation refunded (§7);
   - INFO log `login succeeded user_id=…`;
   - `303` to the validated `next` or `/`.

### 5.2 `CompleteLogin`: the optimistic-concurrency commit

```sql
BEGIN;  -- READ COMMITTED
UPDATE magnetico_web.users
   SET last_login_at = statement_timestamp(),
       password_hash = COALESCE($3, password_hash)      -- $3 = rehash or NULL
 WHERE id = $1 AND active AND password_hash = $2      -- $2 = the exact hash string verified
RETURNING username, role;
-- 0 rows → ROLLBACK, return ErrLoginStale (§5.3)
DELETE FROM magnetico_web.sessions WHERE token_hash = $4;   -- only if a session cookie was presented
INSERT INTO magnetico_web.sessions … RETURNING expires_at, statement_timestamp();  -- §4.2
COMMIT;
```

- **The guard:**
  - The conditional `UPDATE` takes the user row lock.
  - After any lock wait, PostgreSQL re-evaluates `active AND password_hash = $2` against the committed row.
  - So the login, the rehash and the session become visible together or not at all, and only if the account still has exactly the hash that was verified and is still active.
- **What the commit writes:**
  - `last_login_at` is written only here, i.e. only for a login whose session was committed. Failed attempts never write it.
  - `password_changed_at` is **not** touched by a rehash: the password did not change.
- **Lock order:** users row → sessions, the same as `Disable` and the W2 reset paths, so no deadlock cycle.
  - Deleting a presented old session that belongs to another user takes only that session row's lock and never that user's row, so it cannot form a cycle either.
- **Hashing never happens inside the transaction.**

### 5.3 Races and the single bounded retry

| Concurrent writer | Commits first | Outcome |
|---|---|---|
| Admin `set-password`, reset-link completion, or email reset (W6) | the writer | `password_hash ≠ $2` → `ErrLoginStale`. The rehash never overwrites the new password. |
| the same writers | the login | The writer then deletes the new session and replaces the hash, rehashed or not, per W2. |
| `Disable` | the disable | `active = false` → refused, and no session exists. |
| `Disable` | the login | The disable then deletes the new session. |
| A second legacy login of the same user (two tabs) | the other login, which rehashed | `$2` (old legacy hash) ≠ the new argon2id hash → `ErrLoginStale`, although the password is correct. |

**Retry rule for `ErrLoginStale`, at most once:**

1. Re-run `LoginCredential`.
2. If the account is active and the hash changed, verify the password against the **new** hash (one more limiter-bound verification; no rehash, since the new hash is current).
3. If that succeeds, run `CompleteLogin` again with the new hash as `$2`.

The benign two-tab race then succeeds, while a password changed by reset fails verification and yields the generic failure. A second staleness is a generic failure. No unbounded loop.

### 5.4 `password.Service.Rehash` (W2 package addition)

- Same output as `hash()`: argon2id at `CurrentParams` through the limiter.
- Accepts any input of 1..`MaxLoginLength` bytes, because legacy passwords may break the 8–1,024 new-password policy.
- Never called except after a successful `Verify` of that same input.
- **bcrypt note:** a bcrypt-verified password over 72 bytes is rehashed as typed. From then on all its bytes are significant, so a user who relied on bcrypt's truncation must type the same full string. Documented (R5).

## 6. Middleware and routing changes

### 6.1 Access classes on the router

`Router.Handle(pattern string, k Kind, a Access, h http.Handler)` (W1 callers updated to `Public`):

| Access | Authentication | CSRF on POST/PUT/PATCH/DELETE | Used by |
|---|---|---|---|
| `Public` | none; non-safe methods refused at registration (panic) | n/a | `/healthz`, `/readyz`, `/robots.txt`, `/static/` |
| `PublicForm` | none (an optional session lookup is available to the handler) | pre-session token | `/login` (W3); W6 public forms |
| `Session` | required; anonymous or invalid → clear cookie + `303 /login[?next=…]`; DB error → `503` | session token | `/`, `/logout` (W3); W4/W6 pages |
| *(W7)* `APIToken`, `RSSToken` | bearer or RSS token; `401` | none (§4.5.3) | not added in W3 |

- **Wrapper order** inside `routeDeadline`: route label and deadline → authentication → CSRF → handler. The authenticated user ID is stored in `reqState` for the access log (spec §10.1). The username is never logged.
- **Non-HTML requests:** handled by route class, not by `Accept` sniffing. Session routes are UI routes and redirect (spec §4.1); API classes (W7) answer `401`.
- **Unknown paths:** still `404` from the mux for everyone. "Every other route" in §4.1 is read as every *registered* route (consistent with §6.1.1: old routes → 404).

### 6.2 W3 routes

| Route | Class | Behaviour |
|---|---|---|
| `GET /login` | PublicForm | If a valid session cookie is present → `303 /`. Otherwise issue or reuse the pre-session cookie and render the form (username, password, "Keep me signed in on this device", hidden `csrf_token`, hidden `next`). No "Forgot password?" link until W6. |
| `POST /login` | PublicForm | §5 |
| `POST /logout` | Session | Delete the session row, send a deletion `Set-Cookie`, `303 /login`. A replay with an already-deleted session hits the Session class, which clears the cookie and redirects with no write. |
| `GET /{$}` | Session | **Interim (R3), exact root only** (`GET /` would be a ServeMux subtree and swallow unknown GET paths such as `/torrents` before W4; those stay 404 for anonymous and authenticated callers alike, with routing tests for both): anonymous → `303 /login`; signed in → a minimal page "Signed in as {username}" with the logout form. W4 changes the signed-in branch to `303 /torrents` per §6.1. |

**`next` (D4, approved):** tests cover external URLs, `//host`, `/\host`, `%2F%2F`, `%5C`, control characters and over-length values. Accepted only when it parses as a relative reference with an empty scheme and host, starts with exactly one `/` (not `//` or `/\`), contains no control characters, and is at most 2,048 bytes. Otherwise `/` is used. It is never logged (the access log has no URLs).

### 6.3 Templates

- `login.html` and `home.html` on the existing layout and `app.css`, plus a `csrf` partial.
- Plain labelled inputs; no JavaScript.
- W5 restyles them; W3 adds no CSS framework.

### 6.4 Startup and shutdown

- `server.Config` gains `Startup []func(ctx) error`. They run after `backend.Verify` and before `Listen`, within the existing verification timeout, and any error fails startup without listening.
- W3 registers `password.Default.Prepare`, so no login traffic is accepted before the dummy hash exists.
- `main.webErrorDetail` gets a fixed message for a Prepare failure.
- The dev-mode warning is logged at startup.
- **Purger lifecycle (owner correction).** `server.Config` gains one `Background func(ctx context.Context)` field (no job framework). `Server.Run` owns it:
  1. It starts only after verification, the startup hooks and `Listen` all succeeded, under its own `purgeCtx` with a `purgeDone` channel.
  2. A listen failure or startup failure never starts it. Every later exit path (unexpected `Serve` failure, SIGTERM, forced shutdown) calls one `stopBackground()` that cancels `purgeCtx` and waits for `purgeDone` within a bound, always **before** `backend.Close()`.
  3. Order on shutdown: `srv.Shutdown` (grace) → on expiry the forced cancel/close and in-flight wait (as today) → `stopBackground()` → `backend.Close()` with the existing close bound.
  4. A purger that does not stop within its bound sets `incomplete` (`ErrShutdownIncomplete`, exit 3).
  5. The purger uses `purgeCtx`-derived per-run contexts (not `baseCtx`), so it is never cancelled early by `baseCancel` ordering and never outlives the pool.

### 6.5 Purger

- Runs every 15 min with ±20 % jitter, plus once 1 min after start.
- Each run is a batch loop (§4.2) under a 30 s context.
- Logs counts at DEBUG and errors at WARN with type only.
- **D3:** the same run also deletes `one_time_tokens WHERE expires_at <= statement_timestamp()`, the open W2 residual. This is safe without the user lock: expired rows can be neither consumed (W2 consumption requires `expires_at > statement_timestamp()`) nor revived, and issuance deletes the user's old row anyway. Concurrent deletion only makes one of the two deletes affect 0 rows.

## 7. Rate-limit design (`internal/web/ratelimit`)

- **Algorithm:** a token bucket per key: `burst` and a refill `interval`, with an injectable clock (`func() time.Time`) so tests use a fake clock. No goroutine.
- **Bounded memory:**
  - a map plus an LRU list with a fixed `maxKeys`, using lazy eviction on insert: least recently used first, and preferentially entries already refilled to full;
  - keys are fixed-size, so memory is about `maxKeys × ~150 B`;
  - at the default 10,000 keys per limiter that is under 2 MiB each.
- **API:**
  ```go
  r := l.Reserve(key) // r.OK, r.RetryAfter, r.Cancel() refunds the unit
  ```
- **Key derivation:**
  - **Client IP:** `ratelimit.IPKey(addr)` uses the IPv4 `/32` or IPv6 `/64` (an attacker trivially owns a whole /64). The source is only `server.ClientIP(ctx)`, the W1 trusted-proxy resolution, so a forwarded header from an untrusted peer never changes it. An unparsable peer (zero address) shares one "unknown" key.
  - **Username:** `SHA-256("rl/user\x00" ‖ strings.ToLower(username))` truncated to 16 bytes. The typed string is never stored in memory, since a password typed into the username field would otherwise be kept. Case folding makes `Alice`/`alice` share a bucket, which is the conservative choice, and does not affect case-sensitive authentication (§4.2.5).
- **Login policy** (proposal, D5; constants, no new configuration):

  | Bucket | Burst | Refill | Sustained |
  |---|---|---|---|
  | Per IP | 20 | 1 per 30 s | 120 attempts/h |
  | Per username | 5 | 1 per 60 s | 60/h |

  - Every attempt reserves from both buckets atomically: if the second refuses, the first reservation is cancelled.
  - On success the username unit is refunded, so a legitimate user is not throttled by their own successful logins. The IP unit is kept.
  - The response is identical for existing and non-existing usernames, so there is no enumeration.
  - There is no lockout: buckets always refill.
  - The Argon2 limiter (2 concurrent, 3 s wait) remains a hard global ceiling on verification throughput.
- **Overflow:** under extreme key churn the LRU eviction weakens per-key limits (documented, R7). The global hash limiter still bounds the cost.
- **`429` response:** the login page with "Too many sign-in attempts. Please wait and try again." and `Retry-After` set to the larger wait in whole seconds, at least 1.
- **W6/W7 reuse:** each creates its own named limiter instance (reset/verify per IP and per account, registration per IP, failed token authentications per IP, RSS per token) with the same package. W3 wires only login.

## 8. Tests and acceptance criteria

All tests use disposable `internal/pgtest` databases and synthetic users. No production or legacy access and no network beyond loopback.

**Time control:**

- **DB-clock expiry** (sessions, purge, touch): rows are *backdated*. `created_at`, `last_seen_at` and `expires_at` are shifted together by an interval (this preserves `expires_at > created_at`), so tests are deterministic with no sleeps.
- **App-clock parts** (rate limiter, pre-session issue time): an injected fake clock.
- **Rejected alternative:** injecting an app clock into SQL. It would create a second clock and drift from production semantics, against W2's single-clock rule.

### 8.1 `ratelimit` (unit, `-race`)

- burst then refusal with an exact `RetryAfter`;
- refill after advancing the fake clock;
- `Cancel` refunds;
- the two-bucket atomic reserve refunds the first bucket on refusal;
- `maxKeys` bound respected under 100k distinct keys, with full buckets evicted first;
- IPv6 /64 grouping, IPv4-mapped normalisation, zero-address key;
- concurrent `Reserve` from 64 goroutines never over-grants.

### 8.2 `accounts` sessions and login (PostgreSQL, `-race`)

- **Create:**
  - ordinary `expires_at = statement time + absolute`, remember `= + remember`;
  - `expires_at` is never NULL;
  - only the 32-byte hash is stored, and no row contains the raw token.
- **Lookup:**
  - valid;
  - unknown hash;
  - absolute expiry (backdated);
  - ordinary idle expiry;
  - a remember session is **not** idle-expired but **is** absolute-expired;
  - a disabled owner is refused, and after enable the row is gone (deleted by disable), so it stays refused;
  - the role is read live: `SetRole` takes effect on the next lookup.
- **Touch:**
  - `touch_due` false within the throttle and true after a backdate;
  - a touch never revives an idle or expired row;
  - two concurrent touches leave one write;
  - a touch after a concurrent delete affects 0 rows.
- **Delete and purge:**
  - deleting twice is idempotent;
  - purge removes exactly the absolute-expired and idle-expired ordinary rows, keeps valid ones, and works in batches (more than 1,000 rows);
  - (D3) the expired `one_time_tokens` purge removes only expired rows and does not disturb issuance or completion running concurrently.
- **`CompleteLogin`:**
  - writes `last_login_at` and the session atomically;
  - the `beforeCommit` hook aborting rolls both back;
  - a rehash replaces the hash and leaves `password_changed_at` unchanged;
  - `$2` mismatch → `ErrLoginStale` with nothing written;
  - an inactive account → `ErrLoginStale`;
  - the presented old session is deleted in the same transaction.
- **Races** (deterministic, both orders, using the `beforeCommit` hook or a held user lock plus `pg_stat_activity` blocking like the W2 expiry tests; plus 200-iteration stress):
  - login vs `SetPassword`;
  - login vs `CompletePasswordLink`;
  - **rehash vs `SetPassword`** (the stored hash ends up equal to the set-password hash, never the rehash);
  - login vs `Disable` (never an active session for a disabled user after both commit);
  - two concurrent legacy logins (both succeed through the bounded retry; one rehash);
  - lookup vs `Disable`, where every lookup started after `Disable` returns fails.

### 8.3 `password` (addition)

- `Rehash` accepts 1-byte and 4,096-byte inputs, rejects 0 and 4,097 bytes, goes through the limiter, and the output classifies as `ClassCurrent` and verifies.
- After a rehash the PHP fixtures (argon2i, argon2id-sodium, bcrypt including the 72/73/100-byte cases) verify against their own password and not a wrong one.

### 8.4 `auth` and `server` handlers (httptest; fake store and verifier for shape, real store for flows)

| Area | Cases |
|---|---|
| Correct and incorrect passwords | success → 303 plus session `Set-Cookie`; wrong password → 401 with the generic message |
| Generic failure shape | unknown, inactive, NULL-hash, unsupported-hash, wrong-password, invalid-UTF-8, NUL and over-long usernames all give the same status, the same generic message, the same page structure and the same cookie behaviour; the instrumented verifier records **exactly one** limiter-admitted verification (`Verify` or `DummyVerify`) for each. Whole bodies are compared only where input and pre-session state are identical (the page echoes the username and carries a CSRF token). This is the deterministic timing-shape test. A wall-clock comparison is logged only, never asserted. |
| Legacy and rehash | each PHP fixture format logs in and the stored hash becomes current argon2id; D1: `Rehash` → `ErrBusy` still logs in with the hash unchanged |
| Busy | the verifier returns `ErrBusy` → 503 with `Retry-After`, no session, no `last_login_at` |
| Lifetimes | ordinary cookie has no `Expires`/`Max-Age`; remember cookie `Expires` equals the row's `expires_at` |
| Cookie security | production: `__Host-` name, Secure, HttpOnly, SameSite=Lax, Path=/, no Domain (pre-session SameSite=Strict); dev: non-prefixed name, no Secure; dev configuration is unreachable with a non-loopback public URL or listen address (opflags tests, existing plus additions) |
| Fixation | a login with a presented attacker-chosen cookie never yields that token; a presented valid old session is deleted; the new token is server-generated |
| Session enforcement | anonymous `GET /` → 303 `/login?next=%2F`; an invalid or expired cookie → 303 plus a deletion `Set-Cookie`; DB error → 503 without redirect; `GET /login` while signed in → 303 `/` |
| Logout | POST with a valid CSRF token → row deleted, cookie cleared, 303; GET `/logout` → 405; missing or wrong token → 403 and the session survives; replay after logout → 303 `/login` with no DB write; delete failure → 503 and the cookie kept |
| CSRF (unauthenticated POST) | `POST /login` without the pre-session cookie, with an expired (fake clock) or malformed one, a token from a different pre-session, or a session token instead of a pre-session token → 403; after a successful login the old login token is rejected |
| CSRF (authenticated POST) | a session-derived token from another session → 403; the pre-session token on a Session route → 403 |
| Structural | registering `POST` under `Public` panics; every registered non-safe route has CSRF (a test iterates the route table) |
| Cross-origin | `Sec-Fetch-Site: cross-site` and a foreign `Origin` on `POST /login` and `/logout` → 403 even with a valid token; same-origin passes |
| Rate limits | IP exhaustion → 429 with `Retry-After` from the fake clock; username exhaustion across different IPs → 429; identical 429 for existing and unknown usernames; success refunds the username unit; spoofed `X-Forwarded-For` from an untrusted peer does not change the IP bucket, while the same header from a trusted proxy does |
| Concurrency | 32 parallel logins of one user give 32 distinct sessions; disable during a stream of authenticated requests (no request authorized after the `Disable` return succeeds) |
| Redaction | log capture over all of the above: no password, raw session token, cookie value, CSRF token, pre-session value, username, `next` or query string; responses never echo the password; no `Set-Cookie` on 4xx/5xx except the documented deletions |
| No application state on safe requests | `GET /login` and `GET /{$}` perform no application-level change (no users/tokens/invites/links rows changed). Documented bookkeeping is allowed and asserted explicitly: throttled `sessions.last_seen_at` on authenticated GETs, and pre-session cookie issuance on public GET forms. Routing: `GET /torrents` and a legacy path return 404 for anonymous and authenticated callers; `GET /{$}` does not match `/x`. |

### 8.5 Real-binary integration (`main_web_test.go`)

1. **Login flow:**
   - `migrate web-schema-init`, `web user add` then `web user set-password` (piped), then start `magnetico web` (dev mode);
   - `GET /login` → cookie and token → `POST /login` → `GET /` shows the username → `POST /logout` → `GET /` redirects;
   - the session row exists and is then deleted;
   - stderr has no secrets.
2. **Disable/enable:** while signed in, `web user disable` → the next request redirects; `enable` → still signed out until a new login.
3. **Legacy rehash:** a PHP fixture hash inserted by SQL → login succeeds → the stored hash is now `$argon2id$v=19$m=65536,t=3,p=4$…`.
4. **Production cookie mode:** an `https://` public URL with a loopback listener; the client sends the cookie header manually and asserts `__Host-` and `Secure`.
5. **Rate limit through the binary:** a spoofed `X-Forwarded-For` from the untrusted loopback peer does not evade 429.
6. **Startup:** Prepare runs before listen (the readiness log line follows it); SIGTERM exits 0 with the purger running.

### 8.6 Acceptance (W3 row of 07A §8 plus this brief)

- Sessions do not survive disable.
- Every session row has a finite expiry.
- Fixation is covered.
- CSRF applies to all POSTs, including unauthenticated ones.
- Responses and verification shape are generic and equal.
- Rehash is race-safe.
- 429/503 carry `Retry-After`.
- The tests in §8.1–8.5 pass under `-race` (stress tests with `-count=5`).
- `go build ./...` and `go vet ./...` are clean.
- API/RSS token suspension is already proven at store level by W2 and is re-exercised at HTTP level in W7.

## 9. Implementation sequence and stops

| Step | Content | Commit |
|---|---|---|
| **S0 — STOP (now)** | Owner reviews this plan and answers D1–D5 | — |
| 1 | Write `docs/wip/07D-web-auth-session-plan.md` (this plan, with the decisions) and the README row; apply only the spec deltas the owner approves (R1–R4, the §4.7.5 numbering) | `docs: Task 07D — W3 plan` |
| 2 | `ratelimit` package and tests | `feat(web): bounded in-process rate limiter` |
| 3 | `password.Rehash`; `accounts` session and login SQL, purge (D3); store tests including races | `feat(web): session store and race-safe login commit` |
| **S1 — STOP** | Review the SQL, the rehash concurrency and the race tests | — |
| 4 | `server` access classes and user ID in the log; `auth` cookies, CSRF and middleware; `webdb.Pool`; config passthrough | `feat(web): session authentication and CSRF middleware` |
| 5 | Login/logout handlers, templates, startup hooks (Prepare, purger), `main` wiring, handler tests | `feat(web): login and logout` |
| 6 | Binary tests; `docs/web-deployment.md` (cookies, dev mode, rate limits, purge); `docs/wip/07D-web-auth-session-report.md` | `test/docs: W3 integration and report` |
| **S2 — STOP** | Final W3 review. No push; W4 does not start. | — |

**Validation (narrowest first, after each step):**

```
go build ./... && go vet ./...
go test -count=1 -race ./internal/web/ratelimit
go test -count=1 -short -race ./internal/web/password && go test -count=1 ./internal/web/password
go test -count=1 -race ./internal/web/accounts && go test -count=5 -race -run 'Login|Session|Concurrent' ./internal/web/accounts
go test -count=1 -race ./internal/web/auth ./internal/web/server ./opflags
go test -count=1 -race ./internal/web/...
go test -count=1 -race -run 'TestWeb' .   &&   go test -count=1 .
```

`dht/...`, `internal/crawler` and `internal/e2e/...` are not run, as in W1/W2. The report states that a full `go test ./...` was not run.

## 10. Dependencies and owner decisions

**Dependencies:** none new. `http.CrossOriginProtection` is stdlib (Go ≥ 1.25; the repo is on go 1.26 with toolchain 1.27). `crypto/sha256`, `crypto/subtle` and `container/list` are stdlib.

**Genuine owner decisions (recommendation first):**

- **Decisions D1–D5 were approved at S0** (recommendations adopted). D1 is documented as an exception to §4.3.4: only when the second limiter acquisition returns `ErrBusy`; verification is never skipped. D2 is a stateless cookie-bound construction, not a server-stored token.
- **D1 — Rehash when the limiter is busy after a successful verification.**
  - *Recommended:* complete the login without rehashing; it is retried at the next login. Logged and counted.
  - *Alternative:* `503` despite the correct password.
  - §4.3.4 ("MUST be replaced in the same transaction") is satisfied whenever a rehash happens; skipping is the deviation to approve.
- **D2 — CSRF token derivation.**
  - *Recommended:* keyless domain-separated SHA-256 of the HttpOnly cookie secret (§4.3); relies on `__Host-` against cookie injection.
  - *Alternative:* HMAC with a per-process random key, which invalidates open forms on every restart, or with a configured secret, which would be a new environment variable.
- **D3 — Purge expired `one_time_tokens` in the W3 purger.**
  - *Recommended:* yes (closes the W2 residual; reasoning in §6.5).
  - *Alternative:* defer it.
- **D4 — Return to the requested page after login (`next`).**
  - *Recommended:* yes, with the strict local-path validation in §6.2.
  - *Alternative:* always `/`.
- **D5 — Login rate-limit numbers** (IP 20 burst / 30 s refill, username 5 burst / 60 s refill, 10k keys, pre-session TTL 1 h, purge every 15 min). These are proposals as constants; any can be changed now without code-structure impact.

## 11. Risks and deviations from the specification

- **R1 — No session rotation on role change.** 07A §3.6 wants it; spec §4.4.1 does not. The role is read on every lookup and `admin` has no web capability in v1, so rotation adds nothing. Recommend aligning 07A with the spec.
- **R2 — Authentication is per route, not a global middleware.** The spec §3.1 position (after cross-origin, before rate limiting and the handler) is preserved. It avoids a DB lookup on probes and assets. Login rate limiting runs inside the login handler because it needs the form's username.
- **R3 — Interim `/`.** Signed-in users see a placeholder page instead of the §6.1 redirect to `/torrents`, until W4 exists.
- **R4 — Effective idle timeout** is up to `throttle` (≤ 10 % of idle, ≤ 5 min) shorter than configured, because `last_seen_at` writes are throttled (§4.4.4).
- **R5 — bcrypt truncation ends at rehash.** After a rehash all bytes of the typed password are significant.
- **R6 — The purge scans `sessions` without an index.** Acceptable at expected sizes; an index would need a measured, approved gain (§6.2.6).
- **R7 — In-process limits are per instance and weaken under extreme key churn** (LRU). One instance is approved (§4.6.2). The Argon2 limiter caps the global cost.
- **R8 — Concurrent-session count per user is unbounded.** The spec is silent. Purge removes expired rows.
- **R9 — Timing equality is asserted by operation shape (one limiter-admitted verification per attempt), not wall-clock.** Network and DB latency differences between "row found" and "no row" remain at query level. The unknown path still runs one indexed lookup, so the shape is the same.
- **R10 — A CSRF token is valid for the whole session or pre-session lifetime** (per-session synchronizer, not per-request), as is standard. Possession of the cookie is required to use it.
- **R11 — Generic login failure status is `401`.** The spec does not fix a status; all failure classes share it.

## 12. Critical files

- **New:**
  - `internal/web/ratelimit/{ratelimit.go,ratelimit_test.go}`
  - `internal/web/auth/{auth.go,cookies.go,csrf.go,middleware.go,login.go,purge.go,*_test.go}`
  - `internal/web/accounts/{sessions.go,login.go,sessions_test.go,login_test.go}`
  - `internal/web/ui/templates/{login.html,home.html}`
  - `docs/wip/07D-web-auth-session-plan.md` and, later, `…-report.md`
- **Modified:**
  - `internal/web/server/{server.go,middleware.go}` (access classes, startup hooks, user ID in the log) and their tests
  - `internal/web/password/service.go` (`Rehash`)
  - `internal/web/webdb/webdb.go` (`Pool`)
  - `internal/web/accounts/store.go` (package doc)
  - `main.go` (`runWeb` wiring, Prepare error text)
  - `main_web_test.go`
  - `docs/web-deployment.md`
  - `docs/wip/README.md`
  - spec/07A deltas only as approved
- **Reused:**
  - `server.ClientIP` / `ResolveClientIP`
  - `crossOrigin`
  - `routeDeadline`
  - `ui.Renderer`
  - `accounts.inTx`, `beforeCommit`, `hashToken`, `ErrUnauthenticated`
  - `password.Default`, `Limiter.BusyCount`
  - the W2 `pg_stat_activity` lock-wait helpers (`holdUserLock`, `waitBlocked`)
  - `internal/pgtest`
  - `opflags` session validation and `DevMode`

## 13. Safety

- Disposable PostgreSQL only.
- No production or legacy database.
- No DHT or BitTorrent traffic.
- No W4–W9 code.
- Nothing is implemented before approval.
- Commits only on this branch with the configured identity, no push or force-push.
- Stop at S0, S1 and S2.
