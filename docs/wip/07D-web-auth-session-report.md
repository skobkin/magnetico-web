# Task 07D / W3 — status report (S1: store layer, S2: HTTP layer)

**Status:** all six steps of [`07D-web-auth-session-plan.md`](07D-web-auth-session-plan.md) are implemented and committed locally. S1 (steps 1–3) was accepted with two corrections (recorded below, applied in `5a36c135`). **Stopped at S2 for the final W3 review.** W4 and later milestones are not started. Nothing is pushed.

## Delivered

| Commit | Content |
|---|---|
| plan commit | Plan, S0 corrections (exact root `GET /{$}`, purger lifecycle owned by `Server.Run`, test expectations), approved spec deltas (§4.3.4 D1 exception, §4.4.4a, §4.5.2 D2/D4, §4.6.1a D5, §6.1 root) |
| `feat(web): bounded in-process rate limiter` | `internal/web/ratelimit`: GCRA buckets, injectable clock, `maxKeys` LRU bound preferring refilled buckets, IPv4/32 and IPv6/64 keys, case-folded hashed username keys, `ReservePair` with refund, `RetryAfterSeconds` |
| `feat(web): session store and race-safe login commit` | `password.Service.Rehash`; `accounts/sessions.go` (token codec, `LookupSession`, `TouchSession`, `DeleteSession`, `PurgeSessions`, `PurgeExpiredLinks`); `accounts/login.go` (`LoginCredential`, `CompleteLogin`, `ErrLoginStale`) |

## What to review

1. **Session SQL** (`accounts/sessions.go`): the lookup is one statement and reads existence, absolute expiry, idle expiry (ordinary sessions only), owner `active` and the live role with `statement_timestamp()`. Touch is a conditional `UPDATE`, so it never revives an idle or expired session and never recreates a deleted one. Purge is batched (1,000 rows).
2. **Optimistic login commit** (`accounts/login.go`): `UPDATE users … WHERE id=$1 AND active AND password_hash=$2 RETURNING …` takes the user row lock and, after any lock wait, re-checks the committed row. `last_login_at`, the optional rehash, the deletion of a presented old session and the new session insert are one transaction. A mismatch is `ErrLoginStale` with nothing written. A rehash does not touch `password_changed_at`. Lock order is users → sessions.
3. **The bounded retry** (reload, re-verify against the new hash, commit without rehash) is the caller's job and arrives with the handler in step 5. The store test `TestTwoConcurrentLegacyLoginsBoundedRetry` shows the store supports it.
4. **`Rehash`** accepts 1..4,096 bytes (legacy passwords can break the new-password policy), uses the shared limiter, and returns `ErrBusy` when saturated (D1: the caller then completes the login without rehash).
5. **D3** (`PurgeExpiredLinks`) needs no user lock: expired rows cannot be consumed (W2 consumption requires `expires_at > statement_timestamp()`) and issuance deletes the same row itself.

## Tests actually run (all passed)

```
go build ./... && go vet ./...
go test -count=1 -race ./internal/web/... ./opflags            (password non-short, incl. all 34 PHP vectors)
go test -count=3 -race -run 'Login|Session|Touch|Lookup|Purge|Concurrent|Versus' ./internal/web/accounts
go test -count=5 -race -run 'LoginRaces|LegacyLogins|Stress|LookupVersus|ConcurrentTouches' ./internal/web/accounts
```

