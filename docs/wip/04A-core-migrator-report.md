# Task 04A implementation report — offline import of original Magnetico v0.12.0 torrents

Branch `refactor/0.8.0-unify-and-new-storage`, commits 1–9 of the approved plan
(base `c69253ba`). Normative contract:
[`docs/specs/legacy-torrent-migration-v1.md`](../specs/legacy-torrent-migration-v1.md);
assignment: [`docs/wip/04A-core-migrator.md`](04A-core-migrator.md). No spec file was
edited. No schema DDL, wire format or crawler code was changed. No production database
was contacted — every integration test ran against disposable per-test databases from
the Docker harness. Commits stay local; nothing is pushed.

## What shipped

| Commit | Content |
|---|---|
| `docs(specs): legacy torrent migration contract v1 and task 04A assignment` | The two documents, committed as received |
| `refactor(test): extract shared PostgreSQL test bootstrap into internal/pgtest` | `internal/pgtest` extracted from `persistence/harness_test.go`; persistence suites re-run unchanged |
| `feat(persistence): exported ingestion-validation seam for in-module bulk importers` | `ValidateIngest` / `NoBlobEligible` / `WrapEncodeError` thin wrappers + behavior-parity tests (`validate_seam_test.go`) |
| `feat(migration): legacy v0.12.0 source adapter — probe, fingerprint, keyset reader` | `internal/migration/legacy/original`: SELECT-only schema-qualified probe (v0 accepted, partitioned v1 fork refused by partitioning **and** by `migrations` version, unexpected layout refused), frozen-source fingerprint, keyset-paginated torrents + set-oriented files windows, orphan/NULL-owner counting |
| `feat(migration): target preflight, run state, advisory lock, resume validation` | `internal/migration/state` (`magnetico_migration.progress`/`.errors`, phase model, CAS transitions, run-scoped advisory lock + serialized liveness monitor), `internal/migration/target` (strict preflight, full resume reconciliation), `internal/migration/pgidentity` (transport-independent same-database decision, fail-closed) |
| `feat(migration): bounded import pipeline — CopyFrom writer, checkpoints, per-record errors` | `internal/migration/pipeline` (workers, byte-weight semaphore with solo-slot oversized admission, hard batch cap over successes **and** failures), `target/writer.go` (`CopyFrom` torrents + blobs + error rows + CAS checkpoint in one transaction), `internal/migration` runner (contiguous frontier, replay, ambiguous-COMMIT reconciliation, SIGTERM settle), incident registry |
| `feat(migration): secondary-index deferral/finalization, identity reseed, completion phases` | `target/finalize.go`: atomic drop+mark of exactly the three secondary indexes, explicit `data_loaded`→`building_indexes` transition before any `CREATE INDEX`, per-index crash-boundary recovery with definition verification, identity reseed, `ANALYZE`, completion gating on `error_count` **and** `run_error_count` |
| `feat(opflags): migrate torrents subcommand; main wiring with graceful shutdown` | `magnetico migrate torrents` (`--workers/--batch-size/--max-inflight-bytes/--defer-secondary-indexes/--database`); source DSN deliberately flag-less (`migrateSourceDatabaseURL` + `MAGNETICO_MIGRATION_SOURCE_DSN` only, env over file, namespace-scoped, unknown variables rejected); `main.go` exit codes 0/2/1 with secret-safe failure text |
| `test(migration): missing source DSN and invalid tunables refuse before any database contact` | Unit suite for `ErrSourceRequired` + `ConfigError` range refusals (coverage gap found during report writing) |
| `style: golangci-lint clean across the 04A tree` | gci import grouping, tagged switch, ineffectual assignment, documented feeder-drain suppression — `golangci-lint run`: 0 issues |
| `docs: 04A migrator implementation report; update wip README task sequence` | This report |

Net diff over the task: 50 files, ≈9.3k insertions, ≈750 deletions. New packages:
`internal/pgtest`, `internal/migration` (runner/convert/incidents),
`internal/migration/legacy/original`, `…/pipeline`, `…/state`, `…/target`, `…/pgidentity`.
Touched existing packages: `persistence` (seam + test bootstrap delegation only),
`opflags`, `main.go`.

## Separation guarantees (spec §5)

- Legacy source types (`legacy/original`) never mix with `persistence.Torrent`; the
  neutral `pipeline.Record` carries `SourceFileID` for 04C forensics but it is **never**
  mapped into the codec or the target.
- Validation runs exclusively through the exported `persistence` seam — no duplicated
  rules (parity-tested).
- `magnetico_migration.*` is separate from permanent `magneticod.schema_version`; the
  normal runtime's strict verification (`persistence.MakeDatabase`) is never relaxed —
  asserted by tests during and after deferral.

