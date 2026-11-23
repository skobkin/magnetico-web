# Task 07H — W8 user migration: design plan

**Status:** the design was approved by the owner (2026-11-24), including the corrections on sequence semantics, write isolation, password-compatibility categories and plan-digest binding. It is **implemented** as W8 (stages S1–S4); the implementation record and the deliberate refinements of this text are in [`07H-user-migration-report.md`](07H-user-migration-report.md). Task 07I and W9 are separate assignments.

Inputs: `master` PHP entities, Doctrine migrations and `security.yaml`/`doctrine.yaml` history; `internal/web/{webschema,password,accounts}`; `opflags`, `main.go`, `internal/migration/pgidentity`; `docs/specs/web-v1.md` §§4, 5, 12, 14; `07A` §§1.4, 3.3, 3.10, 3.13.

Status labels used throughout: **[APPROVED]** (owner decision already recorded in 07A/web-v1, or in the 07H additional decisions), **[PROPOSAL]** (new in this document, needs approval), **[DECISION]** (open owner decision or blocker).

## 0. Findings that shape the design (from the investigation)

1. **Legacy schema** (`users`, final Doctrine version `Version20250701161732`):
   - `users(id int identity, username varchar(25) uniq, password text not null, email varchar(254) uniq not null, roles json, created_at timestamp(0) without time zone)`
   - `invites(id int identity, user_id int not null FK, used_by_id int null FK, code varchar(32) uniq)`, with no creation or use time
   - `api_tokens(key varchar(32) PK, user_id FK, created_at timestamp(0))`
   - `password_reset_tokens(code, user_id, valid_until)`, which is not migrated.
   - Uniqueness is **exact-case** on username and on email.
2. **Timestamps carry no zone.** They are written by PHP `DateTime` in the PHP runtime's default zone. The target uses `timestamptz`, so the source zone must be **stated by the operator**. It cannot be derived from the data.
3. **Hash history** (from `git log -p` of `security.yaml`):
   - `argon2i` m=16384,t=2 (2018-06 → 2020-01)
   - `auto` (2020-01 → 2022-07; bcrypt `$2y$` or argon2id)
   - `sodium` (2022-07 →; argon2id m=65536,t=4,p=1)
   - There is no `upgradePassword`, so all three eras may still be present.
4. **Three password-compatibility categories.** These must not be conflated.
   - **(a) Verified.** The format and its exact parameters have independently generated PHP vectors in `password/testdata/php/fixtures.json`:
     - argon2i m=16384,t=2,p=4
     - argon2id m=65536 with t=4,p=1, t=2,p=1 or t=3,p=2
     - `$2y$` cost 10 and 13
   - **(b) Parsed but unvetted.** `password.Classify` returns `legacy` (login works and is rehashed) for **any** parameters within its bounds, not only the vector sets. The bounds are:
     - argon2i/argon2id `v=19`, m ≤ 65536 KiB, t ≤ 10, p ≤ 16, salt 8–64 B, key 16–64 B
     - `$2y$` cost 4–16

     So a hash such as `$2y$12$…` or argon2id m=32768,t=3,p=1 is accepted by the current code, but there are no exact PHP vectors for it.
   - **(c) Unsupported.** Everything else, for which `Classify` returns `needs-reset`:
     - out-of-bounds parameters
     - `v≠19`
     - `$2a$`/`$2b$`/`$2x$`
     - empty or unrecognised strings
5. **Legacy `POST /api/v1/login` minted a new token on every API login.** Some users may therefore own **many** tokens, possibly more than the 50-unrevoked cap (`accounts.MaxActiveTokens`). That cap is enforced only on creation, not by the database.
6. **Target constraints that legacy data could violate:**
   - `users_username_length` 1–25, `users_email_length` 3–254, `users_password_hash_nonempty`
   - `invites_code_format ^[A-Za-z0-9_-]{22,64}$` (legacy md5 hex passes)
   - `rss_tokens_token_hash_len = 32`
   - `users.id`/`invites.id` are `GENERATED ALWAYS`, which requires `OVERRIDING SYSTEM VALUE`.
