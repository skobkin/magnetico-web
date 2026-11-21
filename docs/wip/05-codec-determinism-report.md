# Task 05 — Pooled Brotli encoder determinism: implementation report

Branch `fix/05-codec-determinism`, based on `refactor/0.8.0-unify-and-new-storage` at `2f01c8e5`.
Scope: `codec/` and this report. `go.mod`/`go.sum` unchanged. Toolchain: go1.27.4 linux/amd64,
`github.com/andybalholm/brotli v1.2.6`, 16 CPUs.

## 1. Summary

* **Defect reproduced.** At Brotli q9 (`ProfileCompact`), a reused writer produces different bytes than a
  fresh writer whenever the payload crosses the 1 MiB size class relative to what the writer compressed
  first (small → large and large → small). Same-class reuse never diverged.
* **Root cause (confirmed experimentally).** `brotli.Writer.Reset()` clears the stream `size_hint` but
  keeps the hasher object. At quality ≤ 9 the hasher type is chosen once, from `size_hint`
  (`chooseHasher`: h5 below 1 MiB, h6 from `1<<20`), so a reused writer keeps the hasher of its *first*
  payload. This is a compressor-library behaviour, not a bug in the surrounding codec. My initial
  hypothesis (stale ring-buffer bytes) was wrong and is discarded.
* **Fix.** Reuse the pooled Brotli writer only for payloads `< 1<<18` bytes (one input block, 4× below the
  hasher threshold). Larger payloads get a fresh writer. Output of every encode is now independent of
  history, and equal to what a fresh writer / package-level `Encode()` produces.
* **Not q11.** Maximum (q11) never diverged in any history I tried: it always uses h10 regardless of size.
  q11 tests are kept as controls; the task's expectation of a q11 reproduction was **not** confirmed.
* **A simpler fix was tried and rejected on measurements**: a fresh writer per compression is correct but
  made small-list encoding ~100× slower and made the 04D fault tests ~16× slower (see §5.3).
* **Cost of the gate (review follow-up, §5.4–5.5).** The reuse boundary is a step: at q9 a payload just
  over 256 KiB compresses in 2.1× the time of one just under it (+~46 MB allocated); q11 latency is
  flat, allocation +46 MB. Under 120 s of four-worker load memory stabilises and peak RSS is not higher
  than baseline; q9 throughput is −10 %, GC cycles 5×.

## 2. Reproduction

`magneticobench/internal/realbench/determinism_probe_test.go` is not available locally, so the reproducer
was rebuilt from scratch with deterministic, non-random file lists (random bytes would be stored raw and
hide the effect). Fixtures in `codec/determinism_test.go` (`detFixture`):

| Input | Shape | Prefix payload | Trie payload |
|---|---|---|---|
| `large-media` | 52 000 deep media paths | ≥ 1 MiB | ≥ 1 MiB |
| `large-flat` | 70 000 flat high-entropy names | ≥ 1 MiB | ≥ 1 MiB |
| `medium-media` | 12 000 media paths | 2^18 … 1 MiB | 2^18 … 1 MiB |
| `small-shallow` | 800 shallow paths | < 1 MiB | < 1 MiB |
| `tiny` | 2 files | tiny | – |

`detInputs`/`detMedium` assert these size classes, so a fixture edit cannot silently stop crossing the
threshold. Minimal pair: `small-shallow → large-media` (compressor level: same state compresses the small
prefix payload, then the large one).

Compressor-level exploration (exact-size truncations of real payloads, ordered pairs on one reused state,
vs. a fresh state; before any fix):

| q | transition | prefix | trie |
|---|---|---|---|
| 9 | small → small | 0 / 100 | 0 / 100 |
| 9 | small → large (≥ 1 MiB) | **50 / 50** | **60 / 60** |
| 9 | large → small | **45 / 50** | **60 / 60** |
| 9 | large → large | 0 / 25 | 0 / 36 |
| 11 | all four transitions | 0 / 64 | 0 / 64 |

Public-encoder level (`Encoder` with its single compressState pinned, Compact), before the fix:

* `[small-shallow, large-media]` step 1: blob 178 069 bytes vs 178 504 fresh.
* `[tiny, large-flat, small-shallow]` step 1: blob 600 785 bytes vs 611 523 fresh.

Evidence files (session scratch, quoted here): `before-fix.txt` — Compact fails at compressor and
Encoder level; Balanced, Maximum and Zstd controls pass; `after-fix` runs pass.