## Source safety

- All source access is SELECT-only, schema-qualified; `TestSourceSessionIsReadOnly`
  proves the session cannot write, and `TestProbeAndRefusalLeaveSourceUntouched` proves
  refusal paths leave the catalog byte-identical.
- Distinctness of source and target uses only transport-independent facts
  (`pg_postmaster_start_time()`, `current_database()`); addr/port are informational
  fingerprint fields, never decision inputs. Ambiguous or unreadable identity fails
  closed. Same database via TCP and Unix-socket aliases is refused; different databases
  on one instance are allowed.
- The fingerprint records COUNT/MIN/MAX ids, file-row counts, NULL-owner counts and
  anchor info-hashes; its documented weakness (immutability cannot be proven, aliasing
  at proxy level is the operator's responsibility, frozen offline source required) is
  stated in the contract and rechecked on every resume.
- Source DSN can never appear in `ps`: no CLI flag exists; unknown
  `MAGNETICO_MIGRATION_*` variables are rejected; empty resolution refuses with
  `ErrSourceRequired` (never a target fallback). All rendered URLs go through
  `redactDatabaseURL`; incident/error text is sanitized defensively at construction.

## Owner directives implemented (all test-backed)

1. **Checkpoint compare-and-swap** — every committed batch updates
   `last_processed_id`/counters with `WHERE run_id=$1 AND last_processed_id=$expected
   AND phase=$expected`; `RowsAffected != 1` ⇒ full batch rollback + fatal stop
   (`TestWriteBatchCASConflictUnderCompetingWriter`).
2. **Lock-connection liveness, all lock-conn use serialized behind one mutex** — the
   monitor pings only when idle and never interrupts a long index build; backend death
   is fatal (`TestRunLockPingDetectsTerminatedBackend`,
   `TestMonitorLockLossInvokesCallback`); all index DDL executes on the lock-owning
   connection.
3. **Index drop atomic with its state marker** — one transaction drops exactly the three
   secondary indexes and sets `indexes_deferred = true`, either both or neither
   (`TestDropSecondaryIndexesAtomicAndMarked`); refusal on unexpected catalog state
   leaves everything untouched (`TestDropSecondaryIndexesRefusesIncompleteState`).
4. **Deadlock-free oversized admission** — a record heavier than the whole byte
   semaphore takes the solo slot, admitted one at a time; in-flight weight is bounded by
   capacity + one codec-bounded record (`TestOversizedRecordsAdmitWithoutDeadlock`,
   `TestWeightsAcquireReleaseAndSolo`).
5. **Exit codes** — 0 only for a completion phase, 2 for `ErrInterrupted`, 1 for fatal
   (binary smoke asserts 2 and 0 end-to-end).
6. **Fail-closed identity** — unreadable/ambiguous source/target identity refuses
   (`pgidentity` unit tests over every transport combination).
7. **Full resume reconciliation** — fingerprint/adapter/contract/options compatibility
   plus: no rows beyond the checkpoint, torrent/blob/error counts and no-blob shape must
   match the persisted progress row exactly; any planted divergence refuses as corrupt
   state, never auto-repair (`TestPreflightRefusesPlantedInconsistencies`,
   `TestPreflightResumeRefusals`, `TestRunnerRefusesChangedSource`).
8. **`building_indexes` committed before the first `CREATE INDEX`** (separate
   transactions) — a crash after the first CREATE can never regress to `data_loaded`;
   partial index progress is valid and resumes per index with definition verification
   (`TestBuildSecondaryIndexesCrashBoundaries`,
   `TestBuildSecondaryIndexesRefusesWrongDefinition`,
   `TestVerifySchemaDeferredResumeWindow`).
9. **Single orphan policy** — orphaned source file rows are one run-scope integrity
   incident (aggregate counts + ID ranges, no paths), excluded from every torrent,
   never fatal, never repaired; counts preserved for 04B/04C
   (`TestReaderCountsOrphansWithoutAttaching`, `TestRunnerOrphansExcludedAndRecorded`).
10. **`run_error_count` semantics** — counts only unresolved *integrity* failures;
    recorded *operational* incidents are history and never force
    `import_complete_with_errors` (`TestCompleteImportGating`,
    `TestRunnerOperationalIncidentThenComplete`).
11. **Batch cap covers failures** — the transactional hard cap counts successes and
    per-record errors alike, so invalid-record runs slice into bounded batches
    (`TestBatchCapCoversAllProcessedRecords`).
12. **Ambiguous COMMIT** — reconciled against the authoritative DB checkpoint and actual
    rows before continuing; rows present without a checkpoint ⇒ fatal state corruption
    (`TestReconcileAmbiguousCommit`, `TestRunnerAmbiguousCommitReconciles`).

## Record semantics proven (integration)

Explicit source IDs preserved exactly (including ID gaps in the frontier); single-file
no-blob eligibility; atypical single-file blob; multi-file with duplicate `(path, size)`
entries and zero-size members preserved through canonical sorting; invalid totals,
absent files, file-count and codec-limit failures produce categorized per-record errors
(`invalid_metadata`, `missing_files`, `codec_limit`, `source_integrity`) with the run
completing as `import_complete_with_errors`. Encode happens outside target
transactions; correctness is the immediate Decode + canonical-content equality, never
re-encoded byte equality (pooled-Brotli determinism caveat respected;
`TestEqualCanonicalWithDuplicates`).

## Crash/resume proof

- Runner-level: interrupt mid-run settles the contiguous prefix, resumes to completion
  (`TestRunnerInterruptedThenResumes`); ambiguous commit reconciles; a second instance
  is excluded by the advisory lock (`TestPreflightLockExcludesConcurrentRuns`); source
  fingerprint change on resume refuses; source read failure is fatal **after** settling
  in-flight work (`TestSourceReadFailureIsFatalAfterSettling`); cancellation drains
  without goroutine leaks (`TestCancelSettlesContiguousPrefix`, run under `-race`).
- Index lifecycle: interrupted deferred run (drop applied, indexes absent) resumes into
  completion; interrupted *before* the drop resumes and performs the drop then
  completion; `MakeDatabase` refuses the index-less intermediate state throughout
  (`TestRunnerDeferredIndexesInterruptedResume`,
  `TestRunnerDeferredDropPerformedOnResume`).
- End-to-end binary smoke (`TestMigrateTorrentsBinarySmoke`, disposable source of 50,000
  synthetic torrents): SIGTERM mid-run → exit 2 with phase `importing` and an advanced
  checkpoint → resume → exit 0, `import_complete`, exactly 50,000 torrents (no resume
  duplicates) → re-run is an idempotent exit 0.

## Suites actually run vs skipped

All commands below were run green on this tree (Docker harness, `postgres:17`,
disposable per-test databases):

- `go build ./...`, `go vet ./...`, `gofmt -l .` (no output)
- `go test ./...` — all packages pass
- `go test -race ./internal/... ./persistence/ ./opflags/ .` — pass
- `golangci-lint run` — pass (see final battery below)

Verbose census of the migration area (`go test -v ./internal/... .`): **114 test
functions pass, 1 skips** — `TestIdentitySameDatabaseViaSocketAndTCP`, because the
Docker-cached-cluster harness exposes no Unix socket (the initdb bootstrap path does;
the test skips and documents itself when absent, exactly as planned). Everything else
in the task-doc §E matrix ran, including all PostgreSQL-backed suites.

Reproduce with: `MAGNETICO_TEST_POSTGRES_DSN=postgres://user:pass@127.0.0.1:5432/postgres
go test ./internal/... .` (the harness can also bootstrap a cached Docker
`postgres:17` itself; it never pulls images and binds 127.0.0.1 only).

## Known limitations / deviations

- **Progress display is minimal:** the runner emits cumulative snapshots through the
  `Progress` callback after every committed batch and at start/end; the binary prints
  the final cumulative summary. The planned periodic non-TTY logs / rolling ETA / TTY
  line renderer is **not** wired yet — an operator-ergonomics hand-off item for 04D,
  not a correctness gap (all counters the renderer would show are already authoritative
  DB state).
- `--dry-run` was deferred by owner decision; preflight always runs and prints its
  findings before any import instead.
- No `verify-decode`, `verify-content`, `retry-errors`, export or cleanup commands —
  04B/04C scope. The errors table already carries scope, source IDs, info-hash, stage,
  category/code, safe JSON details, attempts and resolution status sufficient for them.
- The 3.46M-torrent real-sample run and fault-injection campaign are 04D (owner-led).
- Pooled-Brotli determinism (same-library re-encode byte instability) remains a
  separately tracked pre-production codec issue, untouched here by design.

## Hand-off requirements for 04B–04D

- 04B (verification/retry): read `magnetico_migration.errors` (record scope) for
  retry; `run_error_count` and the orphan incident identify everything needing
  source-side investigation before cutover. Resume reconciliation gives a free
  integrity re-check of any completed run.
- 04C (export/cleanup): `SourceFileID` and orphan aggregate counts/ID ranges are
  preserved in the registry details for forensic export; `import_complete_with_errors`
  runs are the cleanup candidates.
- 04D: the binary smoke (`TestMigrateTorrentsBinarySmoke`) is the template for the
  full-scale run; progress renderer and tunable sizing (workers/batch/inflight) are the
  operator-facing knobs to exercise at scale.
