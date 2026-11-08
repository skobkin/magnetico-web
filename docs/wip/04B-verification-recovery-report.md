# Task 04B implementation report — verification and recovery of the legacy torrent import

Branch `refactor/0.8.0-unify-and-new-storage`, the 13 commits of the approved plan
(base `3b9cb7f4`, the pushed 04A tip) — 11 planned plus a race fix and a lint pass the
final battery surfaced. Normative contract:
[`docs/specs/legacy-torrent-verification-recovery-v1.md`](../specs/legacy-torrent-verification-recovery-v1.md)
(committed as received); assignment:
[`docs/wip/04B-verification-recovery.md`](04B-verification-recovery.md). No spec file was
edited. No `magneticod` schema DDL, wire format or crawler code was changed. No production
database was contacted — every integration test ran against disposable per-test databases
from the pinned `postgres:17` container. Nothing has been pushed; commits are local until
the owner says otherwise.

## What shipped

| Commit | Content |
|---|---|
| `docs(specs): verification and recovery contract + 04B assignment` | The two documents as received, plus the `/magneticod-sample.sql.gz` gitignore entry (the owner's read-only sample symlink must never be committable) |
| `feat(migration/state): operational schema v2 with definition-validated upgrades` | `EnsureOperationalSchema`: definitions before DDL — the full v1 and v2 expected relation definitions are compared against the catalog before anything executes; absent schema = fresh (InitRun creates v2); exact v1 upgrades in one transaction (07A databases transparently); anything malformed/partial/foreign refuses with the database byte-identical (catalog-snapshot-proven). `data_revision` column, `verification_runs`/`verification_issues`/`retry_runs`/`retry_attempts` under `magnetico_migration` only; `CountErrors` counts unresolved record-scope errors (spec's reconciliation change) |
| `feat(migration/original): FetchByIDs + WindowSnapshot` | `readWindow` refactored so `Window` delegates (04A output byte-identical); REPEATABLE READ READ-ONLY snapshot variant; sparse-ID set-oriented fetch with the window's overflow-drain semantics and budget-split `remaining` |
| `feat(migration/pipeline): export processor + canonical comparison` | `Processor`/`NewProcessor`/`Process` and `CompareCanonical`/`EqualCanonical` exported — bodies unchanged, existing tests green unchanged |
| `feat(migration/state): verification run store` | `StartVerification` adopt-or-new under the exclusive run lock, `CommitVerificationPage` (issues + counter deltas + both checkpoints + the cumulative observed-ID mirrors in one CAS-guarded tx), `FinishVerification`/`MarkInterrupted`/`MarkAborted`/`LatestVerification`; monotonic `seq` gives the latest-run authority (directive ⑰) |
| `feat(migration/target): retry transactions with revision-tracked resume` | `CommitRetrySuccess/Invalid/Missing` — the per-ID transaction proves the error unresolved, proves no target row/info-hash collision, inserts with the exact historical ID, resolves the error, appends `retry_attempts`, moves `success_count/error_count/blob_count` and `data_revision` and `retry_runs.current_data_revision` in the SAME transaction; `scanned_count`/`last_processed_id` untouched; `ReconcileRetryAttempt` for ambiguous COMMIT; `CompleteRetryPhase` with the one new `import_complete_with_errors`→`import_complete` phase-flip edge; `CountUnresolvedRecordIDs` keyset pages |
| `feat(migration): verify-decode runner` | Target-only full decode scan: keyset metadata pages; octet_length-first blob handling (a corrupted oversized BYTEA is never materialized — typed `blob_over_limit` finding); greedy chunk packing under `--max-inflight-bytes` with solo admission of a single blob that alone exceeds the budget but fits the codec (directive ⑧); per-row decode → `file_count` equality → `persistence.ValidateIngest` → canonical/no-blob checks; blobless `file_count=1` rows are reconstructed and validated (failing rows produce `invalid_metadata` findings, never a clean pass — directive ⑨) and only then counted `assumed_single` (the reported historical blind spot); extras-phase orphan-blob check; exact coverage accounting |
| `fix(migration): fail verification fatally on blob-fetch infrastructure errors` | Review fix while building the content pass: an infrastructure failure inside a fetch chunk was being recorded as per-row `decode_failure` findings a page commit would persist; fetch errors now abort the page through the runner's fatal/interrupt classification, never become findings |
| `feat(migration): verify-content runner` | Target-page-driven ascending merge over both keyset iterators: metadata fields + `pipeline.EqualCanonical` `(path,size)` multiset (duplicates preserved, byte-exact, no normalization); source-only → `unresolved_record_error` (coverage) if in the 04A registry else `missing_target_row` (mismatch); target-only → `extra_target_row`; oversized source records drained and counted `unverifiable` (never claimed compared); orphan/NULL-owner source file rows aggregate into a coverage finding; finalize cross-checks totals + fresh counts + the unresolved registry; import-fingerprint drift vs fresh is severity-`info` provenance (Equivalent comparison — a PostgreSQL restart or transport switch never blocks resume, directives ④⑤⑦) and never blocks a fully-equal pass; never writes the source, never touches the 04A `errors` table |
| `feat(migration): retry-errors runner` | Registry-driven batches (re-queried per batch — the set shrinks as it resolves); production-identical `pipeline.Processor` validation/encode/decode-verify; drift policy: `Fingerprint.Equivalent` vs the recorded import fingerprint, restart-tolerant; refusal names the anchors; `--accept-source-repairs` matches by stable evidence only (database name + unchanged ID bounds — never postmaster start time, directive ⑯) and records old+new fingerprints as provenance; resume requires `current_data_revision == progress.data_revision` AND `Fingerprint.Equivalent(fresh, pass's own recorded fingerprint)` (directives ①⑮); a mid-pass source change aborts the pass and requires fresh authorization; phase flip when everything resolved |
| `feat(cli): verify/retry subcommands with exit codes` | `migrate verify-decode` / `verify-content` commands, nested optional `migrate torrents retry-errors` (`subcommands-optional` keeps bare `migrate torrents` importing); DSN fan-out to all five target fields and three source fields, unknown-`MAGNETICO_MIGRATION_*` rejection for every migrate subcommand, no new YAML keys, `--accept-source-repairs` deliberately CLI-only; exit codes 0/3/2/1 with the exit-3 sentinels printing a summary instead of a fatal; renderer `Title` prefix (04A output byte-identical); controlled error rendering with bounded prefix-verified wrap details |
| `fix(migration): merge verify-content decode results single-threaded` | The final battery's `-race` pass caught the content pass's blob-decode workers writing a shared id-keyed map; now mirrors the decode pass's shape (pre-sized slice indexed by the page row, one writer per index) with a single-threaded merge. Regression harness: the multi-worker integration suite under `-race` |
| `style: golangci-lint clean across the 04B tree` | Deliberate-discard rollback defers, inferred pointer types, alias `copy` — `golangci-lint run`: 0 issues |

Net diff over the task: 37 files, ≈10.6k insertions, ≈144 deletions. New files live in the
EXISTING packages (`internal/migration`: `verify_common.go`, `verify_decode.go`,
`verify_content.go`, `retry_errors.go` + three integration suites; `internal/migration/state`:
`schema_v2.go`, `verification_store.go`, `retry_store.go`; `internal/migration/target`:
`verify_preflight.go`, `retry.go`) — no new packages, per the container/RAM discipline.
Touched existing packages: `legacy/original` (+62-line `FetchByIDs`/`WindowSnapshot`),
`pipeline` (exports only), `opflags`, `main.go`, `progress` (one optional `Title` field).

## Pass/fail semantics (spec §4)

- The authoritative verdict for a kind is the `verification_runs` row with the highest
  monotonic `seq` (directive ⑰): a newer `completed_fail` at the same `data_revision`
  supersedes an older pass; a newer interrupted/aborted run leaves NO current verdict.
  A run is a *current pass* iff `state='completed_pass' AND start_data_revision =
  progress.data_revision`.
- A scan only completes as `completed_pass` when zero mismatch/coverage/unverifiable
  issues exist; `info` issues (fingerprint drift) are recorded provenance and never
  counted. A complete scan with discrepancies completes as `completed_fail` — exit 3,
  summary printed, not a fatal. Every unresolved record and every run-scope source
  integrity problem (orphan/NULL-owner files) produces an explicit coverage finding —
  ALL source IDs are accounted, including gaps.
- Verification never writes the 04A `errors` table; the 04A registry counts are reported
  separately (`unresolved record error(s) … counted separately` in the summary line).
- Retry IDs always sit at or below the frozen import checkpoint; `scanned_count` and
  `last_processed_id` stay untouched, so every 04A Reconcile invariant holds atomically
  (proven by the completed-run preflight reconciling the target after repaired passes).
- The phase flip `import_complete_with_errors`→`import_complete` fires only from the
  retry pass, only when no unresolved record AND no run-integrity errors remain, through
  the CAS-guarded phase transition on the lock connection.

## Coverage accounting (exact, resume-honest)

- Both checkpoints advance CAS-guarded with the page's issues and counters in ONE
  transaction; ambiguous COMMIT reconciles against the stored row under the exclusive
  lock (a lost acknowledgement means our own commit landed).
- Cumulative `observed_source_ids`/`observed_target_ids` mirrors are committed per page:
  counters alone cannot reconstruct source-only classified rows after a resume, the
  mirrors can. Finalize recomputes coverage from the DATABASE, never from in-memory
  counters, and any finalize-time mismatch of severity mismatch/coverage/unverifiable
  flips the verdict.
- Source-only rows (beyond the last target row) increment only the source-observed
  mirror — never the target counters — so target-side coverage math stays honest.
- Oversized source records are drained (counted, ID-recorded) and classified
  `source_not_representable` (unverifiable) — never claimed compared, per the contract.

## State-upgrade behavior (directive ③)

- Absent `magnetico_migration` operational schema → fresh-run case, v2 created by
  `InitRun`; no DDL runs before that determination.
- Exact v1 → upgraded in one transaction (`data_revision` column + the six 04B
  relations + marker); the marker alone is never trusted — definitions are re-proven
  from the catalog on every preflight.
- Anything else (malformed v1, partial or incorrect v2, foreign objects in the
  namespace) → `StateError/ErrRunStateCorrupt` naming the offenders, NO DDL executed,
  database byte-identical (snapshot-proven in tests).
- `persistence.MakeDatabase` strictness is untouched — asserted throughout.

## Known blind spots (reported, never hidden)

1. **`assumed_single`** — a target row with `file_count=1` and no stored blob is
   reconstructed as `(name, total_size)` and validated with `persistence.ValidateIngest`;
   the original source path cannot be proven (the blob was legitimately never stored).
   Validated rows are counted `assumed_single` and the count is stated in the summary;
   rows failing validation produce `invalid_metadata` findings and fail the scan.
2. **`source_not_representable`** — source records whose file lists exceed the codec
   limits are drained and counted unverifiable; the content pass explicitly does not
   claim them compared.
3. **Weak source anchors** — fingerprint drift handling is honest about its limits:
   `--accept-source-repairs` is an operator assertion matched on stable evidence
   (database name + ID bounds), NOT proof the source is the original database; unrelated
   rows may have changed; the recorded import fingerprint is never mutated; a full fresh
   content verification is mechanically forced afterward via `data_revision` staleness.

## Performance caveats (verified in principle at fixture scale; real-scale numbers are the owner's one-time experiment, per directive ⑥)

- Blob reads are `octet_length`-probed BEFORE any BYTEA is materialized (directive ②);
  fetch chunks are bounded by `--max-inflight-bytes` with sequential chunks, so
  concurrent raw bytes never exceed the budget + at most one codec-bounded oversized
  blob.
- All scans are keyset (no OFFSET), all file access set-oriented per page/window (no
  N+1), memory bounded by the window/page budgets — no global ID maps.
- The content pass re-reads the registry through small keyset pages alongside the merge
  (no full-registry load).
- Tunables (`--workers`, `--batch-size`, `--max-inflight-bytes`) are validated in the
  runners (`ConfigError`) with the 04A defaults.

## Suites actually run vs skipped

All green on this tree (pinned disposable `postgres:17`, per-test databases):

- `go build ./...`, `go vet ./...`, `gofmt -l .` (no output)
- `golangci-lint run` — 0 issues
- `go test ./... -p 2 -count=1` — all packages pass (exit 0)
- `go test -race ./internal/migration/... ./opflags/ .` — every package passes (run
  per-package to completion; running ALL packages concurrently against the single pinned
  instance occasionally exceeds the harness's 20 s DROP-DATABASE admin timeout during
  peak create/drop churn — teardown-only failures, the assertions of every affected test
  pass and every such test passes standalone; this is an infrastructure artifact of the
  one-container discipline, not a code failure)
- Verbose census (sequential per package, whole tree): **415 tests pass, 1 skips** —
  `TestIdentitySameDatabaseViaSocketAndTCP`, because the Docker-cached-cluster harness
  exposes no Unix socket (the test documents its skip condition, exactly as in 04A).

Test families delivered per the assignment (all integration unless noted):
decode clean/failures/structural/complete-after-findings, oversized-corrupted-blob (no
materialization), collectively-oversized page split, solo oversized admission, malformed
`file_count=1` no-blob (`invalid_metadata`, not a pass), orphan blobs; content
gaps/missing/extra/metadata/file-list-with-duplicates/atypical-blob-missing/no-files/
orphan-and-NULL-owner/coverage-exactness/source-readonly, drift-as-info (a fully-equal
pass WITH a drift issue recorded), resume-after-source-restart (Equivalent-based
adoption), resume-after-source-change refusal; durability interrupt-resume,
issue+checkpoint transaction rollback, out-of-order workers (race), revision staleness,
duplicate invocation after a completed run (fresh run, old results immutable), same-run
lock exclusion; retry unchanged/corrected/missing/conflict/counter-invariants/
no-checkpoint-abort/lost-COMMIT reconciliation/resume-after-{success,failure,ambiguous
COMMIT,external-revision-change,source-change-mid-pass (incl. authorized passes needing
fresh authorization)}/drift+acceptance/phase-flip gating; schema upgrade
malformed-v1/partial-v2/no-mutation-on-refusal/idempotence; CLI exit codes 0/3/1 via a
small binary smoke driving the real binary through the whole recovery lifecycle (the 50k
04A smoke untouched), DSN secrecy assertions, target-unmodified assertions,
TTY/non-TTY rendering, plus `-race` over the whole migration tree.

## Hand-off requirements for 04C/04D

- **04C (cleanup/export)**: verification/retry history is append-only and immutable —
  cleanup must never delete `verification_runs`/`retry_runs` rows for the run they
  document. FK deletion order for teardown: `verification_issues` → `verification_runs`
  → `retry_attempts` → `retry_runs` (all FK to `progress.run_id` last). The
  latest-run-authority rule (§ pass/fail above) is what any 04C cutover gate must
  evaluate; an older pass must never be selected while a newer contradictory or
  incomplete run exists.
- **Staleness rule**: every 04B verdict is scoped to `start_data_revision`; the retry
  pass increments `data_revision` in the same transaction as each attempt, so any
  post-verification mutation mechanically forces fresh verification (a verify pass never
  mutates the revision). 04C gates must check `start_data_revision ==
  progress.data_revision`, not merely a `completed_pass` row existing.
- **04D**: the binary smoke `TestMigrateVerifyRetryBinarySmoke` is the small-scale
  template for the owner's real-data experiment; the operational procedure (restore
  once, `/usr/bin/time -v`, honest SIGKILL/OOM cleanup) is the approved plan's
  directive ⑥ — not yet executed; it runs only after the owner's Codex review loop
  converges.

## Deviations from the plan

- None of substance. The plan's commit 7 gained the 7b infra-error fix (`8c28dedc`)
  when the content-pass design review exposed decode-side finding persistence on
  infrastructure failures; the final battery added the race fix and lint commits above.
  Both are per-finding commits with their own regression harness, per the 04A
  convention.
