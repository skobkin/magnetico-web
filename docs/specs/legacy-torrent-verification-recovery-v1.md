# Magnetico 0.8.0 — Verification and Recovery Contract (Task 04B)

**Status:** Proposed supplement for owner review. **Base implementation:** `refactor/0.8.0-unify-and-new-storage`, Task 04A through `3b9cb7f4`. **Parent contract:** `docs/specs/legacy-torrent-migration-v1.md` §10; also `postgresql-persistence-v2.md` and `wire-format-v1.md`.

This supplement specifies Task 04B behavior where the parent contract is intentionally high-level. It **does not** authorize changes to production `magneticod` schema, codec wire format, original source database, users, crawler or ingest transport. Source means the unpartitioned `boramalper/magnetico v0.12.0` PostgreSQL layout, not the `tgragnato` fork.

## 1. Commands and outcomes

```text
magnetico migrate verify-decode --workers 8
magnetico migrate verify-content --workers 8
magnetico migrate torrents retry-errors --workers 4 --batch-size 256
```

Existing configuration applies: target `--database` / `MAGNETICO_MIGRATE_DATABASE` / `migrateDatabaseURL`; source `MAGNETICO_MIGRATION_SOURCE_DSN` / `migrateSourceDatabaseURL` (no source DSN flag). `verify-decode` requires **no source DSN**. `verify-content` and `retry-errors` require it, with a read-only source session. No source-target DSN fallback or credential leakage.

Verification exit codes: `0` = full, completed check and no mismatches; `3` = scan completed but failed validation/content equality or is blocked by unresolved source/coverage discrepancies; `2` = interrupted and resumable; `1` = infrastructure or state failure. Exit 0 must never be produced from a partial scan. `verify-decode` may succeed while legacy import errors remain: describe this as **target decode pass only**, never cutover-ready. A `retry-errors` run must distinguish completed-with-still-invalid records from fatal failure, report both counts, and must not report verification success.

Do not silently reuse a previously completed verification result: a fresh invocation creates a new verification run. An interrupted run may resume automatically when its source/target identity, progress, target revision and relevant fingerprints remain compatible; otherwise refuse and require a fresh run, preserving the previous report.

## 2. Preflight and frozen-data assumptions

Both commands require a complete 04A import phase (`import_complete` or `import_complete_with_errors`), strict production target schema, and consistent 04A run state. Use the existing target advisory lock, or a lock with **proven exclusion against the same retry/import code**. Verification and retry cannot run concurrently against the same target. Never require the old `--defer-secondary-indexes` CLI choice again: read the completed run's stored value.

Verification reads target/source through bounded, read-only PostgreSQL `REPEATABLE READ` transactions for consistent *per-database* snapshots; write verification checkpoints and findings through separate target connections. Two database snapshots are not cross-database atomic; stop all external writers before a verification run. Report that operator-enforced immutability remains essential. For resume, a new snapshot requires checking the recorded run identity, content fingerprint and target data revision, and never claims weak fingerprints can detect arbitrary source changes.

Introduce a monotonic `data_revision` to **migration-owned** progress (initial value 0), incremented transactionally on any retry mutation, including error attempts. Each verification run records that revision. A later revision makes earlier verification results **stale**, even if they formerly passed. Display stale results distinctly; never use them as cutover evidence. External writers bypassing the migration schema are forbidden operationally, and are not magically detected by `data_revision`.

No automatic source repair. An explicit external source correction is an operator action, not a write performed by Magnetico. By default, retry refuses a changed stored source fingerprint. For deliberately repaired sources, implement a distinct explicit `--accept-source-repairs` flag **only for retry-errors**: record the old/new sanitized fingerprints, verify same source identity and stable ID bounds, constrain retries to unresolved source IDs, and require a fresh full `verify-content` afterward. This flag cannot prove unrelated rows were unchanged; document the limitation and never silently mutate the original stored import fingerprint. A source drift discovered by `verify-content` is reported, not suppressed; an explicitly accepted repair may be recorded as provenance, but only actual full equality may pass the content check.

## 3. `verify-decode`: target-only full scan

Use bounded `id` keyset pagination over **every** target torrent, left joining the blob by `torrent_id`. No skipped rows and no `LIMIT`-only sample. Process blobs via the current `codec.Decode`, never by byte-identical re-encode. Test version/framing/codec errors and limits, canonical decoded order, `file_count > 0`, file count equality, checked nonnegative file sizes and checked `SUM(size) == total_size`, valid target metadata and infohash shape, and no-blob invariants. If `codec.Decode` canonicalizes output before returning, do not claim that a comparison of its returned ordering proves the on-wire serializer was canonical; report this limit rather than pretending to perform a stronger check.

