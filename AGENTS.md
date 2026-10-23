# Agent guidance — Magnetico 0.8.0 unification

## Repository and development status

- This is the existing `magnetico-web` repository. The Go unification is developed **only** in `refactor/0.8.0-unify-and-new-storage` until explicitly merged by the owner. Do not create a new repository or alter `master`.
- The owner has committed a cleanup of PHP files and a seed-documentation commit **before** the upstream Go import. Preserve these commits and preserve `AGENTS.md`, `UPGRADE.md`, `docs/specs/` and `docs/wip/` during the import.
- Historical PHP/Symfony source, templates, API behavior and database schema remain available in `master` / Git history as **read-only references**. They are not current runtime sources.
- Initial Go source must be pinned to a specific commit of `https://github.com/tgragnato/magnetico`, a fork of `https://github.com/boramalper/magnetico`. Preserve Git provenance if feasible and keep original notices and the upstream **AGPL-3.0** `COPYING`. The historical PHP project used MIT; do not mislabel AGPL-derived code as MIT. Preserve applicable notices for reused MIT material.
- Target milestone is unified Magnetico `0.8.0`: one Go codebase, one future CLI executable with separate `crawl`, `web`, `migrate` commands, separate crawler/web containers or hosts, PostgreSQL-only in the first release, new compact file-list storage and a private authenticated web UI to be implemented later.

## Task order and scope

Work through **one approved task at a time** (all in `docs/wip/`):

1. `01-import-go-upstream.md` — pinned upstream import, Git history and attribution, no behavior changes.
2. `02-upstream-cleanup.md` — remove irrelevant upstream functionality and unsafe old DB auto-migration, prepare Go crawler foundation.
3. `03-storage-persistence.md` — codec v1 and fresh PostgreSQL schema/persistence plus tests; do not implement migrator or web UI yet.

Normative contracts: `docs/specs/wire-format-v1.md` and `docs/specs/postgresql-persistence-v2.md`. If task text and a normative specification disagree, stop and ask for the owner's decision; do not silently change wire format, schema contract or data-compatibility promises. Working reports go in `docs/wip/`; stable user-facing documentation may go directly under `docs/` later.

## Safety and engineering

- **Never connect to, write to, migrate or deploy to production infrastructure or the original torrent database.** No production cutover until crawler, torrent and user migration, and replacement web UI are all complete and approved. Use only explicitly disposable development/Test DBs.
- Do not run legacy automatic `tgragnato` PostgreSQL migrations or recreate its 100 `files` partitions on an existing database. The new version uses compact blobs, and a separate, later, offline migrator will read the old schema directly.
- Respect existing saved torrent IDs; don't silently renumber. Do not drop tables in real databases. No loss of filenames, sizes, duplicates or unknown metadata through silent normalization.
- Prefer Go standard library, narrow interfaces and incremental verifiable changes. Keep Go code/tests buildable where feasible; use ephemeral PostgreSQL 17+ for integration tests. Pin Go/toolchain and module dependencies deliberately.
- Do not add migration/backward-compatibility logic or broaden database backend support on your own. PostgreSQL is the only release 0.8.0 backend.
- Comments and implementation documents should be in English. Avoid unnecessary dependencies, architecture rewrites and speculative features.
- Never force-push, rewrite published Git history, push images or change CI production secrets without explicit authorization. Treat upstream fetched code as untrusted input for execution and dependency decisions.
- For failures, provide exact commands/test outcomes and actionable evidence. Do not claim tests passed unless run. Distinguish source review from test execution.

## Later work, not included in the three tasks

Torrent data migration (`migrate torrents`) with resumable checkpoints, progress/ETA, selective retry and forensic JSONL export; `verify-decode` and `verify-content`; separate migration of PHP users and credentials; new authenticated Go web UI compatible with essential PHP endpoints; Woodpecker CI and OCI distribution; final single offline production cutover. These require separate owner-approved specifications/tasks.
