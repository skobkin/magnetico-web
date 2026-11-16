# Task 04D implementation report — end-to-end fault-injection validation of the legacy torrent migration

Branch `refactor/0.8.0-unify-and-new-storage`. The product code validated is exactly the accepted
04A–04C tree at commit `c8b08570` (the 04D fixture-suite commit; zero product changes in 04D).
Assignment: [`docs/wip/04D-end-to-end-fault-validation.md`](04D-end-to-end-fault-validation.md).
No spec file was edited. No production database was contacted. The sample archive was never
modified (R0/R11 prove size, mtime and SHA-256 unchanged); the restored source database was
SELECT-only throughout (no VACUUM, no ANALYZE, no destructive SQL, no v0→v1 automated migration).
Every fault scenario followed the owner's evidence protocol: pre-interruption state recorded,
trigger fired only after the target state was **observed** (no sleeps, no timing guesses), the real
signal and exit status recorded, an immediate post-interruption snapshot taken, resume through the
normal CLI only (no manual repair anywhere), recovery proven beyond exit codes. Raw evidence for
every claim below is preserved under `.scratch/04D/` (gitignored) — per-scenario directories with
`poll.log`, `post-death-snapshot.txt`, `exit-status.txt`, `summary.md`, migrator stderr, VmHWM
samples and WAL LSN before/after.

## What shipped

| Commit | Content |
|---|---|
| `test(main): 04D fault-injection fixture suite with observed-state triggers` | `main_faults_test.go` (F1–F6): SIGKILL mid-import with exact post-death atomicity math; wire-level lost-COMMIT-ack proxy (simple-protocol framing, checkpoint-anchored arming, server-side durability proof, in-process reconciliation to exit 0); target-connection loss → lock-loss refusal → resume; advisory-lock exclusion; mid-import source-drift refusal (then clean resume after the drift is removed); restarted-source ≠ drift; verification staleness after a retry bumps `data_revision`; `.gitignore` `/.scratch/` |
| `docs: 04D report and index row` | This report + the README index row. The real-sample campaign (below) is manual runbook evidence, deliberately not committed |

Battery at report time: `go build ./...` OK, `go vet ./...` OK, `gofmt -l .` empty,
`golangci-lint run` 0 issues; `go test -count=1 -p 2 -parallel 4 ./...` all green (rc=0); the
`-race` pass on `./internal/migration/... ./opflags/ .` first ran unbounded and failed with the
04B-documented teardown-only DROP DATABASE timeout flakes plus one load-dependent `DBOpTimeout`
in a streaming test — rerun with the documented `-p 2 -parallel 4` limiter: **all 10 packages ok
(rc=0)**, zero leftovers in the pinned container. The failure is the known environment artifact,
not a code defect.

## Environment and provenance (R0, `.scratch/04D/R0-preflight/`)

- Sample: symlink `magneticod-sample.sql.gz` → the owner-provided dump; size **2,409,777,887**,
  mtime `2026-10-27 18:27:30.122130246 +0400` (both equal to the 04B report), plus SHA-256
  `964b05df97c25a680a7a8c17bab0c829f574de5bfc81f59883b6d80ea486d78e` (recorded for the first time;
  04B recorded only size+mtime). `gzip -t` clean.