7. **Privileges.** No runtime or operator role has `DELETE` on `users`, `invites`, `api_tokens` or `rss_tokens` (`webschema/privileges.go`, `web-deployment.md` §4). `--replace-users` and the identity `RESTART` need a migration-capable role (D9).
8. **The legacy users DB is configured separately** (`APP_DATABASE_URL`) from the magneticod torrent DB (`MAGNETICOD_DATABASE_URL`). It may or may not be the same PostgreSQL database.
9. **Sequences are not transactional.**
   - `setval()` and `nextval()` are never rolled back (PostgreSQL docs).
   - `ALTER SEQUENCE … RESTART` and the identity form `ALTER TABLE … ALTER COLUMN id RESTART WITH n` **are** transactional, but they require ownership of the table or sequence.
   - Every sequence adjustment in this design therefore uses the identity `RESTART` form, and nothing calls `nextval()`.
10. **The torrent migrator's infrastructure** (run-state schema `magnetico_migration`, checkpoints, workers, the incident registry) is designed for very large resumable imports. None of it is needed for about 30 rows. Reused: config and DSN conventions, `redactDatabaseURL`/`redactSecrets`, `pgidentity`, `pgtest`.

## 1. Field mapping

**`users.users` → `magnetico_web.users`**

| Target | Source / rule | Status |
|---|---|---|
| `id` | `id` verbatim, `OVERRIDING SYSTEM VALUE` | APPROVED |
| `username` | verbatim, byte-exact, no case folding or trimming | APPROVED |
| `email` | verbatim | APPROVED (07H) |
| `email_verified_at` | `NULL` for every account | APPROVED (07H) |
| `password_hash` | `password` verbatim, never transformed | APPROVED |
| `active` | `true` | APPROVED (no legacy disable state) |
| `role` | `'user'`; `roles` JSON is read only for an aggregate warning | APPROVED |
| `created_at` | `created_at AT TIME ZONE <source-timezone>` | PROPOSAL (zone: DECISION D2) |
| `password_changed_at`, `last_login_at` | `NULL` | PROPOSAL |

**`users.invites` → `magnetico_web.invites`**

| Target | Source / rule | Status |
|---|---|---|
| `id` | `id` verbatim, `OVERRIDING SYSTEM VALUE` | APPROVED |
| `owner_id` | `user_id` | APPROVED |
| `code` | verbatim plaintext | APPROVED |
| `used_by` | `used_by_id` | APPROVED |
| `used_at` | `NULL` (unknown; no fabricated time) | PROPOSAL, D3 |
| `created_at` | the owner's converted `users.created_at` | APPROVED (web-v1 §5.3 approximation) |
| `expires_at`, `revoked_at` | `NULL` | PROPOSAL |

**`users.api_tokens` → `magnetico_web.rss_tokens`** only, never `api_tokens`:

| Target | Source / rule | Status |
|---|---|---|
| `id` | new surrogate, **assigned explicitly** with `OVERRIDING SYSTEM VALUE` (no `nextval()`), consecutive from the identity's current next value, in deterministic order `(created_at, user_id, token_hash)`; the identity is then advanced with a transactional `RESTART` | APPROVED (surrogate); mechanics PROPOSAL |
| `token_hash` | `sha256(key bytes as stored)`, using the same function as `accounts.authenticate` (exported or shared, never re-implemented) | APPROVED |
| `user_id` | `user_id` | APPROVED |
| `label` | `NULL` (the UI shows "legacy token") | PROPOSAL |
| `created_at` | converted `created_at` | APPROVED |
| `last_used_at`, `expires_at`, `revoked_at` | `NULL` | PROPOSAL |
| `legacy` | `true` | APPROVED |

**Not migrated [APPROVED]:** `password_reset_tokens` (not even read), PHP sessions, remember-me cookies, reCAPTCHA state.

**Not created [APPROVED/PROPOSAL]:** zero `api_tokens`, `sessions` and `one_time_tokens`; no new invitations (no `INVITE_QUOTA` top-up, PROPOSAL); no emails sent.

## 2. CLI and execution sequence [PROPOSAL]

```
magnetico migrate users --source-timezone <IANA> [--dry-run]
      [--accept-needs-reset N] [--replace-users --confirm-plan <digest>
      [--delete-unmatched-user-ids <id,…>]] [--report-json <path>]
magnetico migrate users verify --source-timezone <IANA> [--report-json <path>]
```

**Target.** The target comes from the existing `--database` / `MAGNETICO_MIGRATE_DATABASE` / `migrateDatabaseURL`.

**Source.** The source DSN comes from **config file key `migrateUsersSourceDatabaseURL` or env `MAGNETICO_MIGRATION_USERS_SOURCE_DSN` only**:
- no CLI flag, no default, and no fallback to the target or to the torrent source DSN (DECISION D7)
- DSNs are never printed; they are redacted with the existing helpers.

`--source-timezone` is required and has no default.

