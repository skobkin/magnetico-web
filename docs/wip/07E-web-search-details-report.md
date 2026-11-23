# Task 07E / W4 — Search, torrent details and file browser: implementation report (Steps 6–8, stop at S2)

Branch `refactor/0.8.0-unify-and-new-storage`, continuing from C1 (`aeb3454b`). Plan: `07E-web-search-details-plan.md` (final values at its end). C1 evidence: `07E-c1-report.md` (decisions in §14). Raw measurements: `07E-w4-measurements/`.

## 1. Outcome

| Item | Status |
|---|---|
| Step 6 — production file-index cache (`internal/web/filecache`) | done, tested under `-race` (repeated) |
| Step 7 — `/torrents`, `/torrents/{id}`, `/torrents/{id}/files`, `/` redirect, templates | done, tested at handler level (real PostgreSQL) and with the real binary |
| Step 8 — measurements: cold/warm, 8 and 32 clients, mixed with search and login, cancellation, 1 GiB cgroup, `GOMEMLIMIT` range, 15-minute soak | done; targets met or missed as listed in §8 |
| Optional 1M/5M-row search measurements | **not run** (not needed; search-index tuning is deferred by the owner) |
| Codec, schema, indexes, storage format | unchanged (only the additive `codec.PeekDecodedSize` from W4 step 3) |

## 2. Commits (local, not pushed)

| Commit | Content |
|---|---|
| `5d9242fd` | `feat(web): file index cache with single-flight builds and weighted decode gate` |
| `d605f904` | `feat(web): torrent search, details and file browser` (handlers, templates, `Server.OnDrained`, `auth.SessionFormToken`, `/` redirect, `main` wiring) |
| see `git log` | real-binary W4 tests and W3 test updates; docs (final values, C1 decisions) with leak/allocation tests; load harness, `Stats.Abandoned`, measurements, deployment notes, this report |

(`git log aeb3454b..HEAD` is authoritative.)

## 3. Design as implemented

**Cache (`filecache`).** Cold path: `FileIdentity → lookup → single-flight → FileHeader → PeekDecodedSize → gate (192 MiB, weighted) → FileBlob → DecodeFileList → BuildArena → insert`.

- **Identity** = (id, file count, total size, blob `xmin`); revalidated on *every* request (one cheap statement, no blob/TOAST access) and expired after 10 min. A mismatching entry is dropped and rebuilt; a request never gets an index of another blob version (tests: replacement, blob swapped mid-fetch).
- **Single flight** per identity key. The flight has its own context: cancelled when *every* waiter leaves *before the decode starts* (checked atomically in `beginDecode`, and the flight is unregistered under the cache lock in the same step so no new request can join a doomed flight); after the decode starts it finishes and caches its result even if all requesters disconnected.
- **No connection held** during queueing or CPU work: each `Source` call is one short statement. Admission precedes the whole-blob read (test: a saturated gate issues no blob fetch).
- **Weight** `stored + declared + 24·files + P + 36·files`, `P = min(files·MaxPathBytes, MaxAggregatePathBytes)` — an admission *estimate*. Largest weight for any blob valid under the default limits = 32 + 64 + 64 MiB + 60 B × 250,000 = **174.3 MiB ≤ 192 MiB** (verified in code at startup, pinned by `TestMaxWeightFitsDefaultCapacity`). No clamping anywhere; a weight above capacity is `ErrTooLargeToIndex` (only possible with raised limits).
- **Retention:** only the `Arena` is cached (neither the blob nor `[]File`). LRU by bytes (128 MiB, `Index.Bytes()` + 256 B/entry); an entry above the budget is served and not retained (counter `Oversize`). Eviction deletes map/list references (a `weak.Pointer` test proves the index becomes collectable). Errors and cancelled builds are never cached.
- **Retry:** at most 3 identity lookups per request on `ErrBlobChanged`; corruption/limit errors are not retried.
- **Shutdown:** `Server.OnDrained` runs after requests drain and before the background task and the database pool stop. `Cache.Close` cancels database phases, waits for build goroutines (bounded) and logs counters. A decode still running at the deadline is CPU-only and touches neither database nor cache afterwards. Gate permits are released by `defer` on success, error, cancellation and recovered panic.