**sync.Pool trap.** The first Encoder-level test passed *vacuously*: under heavy parallel allocation the GC
cleared the `sync.Pool`, so the "reused" Encoder silently got a fresh state. The test now pins one
`compressState` into the Encoder's pool `New` (`newPinnedEncoder`), making reuse unconditional.

## 3. Root cause

Evidence chain:

1. Divergence only across the 1 MiB class (table above); `size_hint` is the first `Write`'s length and
   `chooseHasher` switches on `size_hint >= 1<<20` (`quality.go:148`).
2. `encoderInitState` (called by `Reset`) resets `size_hint` via `encoderInitParams` but only does
   `hasher_.Common().is_prepared_ = false`; `hasherSetup` calls `chooseHasher` only when the handle is nil.
3. **Experiment:** a scratch copy of brotli v1.2.6 with `s.hasher_ = nil` in `encoderInitState` makes the
   whole regression suite pass (through a throwaway `-modfile`, `go.mod` untouched).
4. **Upgrade is not a fix:** unmodified v1.3.0 diverges identically.

Ruled out: ring-buffer stale bytes (disproved by 3), `compressState.buf` (reset each call; payload is
copied before reuse), Zstd reuse (control passes), surrounding candidate/selection logic (reference
`Encode()` builds a fresh state per candidate and is itself history-free).

## 4. Fix and alternatives

Implemented (`codec/compress.go`): `compressState.benc` is reused (`Reset`) only when
`len(payload) < brotliReuseMaxPayload (1<<18)`; otherwise a fresh writer is built and `benc` is untouched.
Wire format, serializer IDs, profiles, quality levels, candidate sets, tie-breaking, limits and the public
`Encoder` API are unchanged.

Why this is safe, per transition (all permitted reuse is among payloads that are a **single block**):

* One block means no ring-buffer wrap, no stitching and no cross-block state inside a stream.
* All streams on the pooled writer pick the same hasher (size hint always < 1 MiB), so the retained
  hasher object is the one a fresh writer would have chosen.
* Remaining cross-stream state (hasher tables, command/storage buffers) is re-initialised by `Prepare` /
  `encoderInitState`; h5/h6 partial `Prepare` only clears buckets that the current input can query, as in
  upstream's C design.
* small → large, large → small: large payloads never touch the pooled writer.
* prefix ↔ trie: the serializer does not enter the writer's state; covered by tests.

This argument rests on undocumented library internals, so I did not rely on it alone.
**Evidence for the gate:**

* Seeded random-history differential (`TestBrotliReuseRandomHistories`): one shared state is pushed through
  shuffled chains of payloads (sizes `1 … 1 MiB`, prefix and trie, several shapes) and every output must
  equal a fresh state's output. Default sweep is CI-sized; `CODEC_REUSE_SWEEP=full` runs the wide sweep
  (q9: 4 shapes × 12 sizes × 4 chains; q11: 2 shapes × 5 sizes × 3 chains) — passes in ~12 s.
* **Mutation checks** (test sizes are literals so they do not move with the gate): gate `1<<30` (old
  behaviour) and `1<<21` both **fail** — e.g. `style0/ser2/1048576 after style1/ser4/1: 147595 bytes,
  fresh state gives 147108`; gate `1<<18` and, informationally, `1<<20` **pass**.
* Curated histories (`TestCompressStateHistoryIndependence`, `TestEncoderHistoryIndependence`,
  `TestEncoderConcurrentMixedInputs`) cover A→B, B→A, A→B→A, B→A→B, small↔large, prefix↔trie, both
  profiles, Balanced/Zstd control, and 2 workers × 4 goroutines with mixed inputs; every blob is decoded
  and compared to the canonical input; blob bytes **and** `Selection` are compared.

Alternatives considered:

| Alternative | Outcome |
|---|---|
| Fresh writer per compression (commit `801681b2`) | Correct, but 100× slower for tiny lists and 04D tests ~16× slower → rejected (§5.3). |
| Two pooled writers, one per hasher class | Safe in principle, relies on more internals; not needed. |
| Gate at `1<<20` (the hasher threshold itself) | Passes the sweep, but leaves no margin against a changed threshold. |
| Upgrade brotli to v1.3.0 | Same defect → no fix. |
| Patch/fork brotli to drop the hasher in `Reset` | Fixes it (confirmed), but adds a fork; an upstream report may be worthwhile (not filed). |

