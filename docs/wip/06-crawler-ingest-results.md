# Task 06 — M7 end-to-end validation results and release readiness

Suite: `internal/e2e` (real `magnetico ingest` / `magnetico crawl` binaries, a test-only crawler harness
`internal/e2e/crawlerd` that runs the production `Enqueuer`, `Sender` and SQLite queue over a synthetic
metadata source, and a disposable PostgreSQL 17 container from `internal/pgtest`). Run with
`go test -count=1 -p 1 -v -timeout 900s ./internal/e2e/` (≈75 s). Nothing touches a production database.

## Results (one run, all passed)

| Scenario | What it proves | Result |
|---|---|---|
| Three crawlers, overlapping hash ranges (1–300, 151–450, 301–600) | 600 distinct hashes stored exactly once; queues empty; `usage` equals table sums | pass |
| PostgreSQL refuses connections mid-run (`ALLOW_CONNECTIONS false` + backend termination), then recovers | ingest survives, crawler keeps queueing, all 400 records delivered after recovery | pass |
| SIGKILL of `ingest` with rows pending in PostgreSQL (6 pending, 54 stored at the kill), restart | all 600 stored exactly once | pass |
| SIGKILL of the crawler three times at 0.4 / 0.9 / 1.5 s, same queue directory, final run | all 500 stored exactly once (51 → 122 → 267 stored at the kills) | pass |
| Lost HTTP acknowledgement (proxy forwards, then drops the 202, three times) | resend is idempotent; 300 stored once | pass |
| Quarantine persistence | 3 invalid records quarantined locally, survive a restart and are not re-sent; a server-side junk row is quarantined and survives an ingest SIGKILL/restart; usage accounting exact | pass |
| Queue corruption | `crawl check-queue` passes a healthy queue, fails a corrupted one, and does not modify the file | pass |
| Loopback smoke, real binaries | `ingest` and `crawl` start (crawler with no database, loopback-only DHT filter), `crawl` prints its summary and both exit 0 on SIGTERM; a stale `MAGNETICO_CRAWL_DATABASE` is refused with a pointer to `UPGRADE.md` and without echoing the secret | pass |
| Load at the expected rate: 40 records/s for 32 s (1,200 records, measured size mix: 70 % one file, 25 % 30 files, 5 % 400 files) | no backlog growth | pass |

Load run measurements (1,200 records, 32.0 s wall): throughput 37.5 rec/s (the producer's pace); maximum
crawler queue depth 0; maximum PostgreSQL backlog 1; crawler RSS ≤ 26 MiB; **ingest RSS ≤ 321 MiB**;
crawler queue directory 44 KiB; `magnetico_ingest.queue` relation 864 KiB. Sampled every 2 s, the stored
count rose linearly (≈80 per interval) with a backlog of 0–1 throughout.

Earlier milestones' measurements (spec §11): SQLite disk/payload 1.0–1.35×; queue append p50 626 / 2,347 /
14,894 records/s with 1 / 8 / 64 producers; drain ≈ 19 k records/s.

## Coverage of the approved fault list

- Multiple concurrent crawlers, PostgreSQL outage and recovery, ingest restart with pending records, lost
  acknowledgement, idempotent redelivery, quarantine persistence, queue corruption, realistic-rate
  throughput, backlog/memory/disk observations, loopback smoke: **executed (above)**.
- SIGKILL at the commit/acknowledgement boundaries: the **crawler** and **ingest** processes were killed
  at arbitrary points (above). The two *exact* boundaries are covered deterministically in-process, not by
  signal timing: "after server commit, before response" by the lost-acknowledgement proxy test, and "after
  `AddTorrent`, before queue delete" by `TestConsumerCrashAfterAddTorrentIsIdempotent` (a hook fails the
  step between the two). Process-level SIGKILL cannot be aimed at those instants without production fault
  hooks, which were deliberately not added.