**Handlers (`torrents`).** Search maps outcomes to distinct responses: invalid query/cursor → 400 with its own text; busy → 503 `Retry-After: 5` "busy"; timeout → 503 `Retry-After: 30` "did not finish in the configured time"; client cancellation → nothing rendered. No `COUNT`, no `OFFSET`; page links carry `q`, `sort`, `order`, `size`, `cursor`. Details never decode the blob (test asserts zero cache activity). Files: `dir` is read **once** from `URL.Query()` (already percent-decoded) — a name containing literal `%2F`, `%`, `+`, `&`, `#`, `?`, `.`, `..`, empty components or invalid UTF-8 round-trips exactly (tests walk every directory link of an adversarial torrent and compare with an independently derived directory set; `a%252Fb/` → `a%2Fb/`, `a%25252Fb/` and `a/b/` are 404). Names render through `pathfmt` (`\xNN`, `\u{XXXX}`, `(empty)` markers; no U+FFFD). Missing/corrupt/oversized/mismatching blobs → typed 500 pages (no list), logs carry only id, category and limit numbers. Layout gets a nav with the session CSRF token for logout (`auth.SessionFormToken`); all styling is the W1 foundation plus a few functional rules (W5 owns design).

## 4. Tests

Real PostgreSQL (disposable Docker `postgres:17` via `internal/pgtest`), synthetic data only.

- **`filecache`** (fake source, deterministic channels/hooks, `-race`): 100 navigations → exactly 1 decode / 99 hits / 100 identity revalidations; 24 simultaneous misses → 1 build; different torrents build concurrently; all-waiters-cancel (one leaver must not cancel; last leaver does; new request starts a fresh build); decode completes after its requester left and serves the next request; errors not cached; blob changed mid-fetch → retry serves the *new* version; bounded churn (exactly 3 attempts); limit/corruption not retried; missing blob / single-file / unknown; replacement; expiry boundary; LRU eviction + release; oversize; gate saturation (busy, no blob read, bounded wait); panic releases everything; `Close` cancels DB phases and a late decode never touches the source; goroutine-leak check; stress with cancellations. Mutations (disable abandonment cancel; ignore identity) make the tests fail. A genuine race (build goroutine reaching `beginDecode` before the creator registered as a waiter) was found by repeating tests 300× and fixed.
- **`torrents`** (full handler stack + real auth): anonymous routing; search sort × direction × both pagination directions with ties (forward walk, then backward reproduces earlier pages exactly, links keep parameters); empty/ignored-words/invalid/cursor states; info-hash lookup; escaping; search timeout + busy + login responsiveness with a real table lock; details incl. malformed ids; full list → details → files → nested directory → logout; unusual-path round trip and markers; 500-row pagination and positions (invalid `from` → 400); cache hit/replacement via real blob `UPDATE`; typed 500s for missing/garbage/mismatching/33 MiB blobs with log-leak checks; no search text/names/paths/cursors in logs; decode saturation → 503 `Retry-After: 5`, details still 200, recovery.
- **`server`**: `OnDrained` ordering and incomplete-shutdown on a failing hook.
- **Real binary (`main_webauth_test.go`, `main_webbrowse_test.go`)**: login → search → details → files → nested directory with the documented runtime role grants; graceful stop (exit 0, no secrets in process output); forced shutdown (search blocked on a table lock, 1 s grace → exit status 3 and "grace period expired"), login answers in < 3 s while a search is stuck.
- W3 tests updated for the replaced homepage (`auth.Config.RootHandler` test override; real-binary tests use `/torrents`).

## 5. Validation

