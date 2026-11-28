# Migrating from the legacy PHP/Symfony version to Magnetico 0.8.0

> **This is a major, partially manual, backward-incompatible migration with a one-way cutover.** The Go-based 0.8.0
> replaces the PHP/Symfony web application *and* the old crawler, stores torrents in a new format in a **new** database,
> and imports accounts with a separate tool. Plan a maintenance window, rehearse on copies, and keep the old databases
> and backups until you have validated the new system.

This guide is the operator's step-by-step procedure and the authoritative description of the migration;
[`UPGRADE.md`](../UPGRADE.md) only summarises it. Related guides: [`container.md`](container.md) (running the image),
[`web-deployment.md`](web-deployment.md) (web configuration, reverse proxy, administrator commands, optional
restricted roles) and [`crawler.md`](crawler.md) (the new crawler and `ingest`).

**Status.** The procedure was rehearsed with the published `0.8.0-rc1` image on a copy of a real 0.7.x installation: a
10 % consistent sample of its torrent database (3.46 million torrents, 123 million file rows) and its complete
application database. Every step passed, with no record errors
([report](wip/real-data-migration-report-0.8.0-rc1.md)). It has not been run against *your* databases: rehearse on
copies of them first (§4.6).

**Conventions.** `UPPER_CASE` words are placeholders. Examples never contain real credentials. Connection strings live
in files that only you can read, never in command arguments or shell history (§4.3). `magnetico …` stands for the
release binary or for the container wrapper of §4.1.

Marks: **[R]** reversible (the legacy system is untouched and you can start over) · **[I]** irreversible ·
**[X]** point of no return for data created in the new system (§2).

---

## 1. What changes

### Compatibility-breaking changes

| Area | Legacy (PHP/Symfony 0.7.x) | 0.8.0 |
|---|---|---|
| Runtime | PHP application (FrankenPHP) and Doctrine, plus the legacy Go crawler writing straight into PostgreSQL | One Go binary, `magnetico`, with the subcommands `crawl`, `ingest`, `web`, `migrate` and `dev` |
| Torrent database | `magneticod` schema with one `files` row per file | **A new, separate database** in a new compact format (file lists stored as encoded blobs). It is **not** an in-place upgrade of the old schema |
| Crawler | Writes to PostgreSQL | **Has no database.** It keeps a local SQLite queue and delivers over authenticated HTTPS to `ingest`, which runs next to PostgreSQL ([`crawler.md`](crawler.md)). Each crawler has its own credential |
| Accounts | PHP users in schema `users` (their own database) | New schema `magnetico_web`; accounts are imported by `magnetico migrate users` |
| Passwords | Hashes in the legacy PHP format | Copied unchanged and rehashed on first login. Hashes in an unsupported format cannot log in until an administrator issues a reset link |
| Sessions | PHP sessions, remember-me cookie | Not migrated: **every user must sign in again** |
| API tokens | Minted by `/api/v1/login`; accepted in a header, a cookie or the query | Every legacy token becomes a **legacy RSS token** (RSS feeds only, stored as SHA-256). **No general API tokens are created**; users create new ones on the account page |
| Email | Required, used for reset | Imported **unverified**. Self-service recovery works only after the user verifies the address (SMTP is optional and off by default) |
| Roles | `ROLE_*` JSON | `user` / `admin`. Everybody is imported as `user`; you promote the first administrator with `web user set-role` |
| Invitations | Codes stored in plaintext | Kept (ids, owners, plaintext codes, used-by). The legacy app never recorded a use time, so `used_at` is empty. No new invitations are created (`MAGNETICO_WEB_INVITE_QUOTA` defaults to 0) |
| URLs | `/auth/login`, `/auth/reset/…`, `/profile/api/…`, `/torrents`, `/api/v1/…` | New routes; **no redirects from legacy UI URLs** (they answer `404`) |
| Public API | `/api/v1/*` | `/api/v2/*` with Bearer API tokens. Removed: `POST /api/v1/login`, `POST /api/v1/logout`, `GET /api/v1/torrents`, `GET /api/v1/torrents/{id}` |
| RSS | `/api/v1/feed/rss/last?api-token=…` | **Kept (deprecated)** for legacy tokens, at the same URL with the same structure. It is on by default; `MAGNETICO_WEB_RSS_V1_ENABLED=false` switches it off and the path then answers `404`. Intentional change: `page=1` is now the newest 1,000 items, so a client may see up to 1,000 items it has never seen once after cutover (GUIDs are info hashes, so seen items are de-duplicated). New feed: `/api/v2/feeds/latest.rss?feed_token=…` |
| Magnet short link | `/magnet/{infohash}` | Unchanged path. Trackers now come from the database, which is seeded once with the historical list |
| reCAPTCHA | Used on reset and registration | Gone: rate limiting instead, with no third-party calls |
| Configuration | `APP_*`, `MAGNETICOD_DATABASE_URL`, `MAILER_DSN`, … | New `MAGNETICO_*` settings; nothing is reused. The image has no default command and runs as `10001:10001` |

### Discontinued functionality

- **File-path search.** The historical PHP interface only ever searched torrent names, and 0.8.0 does the same. The
  upstream Go `magneticow` UI could search file paths; 0.8.0 does not carry that over.
- The legacy JSON listing API, `api-token` in the query string for anything but the deprecated RSS feed, reCAPTCHA,
  the Font Awesome, PicoCSS, Redis and Sentry integrations, PHP sessions and remember-me cookies, and the old pages and
  their URLs.
