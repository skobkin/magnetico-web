# Agent Task 04B — Full verification and targeted retry of legacy torrent import errors

**Branch:** `refactor/0.8.0-unify-and-new-storage` of `skobkin/magnetico-web`. **Baseline:** Task 04A (reviewed fixes through `3b9cb7f4`). **Proposed normative supplement:** `docs/specs/legacy-torrent-verification-recovery-v1.md` (attach from `legacy-torrent-verification-recovery-v1.md` in this package). **Parent:** `docs/specs/legacy-torrent-migration-v1.md` §10; `docs/specs/postgresql-persistence-v2.md`; `docs/specs/wire-format-v1.md`; `docs/engineering-standards.md`.

## Goal

Implement `magnetico migrate verify-decode`, `magnetico migrate verify-content`, and `magnetico migrate torrents retry-errors`, with independent durable verification/retry state, precise full-data coverage, resumability, and history-preserving, atomic recovery of failed source IDs. This completes the 04B scope; it is **not** full-scale 04D verification, user migration, crawler/ingest work, forensic export or cleanup.

## Mandatory design constraints

1. Read the existing 04A code and implementation report, **especially** `state.Load`, `state.CountErrors`, `state.Reconcile`, `target.Preflight`, the completed-import phases, source `Window` reader, `pipeline`, `progress` renderer, and `target.WriteBatchTx`. Identify what can be reused and what must remain separate. Preserve the import's current behavior and tests.
2. Keep `magneticod` production schema, codec wire format and source v0.12.0 DDL unchanged. All new tables/columns must be under `magnetico_migration`. The source is strictly SELECT-only. Do not add support for the partitioned tgragnato fork.
3. Add an **atomic operational-schema extension** for 04B (verification runs/issues, retry runs/attempts, data revision), upgrading existing 04A state safely and validating exact inventory. Do not silently accept unknown migration-owned objects or relax `persistence.MakeDatabase`.
4. Serialize verification/retry/import with the same effective target migration lock and verify the lock's liveness. Separate independently resumable scan/retry checkpoints from the immutable 04A import checkpoint.
5. Target decode pass: scan **every target ID**, decode present blobs and validate strict structural rules, account for legitimate no-blob single files and report the atypical-single-file blind spot. No source connection in this command.
6. Source↔target content pass: bounded independent keyset scans, ascending-ID merge, exact identity and metadata, byte-exact canonical `(path,size)` multiset with duplicate multiplicity, count/limit handling, extra/missing IDs, unresolved registry problems and orphans. No full-table in-memory map or N+1 source file queries. Do not claim coverage for unrepresentable oversized records; count and report them as failures. Never compare re-encoded bytes.
7. Store verification issues and an ordered **contiguous** checkpoint atomically. Per-run state records source observation and target data revision. A retry invalidates all earlier verification passes; do not return cached verification success as if newly checked. Use `0` pass / `3` completed mismatches / `2` interrupted / `1` fatal for verification.
8. Retry only unresolved record-scope IDs from `magnetico_migration.errors`; fetch source records by bounded ID batches; use the production validation and encoder. Preserve historical IDs. Every ID, even an unchanged failure, is attempted once per retry pass, with durable progress and append-only diagnostics. Do not silently re-import a previously successful ID.
9. On success atomically insert torrent/blob, resolve its original error, append retry history, update counts (`success_count++`, `error_count--`, optional `blob_count++`) and data revision. Keep `scanned_count` and `last_processed_id` **unchanged**. On retry failure keep unresolved and record the attempt; no spurious success. Make `CountErrors/Reconcile` count *unresolved* record errors, keeping all historical errors accessible. Phase can change from import_complete_with_errors to import_complete only when no unresolved record/run-integrity errors remain. Transactional ambiguous-COMMIT reconciliation and cancellation are required.
10. Source fingerprint drift should not be silently treated as immutable source. Default refusal for retry on drift; an explicitly audited `--accept-source-repairs` opt-in must support documented operator-side corrections to unresolved source records, without changing the original import fingerprint. A full fresh content verification is mandatory after such a repair. Never claim weak anchors prove all other records unchanged.
11. Use the existing CLI config precedence: verify-decode target-only, verify-content/retry source from `MAGNETICO_MIGRATION_SOURCE_DSN` or `migrateSourceDatabaseURL`, target from existing config. No credential-bearing source flag. Log only safe bounded diagnostics, no arbitrary filenames/paths. Show progress/coverage/ETA via the reviewed 04A renderer.

## Required tests

- Target-only: absent/normal/atypical single-file blob, invalid blob framing/version, multiple malformed files, overflow/sum/count/canonical failures; complete scan even after findings, no false positive cutover claim.
- Full-content: nonoverlapping IDs, source ID gaps, missing and extra target rows, metadata mismatch, path-size mismatch with duplicates, atypical single blob missing, no-source-files, oversized source file list, orphan/NULL-owner files, validation of exact covered and uncovered counts, without writing source.
- Durability: interrupted scan, checkpoint + issues transaction rollback, ordered frontier with out-of-order workers, resumption, revision mismatch, duplicate invocation after completed run, source fingerprint drift, same-target concurrent attempt refused.
- Retry: unchanged invalid row, corrected valid row, missing source ID, duplicate target ID/infohash conflict, resolved error retention, original diagnostic + append-only attempt history, 04A counter invariant, no checkpoint advance, aborted transaction, lost COMMIT acknowledgement, partial retry resume, phase transition and verification-result invalidation.
- CLI: exit codes 0/1/2/3, DSN secrecy, target-only decode without source DSN, target data unmodified by verification, TTY and non-TTY progress, PostgreSQL v17 fixture suites and race tests.

## Deliverables

1. Compact design/implementation plan with migration-state DDL, transitions, memory bounds, exact pass/fail semantics, and reusable boundaries.
2. Focused implementation commits including tests and CLI wiring.
3. `docs/wip/04B-verification-recovery-report.md` with actual test census, coverage proof, known blind spots, performance caveats, state-upgrade behavior, and handoff to 04C/04D. Update `docs/wip/README.md`.
4. Final checks: `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race` on affected packages, `gofmt -l .` empty, `golangci-lint run` clean. Clearly report skipped tests. Use disposable PostgreSQL only; no full real-data benchmark in this task.

Commits remain local until the owner requests a push. Do not begin unrelated tasks. Do not silently change parent contracts; identify discrepancies explicitly.
