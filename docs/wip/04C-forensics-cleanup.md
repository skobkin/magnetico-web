# Agent Task 04C — Forensic Error Export and Safe Migration-State Cleanup

**Repository:** `skobkin/magnetico-web`  
**Branch:** `refactor/0.8.0-unify-and-new-storage`  
**Baseline:** accepted 04B through `cd5d0c5b` (fetch current pushed tip before planning; do not overwrite independent work).  
**Proposed contract to commit first:** `docs/specs/legacy-torrent-forensics-cleanup-v1.md` from the accompanying file.  
**Parent:** `docs/specs/legacy-torrent-migration-v1.md` §10, `docs/specs/legacy-torrent-verification-recovery-v1.md`, `docs/engineering-standards.md`.

## Objective

Implement `magnetico migrate errors export` and `magnetico migrate cleanup` as **separate safety domains**: export works during incomplete, failed or inconsistent imports for diagnosis; cleanup permanently removes migration-owned operational state only after strict verification/backup/evidence gates and explicit operator authorization. **No production contact, no crawler/web/user migration, no `magneticod` schema or wire changes, no push unless the owner explicitly asks.**

## Read and validate before planning

Read the current source code and tests (not just the reports):

- `internal/migration/state/{store.go,errors_store.go,schema_v2.go,verification_store.go,retry_store.go,state.go}`: v1/v2 inventory, progress, error registry (resolved/unresolved and run incidents), append-only retry/verification history, latest verification `seq`, `data_revision`, lock.
- `internal/migration/target/{preflight.go,verify_preflight.go,retry.go}`: strict completed-run preflight is **not appropriate for forensic export**.
- `internal/migration/legacy/original/{original.go,reader.go}`: v0.12.0 schema, read-only adapter, set-oriented reads; may need separate streaming access to *all* file rows beyond normal codec limits for forensics.
- `codec.Decode` and persistence's no-blob invariants: export exact persisted bytes, never re-encode, distinguish reconstructed single-file assumptions.
- `opflags/{opflags.go,config.go,parsers.go}` and `main.go`: nested commands, URL/env/YAML precedence, controlled logging, exit codes, progress renderer, interrupts.
- `docs/wip/04B-verification-recovery-report.md`: latest-run authority and successful real 10% experiment. The operator already completed that experiment and removed its databases: **do not repeat it**.

Resolve any mismatch with the normative parent contracts **explicitly in the plan**, rather than silently changing parents.

## Required design/implementation

1. **Define the wire format for export**, versioned JSONL with stable typed records and deterministic order: original import errors (including resolved), run incidents, retry attempts, verification issues, run metadata and verdict summaries. Preserve historical attempts and provenance. Default export is complete, source-independent and metadata-only; an empty error registry still exports an auditable run summary. Support exact v1 and v2 operational schemas through **read-only** catalog inspection; do not upgrade as a side effect. Include safe filters that are clearly marked partial.
2. **Forensic preflight:** take the same migration advisory lock, inspect operational definitions, but allow interrupted/failed imports, state mismatch, damaged permanent target records. **Do not call `CompletedRunPreflight` or require `Reconcile` for metadata export.** Source is never contacted unless `--include-records` is explicitly requested. Preserve read-only behavior on both systems.
3. **Record expansion (explicit opt-in):** selected source IDs only; full original torrent fields and all `(files.id, torrent_id, path, size)` rows even if unrepresentable by the codec; full available target metadata plus decoded files or typed decode failure. Preserve duplicates. Mark typical no-blob single-file reconstruction as assumed. Missing source access must be fatal. For an incident without a source ID, document that no full record can be linked; never dump unrelated records. Bounded source/target reads; no giant record-in-memory map.
4. **Raw blobs (separate opt-in):** exact BYTEA→Base64 round-trip for selected records, even if decode fails. Probe stored lengths before fetching, hard-fail above explicit per-blob/output caps; no silent truncation. Blob inclusion requires records inclusion.
5. **Safe output:** streaming JSONL; file-only for sensitive modes; `0600`, O_EXCL/no symlink overwrites, same-directory temporary path + atomic **no-clobber** publish (no TOCTOU overwrite), error cleanup, writer failures/disk-full/cancel hard errors; progress/logs to stderr, no credential/record/path leakage. Optional SHA-256 manifest over exact final JSONL bytes, containing counts, filters, run ID and data revision; on failure never claim a complete export. Configurable limits distinct from intentional filters.
6. **Cleanup preview and gates:** preview is always non-destructive. Real execution takes the lock and rechecks exact v2 inventory, complete/consistent import, strict `magneticod` target, `Reconcile`, zero unresolved record/integrity errors, and the latest/current **passing verification for both decode AND content** (`seq` authority, same progress revision, complete coverage). Require explicit backup confirmation and independently retained **complete** metadata export with validated SHA-256 manifest, complete-selection flags and event counts matching the current registry for the same run/revision; no credential-bearing YAML confirmation. Do not pretend backup/immutability was automatically proven.
7. **Override:** explicit `--force-unverified --reason ...` plus all safety acknowledgments may waive verification failure/unresolved findings; it may **never** waive schema ambiguity, incomplete import, inconsistency, loss of lock, missing backup or unknown deletion scope. A reconciled `import_complete_with_errors` may use this override, but incomplete imports may not. An exceptional missing-export-evidence override needs a **separate explicit acknowledgment**. Spell out the risk in output/report.
8. **Atomic deletion:** known migration-owned tables in FK-safe order, then schema `RESTRICT`, one PostgreSQL transaction, **no CASCADE**, no permanent `magneticod` changes. Unexpected dependencies or errors roll the entire cleanup back. Handle ambiguous commit and SIGKILL honestly; do not retry destructive operations automatically. A second invocation after removal must be non-destructive and must not pretend historical evidence still exists.
9. **Compatibility:** existing 04A/04B tests and behavior unchanged until explicitly completed cleanup. Update CLI/help/docs, use existing env/config precedence without introducing secret flags, produce `docs/wip/04C-forensics-cleanup-report.md` and README index row. Implementation may use focused new packages where boundaries materially improve safety; avoid an artificial 'no new packages' restriction.

