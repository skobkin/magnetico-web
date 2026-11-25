# Agent guidance — Magnetico 0.8.0 unification

## Repository and development status

- This is the existing `magnetico-web` repository. The Go unification is developed **only** in `refactor/0.8.0-unify-and-new-storage` until explicitly merged by the owner. Do not create a new repository or alter `master`.
- The owner has committed a cleanup of PHP files and a seed-documentation commit **before** the upstream Go import. Preserve these commits and preserve `AGENTS.md`, `UPGRADE.md`, `docs/specs/` and `docs/wip/` during the import.
- Historical PHP/Symfony source, templates, API behavior and database schema remain available in `master` / Git history as **read-only references**. They are not current runtime sources.
- Initial Go source must be pinned to a specific commit of `https://github.com/tgragnato/magnetico`, a fork of `https://github.com/boramalper/magnetico`. Preserve Git provenance if feasible and keep original notices and the upstream **AGPL-3.0** `COPYING`. The historical PHP project used MIT; do not mislabel AGPL-derived code as MIT. Preserve applicable notices for reused MIT material.
- Target milestone is unified Magnetico `0.8.0`: one Go codebase, one future CLI executable with separate `crawl`, `web`, `migrate` commands, separate crawler/web containers or hosts, PostgreSQL-only in the first release, new compact file-list storage and a private authenticated web UI (implemented in the W1–W8 milestones; see `docs/wip/README.md`).

## Task order and scope

Work through **one approved task at a time** (all in `docs/wip/`):

1. `01-import-go-upstream.md` — pinned upstream import, Git history and attribution, no behavior changes.
2. `02-upstream-cleanup.md` — remove irrelevant upstream functionality and unsafe old DB auto-migration, prepare Go crawler foundation.
3. `03-storage-persistence.md` — codec v1 and fresh PostgreSQL schema/persistence plus tests; do not implement migrator or web UI yet.

The numbered list above is the original three-task sequence. Later tasks (04A–04D migrator, 05 codec determinism, 06 crawler/ingest, 07A–07I web design and implementation, user migration and fixtures) are indexed in `docs/wip/README.md`; tasks 07A–07J have been accepted by the owner. The Go web UI was built **one milestone per owner-approved task** (W1–W9, `docs/wip/07A-web-design-plan.md` §8; W9 is task 07J: integration, security and regression, accepted). Task 08 (`docs/wip/08-ci-oci-plan.md`, report `docs/wip/08-ci-oci-report.md`: repository cleanup, CI/OCI and release engineering) is implemented and awaits owner review; Task 09 (statistics and the administrator section, `docs/wip/09-statistics-admin-plan.md`) is implemented and awaits the owner's review of its report; never start work beyond the current assignment without an explicit owner assignment.

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

## API documentation (OpenAPI)

- Any change to a supported API endpoint, its authentication requirements, request parameters, response schema, error behavior, or other documented contract MUST update the corresponding embedded OpenAPI specification in the same change.
- The rule applies to both v1 and v2, including the RSS authentication contract (RSS tokens are separate from general API tokens).
- Tests SHOULD keep implemented routes and OpenAPI descriptions consistent where feasible.

## Architecture comparison document

[`docs/projects_differences.md`](docs/projects_differences.md) explains how Magnetico Web 0.8.0 differs from `boramalper/magnetico` and `tgragnato/magnetico`. Whenever a change introduces or modifies a **significant architectural difference** (storage, ingestion, migration, the web service and its interfaces, deployment), update that document in the same change:

- the **comparison table** and the explanation of the affected difference;
- the **rationale and trade-offs** (what changed, why, what it costs; not every difference is an improvement);
- every **Mermaid diagram** that shows the affected component, keeping diagrams in agreement with the implementation (no invented components, connections or guarantees);
- the **last-updated date** at the top.

Keep comparisons grounded in identifiable versions or commits of the other projects (tags or commit hashes, never "upstream" in general), cite measurements only with their dataset, configuration and report, and keep **implemented** behaviour separate from planned, researched or refused work (the non-goals section lists the latter and states they are not commitments).

## Statistics and the administrator section

Task 09 (`docs/wip/09-statistics-admin-plan.md`, report `docs/wip/09-statistics-admin-report.md`) added per-crawler credentials, the ingest statistics (`docs/specs/statistics-v1.md`, schema `magnetico_stats`) and a read-only administrator section of the web service. Rules that follow from it and that a later change must keep:

- **Privacy.** Statistics never store a payload, info hash, torrent name, file path, credential or token digest, and never associate a crawler with a torrent; `magnetico_stats` has no `bytea` or `text` column. The administrator pages never select `password_hash`, an invitation `code`, `token_hash`, or the quarantine `payload` and `info_hash` (explicit column lists; the tests use a role that holds nothing more). A credential is shown once, by the command that creates it, and never in a listing, log, statistic or page.
- **Read-only.** Every `/admin` route is `GET` only and registered through the `Admin`/`AdminJSON` access classes; there is no administrator mutation in 0.8.0, and the section's data functions take a role-checked principal that only the guard can create.
- **Statistics never affect ingestion.** A statistics failure must not change an admission decision or an authentication result; the recorder takes one mutex that is never held across admission I/O; no write per request.
- **Schemas are never repaired.** `magnetico_ingest` stays at version 1 and includes `crawlers`; an older development schema is refused and reinitialised by hand (`UPGRADE.md`); `magnetico_stats` is created by `migrate stats-schema-init` and never altered automatically.
- A change to what is counted, its boundaries or the schema updates `docs/specs/statistics-v1.md` in the same change (a test compares the documented metric names with the schema).

## Task 09A rules (pre-RC qualification follow-up)

Task 09A (`docs/wip/09A-pre-rc-qualification-report.md`) added rules that a later change must keep:

- **Advisory-lock keys live in `internal/advisory`** and nowhere else (`internal/repocheck` rejects a duplicate value, a key-sized literal elsewhere and a package that takes an advisory lock without importing the registry). A key shared on purpose is one constant used from several places.
- **The admin read-model cache never mutates a published entry**: a refresh replaces it, the newest last-known-good is served with its own fetch time, loads for distinct keys are bounded, and a cached overview or crawler page is aged to the present when it is served. Stale or cached data must say so on every page and chart.
- **Statistics stored per observation keep their meaning**: the quota utilisation is recorded with the quota in force at the sample, estimates carry their own flags per sample and per bucket, and the `last` storage values are one bundle from the newest sample (a measure it could not read stays missing; "not evaluated" is not "reconciled").
- **A request is an attempt as soon as authentication succeeds**, and every checked hash of an existence request is accounted for (found, missing or unknown), including failed and cancelled lookups.
- **Release engineering stays conventional** (Task 09B): one verification workflow (`ci.yaml`: every gate, no secrets, no privilege; pushes run for `master` only, every other branch through its pull request; a pull request runs everything except the race suite, and the migration end-to-end tests only when it changes migration paths — owner decision, see `docs/release-engineering.md`) and one tag-only workflow (`release.yaml`: GoReleaser for the Forgejo release, the Woodpecker buildx plugin for the image), as in the owner's other repositories; no custom publisher, promotion or release-verification tooling. Node exists only in the `frontend-selftest` CI step, pinned by version and digest, never in a build of the product.
- **A cancellation after a durable verdict is not an interruption**: a finish that fails ambiguously is reconciled against the stored row (`verify-decode`, `verify-content`).

## Later work, not yet started

The torrent migrator, its verification and forensic tooling, the user migration, the crawler/ingest pipeline and the authenticated web UI and API are implemented (see `docs/wip/README.md`). Task 08 added the repository layout (`cmd/magnetico`, `internal/app`, `internal/e2e/cli`, `internal/testkit`), the clean lint gate, `magnetico --version`, the temporary legacy-PHP-environment warning, the `Dockerfile`, `.goreleaser.yaml` and the Woodpecker workflows (`.woodpecker/ci.yaml`, `release.yaml`). They are **configured and verified locally only**: no pipeline has run on Woodpecker and nothing has been tagged or published. Still requiring separate owner-approved tasks: the first real pipeline run and the publication of a release candidate (never create tags or push images without explicit authorization), release-candidate testing including live-DHT acceptance (`docs/wip/rc0-acceptance-checklist.md`), and the final single offline production cutover. Owner decisions on distribution: Forgejo is the primary repository and GitHub a mirror; Woodpecker CI is the build system; `docker.io/skobkin/magnetico-web` and `ghcr.io/skobkin/magnetico-web` are the approved image repositories (linux/amd64 and linux/arm64); Forgejo hosts native release artifacts and is not an OCI publication target; the `latest` tag stays disabled until the owner enables it.

Where things are: test classes and exact commands, the skip census and the mapping of historical report commands to current names are in `docs/testing.md`; running the image in `docs/container.md`; the release path, tag grammar, required secrets and server configuration in `docs/release-engineering.md`. Production code never exports anything only for tests, no release build passes `-tags`, and the workflow security invariants are pinned by `internal/repocheck`.