**Two kinds of run.**
- **Preflight / dry-run (`--dry-run`).**
  - Allowed during normal legacy operation and at any time before cutover.
  - Read-only on **both** databases: the target transaction is `READ ONLY`, takes no table locks, makes no sequence calls and no temporary objects. It performs **zero persistent writes**, sequence state included.
- **Final migration** (no `--dry-run`).
  - An offline operation. It requires that the PHP application is stopped and that legacy writes are prevented (§9 steps 4–6).
  - The operator must assert this explicitly with `--source-writes-stopped` [PROPOSAL]. Without the flag a real run refuses.
  - The flag is an operator assertion, not a proof. The migrator also reports the evidence it can observe (step 2).

**Sequence of one final run** (the dry-run differences are noted at the end):
1. **Configuration validation.** Reject flag conflicts such as `--delete-unmatched-user-ids` without `--replace-users` (a `--confirm-plan` without `--replace-users` is allowed, as §4.3 says: it is checked like any confirmation).
2. **Connections.**
   - Open the source with `default_transaction_read_only=on`.
   - Open the target.
   - Record `pgidentity` for both. "Same database" is allowed and reported.
   - Report the write-isolation evidence:
     - whether the source database has `default_transaction_read_only` set at database level (`pg_db_role_setting`)
     - the number of other client backends connected to the source database (`pg_stat_activity`; details only where the role may see them).
   - This is informational. It cannot prove that no writer remains.
3. **Source snapshot.**
   - Start `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY`.
   - Check the source fingerprint (§4.1).
   - Read all three tables in deterministic order (users by `id`, invites by `id`, tokens by `(created_at, user_id, key)`).
   - Compute the source digest.
4. **Validation** of the in-memory rows (§4.2). Collect all findings, then stop on any blocking finding.
5. **Target transaction.**
   - `BEGIN`, `SET LOCAL lock_timeout='5s'`, `search_path=pg_catalog`.
   - Take a transaction advisory lock (new key `…189`).
   - Run `webschema` structural verification and the migration-privilege check.
   - Take `LOCK TABLE` in `ACCESS EXCLUSIVE` on `users`, `invites`, `api_tokens`, `rss_tokens`, `sessions` and `one_time_tokens`.
     - The locks serialise against conflicting database operations for the duration of the transaction, and `lock_timeout` makes a busy table fail fast.
     - They do **not** prove that the Go web service is stopped: an idle service holds no locks and resumes writing after commit.
     - Stopping the web service is an operator check (§9 step 6).
6. **Target state classification** (§5.2):
   - `empty` → import.
   - `identical` → no-op, exit 0.
   - `replace` → delete (§5.3), then import.
   - otherwise → refuse.
7. **Import.**
   - Insert users, then invites, then rss_tokens. Every row is inserted with an explicit id (`INSERT … OVERRIDING SYSTEM VALUE`), so `nextval()` is never called.
   - The rss_token ids start at `N0`, the identity's current next value. `N0` is read from the sequence (`last_value`, `is_called`) under the table lock.
   - Then advance each of the three identity columns with `ALTER TABLE magnetico_web.<t> ALTER COLUMN id RESTART WITH <n>`. This is transactional, so it is rolled back with everything else on failure.
   - `n = GREATEST(max id now in the table + 1, the identity's current next value)`. A sequence never moves backward.
   - `setval()` is not used.
8. **In-transaction reconciliation** (§6), including the next value of each identity. Any mismatch → `ROLLBACK`.
9. **Source drift check** (defence in depth). Re-read the source fingerprint and digest in a **fresh** read-only transaction. If they differ → `ROLLBACK` (`source-changed`).
   - This detects writes that happened before this point.
   - It does **not** make the two databases an atomic snapshot: a legacy write committed between this check and the target commit would go unnoticed.
   - Write isolation of the source is therefore an operational prerequisite of the final run, not something the migrator guarantees.
10. **Commit**, then write the report.

**`--dry-run` instead does:**
- steps 1–4 and the read-only parts of 5–6: structural verification, privilege check, state classification and the deletion set for `replace`, all in a `READ ONLY` target transaction without table locks
- **explicit validation** of every source row against the version-1 target constraints, replicated in Go:
  - length, format and non-empty checks
  - username uniqueness
  - invite code format and uniqueness
  - FK closure
  - token-digest uniqueness
  - id/sequence arithmetic computed, not applied

  The replicas are sound because the structural verification has just proven that the target is exactly version 1, and a test asserts that every version-1 `CHECK`/`UNIQUE`/FK constraint has a replica.
