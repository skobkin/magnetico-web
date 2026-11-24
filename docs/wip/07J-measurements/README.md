# Task 07J measurements

Raw results and the scripts that produced them. The interpretation, the acceptance gates and the container memory recommendation are in [`../07J-web-integration-security-report.md`](../07J-web-integration-security-report.md). Everything here ran on **synthetic data** in **disposable** databases on the development VM (AMD Ryzen 9 7950X3D, 8 logical CPUs visible, 32 GiB RAM, Linux 6.12, Go 1.27.0, PostgreSQL 17.12 in Docker). No production system, no DHT, no BitTorrent or tracker traffic. **No credential is stored in any file here**: tokens and session identifiers existed only in the memory of the generator.

| Path | What |
|---|---|
| `explain.sh`, `explain/` | `EXPLAIN (ANALYZE, BUFFERS, SETTINGS)` of the production search statements on the large dataset (20 shapes; `SUMMARY.md` plus a plan per shape) |
| `*.jsonl` | one run each (`steps`, `soak`, `diag`, `memfloor`): `start`, one `segment` per load segment, `drain`, per-process `process` reports, the sampler's `samples` |
| `summarize.py` | renders a run as Markdown (segments, process peaks, soak memory windows, diagnostic readings) |
| `*-summary.md` | the output of `summarize.py` for each run |

## Reproduce

1. Start PostgreSQL 17 with the data directory under `/home/herdr` (the root file system of this VM is small), `fsync=on`, `synchronous_commit=on`, `shared_buffers=512MB`, bounded container logs (`--log-driver local --log-opt max-size=10m --log-opt max-file=3`).
2. `magnetico migrate schema-init` on an empty database, then seed it. Every harness command refuses to run unless the database is **named explicitly** (`MAGNETICO_W9_CONFIRM_DISPOSABLE_DB`, which must equal the DSN's database), **every host in the DSN is local** (loopback or a unix socket, fallback hosts included), and, for the load harness, the database carries the **benchmark marker**, a database comment `magnetico-w9-benchmark:w9-large-v1` that the seeder sets before it writes the first row:

   ```text
   MAGNETICO_BENCH_LARGE=1 MAGNETICO_W9_DSN=postgres://…/bench MAGNETICO_W9_CONFIRM_DISPOSABLE_DB=bench \
     MAGNETICO_W9_DATADIR=<data dir> \
     go test . -run TestW9SeedLargeDataset -count=1 -v        # MAGNETICO_W9_SCALE=0.01 for a pilot
   ```

   The seeder refuses a non-empty database and stops if less than 40 GiB is free under the data directory. A database seeded before the marker existed is marked once, explicitly, with `MAGNETICO_W9_MARK=1 MAGNETICO_W9_DSN=… MAGNETICO_W9_CONFIRM_DISPOSABLE_DB=bench go test . -run TestW9MarkBenchmarkDatabase -v` (it refuses a database with non-seeder multi-file torrents or accounts other than `w9u00`…`w9u31`).
3. Run a profile (`steps`, `soak`, `memfloor`, `knee`, `regress`, or `diag` with `MAGNETICO_W9_TAGS=w9diag`):

   ```text
   MAGNETICO_BENCH_LARGE=1 MAGNETICO_W9_DSN=… MAGNETICO_W9_CONFIRM_DISPOSABLE_DB=bench \
     MAGNETICO_W9_OUT=<dir> MAGNETICO_W9_LABEL=<name> \
     MAGNETICO_W9_PROFILE=steps go test . -run 'TestW9Load$' -count=1 -v -timeout 4h
   ```

   Knobs: `MAGNETICO_W9_NO_CONTINUATION=1` (no search paging follow-ups: the earlier campaign's workload), `MAGNETICO_W9_STEP_SECONDS` (180), `MAGNETICO_W9_SOAK_SCALE` (1; the soak phases are 30/90/20 minutes), `MAGNETICO_W9_WEB_MEM` (1G), `MAGNETICO_W9_WEB_GOMEMLIMIT` (768MiB).
4. `python3 summarize.py <run>.jsonl > <run>-summary.md`.

## Method in one page

- **Processes.** `magnetico web` and `magnetico ingest` each run in their **own** systemd user scope (cgroup v2; web `MemoryMax` per the knob, `CPUQuota=200%`, `GOMAXPROCS=2`, `GODEBUG=gctrace=1`; ingest `MemoryMax=1G`, `CPUQuota=100%`, `GOMEMLIMIT=256MiB`). The limits apply to each process alone, never to the two together; PostgreSQL and the generator are outside both and share the host, so CPU contention between them is a limitation.
- **Identities.** 32 users, each with one real session (created through the login form behind a configured trusted proxy, each with its own `X-Forwarded-For`), 10 API tokens and 10 RSS tokens minted through the account page: 320 of each. Requests rotate through the pools so no token comes near its limit (API 2 requests/s, feeds 0.5/s per token).
- **Mix.** 45 % search (common, pairs, mid, selective, none, Unicode, empty; three sorts, both directions, three page sizes), 20 % details, 12 % directory pages, 5 % complete tree (200, or the expected 422 for large torrents), 10 % API search, 2 % API directory pages, 3 % RSS (v1 and v2), 3 % magnet redirect.
- **Outcomes** are classified per request and reported separately: 2xx as expected, 429, 503 *busy* (a resource gate), 503 *search timeout*, other 5xx, unexpected 4xx (any status outside the request's expected set), transport error or client timeout (90 s). A run in which 429s exceed 0.1 % is invalid.
- **Models.** *Closed loop*: N workers send the next request as soon as the previous answered. Up to the capacity of the four search slots this measures throughput; beyond it (32 and 64 workers) it is an **overload** test in which the bounded 503 *busy* answer is the designed behaviour (they return instantly, so their count is large and meaningless as a rate). *Interactive*: N users with an exponentially distributed think time (mean 1 s); the plan's bound of 5 % busy applies here.
- **Ingest.** During the soak's steady phase a producer posts 20 records/s to the real ingest HTTP interface; the queue depth is read every 10 s from the small, bounded `magnetico_ingest.queue` table (no scan of the 3.5 M-row table), the drain must finish within 10 minutes of the producer stopping, and afterwards every acknowledged record is looked up once, in index batches.
- **Memory windows (soak).** The first 30 minutes are warm-up (the file-index cache fills to its 128 MiB cap and starts evicting). The steady window is reduced to 10-minute medians; the slope of heap-after-GC and of RSS over it is reported as a share of the plateau per hour. The thresholds (2 %/h heap-after-GC, 5 %/h RSS) are **screening heuristics** chosen to trigger an investigation, not leak verdicts. The idle phase has **no HTTP request at all** (sampling reads `/proc`, the cgroup files and one bounded PostgreSQL count); RSS is not required to return to its pre-load value because bounded caches retain memory by design and the scavenger returns pages lazily.
- **Goroutines and heap detail.** `SIGQUIT` would end the process, and the production binary carries no profiling endpoint, so a **separate diagnostic run** uses a binary built with the test-only tag `w9diag`, which adds a loopback-only statistics endpoint (goroutines by creation site, memory statistics, file-cache and pool statistics). The default build contains none of it (tests guard this). The diagnostic run is not used for performance numbers.