- The **automatic database migrations** of the old Go crawler fork. 0.8.0 never upgrades an old schema in place: torrent
  data moves only through `migrate torrents` into a fresh database.
- Account management in the browser. The web service has a **read-only** administrator section (overview, crawlers,
  quarantine, users, invitations, trackers). Accounts, invitations and trackers are changed with the
  `magnetico web user …` and `web tracker …` commands ([`web-deployment.md`](web-deployment.md) §5).

## 2. Phases and what can be undone

| Phase | What happens | Reversibility |
|---|---|---|
| **A. Prepare** (§4) | Obtain the release, create the accounts and the **new** database, initialise its schemas, audit the accounts, rehearse on copies, dry-run the user migration | **[R]** Touches only new or disposable databases; the legacy system keeps running |
| **B. Freeze and back up** (§5) | Stop the legacy crawler and PHP application, block every legacy write, back up | **[R]** Re-enable the legacy components and you are back |
| **C. Import the torrents** (§6) | `migrate torrents`; recover recorded errors | **[R]** The source is only read; the target can be dropped and recreated |
| **D. Verify the torrents** (§7) | `migrate verify-content` | **[R]** Read-only |
| **E. Migrate the users** (§8) | `migrate users` in one transaction, `migrate users verify` at once, then promote the first administrator | **[R]** The legacy database is only read; the target can be reset (§8.2) or recreated |
| **F. Smoke-test privately** (§9) | Initialise the ingest (and optional statistics) schema, start `web` where only you can reach it, test | **[R]** While nobody but you uses it |
| **G. Cut over** (§10) | Start `ingest`, switch public routing, start the new crawlers, issue reset links | **[X]** From now on registrations, password changes and new torrents exist **only** in the new database. Going back is possible (§13) but loses them |
| Rollback (§13) | Return to the legacy system | Depends on the phase |

There is **no incremental or reverse migration**: the torrent import needs an empty target, and nothing copies new data
back to the legacy databases.

**Keep every writer away from the data being migrated.** The migration and its verification compare two databases that
must not change underneath them:

- **Legacy writers.** From the freeze (§5) until the end of the user migration (§8), nothing may write to either legacy
  database: not the legacy crawler, not the PHP application, its workers or cron jobs, and not any script. Afterwards
  keep them read-only for a possible rollback.
- **Target writers.** In 0.8.0 only `magnetico ingest` (fed by the crawlers) and the migration commands write torrents.
  `ingest` refuses to start while an import is running or incomplete, but **it would start as soon as the import is
  complete**: do not start it, and do not register or start crawlers, before §10. Never run `magnetico dev
  seed-torrents` against this database. `web`, the `web user …` commands and the user migration never write torrents.

## 3. Prerequisites and decisions

### 3.1 Inventory

