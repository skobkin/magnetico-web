# Task 04D — Real-sample end-to-end recovery and fault-injection validation

**Status:** Assignment for agent planning; **not authorization to execute the real-sample experiment**.

**Branch:** `refactor/0.8.0-unify-and-new-storage`. Start by fetching and checking the current remote tip (accepted 04C: `f9801bcb`). Preserve unrelated work. Do not push without explicit owner authorization.

**Normative sources:**
- `docs/specs/legacy-torrent-migration-v1.md`, especially §§1–3, 6–8, 11–12.
- `docs/specs/legacy-torrent-verification-recovery-v1.md`.
- `docs/specs/legacy-torrent-forensics-cleanup-v1.md`.
- `docs/engineering-standards.md` and the established testing/commit conventions.
- `docs/wip/04A-core-migrator-report.md`, `04B-verification-recovery-report.md`, `04C-forensics-cleanup-report.md`.

## Objective

Complete the remaining 04D acceptance campaign: prove the **implemented 04A–04C tools** survive controlled failures, resume safely from PostgreSQL state, and still produce full, exact source-to-target equality. This is a **validation task, not a new migration design or a production cutover**.

**Avoid duplicate work:** The accepted 04B report already records a successful full real-sample restore, import, `verify-decode`, and `verify-content` on **3,457,046 torrents and 123,460,039 file rows** (source ~17 GB, target ~2.9 GB; 0 discrepancies). Treat that as the positive-path baseline. A fresh 04D sample run must add *meaningful crash/resume/fault coverage*; do not merely rerun the same long sequence and call it 04D.

The accepted 04C report already covers targeted export/manifest/cleanup fixtures. Reuse those results; extend them only where end-to-end failure recovery requires it.

## Hard boundaries

1. **Never connect to or modify production.** Work only with disposable PostgreSQL 17 databases/container(s). No automatic service startup, deployment, cutover, user migration, web rewrite, crawler changes, wire/schema changes or source adapter expansion.
2. The historical **original boramalper/magnetico v0.12.0** sample remains immutable. The source side is SELECT-only; do not run the old automated v0→v1 migration, `VACUUM`, `ANALYZE` or destructive SQL on it.
3. Do not edit the accepted normative contracts unless the owner explicitly approves a change. If a reproducible product bug is discovered, demonstrate it with a small deterministic regression first and propose the narrowest fix; do not conceal it with a test-only workaround.
4. Avoid speculative “faults” that risk the source. Inject crashes, backend kills, network interruptions and restarts only against isolated target/test processes and the disposable PostgreSQL environment. Never induce OOM of the host.
5. **Plan Mode first.** No real-sample restore, large experiment, destructive SQL, repo changes or push until the owner reviews the 04D plan. The 04B one-time successful experiment is not implicit authorization for another restore.

## Required planning deliverables

Produce a concrete, bounded test matrix with **trigger → expected durable state → recovery command → observable proof**, and a resource/execution runbook. Include the following coverage:

### A. Interrupted import and accurate resume

- One **large-sample** import genuinely interrupted *after* one or more committed checkpoints but *before* completion; choose an observable deterministic trigger. Include a `SIGTERM` graceful scenario and a `SIGKILL`/lost-process scenario; at least one must exercise the real sample. Avoid rerunning the entire 3.46M import multiple times when smaller fixtures can validate additional failure classes.
- Resume using the ordinary `magnetico migrate torrents` command and the persisted `magnetico_migration` progress. Confirm monotonically increasing checkpoint, exact `scanned = successes + unresolved errors`, preserved historical IDs and no duplicate or silently overwritten rows; verify source remains unchanged.
- Exercise a genuinely uncertain transaction outcome (lost COMMIT acknowledgement) and restoration of a target connection, preferably on a **small fault-injected fixture**, with explicit evidence of no replay-induced duplication or skipped source IDs.
- Test source/target identity checks, advisory-lock exclusion/loss and mid-resume source-drift refusal using small fixtures where adequate. An unchanged PostgreSQL source restarted between invocations must not be mistaken for content drift.

### B. Index finalization and verification recovery

- Controlled interruption during deferred-secondary-index finalization and resume from **catalog + operational state**, preserving constraints and the exact three allowed secondary indexes. Use small fixtures unless the large run can cover this naturally without a redundant long import.
- Interrupt `verify-decode` and `verify-content` after committed verification checkpoints; resume and prove full coverage without duplicate findings or false passes. Make sure the latest-run-authority rule and `data_revision` staleness survive retry mutations.
- Once the interrupted real-sample import has resumed successfully, run **complete** `verify-decode` and **complete** `verify-content`, not a sample, and record their exact outcomes. A target-only pass does not substitute for full source equality.
- Exercise a targeted `retry-errors` lifecycle on a small controlled invalid-source fixture: repeated failure, correction with explicit acceptance when appropriate, revision increment, stale verification rejection, and fresh verification. Never mutate the actual historical sample for this purpose.