See §10 for the exact commands and outcomes.

## 6. Measurements (reference configuration)

**Machine:** AMD Ryzen 9 7950X3D, 8 logical CPUs visible, 32 GiB RAM, Linux 6.12.117, Go 1.27.0. **Server process:** the real `magnetico web` binary, `GOMAXPROCS=2`, `GOGC=100`; for cgroup runs inside a systemd *user scope* (`systemd-run --user --scope`, cgroup v2) with `MemoryMax=1G`, `MemorySwapMax=0`, `CPUQuota=200%`. **PostgreSQL:** 17.12 in a disposable Docker container (`--user` uid 1000, data bind-mounted under `/home/herdr/w4bench`, published on loopback), `shared_buffers=512MB`, `max_connections=200`, `fsync=off`, `synchronous_commit=off`, `work_mem=4MB` (benchmark-only settings; not production advice). Database not CPU/memory limited. **Web settings:** defaults (`POOL_MAX_CONNS=10`, `SEARCH_CONCURRENCY=4`, `SEARCH_TIMEOUT=60s`). **Data:** 9 named synthetic torrents (typical-250k, longpath-250k, flatmax-250k, deep-250k, wide-250k, adversarial-50k, longname-1500, typical-10k, typical-1k), 24 pool torrents (75k–225k files, ~10 MiB index each → ~240 MiB total, so the 128 MiB cache must evict) and 300,000 blob-less torrents for search. Load generator on the same host (loopback HTTP); one session shared by all workers. Gated load test: `main_webload_test.go` (`MAGNETICO_BENCH_LARGE=1`).

Workload "files" (conc8/conc32/mixed): 60 % warm directory navigation in typical-250k, 35 % root of a random pool torrent (cold/warm mix with eviction churn), 5 % a random heavy shape. It is **deliberately cold-heavy** (cache hit rate ≈ 73 %); real use should be warmer. Duration 60 s per scenario unless stated.

### Cold build and warm navigation (single client, nothing else running)

| Dataset | Cold request (decode+index+render) | Warm request |
|---|---:|---:|
| typical-250k | 199 ms (first run of a fresh process: 530 ms) | 3.2 ms |
| longpath-250k | 154 ms | 1.3 ms |
| flatmax-250k | 499 ms (1.1 s in the first run) | 3.2 ms |
| deep-250k | 508 ms | 3.0 ms |
| wide-250k | 370 ms | 3.1 ms |
| longname-1500 | 677 ms | 0.8 ms |
| adversarial-50k | 17 ms | 2.6 ms |
| typical-10k / 1k | 7 / 2 ms | 1.0 / 0.6 ms |

(Two runs differ by up to 2×; cold times include PostgreSQL blob fetch and a JIT-free first touch of the heap. The target is ≤ 2 s p95.)

**Warm navigation, 1 client, 60 s, 300 directories of typical-250k:** 104,993 requests, p50 **0.5 ms**, p95 0.7 ms, p99 0.9 ms, max 142 ms (target p95 ≤ 50 ms, p99 ≤ 150 ms). **Exactly one decode** over all 105,293 requests (`builds=1 hits=105292`). Allocations per warm navigation through the full stack including the real `FileIdentity` SQL round trip (`TestWarmNavigationAllocations`, 1,000 navigations): **948 allocs, 83.8 KiB** (targets ≤ 10,000 allocs and ≤ 1 MiB).

### Concurrency (60 s each, GOMAXPROCS=2; no cgroup)

| Scenario | requests | 200 / 503 / errors | ok p50 | p95 | p99 | max | cache hit / builds / evictions | RSS peak (HWM) |
|---|---:|---|---:|---:|---:|---:|---|---:|
| 8 clients | 4,379 | 4,379 / 0 / 0 | 7.4 ms | 527 ms | 871 ms | 1.8 s | 3,588 / 836 / 830 | 614 (632) MiB |
| 32 clients | 8,530 | 8,530 / 0 / 0 | 23.8 ms | 1,194 ms | 1,546 ms | 2.3 s | 6,557 / 801 / 788 | 615 (681) MiB |
| 32 + 4 searchers + logins | 8,134 files | 8,134 / 0 / 0 | 31.5 ms | 1,261 ms | 1,617 ms | 2.3 s | 6,340 / 758 / 745 | 683 (706) MiB |

