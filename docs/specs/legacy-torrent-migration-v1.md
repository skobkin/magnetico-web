# Magnetico 0.8.0 — Offline Legacy Torrent Migration Contract (v1)

**Status:** Proposed normative specification for owner review; not implementation authorization. 2026-11-07.

**Applies to:** `magnetico migrate torrents`, `migrate torrents retry-errors`, `migrate verify-decode`, `migrate verify-content`, `migrate errors export`, and `migrate cleanup` in the `refactor/0.8.0-unify-and-new-storage` development branch. The implementation sequence is split into Tasks 04A–04D. This document defines the end-state contract; each task implements only its stated subset.

**Other normative contracts:** `docs/specs/wire-format-v1.md`, `docs/specs/postgresql-persistence-v2.md`, and `docs/engineering-standards.md`. The existing wire format, production schema, validation semantics, and public `persistence.Store` API are unchanged.

## 1. Goal and hard boundaries

Migrate the original **boramalper/magnetico v0.12.0** PostgreSQL `magneticod` database into a **separate, freshly initialized** Magnetico 0.8.0 PostgreSQL database. This is a one-time **offline, source-read-only** operation. Historical torrent IDs, information hashes, names, sizes, discovery timestamps, and complete file `(path, size)` *multisets* must survive without silent rewriting. Original file-row order is not part of the 0.8.0 codec contract.

The current production source is **not** the newer `tgragnato/magnetico` fork. The source adapter implemented by Task 04A targets only original v0 layout: an unpartitioned `magneticod.files`. A future adapter may support other layouts, but do not implement an unrequested fork migration now.

Never run the original fork's automated v0→v1 migration. Never alter source tables, sequences, indexes, schema, row values, or permissions. Do not run VACUUM/ANALYZE on the source. Use a source account with SELECT-only grants wherever possible. Stop the old indexer and PHP web application before migration. An offline source and a backup are operational requirements; opening read-only transactions alone does **not** prevent other clients from writing.

Production cutover is a separate, manual decision. Completing an import does not mean verification or cutover is complete, and never starts crawler/web automatically.

## 2. Exact source contract

Canonical reference: `boramalper/magnetico` release **v0.12.0**, `pkg/persistence/postgres.go`, initial (frozen) PostgreSQL DDL. The 10% real-world sample was independently identified as unpartitioned `migrations.schema_version=0` and successfully benchmarked. Use the original source release *and* the sample report as references; do **not** derive source DDL from `tgragnato/magnetico`.

Expected source (in schema `magneticod`, schema-qualified SQL only):

```sql
magneticod.torrents(
    id INTEGER PRIMARY KEY,
    info_hash BYTEA NOT NULL UNIQUE,
    name TEXT NOT NULL,
    total_size BIGINT NOT NULL,
    discovered_on INTEGER NOT NULL
)
magneticod.files(
    id INTEGER PRIMARY KEY,
    torrent_id INTEGER NULL REFERENCES magneticod.torrents(id),
    size BIGINT NOT NULL,
    path TEXT NOT NULL
)
magneticod.migrations(schema_version SMALLINT UNIQUE) -- exactly version 0
```

The source has sequences for torrent/file IDs and a usable index on `files(torrent_id)`; these sequences are **not** imported. Column and table inventory must check the *actual* catalog rather than trust the table sketch. `files` is an ordinary (`relkind='r'`), **unpartitioned** table. Table and constraint/index names may be inspected, but unexpected structure must be refused, not guessed or automatically modified. Detect and refuse a v1 partitioned fork (`files_p*`, partitioned parent), unsupported versions, missing mandatory columns/constraints, inaccessible data, or unexpectedly missing lookup indexes. Report exact detected state and a non-secret remediation hint.

`files.torrent_id` is nullable in original DDL. A NULL-owned file cannot be assigned to any torrent. Diagnose and count such anomalies at preflight; do not manufacture a link or silently transplant them. A non-NULL orphan indicates broken source integrity and must be reported, not repaired. Decide whether such unrelated invalid rows block by the explicit source-integrity policy below; none may be included in a valid torrent.

