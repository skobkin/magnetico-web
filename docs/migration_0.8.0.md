# Migrating from the legacy PHP/Symfony version to Magnetico 0.8.0

> **This is a major, partially manual, backward-incompatible migration with a one-way cutover.** The Go-based 0.8.0 replaces the PHP/Symfony web application *and* the old crawler, stores torrents in a new format in a **new** database, and imports accounts with a separate tool. Plan a maintenance window, rehearse on copies, and keep the old databases and backups until you have validated the new system.

This guide is the operator's step-by-step procedure. It describes what the **implemented** commands do; design background is in [`docs/specs/legacy-torrent-migration-v1.md`](specs/legacy-torrent-migration-v1.md) and [`docs/wip/07H-user-migration-plan.md`](wip/07H-user-migration-plan.md). Related guides: [`docs/crawler.md`](crawler.md) (the new crawler and ingest), [`docs/web-deployment.md`](web-deployment.md) (web service configuration, roles, reverse proxy), [`UPGRADE.md`](../UPGRADE.md).

**Honest limits of this guide.** The commands below were developed and tested on disposable PostgreSQL databases with synthetic data, and the torrent migrator was additionally exercised on a 3.46-million-torrent real sample in a lab. **Nothing has been run against your production databases**, no 0.8.0 release or release image exists yet, and the user migration has never seen real legacy accounts. Rehearse first. Where this guide cannot know something about your installation it says so.

Conventions: `UPPER_CASE` words are placeholders you replace. Examples never contain real credentials; connection strings are passed through **environment variables**, not command-line arguments, so they never appear in process listings. The binary is written `magnetico` (use the 0.8.0 release binary, or build the release tag with `go build -o magnetico .`).

Marks: **[R]** reversible (you can undo it and the legacy system is untouched) · **[I]** irreversible or hard to undo · **[X]** point of no return for the *new* system's data (§2).

---

## 1. What changes

### Compatibility-breaking changes

| Area | Legacy (PHP/Symfony 0.7.x) | 0.8.0 |
|---|---|---|
| Runtime | PHP app (FrankenPHP), Doctrine, the legacy Go crawler writing straight into PostgreSQL | One Go binary `magnetico` with `crawl`, `ingest`, `web`, `migrate`, `dev` |
| Torrent database | `magneticod` schema, one `files` row per file | **A new, separate database** in a new compact format (file lists stored as encoded blobs). It is **not** an in-place upgrade of the old schema |
| Crawler | Writes to PostgreSQL | **Has no database.** It delivers over authenticated HTTPS to `ingest` next to PostgreSQL; local SQLite queue ([`docs/crawler.md`](crawler.md)) |
| Accounts | PHP users in schema `users` (own database) | New schema `magnetico_web`; accounts imported by `magnetico migrate users` |
| Passwords | Hashes in the legacy PHP format | Copied unchanged and rehashed on first login; hashes in an unsupported format cannot log in until an administrator issues a reset link |
| Sessions | PHP sessions, remember-me cookie | Not migrated: **every user must sign in again** |
| API tokens | `/api/v1/login` minted tokens; accepted in header, cookie or query | Every legacy token becomes a **legacy RSS token** (RSS feeds only, stored as SHA-256). **No general API tokens are created**; users create new ones on the account page |
| Email | Required, used for reset | Imported **unverified**; self-service recovery works only after the user verifies the address (SMTP is optional and off by default) |
| Roles | `ROLE_*` JSON | `user` / `admin`. Everybody is imported as `user`; you promote the first admin with `web user set-role` |
| Invitations | Codes stored in plaintext | Kept (ids, owners, plaintext codes, used-by); the legacy app never recorded a use time, so `used_at` is empty. No new invitations are created (`MAGNETICO_WEB_INVITE_QUOTA` defaults to 0) |
| URLs | `/auth/login`, `/auth/reset/…`, `/profile/api/…`, `/torrents`, `/api/v1/…` | New routes; **no redirects from legacy UI URLs** (they are 404) |
| Public API | `/api/v1/*` | `/api/v2/*` with Bearer API tokens. Removed: `POST /api/v1/login`, `POST /api/v1/logout`, `GET /api/v1/torrents`, `GET /api/v1/torrents/{id}` |
| RSS | `/api/v1/feed/rss/last?api-token=…` | **Kept (deprecated)** for legacy tokens at the same URL and with the same structure. Intentional change: `page=1` is now the newest 1,000 items, so a client may see up to 1,000 items it has never seen once after cutover (GUIDs are info hashes, so seen items de-duplicate). New feed: `/api/v2/feeds/latest.rss?feed_token=…` |
| Magnet short link | `/magnet/{infohash}` | Unchanged path; trackers now come from the database (seeded once with the historical list) |
| reCAPTCHA | Used on reset and registration | Gone (rate limiting instead; no third-party calls) |

