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

- `crawl` — the DHT crawler, persisting into the new PostgreSQL storage. On
  startup it verifies the database schema **read-only**: a missing `pg_trgm`
  extension, a legacy schema or an unsupported schema state is refused — it
  never creates or migrates anything. Known limitation: shutdown is prompt but
  not fully graceful (in-flight leech goroutines are not drained).
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
- `web` — explicit not-implemented placeholder that exits non-zero (the
  authenticated web UI will be designed from the historical PHP application in
  a later phase).

Removed compared to upstream (recoverable from Git history): the upstream Go
web UI and Basic Auth, the JSONL export/import modes, and the SQLite,
legacy-PostgreSQL (including its automatic schema migration and 100-partition
file index), RabbitMQ, ZeroMQ and Bitmagnet backends.

Crawler flags are unchanged from upstream; `--config-file-path` is now a
global option that must precede the subcommand.

## Development status and remaining work

The storage layer (codec + PostgreSQL persistence), the crawler and the
migration toolchain are implemented and validated. The remaining major
workstreams for 0.8.0 are the replacement web UI, the application-level
torrent and user data migration, CI integration, and production cutover
planning — the task documents live in
[`docs/wip/`](docs/wip/), each reviewed separately before code changes.

## Build and test

    go build ./...
    go vet ./...
    go test ./...

Integration tests bootstrap their own disposable PostgreSQL (a container from
the locally cached `postgres:17` image, an explicit
`MAGNETICO_TEST_POSTGRES_DSN`, or local `initdb`/`postgres` binaries) and skip
when none is available; they never touch any other server.

## Examples

    magnetico --config-file-path config.yaml crawl --indexer-addr 0.0.0.0:6881

Optional `config.yaml` keys (all crawl settings, same names as upstream):
`databaseURL`, `pyroscopeURL`, `indexerAddrs`, `indexerMaxNeighbors`,
`leechDeadline`, `leechMaxN`, `maxRPS`, `bootstrappingNodes`,
`filterNodesCIDRs`.

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
