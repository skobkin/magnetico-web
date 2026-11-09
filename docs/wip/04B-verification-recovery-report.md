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

Post-loop final battery (the tree of this report, all 23 validated review findings
fixed): `go build ./...`, `go vet ./...`, `gofmt -l .` (no output), `golangci-lint run`
0 issues; `go test ./... -p 2 -count=1` — all 21 non-empty packages pass (the
`internal/migration` package was re-enqueued once after a teardown-only
DROP-DATABASE timeout burst under peak create/drop churn — the same documented
one-container artifact as above; it passes standalone in 64 s with zero assertion
failures); `go test -race ./internal/migration/... ./opflags/...` green. The review
loop added 23 regression tests; the census above is from the pre-loop battery, so the
loop-inclusive total is **438 tests pass, 1 skips**.

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

## Codex review loop (owner-directed milestone 1)

**Iteration 1** returned 9 findings; all were re-validated against the tree and fixed
as individual commits, each with a regression test that fails without the fix:

| # | Finding (validated) | Fix commit | Regression test |
|---|---|---|---|
| 9 | A losing checkpoint CAS still committed the page's issue rows (issue inserts committed without their checkpoint/counters) | `c777c2a7` — CAS loss rolls back the WHOLE page transaction | CAS-stale page commits nothing |
| 2 | verify-content never compared the stored `file_count` against the decoded list length (two-file blob with `file_count=3` passed; verify-decode correctly failed the same data) | `ccb79e20` — compare the stored `file_count` in verify-content | file_count drift → mismatch finding |
| 4 | Source-only rows incremented the target-side mismatch counter, corrupting `observed_target_ids` coverage accounting (and the persisted mirror) | `d2fdbaf3` — source-only rows carry severity on the issue alone; target counters/mirrors stay exact | `TestVerifyContentSourceOnlyRowsStayOutOfTargetCounters` |
| 3 | On resume, the registry iterator restarted at the import minimum instead of the verification's source checkpoint, so registry IDs ≤ checkpoint re-blocked recognition of unresolved rows | `5ea99a58` — resume the registry walk at the source checkpoint | `TestVerifyContentResumeClassifiesRegistryIDsBeyondCheckpoint` |
| 1 | Unresolved import errors could coexist with a content PASS (a vanished rejected source row left one unresolved error and an exit 0) | `ebe66052` — finalize fails coverage over registry entries with no per-row `unresolved_record_error` (aggregate `unresolved_record_coverage` finding with uncovered count + min/max source ID) | `TestVerifyContentVanishedSourceRecordFailsCoverage` |
| 7 | A source-only prefix (first N imports rejected) accumulated arbitrarily many findings into one uncommitted unit, defeating bounded memory even at `--batch-size 1` | `2a8d7961` — commit units at bounded finding counts mid-page and mid-window, with certified checkpoint boundaries (a target checkpoint only ever advances past CLASSIFIED target rows) | `TestVerifyContentBoundedUnitsFlushMidPageAndMidWindow` |
| 5 | verify-content retained every decoded file list until the whole page finished decoding — the byte budget bounded stored blobs, not retained decoded content | `9eff3123` — decode pages lazily one budget-bounded chunk at a time; only the current chunk's decoded lists live | `TestVerifyContentDecodesPerRowUnderTinyBudget` |
| 6 | retry-errors reassembled FetchByIDs' budget-split windows into one in-memory batch before settling any ID, defeating the fetch-side memory limit | `ac2ebdfc` — settle retry IDs per fetch window (ascending per-window prefix + monotonic cursor) | `TestRetryErrorsSparseBatchSettlesPerWindow` |
| 8 | `migrate torrents --database X retry-errors` silently ignored the parent-positioned `--database X` (the repair pass ran against the default target) | `325bc9fe` — opflags transfers parent-positioned shared flags (database/workers/batch-size/max-inflight-bytes) onto retry-errors when the parent position set them and the nested position did not | `TestParseArgs_RetryErrorsNested/ParentPositionedSharedFlagsTransfer`, `/NestedPositionWins` |

All nine were validated before fixing (findings 1–5 and 8 reproduced by the reviewer;
6–7 confirmed by retention analysis and then covered by fixtures).

**Iteration 2** first validated the round-1 fixes (reversing each of the eight
regression-tested fixes made its test fail; the parent-flag/yaml/env precedence matrix
passed), then returned 5 findings — all reproduced — and all fixed as individual
commits:

| # | Finding (validated) | Fix commit | Regression test |
|---|---|---|---|
| 2 (P1) | YAML config diagnostics echoed neighboring config lines verbatim — a typo printed a DSN with its password (goccy renders errors with source context) | `bf7f1770` — redactSecrets masks URL userinfo and password/pwd key-value pairs at both config decode paths | live-leak check (unredacted diagnostic prints the DSN) + redaction unit tests |
| 1 (P1) | verify-content chunk admission counted STORED bytes, but wire-format-v1 payloads are compressed: one chunk retained a whole compressed page's decoded lists (926 stored bytes → 1.92 MB decoded under a 1 MiB budget) | `56028605` — pageContent serves SEGMENTS: fetch bounded by stored bytes, decode cut so retained DECODED bytes stay within the budget; a single over-budget row is served solo (codec MaxUncompressedBytes bound) | `TestPageContentRetentionStaysWithinDecodedBudget` (7.2 MB retained pre-fix) |
| 5 (P2) | The round-1 memory regressions passed against their pre-fix implementations (outcome checks only) | content half: the white-box retention test above; retry half: `8b2cd8dc` — the batch settlement loop extracted behind a seam with ordering-pinning unit tests (a second-fetch failure proves window one settled first) | `TestSettleBatchWindowsSettlesPerWindow`, `TestSettleBatchWindowsFullBatchOrdering` |
| 3 (P2) | A registry entry whose source row vanished never left the iterator head: every later unresolved row compared against the stale entry and became missing_target_row; uncovered counts wrong (reproduced: live rejected ID 3 misclassified, uncovered:2) | `865721da` — registryIter.skipBelow advances past entries below the current source row; they stay covered by the finalize aggregate | `TestVerifyContentVanishedRegistryEntryDoesNotPoisonLaterRows` |
| 4 (P2) | A pass interrupted after the extras checkpoint resumed with OrphanTargetBlobs=0 while the durable issue kept the real count (both verify-decode and verify-content) | `dc7603fa` — state.OrphanBlobIssueCount; both runners restore the result field when adopting a run already past its scan | `TestVerify{Decode,Content}ExtrasResumeKeepsOrphanCount` |

**Iteration 3** returned 3 findings — all validated before fixing (finding 1 by live
redacted-output reproduction, finding 2 by a white-box admission check, finding 3 by a
cancel-point analysis proving the first regression draft exercised the wrong interrupt
path) — and all fixed as individual commits:

| # | Finding (validated) | Fix commit | Regression test |
|---|---|---|---|
| 1 (P2) | The round-2 redaction missed three credential shapes pgx accepts: empty-username userinfo URLs (`postgres://:SECRET@host`), quoted values with spaces (`password='HEAD SECRET_TAIL'`), percent-encoded keys (`%70assword=SECRET`) — each leaked verbatim in YAML diagnostics | `adde49c8` — urlPasswordPattern allows an empty username (still requires `@`); credentialKeyPattern enumerates password/pwd letters as case classes or percent escapes with bare/single/double-quoted values | `TestRedactSecrets` (7 cases) + live-leak yaml test covering the empty-username DSN |
| 2 (P2) | decodedBytes charged only path bytes: each codec.File is a 24-byte Go value and slice capacity can exceed length — 30,000 one-byte-path rows charged ~510 KiB while the real arrays hold ~740 KiB, so a "budget" segment over-retained | `d3a3b7f0` — estimator charges `cap(files)*24 + sum(len(path))` | `TestPageContentAccountingCoversFileArrays` (pre-fix admits rows [0,2) into one window) |
| 3 (P1) | The round-2 seam extraction gave the retry fetch closure its own error mapping whose graceful-interruption branch returned the bare ErrInterrupted sentinel — a cancel surfacing at the next-window FetchByIDs left `retry_runs.state='running'` behind an exit 2 | `35300b5f` — the closure returns interruptRetry() like every other interrupt path | `TestRetryErrorsInterruptDuringWindowFetchPersists` (fixture rows ~1.1 MiB each under a 1 MiB budget so the batch genuinely splits; verified failing with state 'running' against the bare sentinel) |

**Iteration 4** first re-validated all three round-3 fixes by reversal (each regression
test failed on the pre-fix tree, passed at HEAD), then returned 4 findings — all
reproduced — and all fixed as individual commits:

| # | Finding (validated) | Fix commit | Regression test |
|---|---|---|---|
| 1 (P1) | Two further conninfo shapes pgx accepts leaked verbatim: backslash-escaped quotes inside single-quoted values (`password='HEAD\' SECRET_TAIL'`) and escaped spaces in bare values (`password=HEAD\ SECRET_TAIL`). Bonus: the `pwd` key branch had been dead since it was written (it spelled p-u-d) — `pwd=secret` was never redacted in any round | `f2f186ce` — escape-aware value classes (quoted `\\.` allowed; bare values are runs of plain/escaped characters ending at unescaped whitespace) + repaired the middle-letter class; a premise test pins both shapes as valid pgx conninfo | `TestRedactSecrets` (3 new cases) + `TestKeyValueDSNEscapeShapesAcceptedByPgx` |
| 2 (P2) | FetchByIDs' metadata weight never charged infohash bytes — the legacy schema has no hash-length constraint, so 32 distinct 65,538-byte hashes rode in one 1 MiB window (2.1 MB retained, ~4 KiB charged) | `8dda8770` — the metadata weight includes the hash bytea; the two pinned-weight metadata tests updated for the exact arithmetic | `TestFetchByIDsBudgetChargesInfohashBytes` (32 rows split under a 1 MiB budget) |
| 3 (P2) | Both verifiers mapped an error from the extras-phase `CommitVerificationPage` to verificationFatal unconditionally: cancellation while that UPDATE waits produced exit 1, unlike every scanning branch (exit 2 + durable interrupt mark) | `d72bb45e` — both branches apply the standard mapping (fatal cause → durable interrupt → fatal) | `TestVerify{Decode,Content}ExtrasCommitCancelPersists` (row-locked run + pg_stat_activity proof the extras UPDATE is blocked before cancelling) |
| 4 (P2) | The reported severity split counted finalize issues on top of the pre-commit recount, but storage deduplicates re-proposed issues — the drift provenance (recomputed against the import fingerprint on every resume) reported 2 info findings with 1 durable | `820c0da5` — the severity split is recounted from the durable issue table after the finalize commit (the dedupe is authoritative); the pass decision keeps its pre-commit basis + finalize-severity gate | `TestVerifyContentExtrasResumeDoesNotDoubleCountDrift` (info = 1, durable = 1) |

**Iteration 5** (the final round of the cap) re-validated all four round-4 fixes by
reversal (each regression test failed on the pre-fix tree, passed at HEAD) and returned
2 findings — both reproduced — fixed as individual commits:

