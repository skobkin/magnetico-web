# Upstream cleanup report — task 02

Task: [`docs/wip/02-upstream-cleanup.md`](02-upstream-cleanup.md) — scope and prepare the
imported Go fork for Magnetico 0.8.0. Executed 2026-10-23 on
`refactor/0.8.0-unify-and-new-storage`, base `f0dffb04` (task-01 tip), pinned upstream
import SHA `f80660653e7d1045b95d020a60b12a08e9a3c9b3`. Phase 2 of 3. **Stopping here for
owner review; phase 3 (codec + PostgreSQL persistence) has not been started.**

## Commits

| Commit | Content |
|---|---|
| `f89ae511` | **Safety-first:** `MakeDatabase` fails closed for every scheme (`ErrStorageNotImplemented`) before any other change |
| `93be0d6d` | Entrypoint restructured into `magnetico` subcommands (`crawl` / `web` / `migrate` placeholders); argv[0] dispatch, `-d/-w/-a/-c/-t/-e/-i` flags, credentials loading and SIGHUP reloader removed |
| `225e6ebf` | `web/` package deleted (UI, Basic Auth, API/RSS, embedded assets); `brotli`, `gomponents`, `x/crypto` dropped |
| `5b23e4a2` | All storage backends deleted (incl. legacy PostgreSQL v0→v1 auto-migration); `persistence` reduced to the crawler surface + permanent fail-closed guard; `pgx`, `go-sqlite3`, `zmq4`, `amqp091-go`, `go-sqlmock`, `klauspost/compress` dropped |
| `536e9c27` | `.github/`, `doc/`, `Dockerfile` deleted; engineering standards moved to `docs/engineering-standards.md` and rewritten; `CLAUDE.md` replaced by a minimal pointer; README/ci.yaml refreshed |
| `4f3c9c80` | ENV configuration: `CLI > ENV > config file > defaults`, strict unknown-key handling |
| `67e96aaa` | Module renamed to `github.com/skobkin/magnetico-web` (isolated, revertible commit) |

`git diff f0dffb04..HEAD -- AGENTS.md UPGRADE.md COPYING docs/specs docs/wip` is **empty**
(seed docs, specs, `COPYING` and `UPGRADE.md` byte-identical); the only `docs/wip`
addition is this report. `master`, tags and `origin` untouched.

## Safety-first ordering

The owner required that no intermediate commit be able to run the legacy migration. The
upstream constructors (`makePostgresDatabase`/`makeSqlite3Database`, …) are package-private
and reachable only through the `MakeDatabase` URL-scheme factory (plus tests that never
touch a server), so `f89ae511` (fail-closed factory) made the entire dangerous surface —
`setupDatabase()`, the `files`→`files_old` rename, the 100 `PARTITION BY HASH (id)`
partitions with per-partition `files.path` GIN trgm indexes, the `files.path ILIKE`
predicate, the SQLite `user_version` migrations — unreachable from the binary **before**
the CLI was restructured or any backend file was deleted. The backends remained as dead
code until `5b23e4a2` removed them.

## Removed components and rationale

- **Old web UI** (`web/`, 33 files): upstream `magneticow` (gomponents HTML, vendored
  mustache/plotly/vanillatree JS, embedded static assets), Basic Auth over a bcrypt
  credentials file, `/api/v0.1/*` endpoints, RSS feed, compression middlewares. The 0.8.0
  UI will be a private authenticated application designed from the historical PHP
  `magnetico-web` behavior — not a reskin. Recoverable from Git history; reusable patterns
  noted in the handoff list below.
- **Storage backends** (SQLite3/cgo/FTS5, legacy PostgreSQL + Cockroach URL alias,
  RabbitMQ, ZeroMQ + `!cgo` stub, Bitmagnet, iso8601 helper): 0.8.0 is PostgreSQL-only;
  the legacy PostgreSQL code contained the automatic `v0→v1` migration and path-search
  machinery that must never run. `MakeExport`/`MakeImport` (JSONL/zstd) removed with them.
- **CLI surface:** combined daemon+web run mode, `magneticod`/`magneticow` argv[0]
  dispatch, web flags (`-a`, `-c`, `-t`), export/import flags (`-e`, `-i`), the bcrypt
  credentials file loader and the SIGHUP reloader. Replaced by explicit subcommands; only
  `crawl` is real this phase.
- **Scaffolding:** `.github/` (Actions/CodeQL/release workflows, templates, dependabot,
  upstream CHANGELOG and copilot instructions — canonical forge is Forgejo + Woodpecker,
  Forgejo Actions disabled), upstream `doc/` (multi-backend config example, web-UI
  screenshots; `engineering-standards.md` moved and rewritten instead), upstream
  `Dockerfile` (zeromq/sqlite static-lib builds, `fts5` tag, binary symlinks, tgragnato
  labels — a new one arrives with the future CI task).
