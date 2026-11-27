# Upgrade notes

User-facing and breaking changes that require action when deploying a new version.
Entries are grouped by release date. Read the section covering your current version
before upgrading.

## 0.8.0

> [!WARNING]
> **0.8.0 is not an in-place upgrade.** It is an incompatible rewrite that needs a new database and an explicit,
> partly manual migration with a one-way cutover. Do not point the new image at your existing deployment. Follow
> [`docs/migration_0.8.0.md`](docs/migration_0.8.0.md), rehearse it on copies, and keep the old databases and backups
> until you have validated the new system.

**What 0.8.0 is**

- **A rewrite in Go.** One executable, `magnetico`, replaces the PHP/Symfony application and the old crawler. Its
  subcommands are `web` (the site and the API), `crawl` (the DHT crawler), `ingest` (receives the crawlers' records next
  to PostgreSQL) and `migrate` (schemas and the migration). The image keeps its name, `docker.io/skobkin/magnetico-web`,
  but it is a different program: it has no default command, runs as `10001:10001` and reads only `MAGNETICO_*`
  settings. No PHP configuration is reused.
- **A new database.** Torrents are stored in a new compact format in a **new, separate PostgreSQL database**. The legacy
  databases are only read, never upgraded in place.
- **An explicit migration.** `magnetico migrate torrents` copies the torrents and `magnetico migrate users` imports the
  accounts. Nothing is migrated at startup. Started with the old PHP environment (`APP_DATABASE_URL` or
  `MAGNETICOD_DATABASE_URL` set), 0.8.0 only prints a warning that points here. The warning is temporary and planned
  for removal around 0.10.0.
- **A new crawler.** The legacy crawler is retired. The new `magnetico crawl` has no database access: it queues records
  locally and delivers them over HTTPS to `magnetico ingest`, with a separate credential for each crawler
  ([`docs/crawler.md`](docs/crawler.md)).

**Main compatibility breaks** (the complete table is in the guide, §1)

- Everyone signs in again, because sessions are not migrated. Passwords carry over. Accounts whose hash format is
  unsupported need a reset link issued by an administrator.
- Legacy API tokens become RSS-only tokens. The v1 JSON API is removed: API clients need a new token and `/api/v2`. The
  v1 RSS URL keeps working (it is deprecated and can be switched off).