## 5. Measurements

Same machine, toolchain and benchmark file (`codec/bench_reuse_test.go`, copied untracked into a clean
baseline worktree at `2f01c8e5`), baseline / fresh-per-call / final run **sequentially**.
`-benchmem`; 10 and 1000 files: 6 samples, 50 000 files: 3 iterations × 3 samples. `benchstat` is not
installed, so these are means.

### 5.1 Long-lived single-worker Encoder (`BenchmarkEncoderReuse`) — ms/op, MiB/op

| profile / files | baseline | fresh-per-call | **final (gated)** |
|---|---|---|---|
| compact / 10 | 0.073 ms, 0.02 | 7.73 ms, 64.19 | **0.069 ms, 0.02** |
| compact / 1000 | 3.86 ms, 0.57 | 15.19 ms, 65.18 | **4.04 ms, 0.58** |
| compact / 50 000 | 242.3 ms, 24.2 | 260.4 ms, 113.9 | **260.6 ms, 114.4** |
| maximum / 10 | 1.87 ms, 0.47 | 10.81 ms, 65.53 | **1.90 ms, 0.47** |
| maximum / 1000 | 63.0 ms*, 4.81 | 68.3 ms, 69.57 | **42.6 ms, 4.42** |
| maximum / 50 000 | 2413 ms, 134.6 | 2427 ms, 226.4 | **2481 ms, 226.4** |

\* High run-to-run variance for this row (baseline measured 40.8 ms in the first run); treat as noise.
One-shot `Encode()` (the reference, unchanged code path) is within noise across all three builds
(e.g. compact/10: 7.63 / 7.75 / 7.80 ms).

### 5.2 Four workers in parallel (`BenchmarkEncoderReuseParallel`)

| profile / files | baseline ms, MiB/op | final ms, MiB/op | retained heap after GC (MiB), baseline → final | GC cycles/op, baseline → final |
|---|---|---|---|---|
| compact / 1000 | 1.25, 1.33 | 1.27, 1.27 | 202 → 202 | 0.01 → 0.01 |
| compact / 50 000 | 104.4, 67.1 | 112.6, 115.6 | 144 → 7.6 | 0.67 → 1.89 |
| maximum / 1000 | 12.6, 6.96 | 12.8, 6.91 | 136 → 125 | 0.05 → 0.05 |
| maximum / 50 000 | 878.8, 176.9 | 906.5, 226.4 | 147 → 9.4 | 1.22 → 2.44 |

Peak RSS (dedicated process, 4 workers, 8 iterations of a 50 000-file list; **superseded by the 120 s sustained run in §5.5, which showed no systematic increase — treat these short-run numbers as single noisy samples**):

| profile | baseline | fresh-per-call | final |
|---|---|---|---|
| compact | 332 MB | 400 MB | **417 MB (+26 %)** |
| maximum | 415 MB | 495 MB | **459 MB (+11 %)** |

Memory notes (the worker semaphore bounds *active* compressions, not writers awaiting GC):
small lists keep the old pooled behaviour (one retained writer per state, ~50 MiB, i.e. ~200 MiB for 4
workers); payloads ≥ 2^18 now allocate a transient writer (~32 MiB hasher tables plus ring buffer, up
to ~226 MiB/op for a 50 000-file list at q11), so allocation per op, GC cycles and peak RSS go up for
large lists while retained heap after GC goes down. Latency for 50 000-file lists is +3–8 %.

### 5.3 Why the simple fix was not kept (04D fault tests)

Root package (`go test -count=1 -p 2 -parallel 4 .`, run alone):

| build | result | `TestFaultImportKilledBySignalResumes` / `LostCommitAck` / `TargetConnectionLoss` |
|---|---|---|
| baseline `2f01c8e5` | ok, 176 s | 18 s / 31 s / 25 s |
| fresh-per-call | **timed out at 900 s** | 289 s / 289 s / 297 s |
| final | ok, 171 s | 24 s / 20 s / 17 s |

A first whole-repo run on the fresh-per-call build had already timed out in this package. I did not run
the baseline inside a whole-repo run, so that specific timeout is attributed by the isolated runs above.

### 5.4 The 256 KiB reuse boundary (`BenchmarkBrotliReuseBoundary`, `BenchmarkEncoderBoundary`)