1. **Two legacy databases.** The torrent database (the PHP app's `MAGNETICOD_DATABASE_URL`, schema `magneticod`) and the
   application database (`APP_DATABASE_URL`, schema `users`). They may be on the same PostgreSQL server or even be the
   same database.
2. **Source layout of the torrent database.** The migrator supports the **original magnetico v0.12.0** layout only
   (unpartitioned `magneticod.files`, `migrations.schema_version = 0`). A database created or upgraded by the
   `tgragnato` fork (partitioned `files_p*`) is **refused**; do not "fix" it by hand, ask the project first.
3. **Legacy users schema.** Exactly the five known Doctrine migrations (`Version2018…` to `Version20250701161732`) and the
   tables `users`, `invites`, `api_tokens` and `password_reset_tokens`; anything else is refused.
4. **The PHP time zone.** Legacy timestamps carry no zone. Run `php -r 'echo date_default_timezone_get();'` inside the
   legacy PHP container and keep the IANA name (for example `Europe/Berlin`).
5. **Capacity.** The target needs a new PostgreSQL database (PostgreSQL 17 is what the tests and the rehearsal use) with
   `fsync=on` and room for the data, its indexes and WAL. In the rehearsal the 17 GB legacy sample became 2.8 GB; a
   rehearsal on your own copy tells you your numbers.
6. **Time.** The window is the freeze and backup, the torrent import, `verify-content`, the user migration (seconds)
   and your smoke tests. On the 10 % sample (3.46 million torrents) the import took about 14 minutes and
   `verify-content` about 55 minutes (rehearsal on a laptop; a lab server measured 10–15 minutes and about an hour).
   Both grow roughly with the size of the source. Use your rehearsal numbers.

### 3.2 Database accounts

The default procedure uses **one target account** for everything that touches the new database, and read-only accounts
for the legacy databases.

| Account | Used by | Needs |
|---|---|---|
| **Target account** (for example `magnetico`), owner of the new database | every `migrate` command on the target, `web`, `ingest`, the `web user …`, `web tracker …` and `ingest crawler …` commands | Owning the database. The init commands then create every schema and table as this account, which is what the user import needs (it must run as the owner of the `magnetico_web` tables) |
| **Legacy read-only account** (for example `magnetico_legacy_ro`) | `migrate torrents`, `retry-errors` and `verify-content` (torrent source); `migrate users` and `migrate users verify` (users source) | `SELECT` on the legacy tables (§4.2). The tools never write; an account that can write a legacy users table is reported as a warning |

Separate restricted roles for the web service and the administrator commands are optional hardening for experienced
PostgreSQL administrators ([`web-deployment.md`](web-deployment.md) §4). This guide does not need them.

### 3.3 Decisions

| # | Decision | How to decide |
|---|---|---|
| 1 | **Source time zone** for the user migration | §3.1 item 4. A wrong value only shifts displayed creation times, and it is correctable before cutover |
| 2 | **Password hashes** | Run the audit (§4.5). *Verified* hashes work. *Unvetted* hashes (they parse, but no PHP test vectors exist for the exact parameters) **block the final run** until vectors are added in a reviewed code change. *Unsupported* hashes cannot log in: you acknowledge their exact number (`--accept-needs-reset N`) and issue reset links afterwards |
| 3 | **Data the migrator refuses** | Orphan invitations or tokens, duplicate usernames or token keys, empty usernames, passwords or token keys, emails shorter than 3 characters, invitation codes outside the target format. The run stops and lists record ids. Nothing is transformed or dropped: fix the **legacy** data (on a copy during the rehearsal, deliberately in production) |
| 4 | **Users with more than 50 legacy tokens** | All are imported; such a user cannot create a new RSS token until enough are revoked |
| 5 | **First administrator** | Everyone is imported as `user`; you promote yours after the import has been verified (§8.4) |
| 6 | **SMTP** | Optional. Without it email recovery is off and an administrator issues reset links. Migrated emails stay unverified until their owners verify them |
| 7 | **Public URL and reverse proxy** | `MAGNETICO_WEB_PUBLIC_URL`, trusted proxies, timeouts, and the proxy's access log: keep it off, or drop the request path and all headers, because feed URLs and links carry credentials ([`web-deployment.md`](web-deployment.md) §§1–3) |
| 8 | **Optional `migrate cleanup`** | The default, and the recommendation, is to **keep** the migration state (`magnetico_migration`) and skip cleanup. Cleanup has extra requirements (Appendix B) |
| 9 | **User communication** | §11 |

---

## 4. Phase A — Prepare (no downtime) [R]

### 4.1 Obtain the release and decide how to run it

The release is published as native Linux binaries (amd64, arm64) on the
[Forgejo releases](https://git.skobk.in/skobkin/magnetico-web/releases) and as the image
`docker.io/skobkin/magnetico-web` (also `ghcr.io/skobkin/magnetico-web`, same digest). Run
`magnetico --version`, `magnetico --help` and `magnetico migrate --help` once to confirm the version and the
subcommands.

**With the image**, run every command of this guide as a one-shot container. A shell function keeps the commands short
and keeps credentials out of the arguments:

```sh
# the env files hold the connection strings (§4.3); /srv/magnetico/reports is on durable storage and owned by you
magnetico() {
  docker run --rm --network DB_NETWORK --env-file "${MAGNETICO_ENV_FILE:-/srv/magnetico/migrate.env}" \
    --user "$(id -u):$(id -g)" -v /srv/magnetico/reports:/reports \
    docker.io/skobkin/magnetico-web:0.8.0 "$@"
}
```

- `--network DB_NETWORK` is a Docker network from which the database is reachable (or `--network host`).
- The `migrate` commands use `migrate.env`; the `web user …` commands use `web.env`
  (`MAGNETICO_ENV_FILE=/srv/magnetico/web.env magnetico web user list`).
- With `--user` the reports are written as you, mode `0600`, into a directory you own. Without it, the container user
  `10001` writes them, the directory must be owned by `10001:10001`, and you can read the files only as root or through
  the image (`docker run --rm -v /srv/magnetico/reports:/reports:ro --entrypoint cat IMAGE /reports/FILE`).
- Put the reports directory on **durable storage**: not `/tmp`, which may be a RAM disk that a reboot empties (§14).
- Run the long commands (the import and `verify-content`) inside `tmux` or `screen`, so that a lost SSH session does
  not interrupt them. An interruption is harmless anyway: re-run the same command and it resumes.

More on the image (users, volumes, healthchecks, shutdown, resources): [`container.md`](container.md).

### 4.2 Create the accounts and the new database

On the **new** server, as a PostgreSQL superuser (or a role with `CREATEROLE` and `CREATEDB`), for example in
`psql`:

```sql
CREATE ROLE magnetico LOGIN;
\password magnetico
CREATE DATABASE magnetico OWNER magnetico;
```

It must be a **different database** from both legacy ones (the migrator refuses a target that is the source, also
through aliased connection strings). It may live on the same server. Owning the database is enough for `schema-init` to
create the `pg_trgm` extension.

On the **legacy** server, as a superuser (or the owner of the legacy schemas), create the read-only account. If both
legacy schemas are in one database, run both parts in it:

```sql
CREATE ROLE magnetico_legacy_ro LOGIN;
\password magnetico_legacy_ro
GRANT CONNECT ON DATABASE LEGACY_TORRENT_DB TO magnetico_legacy_ro;
GRANT CONNECT ON DATABASE LEGACY_APP_DB TO magnetico_legacy_ro;

\connect LEGACY_TORRENT_DB
GRANT USAGE ON SCHEMA magneticod TO magnetico_legacy_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA magneticod TO magnetico_legacy_ro;

\connect LEGACY_APP_DB
GRANT USAGE ON SCHEMA users TO magnetico_legacy_ro;
GRANT SELECT ON users.users, users.invites, users.api_tokens, public.migration_versions TO magnetico_legacy_ro;
```

`public.migration_versions` is the Doctrine migrations table of a standard installation; use its actual name if yours
differs. `\password` asks for the password without echo, so it never reaches the shell history.

### 4.3 Store the connection settings

Write the connection strings into files that only you can read (`chmod 600`, never committed):

```sh
# /srv/magnetico/migrate.env: Docker --env-file format, NAME=value with no quotes
MAGNETICO_MIGRATE_DATABASE=postgres://magnetico:PASSWORD@NEW_DB_HOST:5432/magnetico?sslmode=require
MAGNETICO_MIGRATION_SOURCE_DSN=postgres://magnetico_legacy_ro:PASSWORD@LEGACY_DB_HOST:5432/LEGACY_TORRENT_DB?sslmode=require
MAGNETICO_MIGRATION_USERS_SOURCE_DSN=postgres://magnetico_legacy_ro:PASSWORD@LEGACY_DB_HOST:5432/LEGACY_APP_DB?sslmode=require

# /srv/magnetico/web.env: the same target account
MAGNETICO_WEB_DATABASE=postgres://magnetico:PASSWORD@NEW_DB_HOST:5432/magnetico?sslmode=require
MAGNETICO_WEB_PUBLIC_URL=https://magnet.example.org

# /srv/magnetico/ingest.env (needed at the cutover, §10): the same target account
MAGNETICO_INGEST_DATABASE=postgres://magnetico:PASSWORD@NEW_DB_HOST:5432/magnetico?sslmode=require
```

- `MAGNETICO_MIGRATE_DATABASE` is the target. `MAGNETICO_MIGRATION_SOURCE_DSN` is the legacy **torrent** database and
  `MAGNETICO_MIGRATION_USERS_SOURCE_DSN` the legacy **application** database. The two sources are separate settings: the
  user migration never reads the torrent source, and neither ever falls back to the target.
- The source settings have no command-line flags and no defaults. Unknown `MAGNETICO_MIGRATION_*` names are rejected,
  which catches typos. A password with special characters must be percent-encoded in the URL.
- With the native binary, load a file into the environment without typing secrets: `set -a; . /srv/magnetico/migrate.env;
  set +a`. In shell syntax a value that contains `&`, `;` or spaces must be quoted, which the Docker format does not
  allow, so keep a separate file for each tool if you need both.
- Config-file keys also exist (`migrateDatabaseURL`, `migrateSourceDatabaseURL`, `migrateUsersSourceDatabaseURL`, via
  `--config-file-path`); precedence is command line > environment > file.

### 4.4 Initialise the schemas

```sh
magnetico migrate schema-init        # the torrent schema (pg_trgm, tables, indexes); refuses anything non-empty or legacy
magnetico migrate web-schema-init    # the magnetico_web schema and the historical tracker list (seeded once)
```

Both are non-destructive and idempotent; a database in an unexpected state is refused and left untouched. The ingest
and statistics schemas come later (§9.1).

### 4.5 Audit the legacy accounts (read-only)

The audit script is [`legacy-users-audit.sql`](legacy-users-audit.sql). It is not in the image: take it from the
release tag, for example from the GitHub mirror (the Forgejo repository serves the same file in a browser):

```sh
curl -fsSLO https://raw.githubusercontent.com/skobkin/magnetico-web/0.8.0/docs/legacy-users-audit.sql
```

(In `0.8.0-rc1` the same script is `docs/wip/07H-audit.sql`.) Run it as the read-only account against the legacy
**application** database, with a host `psql` or with the PostgreSQL image. The connection comes from libpq variables in
a `chmod 600` file (`PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`, `PGSSLMODE`):

```sh
docker run --rm -i --network DB_NETWORK --env-file /srv/magnetico/legacy-app-psql.env postgres:17 \
  psql -X -f - < legacy-users-audit.sql
```

It runs in a read-only transaction and prints only aggregates: password-hash formats and parameters with counts, and
shape statistics (totals, tokens per user, duplicate emails). It never prints a hash, username, email, token or
invitation code. The categories:

- **verified**: parameters backed by PHP test vectors. Nothing to do.
- **unvetted**: parses, but there are no PHP vectors for these exact parameters. The final migration **refuses**. The
  resolution is a reviewed code change that adds vectors (`internal/web/password/testdata/php/generate.php`); tell the
  project before the window.
- **unsupported**: these accounts cannot log in after the import. Their number is your `--accept-needs-reset N`.
- **empty**: not a hash at all; the migration refuses it (fix the data).

The script is an early look; the migrator's own dry run (§4.7) is **authoritative**.

### 4.6 Rehearse on copies

Restore the latest backups of the legacy databases into **disposable** databases, create a disposable target like
§4.2, and run §§4.4–9 against them. A rehearsal gives you the real timings, warnings and refusals, and exercises the
exact commands. Never rehearse into the real target: both migrations expect an empty one. If you pollute a target, drop
and recreate it.

### 4.7 Dry-run the user migration against the live legacy database (recommended) [R]

`--dry-run` is read-only on both databases (no table locks, no sequence calls) and may run during normal operation:

```sh
magnetico migrate users --source-timezone ZONE --dry-run --report-json /reports/users-preflight.json
```

It checks the legacy schema fingerprint, validates every row against the target constraints, classifies the target,
lists warnings (usernames or emails that fail the new validation but are imported verbatim, duplicate emails, users over
50 tokens, …) and prints a **plan digest**. It exits 0 only if a final run would be accepted; otherwise it names the
reasons (`would_refuse`) and exits 1. Its source summary shows `needs_reset=N` (the number for `--accept-needs-reset`)
and `unvetted=N` (must be 0). `--report-json` writes a secret-free report (counts, ids, classes, digests) with mode
`0600` and never overwrites an existing file.

### 4.8 Announce the maintenance window

Tell users what will change (§11) and when.

---

## 5. Phase B — Freeze the legacy system and back up [R]

The source must not change from now until the user migration is done (§2). The tools cannot prove that; you enforce it:

1. **Stop the legacy crawler** and the **PHP application**, including its workers and cron jobs. Put a maintenance page
   at the proxy.
2. **Block every legacy write** to both legacy databases, as a superuser on the legacy server: revoke `CONNECT` from
   `PUBLIC` and from the legacy application and crawler roles (or make those roles `NOLOGIN`), and terminate their
   sessions. The read-only account keeps its explicit `CONNECT`.

   ```sql
   REVOKE CONNECT ON DATABASE LEGACY_TORRENT_DB FROM PUBLIC, LEGACY_CRAWLER_ROLE;
   REVOKE CONNECT ON DATABASE LEGACY_APP_DB FROM PUBLIC, LEGACY_APP_ROLE;
   SELECT pg_terminate_backend(pid) FROM pg_stat_activity
    WHERE datname IN ('LEGACY_TORRENT_DB', 'LEGACY_APP_DB')
      AND usename <> 'magnetico_legacy_ro' AND pid <> pg_backend_pid();
   ```

   `ALTER DATABASE … SET default_transaction_read_only = on` alone is only advisory: a session can override it. Check
   `pg_stat_activity` afterwards: no legacy backends remain.
3. **Back up** both legacy databases (`pg_dump -Fc …`) and check that the backups restore. Keep them (§14).
4. **Check the target:** no `web`, `ingest` or crawler runs against it, and no `web user …` command is in progress.

## 6. Phase C — Import the torrents [R]

### 6.1 Import

```sh
magnetico migrate torrents --defer-secondary-indexes
```

`--defer-secondary-indexes` is optional: it drops the three secondary indexes for the load and rebuilds them at the end
(only for an empty target; chosen at the first start and kept on resume). Defaults: `--workers 4`, `--batch-size 256`.
Progress is a rolling line on a terminal and a log line every 30 s otherwise.

- The migrator keeps **every torrent id, infohash, name, size, discovery time and complete file list (with
  duplicates)**. It never renumbers and never writes to the source. Records that cannot be stored under the production
  validation are recorded as per-record errors in the migration schema `magnetico_migration`; they are not silently
  fixed or dropped.
- **Exit status:** `0` the import completed, with or without recorded errors (*completed is not verified*); `2`
  interrupted (everything settled is committed: **run the same command again to resume**); `1` fatal (the message says
  why; the checkpoint stands; fix the cause and re-run).
- An abrupt stop (kill, power loss) is recovered the same way: re-run the same command, with the same
  `--defer-secondary-indexes` choice. Do not edit the migration tables by hand.

### 6.2 Recover recorded errors

If the import reported record errors:

```sh
magnetico migrate errors export --output /reports/torrent-errors.jsonl --manifest   # forensic JSONL (metadata only by default)
magnetico migrate torrents retry-errors   # re-process only the unresolved records, repairing the target transactionally
```

The export can include the source records (`--include-records [--include-blobs]`); that output is sensitive and is
written only to the file. `retry-errors` exits `0` when none remain unresolved, `3` when some remain and `2` when
interrupted. It refuses a source that differs from the one imported unless you pass `--accept-source-repairs` (your
assertion that it is the same database after a deliberate repair of the legacy data; it is recorded). A record that
stays invalid stays an error: decide, with the exported evidence, whether the loss is acceptable.

## 7. Phase D — Verify the torrents [R]

```sh
magnetico migrate verify-content
```

This is the **required cutover gate**. It reads both databases, writes nothing to either, and records its verdict in
`magnetico_migration`. Exit status: `0` pass, `3` completed **with findings**, `2` interrupted (re-run the same command to
resume), `1` fatal. A resumed run says so once, before its first progress line: the verification run ID, the last
committed target and source checkpoints and the counts so far (`verify-decode` prints the same notice without the source
side).

- **What it proves.** For every legacy torrent: the same id, infohash, name, total size, discovery time and file count,
  and the complete `(path, size)` list, duplicates included. It decodes every stored file list with the production
  limits, also the single-file torrents stored without a list, and it reports target torrents that are not in the
  source and stored file lists that belong to no torrent.
- **What it assumes.** That neither database changed since the freeze (§2, §5). It cannot detect a change made outside
  the tools, and it says nothing about the accounts (§8 verifies those).
- **One run at a time.** The import, `retry-errors`, the verifications and `cleanup` share one lock on the target. A
  second command started while one runs is refused with exit 1 (`another migration run holds the lock on this target`);
  run them one after the other.
- **Findings (exit 3).** Export the errors (§6.2), investigate, use `retry-errors` for recorded record errors, and run
  `verify-content` again. After a pass do not start `ingest` yet (§10).

`migrate verify-decode` is **not** part of this procedure: `verify-content` already decodes every stored file list and
also compares it with the source. `verify-decode` is a target-only diagnostic (Appendix A), needed only for the optional
cleanup (Appendix B).

## 8. Phase E — Migrate the users [R]

Prerequisites: §5 done, the audit (§4.5) reviewed, the time zone known. `web` is not running.

### 8.1 Final preview

```sh
magnetico migrate users --source-timezone ZONE --dry-run --report-json /reports/users-final-preview.json
```

Review the counts, the warnings, the number of `needs-reset` accounts N, `unvetted` (**must be 0**) and the plan digest.
This is the preview that counts, because it is the first one after the freeze.

### 8.2 The import

```sh
magnetico migrate users --source-timezone ZONE --source-writes-stopped --accept-needs-reset N --report-json /reports/users-final.json
```

- `--source-writes-stopped` is **your assertion** that the legacy application is stopped and its writes are blocked
  (§5). The tool reports what it can observe (other backends, a read-only database default) and re-reads the source
  before committing, but it cannot prove it.
- `--accept-needs-reset N` must equal the dry run's number exactly (use `0` or omit it when there are none).
- The import is **one target transaction**: it completes or changes nothing; failed or interrupted runs leave even the id
  sequences untouched. It takes `ACCESS EXCLUSIVE` locks on the user tables with a 5 s timeout. These locks do **not**
  prove that the web service is stopped; you checked that in §5.
- Typical refusals (exit 1, nothing written): `source-schema`, `target-schema`, `privileges` (not the table owner),
  `source-empty` (almost certainly the wrong connection string), `data-blocked` (§3.3 item 3), `unvetted-hash-format`,
  `needs-reset-unacknowledged`, `target-not-empty`, `source-writes-not-asserted`, `lock-timeout`, `source-changed`.
  Output and reports never contain hashes, tokens, codes, emails, usernames or connection strings.
- Re-running after a completed import is a no-op (`already-migrated`) while the accounts are exactly as imported. After
  an ambiguous failure (for example a connection lost during the commit) re-run the same command.
- **`--replace-users` is destructive.** It is for a target that holds something to discard **before anybody has used
  it** (rehearsal data, an import from a source that has since been corrected). It deletes every account and all
  user-owned data in the target (sessions, one-time links, API and RSS tokens, invitations, users) and imports again,
  in one transaction, never touching torrents, trackers or settings. Accounts that are not in the source (same id and
  username), for example created with `web user add`, make it refuse unless you list exactly those ids with
  `--delete-unmatched-user-ids`. A real run needs `--confirm-plan DIGEST` from a `--dry-run --replace-users` with
  identical options. Prefer a fresh target whenever you can.

### 8.3 Verify the import at once

Immediately after the import, **before** you promote anyone, start `web`, sign in, or run any `web user …` command:

```sh
magnetico migrate users verify --source-timezone ZONE --report-json /reports/users-verify.json
```

It reconciles the source and the target read-only and exits 0 only on an exact match. Keep the successful
`users-verify.json` with the backups (§14): it is the evidence that the import was exact.

Run it only now. Every later change makes the target legitimately different from the frozen source: promoting the
administrator (verify then reports `user-role` mismatches), a first sign-in (the password is rehashed), password changes,
email verification, token use. A later `verify` therefore fails and a later dry run reports the target as diverged.
That is expected and is not a reason to import again: a plain re-run refuses a target that is no longer the exact
import, and `--replace-users` would delete everything that changed since.

If `verify` fails right after the import, stop: do not promote anyone or start `web`. Keep the report and investigate.
Nobody has used the target yet, so `--replace-users` (§8.2) or a recreated target are still safe options.

### 8.4 Promote the first administrator

```sh
export MAGNETICO_ENV_FILE=/srv/magnetico/web.env   # with the image wrapper: web.env holds MAGNETICO_WEB_DATABASE (§4.3)
magnetico web user list                        # password state current/legacy/needs-reset, email state
magnetico web user set-role YOUR_USERNAME admin
unset MAGNETICO_ENV_FILE
```

The `web user …` commands connect with `MAGNETICO_WEB_DATABASE`, the same target account. `web user list` shows `legacy`
for supported hashes (rehashed at the first sign-in), `needs-reset` for unsupported ones and `unverified` email for
everybody.

## 9. Phase F — Start the new system and smoke-test it privately [R]

### 9.1 The ingest and statistics schemas

```sh
magnetico migrate ingest-schema-init   # the magnetico_ingest queue schema; writes no torrents
magnetico migrate stats-schema-init    # optional: the statistics shown in the administrator section
```

Both are idempotent. With the single target account no grants are needed (with the optional restricted web role, see
[`web-deployment.md`](web-deployment.md) §4). **Do not start `ingest` or any crawler yet** (§2).

### 9.2 Start the web service where only you can reach it

Start `magnetico web` with `web.env` ([`web-deployment.md`](web-deployment.md) §1: at least the public URL, the
database and the trusted proxies; [`container.md`](container.md) has a Compose example) **without routing public traffic
to it**. Two ways:

- **A private hostname with TLS behind your reverse proxy** (an unpublished name, restricted by IP). This tests the
  production configuration.
- **Loopback development mode.** A loopback `http://` public URL switches on development mode (cookies without the
  `__Host-` prefix and `Secure`), which the service accepts only with a **loopback listen address**. In a bridge-networked
  container the listener is not on the host's loopback, so the service refuses to start; run that container with host
  networking instead, and reach it through an SSH tunnel:

  ```sh
  docker run -d --name magnetico-web-private --network host --env-file /srv/magnetico/web.env \
    -e MAGNETICO_WEB_LISTEN=127.0.0.1:18081 -e MAGNETICO_WEB_PUBLIC_URL=http://127.0.0.1:18081 \
    --read-only --cap-drop ALL --security-opt no-new-privileges docker.io/skobkin/magnetico-web:0.8.0 web
  ssh -L 18081:127.0.0.1:18081 SERVER        # then open http://127.0.0.1:18081 on your machine
  ```

  The database host in `web.env` must then be reachable from the host network. Never publish this mode on a
  non-loopback address; remove the container when you are done.

### 9.3 Smoke tests

As the administrator, and as a normal migrated user (with their consent) or a rehearsal account:

1. Sign-in with a migrated password works; afterwards `web user list` shows `current` for that account.
2. Search a known name (ASCII and non-ASCII); open a torrent with a long file list in both file-browser modes.
3. A migrated legacy RSS token works in the v2 feed (`/api/v2/feeds/latest.rss?feed_token=LEGACY_TOKEN` answers `200`
   with RSS) and is rejected (`401`) by the JSON API (`/api/v2/torrents`). With `MAGNETICO_WEB_RSS_V1_ENABLED` on (the
   default) the legacy URL `/api/v1/feed/rss/last?api-token=LEGACY_TOKEN` works unchanged; with it off that URL answers
   `404`.
4. `/magnet/{infohash}` redirects with the expected trackers (`web tracker list`).
5. The account page lists the migrated invitations and shows the legacy tokens masked; the administrator pages open.
6. With SMTP configured, a verification email arrives.

A rehearsal account made with `web user add` stays in the database; disable it afterwards (`web user disable`) or
remove it with your rehearsal target.

## 10. Phase G — Cut over [X]

In this order, with a person ready to roll back (§13) until each step is checked:

1. **Migration state.** Keep `magnetico_migration` (the default): there is nothing to do and no further verification
   is needed. Only if you decided to run the optional cleanup, do it now (Appendix B); it is impossible later.
2. **Start `ingest` and register the crawlers** next to PostgreSQL behind the proxy ([`crawler.md`](crawler.md)
   §§2–4): `magnetico ingest crawler add NAME` prints each crawler's credential once. From now on the target can receive
   new torrents, so the torrent verification and cleanup no longer apply to it (§12).
3. **Switch public routing** to the new `magnetico web` and remove the maintenance page.
4. **Start the new crawlers** on their machines ([`crawler.md`](crawler.md) §§5–7). Confirm that `send acked` grows in
   the crawler summary and that the `ingest summary` shows accepted records.
5. **Reset links.** For every `needs-reset` account send an administrator-issued link out of band:
   `magnetico web user reset-password USERNAME` (valid 1 hour for an account with a password hash, 72 hours without).
   **Completing a reset signs the user out everywhere and permanently revokes that user's API and RSS tokens, including
   their legacy RSS URLs** [I].
6. Keep the legacy databases read-only and retained (§14).

## 11. What to tell users

- Everyone has to **sign in again**; sessions did not migrate. Passwords carry over, except for the accounts you told
  that they need a reset.
- **Verify your email address** on the account page before relying on password recovery. A password change or reset
  cancels a confirmation link that is still waiting (the stored address is kept): request the email again afterwards.
- **RSS URLs keep working.** If you switched the v1 feed off, users replace `/api/v1/feed/rss/last?api-token=TOKEN` with
  `/api/v2/feeds/latest.rss?feed_token=TOKEN` (same token). The v1 endpoint is deprecated: create a new RSS URL on the
  account page when convenient. Up to 1,000 recent items may look new once.
- **API clients** need a new API token (account page) and `/api/v2` with `Authorization: Bearer …`; `/api/v1/login` and
  the v1 JSON endpoints no longer exist.
- Old bookmarks of legacy pages may answer `404`. Search is by name only.

## 12. Post-migration checklist

- [ ] `migrate verify-content` passed (exit 0) after the import and after any `retry-errors`, before `ingest` started.
      Its findings, if any, are understood and recorded.
- [ ] The torrent count of the new database equals the legacy count minus the documented unresolved record errors
      (`SELECT count(*) FROM magneticod.torrents;` on each, before ingest started; the legacy count from the frozen
      database).
- [ ] `migrate users verify` exited 0 right after the import, before any account was changed (§8.3), and its report is
      stored. It is not run again.
- [ ] `web user list` matches expectations: the number of `needs-reset` accounts equals N, and you signed in as the
      administrator.
- [ ] A migrated user with a supported hash signed in.
- [ ] A migrated legacy RSS token works in the v2 feed and fails (`401`) on the JSON API.
- [ ] With the v1 feed on (the default): a legacy RSS URL works unchanged. With it off: that URL answers `404`.
- [ ] Search finds known torrents (ASCII and non-ASCII names); a very large torrent opens.
- [ ] `/magnet/{infohash}` redirects; the tracker list is what you expect.
- [ ] The reverse proxy's access log is off, or drops the request path and all headers (Traefik: `RequestPath: drop`,
      not `redact`); it does not cache `/torrents`, `/api` or feeds; its timeouts exceed the search write deadline
      (≥ 90 s) ([`web-deployment.md`](web-deployment.md) §3).
- [ ] Ingest accepts records, the crawler shows `unauthorized=0` and a small `queue records=`, and new torrents appear.
- [ ] Logs contain no secrets (spot-check); SMTP, if configured, delivers a verification email.
- [ ] The backups and every report are stored durably, where the new system's administrators cannot delete them by
      accident (§14).

After the cutover `magnetico_migration` stays in the database. That is supported and harmless: `ingest` only reads the
import phase from it. Once `ingest` has stored a torrent the migration did not write, re-running `migrate torrents` or
`verify-content` against this database is no longer meaningful (the new torrents are not in the legacy source), and
`migrate cleanup` refuses for good. Keep the schema and the reports from before the cutover as the record; do not drop the
schema by hand.

## 13. Rollback

| Situation | What you can do | What is lost |
|---|---|---|
| **Before cutover** (anything before §10 step 3) | Stop the new services; drop or recreate the new database; restore `CONNECT` on the legacy databases (`GRANT CONNECT … TO PUBLIC` and the legacy roles, or `LOGIN` again); re-enable the legacy crawler and PHP application | Nothing: the legacy databases were only read |
| **User migration went wrong** (before anybody used the target) | `migrate users … --replace-users --confirm-plan DIGEST` after a matching dry run, or recreate the target | Nothing legacy |
| **After cutover** | Point the proxy back to the legacy PHP application, re-enable the legacy crawler, restore write access to the legacy databases (unchanged since the freeze) | Everything created in the new system: torrents found by the new crawlers, registrations, password changes and resets, new tokens and invitations. There is **no reverse migration**: keep the new database (do not drop it) in case you want to recover data from it by hand |
| **Password reset issued** | None: completing a reset permanently revokes that user's API and RSS tokens | The user's legacy RSS URLs |
| **`migrate cleanup --execute` run** (Appendix B) | Restore the backup you attested, or recreate the target and redo §§4.4–7 | The migration history and verdicts |

Users who changed their password in the new system must use the **old** password again after a rollback. If you roll
back and retry the migration later, start from a fresh target: `migrate torrents` needs an empty database.

## 14. Keep backups, reports and evidence

Do **not** delete, repurpose or "clean up" any of the following until you have explicitly decided that the new system
is validated (the period is yours; do not shorten it because the checklist passed):

- the legacy torrent and application databases (read-only and connected to no application);
- the `pg_dump` backups of both legacy databases, and of the new database before each irreversible step;
- the reports: the user-migration reports (`users-final-preview.json`, `users-final.json`, the successful
  `users-verify.json`), the torrent-error exports and their manifests, the console output of `verify-content` and the
  audit output;
- the release binary or image that performed the migration (note its version).

The reports are secret-free (counts, numeric ids, digests), but treat them as operational records. They are written
with mode `0600`; keep the directory on durable storage that is backed up (not `/tmp`) and copy them next to the
backups. If the container user `10001` wrote them, read or copy them as root, or through the image (§4.1).

After your decision, remove old data the way your retention policy requires. The legacy databases contain password
hashes and credential-equivalent tokens: protect them until they are destroyed.

---

## Appendix A. `migrate verify-decode`: a target-only diagnostic

```sh
magnetico migrate verify-decode
```

It reads every torrent stored in the target and decodes its file list with the production limits. It checks file
counts, that the sizes add up to the total, and that each list uses the canonical storage form (a single ordinary file
must be stored without a list). Then it looks for stored lists that belong to no torrent. It reads no legacy database.
Exit status is the same as for `verify-content`; an interrupted run resumes when re-run.

**It is not part of the migration procedure.** `verify-content` (§7) already decodes every stored file list and in
addition compares it with the source. Running both repeats a full scan without adding any guarantee of equality; on a
large production database that can add about a day of downtime.

**When it is useful.**

- **Investigating suspected storage or file-list problems** in an existing 0.8.0 installation that was created by
  `migrate torrents`, for example after a storage fault or a restore from backup. It also covers the torrents stored by
  `ingest` since the migration.
- **When a full source comparison is impossible**, for example because the legacy database is no longer available. It
  then proves only that what the target stores is internally consistent and decodable, **not** that it equals the
  legacy data: a missing torrent, a wrong name, infohash or date, or the lost file list of a torrent stored as a single
  file (reported as "assumed single-file", because it cannot be compared) goes unnoticed. Record that the migration was
  not verified against its source.
- **Before the optional cleanup**, which requires current passing verdicts of both commands (Appendix B).

**Prerequisites.**

- The target still holds the migration state of a completed import (`magnetico_migration`). It refuses otherwise, so it
  cannot check a database that was never migrated or whose migration state was removed by cleanup.
- **No torrent writes during the scan:** stop `magnetico ingest` (crawlers keep their records queued locally and deliver
  them when it is back) and do not run other `migrate` commands (the shared lock refuses a second one). `web` may keep
  running; it does not write torrents.

## Appendix B. Optional `migrate cleanup`

`migrate torrents` leaves the operational schema `magnetico_migration` (progress counters, the error registry,
verification verdicts). **Keeping it is the default and the recommendation**, also after cutover: it is harmless to the
services, it is the forensic record, and it costs almost nothing. If you keep it, skip this appendix; you need neither
`verify-decode` nor anything else from here.

`migrate cleanup` removes that schema. It is possible only during the offline phase, **before `ingest` stores its first
torrent** (§10 step 1): its reconciliation gate compares the target's torrent and file-list counts with the recorded
import counters, and it can never pass again once `ingest` has stored a torrent. Its other requirements:

- **current passing verdicts of both `verify-content` and `verify-decode`**, with nothing changed since (run
  `verify-decode` as in Appendix A if you have not, after the final `verify-content`);
- no unresolved findings or record errors;
- a complete export of the migration state with its manifest, and an independent backup of the new database.

```sh
magnetico migrate errors export --output /reports/final-export.jsonl --manifest   # the evidence cleanup validates
magnetico migrate cleanup        # a non-destructive, gate-by-gate preview: read every gate
magnetico migrate cleanup --execute --confirm-backup --confirm-export --evidence-manifest /reports/final-export.jsonl.manifest.json
```

`--execute` drops `magnetico_migration` in one transaction after every gate passes and **destroys the migration history
irrecoverably** [I] (the permanent `schema_version` and all torrent and account data stay). `--confirm-backup` is your
attestation that an independent backup of the new database exists. If a gate fails, do not work around it: keep the
schema, which loses nothing.