- the plan digest.

Temporary tables were considered and rejected: PostgreSQL forbids `CREATE` in a `READ ONLY` transaction, and the replicas suffice for about 30 rows. A real run can therefore still fail at a constraint the replicas missed. It then rolls back completely (§5.1). Before a target exists, the owner uses the aggregate audit of §3.

## 3. Prerequisites and owner decisions

**Blockers before implementation or cutover**

- **D1 — Password-hash audit [DECISION, owner runs it].** Read-only and aggregate-only, run with `psql` against the legacy DB:

  ```sql
  BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
  WITH c AS (
    SELECT password AS p, CASE
      WHEN password ~ '^\$argon2(i|id|d)\$v=[0-9]+\$m=[0-9]+,t=[0-9]+,p=[0-9]+\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$' THEN 'argon2'
      WHEN password ~ '^\$2[abxy]\$[0-9]{2}\$[./A-Za-z0-9]{53}$' THEN 'bcrypt'
      WHEN password = '' THEN 'empty'
      ELSE 'unrecognized' END AS family
    FROM users.users)
  SELECT family,
    CASE family WHEN 'argon2' THEN split_part(p,'$',2) WHEN 'bcrypt' THEN substr(p,2,2) END AS variant,
    CASE family WHEN 'argon2' THEN split_part(p,'$',3) END AS version,
    CASE family WHEN 'argon2' THEN split_part(p,'$',4) WHEN 'bcrypt' THEN 'cost=' || substr(p,5,2) END AS params,
    CASE family WHEN 'argon2' THEN length(split_part(p,'$',5)) END AS salt_b64_len,
    CASE family WHEN 'argon2' THEN length(split_part(p,'$',6)) END AS key_b64_len,
    CASE family WHEN 'unrecognized' THEN (p LIKE '$%') END AS starts_with_dollar,
    CASE family WHEN 'unrecognized' THEN width_bucket(length(p), 0, 256, 8) END AS length_bucket,
    count(*) AS accounts
  FROM c GROUP BY 1,2,3,4,5,6,7,8 ORDER BY accounts DESC, 1,2,3,4;
  ROLLBACK;
  ```

  - **Output:** algorithm, variant, version, parameters, encoded lengths and counts. It contains no hash, no salt, no username, email or token.
  - **Optional aggregate shape query:** user/invite/token totals; used vs unused invites; max and p95 tokens per user; number of users above 50 tokens; number of case-insensitive email duplicate groups; number of emails not matching a simple `^[^@\s]+@[^@\s]+$`. All counts only. The full SQL will be written in the document.
  - **Interpretation table** (in the document). Each audit row maps to one of the three categories of §0.4:
    - (a) **verified**: an exact fixture family.
    - (b) **parsed but unvetted**: accepted by the current `Classify`/verifier, but its exact parameters have no PHP vectors.
      - Production compatibility for (b) is **not approved** until independently generated PHP vectors for those exact parameters are added with `generate.php`, in a reviewed change, before cutover.
      - Until then these accounts are reported as `unvetted`. They are never described as verified, and the current code does **not** report them as `needs-reset`.
    - (c) **unsupported** (`needs-reset`).
  - **Implications of an unsupported hash:**
    - The account is `needs-reset` and cannot log in.
    - So it cannot reach the account page to verify its migrated email, so self-service recovery is impossible. Only an **administrator-issued reset link** (delivered out of band) works.
    - Per web-v1 §4.7, completing any reset **permanently revokes that user's legacy RSS tokens**, so their existing RSS URLs stop working.
    - Users with supported hashes keep their legacy RSS URLs across the transparent rehash.
- **D2 — Source timezone [DECISION].** The owner confirms the PHP runtime zone, for example `php -r 'echo date_default_timezone_get();'` in the legacy container. It is passed as `--source-timezone`. A wrong value only shifts displayed creation times, and the shift is correctable with `--replace-users` before cutover.

**Proposals needing approval**