- Pinned binary: repo rev `c8b08570…`, `go1.27.4` linux/amd64, kept at `.scratch/04D/bin/magnetico`.
- Pinned container `magnetico-04c-pgtest` (postgres:17, **AutoRemove=false**, **static** published
  port `127.0.0.1:15472`). TCP password auth prerequisite recorded (fe_sendauth →
  `ALTER USER postgres PASSWORD …` inside the disposable container only). Role `magneticod` (the
  dump's `ALTER SCHEMA … OWNER TO magneticod`) created as an R0 prerequisite. 198 GB free at start
  (floor 60 GB). Stop conditions recorded verbatim in `R0-preflight/stop-conditions.md`.
- Schema lesson recorded: `migrate schema-init` creates only `magneticod.*` (v1, 0 rows); the
  `magnetico_migration` operational schema is created by the migrator itself
  (`internal/migration/state/store.go:68`) — observed live in R1b.

## Scenario matrix (real sample)

**R1 — restore (once): EXERCISED.** 542 s wall (04B: 566 s — independent runs, not asserted
equal), `ON_ERROR_STOP=1` exit 0, stderr empty. Counts **exactly** 3,457,046 torrents /
123,460,039 files; anchors: torrent ids 1→38,594,156, file ids 1→1,297,921,112,
sum(total_size)=11,065,551,382,535,969, discovered_on 1,571,524,111→1,791,956,101,
`migrations.schema_version=0` (v0 legacy source).

**R2 — import SIGTERM at observed scanned ≥ 1,200,000: EXERCISED at phase `importing`.**
Trigger fired at committed scanned=1,207,040 after monotonic advance across polls (`poll.log`).
Exit **2** (graceful `ErrInterrupted`, not shell 143). Post-death authoritative checkpoint
`last_processed_id=11,580,708`: rows-beyond-checkpoint **0**, scanned = success + errors,
torrent_rows = distinct ids = distinct info_hash, blob rows = blob_count = 763,987,
`indexes_deferred=t`, only the two constraint indexes present. Peak RSS 793 MiB.

**R3 — resume, SIGKILL at observed scanned ≥ 2,400,000: EXERCISED at phase `importing`.**
Checkpoint adoption proven by the first poll = R2's post-death values exactly (1,207,040 /
11,580,708). SIGKILL → exit **137** (death by signal 9). Post-death: scanned advanced
2,405,376→2,405,632 between trigger and death = one committed batch; atomicity intact
(rows-beyond **0**, all identities distinct, blob rows match). Peak RSS 866 MiB.

**R4 — SIGKILL during `building_indexes`: NOT EXERCISED on the original target (harness
defect), then EXERCISED as R4b with owner approval.** The original driver's `case` ordering made
the building-index branch unreachable (`KILL@BUILDING_INDEX` was swallowed by the earlier
`KILL@*` pattern; non-numeric threshold compare always false) — **no probes were ever issued**;
the run completed to `import_complete` rc=0 instead. Root cause documented after isolating the
harness: protocol experiments on a scratch database (dropped afterwards) proved a pgx
extended-protocol `CREATE INDEX … USING GIN` **is** visible to the exact probe SQL for the whole
build, and the migrator's own log shows the GIN-trgm window was **~81 s** — trivially hittable by
a working driver (2 s cadence). The product was never implicated. Per the boundedness directive
the full import was *not* rerun automatically; the owner explicitly approved one bounded retry on
a fresh disposable target.
**R4b — kill: EXERCISED at phase `building_indexes`.** Fresh DB + `schema-init` (0/0, v1); full
import 10m5s; trigger fired on positive simultaneous evidence — `pg_stat_activity` active query
text `CREATE INDEX idx_torrents_total_size ON magneticod.torrents (total_size)` plus the catalog
snapshot (0 of 3 secondary indexes present) — SIGKILL → **137**. Post-death: phase
`building_indexes`, `indexes_deferred=t`, **0 of 3** secondary indexes (per-index VALID-or-absent
assertion), data intact (3,457,046 = success = distinct, blobs 2,247,594, rows-beyond 0).
**R4b — resume: EXERCISED.** Plain CLI adopted the checkpoint, rebuilt 1/3 (+1 s btree),
2/3 (+2 s btree), 3/3 (+68 s GIN), `import_complete` **rc=0** in ~88 s; all three indexes
`indisvalid=t, indisready=t` with definitions **exactly** the product DDL
(`btree(total_size)`, `btree(discovered_on)`, `gin(name gin_trgm_ops)`).