Do not assume all historical rows pass current validation merely because the benchmark sample did. Source values outside the new production codec/Store limits become per-record errors.

## 3. Target contract and preflight

Target must be an **independent** database with `magneticod.torrents`, `magneticod.torrent_file_blobs`, and `magneticod.schema_version=1`, as created by `magnetico migrate schema-init`. Check current production schema and `pg_trgm`. Source and target must not be the same PostgreSQL database (also reject aliased DSNs resolving to the same actual database identity). All source and target SQL must use qualified object names; never rely on `search_path`.

Initial import requires no pre-existing production torrents/blobs or unrelated migration run. A documented, validated resume may use a partially filled target **only if its persisted migration state proves ownership and consistency**. Otherwise refuse; do not infer that a database with rows is safe to resume. Do not clear or overwrite target rows automatically. Uniqueness or ID conflicts must stop the import (not be counted as ordinary bad-metadata skips).

Preflight checks source/target connectivity and roles, schema/version, source MIN/MAX/COUNT and counts of file rows, ID range and signed INTEGER headroom, `pg_trgm`, target's expected constraints/indexes, target emptiness or valid existing checkpoint, enough free disk for new data/indexes/WAL and any necessary PostgreSQL privileges. Display user-actionable failures without leaking DSNs, passwords or filenames.

Keep historical `id`; never renumber. The destination identity sequence must be reseeded **after** importing all valid rows, above the actual imported maximum. If there is no valid next `INTEGER` ID, fail clearly and raise a schema design question; do not truncate or silently upgrade to BIGINT.

## 4. CLI and secret-safe configuration

```text
magnetico migrate schema-init                 # already implemented; target only
magnetico migrate torrents [--workers 4] [--batch-size 256] [--defer-secondary-indexes]
magnetico migrate torrents retry-errors       # Task 04B
magnetico migrate verify-decode               # Task 04B
magnetico migrate verify-content              # Task 04B
magnetico migrate errors export --format jsonl [--include-records] [--include-blobs]  # Task 04C
magnetico migrate cleanup                     # Task 04C
```

Recommended defaults and naming:

- `MAGNETICO_MIGRATION_SOURCE_DSN` — old/source database URI; new, migration-specific setting. YAML key `migrateSourceDatabaseURL` if a config file is used.
- **Reuse existing** `MAGNETICO_MIGRATE_DATABASE` / YAML `migrateDatabaseURL` — new/target database URI, compatible with `schema-init`. Do not introduce a competing default target DSN.
- Optional `--source-db-env` naming the environment variable containing a source DSN; existing `--database` target configuration remains compatible. DSNs containing passwords must not be required on the process command line and must never appear in output. Consistent file/config precedence must be tested and documented.
- No implicit fallback from a missing source DSN to the target DSN. A source and target must both be explicitly resolved.
- `--workers=4`, `--batch-size=256` defaults; configurable bounded bytes-in-flight, file count, and connection budgets; `--defer-secondary-indexes` is explicit, not default. `--dry-run`/`preflight` may assess the source without writing target data; no accidental schema mutation in this mode.

Do not accidentally change the existing `crawl` or `migrate schema-init` configuration behavior.

## 5. Package boundaries

Keep legacy models and source SQL **internal to the migration package**, distinct from the new `persistence.Torrent`, `codec.File`, and future ingest models. Suggested organization (names are illustrative, separation is binding):

```text
internal/migration/
    legacy/original/     # source v0.12.0 models, schema probe, read-only reader
    pipeline/            # bounded batch coordination, per-record validation, codec
    target/              # private transactional bulk writer and index finalization
    state/               # migration progress/errors, resume identity
    progress/            # TTY and structured non-TTY rendering
```