- **D3 — `invites.used_at` [PROPOSAL].** Use `NULL`. The legacy app never recorded the time. The alternative is the registrant's `created_at` as an approximation.
- **D4 — Unsupported hashes [PROPOSAL].** Import verbatim, but require `--accept-needs-reset N`, where N must equal the preflight count exactly. Otherwise refuse. This treats loss of login ability as an explicitly acknowledged outcome, never a silent one.
- **D5 — More than 50 legacy tokens per user [PROPOSAL].** Import **all** of them, as approved ("every legacy token"). The report lists the affected user ids and counts. Consequence: the user cannot create a new RSS token until they revoke enough legacy ones. The cap is not changed.
- **D6 — Unstorable values [PROPOSAL].** Refuse, with a list of record ids. This covers an empty or <3-character email, an empty username, an empty password, an invite code outside the target format, and an empty token key. The migrator applies no transformation. The operator fixes the legacy DB before cutover, or the owner approves a specific mapping later.
- **D7 — Separate source DSN setting [PROPOSAL].** Use `MAGNETICO_MIGRATION_USERS_SOURCE_DSN` / `migrateUsersSourceDatabaseURL`. Reason: the PHP app DB is configured separately from the torrent source.
- **D8 — Unmatched target accounts under `--replace-users` [PROPOSAL]** (§5.3).
- **D9 — Migration role [PROPOSAL].** A final run uses the **owner of the `magnetico_web` tables**, because the transactional identity `RESTART` (an `ALTER TABLE`) requires ownership. It is never run as the runtime or operator role. Privileges used:
  - SELECT/INSERT on `users`, `invites`, `rss_tokens`
  - SELECT on `api_tokens`, `sessions`, `one_time_tokens` and on the three identity sequences
  - with `--replace-users`: DELETE on all six user-owned tables.

  `--dry-run` needs only SELECT. No grant or schema change is proposed.
- **D10 — Unvetted hash formats [PROPOSAL].**
  - The migrator carries the list of vector-backed parameter sets. A test asserts that the list equals the families in `fixtures.json`.
  - A final run refuses with `unvetted-hash-format` while any account is in category (b). The resolution is added PHP vectors, never a flag.
  - The dry-run and the audit report the count.
- **D11 — Write-isolation assertion [PROPOSAL].** `--source-writes-stopped` is required for a final run (§2).
- **D12 — Source role [PROPOSAL].** The source role needs CONNECT, USAGE on schema `users`, SELECT on `users.users`, `users.invites`, `users.api_tokens`, and SELECT on the Doctrine `migration_versions` table. If the role holds INSERT/UPDATE/DELETE on any source table, a warning is reported. All source transactions are `READ ONLY` regardless.

## 4. Preflight and dry-run design

### 4.1 Source fingerprint [PROPOSAL]

- Schema `users` exists with exactly `users`, `invites`, `api_tokens`, `password_reset_tokens`. An extra table → refuse, because it might hold data we would discard.
- Each migrated table has exactly the expected column set, with types and nullability. An extra or missing column → refuse.
- The Doctrine `migration_versions` table (looked up in `public`, then `users`) contains all five known versions and no unknown newer one. A missing table → refuse.
- PK, unique and FK constraints are reported. Their absence is a warning, because the data checks in §4.2 re-establish every invariant themselves.
- The source digest is SHA-256 over a canonical encoding of every migrated column value in deterministic order. It is a one-way value over high-entropy content and is printed only as a digest.

### 4.2 Data validation

All findings collected before deciding.

Blocking findings (stop the run):
- D6 values
- an orphan `invites.user_id`, `used_by_id` or `api_tokens.user_id` (anti-joins, even when FKs exist)
- a duplicate username or token key (would require dropped constraints)
- an empty source (zero users; most likely the wrong DSN)
- a source/target id overflow (cannot occur, int → bigint; asserted anyway)

Acknowledgement required:
- the `needs-reset` count (D4, category (c)), using `password.Classify` (the authoritative classifier, the same one the login path uses).

Blocking for a final run only:
- any category (b) `unvetted` account (D10). The dry-run reports it.

Warnings (reported, never block, data unchanged):
- a username failing `ValidateUsername` (e.g. 1 character, edge whitespace). Imported verbatim; login matches exact bytes.
- an email failing `ValidateEmail`. The user can change it on the account page.
- case-insensitive email duplicate groups. Both are kept unverified; only the first to verify holds the address, and the second verification fails generically (web-v1 §14.3).
- a token key that is not 32 lowercase hex. It is hashed as stored, matching the exact-match legacy lookup.
- users above 50 tokens (D5)
- a user who is `used_by` of more than one invitation
- a self-used invitation
- legacy `roles` other than `["ROLE_USER"]` (count only; no admin is inferred)

Target checks: `webschema` structural verification (exactly version 1), migration privileges, and the state classification (§5.2).

### 4.3 Plan digest