**R5 — complete + invariants + idempotent re-run: EXERCISED.** Final state: `import_complete`,
scanned = success = **3,457,046**, errors = 0, blob_count = **2,247,594**, rows-beyond **0**,
distinct ids = distinct info_hash = 3,457,046, max id = 38,594,156 (== R1 anchor),
`torrents_id_seq` reseeded (last_value 38,594,157, is_called=f → next issued id > max id;
blobs are keyed by torrent_id, no sequence). Idempotent re-run with `--defer-secondary-indexes`
→ **rc=0 in ~16 s**, state byte-identical. (`--defer-secondary-indexes` was passed on **every**
invocation of this run, as Preflight's immutability check requires.)

**R5b — container restart ≠ drift: EXERCISED.** `docker restart` issued 12:08:17Z; readiness in
~17 s; published port unchanged (static binding). `pg_postmaster_start_time()` **changed**
(01:41:26→12:08:17) while every content anchor stayed identical — the restart was genuinely
exercised. Post-restart idempotent import → **rc=0**, no drift refusal ("stored source fingerprint
does not match…" never appeared), state unchanged.

**R6 — full verify-decode, SIGKILL mid-scan: EXERCISED.** ~135 polls logged committed
`verified_count` advancing 44,279→1,002,791 (monotonic); SIGKILL → **137**. Run row left
`seq=1, state='running', phase='scanning'`, target_checkpoint=15,760,990, mismatch/unverifiable/
issues **0**, assumed_single=567,769, `verification_issues` rows **0** (issues never ahead of the
checkpoint). Peak RSS 51 MiB; decode rate ~1,470/s.

**R7 — decode resume: EXERCISED.** First poll = **seq=1** (same row — adoption, not a new run) at
R6's exact values, continuing in place. ~45.4 min → **rc=0**: `completed_pass @ data_revision 0`,
verified **2,247,594**, assumed_single **1,209,452**, mismatched/unverifiable/issues **0**,
coverage 3,457,046 = 3,457,046 — all equal to the 04B full-pass numbers (same sample).

**R8 — full verify-content, SIGTERM mid-scan: EXERCISED.** Committed progress observed
(checkpoints 1,539→3,821,246, observed ids 1,536→363,264); SIGTERM → **2** (graceful). Run row
`seq=2, state='interrupted'` with committed checkpoints; decode run seq=1 untouched. Trigger
counter note: field 5 of the content poll row is `target_checkpoint` (committed), not
`observed_source_ids` — same committed-progress semantics, recorded as-is.

**R9 — content resume: EXERCISED (closing equality gate).** First poll = **seq=2**
`interrupted` at R8's exact values; the resumed run flipped it to `running` and continued in
place. ~65 min → **rc=0**: `completed_pass @ data_revision 0`, source_checkpoint =
target_checkpoint = 38,594,156, expected = observed = **3,457,046 on both sides**, verified
3,457,046, mismatched/unverifiable/assumed_single/issues **0** — migrator verdict "PASS (full
source↔target equality scan)".

**R10 — forensic export + manifest (NO cleanup): EXERCISED.** `migrate errors export --output …
--manifest` → rc=0; 5 events / 4,576 bytes; **independent `sha256sum` == manifest.sha256**
(`d42ae8f1…3dd24`); run binding, `data_revision=0`, registry-state digests recorded
(errors/issues/retry tables digest = SHA-256 of empty input — 0 rows, consistent with the whole
campaign having 0 record errors and 0 issues); `coverage: complete`. The `cleanup` command was
**not** executed against this target; the artifact and manifest are retained for owner review.

**R11 — post-campaign: EXERCISED, no deviations.** Source fingerprint re-read == R1 exactly;
archive size/mtime/SHA-256 == R0 exactly; 178 GB free (floor 60 GB); DSN-leak grep over all
captured logs/artifacts: **0** occurrences of the campaign password; the migrator's own output
prints redacted DSNs (`postgres://127.0.0.1:15472/…`, no credentials). Databases
(`magnetico_04d_sample_{src,tgt,tgt2}`) and all evidence retained for owner review; recovery
procedure below.

## Fixture suite (small-fixture fault classes, committed)

| ID | Scenario | Result |
|---|---|---|
| F1 | SIGKILL mid-import (fixture ~10–20k rows), observed-state trigger | EXERCISED — dies by signal; post-death checkpoint math exact; resume exit 0, full ID-set equality |
| F2 | Lost COMMIT ack at the wire level (test-local TCP proxy; checkpoint-anchored arming; server-side durability proof) | EXERCISED — intercepted tx committed server-side (checkpoint advanced beyond the recorded pre-injection value), run reconciled in-process to exit 0 with exact counts, no dup/skip |
| F3 | Target backends terminated mid-import | EXERCISED — lock-loss fatal, exit 1; rerun resumes exit 0 |
| F4 | Concurrent import under advisory lock | EXERCISED — second invocation refuses ("holds the lock"), first completes |
| F5 | Mid-import source drift (insert beyond fingerprint) then removal | EXERCISED — resume refuses exit 1 with the fingerprint detail; after removal resumes exit 0 |
| F6i | Restarted source ≠ drift | EXERCISED — postmaster change recorded, anchors unchanged, resume succeeds |
| F6ii | Verification staleness after a retry bumps `data_revision` | EXERCISED — older verdict stale, fresh run re-verifies |

Reused without new code (cited): interrupted/resumed import, ambiguous-commit in-process doubles,
lock-loss during finalization, deferred-index crash boundaries
(`runner_integration_test.go`, `finalize_integration_test.go`), verify interrupt/resume incl.
extras, retry lifecycle, export tamper/staleness refusal, cleanup gates incl. rollback +
ambiguous-COMMIT classification + already-cleaned no-op.

## 04B baseline vs 04D results (deliberate deltas)

The accepted 04B report proves the clean positive path (one uninterrupted import, full
verification, 0 discrepancies). 04D never re-asserts 04B's timings; it adds the fault/resume
delta. Where 04D's final counters overlap 04B's (3,457,046 torrents; 2,247,594 verified;
1,209,452 assumed_single; 0 discrepancies) they **match exactly**, as required for the same
sample. Deliberate deviations from 04B's procedure: `--defer-secondary-indexes` on every import
invocation (owner-approved); interrupts are the point; archive SHA-256 recorded; container-restart
step and WAL sampling added; absolute scratch path recorded. Observed timings (04D, recorded, not
asserted equal): full import ~10–15 min at 5,000–5,800 rows/s; full verify-decode ~57 min total
across its two invocations; full verify-content ~66 min; peak RSS: import ~906 MiB (04B: 931 MiB,
same order), verifies 46–72 MiB; overall WAL from 6/7670A000 to 8/8262A3B0.

## Limitations (honest)

- `assumed_single` (1,209,452) remains the decode-side historical blind spot: blobs whose source
  rows are gone cannot be decoded against full file lists. It is **not** an equality hole:
  verify-content classified all 3,457,046 source↔target rows with 0 issues, including those
  1,209,452. `source_not_representable` did not occur (0 record errors end-to-end).
- The wire-level lost-COMMIT-ack scenario (F2) is fixture-level by design; no real-sample
  equivalent was attempted (the proxy would need the campaign's real traffic; the fault class is
  fully covered by the deterministic fixture).
- `docker restart` exercises process/postmaster churn, not host power loss; asserting anything
  about power-loss semantics was out of scope per the assignment.
- R4's original miss was a harness defect (documented above with its reproduction and protocol
  experiments); the fault class it targets is covered both by R4b on the real sample and by the
  committed fixture boundary tests.