`codec/bench_boundary_test.go` builds deterministic file lists (style-0 media tree) whose **real**
prefix-delta serialization straddles `1<<18` = 262 144 bytes: `below` is the largest list under the gate,
`at` the next list (one file more), plus half a block, +25 %, two blocks and 1 MiB. Actual payload sizes
are reported as the `payload-bytes` metric. Same harness, machine and toolchain for baseline
(`2f01c8e5`, every size reuses the writer) and fixed tree; sequential runs, compact 6 samples,
maximum 3×3 iterations, means. Compressor level = one warmed `compressState`, Brotli only.

| Profile | Case | Payload B | Baseline ms | Fixed ms | Baseline B/op | Fixed B/op | Fixed allocs/op |
|---|---|---:|---:|---:|---:|---:|---:|
| q9  | half   | 131 083 | 5.25 | 5.14 | 111 | 672 | 0 |
| q9  | below  | 262 128 | 11.44 | 11.42 | 2 847 | 2 847 | 0 |
| q9  | **at** | 262 153 | 11.45 | **23.76** | 401 | **46 114 804** | 23 |
| q9  | above  | 327 693 | 15.37 | 27.79 | 4 505 | 46 271 698 | 23 |
| q9  | double | 524 303 | 28.16 | 38.60 | 1 297 | 46 759 244 | 23 |
| q9  | 1 MiB  | 1 048 591 | 70.8 | 82.0 | 5.1 M | 52.7 M | 204 |
| q11 | half   | 131 083 | 211.7 | 204.3 | 10.7 M | 11.0 M | 61 |
| q11 | below  | 262 128 | 468.9 | 451.7 | 20.4 M | 20.4 M | 67 |
| q11 | **at** | 262 153 | 481.5 | 457.6 | 21.1 M | **67.0 M** | 79 |
| q11 | above  | 327 693 | 626.1 | 595.0 | 25.4 M | 73.7 M | 95 |
| q11 | double | 524 303 | 1067.9 | 1014.2 | 41.7 M | 88.7 M | 109 |
| q11 | 1 MiB  | 1 048 591 | 2244 | 2220 | 78.1 M | 127.1 M | 120 |

Public `Encoder` (1 worker, every candidate of the profile, selection included; the candidate payloads
differ from the prefix-delta size above, so one candidate can cross the gate earlier than the label
suggests):

| Profile | Case | Baseline ms | Fixed ms | Baseline MiB/op | Fixed MiB/op |
|---|---|---:|---:|---:|---:|
| q9  | half   | 21.0 | 20.8 | 4.9 | 4.9 |
| q9  | below  | 45.2 | 52.3 (+16 %) | 9.3 | 53.4 |
| q9  | at     | 45.9 | 71.9 (+57 %) | 9.4 | 97.6 |
| q9  | above  | 59.1 | 86.7 (+47 %) | 11.6 | 99.9 |
| q9  | double | 107.4 | 131.0 (+22 %) | 20.0 | 109.2 |
| q11 | half   | 489.7 | 486.7 | 29.6 | 29.9 |
| q11 | below  | 1114.9 | 1119.0 | 56.3 | 100.8 |
| q11 | at     | 1128.0 | 1130.2 | 56.3 | 145.3 |
| q11 | above  | 1505.4 | 1513.4 | 70.2 | 159.2 |
| q11 | double | 2594.2 | 2609.3 | 111.7 | 201.8 |

**Discontinuities at the gate (significant, documented):**

* **q9 latency doubles at the gate**: 11.4 → 23.8 ms (+108 %) between 262 128 and 262 153 bytes. This is
  the cost of zeroing and populating a fresh ~46 MB hasher/ring set that a reused writer avoids. It is a
  step, not a slope: the baseline grows smoothly with size, the fixed tree adds a roughly constant
  ~12 ms to every payload ≥ 256 KiB, which fades relatively as payloads grow (+16 % at 1 MiB, +37 % at
  2 blocks).
* **Allocation steps by ~46 MB per compression at q9 and by ~46 MB (3.2×) at q11** at the gate. At q11
  the *latency* is unchanged (compression time dominates: ±5 %, within noise and slightly better).
* The Encoder-level step is larger than the compressor-level step because the encode runs more than one
  Brotli compression per call (one per serializer candidate) and each payload at or above the gate
  allocates its own writer. At q9 an encode of a ~256 KiB list costs +16…57 % and ~45–90 MiB more
  allocation per call.