The plan digest is SHA-256 over a canonical, versioned encoding of everything that determines what a run writes or destroys:
- the source digest
- the effective `--source-timezone` (its IANA name), and therefore every converted timestamp
- the target state class (`empty`, `identical`, `replace`)
- the requested destructive operation (`--replace-users` present or absent)
- the effective deletion scope:
  - the sorted ids of matched target users
  - the sorted ids authorised by `--delete-unmatched-user-ids`
  - per-table row counts
  - the row digest of every row that would be deleted
- the `--accept-needs-reset` count.

Any change to one of these produces a different digest and invalidates a previous confirmation. That includes a different timezone, adding or removing an id in `--delete-unmatched-user-ids`, toggling `--replace-users`, and any source or target data change.

**Preview and confirmation:**
- `--dry-run` (with or without `--replace-users`) **never requires** `--confirm-plan`. It generates and prints the digest.
- `--dry-run --replace-users --delete-unmatched-user-ids <ids>` fully evaluates the unmatched-account rule in the preview: an exact match is reported as authorised, and a larger or smaller list is reported as a refusal, as in a real run. The operator can therefore settle the id list before the final run.
- A real `--replace-users` run **requires** `--confirm-plan <digest>` from such a preview with identical options. It recomputes the digest after taking the table locks and refuses with `plan-mismatch` unless the two are equal.
- A real run without `--replace-users` accepts an optional `--confirm-plan` with the same check.

## 5. Transaction, retry and rollback model

### 5.1 Single transaction [APPROVED direction]

- The whole import, any `--replace-users` cleanup, the identity `RESTART`s and reconciliation run in one target transaction.
- Any error, a cancelled context (SIGINT/SIGTERM) or a lost connection rolls back completely.
- There is no run-state table, no checkpoint and no resumability. About 30 users × a few rows each finishes in well under a second.

**Account records and sequences are separate guarantees.**
- Account records (rows) are protected by the transaction.
- Sequence state is protected **only because** this design never calls `setval()` or `nextval()`: all ids are explicit, and identity changes use the transactional `RESTART`. A failed or interrupted final run, or any dry-run, therefore leaves every identity's next value unchanged, and there are no gaps.
- This is a property of the design, not of transactions in general. A future change that lets the database generate ids (`DEFAULT`/`nextval`) would leave gaps after a rollback, which is harmless but must then be documented. Tests pin the property (§8).
- Gaps can still arise from concurrent writers on the same identities, and these are prevented by the table locks.

### 5.2 Target classification

Evaluated under the table locks (a dry-run evaluates it in its read-only snapshot).

| State | Condition | Action |
|---|---|---|
| `empty` | zero rows in all six user-owned tables | import |
| `identical` | target equals the expected import exactly (reconciliation of §6 passes, and there are no sessions, no one-time tokens, no `api_tokens`, no non-legacy RSS tokens, no extra rows, all users `role='user'`, `active`, unverified, with the source hash) | no writes; "already migrated", exit 0. This makes a retry safe after an ambiguous commit. |
| anything else, without `--replace-users` | — | refuse with category counts (ids only), no writes |
| anything else, with `--replace-users` | — | §5.3 |

### 5.3 `--replace-users` [APPROVED direction, mechanics PROPOSAL]

- **Scope.** It deletes, in FK order, `sessions` → `one_time_tokens` → `api_tokens` → `rss_tokens` → `invites` → `users`, then imports, all in the same transaction.
- **Never touched:** `magneticod.*`, `magnetico_ingest.*`, `trackers`, `settings`, `web_version`, any other schema, or the source.
- **Protection of independently created accounts.**
  - A target user is **matched** when the source has a user with the same `id` **and** byte-identical `username`.
  - Any **unmatched** target user (for example created with `web user add`) makes the run refuse. The refusal lists only the unmatched ids.
  - The only override is `--delete-unmatched-user-ids <list>`, which must equal the unmatched set **exactly**. A list that is larger or smaller refuses.
- **Explicit intent.**
  - A real run requires `--confirm-plan <digest>` from a prior `--dry-run --replace-users` with the same timezone and deletion scope (§4.3). The preview itself needs no digest.
  - The run prints a warning block before acting. It counts what will be discarded: sessions, API tokens, non-legacy RSS tokens, new invitations, one-time links, accounts whose hash differs from the source (rehashed or changed passwords), admin roles, disabled accounts, verified emails. Discarding these is the purpose of the flag; the digest proves the operator saw exactly this set.
- **Sequences** never move backward (§2 step 7, via a transactional `RESTART`), so ids of deleted target-only accounts and tokens are not reused.
- **Operator prerequisite:** do not create accounts in the target before the final migration. The first administrator is a migrated account promoted with `web user set-role` **after** the final run.