Race coverage: login vs `SetPassword`, vs `CompletePasswordLink` and vs `Disable`, each in **both commit orders** and with and without a rehash (deterministic, using the `beforeCommit` hook and `pg_stat_activity` lock-wait detection); two concurrent legacy logins; 200-iteration stress for login vs `Disable` and for rehash vs `SetPassword` (the operator's password is never overwritten; no session survives); 32 parallel logins give 32 distinct sessions; lookups started after `Disable` returned never authenticate; concurrent touches write once; expired-link purge beside issuance.

Time control: expiry tests **backdate** rows in the database (all three timestamps together, which keeps `expires_at > created_at`) instead of sleeping or injecting an application clock into SQL; the rate limiter uses a fake clock.

**Mutation checks** (each reverted afterwards, code verified identical): dropping the `password_hash = $2` guard, dropping `AND active` in the login commit, ignoring `u.active` in the lookup, and removing the expiry conditions from touch each make at least one of the new tests fail.

## S1 review corrections (owner review of `aa6f8d1e`, `4bbfe503`, `b5ba06d3`: accepted with two corrections)

1. **Refund bound to its bucket instance.** `Reservation.Cancel` used to refund by key, so after the original bucket was evicted a recreated bucket under the same key would receive the refund. Each reservation now holds the `*entry` it charged, and `refund` is ignored unless the key still maps to that exact entry. GCRA and LRU are unchanged. Regression test `TestCancelIgnoresEvictedBucket` (`MaxKeys=1`): reserve A, evict A by reserving B, recreate and exhaust A, cancel the original reservation, and verify that the new A gains nothing; a refund of a live bucket still works.
2. **Concurrency tests fail promptly.** The earlier 600 s timeout came from a failing test leaving a goroutine parked in the commit hook while holding a pooled connection, which then deadlocked `pool.Close` in cleanup. Now:
   - `raceLogin` closes `release` in a deferred function on every exit path, then joins its goroutines, then removes the hook; the hook itself gives up after `blockWait + 10 s`.
   - The wait for the first operation to reach its commit point is bounded (`blockWait` = 20 s) and fails at once if that operation finishes without reaching it.
   - `waitBlockedOr` fails at once if the operation expected to block has already finished (no lock wait occurred), and otherwise after `blockWait`.
   - `holdUserLock` registers an idempotent release as a `t.Cleanup`, so a `Fatal` while holding the lock cannot leave the transaction open (this also protects the W2 expiry tests that use it).
   - Re-running the "drop the `password_hash = $2` guard" mutation now fails in about 14 s (4 tests fail) instead of reaching the 600 s timeout; the code was restored and verified.
3. `TestTwoConcurrentLegacyLoginsBoundedRetry` was renamed `TestStaleLegacyLoginThenBoundedRetry`, and its comment now says what it covers: the *sequential* stale/retry sequence at store level. Genuinely concurrent two-login behaviour is covered by the HTTP integration tests of steps 5–6.

## Known limitations / notes

- Not run: `dht`, `crawler`, `e2e` and other untouched packages, and the full `go test ./...` and root-package binary tests (nothing in the root package changed in this stage).
- The idle window of an ordinary session is up to `throttle` (≤ 10 %, ≤ 5 min) shorter than configured (R4, approved).
- The sessions purge scans without an index (R6, documented in the plan).
- A leftover `postgres:17` test container (`recursing_sinoussi`) from an earlier interrupted run was still running; stopping it was denied by the permission classifier, so it is left for the owner to stop.

---

# Stage S2 — HTTP authentication (steps 4–6)

## Delivered

| Area | Content |
|---|---|
| `internal/web/server` | Access classes on `Router.Handle(pattern, kind, access, handler)`: `Public` (GET/HEAD only, enforced by a panic at registration), `PublicForm`, `Session`. Guards are installed by `Router.SetGuards`; a guarded class without a guard panics at registration. `Router.Routes()` exposes the route table. The access log carries `user_id` (never the username) when authenticated. `Server.OnStartup` hooks run after verification and before `Listen`. `Server.SetBackground` is the single background task. |
| Purger lifecycle (owner correction 2) | `Run` starts the background task only after verification, startup hooks and `Listen` succeeded, under its own context. Every exit path (startup/verification/listen failure: never started; unexpected `Serve` failure; SIGTERM; forced shutdown) calls one `stopBackground`, which cancels and waits (bounded by `bgStopWait`, 5 s) **before** `backend.Close()`. A task that does not stop in time makes the shutdown `ErrShutdownIncomplete` (exit 3) and the pool is still closed within its bound. No job framework. |
| `internal/web/auth` | `Install(srv, cfg)`; cookies (session, pre-session; production `__Host-` names, dev names); cookie-bound CSRF tokens; the `PublicForm` and `Session` guards; `GET/POST /login`, `POST /logout`, `GET /{$}`; the login attempt logic including the single bounded retry after `ErrLoginStale`; the purger (`Background`). |
| `internal/web/ui` | `login`, `home`, `message` templates, a shared `csrf` field template and the page types. |
| `webdb`, `main.go` | `Backend.Pool()`; `runWeb` installs auth with the validated session lifetimes and `DevMode`; a fixed message for a `Prepare` failure. |
| Docs | `docs/web-deployment.md` §6 (routes, cookies, lifetimes, revocation visibility, CSRF, rate limits, hashing capacity, housekeeping, privileges). |

## Behaviour worth reviewing

1. **Exact root** `GET /{$}` (owner correction 1): `/torrents`, legacy paths and anything else is 404 for anonymous and authenticated callers; `TestRoutingExactRootAndUnknownPaths`.
2. **Login order:** rate limit → input shape → credential load → verification (outside any transaction) → optional rehash → `CompleteLogin`. Unknown, inactive, passwordless and unsupported-hash accounts each spend exactly one limiter-admitted verification (`DummyVerify`; an unsupported hash is refused without hashing and *then* followed by a dummy verification). An empty or over-4,096-byte password fails without hashing (account-independent). Invalid-UTF-8, NUL and over-long usernames never reach PostgreSQL (the store answers "unknown").
3. **Rehash exception (D1):** `ErrBusy` from `Rehash` after a successful verification logs in without upgrading; any other rehash failure is a 503.
4. **CSRF (D2):** stateless, cookie-bound, domain-separated SHA-256 of the HttpOnly cookie value. The session class and the pre-session class use different domains. Because the pre-session cookie is stateless, a *copied* pre-session cookie remains valid until its one-hour expiry; the browser deletes it at sign-in, so the old login form is dead for the browser, and a holder of a copied cookie would also have to know the password.
5. **`next` (D4):** `validNext` accepts only a path with exactly one leading `/`, no backslash, no control characters, at most 2,048 bytes, and re-checks the percent-decoded path for a leading `//` or `/\`. Tested for `//host`, `/\host`, `%2F%2F`, `%5C`, `javascript:`, `http:host`, absolute URLs, control characters and over-length values, at unit level and through the form.
6. **Database errors:** a failing lookup is 503 (never an anonymous redirect, never authenticated); a failing logout delete is 503 and keeps the cookie.

## Tests actually run (all passed)

```
go build ./... && go vet ./...
go test -count=1 -race ./internal/web/... ./opflags ./persistence ./internal/ingest/pgqueue
go test -count=3 -race -run 'Concurrent|Parallel|Disable|Stale|Races|Stress' ./internal/web/auth ./internal/web/accounts
go test -count=1 -race -run 'TestWeb|TestMigrateWebSchemaInit|TestSchemaDiagnostics' .
go test -count=1 .                                   (whole root package, 77 s)
```

`internal/web/auth` (real PostgreSQL, handler level, production and dev cookie modes): login/logout flow and cookie attributes; remember-me `Expires` equals the row's `expires_at`; the generic-failure matrix (wrong password, unknown, inactive, passwordless, unsupported hash, case-sensitive name, invalid UTF-8, NUL, over-long): same status, message, structure and cookie behaviour, exactly one limiter-admitted verification each, and whole-body equality only for identical input; legacy argon2i / bcrypt `$2y$` / sodium argon2id fixtures log in and are upgraded; busy and database-failure 503s; fixation; session enforcement (malformed cookie never reaches the database, expired/absolute/idle, DB error); disabled-user sessions; CSRF in both classes (missing, empty, wrong, query-only, other session/pre-session, session token on a pre-session form, expired pre-session via a fake clock, replay after sign-in); cross-origin refusal even with a valid token; structural route classification; safe requests change no application state (documented `last_seen_at` throttling and pre-session cookies are asserted explicitly); rate limits (per IP, per username across IPs, identical 429 for known and unknown, refund on success, spoofed `X-Forwarded-For` ignored, trusted proxy honoured, CSRF rejections not counted); handler-level races (password reset or disable between verification and commit, two concurrent legacy logins both succeeding through the bounded retry — this is where the concurrent two-login behaviour requested in the S1 review is covered); 32 parallel logins; requests started after `Disable` never served; log redaction; purger and startup (Prepare failure fails closed before listening; dev warning).

`internal/web/server`: access-class registration panics, guard application, route table, user ID logging, and the purger lifecycle (startup hook failure, verification and listen failure never start it, SIGTERM and unexpected-`Serve`-failure and forced-shutdown stop it before the backend closes, a stuck task is bounded and reported as incomplete).

Real binary (`main_webauth_test.go`, as the **documented minimum runtime role**): sign-in, private page, wrong password, `web user disable`/`enable` through the CLI (the next request is anonymous; enable restores nothing), logout with CSRF and cross-site refusal, legacy bcrypt upgrade to `$argon2id$v=19$m=65536,t=3,p=4$`, dev-mode cookies and warning, a production-mode (https public URL) run with hand-sent cookies asserting the `__Host-` names and attributes and the headers, per-IP limit at attempt 20 despite a rotating spoofed `X-Forwarded-For`, clean SIGTERM, and no password, token or cookie value in the service output.

**Mutation checks** (each reverted afterwards, files verified identical): removing the session CSRF check, removing the pre-session check, registering `GET /` instead of `GET /{$}`, making `validNext` accept everything, and skipping the dummy verification for an unsupported hash each make at least one test fail.

## Not run / limitations

- Not run: `dht`, `crawler`, `e2e` and other untouched packages; the full `go test ./...` was not run (as in W1/W2).
- The pre-session cookie is stateless (item 4 above).
- Sessions per user are unbounded (R8); the purge scans without an index (R6); effective idle window up to the throttle shorter (R4); rate limits are per process (R7).
- Binary tests assume loopback ports and the disposable PostgreSQL harness; the `postgres:17` container `3e6f2a555b2a` left by an interrupted earlier run was not stopped (the permission was denied).