- Blob present: decode, verify `file_count`, sizes, limits and canonical source representation. A single typical file encoded with a blob instead of the required no-blob representation is a noncanonical representation finding.
- Blob absent, `file_count == 1`: logically reconstruct `(name,total_size)` and mark as a **target-only assumption**; you cannot prove that the original source had that path. The report must state the number of such records and the atypical-single-file blind spot.
- Blob absent, `file_count != 1`: integrity failure.
- Every inconsistent row creates a bounded, typed issue; do not modify data. Also check for unexpected blobs/relational anomalies where accessible. Do not rely solely on PostgreSQL constraints for data validity.

A target decode pass covers all stored target rows, **not** all original source rows. Report both scanned target records and the 04A unresolved import error counts without conflating the two.

## 4. `verify-content`: source ↔ target full equality

Do a complete merge comparison in **ascending historical `id`** order, independently scanning both databases with keyset pagination (no `OFFSET`, no N+1 file queries, no giant JSON aggregates, no global ID map). For corresponding IDs compare exact `info_hash`, `name`, `total_size`, `discovered_on`, `file_count`, and the complete canonical `(path,size)` **multiset including duplicates**. Compare byte-exact UTF-8 strings without path/name normalization. The source file-row *order* and source `files.id` are not target identities.

Read source file lists in bounded, set-oriented range/ID-set queries based on the original adapter; keep file lists intact within production limits. For representable records, compare them to the decoded target list (or the reconstructed no-blob single file). This detects atypical single-file blobs missing from the target. Never compare recompressed bytes. For records whose source file list cannot fit production limits, drain/count without unbounded memory, classify them as **not representable / missing source coverage** and a verification mismatch; never claim their full lists compared. An existing source error does not excuse a missing target row from the equality gate. All IDs from both databases must be accounted for, including extra target rows after the source iterator ends, missing source rows, ID gaps, rejected and malformed source torrents, and dangling/NULL-owned source file rows.

Compare source/target totals and 04A unresolved error registry, and issue an explicit mismatch/coverage finding for every unresolved record or run-scope source-integrity problem. Do not add verification mismatches to the 04A `errors` table: they are independent diagnostics and must never become retry candidates automatically. A complete scan with discrepancies is **completed-fail**, not an infrastructure failure and not a pass.

## 5. Verification bookkeeping and crash/resume

Extend **only** `magnetico_migration` with versioned/verified, migration-owned objects, for example:

- `verification_runs`: UUID, import run ID, `kind` (`decode`/`content`), state (`running`/`interrupted`/`completed_pass`/`completed_fail`/`aborted`), start target `data_revision`, source identity/fingerprint where applicable, source and target checkpoints, phase (`scanning`/`extras`/`complete`), expected/observed coverage, verified/mismatch/unverifiable/assumed-single counters, timestamps, settings.
- `verification_issues`: verification ID, optional source ID/infohash, stable code/stage/severity, bounded secret-safe JSONB diagnostics, unique per `(verification_id, source_id, issue_code)` where applicable; avoid storing raw names/paths in routine findings. Later 04C forensic export may fetch complete records explicitly.

Separate verification state from `magnetico_migration.progress` and its `errors`: never overwrite import checkpoint/counters to record scan progress. Persist each verification issue batch **atomically with its own monotonic checkpoint and counters**. Use a contiguous ordered frontier under concurrent decode workers; no ``checkpoint = highest completed worker ID``. An interrupted verification resumes from its own state. Allow one verifier per target using advisory-lock exclusion. A new run must not destroy older verification results. Tighten `state.Load`'s operational-schema inventory to accept the **exact expected 04B extension** (including validated column and index definitions), retaining refusal on foreign or partially installed objects. Upgrade existing 04A operational schema transactionally; preserve all old progress/errors/identities and permit previously finished 04A runs to be read.

A verification pass is valid for a particular data revision and source observation. If retry changes target data or error status, previous passes become stale. `cleanup` in 04C must consult the latest, current, **passing** verification run rather than any historical pass.

## 6. `retry-errors`: targeted, transactional repair

