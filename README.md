# magnetico-web (unified Magnetico 0.8.0 — under development)

This repository is a unified Go implementation of
[Magnetico](https://github.com/boramalper/magnetico), targeting the upcoming
**0.8.0** milestone: one Go codebase with a single CLI providing separate
`crawl`, `web` and `migrate` commands, PostgreSQL as the only storage backend,
a compact versioned torrent file-list storage format, and a private,
authenticated web UI to be implemented later.

> **Status: under active development — not deployable.**
> Nothing in this branch is production-ready yet, and no production migration
> or cutover has happened or been approved.
>
> **Released versions are the PHP/Symfony 0.7.x application** on the `master`
> branch (see `UPGRADE.md` for their upgrade history). Everything on this
> development branch is unreleased Go work; no 0.8.0 release exists yet, and
> its breaking changes will be documented in `UPGRADE.md` when it is ready.

## Current state

A single binary `magnetico` with the following subcommands:

- `crawl` — the DHT crawler. It has **no database**: discovered metadata goes
  into a durable local queue (SQLite WAL, `synchronous=FULL`) and is delivered
  to `ingest` over authenticated HTTP (bearer token from the environment only).
  It prechecks hashes against ingest, bounds active leeches, keeps records
  through outages and quarantines only permanently invalid ones. Shutdown is
  bounded; committed records survive any exit. `crawl check-queue` runs queue
  diagnostics. See `UPGRADE.md` for settings.
- `ingest` — the HTTP service in front of PostgreSQL: existence lookup
  (`POST /api/ingest/v1/torrents/exists`) and all-or-nothing durable admission
  (`POST /api/ingest/v1/torrents`, `202` only after a PostgreSQL commit) into a
  `magnetico_ingest` queue with a byte quota, drained by a single consumer
  holding an advisory lock. It verifies the 0.8.0 schema and the queue schema
  (`magnetico migrate ingest-schema-init`) read-only at startup.
- `migrate` — the offline migration toolchain for original magnetico v0.12.0
  databases. Checkpointed and resumable (crash-safe, all-or-nothing batches,
  advisory-lock exclusion, durable operational state in a dedicated
  `magnetico_migration` schema), with the legacy source database treated
  strictly as read-only:
  - `migrate schema-init` — creates the 0.8.0 schema non-destructively and
    is idempotent on an already valid 0.8.0 schema; incompatible or legacy
    schemas are refused, never modified.
  - `migrate torrents` — the checkpointed import; `--defer-secondary-indexes`
    defers the three secondary indexes to a rebuild at finalization.
  - `migrate torrents retry-errors` — the transactional repair pass over the
    import's recorded errors.
  - `migrate verify-decode` — the full target-only decode scan of the stored
    blobs (the source is never contacted).
  - `migrate verify-content` — the full source↔target equality scan.
  - `migrate errors export` — the forensic, fail-open JSONL export of the
    migration's operational state, with an optional SHA-256 manifest.
  - `migrate cleanup` — the gated, fail-closed teardown of the
    `magnetico_migration` schema (a non-destructive preview by default;
    `--execute` runs only when every gate passes).

  The source database is reached through the `MAGNETICO_MIGRATION_SOURCE_DSN`
  environment variable or the `migrateSourceDatabaseURL` config-file key (no
  CLI flag). Exit codes across the migrate commands: `0` ok, `1` fatal or
  refusal, `2` interrupted (re-run the same command to resume),
  `3` completed with findings.

  The toolchain was validated end-to-end on the real historical sample
  (3,457,046 torrents / 123,460,039 files) with controlled fault injection —
  process kills in every phase, connection loss, lost COMMIT acknowledgements,
  source drift and container restarts — finishing with a full
  source↔target equality pass; see
  [`docs/wip/04D-end-to-end-validation-report.md`](docs/wip/04D-end-to-end-validation-report.md).
- `migrate users` — the one-transaction import of the legacy PHP accounts,
  invitations and API tokens (as legacy RSS tokens) into the `magnetico_web`
  schema, with a read-only `--dry-run` and a `verify` subcommand; see
  [`docs/web-deployment.md`](docs/web-deployment.md) §10.
- `web` — the authenticated web UI and JSON API (search, details and file
  browser, accounts, API v2, RSS, magnet links), plus the operator commands
  `web user …` and `web tracker …`; see
  [`docs/web-deployment.md`](docs/web-deployment.md).
- `dev seed-torrents` — a development helper that adds a deterministic
  synthetic torrent dataset (101 torrents, one with 10,000 files) to a
  **disposable** 0.8.0 database for manual testing; see
  [`docs/development-fixtures.md`](docs/development-fixtures.md).

Removed compared to upstream (recoverable from Git history): the upstream Go
web UI and Basic Auth, the JSONL export/import modes, and the SQLite,
legacy-PostgreSQL (including its automatic schema migration and 100-partition
file index), RabbitMQ, ZeroMQ and Bitmagnet backends.

Crawler flags are unchanged from upstream; `--config-file-path` is now a
global option that must precede the subcommand.

## Documentation

| Guide | Audience |
|---|---|
| [`UPGRADE.md`](UPGRADE.md) | Breaking changes per release; the 0.8.0 warning |
| [`docs/migration_0.8.0.md`](docs/migration_0.8.0.md) | Step-by-step migration from the legacy PHP/Symfony version |
| [`docs/crawler.md`](docs/crawler.md) | Running `magnetico crawl` on a separate machine with `magnetico ingest` next to PostgreSQL |
| [`docs/web-deployment.md`](docs/web-deployment.md) | Web service configuration, database roles, reverse proxy, administrator commands, user migration |
| [`docs/development-fixtures.md`](docs/development-fixtures.md) | Synthetic torrents for manual testing |
| [`docs/specs/`](docs/specs/) | Normative contracts |

## Development status and remaining work

The storage layer (codec + PostgreSQL persistence), the crawler/ingest delivery,
the migration toolchain, the web UI and API (milestones W1–W7) and the user
migration (W8) are implemented; reviews of the latest milestones are tracked in
[`docs/wip/`](docs/wip/). Not done: milestone W9, CI and OCI image publishing,
release-candidate testing (including live-DHT acceptance) and the production
cutover. No 0.8.0 release exists yet.

## Build and test

    go build ./...
    go vet ./...
    go test ./...

Integration tests bootstrap their own disposable PostgreSQL (a container from
the locally cached `postgres:17` image, an explicit
`MAGNETICO_TEST_POSTGRES_DSN`, or local `initdb`/`postgres` binaries) and skip
when none is available; they never touch any other server.

## Examples

    MAGNETICO_INGEST_TOKEN=… magnetico ingest --listen 127.0.0.1:8080
    MAGNETICO_CRAWL_INGEST_TOKEN=… magnetico --config-file-path config.yaml crawl \
      --ingest-url http://127.0.0.1:8080 --indexer-addr 0.0.0.0:6881

Optional `config.yaml` keys (crawl settings): `ingestURL`, `queueDir`,
`queueMaxBytes`, `leechMaxActive`, `pyroscopeURL`, `indexerAddrs`,
`indexerMaxNeighbors`, `leechDeadline`, `leechMaxN`, `maxRPS`,
`bootstrappingNodes`, `filterNodesCIDRs`. Tokens are never read from the
config file or flags. The former `databaseURL` key was removed.

Initialize a fresh application database and import a legacy v0.12.0 database
into it (resumable — re-run the same command after any interruption):

    magnetico migrate schema-init --database postgres://localhost/magnetico_app
    MAGNETICO_MIGRATION_SOURCE_DSN=postgres://localhost/legacy \
      magnetico migrate torrents --database postgres://localhost/magnetico_app \
      --defer-secondary-indexes

## Provenance

- The Go code base is imported verbatim from
  [`tgragnato/magnetico`](https://github.com/tgragnato/magnetico) at commit
  [`f80660653e7d1045b95d020a60b12a08e9a3c9b3`](https://github.com/tgragnato/magnetico/tree/f80660653e7d1045b95d020a60b12a08e9a3c9b3).
- `tgragnato/magnetico` is a fork of the original
  [`boramalper/magnetico`](https://github.com/boramalper/magnetico)
  (now archived), the autonomous self-hosted BitTorrent DHT search engine suite.
- The previous PHP/Symfony application that lived in this repository remains
  fully recoverable from Git history on the `master` branch (last PHP code
  commit: [`3d2aa69`](https://git.skobk.in/skobkin/magnetico-web/commit/3d2aa69700fbd4ba4f888338b45e5d2c098a1a85)).
  Its source was MIT licensed.

## License

The imported Go code is licensed under the **GNU Affero General Public
License v3.0** — see [`COPYING`](COPYING). Upstream copyright notices and
required attribution are retained. The historical PHP/Symfony application was
MIT licensed; no relicensing of third-party work is claimed or performed.

## Repository

- Canonical source: [git.skobk.in/skobkin/magnetico-web](https://git.skobk.in/skobkin/magnetico-web) (Forgejo).
- Go module path: `github.com/skobkin/magnetico-web` (the public GitHub mirror; importable once a version is tagged).
- No production container image has been published for the unified version.