The `legacy/original` adapter maps source fields to a neutral migration record. It may optionally retain source `files.id` for forensic reports, but `files.id` does not map into the new codec. Avoid designing a generic universal ETL framework. A future `legacy/tgragnato` adapter can be added independently without contaminating production persistence models.

Reuse the real production `persistence` validation rules and `codec.Encoder`/`codec.Decode`, including limits and the strict no-blob eligibility predicate. If an unexported helper prevents reuse, add a small and well-tested, behavior-preserving internal seam rather than duplicate validation logic. Do not call `Store.AddTorrent` per source torrent: it generates new IDs and is not the bulk import API.

## 6. Source read and ordering

Select source torrents by stable ID keyset, no OFFSET pagination:

```sql
SELECT id, info_hash, name, total_size, discovered_on
FROM magneticod.torrents
WHERE id > $1 AND id <= $2
ORDER BY id
LIMIT $3;
```

Read all associated `files` in **bounded, separate, set-oriented queries**, using `files(torrent_id)` and preserving duplicates. Do not issue one file query per torrent or create a huge SQL-side JSON aggregate. Order/group by `torrent_id`; if a source torrent has no files, treat it as an invalid record. Do not assume `files` come in ID order. Use one bounded source reader and 4 encoder workers initially, with independent connection bounds.

An oversized source torrent must not cause unbounded allocation or stall the pipeline: either process it within the production codec's allowed limits, or enumerate/drain its records and record a limit error, without retaining an uncontrolled file slice. A single valid large torrent should be allowed even when it exceeds the *per-batch* target; flush other records around it. Bounded in-flight work and backpressure are mandatory. Preserve each `(path, size)` duplicate; sort a copy canonically only via production codec.

Offline mode assumes the source remains unchanged for the entire run and verification. Record a sanitized source identity (server/database/schema identifiers where accessible, otherwise a documented fingerprint), selected source count/min/max, and sample anchors. Revalidate preflight identity and bounds on every resume; never claim these weak anchors *prove* that source data stayed unchanged. The operator must stop other writers; `verify-content` is the final equality gate.

## 7. Encode and write batches

For each torrent:

1. Retain source `id`, `info_hash`, `name`, `total_size`, `discovered_on`; derive `file_count` from **all** associated file rows.
2. Apply exactly the new ingestion validation and codec limits; reject invalid records as typed per-record errors (do not fix zeros, reformat names/paths, collapse duplicates, or recompute total size).
3. If and only if the single real file is `(Path==Name, Size==TotalSize)`, store no blob. Otherwise encode via production **Compact** profile (prefix/trie with Brotli q9 and uncompressed fallback). Encode outside the target transaction; verify new blobs by immediate decode and canonical equality if enabled by the integrity policy (recommended default in 04A).
4. Hand results, **including errors**, to an ordered commit coordinator. Never advance over a source ID for which no success/error outcome has been committed.
5. Write the successful subset of the batch with `pgx.CopyFrom` (explicit source IDs), related blobs, typed errors and a contiguous progress checkpoint in **one target transaction**. Honor the exact batch-size cap and bounded byte cap. No partially committed batch may be represented as completed.

On already-imported IDs during a replay: read authoritative DB checkpoint, not a JSON cache. A mismatching ID/infohash/metadata is a **fatal** conflict. Do not silently `ON CONFLICT DO NOTHING` and label the row migrated.

### Outcomes and abort conditions

- **Per-record:** invalid metadata, codec limit, missing source file list, impossible total-size sum, etc.: add an error row, mark this source ID processed-with-error, continue.
- **Fatal:** DB connection/transaction failure, SQL privilege/catalog errors, incompatible schemas, target conflicts, checkpoint/state corruption, cancellation during uncommitted batch, or any error that cannot be safely assigned to one source record: stop with non-zero status. Checkpoint does not advance on aborted transaction.
- A lost COMMIT acknowledgement is an *unknown outcome*: reconnect and inspect the authoritative checkpoint/rows before replaying, rather than assuming failure or success.