- **Dependency changes:** direct requires reduced 14 → 5 (`goccy/go-yaml`,
  `grafana/pyroscope-go`, `jessevdk/go-flags`, `multiformats/go-multihash`,
  `prometheus/client_golang`). Remaining indirects of kept deps: `klauspost/compress` +
  `cpuid` (via pyroscope-go), `x/crypto` (via go-multihash). No new dependencies were
  added.

## Kept components

`bencode`, `dht` + `dht/mainline`, `metadata` + `metadata/btconn`, `metainfo`, `merkle`,
`types/infohash`, `types/infohash-v2`, `stats` (process-wide crawler diagnostics; its
`/metrics` HTTP transport disappeared with `web/` and returns with the future UI —
`stats.MakePrometheusHandler` stays exported), `opflags` core, `.golangci.yml`,
`.gitignore`, `COPYING` (pure AGPL-3.0, untouched). Crawler flags, defaults and config
keys are byte-identical to upstream; DHT/crawler logic is unchanged.

## Persistence interface decision

`persistence` now contains: the `File` type (used by `metadata`), a narrow
`Database` interface (`DoesTorrentExist`, `AddNewTorrent`, `Close`), the fail-closed
`MakeDatabase`/`ErrStorageNotImplemented`, and a permanent table test over all former URL
schemes. The old read-side methods (querying, statistics, ordering) were deliberately not
carried over: `docs/specs/postgresql-persistence-v2.md` §3 defines a materially different
`Store` shape (context-aware methods, typed `AlreadyExists`), and keeping the old surface
would preserve compatibility code for unwanted legacy schemas — a documented deviation
from the task text's "crawler and the future web" wording, resolved in favor of the spec.
No test adapter/fake `Database` was added (owner decision): nothing requires one, and
`crawl` fails closed before any network activity.

## CLI and configuration

- Single binary `magnetico`; `crawl` runs the crawler, `web`/`migrate` are explicit
  placeholders printing a reason and exiting non-zero. `--config-file-path` is global and
  must precede the subcommand.
- Configuration precedence (commit `4f3c9c80`): **CLI > ENV > config file > defaults**.
  ENV names: `MAGNETICO_CRAWL_<LONG_NAME>` (e.g. `MAGNETICO_CRAWL_INDEXER_MAX_NEIGHBORS`,
  lists comma-separated) and `MAGNETICO_CONFIG_FILE_PATH`; empty values treated as unset.
  Config-file keys unchanged from upstream; unknown YAML keys and unknown
  `MAGNETICO_CRAWL_*` variables are hard errors. Sparse files never clobber defaults with
  zero values (presence-aware merge).
- Behavior change vs. upstream: the config file no longer overrides explicitly given CLI
  flags (the old file-over-CLI quirk) — the documented hierarchy replaces it.
- **Config architecture evaluation (owner directive):** `koanf` was evaluated and
  rejected for this phase. It would add `koanf` (+ mapstructure, and typically
  spf13/pflag) and effectively replace the CLI parser (help output, flag semantics,
  repeatable flags), while go-flags already provides explicit-set detection and goccy
  provides `yaml.Strict()` for unknown-key rejection — the full hierarchy costs ~200
  lines with zero new dependencies at the current surface (one command, nine options).
  Revisit only if the option surface grows substantially; the layering in
  `opflags/config.go` is table-driven and extensible.

## Documentation changes

- `docs/engineering-standards.md`: adapted from the upstream standards (AGPL attribution
  noted in the header); persistence section rewritten for PostgreSQL-only, fail-closed,
  no-automatic-migrations, specs-are-normative; dependency list updated; DHT/concurrency/
  testing guidance kept.
- Root `CLAUDE.md`: minimal pointer to `AGENTS.md` (authoritative), the standards,
  `docs/specs/`, `README.md`; no rule duplication.
- `README.md`: phase-2 state, build/test, usage example, config keys; explicit distinction
  between released PHP 0.7.x versions (`master`, `UPGRADE.md`) and this unreleased Go
  branch; Go module path documented. `UPGRADE.md` stays the ongoing upgrade changelog —
  the 0.8.0 entry will be written at release time, not before.
- `.woodpecker/ci.yaml`: the cgo `apk add` line is gone (pure Go now); build + test on
  `golang:1.27-alpine`; `docker-publish` still commented with the existing
  `DOCKER_LOGIN`/`DOCKER_TOKEN`/`DOCKER_REPO` secret references for reuse. Note: the
  pipeline was validated locally only; its first real run happens on the post-review push.

## Outstanding issue: BitTorrent v2 / hybrid ingestion (corrected framing)

Preserved as an explicit audit item, per spec §10.4 and the owner's correction: the
20-byte truncation of SHA-256 infohashes for DHT operations is **expected BEP 52
behavior**, not a defect. The genuine limitation is **ingestion**: metadata retrieval is
BEP 9 `ut_metadata` with SHA-1 verification only; v2-only and hybrid torrents are not
ingested (no v2 file-tree/piece-layers handling in the fetch path), and the hybrid
v1-hash semantics are undecided. The v2 type machinery (`types/infohash-v2`,
`metainfo` file-tree/BEP 52 parsing, `merkle`) is retained unchanged for that audit. No
BEP 52 code was touched in this task.