## Reproducible recovery (all steps normal CLI only, observed live in this campaign)

Interrupted import: rerun `migrate torrents --database <target> --defer-secondary-indexes` —
Preflight adopts the committed checkpoint (phase, `last_processed_id`, counters, immutability of
the deferral option) and continues; a kill inside `building_indexes` is recovered by the same
command (catalog reconciliation: valid identical indexes kept, missing ones rebuilt). Interrupted
verification: rerun the same `verify-decode`/`verify-content` command — the `running`/`interrupted`
run row is adopted at the same `seq` and continued; after a `data_revision`-bumping mutation the
old verdict goes stale and the fresh run re-verifies. Forensics: `migrate errors export
--manifest` re-derives everything from the surviving registry. Nothing in the campaign ever
required manual checkpoint/DDL repair.

## Pre-production blockers

1. **Pooled Brotli encoder A→B determinism** (carried from 04B; outside 04D's boundary — separate
   codec task). Integrity in 04D was established as decode + canonical-content comparison only,
   never byte equality.
2. *None added by 04D.* Every fault scenario behaved as specified: exact checkpoint resumption,
   all-or-nothing batches (0 rows beyond any post-death checkpoint), index rebuilds
   definition-identical and valid, restart ≠ drift, revision staleness honored, exit codes 0/2/137
   (and 1/3 semantics from the committed suite) as designed. The two honest incidents (R4 driver
   case-order bug; R6 first launch 126 = missing script exec bit) were harness-side, fixed,
   regression-checked, and re-run — no product change resulted.