* Below the gate nothing changes: allocations and latency are identical to the baseline (within noise).

### 5.5 Sustained four-worker behaviour (`BenchmarkEncoderStress`, opt-in)

`codec/bench_stress_test.go` is skipped unless `CODEC_STRESS=compact|maximum` is set, so it is never part
of the default suite. Four goroutines call one 4-worker `Encoder` in a loop on a 50 000-file list
(`sampleFiles`; payloads well above the gate) for `CODEC_STRESS_SECONDS` (120 here); a sampler records
`VmRSS`, heap and GC counters every second; `VmHWM` gives peak RSS. Run in its own process, one iteration:

```
CODEC_STRESS=compact CODEC_STRESS_SECONDS=120 \
  go test -c -o codec.test ./codec && ./codec.test -test.run '^$' \
  -test.bench '^BenchmarkEncoderStress$' -test.benchtime 1x -test.v
```

Baseline (`2f01c8e5` plus the bench file) and fixed binaries were run sequentially, one 120 s run each
(a single sample per cell: treat differences of ~10 % as within run-to-run noise), default `GOGC`,
16 CPUs.

| Profile | Build | ops/s | peak RSS MiB | RSS max, 2nd quarter | RSS max, last quarter | max HeapSys MiB | GC/s | GC pause total | alloc MiB/op |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| q9  | baseline | 13.76 | 638.8 | 605.4 | 629.5 | 655 | 1.7 | 29 ms | 28 |
| q9  | fixed    | 12.35 (−10 %) | 442.0 | 396.0 | 428.2 | 467 | 9.1 | 168 ms | 115 |
| q11 | baseline | 1.385 | 585.3 | 542.8 | 521.3 | 663 | 0.8 | 12 ms | 140 |
| q11 | fixed    | 1.378 (−0.5 %) | 550.9 | 512.9 | 521.1 | 603 | 1.5 | 24 ms | 226 |

Findings:

* **Memory stabilises after warm-up** on both builds: the last-quarter RSS maximum is within ~7 % of the
  second-quarter maximum, HeapSys is flat after the first ~30 s, and there is no upward trend in the
  1-second samples. Repeated transient writers do not leak and do not ratchet RSS up.
* **GC pressure rises as predicted**: allocation volume per Encode is 4× at q9 (28 → 115 MiB) and
  1.6× at q11, GC cycles per second are 5× at q9 and ~2× at q11. Total pause stayed ≤ 170 ms over
  120 s (≈0.14 %), so the cost is CPU in the collector and allocator, not pauses.
* **Throughput**: q9 −10 % under four-way load (consistent with the +3–8 % single-worker figure plus
  collector contention); q11 unchanged.
* **Peak RSS did not increase in this sustained run** (q9 −31 %, q11 −6 %). This contradicts the
  earlier short-run figure (+26 % / +11 %, §5.2) in direction. Peak RSS is governed by GC pacing and
  where in the cycle the process is sampled; the baseline's low GC rate lets the heap grow to its goal
  before collecting, while the fixed build collects more often. The correct reading is that **peak RSS is
  not systematically higher** for the fixed build, with the earlier number now treated as one unlucky
  sample, not a trend. §5.2's RSS figures should be read with that caveat.

## 6. Blob compatibility and candidate selection

Scratch comparison (not committed) over 8 inputs × 3 profiles and all 64 ordered pairs of a pinned,
reused Compact Encoder, baseline vs. final:

* The 21 fresh `Encode()` results (size, `Selection`, SHA-256) are **byte-identical** between baseline and
  final: the fix changes no blob a fresh encoder produced; wire format v1 and golden vectors untouched.
* Every encoded blob decodes to the canonical input (`decodeEqual` true for all, both trees). Inputs from
  `sampleFiles` had to be canonicalised first: `Decode` returns canonical order and `sampleFiles` is not
  sorted (an initial run reported false mismatches for exactly that reason).
* Baseline pooled encoder: **20 of 64** histories differed from the fresh result, all "small/medium/tiny
  first, then a ≥ 1 MiB input"; **0** changed the selected serializer/compressor (the corpus' winners were
  unambiguous). Final: 0 of 64 differ.
* The old divergent blobs were slightly **smaller** (178 069 vs 178 504; 600 785 vs 611 523 bytes) because
  the stale small-class hasher (h5) happened to compress these large inputs better than the
  size-appropriate h6. A fresh encoder has always produced the larger form, which is now the only one.
  A selection flip is theoretically possible when two candidates are within a few hundred bytes, but none
  occurred here.