### 5.4 Retry

- After a failed run, the target is unchanged, so the operator reruns the same command.
- After an ambiguous commit (connection lost during COMMIT), a rerun classifies the target as `identical` → no-op.
- After a completed run with a changed source, the operator reruns with `--replace-users` (dry-run first).

### 5.5 Rollback

- *Before the web service opens:* `--replace-users`, or restore of the pre-run `pg_dump -n magnetico_web` backup.
- *After cutover:* there is no automated rollback. The legacy DB is unchanged and retained, so a fallback means re-opening the PHP app, and changes made in the new system are lost. This is documented in the cutover checklist.

## 6. Verification and reconciliation

The same reconciliation runs in-transaction before commit and in `migrate users verify` (read-only on both sides, separate snapshots, exit 0 only on an exact match). Checks:
- user, invite, used-invite and token counts equal the source
- per-user invitation and token counts equal the source
- for every source row:
  - the target row exists with the same id, username, email, hash and converted `created_at`
  - `role='user'`, `active`, `email_verified_at IS NULL`
  - invite `owner_id`/`used_by`/`code`
  - `rss_tokens` row with `token_hash = sha256(key)`, the same owner and `created_at`, `legacy=true`, not revoked
- `api_tokens` count is 0
- FK anti-joins are 0
- each identity sequence's next value is greater than the max id
- no target row is unaccounted for

The report contains counts, ids, classes and digests only.

## 7. Error classification and secret-safe reporting

**Error classes** (stable kebab-case codes, exit status 1; usage errors as in the existing CLI):
- `config`: missing DSN or timezone, conflicting flags
- `source-schema`, `target-schema`, `privileges`
- `source-empty`, `target-not-empty`, `unmatched-target-accounts`, `plan-mismatch`, `needs-reset-unacknowledged`, `unvetted-hash-format`, `source-writes-not-asserted`
- `data-blocked` (with per-record finding codes)
- `lock-timeout` (the web service is probably running)
- `source-changed`
- `verify-failed`
- `storage` (wrapped DB error)

**Secret safety:**
- Reports, logs and errors never contain DSNs, password hashes, token keys or their digests, invitation codes, emails or usernames. Records are identified by numeric id and finding code.
- PostgreSQL errors are reduced to SQLSTATE and constraint name. The `Detail`/`Where` fields, which echo key values such as `Key (code)=(…)`, are dropped.
- `--report-json` gets file mode 0600 and is created exclusively (no overwrite).
- Plaintext token keys live only in process memory for the duration of the run. Invitation codes are stored in plaintext in the target by approved design, and that is the only plaintext secret written.

## 8. Implementation stages (after approval of this design)

| Stage | Content | Packages | Tests |
|---|---|---|---|
| S1 | Source adapter: fingerprint, snapshot read, validation, aggregate classification; legacy fixture DDL replaying the five Doctrine migrations verbatim into a disposable DB | new `internal/migration/users` (`source.go`, `validate.go`, `report.go`); testdata `legacy_users.sql` | fingerprint accept/refuse (extra table/column, missing version); every blocking/warning finding; Classify mapping for each `fixtures.json` family plus unsupported strings |
| S2 | Target writer for an empty target, explicit ids and transactional identity `RESTART`, reconciliation, `verify`, CLI wiring, read-only `--dry-run` with constraint replicas, report | `internal/migration/users` (`target.go`, `verify.go`, `constraints.go`); `opflags` (`UsersOptions`); `main.go`; `webschema` (new privilege requirement, no DDL change); `accounts` (export the token digest function) | ids/relations preserved; tz conversion incl. a DST zone; **sequence state** (`last_value`, `is_called` of all three identities) recorded before and after: unchanged after a dry-run, unchanged after an injected failure after N inserts, a context cancel and a constraint failure; after success the next values are `max+1` (and `web user add` / `invite add` / new RSS token get them); dry-run runs in a `READ ONLY` transaction and the target is byte-identical afterwards; every version-1 constraint has a dry-run replica; a source-side refusal without `--source-writes-stopped` |
| S3 | Rerun classification and `--replace-users` | same | identical → no-op; diverged → refuse; digest mismatch → refuse; unmatched account → refuse; exact `--delete-unmatched-user-ids` → deleted; replace never moves an identity backward and a failed replace leaves rows and sequences unchanged; source written between snapshot and drift check → `source-changed`, target unchanged; `magneticod`, trackers, settings and `web_version` checksums unchanged |
| S4 | Integration with the web service; docs | tests in `accounts`/`web` packages and a real-binary test in `main_*_test.go`; `docs/web-deployment.md`, `web-v1` §14 record, W8 report | login with each supported legacy family → success + rehash to current argon2id (conditional update); needs-reset → generic failure, `web user list` flags it; legacy token on v1 RSS (query/header/cookie) works, rejected by `/api/v2/*`, zero `api_tokens`; disable/enable keeps the legacy RSS URL working (§12.4b case 3); admin reset revokes it; migrated invites register a new user and are suspended while the owner is disabled; source role SELECT-only and source checksums unchanged; secret scan of stdout/stderr/slog/report JSON/error strings against every fixture token, hash, code, email and username; `rss_tokens` contains no raw key bytes |

