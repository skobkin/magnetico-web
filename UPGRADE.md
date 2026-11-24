# Upgrade notes

User-facing and breaking changes that require action when deploying a new version.
Entries are grouped by release date. Read the section covering your current version
before upgrading.

## Unreleased 0.8.0 (development branch)

> [!WARNING]
> **0.8.0 is a major, partially manual, backward-incompatible migration.** It replaces the PHP/Symfony
> application and the old crawler, stores torrents in a **new database in a new format**, and imports
> accounts with a separate tool. There is no in-place upgrade, the cutover cannot be fully undone, and
> several steps need operator decisions (password-hash audit, time zone, administrator accounts,
> maintenance window). **Do not deploy it over an existing installation.** Follow the step-by-step guide
> in [`docs/migration_0.8.0.md`](docs/migration_0.8.0.md) and keep the old databases and backups until
> you have validated the new system.

### Starting the Go application with the old PHP deployment's environment (temporary warning)

Magnetico Web 0.8.0 is a rewrite in Go: the PHP application's configuration and database layout are **incompatible**
with it, and nothing is migrated or translated automatically. To save operators who point the new image at their old
deployment from reading unrelated configuration errors, every command except `--help`, `-h` and `--version` first checks
the process environment for the **names** `APP_DATABASE_URL` and
`MAGNETICOD_DATABASE_URL` (the two variables only the PHP application used; the generic `APP_ENV`, `APP_SECRET`,
`MAILER_DSN`, … are deliberately not triggers). If one is present it prints a boxed warning on standard error, before
the configuration is read:

```
================================================================================
WARNING: the environment of the previous PHP deployment of Magnetico Web was detected
         (variables set: APP_DATABASE_URL).

Magnetico Web has been rewritten in Go. The old deployment configuration and the old
database layout are incompatible with this application. Explicit migration and
configuration changes are required; nothing is migrated or translated automatically
and no database is modified by this notice.

Upgrade instructions: https://github.com/skobkin/magnetico-web/blob/master/UPGRADE.md
(Temporary notice; planned for removal around version 0.10.0.)
================================================================================
```

The old PHP image had no command of its own, so a deployment whose image tag is merely bumped starts this image
**without arguments**: the banner is printed before the usage error then too (the usage text and exit status 1 are
unchanged). The warning prints variable **names only**, never a value, a DSN or a credential; it changes no exit code, never
connects to anything and does not weaken the fail-closed schema verification or the required-configuration checks (the
real error follows it unchanged). The link targets `master`: the instructions are this file's 0.8.0 section and
[`docs/migration_0.8.0.md`](docs/migration_0.8.0.md), which reach `master` when the owner merges the 0.8.0 branch.

**This warning is temporary and is intended to be removed around version 0.10.0** (the code is the one call in
`internal/app/app.go` and the package `internal/legacywarn`; its tests are `internal/legacywarn` and
`internal/e2e/cli/legacywarn_test.go`).

### The crawler no longer connects to PostgreSQL (distributed crawler/ingest)

`magnetico crawl` now delivers metadata to a separate **`magnetico ingest`** service over
authenticated HTTP. The crawler keeps a durable local queue (SQLite, WAL) so records survive
ingest or network outages; the ingest service queues them durably in PostgreSQL and a single
consumer stores them. Design: `docs/specs/crawler-ingest-delivery-v1.md`.

**Removed from `crawl`** (stale values fail loudly at startup, with a pointer to this file):

| Removed | Replacement |
|---|---|
| `--database`, `MAGNETICO_CRAWL_DATABASE`, yaml `databaseURL` | none — the crawler has no database |

**New crawler settings**

