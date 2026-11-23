# Task 07H — W8 user migration: implementation report

**Status:** implemented (stages S1–S4 of [`07H-user-migration-plan.md`](07H-user-migration-plan.md)), committed locally on `refactor/0.8.0-unify-and-new-storage`, **not pushed**, awaiting code review. Task 07I and W9 are not started.

**Not done and not claimed:** no production database was contacted; nothing was run against real legacy data; the owner's aggregate password-hash audit (plan D1) and the confirmation of the PHP time zone (D2) are still prerequisites of the *final production* migration. The behaviour below is proven only on disposable PostgreSQL 17 with synthetic data and the PHP-generated fixtures already in the repository.

## 1. What was built

| Area | Where |
|---|---|
| Migrator package | `internal/migration/users/` — `source.go` (read-only snapshot, schema fingerprint, isolation evidence), `validate.go` (analysis, hash categories, `ConstraintCoverage`), `plan.go` (expected import, target read, reconciliation, classification), `run.go` (config, dry-run, final transaction, import, `--replace-users`, plan digest, verify), `report.go` (error classes, secret-free report), `hasher.go` |
| CLI | `opflags/opflags.go` (`UsersOptions`, `UsersVerifyOptions`), `opflags/parsers.go`, `opflags/config.go` (target DSN fan-out, the separate users source DSN: key `migrateUsersSourceDatabaseURL`, variable `MAGNETICO_MIGRATION_USERS_SOURCE_DSN`, no flag), `main.go` (`runMigrateUsers`, `runMigrateUsersVerify`) |
| Two small hooks in the web packages | `accounts.TokenDigest` (the exact function authentication uses, exported rather than re-implemented), `webschema.VerifyIn` (the existing structural check, callable inside a caller's transaction). No schema, DDL, grant or behaviour change |
| Owner audit | `docs/wip/07H-audit.sql` — read-only, aggregate-only; its per-account `category` is tested equal to the Go classifier |
| Docs | `docs/web-deployment.md` §10 (operator reference), `docs/specs/web-v1.md` §14.8 (additive implementation record, no contract change), plan status, `docs/wip/README.md` |

Commands: `magnetico migrate users [--dry-run] …` and `magnetico migrate users verify …` (flags in `--help` and `web-deployment.md` §10).

## 2. Behaviour as built (what a reviewer should check)

- **One target transaction.** Import, optional `--replace-users` deletion, identity advance and reconciliation commit together or not at all. No run-state table, no checkpoints.
- **Sequences.** The code never calls `nextval()` or `setval()`. Users and invitations are inserted with their legacy ids; RSS token ids are assigned explicitly, consecutive from the identity's current next value (read from the sequence relation under the table locks); the three identities are advanced with `ALTER TABLE … ALTER COLUMN id RESTART WITH n`, `n = max(max id + 1, next value before)`, so an identity never moves backward. Tests record `last_value`/`is_called` of all identities and assert them **byte-identical** after a dry-run, after an injected failure at every stage (`after-users`, `after-invites`, `after-tokens`, `after-restart`, `before-drift-check`), after a real constraint violation, after a cancelled context, after a failed `--replace-users` (at `after-delete`, `after-users`, `after-restart`), and after a source-drift abort; and exact (`10 / 13 / 5`) after success.
- **Dry-run** runs in a `READ ONLY` target transaction (also `default_transaction_read_only=on` on the connection), takes no table locks and makes no sequence call. It validates every row in Go against the version-1 constraints; `ConstraintCoverage` plus a test make a new schema constraint without a replica fail the build. A role holding only `SELECT` runs it successfully; the same role is refused by the final run (`privileges`).
- **Final run** requires `--source-writes-stopped` (an operator assertion), takes a transaction advisory lock and `ACCESS EXCLUSIVE` locks on the six user-owned tables with `lock_timeout 5s`, verifies the exact web schema structure and the role's privileges (including table ownership, needed for the identity `RESTART`), classifies the target (`empty` / `identical` / `replace` / `diverged`), imports, reconciles in the transaction, re-reads the source as a **defence-in-depth** drift check, then commits.
- **Plan digest** binds the source digest, the IANA zone, the target state class, the destructive operation, `--accept-needs-reset`, the authorised unmatched ids, and (with `--replace-users`) the matched ids, per-table counts and the row digest of everything to be deleted. `--dry-run` (with or without `--replace-users`) never requires `--confirm-plan`; it prints the digest and fully evaluates `--delete-unmatched-user-ids` (an exact match is accepted, a larger or smaller list is a refusal). A real `--replace-users` run requires the digest.
- **Password categories** (verified / unvetted / unsupported) use `password.Classify`; `VettedFamilies` is asserted equal to the families in `fixtures.json`. A final run refuses while any account is *unvetted*; *unsupported* accounts need `--accept-needs-reset N` with the exact count.
- **Secret safety.** Reports, summaries and errors carry counts, ids, codes and digests only. PostgreSQL errors are reduced to SQLSTATE and constraint name; other errors to their Go type. The source DSN has no flag, never falls back to the target or the torrent source, and is never printed. `--report-json` is mode 0600 and created exclusively.

## 3. Deliberate refinements of the approved text

1. The plan's step 1 listed `--confirm-plan` without `--replace-users` as a conflict while §4.3 allowed it. Implemented per §4.3 (optional, checked like any confirmation); the plan text was corrected.
2. A dry-run exits **1** when a final run would refuse (reasons in `would_refuse`), so an operator gets a clear signal; `source-writes-not-asserted` is not a dry-run reason.
3. The privilege check lives in the migration package (`checkPrivileges`), not in `webschema/privileges.go`, so the web runtime's documented grants are untouched. The dry-run also needs `SELECT` on the three identity sequences (read via the sequence relation).
4. An argon2id hash at the application's *current* parameters but without PHP vectors is categorised *unvetted* (the vetted list is the PHP fixture families only).
5. Added beyond the plan: `docs/wip/07H-audit.sql` ready to run (with the verified/unvetted/unsupported category and a data-shape query), tested against the Go classifier; `migrate users verify`.
6. Test seams: an unexported `testHook` (nil in production) injects failures inside the target transaction, and `lockTimeout` is a variable so the lock-timeout test does not wait five seconds.

## 4. Verification (commands actually run)

```text
go build ./... && go vet ./...                                   # clean
gofmt -l .                                                       # clean
go test ./... -count=1                                           # exit 0 (whole repository; root package 108 s)
go test -race ./internal/migration/users -count=1                # ok
go test ./internal/migration/users -count=1 -v                   # 0 failed, 0 skipped
```

Integration tests need the disposable PostgreSQL harness (`internal/pgtest`, Docker `postgres:17`); they skip with a message when it is unavailable, so a green run on a machine without it proves nothing.

| Stage | Tests (package `internal/migration/users` unless noted) |
|---|---|
| S1 source | fingerprint accepts the replayed Doctrine schema; refuses an extra table, extra column, changed type, missing/unknown Doctrine version, missing Doctrine table, missing reset table, missing `users` schema; digest independent of insertion order; every blocking and warning finding; verified/unvetted/unsupported categories; `VettedFamilies` = fixtures; audit SQL ≡ Go classifier, aggregate-only output, no write |
| S2 import | ids/relationships/usernames/emails/hashes verbatim, `role user`, unverified, DST-correct conversions (winter and summer), invitation `used_at` NULL and owner `created_at`, legacy tokens (sha256, `legacy`, order and ids), zero `api_tokens`/sessions/links, identity next values `10/13/5`, unrelated tables untouched, source unchanged, `verify` exact, a wrong zone detected; dry-run writes nothing (also as a `SELECT`-only role); sequence state across every failure mode; RSS ids start at the current identity; needs-reset exact acknowledgement; unvetted refusal; malformed source data (orphans, duplicates, empty values, bad codes) all reported, nothing written, no data in messages; empty source; 51-token user and non-hex key imported with warnings; select-only source role; source and target in one database; ownership refusal; lock timeout; write-isolation assertion required |
| S3 rerun/replace | idempotent rerun (`already-migrated`); populated target refused; replace discards exactly the expected counts, requires and checks the digest, is atomic under injected failures, never moves identities backward, leaves trackers/settings/`web_version`/torrent data and the source untouched; unmatched-account protection (same id with a different username counts as unmatched; exact list only); digest changes with zone, scope and operation; source change after the preview invalidates the confirmation; source change during the run rolls back |
| S4 integration | login with each of the six verified PHP families succeeds and rehashes to current argon2id, a wrong password fails; needs-reset account fails generically and the admin link works; `Bob` ≠ `bob`; legacy tokens authenticate on the RSS path and are rejected by the API path, listed masked; disable/enable leaves the token row byte-identical and suspends/restores a migrated invitation; a migrated invitation registers a plain `user`, a used one cannot; admin reset revokes the legacy tokens; migrated emails are unverified, unusable for recovery, and the case-variant duplicate cannot be verified twice; **no secret** in any report/error/target token table (`TestNoSecretsInReportsErrorsOrTargetTokens`). Real binary (`main_users_test.go`): refusals, dry-run digest, confirmed final run, 0600 exclusive report, `verify`, repeat no-op, then the real `magnetico web` accepts the migrated passwords (rehash observed in the database), serves the v1 RSS feed to legacy tokens by query, header and cookie, and answers 401 to the same tokens on `/api/v2`; output scanned for every credential |
| CLI (`opflags/users_test.go`) | defaults and flags; no flag exists for the source DSN; source from environment/file only, never from the torrent source or the target; nested `verify` flag transfer; unknown `MAGNETICO_MIGRATION_*` variables still rejected |

## 5. Known limitations and review pointers

- **Not exercised on real legacy data.** The legacy schema is the five Doctrine migrations replayed verbatim; production may differ (the fingerprint refuses anything that does).
- **Local wall-clock times** that do not exist or occur twice at a DST change convert with Go's `time.Date` rules (deterministic, covered by the digest); the owner should check the zone with the audit window in mind.
- **Write isolation is operational.** The tool reports evidence and re-reads the source, but cannot prove the legacy application or the web service is stopped (documented in `web-deployment.md` §10 and the plan's cutover checklist).
- A user with more than 50 legacy tokens keeps all of them and cannot create a new RSS token until enough are revoked (plan D5).
- Case-insensitive duplicate emails are both imported unverified; only the first to verify holds the address.
- The audit SQL's category is an advisory re-implementation; the migrator's dry-run is authoritative.
- Dry-run replicas of the constraints cannot prove the database accepts every row; a final run can still fail on a constraint the replicas missed, and then rolls back completely.

## 6. Owner actions before production

1. Run `docs/wip/07H-audit.sql` against the legacy database; if any account is *unvetted*, generate PHP vectors for those exact parameters (`internal/web/password/testdata/php/generate.php`) in a reviewed change; decide the *unsupported* accounts' reset plan.
2. Confirm the PHP runtime time zone and pass it as `--source-timezone`.
3. Rehearse on copies (plan §9) before the offline final run.
