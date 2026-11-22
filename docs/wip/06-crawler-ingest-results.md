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
   the investigator's scratchpad, not committed. Not done: multi-hour soak (see plan below).
2. One 32-second run is an acceptance check of bounded behaviour, not a soak test. Long-duration, many-hour
   behaviour (autovacuum of the queue table, WAL growth of the SQLite file, cache hit rate) is unmeasured.
3. The crawler's DHT/leech path was validated in isolation (`metadata` tests under `-race`, shutdown with
   stalled peers) and through a loopback smoke without live DHT traffic. No run against the real BitTorrent
   network was performed.
4. Ingest YAML configuration is not supported (environment and flags only); tokens are environment-only by
   design. A single token; rotation requires restarting both sides.
5. Quarantine has SQL access only (no CLI); a failed record-specific rejection never leaves the crawler's
   quarantine automatically.
6. The consumer is single and sequential; throughput headroom is ≈19 k rec/s on the measurement host against
   an expected 10–50 rec/s.
7. The M0–M7 test runs avoided an unrestricted `go test -race ./...`. **Not executed:** the full-repository
   race run and `-race` on the root package. Targeted `-race` was run on every package changed by M4–M6 and
   by the post-M7 corrections.
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

## Soak / profiling plan for a separate job (PVE VM, ≈ 16–24 GiB RAM)

Not executed; the VM was not touched and its readiness is not assumed. The workload is synthetic, so no
torrent dump is needed. Prerequisites to confirm first: Go toolchain matching `go.mod`; a disposable
PostgreSQL 17 (native binaries, or `MAGNETICO_TEST_POSTGRES_DSN` pointing at an explicitly authorised
throw-away instance — the VM has no Docker); a checkout of this branch; ≥ 20 GiB free disk.

```
MAGNETICO_E2E_PROFILE_DIR=/var/tmp/ingest-prof MAGNETICO_E2E_PROFILE_SECONDS=14400 \
MAGNETICO_E2E_PROFILE_ENV="GOMEMLIMIT=384MiB" \
go test -count=1 -p 1 -timeout 6h -run TestE2EIngestMemoryProfile -v ./internal/e2e/
```

Run it twice (with and without `GOMEMLIMIT`). Watch: RSS/heap trend in `ingestd-mem.log` (flat vs growing),
queue table bloat (`pg_total_relation_size('magnetico_ingest.queue')`) and autovacuum, GC CPU share at the
limit, and heap profiles at the start, middle and end.
