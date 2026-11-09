# Task 04C implementation report — forensic error export and safe migration-state cleanup

Branch `refactor/0.8.0-unify-and-new-storage`, the 8 commits of the approved plan
(base `cd5d0c5b`, the 04B tip; pushed as `cd5d0c5b..69ece9e8`) plus the owner-review-
round-1 fixes (five commits, listed in the review section below). Normative contract:
[`docs/specs/legacy-torrent-forensics-cleanup-v1.md`](../specs/legacy-torrent-forensics-cleanup-v1.md)
(committed as received); assignment:
[`docs/wip/04C-forensics-cleanup.md`](04C-forensics-cleanup.md). No spec file was
edited. No `magneticod` schema DDL, wire-format or crawler code was changed. The
original sample dump was never touched and the 10% extract was never restored (04B's
experiment is complete). No production database was contacted — every integration test
ran against disposable per-test databases from the one pinned `postgres:17` container
(maintained across 04B/04C, dropped at session end). The plan's 8 commits were pushed
at the end on the owner's explicit instruction (`cd5d0c5b..69ece9e8`, CI green on the
tip). The review-round fix commits were pushed on the owner's explicit mid-round
command (`69ece9e8..fdb967ac`); this report commit remains local until the next
explicit push.

## What shipped

| Commit | Content |
|---|---|
| `docs(specs): add the 04C forensic-export and cleanup contract` | The two documents as received (spec + assignment) |
| `feat(migration/state): read-only operational-schema inspection and full-history reads` | Exported `Queryer` seam (pool/conn/tx all satisfy it — every 04C read runs unchanged inside the drop transaction); `InspectOperationalSchema` (absent/v1/v2/unrecognized classification, never DDL, never inserts the marker); `ReadProgressRaw` (bypasses every `Load` invariant; v1 projects `data_revision 0`; 0 rows / 2 rows typed); full-history keyset reads — `ListErrorRows` (resolved and unresolved), `ListVerificationRuns`, `MaxVerificationSeq`, `ListVerificationIssuesAfter` (table-wide), `ListRetryRuns` (composite `(started_at, retry_id)` cursor), `ListRetryAttemptsAfter`; `SelectedRecordIDs` (one set-oriented UNION over errors(scope='record') ∪ verification_issues ∪ retry_attempts, DISTINCT ascending, NULL source IDs excluded); `CountRegistry`; `DropOperationalSchema` — 8 fixed statements (6 tables in FK order, marker, `DROP SCHEMA … RESTRICT`), no `CASCADE`, no `IF EXISTS`, pinned by a DDL-invariant test |
| `feat(migration/legacy/original): streaming forensic reads beyond codec limits` | `Source.WithSnapshot` (ONE `REPEATABLE READ READ ONLY` transaction on the existing private pool; the handle exposes only the two forensic reads) with `Snapshot.FetchTorrentRows` (sparse set-oriented metadata, no caps, absent IDs simply absent) and `Snapshot.StreamFileRows` (every `files.id` row streamed with O(1) retention — duplicates, oversized lists and NULL-owner orphans handled per contract; orphans structurally excluded from selection by torrent-ID selection) |
| `feat(migration/forensics): metadata forensic export with safe output lifecycle` | The `forensics` package: sentinels; deny-by-default semantic redaction (finding ①); `Preflight` (target pool → pgidentity → run lock → inspect → raw progress; absent schema is typed `ErrNoMigrationHistory`, unrecognized/v1 exportable, absent-row v2 inconsistent; NEVER `CompletedRunPreflight`, `Reconcile`, `EnsureOperationalSchema`, or source contact); the metadata pass (header → import_run → full error history → verification runs+issues with `authoritative`/`current_pass` flags → retry history; v1 marks the four 04B collections unavailable and walks none); canonical event order; safe output (`0600` under explicit chmod, same-dir hidden temp `O_EXCL`, publish = `os.Link` + remove — atomic no-clobber, no TOCTOU overwrite, parent-dir fsync; any failure/cancel/cap removes the temp and never leaves a final file); stdout mode metadata-only; `--manifest` written only after a successful publish, binding SHA-256 + byte count + event counts + run/revision + sanitized identities + filters + limits + the registry-state digests (finding ②); root runner `migration.ExportErrors` (context cancellation → `ErrInterrupted`) |
| `feat(migration/forensics): opt-in record and blob expansion` | `--include-records`/`--include-blobs`: `original.Open` + identity distinctness (fail-closed) + the target production-schema probe (typed `ErrRecordsTargetUnavailable`, never per-record); selection pages of 64 through `SelectedRecordIDs`; per batch ONE target snapshot (metadata + `octet_length` probe + in-cap byte fetch) and ONE source snapshot inside which every record's full block is emitted in contract order (`source_record → source_file* → target_record → decode_finding → target_file* → target_blob → target_blob_chunk*`) — O(1) file streaming AND block order together; decode-vs-raw decided independently (finding ⑤: a blob far over `codec.MaxStoredBytes` is typed `blob_over_limit`, never fed to the codec, yet exports raw in full under `--include-blobs`; only exceeding `--max-blob-bytes` hard-fails naming size + flag); blobless `file_count=1` reconstruction validated through `persistence.ValidateIngest` and marked `assumed`; corrupt-blob exact Base64 round-trip with the SHA-256 binding; decode findings share the codec taxonomy with 04B (`decodeErrorIssueCode` parity test); caps are hard errors, never truncation |
| `feat(migration/forensics): gated atomic cleanup` | `Cleanup`: G10 flag coherence before any connection; dry-run preview (gate-by-gate PASS/FAIL/WAIVED with reasons, registry rows, target facts, latest verdict per kind with staleness reasoning, expected drop list, blockers); gates G1–G9 pre-transaction and RECHECKED inside the drop transaction on its own snapshot (G4's `MakeDatabase` pre-tx only — documented deviation); independent persisted-issue validation (finding ③, corrected per the owner: `issue_count ==` persisted rows, severity recounted independently, ZERO blocking-severity issues for a normal pass, target-row accounting identity `verified+mismatch+unverifiable+assumed == observed_target_ids`, coverage validated separately; a `completed_pass` coexisting with blocking issues is CORRUPT — never waivable — while source-only mismatches and stale/failed verdicts are ordinary and waivable); G9 evidence with the full registry-state digest recompute in-transaction (stale evidence refuses even when counts and revision never moved); the drop = explicit transaction, gates re-read on the tx, fixed DDL, `COMMIT`; a lost COMMIT acknowledgment is classified on a fresh connection (schema present → definitive rollback; absent → explicit `ErrCleanupAmbiguous` — catalog postconditions never prove provenance; production counts compared as best-effort evidence; never auto-retried); already-cleaned no-op reports honestly that absence proves nothing; v1 and unrecognized schemas refuse without DDL |
| `feat(opflags,main): wire migrate errors export and migrate cleanup` | `migrate errors` group with required nested `export` (bare `migrate errors` is a usage error; parent-positioned `--database` transfers, retry-errors precedent) and the flat `migrate cleanup`; the four cap flags, the two path-exclusive evidence attestations, authorization booleans CLI-only; target-DSN yaml/env fan-out to all new fields; `MAGNETICO_MIGRATION_SOURCE_DSN` applies to `export`, validated-but-ignored on cleanup; NO new YAML keys; main dispatch + exit codes + renderer discipline (explicit Stop before every exit) + controlled-content renderers (fixed sentinel texts with bounded quoted engine-composed wrap details; the ambiguous classifier sanitizes driver fragments at composition so the bounded echo stays controlled); `migration.Cleanup` wrapper mirrors `ExportErrors`; end-to-end binary smoke through the whole lifecycle |
| `docs: 04C report and index row` | This report + the README index row |

Net diff over the task: 38 files, 9,967 insertions(+), 33 deletions(−). One new package
(`internal/migration/forensics` — the assignment explicitly permits it; the
"forensic read + terminal teardown" boundary is cohesive and keeps all
`magnetico_migration` SQL in `state` and all v0.12.0 source SQL in `legacy/original`).
Touched existing packages: `state` (Queryer seam + read-only forensics reads + the
fixed DDL), `legacy/original` (the snapshot handle only), `migration` (two thin
wrappers + a parity test), `opflags`, `main.go`.

## The two safety domains

Export is **forensic, fail-open on state content** (interrupted, failed, unreconciled
and v1 state all export — `ReadProgressRaw` bypasses every invariant, unknown phases
are exported verbatim with `reconcile_status:"not_reconciled"`, meaning "not evaluated;
counters as recorded"), **fail-closed on structure and connections** (schema class,
identity distinctness, source availability, output safety, caps). Cleanup is
**destructive, fail-closed**: every gate must pass immediately before AND inside the
single drop transaction, on the transaction's own snapshot.

## Export wire format

Every line carries the envelope `{"format":"magnetico-migration-forensics",
"format_version":1,"type":…,"run_id":…}` plus the event's fields; Go struct field
order is the deterministic output order; `json.Encoder` with `SetEscapeHTML(false)`;
infohashes lowercase hex; timestamps RFC 3339 UTC; nullables are `null`, never
omitted. Canonical order (documented in the header's `order` field): `export_header`,
`import_run`, `import_error*` (errors.id ASC, resolved and unresolved),
`verification_run*` (seq ASC — `authoritative` = per-kind max seq, `current_pass` =
authoritative AND completed_pass AND `start_data_revision == progress.data_revision`),
`verification_issue*` (id ASC, table-wide, `verification_seq` denormalized, run-scope
rows carry `record_unavailable_reason:"no_source_id"`), `retry_run*`
(`(started_at, retry_id)` ASC), `retry_attempt*` (id ASC), then in records mode per
selected source_id ASC the full block, then `export_footer` (actual emission counts;
its presence means the whole path completed). v1 exports mark
`operational_schema:"v1"` and name the four 04B collections in
`unavailable_collections` — never silently empty. Stored JSONB (`details`, `settings`,
fingerprints, identities, options) is NEVER echoed raw — see redaction below.

## Deny-by-default redaction (owner finding ①)

A per-kind whitelist maps each KNOWN key (enumerated from what 04A/04B actually write)
to an explicit value constraint (`int`, `bool`, `enum(set)`, `hex40`, `rfc3339`,
`dbident`, `hostport`, `sqlstate`). Unknown
keys are dropped entirely (a hostile key NAME itself can carry a secret; only a count
survives); a known key whose value fails its constraint becomes
`{"redacted":"constraint"}`; a non-object top level becomes `{"redacted":"shape"}`;
the whole redacted value over 4 KiB collapses to an oversize marker. Nonzero
drop/refusal counts are embedded as a `redacted` object. The policy applies
identically in metadata-only and record-expansion modes — the records opt-in governs
source/target ROW content only. Table-driven tests cover credential shapes, hostile
key names, nested objects, oversize strings and safe-value preservation.

Review round 1 (finding 6) closed the free-text hole: the original
bounded-credential-screened `safetext` constraint accepted ANY short control-free
value under known keys — an unmarked secret like `hunter2` passed unchanged. Free
text cannot be proven safe by shape, so the `safetext` constraint no longer exists
and the free-text detail keys (`reason`, `error`, `stage`) are not whitelisted at
all: they drop with the unknown keys and only a count survives. Numeric, boolean,
enum, `sqlstate`, `hex40`, `rfc3339`, `dbident`, `hostport` and `port` diagnostics
are preserved.

## Record expansion and the decode-vs-raw split (owner finding ⑤)

Selection is set-oriented keyset pages of 64 (batch bounded by `--max-records`; a cap
overrun past a batch boundary is a typed hard error — never truncation, never a panic —
and the exact-fit boundary succeeds across the split, review round 1 finding 2).
Expansion is ONE `REPEATABLE READ READ ONLY` target transaction + one source snapshot
per batch. Each blob is fetched ONE RECORD AT A TIME inside the target transaction,
immediately before that record's block is emitted — peak retention is a single blob
plus one chunk regardless of batch size, never a prefetched per-batch map (review
round 1, finding 1) — while file rows still stream with O(1) retention and the contract
block order holds. Under `--include-blobs` an over-cap blob is rejected from the
batch's `octet_length` metadata probe BEFORE any blob byte is fetched. The RR target
snapshot makes a concurrent target commit mid-batch invisible (proven by a mid-batch
`UPDATE` test whose export still delivers the snapshot's original bytes). Source read failures are classified
`ErrSourceUnavailable` (sanitized) while sink/cap failures pass verbatim; context
cancellation is an interrupt, never a source-unavailable claim. The decode verdict and
the raw export are decided independently: `missing_blob` (no row, `file_count≠1`),
`blob_null_data`, `blob_over_limit` (never fed to the codec), `assumed_single`
(validated reconstruction, `target_file.assumed:true`), the nine-code codec taxonomy
(parity-tested against 04B's `decodeErrorIssueCode`), or `decoded`. Raw export
(`--include-blobs`) emits the exact stored BYTEA as ordered 1 MiB Base64 chunks with
the documented lossless rule `concat(base64-decode(chunks by chunk_index)) == stored
bytes`, bound by the `target_blob` SHA-256 — including blobs far above codec limits.
Only a blob over `--max-blob-bytes` fails the whole export (size + flag named, temp
removed, no final file). `target_record.blob` classifies `"none" | "null_data" |
"stored_bytes"` — absence distinct from NULL.

## Caps and output safety

| Flag | Default | Breach |
|---|---|---|
| `--max-events` | 10,000,000 | hard error (naming cap + value + remediation) |
| `--max-records` | 1,000,000 | hard error |
| `--max-output-bytes` | 8 GiB | hard error |
| `--max-blob-bytes` | 64 MiB (2× codec) | hard error before materialization |

Internal constants: selection batch 64, keyset page 5,000, blob chunk 1 MiB, manifest
parse cap 1 MiB. The sink writes UNBUFFERED through the counting+SHA-256 writer
(deliberate deviation from the plan's `bufio` 256 KiB): byte counts and the checksum
are exact after every event, so caps are enforced exactly and an underlying write
failure surfaces on the event that hit it. File output: existing final paths and
symlinks refuse; the hidden same-directory temp is `O_EXCL` 0600 with an explicit
chmod (umask-proof); publish = `os.Link(temp, final)` + remove (atomic no-clobber
under concurrent creation — never a clobbering rename); parent dir fsynced. Any
failure, cancellation, cap breach or interrupt removes the temp and leaves NO final
file — a final path always names a complete artifact (a crash between link and unlink
can orphan a recognizable `.magnetico-export-*.tmp`, documented). `--output -` streams
metadata-only JSONL to stdout (records/blobs refuse; `--manifest` + stdout is a
config error) with a stderr warning that redirection is the operator's
responsibility; the final stderr summary carries the SHA-256. Logs carry redacted
URLs and basenames only — no raw paths, names, blob bytes or driver text.

## Manifest binding and registry-state digests (owner finding ②)

`--manifest` writes `<output>.manifest.json` (0600, no-clobber, fsync) ONLY after a
successful publish: format/version, artifact basename (resolved as a manifest
sibling), exact-byte SHA-256 + count, per-event counts, run/revision, schema class,
coverage + filters, include flags, limits, sanitized identities, generation time and
the registry-state digests — a deterministic SHA-256 per content table plus a
combined digest, streamed in canonical keyset order with every column of every row in
fixed field order (exact stored JSONB bytes, bytea as hex, NULL sentinel). This
detects ANY registry mutation — `running → completed_pass`, retry terminal
transitions, `updated_at`-only touches — even when `data_revision` and row counts
never moved. Cleanup validates the manifest structurally (regular files only,
symlinks refused, strict parse, coverage complete, no filters), re-hashes the
artifact, binds run/revision/counts to the live registry — and RECOMPUTES the full
digest set inside the drop transaction: any digest differing from the manifest
refuses cleanup as stale evidence (re-export required), tested with a post-export
`completed_pass → running` flip that leaves counts and revision untouched.

## Cleanup gates and waiver semantics

| Gate | Check | In-tx | Waivable? |
|---|---|---|---|
| G1 | advisory run lock held AND liveness-monitored for the whole command (`MonitorLockLoss`; a lost lock aborts with typed `ErrLockLost` — a SAFETY failure, exit 1, never the interrupt contract) | held | no |
| G2 | operational schema exactly v2 (re-classified) | yes | no |
| G3 | one progress row, completion phase (`import_complete_with_errors` only via override AND reconciled) | yes | override |
| G4 | strict `magneticod` schema via `persistence.MakeDatabase` | **pre-tx only** (deviation A6) | no |
| G5 | `TargetFacts` + `state.Reconcile` | yes | **never** |
| G6 | zero unresolved record errors + integrity incidents | yes | override |
| G7 | verification verdicts + independent persisted-issue validation | yes | ordinary failures only |
| G8 | `--confirm-backup` attestation | — | no |
| G9 | evidence: `--confirm-export --evidence-manifest` validated (structure, hash, binding, digests in-tx) OR the exceptional `--allow-missing-export-evidence --confirm-history-loss` (requires override + reason) | yes (digests) | no |
| G10 | flag coherence | before any connection | — |

G7's corrected semantics (owner correction, all regression-tested): persisted issue
rows must equal the run's `issue_count`; severity is recounted independently from the
table (the run's own `mismatch_count`/`unverifiable_count` are deliberately NOT
compared — 04B semantics leave source-only discrepancies and aggregate findings out
of the target-row counters); a normal pass requires ZERO blocking-severity
(`mismatch`/`coverage`/`unverifiable`) persisted issues (`info` permitted, reported in
the pass detail); target-row accounting `verified+mismatch+unverifiable+assumed ==
observed_target_ids` must hold; expected/observed coverage validated separately per
kind's non-NULL pairs. The two refusal classes are distinct and both pinned:
a `completed_pass` coexisting with actual blocking issues, a counter/row mismatch or
a violated accounting identity is **corrupt state** (`ErrRunStateCorrupt` inside
`ErrCleanupRefused`) and `--force-unverified` NEVER waives it; a failed/stale verdict
or a source-only mismatch on a non-pass run is ordinary and the override waives it.

Evidence attestations are path-exclusive and truthful (finding ⑦): the normal path
attests retained complete evidence (`--confirm-export` + `--evidence-manifest`, a
pair); the exceptional path attests NO evidence exists (`--allow-missing-export-evidence`
+ `--confirm-history-loss`, requires `--force-unverified --reason`, warns history is
irrecoverable); any cross-combination is a config error before any connection.
`--execute` is the single confirmation; the default is the non-destructive preview
(`--dry-run` is an explicit alias; exit 0 with blockers printed as findings).

## The drop transaction and the ambiguous COMMIT (owner finding ⑥)

Execution opens an explicit transaction, re-loads the state and re-runs every gate on
the transaction's own snapshot (drift → rollback + typed refusal "rolled back;
nothing dropped"), re-checks the run lock's liveness immediately BEFORE the DDL and
again BEFORE the COMMIT (`lock.Lost()` — a lost exclusion rolls the transaction back
and refuses with `ErrCleanupRefused`; the destructive DDL is never executed after
exclusion is lost; review round 1, finding 5), executes the 8 fixed DDL statements
(FK order, no `CASCADE`, no
`IF EXISTS` — a missing object or a foreign dependent object trips
`DROP SCHEMA … RESTRICT` and rolls the WHOLE transaction back, pinned by test), and
commits. A lost COMMIT acknowledgment is never success: a fresh read-only connection
classifies the outcome — schema still present → definitive rollback (exit 1);
schema absent → explicit `ErrCleanupAmbiguous` (exit 1) even when the production
schema is intact and row counts match the pre-drop snapshot, because absence plus
unchanged counts cannot establish byte-for-byte production equality or transaction
provenance ("do not prove it — inspect; do not re-run blindly"); inspection
impossible → the same ambiguous status with the reason. The destructive DDL is never
retried in any branch. An already-cleaned target is a harmless no-op (exit 0) that
reports honestly: past cleanup cannot be established from absence alone. v1 refuses
with upgrade guidance and never runs DDL; unrecognized drift refuses with the
catalog details.

## CLI wiring, renderer, exit codes

`migrate errors export` is the third nested level (per the retry-errors precedent);
bare `migrate errors` is a usage error; a `--database` positioned on the `errors`
group transfers onto the export (nested position wins), mirroring
`transferSharedFlagsToRetry`. The source DSN keeps the no-flag discipline
(config file `migrateSourceDatabaseURL` / `MAGNETICO_MIGRATION_SOURCE_DSN` only;
applies to `export`, validated-but-ignored on `cleanup`). Target-DSN fan-out
(`migrateDatabaseURL` yaml key + `MAGNETICO_MIGRATE_DATABASE`) covers every new
field; **no new YAML keys**; authorization booleans are CLI-only (retry-errors
precedent). Runners keep the renderer discipline (signal.NotifyContext, non-blocking
updates, explicit Stop before every exit — `os.Exit`/`log.Fatalln` bypass deferred
calls); the engine reports export phases `metadata`/`records` (events, record
groups). Every log line is controlled content: `redactDatabaseURL`, fixed sentinel
texts, bounded quoted engine-composed wrap details (corrupt-state detail lists; the
ambiguous classifier sanitizes its driver fragments at composition). Exit codes:
export 0 published / 1 fatal (refusals, caps, output collision) / 2 interrupted (no
file published); cleanup 0 for dry-run, definite COMMIT ack and the already-cleaned
no-op / 1 for gate refusals and ambiguous COMMIT / 2 for a pre-COMMIT interrupt
(rollback; nothing half-dropped). The binary smoke drives the real binary end to
end: import → verify-decode PASS → verify-content PASS → stdout export (pure JSONL,
progress on stderr only) → file+manifest export (both 0600) → dry-run (exit 0,
blockers printed) → execute without attestations (exit 1, nothing dropped) →
tampered artifact (exit 1, evidence rejected) → fresh export → gated drop (exit 0,
schema gone, production intact) → second cleanup (exit 0 no-op) → verify-decode
after cleanup (exit 1, no migration run state) → `--help` registration.

## Documented deviations and resolved ambiguities

- **A1** `--execute` is the single confirmation (matches the spec's own example; no `--yes`).
- **A2** Blobs are typed ordered 1 MiB chunk events with the documented lossless concat rule.
- **A3** `reconcile_status:"not_reconciled"` = "not evaluated; counters as recorded".
- **A4** Redaction is semantic, deny-by-default (owner finding ①), in both modes.
- **A5** Missing `magneticod` tables under records mode = hard typed refusal
  (`ErrRecordsTargetUnavailable`) before any byte is written.
- **A6** Production-schema gate (G4) recheck runs pre-transaction ONLY:
  `MakeDatabase` is DSN-scoped, and the drop statements reference only fixed
  `magnetico_migration` identifiers — the drops cannot touch `magneticod`.
  Documented deviation from the plan's "preferably in-tx".
- **A7** `--output` defaults to `-` (safe: records/blobs refuse stdout).
- **A8** The assignment's "50,000-file limit" is stale (`MaxFiles`=250,000) — moot
  anyway: the forensic file reads have no codec cap.
- **A9** Manifest freshness = the full registry-state digest recompute inside the
  drop transaction (owner finding ②), not row counts or timestamps.
- **A10** Evidence attestations are path-exclusive and truthful (owner finding ⑦).
- **Blob-class tokens**: the spec leaves the exact tokens open ("blob absence is
  distinct from a NULL-valued blob row"); implemented as `"none" | "null_data" |
  "stored_bytes"` (the plan draft had said `row` for the first).
- **Unbuffered sink**: the plan's `bufio` 256 KiB was dropped deliberately — the
  unbuffered counting writer makes caps exact after every event and surfaces write
  failures on the event that hit them (spec: bounded buffer; the bound is per-event,
  and no bufio can hide an ENOSPC until flush).
- **Missing `--reason`** (force-unverified without one, or the exception without
  one) is flag-coherence (`OptionsError`, G10) rather than a gate refusal — a shape
  error, not an evaluated finding.
- **`ExportInput.Progress`** (slice 7): an optional reporting hook (nil in the
  engine tests) so the CLI renderer can show the `metadata`/`records` phases; no
  engine behavior depends on it.
- **`classifyCommitOutcome` sanitization** (slice 7): the classifier's driver
  fragments pass `sanitizeDetailText` at composition, so main's bounded wrap-detail
  echo stays controlled content (the same trust rule as 04B's engine-composed wraps).

## Suites actually run vs skipped

All green on the final tree (pinned disposable `postgres:17`, per-test databases):

- `go build ./...`, `go vet ./...`, `gofmt -l .` (no output), `golangci-lint run` — 0 issues
- `go test ./... -p 2 -parallel 4` — 23 of 23 packages `ok` (EXIT=0, zero failures; root 55 s,
  migration 160 s, forensics 77 s, state 37 s, target 39 s, original 40 s)
- `go test -race ./internal/migration/... ./opflags/... -parallel 4` — all 9 packages pass
  (migration root 197 s, forensics 74 s, target 111 s, state 102 s, original 63 s under race)
- Census: 527 top-level test functions repo-wide — 261 in the migration tree + opflags
  (root runner 71, forensics 38, state 39, target 31, original 31, opflags 30, pipeline 12,
  progress 8, pgidentity 1; +8 in the repo root package). The battery ran without `-v`, so
  package-level results are the record: 23/23 packages `ok`, 0 failures, and NO reruns were
  needed in the final battery — the two parallelism incidents below happened in earlier
  full-suite probes, not in this run.

**Environment incidents, reported precisely** (this host exposes 16 CPUs, so a bare
`go test` runs the suite at `-parallel 16`): at that unbounded parallelism every
`DROP DATABASE` (which forces an immediate checkpoint, serialized cluster-wide)
overshot the harness's 20 s admin timeout in teardown-only failures, and the `-race`
run exhausted the container's `max_connections` (`SQLSTATE 53300`) in two
connection-attempt failures. Every affected test passes standalone and at the
battery's documented `-parallel 4` (zero assertion failures in either incident —
all failures were create/drop/connect infrastructure artifacts of the one-container
discipline, the same class 04B's report documented). Leftover `magnetico_test_*`
databases from an earlier interrupted run were dropped before the final battery.

## Test families delivered

- **State seam**: schema classification (absent/v1/v2/unrecognized/foreign/partial)
  with catalog-snapshot no-DDL proofs; the transaction-path variant; `ReadProgressRaw`
  (readable inconsistent counters, 0 rows, 2 rows); every keyset read incl. page
  boundaries; `SelectedRecordIDs` union/dedup/NULL-exclusion; `CountRegistry` v1+v2;
  DDL invariants (no CASCADE/IF EXISTS), happy drop, foreign-dependent rollback,
  `RESTRICT` tripwire.
- **Source streaming**: `StreamFileRows` past the codec file cap with duplicates
  (every `files.id`, O(1) retention); sparse `FetchTorrentRows`; `WithSnapshot`
  single-transaction sharing.
- **Redaction**: table-driven hostile shapes (credential values, hostile KEY names,
  nested objects, oversize strings, constraint failures) and safe-value preservation,
  in metadata AND records modes.
- **Export**: v1 incomplete (no-upgrade proof), failed-Reconcile (counters verbatim,
  zero production reads — no-production-schema fixture), order/shape/version/fidelity,
  authority flags (a historic pass visibly not current), footer == actual counts,
  filters → partial coverage; records expansion (present/absent, duplicate paths,
  empty files, assumed-single, missing_blob, null_data, corrupt-blob round-trip,
  over-limit raw export); the codec-vs-export cap split (over-limit decode finding
  AND full raw round-trip; over-cap → hard error); source-unavailable split
  (metadata OK, records fail; same-database refusal; missing production schema);
  caps → hard error with no finalized file; safe-file lifecycle (exists, symlink,
  race, 0600-under-umask, writer failure → no final file); manifest write + validate
  (tamper, wrong run, filtered, count mismatch, digest staleness after a
  `running → completed_pass` flip with unchanged revision+counts).
- **Cleanup**: dry-run read-only; green path (exact drops, production untouched,
  `schema_version` intact, post-state.Load = no run state) + already-cleaned no-op;
  with-errors override (refused without, dropped with); the gate-refusal table
  (importing phase, v1, unrecognized drift, missing content verification, stale
  verdict, missing/wrong-run/tampered/filtered/stale-digest manifests, missing
  backup attestation) — all no-DDL; flag contradictions (11 cases, never touching a
  database); corrupt-vs-ordinary verification classes (pass+blocking issue corrupt
  and unwaivable; issue_count mismatch; accounting violation; source-only mismatch
  ordinary and waivable; aggregate orphan blocking; info issues permitted);
  mid-drop rollback (foreign FK dependent); ambiguous-COMMIT classification (all
  branches); absent-schema no-op honesty.
- **CLI**: parser nesting/transfer/unknown-env rejection/source-DSN switch/yaml
  fan-out/strict-decode behavior; the wrapper cancellation contract; the binary
  smoke above (secret redaction asserted via `redactDatabaseURL` everywhere, stdout
  JSONL purity, 0600 modes, `--help` registration). `go test -race` over the whole
  migration tree and opflags.

## Owner review round 1 (tip `69ece9e8`)

The owner reviewed the pushed tip and reported six findings; all six are fixed without
redesigning the module, each with regression tests, in five commits:

| Commit | Finding(s) | Fix |
|---|---|---|
| `8a5f9433` | 4 | `verification_issue` envelopes stamp `progress.RunID` (was `""`); a test asserts EVERY emitted event carries the exact migration run_id |
| `a9d31959` | 1, 2 | Batch blob streaming redesign + RR target snapshot + pre-fetch cap rejection; `--max-records` overrun is a typed cap error, never a panic |
| `f163f88a` | 3 | NULL verification coverage is corrupt state, never valid |
| `7f73848b` | 5 | Advisory-lock liveness monitoring for export and cleanup |
| `fdb967ac` | 6 | Free-text metadata values are no longer whitelisted at all |

**Finding 1 — record-batch memory budget.** `expandBatch` no longer prefetches every
blob into a per-batch map (64 records × a 64 MiB cap ≈ 4 GiB worst case). The batch is
now ONE `REPEATABLE READ READ ONLY READ ONLY`-isolation target transaction spanning the
metadata scan, the per-record blob fetches and the emission: each blob's
`SELECT data` runs immediately before that record's block is emitted, so peak retention
is a single blob plus one chunk. Record ordering and snapshot consistency are preserved
— the target tx nests the source `WithSnapshot` (a READ ONLY tx holds no row locks, so
no deadlock), and the contract block order is unchanged. Over-cap blobs under
`--include-blobs` are rejected from the batch's `octet_length` probe BEFORE any blob
byte is fetched (zero fetches, zero events on rejection). Both properties are directly
observable: a pgx `QueryTracer` and an event-recording sink writer share one
mutex-guarded timeline — the test asserts `blobfetch:1 < target_blob:1 < blobfetch:2 <
target_blob:2` and exactly one metadata scan (the old prefetch-map code fails this),
and a concurrent `UPDATE` committed from the event hook MID-batch (different length AND
content) leaves the export delivering the snapshot's original bytes (sha256 + lossless
Base64 round-trip) — under READ COMMITTED the fetch would return the new bytes and the
assertions would fail.

**Finding 2 — `--max-records` panic.** When the previous batch consumed exactly
`MaxRecords` and more IDs remained, the next page was sliced to zero elements and
indexed `take[len(take)-1]` → panic. The exhausted budget is now checked BEFORE any
slicing or indexing: exactly-at-cap succeeds, one-past-cap is `ErrExportCapExceeded`
naming `--max-records`. Regression: 65 selected IDs with `--max-records=64` → typed
cap error and no output file; the exact fit (65 IDs, cap 65) succeeds across the batch
split with all 65 record blocks.

**Finding 3 — NULL coverage accepted.** `coverageHolds` treated NULL
`expected_target_ids`/`observed_target_ids` as valid coverage. NULL required coverage
columns are now corrupt verification state (`ErrRunStateCorrupt` inside
`ErrCleanupRefused`) — never waivable; verify-content runs additionally require
non-NULL `expected_source_ids`/`observed_source_ids`. Zero recorded counts remain
valid coverage: NULL is not zero (the green-path drop still passes with zero rows).

**Finding 4 — empty run_id.** `walkVerification` constructed
`newEnvelope(TypeVerificationIssue, "")`; every envelope now carries `progress.RunID`.
`TestExportEnvelopeRunIDOnEveryEvent` decodes every emitted line of a full export and
asserts the exact migration run_id on all of them.

**Finding 5 — lock liveness.** Both `ExportErrors` and `Cleanup` now monitor the
advisory lock after acquiring it (the accepted 04B runner pattern): a
`MonitorLockLoss` goroutine watches the lock connection; a lost lock cancels the run
context with a typed `ErrLockLost` cause, the work aborts, and the failure surfaces as
a SAFETY failure (exit 1) — never as the operator-interrupt contract (exit 2), pinned
at the wrapper level for both runners. Cleanup additionally re-checks `lock.Lost()`
immediately before the destructive DDL and again before the COMMIT: a lost exclusion
rolls back and refuses with `ErrCleanupRefused` ("nothing dropped") — the drop is
never executed or committed after exclusion is lost. Tests: lock loss MID-export
(the advisory backend is `pg_terminate_backend`-killed via a Progress hook; the export
aborts with `ErrLockLost`, no output, operational schema intact); lock loss BEFORE the
DROP (direct `runDropTransaction` with a killed + marked-dead lock → typed refusal,
nothing dropped, `CountRegistry` still works on the intact schema). The advisory-lock
kill query is database-scoped (`pg_locks` joined to `current_database()`) so paired
lock-loss tests on one shared cluster cannot terminate each other's backends.

**Finding 6 — free-text redaction hole.** The `safetext` constraint accepted any
short control-free value under known keys (`reason`, `error`, `stage`) — an unmarked
secret like `hunter2` passed unchanged. Free text cannot be proven safe by shape, so
the constraint was removed entirely and those keys are no longer whitelisted: they
drop with the unknown keys (deny-by-default) and only a count survives; the
`credentialish` screen survives only inside the `hostport` constraint. Numeric,
boolean, enum, `sqlstate`, `hex40`, `rfc3339`, `dbident`, `hostport` and `port`
diagnostics are preserved. Regressions: `{"reason":"hunter2","error":"short",
"stage":"decode"}` → all three dropped with `unknown_keys:3`; credential-grammar
values under former safetext keys → dropped.

New test families (8 new top-level test functions): the traced fetch/emit timeline
(batch budget), the mid-batch concurrent `UPDATE` (RR snapshot), over-cap pre-fetch
rejection, the 65/64 cap overrun + exact-fit pair, NULL coverage corrupt-vs-valid
(three subtests), the lock-loss trio (mid-export, pre-DROP, wrapper mapping),
every-event run_id, and the redaction regressions.

### Round-1 verification battery

Ran against the same pinned disposable `postgres:17` container (per-test databases,
small KiB-scale fixtures only; the 10% extract was never restored):

- Per-commit gates (all five commits): `go build ./...`, `go vet ./...`, `gofmt -l .`
  clean, `golangci-lint run` — 0 issues — plus each commit's focused regression tests
  (the round-1 test families above, run as they landed) against the pinned container.
- First combined verification run (00:59–01:12 local): race suite — every package `ok`
  except ONE infra-shaped failure, `TestStreamFileRowsBeyondCodecLimits`
  (`internal/migration/legacy/original`, "context deadline exceeded" at 40.2 s — the
  04B-era source-streaming test, untouched by every round-1 commit; it passed in the
  pre-push battery at `69ece9e8`); full battery `-p 2 -parallel 4` — ZERO failure
  lines with build/vet/gofmt/lint clean. Honest logging note: that run's recorded
  `RACE-EXIT=0`/`BATTERY-EXIT=1` were both artifacts of my log-filter pipes
  (`| tail` and `| grep -v` exit codes, the latter's "no lines selected"), so the
  true exit codes were re-established by rerun, not inferred.
- Clean rerun with true exit codes: `legacy/original` standalone under `-race` —
  `ok` 18.9 s, exit 0 (vs the 40.2 s deadline it exceeded under full-tree load);
  complete race suite rerun — all 9 packages `ok`, exit 0 (migration root 215.9 s,
  forensics 115.8 s, original 96.8 s, state 128.2 s, target 134.4 s). The incident is
  the documented load class (deadline pressure under full-tree `-race -parallel 4` on
  this 16-CPU host), not a regression.
- Full battery at `-p 2 -parallel 4` on the final tree: zero failures across all
  packages (the log filter passes through every non-`ok` line; none appeared),
  build OK, vet OK, gofmt clean, `golangci-lint run` 0 issues.
- Census: 535 top-level test functions repo-wide — 527 before the round plus exactly
  the 8 new round-1 test functions.

## Hand-off notes for 04D

- After a successful cleanup the operational schema is GONE: 04A/04B commands
  report `no migration run state on the target` (exit 1) and never recreate state on
  a nonempty target; the production data and `schema_version` are untouched. The
  binary smoke pins this.
- The evidence chain for a future re-import decision is: the manifest + artifact
  (retained by the operator) — cleanup refuses without it or with stale digests;
  the exceptional history-loss path exists precisely for the no-evidence case and is
  never silent.
- The export JSONL of a completed run is a complete audit artifact (footer present,
  digests bound); large registries are bounded by the four caps, which fail loudly
  rather than truncate — raise them deliberately with the remediation the error names.
