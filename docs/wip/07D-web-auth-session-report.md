# Task 07D / W3 — status report (stage S1: store layer)

**Status:** steps 1–3 of [`07D-web-auth-session-plan.md`](07D-web-auth-session-plan.md) are implemented and committed locally; stopped at **S1** for owner review. Steps 4–6 (HTTP middleware, CSRF handlers, login/logout pages, binary tests) are **not started**. Nothing is pushed.

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

## Known limitations / notes

- With the hash guard removed, the mutated run also hit the 600 s `go test` timeout (a race test waits for a blocked statement that no longer blocks). It fails loudly on a regression, but not quickly; the tests are correct on the real code.
- Not run: `dht`, `crawler`, `e2e` and other untouched packages, and the full `go test ./...` and root-package binary tests (nothing in the root package changed in this stage).
- The idle window of an ordinary session is up to `throttle` (≤ 10 %, ≤ 5 min) shorter than configured (R4, approved).
- The sessions purge scans without an index (R6, documented in the plan).
- A leftover `postgres:17` test container (`recursing_sinoussi`) from an earlier interrupted run was still running; stopping it was denied by the permission classifier, so it is left for the owner to stop.
