# Agent Task 04A — Offline import of original Magnetico PostgreSQL torrents

**Status:** Implementation assignment, subject to owner approval. **Branch:** `refactor/0.8.0-unify-and-new-storage` of `skobkin/magnetico-web`. **Parent contract:** `docs/specs/legacy-torrent-migration-v1.md` (the normative document delivered alongside this task; put it in the repo before starting implementation). **Language:** Go.

## Objective

Implement the core of `magnetico migrate torrents`: read **original boramalper/magnetico v0.12.0** PostgreSQL `magneticod` (unpartitioned `files`, `migrations.schema_version=0`), validate and encode each complete historical torrent with the **existing production codec**, bulk-load exact source IDs into a fresh 0.8.0 PostgreSQL database, keep authoritative transactional checkpoints and an error registry, resume after interruption, report progress, and optionally defer and finalize three secondary indexes.

**This is not** the later `tgragnato` migration. The source schema is the frozen **original release** version, not whichever source fork was imported into this branch. Do not implement users, verify-content, retry-errors, forensic export, cleanup, the new web or the future crawler→ingest transport in 04A. Those are separately assigned later; preserve sufficient data for them.

**No production database, real user dump or deployment is authorized.** Use only disposable small test databases/fixtures for this task. The 3.46M-real-torrent, 123M-file source sample will be exercised in a later comprehensive Task 04D, after 04A–04C are reviewed.

## Inspect before coding

Read in full:

1. `docs/specs/legacy-torrent-migration-v1.md` (normative Task 04 contract).
2. `docs/specs/postgresql-persistence-v2.md`, `docs/specs/wire-format-v1.md`, `docs/engineering-standards.md`.
3. `docs/wip/03-storage-persistence.md` and `docs/wip/storage-implementation-report.md`, current `persistence/` and `codec/`, `opflags/`, `main.go`.
4. Original `boramalper/magnetico` **v0.12.0** `pkg/persistence/postgres.go` for source DDL; distinguish it from `tgragnato/magnetico`'s partitioned files.
5. The real benchmark's measurement summary if available as a **read-only** reference; don't copy benchmark harness into this repo. Its real-data import used 4–8 workers and batch `COPY`, but is not a production migrator implementation.

Confirm current branch HEAD and a clean tree; do not re-import upstream, merge master, modify `master`, or alter existing normative specs for convenience. A discrepancy needing a schema/wire change is a **stop-and-report decision**, not license to change the contract.

## Work package A — precise source adapter and preflight

Build a private `legacy/original` source adapter inside `internal/migration/`, with old database record types **separate** from new `persistence.Torrent` and future ingest types. Do not build a universal migration framework. A future `legacy/tgragnato` adapter should be possible without rewriting the import pipeline, but implementing it now is not needed.

- Read source via **schema-qualified, SELECT-only** SQL; require/encourage a read-only source user.
- Identify exact original v0 schema in `magneticod`: `torrents(id, info_hash, name, total_size, discovered_on)`, ordinary unpartitioned `files(id, torrent_id, size, path)`, `migrations` version 0, appropriate constraints and `files(torrent_id)` lookup index.
- Refuse a partitioned v1 fork, unsupported schema version, missing indexes/mandatory columns, ambiguous mapping, nonempty or incompatible target state.
- Source and destination must be distinct actual PostgreSQL databases; reject equal/aliased identity rather than comparing URL text only.
- Record source identity and COUNT/MIN/MAX ID, file count and optional NULL-owner file count; test bounds/capacity for target INTEGER IDs. No source DDL, manual repairs or VACUUM.
- Do not require privileged access to postgres internals for ordinary source identity; document which parts of the fingerprint are weak, and require frozen offline source.

**Configuration:** introduce `MAGNETICO_MIGRATION_SOURCE_DSN` (+ config `migrateSourceDatabaseURL`) for source. Reuse existing `MAGNETICO_MIGRATE_DATABASE` / `migrateDatabaseURL` for target, preserving `migrate schema-init` semantics and existing CLI precedence. Do not print DSNs containing passwords or require credential-bearing command-line flags; source missing must never fall back to target.

## Work package B — bounded source reader / encoding pipeline