| # | Finding (validated) | Fix commit | Regression test |
|---|---|---|---|
| 1 (P1) | A YAML double-quoted value re-encodes conninfo escapes on top of the DSN's own (`\\ ` spells an escaped space): the raw excerpt line cannot be value-parsed, and the masked-prefix left the tail leaking (`password=HEAD\\ REVIEWTAIL…` masked only `HEAD\\`) | `252b37ac` — redactSecrets gains a structural second layer: any quoted configuration line whose content may carry a credential (loose detector: password/pwd spellings, percent-encoded keys, URLs with userinfo) is dropped whole — line number survives, content does not; in-place masking stays for error text and inline DSNs | excerpt-drop unit cases + the live yaml-dq syntax-error diagnostic asserting the tail absent |
| 2 (P2) | Two post-finalize REPORT-ONLY statements still ran on the run context (the post-commit severity recount; decode's 04A registry count report): a cancellation landing there aborted a run that is already completed_pass/completed_fail and can never resume, misreported as interrupted (exit 2) or fatal (exit 1) | `ecb21531` — both run on a context detached from the run's cancellation, like the durable-mark paths | `TestVerifyDecodeFinalizedRunNotInterruptedByCancel` (100k probe issues widen the post-commit window; cancel fires the moment the run row turns terminal; the pass still returns its completed verdict) |

**Loop outcome: converged at the 5-iteration cap.** 23 validated findings across five
rounds (9 + 5 + 3 + 4 + 2), every one fixed as an individual commit with a regression
test proven to fail on its pre-fix tree. The finding classes stabilized around two
themes: interruption/cancellation semantics at every durable-state boundary (extras
commits, post-commit reporting, the retry fetch seam) and credential-redaction
soundness in configuration diagnostics — the latter resolved structurally (drop
credential-bearing lines) after three rounds of value-shape hardening kept finding new
escape layers. Round 5 validated all round-4 fixes by reversal before hunting, so the
regression net is self-consistent as of HEAD.

## Owner review round (post-loop)

Two findings from the owner's review of the loop-closed HEAD, both fixed with
reversal-proven regression tests.

**1. Target metadata pages are byte-budgeted.** `readDecodePage` — the shared
target reader of both verification commands — bounded row count only, so
unbounded target `name` text could balloon one page's retention regardless of
`--batch-size`. It now enforces the same structural budget as the 04A source
reader, fed from `--max-inflight-bytes` (the command's existing one in-flight
tunable): each row is charged `retainedDecodeRowOverhead + infohash + name`
bytes; an OCCUPIED page stops BEFORE the first row that would overflow it (that
row and everything after it arrive in the next keyset page — coverage and
checkpoint resumability are unchanged, a page just gets smaller under
backpressure); an EMPTY page admits its first row whatever it weighs (a single
oversized row is bounded by itself alone, never skipped for scheduling
reasons). Tests: `TestReadDecodePageByteBudgetCutsAtRowBoundary` (cut point,
keyset continuation to exact coverage, oversized solo admission),
`TestVerifyDecodeMetadataBudgetPreservesCoverage` and
`TestVerifyContentMetadataBudgetPreservesCoverage` (end-to-end: 25 rows of
100 KB names under a 1 MiB budget — every committed checkpoint jump ≤ 10 rows
although `--batch-size` allowed 1000, full clean verdicts on both commands).
All three proven to fail on the pre-fix tree.

**2. `--workers` is now truthful per command.** The flag claimed parallel
processing everywhere, but only verify-decode actually parallelizes (blob
decoders across one fetch chunk). verify-content never used the value — its
decoded-content segment cut needs each row's decoded size before admitting
the next row, so concurrent admission would recreate the unbounded retention
the segment design exists to prevent; parallelizing it would sacrifice the
memory-safety fix, which the review forbade. retry-errors passed the value to
the shared encoder's semaphore, which a one-record-at-a-time feed never
fills — attempts settle strictly sequentially ON PURPOSE (every attempt is
one per-ID transaction advancing the revision-tracked cursor in order).
Decision: documentation and flag-description adjustment, not parallelism —
the value stays accepted (validated 1..64, recorded in run settings) and each
input struct / flag now states exactly what it does and does not control.
verify-decode's advertised concurrency is pinned by
`TestVerifyDecodeWorkersDecodeConcurrently`: a hook gate fails the run unless
`--workers` goroutines hold blob-decode work at the same instant (proven to
fail with the worker pool forced to one). `normalizeVerifyConfig` documents
the per-command split.

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
  directive ⑥.

### Real-data experiment (directive ⑥): first attempt FAILED at restore — stopped, owner input required

The full battery (including the race suite) and the Codex review loop (closed at the
5-iteration cap) were green before the attempt; no other suite ran concurrently; the
host had 208 GB free against an estimated 30–50 GB footprint. The restore was executed
exactly once, per the directive:

```
set -o pipefail; gzip -dc magneticod-sample.sql.gz \
  | docker exec -i <pinned container> psql -U postgres -v ON_ERROR_STOP=1 -q \
    -d magnetico_04b_sample_src
```

It aborted after 11 seconds: the dump's preamble (line 28) runs
`ALTER SCHEMA magneticod OWNER TO magneticod;` — the dump expects a cluster role
`magneticod` that the pinned disposable container does not have, so `ON_ERROR_STOP`
fired before any table data was copied (the partial database contained an empty
`magneticod` schema and zero tables). Per the directive — an incomplete restore fails
the experiment; no partial-data run; no automatic repetition — the attempt was stopped
and NOT retried. Both disposable databases (`magnetico_04b_sample_src`,
`magnetico_04b_sample_tgt`) were dropped `WITH (FORCE)`; the dump file was never
modified (size and mtime verified after the attempt). Timings, the disk-before capture
and the raw restore log are preserved under the experiment's scratch directory.

**A single corrected attempt needs exactly one prerequisite**: `CREATE ROLE magneticod;`
(NOLOGIN) in the container before the restore, so the dump's ownership statements
resolve. That is a procedure change beyond the approved directive, so it waits for the
owner's explicit go-ahead; the export/import procedure itself is unchanged.

## Deviations from the plan

- None of substance. The plan's commit 7 gained the 7b infra-error fix (`8c28dedc`)
  when the content-pass design review exposed decode-side finding persistence on
  infrastructure failures; the final battery added the race fix and lint commits above.
  Both are per-finding commits with their own regression harness, per the 04A
  convention.