All tests use `internal/pgtest` disposable PostgreSQL (17+) and synthetic data. Fixture password hashes are reused from `password/testdata/php/fixtures.json`. There is no production access and no DHT traffic.

## 9. Production cutover checklist

1. The owner runs the D1 audit and shape query. D1–D12 are decided. PHP vectors are added for any category (b) parameters.
2. Rehearsal: restore a copy of the legacy `users` schema into a disposable DB. Run `--dry-run`, the real run and `verify` against a disposable target. The owner logs in with their own account and checks one legacy RSS URL.
3. Announce the maintenance window and user communication:
   - everyone logs in again
   - verify your email before relying on self-service reset
   - RSS URLs are unchanged
   - API clients must create new API tokens; `/api/v1/login` no longer exists.
4. **Write isolation of the source (mandatory for the final run).**
   - Stop the PHP app, including workers and cron jobs.
   - Terminate its DB connections.
   - Prevent further legacy writes by **revoking the app role's write privileges or `CONNECT`**. `ALTER DATABASE … SET default_transaction_read_only = on` alone is advisory, because a client can override it per session.
   - Check `pg_stat_activity` (as a privileged role): no application backends remain on the legacy database.
5. Back up the legacy DB and `magnetico_web` (`pg_dump`).
6. **Target writers.**
   - Confirm that the Go web service is stopped (container or process state, not table locks) and that no `web user …` operator command is running.
   - Confirm in `pg_stat_activity` that no web-service backend is connected to the target.
   - Confirm that no target accounts were created (or plan `--delete-unmatched-user-ids`).
7. Run `magnetico migrate users --source-timezone … --dry-run` and review the report and warnings. Note the plan digest, the needs-reset count and the unvetted count (which must be 0).
   - Earlier dry-runs and audits may run during normal operation. Only this one, after steps 4–6, is the basis for the final digest.
8. Run the real command with `--source-writes-stopped`, `--accept-needs-reset N`, and `--replace-users --confirm-plan …` if rehearsal data exists.
9. Run `magnetico migrate users verify` → exact match.
10. `web user set-role <owner> admin`. Start `magnetico web`. The owner logs in; `web user list` shows their hash as `current`.
11. Check the owner's legacy v1 RSS URL (works) and the same token against `/api/v2/torrents` (401).
12. Issue admin reset links to needs-reset users, out of band. Remind them this revokes their legacy RSS URLs.
13. Switch Traefik routing. Keep the legacy DB read-only and retained for the rollback window.

## 10. Risks, trade-offs and exclusions

**Risks:**
- unknown hash distribution (D1)
- a wrong source timezone (correctable pre-cutover)
- users above the 50-token cap
- needs-reset users lose legacy RSS continuity through the mandatory reset
- case-insensitive email collisions resolved first-come at verification
- an operator running against the wrong DSN (mitigated by the fingerprint, the empty-source refusal and the plan digest)
- Traefik query-string logging of RSS tokens (existing deployment note)
- a legacy write slipping in after the drift check if the operator skipped step 4. The drift check is defence in depth only; there is no cross-database atomic snapshot and no distributed transaction.
- unvetted hash parameters delaying cutover until vectors are added (D10)

**Trade-offs:**
- **No resumability:** simplicity over restartability, which is justified at about 30 users.
- **Exact-match acknowledgements** (`--accept-needs-reset N`, `--delete-unmatched-user-ids`, `--confirm-plan`): more operator friction in exchange for no silent loss.
- **`used_at` NULL:** honest over pretty.

**Explicit exclusions:**
- no schema, grant or spec changes
- no migration-state tables
- no PHP sessions, remember-me cookies, reset tokens or reCAPTCHA state
- no admin inference
- no generated tokens, invitations or emails
- no writes to the legacy DB
- no torrent/tracker/settings changes
- no W9 work.