- Email addresses are imported unverified. SMTP is optional.
- Legacy page URLs answer `404` (there are no redirects), reCAPTCHA is gone, and search covers torrent names only.
- The [`docker-stacks`](https://git.skobk.in/skobkin/docker-stacks/src/branch/master/magnetico-web) deployment still
  describes the PHP version and does not work with 0.8.0.

**Decisions to prepare** (guide §3):

- the legacy PHP time zone;
- the outcome of the password-hash audit, and which accounts will need a reset link;
- the first administrator;
- SMTP;
- the public URL and the reverse-proxy settings, including its access log;
- the maintenance window, sized by a rehearsal (the torrent import plus `migrate verify-content`);
- whether to run the optional `migrate cleanup` (the default is to keep the migration state).

**Procedure:** [`docs/migration_0.8.0.md`](docs/migration_0.8.0.md) is the authoritative step-by-step procedure; this
entry is only an overview. `0.8.0-rc1` was rehearsed on a copy of a real 0.7.x installation
([report](docs/wip/real-data-migration-report-0.8.0-rc1.md)).

### Only for installations of 0.8.0 development builds

Skip this part when you upgrade from 0.7.x. It is for installations that ran a pre-release development build of 0.8.0,
and some startup errors point here.

- **`crawl` has no database setting.** `--database`, `MAGNETICO_CRAWL_DATABASE` and the YAML key `databaseURL` were
  removed and are refused at startup. Set `MAGNETICO_CRAWL_INGEST_URL` (`ingestURL`) and the crawler's own
  `MAGNETICO_CRAWL_INGEST_TOKEN` (environment only) instead ([`docs/crawler.md`](docs/crawler.md) §6).
- **The shared ingest token was removed.** `MAGNETICO_INGEST_TOKEN` is refused at startup, with no fallback. Register
  each crawler with `magnetico ingest crawler add <name>` and give it the printed token as
  `MAGNETICO_CRAWL_INGEST_TOKEN` ([`docs/crawler.md`](docs/crawler.md) §3).

**Reinitialising an older development ingest schema.** The `crawlers` table (per-crawler credentials) is part of the
initial `magnetico_ingest` definition, so there is no upgrade command. `ingest` and `magnetico ingest crawler …` refuse a
development database whose `magnetico_ingest` predates it, with `missing table crawlers`. Nothing repairs or alters it.
Reinitialise it by hand:

1. Stop `magnetico ingest`. The next step **loses** records that crawlers already had acknowledged but that were not
   processed yet; if they matter, let the queue drain first.
2. `DROP SCHEMA magnetico_ingest CASCADE;`
3. `magnetico migrate ingest-schema-init`
4. `magnetico ingest crawler add <name>` once per crawler, and give each crawler its token.

Only `magnetico_ingest` is touched: the torrent dataset (`magneticod`), the web schema (`magnetico_web`: users, tokens,
invitations) and `magnetico_stats` stay as they are. Statistics rows of crawlers from the dropped registry
(`magnetico_stats.crawler_activity`) stay too, unreferenced and harmless; the Crawlers page lists only the registry.

**Reinitialising an older development statistics schema.** A development `magnetico_stats` created before the final
definition fails the structural check. `ingest` and `web` start without it, and the administrator overview says so;
nothing alters it. Statistics are disposable: `DROP SCHEMA magnetico_stats CASCADE;`, then
`magnetico migrate stats-schema-init`, then apply its grants again if you use the restricted web role
([`docs/web-deployment.md`](docs/web-deployment.md) §4). Only the collected aggregates are lost.

## 2026-11-21 (PHP 0.7.x line, unreleased; superseded by 0.8.0)

> Kept as history from `master` (commit `0c7b2cb6`), merged into the 0.8.0 branch. It applies to the PHP image of the
> 0.7.x line only: 0.8.0 replaces that image (and its PHP and FrankenPHP floors) with the Go image described above.

### FrankenPHP security floor: 1.13.6 (critical PHP fixes published 2026-11-19)

The Docker image is now built on FrankenPHP >= 1.13.6 and **refuses to build** on
an older base image. Every FrankenPHP release before 1.13.6 embeds a PHP without
the security fixes published 2026-11-19 (the most severe is rated critical by the
PHP project; 1.13.6 ships PHP 8.5.13) — see the
[php/frankenphp v1.13.6 release notes](https://github.com/php/frankenphp/releases/tag/v1.13.6).

Rebuild the image from a freshly pulled base image (`docker build --pull` or
`docker compose build --pull`) — a cached `dunglas/frankenphp:1-php8.5-alpine`
can be months old and will now be rejected by the build on purpose. If you run
the app outside Docker, run PHP >= 8.5.13 (or 8.4.28+/8.3.37+/8.2.36+ on older
branches) and update immediately.

### PHP security floor raised: 8.5.13 (2026-11-19 security batch)

The PHP security floor in the Docker build was raised from 8.5.12 to **8.5.13**
(released 2026-11-19 as 8.5.13 / 8.4.28 / 8.3.37 / 8.2.36 — see the
[PHP news archive](https://www.php.net/archive/2026.php)). The release fixes
multiple critical use-after-free vulnerabilities that can lead to remote code
execution (timezone cache, magic method backtraces, interrupted session
shutdown, partially overwritten session files — advisories `GHSA-gr4f-wgx5-jpfv`,
`GHSA-8mjj-2r7p-4hg2`, `GHSA-2fjv-p558-g82m`, `GHSA-g3vh-838c-65v7`,
`GHSA-fm2m-fjf5-v3cp` on the
[php-src security advisories page](https://github.com/php/php-src/security/advisories)),
in addition to the October critical RCE fixes (CVE-2026-109618/109619). The
build now **refuses** any base image with PHP < 8.5.13. If you run the app
outside Docker, update to PHP 8.5.13+ (or the matching patch version on older
branches) immediately.

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