### Discontinued functionality

- Searching by **file path** (removed in 0.8.0; names only), the legacy JSON listing API, the admin-less PHP UI pages and their URLs, `api-token` in the query string for anything but the deprecated RSS feed, reCAPTCHA, Font Awesome/PicoCSS/Redis/Sentry integrations, PHP sessions and remember-me cookies.
- The **legacy automatic database migrations** of the old Go crawler fork are gone: 0.8.0 never upgrades an old schema in place. Torrent data moves only through `migrate torrents` into a fresh database.
- There is no administration UI yet: accounts, invitations and trackers are managed with the `magnetico web user …` and `web tracker …` commands ([`docs/web-deployment.md`](web-deployment.md) §5).

## 2. Overview and what can be undone

| Phase | What happens | Reversibility |
|---|---|---|
| **A. Prepare** (§4) | Build/obtain the release, create the **new** database, `schema-init`, `web-schema-init`, run the audit, rehearse on copies, dry-run the user migration | **[R]** Everything here touches only new or disposable databases; legacy systems keep running |
| **B. Freeze and back up** (§5) | Stop the legacy crawler and PHP app, block legacy writes, back up | **[R]** until you delete anything: re-enable the legacy components and you are back |
| **C. Torrent migration** (§6) | `migrate torrents`, verification, repair | **[R]** The legacy source is read-only; the target can be dropped and recreated. Only `migrate cleanup --execute` (§8.3) is **[I]**, and it is optional |
| **D. User migration** (§7) | `migrate users` in one transaction, `verify`, promote the first admin | **[R]** the legacy database is read-only; the target can be reset with the guarded `--replace-users`, or the database dropped. Administrator-issued password resets are **[I]** (they permanently revoke that user's tokens) |
| **E. Start the new system** (§8.1–8.2) | ingest schema, start `web`, smoke test | **[R]** while nobody but you uses it |
| **F. Cutover** (§8.3–8.4) | Optional cleanup (default: keep the migration schema), start ingest, switch routing, start the new crawler, tell users | **[X]** Once users sign in and the new crawler stores torrents, those changes exist **only** in the new databases. Going back is possible (§11) but loses them |

There is **no incremental or reverse migration**: torrent import needs an empty target, and nothing copies new data back to the legacy databases.

## 3. Prerequisites and decisions

### 3.1 Inventory (find out before you start)

1. **Two legacy databases.** The torrent database (the PHP app's `MAGNETICOD_DATABASE_URL`, schema `magneticod`) and the application database (`APP_DATABASE_URL`, schema `users`). They may be the same PostgreSQL server or even the same database.
2. **Source layout of the torrent database.** The migrator supports the **original magnetico v0.12.0** layout only (unpartitioned `magneticod.files`, `migrations.schema_version = 0`). A database created or upgraded by the `tgragnato` fork (partitioned `files_p*`) is **refused**; do not "fix" it by hand, ask for a decision first.
3. **Legacy users schema.** Exactly the five known Doctrine migrations (`Version2018…` to `Version20250701161732`) and the tables `users`, `invites`, `api_tokens`, `password_reset_tokens`; anything else is refused.
4. **The PHP time zone.** Legacy timestamps carry no zone: run `php -r 'echo date_default_timezone_get();'` inside the legacy PHP container and keep the IANA name (for example `Europe/Berlin`).
5. **Capacity.** The target needs a new PostgreSQL database (version 17 is what the tests use) with the `pg_trgm` extension (created by `schema-init`; the database owner can create it), `fsync=on`, and room for the new data, its indexes and WAL. This guide cannot predict sizes: a rehearsal on a restored copy tells you.
6. **Time.** Measured on a lab sample of 3.46 million torrents: import about 10–15 minutes, `verify-decode` about an hour, `verify-content` about an hour. Production scale and hardware differ; use your rehearsal numbers to size the window.

### 3.2 Database roles

| Role | Used for | Needs |
|---|---|---|
| Target administrator (database owner) | `schema-init`, `web-schema-init`, `ingest-schema-init`, `migrate torrents`, verification, `migrate users` (final run), `migrate cleanup` | owns the new database and the `magnetico_web` tables (the user migration's identity `RESTART` is an `ALTER TABLE`, so it must be the **table owner**) |
| Legacy torrent source | `migrate torrents`, `verify-content`, `retry-errors` | `SELECT` only (the tools never write; a read-only account is strongly recommended) |
| Legacy users source | `migrate users` | `CONNECT`, `USAGE` on schema `users`, `SELECT` on `users.users`, `users.invites`, `users.api_tokens` and the Doctrine `migration_versions` table. A role that can write is reported as a warning |
| Web runtime and operator roles | `magnetico web`, `web user …` | the minimum grants in [`docs/web-deployment.md`](web-deployment.md) §4 |

### 3.3 Decisions the operator must make

| # | Decision | How to decide |
|---|---|---|
| 1 | **Source time zone** for the user migration | §3.1 item 4. A wrong value only shifts displayed creation times and is correctable before cutover |
| 2 | **Password hashes** | Run the audit (§4.4). *Verified* hashes work. *Unvetted* hashes (parsed but no PHP test vectors exist for the exact parameters) **block the final run** until vectors are added in a reviewed code change. *Unsupported* hashes cannot log in: you must acknowledge their exact number (`--accept-needs-reset N`) and issue reset links afterwards |
| 3 | **Data the migrator refuses** | Orphan invitations/tokens, duplicate usernames or token keys, empty usernames, passwords or token keys, emails shorter than 3 characters, invitation codes outside the target format: the run stops and lists record ids. Nothing is transformed or dropped; fix the **legacy** data (on a copy during rehearsal, deliberately in production) or get an approved mapping |
| 4 | **Users with more than 50 legacy tokens** | All are imported (as approved); such a user cannot create a new RSS token until enough are revoked |
| 5 | **Who becomes administrator** | Everyone is imported as `user`; promote yours with `web user set-role` after the import |
| 6 | **SMTP** | Optional. Without it, email recovery is off and an administrator issues reset links. Migrated emails are unverified until their owners verify them |
| 7 | **Public URL and reverse proxy** | `MAGNETICO_WEB_PUBLIC_URL`, trusted proxies, timeouts, query-string masking for RSS URLs ([`docs/web-deployment.md`](web-deployment.md) §§1–3, 9) |
| 8 | **Whether to run `migrate cleanup`** | §8.3: the default is to **keep** `magnetico_migration` in production. Cleanup is optional, belongs to the offline migration-completion phase and is only possible before the new system stores its first torrent |
| 9 | **User communication** | §8.5 |

---

## 4. Phase A — Prepare (no downtime) [R]

### 4.1 Obtain and pin the release

Use the 0.8.0 release binary or image, pinned to the exact version (and digest for images). *No release has been published yet; build the tag yourself until it is.* Run `magnetico --help` and `magnetico migrate --help` once to confirm the subcommands below exist in your build.

### 4.2 Create the new database and initialize the schemas

Create an empty database (a **different database** from both legacy ones: the migrator refuses a target that is the same database as the source, also through aliased connection strings). Then, as the target administrator:

```bash
export MAGNETICO_MIGRATE_DATABASE='postgres://ADMIN:PASSWORD@NEW_DB_HOST:5432/magnetico_new?sslmode=require'

magnetico migrate schema-init        # the torrent schema (creates pg_trgm, tables, indexes); refuses anything non-empty/legacy
magnetico migrate web-schema-init    # the magnetico_web schema + the historical tracker list (once)
```

Both are non-destructive and idempotent; a database in an unexpected state is refused and left untouched. Create the web runtime and operator roles and grants from [`docs/web-deployment.md`](web-deployment.md) §4 now or before starting the web service. **Do not initialize or start the ingest service yet** (it must wait for the finished torrent import, §8).

### 4.3 Configure the migration connections

```bash
# the legacy torrent database: read-only account, separate variable per source
export MAGNETICO_MIGRATION_SOURCE_DSN='postgres://RO_USER:PASSWORD@LEGACY_DB_HOST:5432/LEGACY_TORRENT_DB?sslmode=require'
# the legacy PHP application database (schema "users"): read-only account
export MAGNETICO_MIGRATION_USERS_SOURCE_DSN='postgres://RO_USER:PASSWORD@LEGACY_DB_HOST:5432/LEGACY_APP_DB?sslmode=require'
```

The source settings have **no command-line flags** (so they cannot appear in `ps`), have **no default**, and never fall back to the target. The two sources are separate settings: the user migration never reads `MAGNETICO_MIGRATION_SOURCE_DSN`. Config-file keys exist (`migrateDatabaseURL`, `migrateSourceDatabaseURL`, `migrateUsersSourceDatabaseURL`, via `--config-file-path`); precedence is command line > environment > file. Unknown `MAGNETICO_MIGRATION_*` variables are rejected, which catches typos.

### 4.4 Audit the legacy accounts (read-only)

```bash
psql "$LEGACY_APP_DSN" -X -f docs/wip/07H-audit.sql      # LEGACY_APP_DSN: a psql connection string for the legacy app database
```

It runs in a read-only transaction and prints only aggregates: password-hash formats and parameters with counts, and shape statistics (totals, tokens per user, duplicate emails). It never prints a hash, username, email, token or invitation code. Interpret the categories:

- **verified**: PHP-vector-backed parameters. Nothing to do.
- **unvetted**: parses, but there are no PHP vectors for these exact parameters. The final migration **refuses**. Resolution: add vectors with `internal/web/password/testdata/php/generate.php` in a reviewed change (a code change, not a flag). Tell the project owner before the window.
- **unsupported**: these accounts cannot log in after the import. Count them: that number is your `--accept-needs-reset N`.
- **empty**: not a hash at all; the migration refuses it (data fix).

The migrator's own `--dry-run` (§4.6) is authoritative; the SQL is an advisory early look.

### 4.5 Rehearse on copies

Restore the latest backups of the legacy databases into **disposable** databases and run the complete procedure of §§6–7 against them and a disposable target (create it like §4.2). A rehearsal gives you the real timings, the real warnings and refusals, and exercises the exact commands. Never rehearse into the real target database: user and torrent migrations expect to start from an empty one. If you do pollute a target, drop and recreate it.

### 4.6 Dry-run the user migration against the live legacy database (optional but recommended) [R]

`--dry-run` is read-only on both databases (no table locks, no sequence calls) and may run during normal operation:

```bash
magnetico migrate users --source-timezone ZONE --dry-run --report-json /secure/dir/users-preflight.json
```

It checks the legacy schema fingerprint, validates every row against the target constraints, classifies the target, lists warnings (usernames or emails that fail the new validation but are imported verbatim, duplicate emails, users over 50 tokens, …) and prints a **plan digest**. It exits 0 only if a final run would be accepted; otherwise it names the reasons (`would_refuse`) and exits 1. `--report-json` writes a secret-free report (counts, ids, classes, digests) with mode 0600; it never overwrites an existing file. Its source summary line shows `needs_reset=N` and `unvetted=N`: `needs_reset` is the number to pass as `--accept-needs-reset` later, `unvetted` must be 0 for a final run. The dry-run needs the target's `magnetico_web` schema, hence §4.2.

### 4.7 Announce the maintenance window

Tell users (see §8.5) and decide the date. Recommended: the user migration and the torrent migration inside one window, torrent verification at the end of it or after it (the old data is unchanged).

---

## 5. Phase B — Freeze the legacy system and back up [R]

The torrent migrator reads a source that **must not change** during import and verification; the user migrator needs the legacy app stopped and its writes prevented. Operational prerequisites, not things the tools can prove:

1. **Stop the legacy crawler** (the old indexer) and the **PHP application**, including its workers and cron jobs. Put a maintenance page at the proxy.
2. **Prevent legacy writes** to the application database: terminate its connections and revoke the application role's write privileges or `CONNECT`. `ALTER DATABASE … SET default_transaction_read_only = on` alone is only advisory (a session can override it). Check `pg_stat_activity` as a privileged role: no application backends remain.
3. **Back up** both legacy databases (`pg_dump -Fc …`) and verify that the backups restore. Keep them (§12).
4. **Confirm the web service of the new system is not running**, and no `web user …` command is in progress, on the target database.
5. If the target database already holds accounts (it should not), either plan `--delete-unmatched-user-ids` (§7) or recreate it.

## 6. Phase C — Migrate the torrents [R]

### 6.1 Import

```bash
# MAGNETICO_MIGRATE_DATABASE (new, empty, schema-init done) and MAGNETICO_MIGRATION_SOURCE_DSN are exported (§4.2–4.3)
magnetico migrate torrents --defer-secondary-indexes
```

`--defer-secondary-indexes` is optional: it drops the three secondary indexes for the load and rebuilds them at the end (only for an empty target, chosen at the first start and kept on resume). Defaults: `--workers 4`, `--batch-size 256`. Progress is a rolling line on a terminal and a log line every 30 s otherwise.

- The migrator keeps **every torrent id, infohash, name, size, discovery time and complete file list (with duplicates)**; it never renumbers and never writes to the source. Records that cannot be stored under the production validation are recorded as per-record errors in the temporary `magnetico_migration` schema (they are not silently fixed or dropped).
- **Exit status:** `0` the import completed (with or without recorded errors: *completed is not verified*), `2` interrupted by SIGINT/SIGTERM or similar (everything settled is committed: **run the same command again to resume**), `1` fatal (the message says why; the checkpoint stands; fix the cause and rerun).
- An abrupt stop (kill, power loss) is recovered the same way: rerun the same command, including the same `--defer-secondary-indexes` choice. Do not edit the migration tables by hand.

### 6.2 Verify

Run both; they are read-only for the data and persist their verdicts:

```bash
magnetico migrate verify-decode      # target only: every stored torrent is fetched and decoded
magnetico migrate verify-content     # source ↔ target: every legacy torrent equals its migrated counterpart (needs the source DSN)
```

Exit status: `0` pass, `3` completed **with findings**, `2` interrupted (rerun the same command to resume), `1` fatal. `verify-content` is the cutover gate: it proves source/target equality (including duplicate file entries and that no unexpected extra torrents exist). `verify-decode` alone is not equality. One documented blind spot of the target-only scan is reported in its summary (single-file torrents stored without a blob are validated, not compared; `verify-content` does compare them).

### 6.3 Recover from recorded errors

If the import or verification reports record errors:

```bash
magnetico migrate errors export --output /secure/dir/torrent-errors.jsonl --manifest      # forensic JSONL of the registry (metadata only by default)
# optionally, with the source: --include-records [--include-blobs]   (sensitive; written to a file, never to a terminal)
magnetico migrate torrents retry-errors        # re-process only the unresolved records against the source, repairing the target transactionally
```

`retry-errors` exits `0` when none remain unresolved, `3` when some remain, `2` interrupted. It refuses a source that differs from the one imported unless you pass `--accept-source-repairs` (an assertion by you that the source is the same database after a deliberate repair of the legacy data; it is recorded as provenance and forces a fresh `verify-content`). A record that stays invalid stays an error: decide, with the exported evidence, whether the loss is acceptable. Re-run `verify-content` after any retry.

Keep the migration state for now: §8.3 explains why keeping it is the default and when the optional `migrate cleanup` is possible.

## 7. Phase D — Migrate the users [R]

Prerequisites: §5 steps 1–5 done, §4.4 audit reviewed, the time zone known. The final run uses the **owner of the `magnetico_web` tables** as target role.

### 7.1 Final preview

```bash
magnetico migrate users --source-timezone ZONE --dry-run --report-json /secure/dir/users-final-preview.json
```

Review the output: counts, warnings, `needs-reset` accounts N, `unvetted` accounts **must be 0**, and the plan digest. This is the preview that counts, because it is the first one after the legacy system was frozen (earlier dry-runs were informational).

### 7.2 The import

```bash
magnetico migrate users --source-timezone ZONE --source-writes-stopped --accept-needs-reset N --report-json /secure/dir/users-final.json
```

- `--source-writes-stopped` is **your assertion** that the legacy application is stopped and its writes are prevented (§5); the tool reports what it can observe (other backends, a read-only database default) and re-reads the source before committing, but it cannot prove it.
- `--accept-needs-reset N` must equal the dry-run's number exactly (use `0` or omit it when there are none).
- The whole import is **one target transaction**: it either completes or changes nothing, and failed, cancelled or interrupted runs leave even the id sequences untouched. It takes `ACCESS EXCLUSIVE` locks on the user-owned tables with a 5 s lock timeout; these do **not** prove the web service is stopped, you must have checked (§5, step 4).
- Typical refusals (exit 1, nothing written): `source-schema`, `target-schema`, `privileges` (not the table owner), `source-empty` (almost certainly the wrong DSN), `data-blocked` (§3.3 item 3), `unvetted-hash-format`, `needs-reset-unacknowledged`, `target-not-empty`, `source-writes-not-asserted`, `lock-timeout` (the web service or an operator command is running), `source-changed` (the legacy database changed during the run). Output and reports never contain hashes, tokens, codes, emails, usernames or connection strings; records are identified by numeric id.
- **Re-running** after a completed import is a no-op (`already-migrated`) as long as the rows and id sequences are exactly as imported. After an ambiguous failure (for example a lost connection during commit) simply re-run the same command.
- **`--replace-users` is destructive** and only for a target that contains something that must be discarded (rehearsal data, a changed source). It deletes every account and all user-owned data in the target (sessions, one-time links, API and RSS tokens, invitations, users), then imports again, in one transaction, never touching torrents, trackers, settings or schema versions. Accounts that are not in the source (same id **and** username), for example created with `web user add`, make it refuse unless you list exactly those ids with `--delete-unmatched-user-ids`. A real run needs `--confirm-plan DIGEST` from a `--dry-run --replace-users` with **identical** options; any change to the source, target, time zone, id list or acknowledgement changes the digest and invalidates it. Use a fresh/rehearsal-free target instead whenever you can.

### 7.3 Verify and promote the first administrator

```bash
magnetico migrate users verify --source-timezone ZONE --report-json /secure/dir/users-verify.json   # read-only source↔target reconciliation; exit 0 only on an exact match

export MAGNETICO_WEB_DATABASE='postgres://OPERATOR_ROLE:PASSWORD@NEW_DB_HOST:5432/magnetico_new?sslmode=require'
export MAGNETICO_WEB_PUBLIC_URL='https://magnet.example.org'
magnetico web user list                          # states: password current/legacy/needs-reset, emails
magnetico web user set-role YOUR_USERNAME admin
```

`web user list` shows `legacy` for supported hashes (rehashed at first login), `needs-reset` for unsupported ones, and email state `unverified` for everybody.

## 8. Phase E and F — Start the new system and cut over

### 8.1 Initialize the ingest schema [R]

Only after the torrent import is complete and verified:

```bash
magnetico migrate ingest-schema-init
```

This creates the `magnetico_ingest` queue schema and writes no torrents. **Do not start `magnetico ingest` or any crawler yet:** the next steps must happen before the new system stores its first torrent (§8.3).

### 8.2 Start the web service and smoke-test it privately [R]

Start `magnetico web` ([`docs/web-deployment.md`](web-deployment.md) §1: at least `MAGNETICO_WEB_PUBLIC_URL`, `MAGNETICO_WEB_DATABASE`, trusted proxies) **without routing public traffic to it yet** (a private hostname, a port-forward, or the loopback address). Check, as yourself and as a normal migrated user (with their consent) or a rehearsal account:

1. Login with a migrated password works (first success rehashes to the current format: `web user list` then shows `current`).
2. Search a known name; open a torrent with a long file list (both file-browser modes).
3. A legacy RSS URL works unchanged (`/api/v1/feed/rss/last?api-token=LEGACY_TOKEN`) and the same token is rejected (`401`) on `/api/v2/torrents`.
4. `/magnet/{infohash}` redirects.
5. The account page lists the migrated invitations and shows the legacy tokens masked.

### 8.3 Migration state: keep it by default; cleanup is optional and offline [I]

`migrate torrents` leaves an operational schema `magnetico_migration` (progress counters, the error registry, verification verdicts). **The default, and the recommended production policy, is to keep it**, including after ingest starts: it is harmless to the services (ingest only reads its phase), it is forensic evidence, and keeping it costs nothing. Whatever you decide, **preserve the exported migration evidence and the verification reports** next to the backups (§12).

`migrate cleanup` is **optional** and is meant for the **offline migration-completion phase**: after the §6 verification and the §8.2 smoke tests, **before** the new system ingests its first torrent. It has one hard precondition: **the target must still contain exactly the migrated data.** Its reconcile gate (G5) compares the target's torrent and blob counts with the recorded import counters, and it is never waivable, not even by `--force-unverified`. The moment ingest stores a single new torrent next to the migrated ones, cleanup refuses for good (a regression test pins this). There is no post-cutover cleanup. If you do want the schema removed with the tool, do it now:

```bash
magnetico migrate errors export --output /secure/dir/final-export.jsonl --manifest   # complete export + SHA-256 manifest = the evidence cleanup validates
magnetico migrate cleanup                    # default: a non-destructive gate-by-gate preview; read every gate
magnetico migrate cleanup --execute --confirm-backup --confirm-export --evidence-manifest /secure/dir/final-export.jsonl.manifest.json
```

`--execute` drops `magnetico_migration` in one transaction after every gate passes and **destroys the migration history irrecoverably** (the permanent `schema_version` and all torrent and user data stay). It requires an independent backup of the new database (`--confirm-backup`, your attestation) and the complete export with its manifest. Stale or failed verification verdicts and unresolved findings are ordinary gate failures that `--force-unverified --reason "TEXT"` can waive (the reason is recorded); corrupt state, reconciliation failures and missing attestations are never waivable. Otherwise skip cleanup, which is the default: keep the exported evidence next to the backups (§12) and leave the schema in place.

**Then start `magnetico ingest`** (after cleanup if you ran it, otherwise right away) next to PostgreSQL behind Traefik with its shared secret as described in [`docs/crawler.md`](crawler.md) §§2–4 (ingest refuses to start while an import is running or incomplete). Still, nothing has been stored yet: ingest has no records until a crawler delivers some.

### 8.4 Cutover [X]

In this order, with a person ready to roll back (§11) until each step is checked (ingest runs already, §8.3):

1. Switch the proxy/DNS so the public hostname points to the new `magnetico web` (remove the maintenance page).
2. Start the new crawler(s) on their machine ([`docs/crawler.md`](crawler.md) §§5–7). Confirm `send acked` grows in the crawler summary and `ingest summary` shows accepted records.
3. For every `needs-reset` user send an **administrator-issued reset link** out of band: `magnetico web user reset-password USERNAME` prints a one-time link (valid 1 hour for an account that has a password hash, 72 hours when it has none). **Completing a reset signs the user out everywhere and permanently revokes that user's API and RSS tokens, including their legacy RSS URLs** [I].
4. Keep the legacy databases read-only and retained (§12).

### 8.5 What to tell users

- Everyone has to **sign in again**; sessions did not migrate. Passwords carry over, except for the accounts you told you must be reset.
- **Verify your email address** on the account page before relying on password recovery.
- **RSS URLs keep working.** Create a new RSS URL on the account page when convenient: the old (v1) endpoint is deprecated and may eventually be removed. Expect up to 1,000 recent items to look new once.
- **API clients** must create a new API token (account page) and use `/api/v2` with `Authorization: Bearer …`; `/api/v1/login` and the v1 JSON endpoints no longer exist.
- Old bookmarks of the legacy pages may be 404. Search is by name only.

## 9. Post-migration verification checklist

- [ ] `verify-content` passed with no findings (or the findings are understood and recorded), and `verify-decode` passed (both *before* any new torrent was stored; §10).
- [ ] The torrent count of the new database equals the legacy count minus the documented unresolved record errors (`SELECT count(*) FROM magneticod.torrents;` on each; the legacy count is taken from the frozen database).
- [ ] `magnetico migrate users verify` exited 0; the reports from §7 are stored.
- [ ] `magnetico web user list` matches expectations: the number of `needs-reset` accounts equals N, and you can sign in as an administrator.
- [ ] A migrated user with a supported hash signs in; a legacy RSS URL works; the same token fails on `/api/v2`.
- [ ] Search finds known torrents (ASCII and non-ASCII names); a very large torrent opens.
- [ ] `/magnet/{infohash}` redirects; the tracker list is what you expect (`web tracker list`).
- [ ] The reverse proxy omits or masks query strings in its access logs (RSS URLs carry credentials), does not cache `/torrents`, `/api` or feeds, and its timeouts exceed the search write deadline (≥ 90 s; [`docs/web-deployment.md`](web-deployment.md) §3).
- [ ] Ingest accepts records, the crawler's `unauthorized=0`, `queue records=` is small, torrents appear in the UI.
- [ ] Logs contain no secrets (spot-check), SMTP (if configured) delivers a verification email.
- [ ] The backups from §5 and all reports are stored in a place the new system's administrators cannot delete from by accident.

## 10. The migration state after cutover

By default `magnetico_migration` stays in the database (§8.3), also while ingest runs. That is supported and harmless to the services. Once ingest has stored a torrent the migration did not write, `migrate cleanup` refuses (its reconcile gate cannot pass and cannot be waived), and re-running `migrate torrents` or the verifications against that database is no longer meaningful: `verify-content` reports torrents that are not in the legacy source as extra target rows. Keep the schema, the exported evidence and the verification output from before cutover as the record. There is no supported way to remove the schema after ingest has started; do not drop it by hand.

## 11. Rollback options and limitations

| Situation | What you can do | What is lost |
|---|---|---|
| **Before cutover** (anything up to §8.3 step 1) | Stop the new services; drop or recreate the new database; re-enable the legacy crawler and PHP app, restore the application role's write privileges and `CONNECT` | Nothing: the legacy databases were only read |
| **User migration went wrong** (before cutover) | `magnetico migrate users … --replace-users --confirm-plan DIGEST` after a matching dry-run, or restore the pre-run `pg_dump -n magnetico_web` backup, or recreate the target | nothing legacy |
| **After cutover** | Point the proxy back to the legacy PHP app, re-enable the legacy crawler, restore write access on the legacy databases (they are unchanged since the freeze) | Everything created in the new system: torrents found by the new crawler, new registrations, password changes and resets, new tokens and invitations. **There is no reverse or incremental migration**; the new database should be kept (not dropped) in case you want to recover that data by hand |
| **Password reset issued** | None: completing a reset permanently revokes that user's API and RSS tokens | The user's legacy RSS URLs |
| **`migrate cleanup --execute` run** (§8.3) | Restore from the backup you attested (or drop and recreate the target and redo §6) | The migration history and verdicts |

Users who changed their password in the new system must use the **old** password again after a rollback to the legacy app. If you roll back and later retry the migration, start from fresh targets: `migrate torrents` needs an empty database.

## 12. Preserve old backups and data until validation is complete

Do **not** delete, repurpose or "clean up" any of the following until you have explicitly decided that the new system is validated (the period is yours to choose; do not shorten it because the checklist passed):

- the legacy torrent and application databases (keep them read-only and unconnected to any application);
- the `pg_dump` backups of both legacy databases and of the new database taken before each irreversible step;
- the migration reports (`--report-json`), the torrent-error exports and their manifests, the verification output, and the audit output (all secret-free, but store them as you would other operational records);
- the pinned release binary or image that produced the migration.

After the decision, remove old data the way your retention policy requires. The legacy databases contain password hashes and credentials-equivalent tokens: protect them accordingly until they are destroyed.