* Previously written blobs remain readable (decode path unchanged; blobs produced by the old pooled
  encoder are valid Brotli streams). No re-encoding is required.

## 7. Verification (final tree, after the review follow-up)

Run in `../magnetico-web-05-codec-determinism`; all outcomes as observed:

| Command | Result |
|---|---|
| `CODEC_REUSE_SWEEP=full go test -count=1 -v -run TestBrotliReuseRandomHistories ./codec/` | ok, 10.4 s (compact 8.9 s, maximum 10.1 s; both subtests ran, none skipped) |
| `go test -race -count=1 ./codec/...` | ok, 89.5 s (unchanged baseline 16 s) |
| `go test -count=1 -p 2 -parallel 4 ./...` | all 22 packages ok (root 320 s and `internal/migration` 164 s under `-p 2` load; codec 8.3 s) |
| `go build ./...` | ok |
| `go vet ./...` | ok |
| `gofmt -l .` | no output |
| `golangci-lint run` | no findings (exit 0) |

No environmental failures. The new `BenchmarkEncoderStress` is skipped unless `CODEC_STRESS` is set, so
it does not affect `go test`; `-short` skips the random-history sweep.

Existing golden vectors, compressed decode fixtures, limit and corruption tests pass unchanged.


## 8. Remaining limitations and risks

* **The gate depends on undocumented brotli internals** (hasher choice by size hint, single-block reuse
  safety). It is validated for v1.2.6 only. After any brotli upgrade or gate change run
  `CODEC_REUSE_SWEEP=full go test -run TestBrotliReuseRandomHistories ./codec/` and the full codec suite.
* q11 transitions above 2^18 are not exercised by default tests for cost reasons (they cannot reach the
  pooled writer by construction; a one-off probe found no q11 divergence).
* **Large lists (≥ 256 KiB payload) allocate a fresh writer per compression.** Measured cost
  (§5.4–5.5): q9 compression latency doubles at the gate (11.4 → 23.8 ms, a step of ~12 ms and ~46 MB
  of allocation, shrinking relatively as payloads grow), q11 latency is unchanged but allocation per
  call is +46 MB; under four-way sustained load q9 throughput is −10 %, allocation volume 4× and GC
  cycles 5× (pause total still ≈0.14 %), q11 throughput unchanged. **Peak RSS was not higher in the
  120 s sustained run** and memory stabilised after warm-up, which supersedes the earlier short-run
  "+26 % RSS" claim. Remaining operational risk: more collector CPU on hosts that encode many ≥ 256 KiB
  lists concurrently, and RSS that depends on GC pacing (a `GOMEMLIMIT` or a lower `GOGC` bounds it).
  The average torrent is far below the gate and is unaffected. Only one 120 s sample per cell was
  taken; ~10 % differences are within noise.
* Test time grew: +5–7 s normally, ~+85 s under `-race` (the new large-payload tests). `-race` CI budgets
  may need review.
* Fresh h6 output for ≥ 1 MiB payloads is up to ~1.8 % larger than the old (accidental) h5 output on this
  corpus. Not a correctness issue; a future optimisation could pin the hasher via a library change.
* The base branch has since advanced to `eb8d7a12` (unrelated 04D test/doc commits); `codec/` and `go.mod`
  are identical there, so this branch should merge cleanly.

## 9. Commit structure

The history was rewritten after review so that every commit builds and passes its tests (`git bisect`
safe) and the rejected intermediate implementation is not in it. The fresh-writer-per-call attempt
(+100× small-list latency, 04D fault tests at ~290 s each and a 900 s package timeout) is documented in
§4 and §5.3 only. The earlier, now-obsolete history is on the PR's previous head `37135e5e`.

| Commit | Summary |
|---|---|
| 1 | `fix(codec): reuse the Brotli writer only below one input block` — `compress.go` gate, `encoder.go` doc, regression tests (`determinism_test.go`, `determinism_reuse_test.go`); tests fail without the gate and pass with it |
| 2 | `test(codec): encoder reuse, 256 KiB boundary and sustained-load benchmarks` — `bench_reuse_test.go`, `bench_boundary_test.go`, opt-in `bench_stress_test.go` |
| 3 | `docs: 05 report` — this document |