Mixed run, other classes: search 9,897 requests all 200, p50 14 ms, p95 80 ms, p99 150 ms (300k-row table, ILIKE and name/size sorts, no tuning); login: 21 completed in 60 s with p50 179 ms (full Argon2id verification under load) — the other 248 attempts of the test's login loop were refused by the per-IP login rate limiter (HTTP 429, working as designed; this loop is not a throughput measurement). Latency tails are queueing behind the 192 MiB decode gate: a typical 250k list weighs ≈ 102 MiB, so **builds of large lists run one at a time**; p95 ≈ 1.2 s at 32 clients is wait + build, p99 1.5 s, max ≤ 3.4 s, and the 5 s queue bound / 32-waiter limit was never hit (0 busy responses).

### Cancellations and recovery (16 clients abandoning requests after 1–80 ms + 4 steady, 60 s)

~20,000 abandoned requests; the 4 steady clients saw only 200s (p50 26–30 ms). After quiescing: file descriptors 41 → 17–21, PostgreSQL connections 8 → 4–6 (pool idle connections, bounded by the pool), no growth. The cache counted ≈ 3,800 `abandoned` builds (cancelled because every waiter left; these were reported as `build_errors` in this run before `Stats.Abandoned` was split out), 0 real failures. One run in the 896 MiB series saw a single 503 and a 2.1 s shutdown (in-flight work finishing); no other anomalies.

## 7. Memory: `GOMEMLIMIT` and a real 1 GiB cgroup

Real cgroup (user scope, `MemoryMax=1G`, swap off, `CPUQuota=200%`), the three scenarios above (60 s each):

| `GOMEMLIMIT` | conc32 cgroup peak / RSS peak | mixed32 | cancel | OOM kills | failed requests |
|---|---|---|---|---:|---:|
| unset | 719 / 649 MiB | 719 / 689 MiB | 640 / 588 MiB | 0 | 0 |
| 640 MiB | 688 / 626 | 661 / 648 | 675 / 582 | 0 | 0 |
| 768 MiB | 657 / 598 | 699 / 644 | 650 / 595 | 0 | 0 |
| 896 MiB | 681 / 594 | 749 / 703 | 720 / 580 | 0 | 0 (1 × 503) |

Unconstrained process (no cgroup): VmHWM 681–706 MiB (conc32/mixed32). GC: 470–540 collections per 60 s run, total stop-the-world time 60–280 ms (no setting stood out). Heap before GC peaked at 475–640 MiB (gctrace).

**15-minute soak** (mixed32, cgroup 1 GiB, `GOMEMLIMIT=768MiB`, `soak-15m.jsonl`): 127,737 file requests, 162,865 searches (all 200, 0 errors), 49 logins; file p95 1.15 s, p99 1.44 s, max 3.4 s; search p95 75 ms. RSS oscillated 412–675 MiB with the cache churn; mean RSS 480 MiB (first half) vs 494 MiB (second half) — **no growth**; cgroup peak 738 MiB; FDs 55 → 54, PostgreSQL connections 10 → 10 (pool cap), threads 7–9; no OOM; 8,051 GCs, 1.3 s total pause; 11,800 builds, 11,785 evictions, cache ended at 134.17 MB ≤ 134.22 MB budget; clean exit 0 in 34 ms after the load.