## 8. Transactional progress and errors

A migration-owned schema `magnetico_migration` is **temporary operational state**, distinct from permanent `magneticod.schema_version`.

Use at least:

- `progress`: run identity/type, sanitized source fingerprint, source schema adapter/version, expected total/min/max ID and file-row count, last **contiguously** processed source ID, processed/success/error counters, current phase (e.g. `importing`, `data_loaded`, `building_indexes`, `import_complete`, `import_complete_with_errors`), migration contract/codec version/profile, timestamps and import options needed for safe resume. The checkpoint is monotonic; gaps in source ID are normal.
- `errors`: generated error identifier; run ID; optional `source_id` and hash; scope (`record` or `run`); stage/category/stable error code; safe details (`JSONB`, length-limited); attempt count; unresolved/resolved status and timestamps. Ordinary per-record errors should be uniquely identifiable by `(run_id, source_id)` so retry is idempotent; run-level incidents require no record ID.

Do not store passwords, unredacted connection URLs, API tokens, or complete file paths in routine logs/errors. Record useful debug metadata (counts, limits, SQLSTATE when safe, expected/actual values, codec selection) without leaking full records. Later forensic export retrieves entire source and target records *on explicit request*; ensure 04A preserves enough identifiers/stage/context to support that.

For fatal failure, record a run-level incident **in a separate transaction where the target remains available**. If PostgreSQL is inaccessible, emit a local structured error log with a correlation/run ID and no secret values. Be explicit: DB-only forensic export cannot retrieve an incident that never reached the DB; after reconnection the operator may import/reconcile a journal entry. Never claim impossible cross-system atomicity.

Acquire a run-scoped PostgreSQL advisory lock, held for the lifetime of the migration, to exclude simultaneous migrations against the same target. Use **transactional** database checkpoint updates. Atomic JSON file rename is not a substitute for a DB transaction. Track both processed IDs and counters committed in that same transaction.

On clean SIGINT/SIGTERM, stop dispatching work, complete or roll back the current batch, release resources, and leave resumable state. SIGKILL/power loss is recovered from database state. Never bypass a missing expected checkpoint.

## 9. Secondary index strategy

Default `migrate schema-init` creates the complete schema including all three secondary indexes. With the explicit `--defer-secondary-indexes` flag and only at the beginning of a **validated empty** target run, the migrator may drop **exactly** these three known non-constraint indexes:

- `magneticod.idx_torrents_total_size`
- `magneticod.idx_torrents_discovered_on`
- `magneticod.idx_torrents_name_gin_trgm`

Retain PK, UNIQUE infohash, FK, CHECK, schema_version and blob PK throughout. Protect index removal with the migration lock, schema inventory and recorded migration state. On resume from an already started deferred-index run, absent indexes are **expected only for the migrator**; do not relax the normal application's strict `MakeDatabase`/`schema-init` verification.

After reaching the final source torrent ID, switch phase to `data_loaded`; create the missing indexes one at a time, using normal transactional `CREATE INDEX`, and recover safely after a crash by reconciling actual catalog state with migration state. Do not use `CREATE INDEX CONCURRENTLY` unless the design is explicitly revised (it adds different crash/invalid-index states). Run `ANALYZE` on target after finalization, and reseed the identity to strictly greater than imported MAX(id), with correct empty-table and INT32 boundary cases. Secondary index completion occurs even if some *per-record* errors remain, but is **not** equivalent to successful verification or cutover readiness. Future `retry-errors` may add a small number of rows using the completed indexes.

If `--defer-secondary-indexes` is not requested, preserve all indexes for the entire run. Never automatically drop indexes on an arbitrary nonempty database or on a nonmatching resumed run.

## 10. Verification gates and future commands (implementation in 04B/04C)

### `verify-decode` (target-only)