| Flag | Environment | YAML | Default |
|---|---|---|---|
| `--ingest-url` | `MAGNETICO_CRAWL_INGEST_URL` | `ingestURL` | — (required; `https://`, or `http://` to a loopback host only) |
| *(none)* | `MAGNETICO_CRAWL_INGEST_TOKEN` | *(none)* | — (required, ≥ 32 bytes; **environment only**) |
| `--queue-dir` | `MAGNETICO_CRAWL_QUEUE_DIR` | `queueDir` | `magnetico-crawl-queue` |
| `--queue-max-bytes` | `MAGNETICO_CRAWL_QUEUE_MAX_BYTES` | `queueMaxBytes` | `1073741824` |
| `--leech-max-active` | `MAGNETICO_CRAWL_LEECH_MAX_ACTIVE` | `leechMaxActive` | `32` |

Put the queue directory on a **local** filesystem that honours `fsync` (ext4/xfs; not NFS/SMB) and
mount it as a persistent volume in Docker. The directory is locked: one crawler per queue. `magnetico
crawl check-queue [--queue-dir DIR] [--full]` runs SQLite integrity diagnostics (run it while the
crawler is stopped). Queued records are never discarded on shutdown; on `SIGTERM` the crawler stops
the DHT, cancels leeches, commits what is buffered (5 s), lets the sender finish its request (30 s)
and closes the queue with a bounded wait. If the SQLite writer is stuck in the operating system the
process exits with status 3 and SQLite recovers the queue at the next start.

