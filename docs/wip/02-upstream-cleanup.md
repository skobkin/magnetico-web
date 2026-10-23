# Agent task 02 — Scope and prepare the imported Go fork for Magnetico 0.8.0

**Prerequisite:** phase 1 completed and reviewed in `refactor/0.8.0-unify-and-new-storage` in `magnetico-web`; exact imported `tgragnato/magnetico` SHA recorded. **Phase 2 of 3.** Work in the **same branch**. No production deployment or data migration.

## Objective

Turn the faithful upstream Go snapshot into a **small, unified Go crawler foundation** suitable for the forthcoming blob storage/persistence implementation and, later, a completely new authenticated Go web UI based on the historical PHP `magnetico-web` behavior/design.

The new 0.8.0 application is intentionally **not a general-purpose upstream clone**. It will support **PostgreSQL only** initially, two separable runtime processes/containers using the same image (`crawl` and later `web`), a future one-off `migrate` command, and filename-independent torrent-name search. The actual blob codec and new PostgreSQL persistence schema belong to **phase 3**, not here.

## Read first

- The pinned upstream commit and phase-1 report; current entrypoint, flag parsing and package dependencies (`main.go`, `opflags`, `metadata`, `metainfo`, `persistence`, `web`, `stats`, Go tests).
- `docs/specs/wire-format-v1.md` and `docs/specs/postgresql-persistence-v2.md` (already committed in the seed package).
- Historical PHP behavior from `git show <original-php-sha>:...` as a **read-only reference**, not functionality to port yet.
- Upstream AGPL-3.0 obligations and notices; do not remove legitimate upstream authorship information while trimming code.

## Work packages

### A. Produce a precise inventory and proposed deletion plan

Classify each upstream feature/package/dependency as KEEP, REMOVE, REPLACE-IN-PHASE-3 or DEFER-TO-WEB. Explain its dependencies and test implications before making extensive deletions.

**Keep:** DHT discovery, metadata retrieval/verification, bencode/BEP parsing, useful crawler reliability improvements, reusable Go code and tests, essential diagnostics and clearly useful statistics. Preserve the BEP 52/v2-only/hybrid audit as an explicit outstanding issue. Do not assume that declaring BEP 52 types implies working v2 ingestion.

**Remove or isolate from the future production path:**

- public upstream Go web UI, Basic Auth, old templates/assets and original web routes; our replacement will be a private authenticated UI designed from the PHP app, **not** a reskin of `tgragnato/web`;
- search-by-filepath/file-content behavior (including the `files.path` search predicate), path GIN indexes and 100-partition index machinery;
- legacy **automatic** PostgreSQL schema migration `v0 -> v1` and any startup side effect that can rewrite an existing `files` table;
- upstream database/backend modes not supported by 0.8.0 (SQLite, CockroachDB, RabbitMQ, ZeroMQ, Bitmagnet, etc.) **once** their dependency edges are identified; trim only when it makes the module simpler and tests remain meaningful; retain a narrow interface for future database implementations;
- stale upstream docs, GitHub Actions, Docker/CI publication scripts and options that falsely imply the new project supports all upstream modes or is production-ready;
- unused CGO/native dependencies once their users are removed.

**Avoid deleting:** generic file metadata structures that the crawler needs, DHT runtime logic, useful test cases, simple reusable API helpers or observability hooks **solely because** the old web module used them. Preserve small useful pieces separately when justified. No scope creep into rewriting the crawler.

### B. Establish safe entrypoints and development boundaries

- Organize for a single future binary `magnetico` with `crawl`, `web`, and `migrate` commands. `crawl` is the only runtime to prepare in this phase; `web` and `migrate` may be explicit **not implemented** placeholders or omitted until the relevant phase. **No `serve` combined mode is required.**
- Ensure the code is buildable/testable on the current branch. This may require temporarily decoupling the crawler from the old persistence implementation, preserving a non-production test adapter, or deferring final DB wiring to phase 3. Choose the smallest safe approach and document it.
- **Fail closed:** a partly prepared build must never start against an old `magneticod` database and initiate schema creation, `v0→v1` migration, partition rebuilding or destructive writes. Until phase 3's new schema is ready, a PostgreSQL-backed production run must fail clearly or be explicitly disabled. No best-effort legacy schema adaptation.
- Maintain a narrow database persistence interface for the operations actually required by crawler and the future web; **PostgreSQL is the only implementation required in 0.8.0**. No generic universal database abstraction.
- Treat Go module path as an explicit decision: preserve it during phase 1; in phase 2 assess the canonical Forgejo module path and internal import changes; propose/implement a consistent path only after confirming its discoverability and module tooling compatibility. Do not introduce two conflicting module identities.
- Preserve meaningful upstream CLI flags when they serve the crawler; rename/remove unsupported options deliberately, with documentation. Avoid changing DHT behavior unnecessarily.

### C. Project scaffolding

- Replace upstream-facing README/usage examples with an accurate WIP description of `magnetico-web` 0.8.0 and attribution to `boramalper` and `tgragnato`; preserve AGPL `COPYING` and relevant notices.
- Prepare Go-oriented `.gitignore`, minimal test/build instructions and optional local development configuration. Prefer **Woodpecker** for future CI; do not import large workflows prematurely. Do not configure production Docker Hub/GHCR publishing or publish images in this phase.
- Keep normative documents under `docs/specs/` and work-in-progress reports under `docs/wip/` and reference the planned new `magneticod.torrents` + `torrent_file_blobs` model. Clearly mark that storage work follows in phase 3.

## Design and deletion constraints

- **Do not edit agreed specification content without requesting design approval. Do not implement the new codec, compress profiles, `torrent_file_blobs` DDL, or database conversion** here; those are phase 3 or later.
- Do not implement the one-time `migrate torrents/users`, repeat-errors, JSONL export, decode/content verification, production deployment, PHP web compatibility or template port here.
- Never delete a table, schema, migration record or real data as part of 'cleanup'. Clean **source code in the development branch only**.
- Work in focused commits (e.g. web separation, backend removal, legacy migration deactivation, entrypoints/module/docs). Do not commit one opaque large deletion without a reviewed inventory.
- If maintaining a compileable crawler requires making a transitional adapter that conflicts with the agreed final persistence design, **stop and propose a minimal alternative**; do not add long-lived compatibility code for unwanted legacy schemas.

## Verification and acceptance

- Go build and relevant crawler/BEP unit tests pass using pinned toolchain, or precise justified test exceptions are documented.
- No PHP runtime files remain in the branch (historical PHP sources remain in Git history). `AGENTS.md`, `docs/specs/`, and `docs/wip/` remain intact. Old PHP files remain recoverable from Git history.
- No old Go web UI or Basic Auth is reachable from the proposed 0.8.0 runtime.
- No automatic `files` migration, `files.path` search, path GIN index or 100-partition creation can be invoked from the future active code path.
- Obsolete backends are not exposed by the active CLI; retained backend code, if any, is explicitly justified and isolated.
- No production database is accessed. No containers are pushed or deployed.
- A short Markdown report `docs/wip/upstream-cleanup-report.md` provides: removed/kept components, rationale, `go.mod`/CI/CLI changes, exact tests run, pinned ancestry SHA, compatibility caveats and phase-3 handoff list.

**Stop after phase 2 and request the owner's review**. Phase 3 (codec + PostgreSQL persistence) starts only after the codebase is ready and the owner decides to proceed.