**Findings.**
- The 1 GiB container was never OOM-killed; worst cgroup peak **749 MiB** (RSS ≤ 740 MiB). The target "peak RSS ≤ 640 MiB at 32 concurrent" was **missed** by up to ≈ 15 % in the cold-heavy mix (the 32-client file-only runs peaked at 598–681 MiB). Cause: live set ≈ cache (≤ 128 MiB) + one or two decodes in flight (≈ 100–200 MiB transient) + decoded garbage not yet collected + runtime/stacks, i.e. the budgets alone add up to ≈ 450 MiB before GC headroom.
- `GOMEMLIMIT` had only a small effect (peaks differ by tens of MiB; no trend that exceeds run-to-run noise), so the budgets, not the soft limit, determine the footprint. **Recommendation for a ≈ 1 GiB container: hard limit 1 GiB, `GOMEMLIMIT=768MiB`** (leaves ≈ 250 MiB for non-heap memory and spikes). This is a recommendation from one workload on one machine, **not proof of OOM safety**: the weight model underestimates trie-encoded long-path and flat near-limit lists (C1: up to 2.1×), and a burst of those could exceed the gate's intent (the single-build peak is the exposure, since the gate admits one such build at a time).
- The 5 s decode queue plus one-at-a-time admission mean that, with many *distinct* cold large lists requested simultaneously, some requests would receive 503 "busy"; none did in these runs because the workload's builds are short (≈ 0.2–0.5 s each).

## 8. Targets vs results

| Target | Result |
|---|---|
| Cold build, 250k files, p95 ≤ 2 s | single request 0.15–0.68 s; under 32-client contention p95 1.2–1.3 s, p99 1.4–1.7 s, max 3.4 s (the max exceeds 2 s: queueing behind the gate). Not a p95 over a production population |
| Allocations ≤ 3× uncompressed (decode) | **missed by the codec itself** (C1: 3.4–8.5× typical, up to 166× trie depth-100); documented target, not a gate; no codec change |
| Cached navigation p95 ≤ 50 ms, p99 ≤ 150 ms | **met**: p95 0.7 ms, p99 0.9 ms (1 client); under 32 clients the 60 % warm share has p50 24–33 ms overall including cold mix |
| ≤ 10,000 allocs, ≤ 1 MiB per warm request | **met**: 948 allocs, 84 KiB |
| 100 navigations → exactly 1 decode | **met** (also 105,293 requests → 1 decode) |
| 8 / 32 concurrent: no errors, bounded queue | **met**: 0 errors, 0 busy |
| Peak RSS ≤ 640 MiB at 32 concurrent | **missed** (≈ 680–740 MiB in this worst mix), within 1 GiB |
| Login responsive during expensive work | **met** (login answers in < 0.4 s p99 while searches/decodes run; binary test < 3 s with a blocked search) |

## 9. Limitations and unavailable infrastructure

- **Real container:** no Docker container memory limit was tested. Docker's data root is on the small `/` filesystem (8 GB free), so the web process ran in a **cgroup-v2 systemd user scope** (a genuine kernel memory limit with OOM killing — verified separately with a deliberate allocation test) rather than in a container; PostgreSQL ran in a Docker container with its data under `/home/herdr`. Docker/container CPU accounting may differ from `CPUQuota`.
- One machine, one run per configuration (no repeats → no confidence intervals); the load generator shares the CPU with the server and PostgreSQL (8 vCPUs, quota 200 % only on the server). Loopback network, warm OS file cache, PostgreSQL with `fsync=off`.
- The mixed workload is synthetic and cold-heavy; the synthetic name alphabet is random, so stored sizes and search selectivity are not realistic. Search was measured only on 300k rows; no 1M/5M run; no plan analysis (deferred by the owner).
- The login figure includes Argon2id at production parameters under CPU contention; the 429s are the rate limiter reacting to the test's login loop.
- Goroutine counts were verified only in-process (`TestNoGoroutineLeakAfterChurnAndClose`); externally FDs, threads and PostgreSQL connections were sampled every 0.5 s.
- A pre-existing flake: `TestGracefulShutdownLetsInFlightFinish` (server package) fails about 1 run in 40 on the base commit as well (verified with the W4 changes stashed); not caused by W4 and not fixed here.
- The weight model remains an estimate; see §7.