Other handoff notes: the legacy PostgreSQL backend silently dropped non-UTF-8/zero-size
metadata (removed with the backend; phase 3 must follow the spec's no-silent-
normalization rule); known upstream shutdown gaps remain (SIGINT-only handling,
`metadataSink.Terminate()` never called on shutdown, indexer ticker goroutine) — noted,
deliberately not fixed to avoid crawler behavior changes in this task.

## Verification results (exact)

- `go build ./...` → exit 0. `go vet ./...` → clean. `gofmt -l .` → empty.
- `go test ./...` → **all 12 packages pass** (root package has no test files):
  bencode, dht, dht/mainline, merkle, metadata, metadata/btconn, metainfo, opflags,
  persistence, stats, types/infohash, types/infohash-v2. The two failures inherited from
  task 01 (persistence sqlite3, web statistics) are gone with their packages.
- **Diagnostic correction to the task-01 report:** the recorded
  `persistence` "nil-pointer panic at sqlite3_test.go:126" was root-caused during this
  task: plain `go test` compiles `mattn/go-sqlite3` **without** the `fts5` build tag
  (upstream's Dockerfile used `--tags fts5`), so `newDb` failed with
  `setupDatabase sql.Tx.Exec (v2 -> v3) no such module: fts5`, returned a nil `Database`,
  and the first use panicked — the exact failing test varied between runs because
  `t.Parallel()` scheduling decides which test dereferences first. With
  `go test -tags fts5 ./persistence/` the whole legacy package passed. Moot after `5b23e4a2`
  (package deleted), but the "deterministic upstream bug" characterization in task 01 was
  wrong; the `web`/`statistics.go:123` panic was not investigated further for the same
  reason.
- Acceptance greps over `*.go`: no `files_old|pg_trgm|partition|ilike`; no
  `StartWeb|HandleFunc|basicAuth|/api/v0.1|gomponents`; no
  `MakeExport|MakeImport|SimpleTorrentSummary|OrderingCriteria|databaseEngine`;
  backend names appear only in the fail-closed factory's scheme table/test.
- Module checks: direct requires = the 5 keepers; `sqlite|zmq|amqp|pgx|brotli|gomponents|sqlmock`
  absent from the entire module graph; `klauspost/compress` and `x/crypto` present only
  as indirects of kept dependencies (pyroscope-go, go-multihash).
- Runtime smoke (no database, no production contact): no command → usage error; `crawl`
  (default URL), `--database sqlite://x`, `postgres://x` → `ErrStorageNotImplemented`
  message naming the requested scheme, exit 1, before any network activity
  (`MakeDatabase` precedes pyroscope and DHT startup); `web`/`migrate` → placeholder
  message, exit 1; `crawl --help` → exit 0. Config precedence smoke: ENV-only value used;
  sparse config file applied without touching other defaults; CLI flag beat both ENV and
  file; `MAGNETICO_CONFIG_FILE_PATH` honored; unknown YAML key →
  `config file: [2:1] unknown field "bogusKey"` + exit 1; unknown
  `MAGNETICO_CRAWL_BOGUS` → error listing all valid variable names.
- Seed integrity: diff over `AGENTS.md UPGRADE.md COPYING docs/specs docs/wip/*` (except
  this report) vs `f0dffb04` → empty; no `*.php` files in the tree; `web/`, `.github/`,
  `doc/`, `Dockerfile` absent.

## Compatibility caveats

- CLI is incompatible with upstream invocations: subcommands are mandatory, there is no
  combined daemon+web mode, and `magneticod`/`magneticow` argv[0] names no longer change
  behavior. Crawler flag names/defaults are unchanged; config file keys are unchanged.
- `crawl` cannot store anything until phase 3; it exits with `ErrStorageNotImplemented`
  for every database URL by design.
- CI validated locally only; first pipeline run happens on the post-review push.

## Phase-3 handoff list

1. Implement `docs/specs/postgresql-persistence-v2.md` §2 DDL + §3 `Store` +
   §4/§5 semantics (encode-outside-transaction, typed errors, no-blob rule).
2. Implement `docs/specs/wire-format-v1.md` (codec v1, golden vectors, limits).
3. Wire `crawl` to the new store (replace the fail-closed factory error path; keep the
   fail-closed contract for missing/unsupported configurations), re-expose `/metrics`.
4. Decide the BitTorrent v2/hybrid ingestion audit outcome before schema freeze
   (`info_hash` length check is intentionally unresolved).
5. New Dockerfile + real Woodpecker pipeline (publish step, ephemeral PostgreSQL for
   integration tests); replace the placeholder.
6. `magnetico web` (new authenticated UI from the PHP behavior) and `magnetico migrate`
   (offline migrator, verify-decode/verify-content) as separately specified phases.
7. Revisit configuration layering only if the option surface grows (see config
   evaluation above); consider ENV support for the future `web` process in the same
   style (`MAGNETICO_WEB_*`).