Scan **all** target torrents with bounded keyset iteration and decode every present blob. Verify codec framing, canonicality, count, file-size sum, overflow and single-file no-blob rules. Reject `file_count>1` without blob. A missing atypical single-file blob cannot be detected from target alone; report that limitation. Persist diagnostic outcomes and truthful coverage; support resume. This is not source equality verification.

### `verify-content` (source ↔ target; preferred cutover gate)

Read the **full** source again and compare every original torrent, including ID, infohash, name, total_size, discovered_on and canonicalized `(path, size)` multiset with exact duplicate multiplicity. Also verify target has no unexpected extra torrents, and compare source/target totals and error registry. Detect missing atypical single-file blobs. Use bounded streaming and independent verification progress/checkpoint. Running just `verify-decode` does not establish source-to-target equality.

### `retry-errors`

Select only unresolved source IDs in `magnetico_migration.errors`; do **not** rescan every legacy torrent to find them. Re-read source and insert successfully corrected rows with their original IDs, atomically update error statuses/attempts and progress. Do not overwrite previously imported rows without a separately approved repair operation. Retrying an unchanged invalid record yields a new diagnostic attempt, not a fabricated success. Preserve all error history needed by export.

### `errors export`

Always support JSONL of error-only metadata without reconnecting to the source. `--include-records` (explicit) reads complete original torrent fields **and all** source `(files.id, path, size)` rows plus available target metadata and decoded list; `--include-blobs` adds exact raw BYTEA in reversible Base64, even when decoding fails. Sensitive dumps must be streamed with record/output limits, safe permissions and an optional checksummed manifest; no silent truncation, no secrets, no auto-publishing. Missing source access when requested is a hard error.

### `cleanup`

Only removes temporary migration-owned bookkeeping, never permanent `schema_version` or user data. Requires explicit user confirmation/flag and successful completion of the agreed verification gates and backups, or a separately documented operator override. No implicit cleanup on successful import.

`migrate users` (including **invites** and API tokens, excluding password-reset tokens), replacement web UI and crawler/ingest decoupling are independent follow-on workstreams.

## 11. Test and acceptance strategy

**Task 04A** includes focused unit and small PostgreSQL integration tests in the main repository for: exact source v0 detection and v1 partitioned rejection; independent source/target DSNs; empty/partial target refusal; explicit IDs and no-blob; malformed source records; duplicates; checked sums; batch size/byte cap; transactional error+progress commit; lost-COMMIT-response and kill/restart; advisory lock; index deferral/finalization/resume; source read-only guarantee; no credential leakage. These tests are mandatory for merging 04A, but do **not** run the full 3.46M-torrent sample after every 04A change.

**Task 04B** adds focused verification and retry tests; **04C** adds export and cleanup tests. **Task 04D** is the substantial end-to-end run using the reusable 10% historical PostgreSQL sample (3,457,046 torrents / 123,460,039 files) **in the main repository**, plus injected faults, resumed runs and full `verify-content`. Baseline full-scale time/RSS is measured afresh, never asserted equal to the benchmark. No production server contact until the owner approves the final cutover plan.

**Pre-production blocker outside this task:** A real benchmark reproduction (`magneticobench/internal/realbench/determinism_probe_test.go`) shows that a history-primed pooled Brotli writer can produce different valid bytes or even a different adaptive serializer selection from a fresh writer at the **same library version**. The current production `codec/encoder_test.go` mostly tests repeated *identical* inputs and does not disprove the A→B history case. Fix/review this under a separate focused codec task before production cutover. Do not use byte-for-byte *re-encoding* as an integrity test until fixed; decode and compare canonical content instead. This does not change the codec envelope or decoding contract.

## 12. No-go conditions

Never silently: upgrade source v0 to v1; write to source; rename IDs; drop unknown indexes; remove constraints; ignore duplicate paths; normalize filenames; treat absent/malformed files as empty; turn fatal transaction errors into per-record skips; claim `import_complete` means `verified`; auto-run web/crawler; print DB secrets; run full-scale tests against production; or revise the wire format/target schema without approval.