**New `ingest` service** (`magnetico ingest`; environment `MAGNETICO_INGEST_*`, see
`magnetico ingest --help`): `LISTEN` (default `127.0.0.1:8080`, plain HTTP — terminate TLS in a
reverse proxy such as Traefik), `TOKEN` (required, ≥ 32 bytes, environment only, must equal the
crawler's), `DATABASE`, optional `QUEUE_DATABASE`, `QUEUE_MAX_BYTES` (default 2 GiB),
`CACHE_MAX_BYTES` (default 8 MiB), `CACHE_DISABLED`, `ALLOW_NON_DURABLE_DB` (development only).
PostgreSQL must run with `fsync=on` (ingest refuses to start otherwise). The proxy must allow request
bodies of at least 17 MiB on `/api/ingest/v1/torrents`.

**Memory:** set `GOMEMLIMIT=256MiB` on `magnetico ingest` (measured on synthetic load at 40 records/s over two
hours plus bursts: mean RSS 459 → 304 MiB, peak 621 → 354 MiB; ingest CPU rose slightly, from about 5.0 % to
5.2 % of one core, with no measurable throughput or backlog regression and GC CPU still small; see
`docs/wip/06-crawler-ingest-results.md`). This is a Go runtime memory *target*, not a hard RSS or container
limit: the process can exceed it. For initial staging give the container a **1 GiB** memory limit: with
`GOMEMLIMIT=256MiB` it survived every tested case, including records near the 16 MiB limit and eight
simultaneous near-limit submissions (peak 518 MiB). A 512 MiB limit is not recommended (the default runtime was
OOM-killed there in that worst case; with `GOMEMLIMIT` it ran at the limit). 1 GiB has been tested on synthetic
data only and is not claimed to suffice for every workload. The crawler needs no setting (RSS ≈ 30 MiB).

**Deployment order** (new databases or after the migration):

1. `magnetico migrate schema-init`
2. `magnetico migrate torrents` (legacy import), verification, optional `cleanup` (offline only, before any ingest; by default the migration schema is kept)
3. `magnetico migrate ingest-schema-init` (creates the `magnetico_ingest` schema; idempotent)
4. start `magnetico ingest`, then the crawlers

`ingest` refuses to start while an import is running or incomplete. Records ingested after the
import make earlier migration-verification verdicts stale.

**Quarantine.** Both sides keep records that are permanently invalid in a quarantine table
(`quarantine` in the crawler's SQLite file, `magnetico_ingest.quarantine` in PostgreSQL). Nothing is
deleted automatically and quarantined bytes count toward the quota. Inspect with SQL, for example
`SELECT reason_code, count(*) FROM magnetico_ingest.quarantine GROUP BY 1;`; to retry after fixing
the cause, `INSERT INTO magnetico_ingest.queue (info_hash, format, payload, payload_bytes) SELECT
info_hash, format, payload, payload_bytes FROM magnetico_ingest.quarantine WHERE …;` followed by the
matching `DELETE` in the same transaction (the usage counter is maintained by triggers).

## 2026-10-21 0.7.0

### PHP 8.5 required

The application now requires PHP >= 8.5 (previously 8.2). The Docker image is based
on `dunglas/frankenphp:1-php8.5-alpine`. If you run the app outside Docker, upgrade
your PHP runtime first.

### PHP security floor: 8.5.12 (critical RCE fixes, CVE-2026-109618 / CVE-2026-109619)

PHP 8.5.12, 8.4.27, 8.3.36 and 8.2.35 (released 2026-10-21) fix two critical
unauthenticated remote code execution vulnerabilities affecting all web-serving
SAPIs, including FrankenPHP (see the
[PHP announcement](https://www.php.net/archive/2026.php#2026-10-21-5)). The Docker
image in this release is built on PHP 8.5.12 and **refuses to build** on an older
base image. If you run the app outside Docker, make sure your runtime is 8.5.12+
(or 8.4.27+/8.3.36+/8.2.35+ on older branches) and update immediately — per the
advisory there are no complete mitigations, updating is the only fix.

### reCAPTCHA bundle replaced (env vars renamed)

`excelwebzone/recaptcha-bundle` was replaced with `karser/karser-recaptcha3-bundle`
(reCAPTCHA v3). Rename your environment variables:

| Old variable             | New variable       |
|--------------------------|--------------------|
| `EWZ_RECAPTCHA_SITE_KEY` | `RECAPTCHA3_KEY`   |
| `EWZ_RECAPTCHA_SECRET`   | `RECAPTCHA3_SECRET`|

reCAPTCHA v3 keys are issued per site; if your existing keys were registered for
v3, their values can be reused directly. Otherwise register the site for
[reCAPTCHA v3](https://www.google.com/recaptcha/admin/create) and use the new keys.
reCAPTCHA is now also enforced on the registration form (previously only on the
password reset request form).

### FrankenPHP runtime package removed

`runtime/frankenphp-symfony` is no longer used — `symfony/runtime` >= 7.4 detects
FrankenPHP worker mode natively. If you set `APP_RUNTIME=Runtime\FrankenPhpSymfony\Runtime`
in your environment (e.g. in a custom `docker-compose.yml`), remove that variable.
The bundled Dockerfile no longer sets it.

### Symfony 8 upgrade: re-login required

The upgrade to Symfony 8 changes the remember-me cookie format. All logged-in
users (including "remember me" sessions) will be logged out once after the deploy
and must log in again. No stored data is affected.

### Docker: clear the stale prod cache once when upgrading

The image used to declare `VOLUME /app/var`, so the compiled prod cache lived in
an anonymous volume. Docker Compose reuses anonymous volumes when recreating a
container, so the first start of this release can boot with a cache compiled by
the previous (Symfony 6.4-based) image and fail every request with `TypeError`s
like `RememberMeAuthenticator::__construct(): Argument #2 ($tokenStorage) must be
of type TokenStorageInterface, string given`. The image no longer declares that
volume.

If you deploy with Docker Compose, recreate the container with fresh anonymous
volumes once:

    docker compose up -d --renew-anon-volumes

The old dangling volume can be removed afterwards with `docker volume prune`.
Without Compose: remove the volume attached at `/app/var` (or wipe `/app/var/cache`
inside the container) and recreate the container.

### Before you deploy

1. **Back up both PostgreSQL databases** — the application database
   (`APP_DATABASE_URL`) and the magneticod database (`MAGNETICOD_DATABASE_URL`).
2. Deploy the new version. This release contains **no database schema changes**
   and no new migrations.
3. After the deploy, verify both entity managers:
   `bin/console doctrine:schema:validate --em=default` and
   `bin/console doctrine:schema:validate --em=magneticod`.