- Use stable keyset pagination on `torrents.id` (not OFFSET), plus bounded set-oriented `files` reads via the source `files(torrent_id)` index. Avoid N+1 queries, giant SQL JSON aggregates, and in-memory storage of the full source population.
- Preserve **all** file records and duplicate `(path, size)` entries. Canonical sort is delegated to the production codec, not a destructive source rewrite.
- Reuse existing `persistence` validation and no-blob eligibility semantics. Add a minimal internal shared seam for unexported validation helpers if needed, tested for behavior parity; do not duplicate validation rules.
- Use the existing production Compact codec and its limits. Encode outside target transactions. An immediate Decode + canonical-content comparison is strongly preferred, but do not use re-encoded byte equality as the correctness check due to the separately tracked pooled-Brotli determinism issue.
- Bound worker count, read buffers, file-entry counts, total in-flight decoded path bytes and prepared DB rows/blobs; maintain backpressure. One valid large torrent may exceed a *batch* soft target without being rejected by a stricter, invented migration-only file limit.
- Default `--workers 4`, `--batch-size 256`; batch size must be a **hard maximum** (remember the benchmark's former overfilled-batch accounting bug). Provide resource limits and sensible, documented tuning options; keep one ordered transactional writer by default.
- Results can complete out of order; only commit/advance a contiguous range of processed **source IDs**. Preserve gaps in numeric ID space correctly. Every scanned record produces one success or a categorized per-record failure.

## Work package C — transactionally safe target writer and run state

Implement a small `magnetico_migration` operational schema with `progress` and `errors` per the normative contract, **separate** from permanent `magneticod.schema_version`. Include stable migration/run IDs, source fingerprint/schema version, source bounds/counts, last processed ID, counters, stage/profile, options and timestamps. Errors support record/run scope with source IDs, typed error codes/stages, safe diagnostics, attempts and resolution status.

- On a **fresh, schema-initialized and empty** target, create/initialize run state. Strictly refuse unknown preexisting rows/partial schema or stale foreign run state.
- Within one `pgx` transaction: bulk insert explicit-ID torrents (`CopyFrom`), corresponding blobs, all per-record errors, and the monotonically increasing contiguous checkpoint. If the transaction fails, checkpoint and data must both be unchanged.
- Treat malformed record/codec-limit violations as errors to persist and skip; treat target conflicts, infrastructure/transaction failures, or unknown per-record ownership as fatal. Never silently repair data.
- An ambiguous COMMIT outcome is reconciled against the DB checkpoint and actual rows before continuing. JSON/filesystem checkpoints are **non-authoritative**.
- Exclude concurrent migrant instances via a run-scoped advisory lock. Handle SIGTERM/SIGINT gracefully and SIGKILL/reboot idempotently. A run-level incident should be saved if possible on fatal failure; when the target is inaccessible, log a secret-safe local correlation ID, not an impossible guaranteed DB event.
- Expose clear progress: exact `processed/total`, migrated, per-record errors, throughput, elapsed, rolling ETA, current phase and index-finalization progress. TTY progress display and periodic structured non-TTY logs. Treat resume counts as cumulative and honest.

## Work package D — explicit secondary index deferral and finalization

`migrate schema-init` currently creates all three non-constraint indexes. Provide `--defer-secondary-indexes`, **opt-in only**, permitted at start of an empty validated migration run. It may drop *only* `magneticod.idx_torrents_total_size`, `magneticod.idx_torrents_discovered_on` and `magneticod.idx_torrents_name_gin_trgm` after catalog verification and state initialization. Do not touch PK, UNIQUE, FK, CHECK or permanent schema version.

Once the last source ID has been processed, commit the `data_loaded` phase, build/verify the three secondary indexes using recoverable operations, reseed the target identity sequence safely, and run `ANALYZE`. Distinguish `import_complete` and `import_complete_with_errors`; **neither means verified for cutover**. A crash during the index phase must resume from catalog and migration state, not recreate data or treat missing secondary indexes as general application schema validity. The normal runtime's strict schema checks remain unchanged.

If deferral was not requested, maintain indexes throughout; still perform safe identity reseed and post-load checks. Do not disable PostgreSQL durability or integrity constraints.

## Work package E — focused tests and docs

Tests are required **inside the main project** for the feature being added, but do not perform the full 3.46M-record integration benchmark now. At minimum:

1. Original v0.12.0 source schema accepted, partitioned `tgragnato` v1 schema refused, unexpected layout/role/index refused; no source writes observed.
2. Distinct DSNs and secret redaction; missing source config refuses; original `schema-init` and `crawl` config unchanged.
3. Empty target first run, unknown nonempty target refusal, authorized partial resume.
4. Explicit ID preserved; single-file no-blob, atypical single-file blob, multi-file with duplicate paths and zero-size member files, invalid total, absent files, file-count/codec limit errors.
5. Atomic batch data + blob + error + checkpoint; last-ID gaps; strict 256 batch cap; missing/broken blob insert rolls back all.
6. Interruption before commit, after commit but before client acknowledgement, repeated resume, concurrent-run lock, source-fingerprint mismatch on resume.
7. All three deferred indexes absent only during authorized run; PK/FK/UNIQUE preserved; interrupted/restarted index build and finalization; normal `MakeDatabase` refuses incomplete index state.
8. Correct cumulative counters and report fields; bounded memory, cancellation/drain without goroutine leaks.

Tests with disposable PostgreSQL are not optional; record which actually ran. Run `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race` on affected packages, and project formatting/lint checks where available. Document reproducible Docker/test commands; never claim a suite passed if skipped.

Update `docs/wip/README.md` to include the new task sequence, and write `docs/wip/04A-core-migrator-report.md` with changed packages, assumptions, preflight checks, exact tests, crash/resume proof, known limitations, and hand-off requirements for 04B–04D. Implement in focused commits and **stop for owner review**, no push without approval.

## Explicitly excluded from Task 04A

- Full `verify-decode`, full source-vs-target `verify-content`, `retry-errors` logic — **04B**.
- JSONL `errors export --include-records/--include-blobs`, cleanup command — **04C**. Error records must already contain enough IDs/stage/category to support them.
- Multi-million-row real sample restore/complete run and fault-injection campaign — **04D** (owner-led separate test pass using main repo code).
- Symfony accounts/invites/API tokens, password-reset tokens (not migrated), replacement web UI, crawler queue and ingest API — later unrelated tasks.
- Changes to production `codec` for Brotli pooling; the real benchmark reproduction is a separately tracked **pre-production issue**. Do not assert the within-library-version A→B problem is covered by tests that only repeat the same file set.
- Automatic `tgragnato` v1 source migration, source DDL, destination table-schema change, wire-format change, production cutover or any destructive source action.

## Acceptance gate

The implementation is acceptable for **04A review** only when exact legacy v0 source detection, bounded complete-source reads, no-blob/codec correctness, exact historical IDs, atomic target checkpoint+errors, reliable crash/resume, safe secondary-index lifecycle, secret-safe diagnostics and focused PG tests all work. Success must not be described as production-ready migration: no full `verify-content`, retry/export/cleanup, full-scale sample test or production cutover is included yet.
