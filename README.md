# magnetico-web [![build status](https://ci.skobk.in/api/badges/skobkin/magnetico-web/status.svg?events=push%2Ctag)](https://ci.skobk.in/repos/skobkin/magnetico-web)[![latest release](https://git.skobk.in/skobkin/magnetico-web/badges/release.svg)](https://git.skobk.in/skobkin/magnetico-web/releases)

A private, self-hosted search engine for torrents discovered on the BitTorrent DHT, written in Go with PostgreSQL
storage and optimized for storage efficiency and low resource consumption.

Upgrading from the PHP/Symfony 0.7.x version is a manual migration: see [`UPGRADE.md`](UPGRADE.md) and
[`docs/migration_0.8.0.md`](docs/migration_0.8.0.md).

## Container images and releases

| | |
|---|---|
| **Docker Hub** | [`skobkin/magnetico-web`](https://hub.docker.com/r/skobkin/magnetico-web) (`docker.io/skobkin/magnetico-web`) |
| **GitHub Container Registry** | [`ghcr.io/skobkin/magnetico-web`](https://github.com/skobkin/magnetico-web/pkgs/container/magnetico-web) |
| **Deployment stack** | [`docker-stacks/magnetico-web`](https://git.skobk.in/skobkin/docker-stacks/src/branch/master/magnetico-web) |
| **Native binaries** | Linux amd64 and arm64 archives with SHA-256 checksums on the [Forgejo releases](https://git.skobk.in/skobkin/magnetico-web/releases) |

One multi-platform image (`linux/amd64`, `linux/arm64`) holds the single `magnetico` executable and every subcommand.
Every release is tagged with its exact version (`0.8.0`, …), and stable releases also update `latest`. The image runs
as `10001:10001`, with `/data` (the crawler queue) and `/reports` (migration reports and exports) as the only writable
places.

```sh
docker run --rm docker.io/skobkin/magnetico-web:0.8.0 --version
# web (behind a TLS-terminating reverse proxy), ingest, crawler with a persistent queue and its UDP port,
# migration/admin commands, healthchecks, graceful shutdown, resources: see docs/container.md
docker run -d --name magnetico-crawl -p 6881:6881/udp -v magnetico-queue:/data --env-file crawler.env \
  -e MAGNETICO_CRAWL_INGEST_URL=https://ingest.example.org -e MAGNETICO_CRAWL_INDEXER_ADDR=0.0.0.0:6881 \
  docker.io/skobkin/magnetico-web:0.8.0 crawl
```

Starting this image with the environment of the **old PHP deployment** (`APP_DATABASE_URL` or
`MAGNETICOD_DATABASE_URL` set) prints a prominent warning that the application was rewritten in Go and needs explicit
migration and configuration changes (see [`UPGRADE.md`](UPGRADE.md)); it never migrates or translates anything.

## Components

One executable, `magnetico`, with a subcommand per service:

- `crawl`: the DHT crawler. It has no database: discovered metadata goes into a durable local queue and is delivered
  to `ingest` over authenticated HTTP. See [`docs/crawler.md`](docs/crawler.md).
- `ingest`: the HTTP service next to PostgreSQL that admits the crawlers' records, with a separate credential per
  crawler and optional operating statistics. See [`docs/crawler.md`](docs/crawler.md) and
  [`docs/operator-statistics.md`](docs/operator-statistics.md).
- `web`: the authenticated web UI and JSON API (search, torrent details and file browser, accounts and invitations,
  API v2, RSS, magnet links) and a read-only administrator section. See
  [`docs/web-deployment.md`](docs/web-deployment.md).
- `migrate`: the offline, resumable import of original magnetico v0.12.0 torrent databases and of the legacy PHP
  accounts. See [`docs/migration_0.8.0.md`](docs/migration_0.8.0.md).
- `dev seed-torrents`: a synthetic dataset for a disposable development database. See
  [`docs/development-fixtures.md`](docs/development-fixtures.md).

Removed compared to upstream (recoverable from Git history): the upstream Go web UI and Basic Auth, the JSONL
export/import modes, and the SQLite, legacy-PostgreSQL, RabbitMQ, ZeroMQ and Bitmagnet backends.

## Documentation

| Guide | Audience |
|---|---|
| [`UPGRADE.md`](UPGRADE.md) | Breaking changes per release |
| [`docs/migration_0.8.0.md`](docs/migration_0.8.0.md) | Step-by-step migration from the legacy PHP/Symfony version |
| [`docs/container.md`](docs/container.md) | Running the image: services, volumes, healthchecks, shutdown, resources |
| [`docs/web-deployment.md`](docs/web-deployment.md) | Web service configuration, database roles, reverse proxy, administrator commands, user migration |
| [`docs/crawler.md`](docs/crawler.md) | Running `magnetico crawl` on a separate machine with `magnetico ingest` next to PostgreSQL |
| [`docs/operator-statistics.md`](docs/operator-statistics.md) | Ingest statistics and the administrator section |
| [`docs/projects_differences.md`](docs/projects_differences.md) | How 0.8.0 differs from `boramalper/magnetico` and `tgragnato/magnetico`: lineage, architecture, rationale and trade-offs |
| [`docs/development.md`](docs/development.md) | Building, testing and contributing; links to the testing, release-engineering and engineering-standards guides |
| [`docs/specs/`](docs/specs/) | Normative contracts |

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

- Primary repository: [git.skobk.in/skobkin/magnetico-web](https://git.skobk.in/skobkin/magnetico-web) (Forgejo). GitHub is a mirror.
- Go module path: `github.com/skobkin/magnetico-web` (served from the GitHub mirror).
- CI: Woodpecker. Native binaries are published to Forgejo releases, images to Docker Hub and GHCR: see
  [`docs/release-engineering.md`](docs/release-engineering.md).
