# magnetico-web (unified Magnetico 0.8.0 — under development)

This repository is being converted into a unified Go implementation of
[Magnetico](https://github.com/boramalper/magnetico), targeting the upcoming
**0.8.0** milestone: one Go codebase with a single future CLI providing
separate `crawl`, `web` and `migrate` commands, PostgreSQL as the only storage
backend for the first release, a new compact torrent file-list storage format,
and a private, authenticated web UI to be implemented later.

> **Status: under active development — not deployable.**
> Nothing in this branch is production-ready yet, and no production migration
> or cutover has happened or been approved.
>
> **Released versions are the PHP/Symfony 0.7.x application** on the `master`
> branch (see `UPGRADE.md` for their upgrade history). Everything on this
> development branch is unreleased Go work; no 0.8.0 release exists yet, and
> its breaking changes will be documented in `UPGRADE.md` when it is ready.

## Current state (phase 2 of the 0.8.0 roadmap)

The tree is a scoped crawler foundation:

- A single binary `magnetico` with subcommands:
  - `crawl` — the DHT crawler. **Storage backends are not implemented yet**:
    `crawl` refuses every database URL and exits before any network activity.
    The new PostgreSQL storage arrives in phase 3
    ([docs/wip/03-storage-persistence.md](docs/wip/03-storage-persistence.md)).
  - `web`, `migrate` — explicit not-implemented placeholders that exit
    non-zero.
- Removed compared to upstream (recoverable from Git history): the upstream Go
  web UI and Basic Auth, the JSONL export/import modes, and the SQLite,
  legacy-PostgreSQL (including its automatic schema migration and 100-partition
  file index), RabbitMQ, ZeroMQ and Bitmagnet backends.
- Crawler flags are unchanged from upstream; `--config-file-path` is now a
  global option that must precede the subcommand.

## Build and test

    go build ./...
    go test ./...

## Example

    magnetico --config-file-path config.yaml crawl --indexer-addr 0.0.0.0:6881

Optional `config.yaml` keys (all crawl settings, same names as upstream):
`databaseURL`, `pyroscopeURL`, `indexerAddrs`, `indexerMaxNeighbors`,
`leechDeadline`, `leechMaxN`, `maxRPS`, `bootstrappingNodes`,
`filterNodesCIDRs`.

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
- A GitHub mirror may be published later.
- No production container image has been published for the unified version.