- Disk-full: only the SQLite `SQLITE_FULL` path (`max_page_count`, M2 tests) was exercised. No real
  full-filesystem test was run, and PostgreSQL disk exhaustion was not simulated.

## Known limitations and open observations

1. **Ingest memory — investigated (post-M7 review).** Method: `TestE2EIngestMemoryProfile` (opt-in via
   `MAGNETICO_E2E_PROFILE_DIR`) runs the test-only `internal/e2e/ingestd` wrapper (production `server.Run` +
   loopback pprof + a MEM line every 2 s) under 90 s at 40 records/s (measured size mix) followed by a
   3,000-record burst, with heap/allocs/goroutine profiles. Default settings: idle RSS 24 MiB; peak RSS
   395 MiB (HWM 450 MiB); after settling 387 / 326 MiB; heap live at last GC 169 MiB, goal ≈ 340 MiB. After
   `debug.FreeOSMemory`: **live heap 8 MiB, RSS 38 MiB** — so the memory is runtime-retained garbage and
   freed-but-unreturned heap, not a leak and not live data; goroutines stay at 12. Allocation profile:
   **68 % of all allocated bytes are `brotli.(*h5).Initialize`** (hasher tables of the Brotli encoder inside
   `codec.compress`, reached via `AddTorrent` → `Encoder.Encode`); HTTP body/JSON decoding is ≈ 10 %; the
   8 MiB existence cache is as configured. The in-use snapshot at steady load shows ≈ 160 MB held by Brotli
   hasher state. This follows from the codec's documented design (`codec/compress.go`: fresh writer for
   payloads ≥ 256 KiB, pooled writer below, kept for determinism), which is wire-format territory and was not
   changed. **Operational mitigation, measured:** `GOMEMLIMIT=128MiB` → peak RSS 144 MiB (HWM 167 MiB), same
   workload fully drained, at the price of more GC cycles (1,230 vs 10; total GC pause 230 ms over the run;
   CPU cost not separately measured). Recommendation: run `ingest` with `GOMEMLIMIT` ≈ 256–512 MiB, and
   treat reducing Brotli encoder allocation as a separate codec issue. Raw data: the run directories are in
   the investigator's scratchpad, not committed. The multi-hour soak and a controlled CPU comparison were
   done afterwards: see "VM qualification" below, which supersedes the 128 MiB suggestion.
2. One 32-second run is an acceptance check of bounded behaviour, not a soak test. The two-hour soaks
   (queue-table autovacuum, SQLite WAL growth) are now measured, see "VM qualification". The ingest existence
   cache hit rate is still unmeasured: the synthetic workload issues no `exists` requests.
3. The crawler's DHT/leech path was validated in isolation (`metadata` tests under `-race`, shutdown with
   stalled peers) and through a loopback smoke without live DHT traffic. No run against the real BitTorrent
   network was performed.
4. Ingest YAML configuration is not supported (environment and flags only); tokens are environment-only by
   design. A single token; rotation requires restarting both sides.
5. Quarantine has SQL access only (no CLI); a failed record-specific rejection never leaves the crawler's
   quarantine automatically.
6. The consumer is single and sequential; throughput headroom is ≈19 k rec/s on the measurement host against
   an expected 10–50 rec/s.
7. The M0–M7 test runs avoided an unrestricted `go test -race ./...`. This was later executed in full on the
   VM and passed, see "VM qualification".