## 10. Commands, durations and validation

```
# Build, vet, tests (Go 1.27.0, race detector)
go build ./... && go vet ./...
go test -race -count=1 ./internal/web/... ./codec ./persistence
go test -race -count=1 -run 'TestWeb' .                       # real-binary suites (W1–W4)
go test -race -count=25 ./internal/web/filecache              # repeated concurrency tests
go test -race -count=4  ./internal/web/torrents ./internal/web/gate ./internal/web/search

# Explicit measurements (benchmark PostgreSQL: docker run postgres:17 as in §6, data under /home/herdr/w4bench)
MAGNETICO_BENCH_LARGE=1 MAGNETICO_LOAD_DSN=... MAGNETICO_LOAD_DURATION=60s MAGNETICO_LOAD_SCENARIOS=cold,warm,conc8,conc32,mixed32,cancel \
  go test -run TestWebLoad .            # baseline; ~5.2 min
# cgroup variants: + MAGNETICO_LOAD_CGROUP_MEM=1G MAGNETICO_LOAD_CPU_PERCENT=200 MAGNETICO_LOAD_GOMEMLIMIT={,640MiB,768MiB,896MiB}  (~3.1 min each)
# soak: MAGNETICO_LOAD_DURATION=900s MAGNETICO_LOAD_SCENARIOS=mixed32 ...  (15 min)
MAGNETICO_BENCH_LARGE=1 go test -run TestWarmNavigationAllocations -v ./internal/web/torrents
```

`07E-w4-measurements/campaign.sh` is the exact campaign script; `summarize.py` prints the tables; `*.jsonl` are the raw rows (machine record, environment, per-class latency, server RSS/cgroup/GC/FD/PG series).

**Validation outcomes (all run, all passed):** `go build ./...`, `go vet ./...`; `go test -race -count=1 ./internal/web/... ./codec ./persistence` (every package ok, including the persistence `FileBlob`/`GetFiles` regressions); `go test -race -run TestWeb .` (real-binary suites ok); `-race -count=25 ./internal/web/filecache` ok; `-race -count=4` of `torrents`, `gate`, `search` ok. **Not run:** the full `go test ./...`, `dht`, `crawler`, `e2e`; the known flaky `TestGracefulShutdownLetsInFlightFinish` passed in this run but is flaky at base (§9).

## 11. Cleanup

Test-created resources: Docker container `magnetico-w4-bench-pg` (label `magnetico.w4bench=1`) — stopped and removed; its data directory `/home/herdr/w4bench` (1.4 GB) — deleted; systemd scopes `magnetico-w4-bench-*` — none remaining; no load generators or test PostgreSQL left running; `/` free space unchanged (8.0 GB), `/home` 108 GB free. **Left over, not created by this campaign:** 7 anonymous Docker volumes created on 22–23 Nov earlier than the campaign (probably from earlier test-cluster runs); they are not identifiable as test-owned and were not touched.

## 12. Remaining issues / owner decisions

1. **Decode concurrency vs the 192 MiB gate.** With the 36 B/file allowance a typical 250k list weighs ≈ 102 MiB, so large builds serialize (p95 ≈ 1.2 s at 32 clients). Options: accept (no failures observed), raise the gate towards 256 MiB at the cost of up to ≈ 100 MiB more peak memory, or refine the weight (peek-based exact `P`, codec work).
2. **RSS target 640 MiB missed** (≈ 740 MiB worst case, 1 GiB limit OK). Accept, or lower the cache/gate.
3. **Allocation target (≤ 3×)** stays a documented miss (codec).
4. 1M/5M search measurements and index tuning remain deferred to after 0.8.0.
5. The pre-existing flaky shutdown test (§9) needs a separate fix.