Select only `scope='record' AND NOT resolved` source IDs from the **existing** `magnetico_migration.errors` registry, ordered by ID; no whole-source discovery pass. Fetch their original metadata and full files using a new bounded `legacy/original` batch-by-ID method (not a per-ID N+1 reader). Validate/encode with the exact production `persistence` seam and Compact codec, preserving historical IDs and duplicate file entries. Apply per-torrent production limits and bounded in-flight memory as in 04A.

Provide an independent `retry_runs` checkpoint (one pass per invocation, safe interruption/resume) and append-only `retry_attempts` diagnostics, or an equivalent **unbounded-in-time, bounded-per-transaction** history design. The original record error is attempt 1 and must remain available to 04C; repeated failed attempts must not silently replace it. The retry cursor advances on **every attempted ID**, whether fixed or still invalid, in the **same transaction** that records its result. A resumed pass must not repeat already settled attempts. A later invocation starts a new pass over still-unresolved IDs.

**Successful retry atomic transaction:** prove error row still unresolved and no target row exists for that ID or colliding infohash; insert target torrent/blob with exact source ID; mark the error resolved with timestamp and increment attempts; append retry history; increment `progress.success_count` and (if applicable) `blob_count`, decrement `progress.error_count`, leave `scanned_count` and 04A `last_processed_id` unchanged, increment `data_revision`, advance retry checkpoint. No `ON CONFLICT DO NOTHING`, no overwrites or automatic fix-up. If a target row exists unexpectedly, stop as state/conflict failure.

**Unsuccessful validation retry atomic transaction:** keep record error unresolved, increment attempts, preserve original finding and append the new typed reason in retry history, leave import counts/data unchanged, increment `data_revision` if registry/attempt state changes, advance retry checkpoint. If original source ID disappears, diagnose explicitly, do not silently resolve it.

**Critical reconciliation change:** currently `state.CountErrors()` counts *all* record errors, whereas `progress.error_count` must count **unresolved** record errors after retry. Modify target facts / `state.Reconcile` to count `scope='record' AND NOT resolved`; the invariant remains `scanned_count = success_count + error_count`. This is backward-compatible with 04A data because record errors start unresolved. Keep the full history (`errors` plus `retry_attempts`) separately from the unresolved count. Only when both record errors and run-scope integrity errors are resolved may an import phase `import_complete_with_errors` transition to `import_complete`; neither phase means verified. Never auto-clear run-scope orphan errors as part of per-record retry.

All mutations require the same target run lock (the completed import's recorded original index-deferral setting is read from state), read-only source, nonzero exit on incomplete/failed infrastructure work, and safe ambiguous-COMMIT reconciliation. Do not reuse 04A `WriteBatchTx` as-is: it increments the source scanned total and changes the import checkpoint, which is invalid for retry.

## 7. Operator reporting, security, tests and limits

Reuse the 04A `progress` renderer where possible, with operation-specific snapshots. Provide TTY/non-TTY rate/elapsed/ETA and exact coverage (including mismatch and not-comparable counts). No arbitrary path or credential exposure in routine logs. Output of verification findings must be bounded and include stable ID/code; 04C handles sensitive full-row export. `verify-decode` performs no source connection; `verify-content` and `retry-errors` never mutate it.

At minimum test: empty data; no-blob normal vs atypical missing blob; multi-file duplicate paths; bad framing/codec version; count/sum/overflow/canonicality; extra/missing target rows; metadata mismatch; unresolved source errors and orphan rows; source list above limits; target-only blind-spot reporting; fresh run/resume/interrupt; crash after issue insert before checkpoint (transaction must rollback); reordered worker finishes; target revision invalidation; repeated successful verification; retry success/failure/missing source/conflict; correct counters and error attempt preservation; retry crash before/after commit; partial success with remaining bad records; phase transitions; signature/credentials safety; source read-only; extended 04A state schema compatibility. Use disposable PostgreSQL integration fixtures, real race tests and binary CLI smoke. **Do not** run the 3.46M/123M full real-data experiment here (04D).

## 8. Acceptance criteria

Verification result stores exact coverage, is restart-safe and does not modify the import dataset; target-only pass states its unavoidable historical blind spots; content pass proves complete exact source↔target equality for its recorded snapshots and has no unresolved import-integrity discrepancies; retry modifies only formerly missing target rows and their migration bookkeeping, with exact historical IDs, transactional accounting and immutable original error history. Full source data and secrets remain outside ordinary issue records. No reported pass is silently reused after target data changes.