### C. Forensic export and destructive cleanup gates

- Validate the end-to-end evidence chain: complete metadata JSONL + footer, SHA-256 manifest bound to the exact registry state, tamper/staleness refusal, fresh evidence acceptance and independent retention of artifact/manifest.
- Test `migrate cleanup` on a **dedicated disposable target/fixture** with truthful backup/evidence attestations, exact transaction-scoped drops, unchanged `magneticod`/`schema_version`, idempotent absent-schema response, rollback on a forced failure and explicit ambiguous-COMMIT handling. **Do not automatically drop the large-sample target's migration state** merely to satisfy this item; preserve the evidence for review or justify a separately approved deletion.
- A missing, corrupt or partial verification/export state must never produce an unconditional cleanup pass. Do not use `--force-unverified` as a shortcut for a green-path result.

### D. Security, boundedness, and failure reporting

- Confirm source SELECT-only permissions, target/source separation, no DSN/password/path leakage and bounded resource usage. Capture actual peak RSS, disk/WAL usage, wall time and exit codes. The 04B baseline used `/proc/<pid>/status` `VmHWM` sampled at 1 Hz because `/usr/bin/time` was unavailable; explicitly record whichever method is actually used.
- Use `set -o pipefail`, record the real exit status of each process (not `tail`/`grep` pipeline status), and retain sanitized structured logs/checkpoint snapshots. On failure, do not claim a partial restore or partially verified run was successful.
- Preflight the available sample archive **without modifying it** (size/hash/mtime, readability, role/ownership statements, available capacity), and show the exact stop conditions. 04B required `CREATE ROLE magneticod NOLOGIN` before `psql -v ON_ERROR_STOP=1` restore; include it as an explicit disposable-container prerequisite, not an ad hoc retry.
- Use one pinned disposable PostgreSQL 17 container and bounded test concurrency, preferably leveraging `internal/pgtest`; never run large-scale tests in parallel with `go test`/race/lint. Do not assert that SIGKILL, OOM or a power loss invokes cleanup hooks. Document manual recovery of orphaned test databases and containers.

## Expected artifacts

After an owner-approved plan and execution:

1. Focused deterministic regression/integration tests or a minimal opt-in test harness where needed; **do not add a permanent mandatory multi-hour CI test**. Keep the real-sample campaign opt-in with explicit prerequisites and a non-production guard.
2. `docs/wip/04D-end-to-end-validation-report.md`: pinned binary/revision, sample provenance, exact scenario matrix (attempted/pass/fail/skipped), signals/fault triggers, checkpoint and schema snapshots, resume evidence, full verification results, exit codes, time/RSS/disk measurements, unambiguous limitations and reproducible recovery procedure. Explicitly distinguish the accepted **04B baseline** from **new 04D results**.
3. Update `docs/wip/README.md` with the 04D assignment/report and the resulting acceptance gate; do not imply that a successful 04D test authorizes production cutover.
4. Full code battery (when code/tests change): build, vet, gofmt, golangci-lint, `go test -count=1 -p 2 -parallel 4 ./...`, race tests for affected migration/opflags packages. Record actual subprocess exit codes, skips, retries and infrastructure failures honestly.
5. A list of unresolved **pre-production blockers**. In particular, the independently demonstrated **pooled Brotli encoder A→B history/determinism problem** is explicitly outside 04D and must be tracked as a separate focused codec task before cutover. Do not redefine codec byte equality as an integrity check; use decode + canonical-content comparison.

## Stop points

**First response:** return the 04D plan only. Identify (a) the minimum number of full historical-sample restores and multi-hour passes necessary, (b) which faults run on the real sample versus small fixtures, (c) required disk/CPU time and safe cleanup, (d) exact assertions per failure boundary, and (e) any discrepancies between the 04B baseline and the proposed 04D scenarios. Then **stop for owner review**; do not execute or push.

**During a later approved experiment:** an incomplete restore, source drift, unknown catalog shape, missing prerequisite, exhausted capacity or ambiguous migration state is a stop condition. Never rerun a destructive/expensive step automatically. The owner decides whether to correct the environment and make another attempt.