## Planning requirements

Before writing code, return an implementation plan identifying exact DDL/read-only preflight boundaries, JSONL event schema, event ordering/dedupe, source/target query patterns, memory and output bounds, manifest/atomic-file lifecycle, lock and cleanup transaction, exit codes, negative-case matrix and commit slices. Call out especially the **v1 incomplete-import export** and **failed `state.Reconcile` export** cases. Specify the proposed exact default limits and flags. Do not start coding until the owner has reviewed the plan.

## Required tests

- JSONL completeness/order/version with v1 and v2 fixtures; resolved/unresolved run/record errors, retry history and multiple verification runs including stale/failed newest run.
- Source absent: metadata export succeeds; record export fails. Source exact records, all file IDs and duplicates, >codec-limit list, missing record, malformed/NULL blob, corrupt raw bytes Base64 round-trip; no source/target writes.
- Stream limits and resource safety: oversized blob refusal before materialization; large registry; file-list streaming; writer failure, cancellation, path already exists, symlink, restrictive permissions, output/manifest checksum, disk-full simulation; no finalized partial output.
- Cleanup refusal **without any DDL**: incomplete run, unresolved errors, corrupted counts, partial/foreign operational schema, mismatched/missing manifest, newer failed/interrupted verification, stale revision, absent backup/confirmation, live concurrent migration, unexpected PostgreSQL dependency.
- Cleanup transaction rollback on forced mid-drop SQL error, exact permitted tables only, production schema/data unchanged, no CASCADE, idempotent already-cleaned case and no automatic checkpoint recreation.
- Binary CLI smokes incl. secret redaction and stdout/stderr separation, interrupted exit; `go test -race` on affected packages.

## Verification/resource discipline

Run focused unit and small PostgreSQL integration tests during development. Prefer one pinned disposable PostgreSQL 17 container, per-test DB teardown, bounded test concurrency; avoid the prior container explosion. Final battery: `go build ./...`, `go vet ./...`, `gofmt -l .`, `golangci-lint run`, `go test ./... -p 2`, `go test -race ./internal/migration/... ./opflags/...`. Record pass/skip census and any reruns precisely. **Do not restore or exercise the large 10% extract**: 04B's 3,457,046-torrent, 123,460,039-file full run already passed, and 04C is principally destructive/forensic-state logic. Do not contact production.

The owner may supply additional Codex/Herdr review instructions; respect those and cap review iterations if instructed. Do not push automatically. Stop for owner review with a detailed report and exact commits.