8. **Baseline comparison of `internal/migration` and `legacy/original` (post-M7 review).** Clean worktree at
   `879903ba` (immediately before M0) versus the current branch, each package run separately, no `-race`,
   `-p 1`, `-count=1`, 25 min timeout, `GOMAXPROCS=4`, `GOMEMLIMIT=2GiB`, inside a 6 GiB cgroup memory cap,
   sequentially, one disposable PostgreSQL 17 container (the harness's). Results — baseline: `original` ok
   (13.5 s; 30 pass, 1 skip), `migration` ok (132 s; 72 top-level pass). Current: `original` ok (19.0 s;
   30 pass, 1 skip), `migration` ok (124 s; 72 pass). **The earlier teardown timeouts did not reproduce in
   either tree in one run each.** One run per tree cannot show that they are fixed or were never present
   (they were reported under concurrent load, and two unrelated `postgres:17` containers were also running
   on the machine); the cause remains unattributed.

## Deployment instructions (staging)

See `UPGRADE.md` ("Unreleased 0.8.0") for settings. Order: `schema-init` → `migrate torrents` →
verification → optional `cleanup` → `ingest-schema-init` → start `ingest` → start crawlers.

Checklist before staging:

- [ ] PostgreSQL `fsync=on`, `synchronous_commit=on` (ingest refuses `fsync=off`); a backup exists.
- [ ] TLS terminated by the proxy; request body limit ≥ 17 MiB for `/api/ingest/v1/torrents`; the crawler
      uses `https://`.
- [ ] Both tokens ≥ 32 bytes and identical; supplied by secrets, not in YAML or the command line.
- [ ] Crawler queue directory on a local, persistent, `fsync`-honouring volume; size ≥ `queue-max-bytes`.
- [ ] Ingest `QUEUE_MAX_BYTES` (2 GiB default) sized for the longest expected ingest-to-PostgreSQL outage.
- [ ] Alerting on the periodic summary lines: `crawl_dropped_queue_full`, `crawl_send_unrecognised_rejections`,
      `guard_trips`, ingest `queue_full`, `storage_errors`, `lock_losses`, and growing quarantine counts.
- [ ] Container stop timeout ≥ 60 s for the crawler (shutdown grace: 5 s writer + 30 s sender + 10 s queue
      close) and ≥ 40 s for ingest (bounded: ingest returns within 28 s of SIGTERM; see spec §8). Exit status 3
      means a stuck goroutine was abandoned on purpose (crawler: the SQLite writer, recovered by the next
      start; ingest: the consumer or a handler, whose interrupted record is reprocessed idempotently).
- [ ] Operator SQL for quarantine reviewed (`UPGRADE.md`).

## VM qualification (Task 06 qualification run, 2026-11-22)

Nothing here touched production or the public DHT; all databases were disposable `postgres:17` containers
and the workload is synthetic (the harness's measured size mix: 70 % one file, 25 % 30 files, 5 % 400 files).
Raw profiles, CSV time series and logs are in `/home/herdr/qual/` on the VM, outside Git.

### Environment

| | |
|---|---|
| VM | PVE guest under Herdr Core, Debian 13, kernel 6.12.117 |
| CPU / RAM | AMD Ryzen 9 7950X3D, 8 vCPU / 31 GiB (7 GiB swap, never used) |
| Go / Docker / PostgreSQL | go1.27.0 linux/amd64 / 29.10.0 / `postgres:17` (default configuration, `fsync=on`) |
| Commit | race run: `a9c794ef`; profile runs: `deee89d5` (= `a9c794ef` + the test-harness commit below) |
| Disk | `/home` 113 GiB free (> 20 GiB). `/` has only ≈ 10 GiB and holds `/var/lib/docker`; `/tmp` is a RAM tmpfs |

**Deviation (disk).** Because Docker's root is on the small `/`, the profile/soak runs did not use the
harness's per-process container (its data would have lived on `/`). They used pgtest's documented
`MAGNETICO_TEST_POSTGRES_DSN` path with one dedicated, freshly created disposable `postgres:17` container
(data directory on `/home`, bound to `127.0.0.1` only), recreated before every run. The race run used the
default harness (containers on `/`). `TMPDIR` was on `/home` for every run. Docker's configuration and the
unrelated `beszel-agent` container were not touched.

### Full-repository race test

```
go test -race -count=1 -p 1 -timeout 45m -v ./...      # in a worktree at a9c794ef
```

**Pass:** 30 packages `ok`, 3 without test files, no failure, no `DATA RACE`, 7 min wall (06:13:17–06:20:13).
Slowest: `codec` 65.7 s, `internal/e2e` 65.6 s, root 64.8 s, `persistence` 21.4 s, `metadata` 18.6 s,
`internal/migration` 16.6 s. Host observations (10 s samples): load average ≤ 7.5, memory used ≤ 7.6 GiB,
no swap, `/` free ≥ 10 GiB, ≤ 3 containers. No timeout was extended; none was needed. The earlier teardown
timeouts of the laptop runs did not occur. Finding: `internal/migration/forensics` uses pgtest without
`pgtest.RunTests` in `TestMain`, so its container was **left running** after the run (removed by hand; it
was the run's own container). Not changed here (out of scope), a one-line follow-up.

### Harness extension (test-only, commit `deee89d5`)

`ingestd` reports process CPU and GC CPU (`runtime/metrics`) and now **refuses any pprof address that is not
a literal loopback IP** (unit-tested, also when started by hand). `TestE2EIngestMemoryProfile` writes a
10-second time series (`timeseries.csv`: memory, CPU, crawler queue/WAL, PostgreSQL backlog, queue relation
size, dead tuples, autovacuum counters, free disk), checks integrity (exactly-once persistence, no
quarantine, usage accounting, error-like log lines, crawler `DONE` counters), and ends the run with
diagnostics preserved on any **early-stop condition**:

- ingest anonymous RSS > 1.5 GiB, goroutines > 200, free disk < 12 GiB, ingest or crawler exited;
- steady-phase crawler queue > 500 or PostgreSQL backlog > 2,000 records for 5 min;
- no persistence progress for 2 min with a backlog, records lost (queue empty, count short), or a drain
  not finished within 15 min;
- live heap or anonymous RSS median growing in two consecutive 20-min windows (> 50 % and 64 MiB, or > 25 %
  and 100 MiB) after a 20-min warm-up;
- GC > 60 % of ingest CPU over 10 min; any quarantine row; usage drift in 3 consecutive samples; any
  error-like log line.

None triggered in any run.

### Short comparison (12 min at 40 records/s, then a 3,000-record burst; identical workload)

Command per row: `MAGNETICO_E2E_PROFILE_SECONDS=720 MAGNETICO_E2E_PROFILE_ENV="<env>" go test -count=1 -p 1
-run TestE2EIngestMemoryProfile -v ./internal/e2e/` (disposable PostgreSQL via the DSN variable above).

| | default | `GOMEMLIMIT=256MiB` | `384MiB` | `512MiB` |
|---|---|---|---|---|
| Accepted / persisted per s | 40.0 / 40.0 | 40.0 / 40.0 | 40.0 / 40.0 | 40.0 / 40.0 |
| RSS mean / p95 / max (MiB) | 403 / 469 / 499 | **269 / 280 / 292** | 363 / 426 / 453 | 408 / 502 / 524 |
| Peak RSS incl. burst; HWM (MiB) | 499; 516 | **292; 293** | 453; 454 | 524; 524 |
| Live heap mean / max (MiB) | 171 / 202 | 137 / 169 | 160 / 169 | 166 / 201 |
| Heap alloc mean / max (MiB) | 268 / 383 | 196 / 229 | 249 / 333 | 264 / 392 |
| Heap in use max (MiB) | 388 | 231 | 336 | 398 |
| Heap released at end (MiB) | 162 | 93 | 229 | 231 |
| GC cycles (steady) / total pause | 29 / 3 ms | 104 / 8 ms | 23 / 2 ms | 35 / 7 ms |
| Ingest CPU, steady | 4.9 % of a core (34.8 s) | 4.9 % (34.6 s) | 5.0 % (35.3 s) | 5.3 % (37.3 s) |
| of which GC CPU | 0.1 s (0.2 %) | 0.2 s (0.6 %) | 0.0 s (0.1 %) | 0.1 s (0.3 %) |
| Crawler queue depth max (steady) | 0 | 0 | 0 | 0 |
| PostgreSQL backlog max, steady / burst | 0 / 2,403 | 0 / 2,415 | 0 / 2,431 | 1 / 2,416 |
| Burst fully persisted after producer finished | 4.0 s | 4.0 s | 4.0 s | 4.0 s |
| Queue relation max (KiB); dead tuples max | 1,816 → 3,952; 4,261 | 1,816 → 3,944; 4,232 | 1,824 → 3,952; 4,250 | 1,824 → 4,112; 4,269 |
| Autovacuum runs (analyze) | 12 (12) | 12 (12) | 12 (12) | 12 (12) |
| Final | 31,800 of 31,800 stored; 0 quarantined; usage drift 0 | same | same | same |

Reading: the ingest process uses ≈ 5 % of one core in every configuration. The limit changes memory, not CPU:
at 256 MiB GC takes 0.6 % of that 5 % (≈ 4× the cycles, 8 ms total pause). 384 and 512 MiB barely bind
(the default GC goal is already 340–410 MiB), so they save little or nothing. The ingest-side time was
dominated by system time (≈ 45 %), i.e. HTTP and PostgreSQL I/O, not GC.

### Long soaks (7,200 s at 40.0 records/s, then a 3,000-record burst; run sequentially)

Both: 288,000 + 3,000 records. Command: `MAGNETICO_E2E_PROFILE_SECONDS=7200 MAGNETICO_E2E_PROFILE_PROFILE_EVERY_MIN=30
MAGNETICO_E2E_PROFILE_ENV="<env>"` as above. A: default (07:13–09:14), B: `GOMEMLIMIT=256MiB` (09:14–11:14).

| | A default | B `GOMEMLIMIT=256MiB` |
|---|---|---|
| Verdict | **pass** | **pass** |
| Stored / expected; distinct hashes | 291,000 / 291,000; equal | 291,000 / 291,000; equal |
| Ingest summary (final) | `storage_errors=0 quarantined=0 retried=0 lock_losses=0 queue_full=0` | same |
| Quarantine rows / usage drift / error lines | 0 / 0 / 0 | 0 / 0 / 0 |
| RSS mean / p95 / max / HWM (MiB) | 459 / 552 / 621 / 634 | **304 / 313 / 354 / 355** |
| Live heap mean / max (MiB) | 167 / 205 | 141 / 201 |
| GC cycles / total pause | 301 / 59 ms | 900 / 91 ms |
| Ingest CPU (steady) | 358 s = 5.0 % of a core | 374 s = 5.2 % |
| GC CPU | 0.9 s (0.3 %) | 2.0 s (0.5 %) |
| Goroutines max | 12 | 12 |
| Crawler queue depth max / PostgreSQL backlog max (steady) | 1 / 1 | 0 / 1 |
| Burst: peak backlog; persisted after producer finished | 2,325; 4.1 s | 2,343; 4.1 s |
| Burst peak RSS (MiB) | 401 | 310 |

Time-series observations (four 30-min windows each, medians): **no sustained growth after warm-up.**
Anonymous RSS median A 431 / 459 / 429 / 447 MiB, B 292 / 293 / 293 / 294 MiB. Live heap median A 169 / 169 / 169 / 172,
B 137 / 137 / 137 / 140 MiB. Goroutines ≤ 12 throughout. Backlogs never recovered from anything because they
never grew. Twelve profiles each (every 30 min plus start, mid, after-load, after-burst): in-use heap at
steady state is ≈ 10 MiB at the sample point after GC (the 8 MiB existence cache dominates); the rest is
garbage awaiting collection.

Storage behaviour. **PostgreSQL queue table:** autovacuum ran 120 times over 2 h (one per minute, `autoanalyze`
counted the same) in both soaks; the queue relation stayed ≤ 1.8 MiB (heap ≤ 0.98 MiB) in the steady phase and
≤ 4.0 MiB at the burst peak, dead tuples ≤ 2.3 k (≤ 4.3 k at the burst), never accumulating across windows
(per-window maxima 2,319 → 2,289 → 2,278 → 2,248). No bloat. The `torrents` relation grew linearly to 65 MiB
(122 MiB database; 126 MiB after the burst). **SQLite (crawler queue):** database file 44 KiB, `-wal` peak
4.0 MiB, flat (4,035–4,055 KiB per window), no unreclaimed growth; the queue was empty after every drain.
**Disk:** `/home` free 111 → 110 GiB, no pressure. Crawler RSS ≤ 28 MiB, CPU ≈ 280–290 s per 2 h.

### Deviations and limits

- Disk-root deviation and the forensics container leak: above.
- The first 60-second smoke run exposed two harness errors (steady rate diluted by a sample taken after the
  producer exited; negative crawler CPU), fixed before the reported runs. No reported number comes from it.
- The profiling wrapper's heap figures are measured in the test-only `ingestd` (production `server.Run`); the
  shipped `magnetico ingest` has no pprof or sampler.
- Not covered: real DHT traffic; records larger than the measured mix (up to the 17 MiB request limit);
  concurrent crawlers during a soak; the existence endpoint; PostgreSQL on a separate host or under
  `shared_buffers` tuning; the PostgreSQL container's own CPU. One run per configuration (the two soaks are
  independent, but the short comparison is single-run each). Three-thousand-record bursts are small relative
  to the 2 GiB queue limit.

### Recommendation: `GOMEMLIMIT=256MiB` for `magnetico ingest`

Evidence: over two hours it lowered mean RSS from 459 to 304 MiB and peak RSS from 621 to 354 MiB, with no
measurable throughput, backlog, drain-time, correctness or CPU difference (≈ 5 % of a core either way; GC
0.5 %). It must be read as a **Go runtime memory target**, not a hard limit: it cannot lower live data, and
the process may exceed it (here the live heap was ≤ 205 MiB, close to it; a workload with a larger live
set, e.g. larger records, makes the GC run more often rather than keeping RSS below the figure). Leave the
container memory limit at least 2× higher (≥ 512 MiB; the default-runtime peak was 634 MiB HWM). Watch the GC
share if records grow: `ingest` CPU must stay in the single digits percent. 384/512 MiB are not worth
setting (no benefit measured). Added to `UPGRADE.md`.

### Follow-up proposal (not done; wire format and Brotli encoder untouched)

*Reduce Brotli encoder allocation churn in `codec.compress`.* Evidence: in the 2 h default soak
`brotli.(*h5).Initialize` allocated 24.9 GB = **53 % of all 46.7 GB** allocated (≈ 85 KB per stored record),
`buildTrie`/`appendTrieNode` 14 %, JSON decoding 8 %. It is what makes the default GC goal 340–410 MiB for a
≈ 170 MiB live set; the runtime-retained heap, not live data, is the memory. It does not currently cost
measurable CPU (GC 0.3 %), so this is an optimisation, not a defect. Any change must keep the codec's
determinism rules (`codec/compress.go`) and needs its own task and owner approval.

### Verdicts and readiness

Race suite **pass**; short comparison **pass** (no anomalies); soak A **pass**; soak B **pass**; no early stop,
no correctness defect, no lost, duplicated or quarantined records, usage accounting exact, all queues drained.
Remaining blockers from this qualification: **none**. Open items unrelated to the verdicts: the
`forensics` TestMain container leak; real-DHT acceptance check (separate, not run); behaviour with large
records and several concurrent crawlers (not covered). The implementation is, on this evidence, ready for the
next staging-review decision.
