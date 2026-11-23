# Task 07E / W4 — C1 report: file-index prototype, memory measurements and independent work

**Status: STOPPED AT C1.** Nothing is pushed. The production cache, the single-flight coordinator, the W4 HTTP handlers/templates and everything after C1 are **not** started. Branch `refactor/0.8.0-unify-and-new-storage`, starting from the S1 checkpoint `d34e260e`.

All measurements below were taken on **synthetic data only** (`internal/web/synth`), in a disposable process, with no database, no network and no DHT traffic. Raw results are in [`07E-c1-measurements/`](07E-c1-measurements/README.md).

## 1. Summary and recommendation

| Question | Answer (evidence in §§4–6) |
|---|---|
| Which index representation? | **Recommend A, the arena index**, to be retained in the cache. It retains 1.1–10× less memory than B (7–8 MiB vs 69–76 MiB for the long-prefix shapes; 17.7 vs 29.9 MiB for a typical 250k-file torrent; nearly equal, 64.9 vs 68.9 MiB, when ≈63 MB of unshared names sit in one directory). Its price is 8–66 MiB of extra *transient* allocation and +9–112 ms during the build. Warm queries are microseconds either way. |
| Cold-build time (target p95 ≤ 2 s) | Met with margin: median 99–242 ms for 250k files (slowest of 5 isolated runs: 494 ms) at GOMAXPROCS=2. Not a p95 over a request population (see limitations). |
| Allocations ≤ 3× uncompressed | **Not met, by the codec, not the index.** Decode alone allocates 3.4–8.5× the declared payload for prefix-delta blobs of typical shapes (21.7× for the deep trie), ≈30× for long-prefix prefix-delta blobs and 166× (715 MiB) for a depth-100 trie blob. The index adds 2–9 MiB (B) or 8–22 MiB (A; 66 MiB for flat near-limit names). Nothing was changed. |
| Is the conservative weight model a bound? | **No.** It matches typical 250k torrents (observed/W0 ≈ 0.9–1.0) but **underestimates by 1.5–2.6×** (peak heap/RSS vs `W0`) for flat near-limit names (arena copy) and for trie-encoded deep/long paths; and it **overestimates by 17–125×** for small torrents (≥ 1,000 files) because `P` is assumed to be 64 MiB. See §5. |
| 256 MiB decode gate / 128 MiB cache | Measured peaks of the *model-admitted* concurrency stay below 256 MiB for typical, deep and long-name torrents, but **exceed it** for long-prefix shapes at the admitted K=3 (275–300 MiB prefix-delta, 429–439 MiB trie); flat near-limit lists are admitted alone (214 MiB heap / 247 MiB RSS) and would reach 409–512 MiB at K=2. The 128 MiB cache holds the worst measured A entry (65 MiB) once, and typical entries 7×. See §6. |
| Simultaneous cold builds admitted by the model | 1 (flat near-limit) / 2 (typical, deep, long-names) / 3 (long-prefix) for 250k-file torrents; 3–4 for 1k–10k-file torrents; 40 for 100 files. |

Decisions requested at C1 are in §10.

## 2. What was delivered

Commits (local, in order, all after `d34e260e`):

| Commit | Content |
|---|---|
| `7e1bb2dc` | `fix(persistence)`: `FileBlob` separates an oversized unchanged blob (non-retryable) from a changed/missing one (retryable) |
| `c1e1d446` | `feat(web)`: lossless path presentation and directory link encoding (`internal/web/pathfmt`) |
| `f4b8329c` | `feat(web)`: directory index prototype, both variants (`internal/web/filetree`) |
| `26ce7bfa` | `test(web)`: synthetic corpus (`synth`), benchmark toolkit (`benchkit`), codec test-only blob hooks, cold-build harness |
| `04d884a5` | `test(web)`: `flatmax` synthetic shape |
| (this commit) | this report, the raw measurements and the README/S1 notes |

**Index prototype (`internal/web/filetree`).** Input is the canonical `[]codec.File`. Phase 1 (**scan**) walks the list once with a stack of open directories, recording directories, per-file parents and checked recursive aggregates; it does not fill children. Phase 2 (**pack**) prefix-sums per-directory child counts, writes each directory's subdirectory ids (sorted by name bytes) and then its files (canonical order) into one contiguous block of a shared `kids []uint32`. Both variants share that code and differ only in what they retain:

- **A, `Arena`**: one string arena holding directory and file *base names* (copied), 16-byte file records, 32-byte directory records; the decoded list can be dropped.
- **B, `Ref`**: the decoded `[]File` is kept; the index only holds offsets (12-byte name references + the shared 24-byte directory core).

Preserved exactly (tests with a map-based reference model on random path sets, a mutation check on sibling sorting, a 100k-child directory, and `-race` concurrent readers): original path bytes; duplicate files incl. identical `(path, size)` (separate positions, `Dup` flag, correct across page boundaries); empty components and `/`-directory vs root; leading separators; literal `.`/`..`; arbitrary invalid UTF-8 and NUL; file order = canonical position; checked aggregates (overflow → `ErrOverflow`); non-canonical input rejected. No normalisation, deduplication, lossy conversion or per-node heap object. Cancellation: `BuildArena/BuildRef` check the context every 4096 files.

**Independent work:** persistence correction (§7), path presentation (§8), benchmark infrastructure (§9).

## 3. Methodology

- **Data:** `synth.Corpus()` — 12 deterministic seeded datasets (table in §4). All are canonical and inside `codec.DefaultLimits()`. Blobs are encoded under six *forced* serializer × compressor pairs (flat/prefix-delta/trie × none/Zstd/Brotli q9) by a test-only codec hook (`codec/export_test.go`, `TestDumpBenchBlobs`), so the same list can be compared across serializers; `Encode`'s own selection is not used. Pairs that violate limits or that the trie cannot represent are skipped (recorded in `blob-meta.json`).
- **Isolation:** every measurement is a separate child process (the test binary re-executed). Child sequence: read blob; *peak run* = `codec.Decode` + index build back to back with a heap/RSS probe (2 ms sampling) and the kernel's `VmHWM` (reset beforehand through `clear_refs`); then a *phase run* measures decode and build separately (forced GC before each) for time, allocated bytes and malloc counts; finally the live heap after dropping what the variant lets go is the **retained** size. Peak heap = max sampled `HeapAlloc` minus the pre-load baseline (includes the stored blob). RSS figures add the stored blob back (¹) because the RSS baseline is taken after it was read.
- **Repetitions:** 5 runs per cell; tables show the median-by-cold-time run and, for cold time, `[min–max]`. Peaks are the max of the repetitions where marked.
- **Machine:** AMD Ryzen 9 7950X3D (8 logical CPUs visible), 32 GiB, Linux 6.12, Go 1.27.0, `GOGC=100`, no cgroup limits. The 2 vCPU / 1 GiB target was approximated **only** by `GOMAXPROCS=2`; there is no memory limit, so RSS could overshoot what a 1 GiB container allows. GOMAXPROCS=8 changed cold times by up to ≈ ±30% and peak heap by up to ≈ 15% with no consistent direction (the work is single-threaded; `gomaxprocs8-subset.jsonl`, 3 runs per cell).
- **Explicit commands only**; nothing expensive runs in `go test ./...`: see §11.

## 4. Results — index variants

Prefix-delta + Brotli q9 blobs, GOMAXPROCS=2. "retained" = live heap after the build with the variant's own ownership rules (A drops the decoded list; B keeps it).

| dataset | files | dirs | path MiB | **A** retained | **B** retained | A / B index build ms | A / B build alloc MiB | A cold ms med [min–max] | B cold ms med [min–max] | decode-only cold ms | A / B / decode-only peak heap MiB |
|---|---:|---:|---:|---:|---:|---|---|---|---|---:|---|
| typical-100 | 100 | 5 | 0.0 | 0.0 | 0.0 | 0 / 0 | 0.0 / 0.0 | 0 [0–0] | 0 [0–0] | 0 | 0 / 0 / 0 |
| typical-1k | 1000 | 41 | 0.1 | 0.1 | 0.1 | 0 / 0 | 0.1 / 0.0 | 1 [1–1] | 1 [0–1] | 1 | 0 / 0 / 0 |
| typical-10k | 10000 | 401 | 0.8 | 0.7 | 1.2 | 1 / 0 | 0.8 / 0.1 | 6 [6–7] | 5 [5–7] | 6 | 4 / 4 / 4 |
| typical-250k | 250000 | 10001 | 19.8 | 17.7 | 29.9 | 34 / 13 | 20.1 / 3.8 | 185 [177–198] | 165 [138–178] | 139 | 86 / 70 / 66 |
| flat-250k | 250000 | 1 | 12.4 | 17.2 | 22.1 | 18 / 4 | 18.1 / 1.9 | 124 [122–129] | 110 [106–114] | 107 | 73 / 59 / 57 |
| deep-250k | 250000 | 31246 | 32.6 | 15.3 | 43.7 | 48 / 28 | 22.1 / 9.1 | 139 [135–158] | 124 [119–136] | 107 | 80 / 77 / 79 |
| deepchain-250k | 250000 | 2496 | 62.5 | 7.8 | 75.7 | 182 / 161 | 9.0 / 2.3 | 242 [227–494] | 219 [216–248] | 52 | 92 / 95 / 92 |
| wide-250k | 250000 | 2002 | 16.9 | 17.3 | 26.6 | 26 / 10 | 18.4 / 2.2 | 184 [167–207] | 140 [130–152] | 129 | 78 / 66 / 63 |
| longpath-250k | 250000 | 121 | 60.6 | 6.9 | 68.9 | 48 / 39 | 7.9 / 1.9 | 99 [89–117] | 96 [92–98] | 46 | 87 / 85 / 83 |
| flatmax-250k | 250000 | 1 | 60.1 | 64.9 | 68.9 | 140 / 11 | 65.8 / 1.9 | 209 [180–220] | 67 [64–73] | 60 | 208 / 176 / 172 |
| adversarial-50k | 50000 | 70104 | 0.6 | 4.0 | 5.1 | 10 / 6 | 16.3 / 15.4 | 17 [16–19] | 15 [14–18] | 10 | 9 / 10 / 6 |
| longname-1500 | 1500 | 52 | 28.7 | 28.8 | 30.9 | 23 / 2 | 28.8 / 0.0 | 222 [176–508] | 164 [163–473] | 163 | 119 / 94 / 95 |

Reading the table:

- **Retained memory** is the deciding column. A is ≈ `Σ base names + 16·files + 4·(files+dirs) + 32·dirs`; B is ≈ `Σ full paths + 24·files + size-class rounding + index`. They are close only when the names themselves dominate (flat/long-name shapes) and A is up to **10× smaller** where directory prefixes are long and shared (longpath 6.9 vs 68.9 MiB, deepchain 7.8 vs 75.7 MiB, deep 15.3 vs 43.7 MiB). `Index.Bytes()` matches the measured retained size within 0.1 MiB for A and within ~5 MiB for B (B's figure excludes allocator size-class rounding of the individually allocated path strings).
- **Build cost.** B's build only writes references (2–9 MiB, 4–39 ms for typical shapes); A additionally copies names (8–66 MiB allocated, +9–112 ms). The two long-chain shapes cost ~160–180 ms *for both*: the scan compares each file's components against the open-directory stack, O(files × depth); a longest-common-prefix shortcut against the previous path would remove most of it but was not needed (cold total ≤ 0.25 s) and was not implemented.
- **Cold total** is dominated by decode. A is 10–35% slower than B for typical shapes (e.g. 185 vs 165 ms) and ≈3× slower for flatmax (209 vs 67 ms, the 63 MB name copy); medians for 250k-file lists are 99–242 ms (A). The slowest single repetitions were 494 ms (A, `deepchain`) and 508 ms (A, `longname-1500`); those wide `[min–max]` spreads are outliers on a shared machine, not a trend.
- **Peak heap** during a cold build is the decode peak plus A's name copy (A − decode-only = 0–37 MiB; 37 MiB for flatmax) or ≈ the decode peak for B.
- **Warm queries** (`BenchmarkQuery`, `07E-c1-measurements/bench-query.txt`): directory lookup 1.6–416 ns, a 500-row page 0.03–3.7 µs for A and 0.1–16.7 µs for B (the worst B case is a 200k-child directory whose names live in scattered path strings); **0 allocations** in all cases. The 50 ms / 10,000-allocation / 1 MiB per-request targets are not threatened by either variant; the request-level numbers (HTTP, rendering) are for step 8.

### Effect of the trie serializer on decode (same lists, trie+Brotli vs prefix-delta+Brotli)

| dataset | files | decode alloc MiB: trie (prefix) | decode mallocs: trie (prefix) | decode ms: trie (prefix) | decode-only peak heap MiB: trie (prefix) | A cold ms: trie (prefix) |
|---|---:|---|---|---|---|---|
| typical-100 | 100 | 0 (0) | 217 (195) | 0 (0) | 0 (0) | 0 (0) |
| typical-1k | 1000 | 1 (0) | 2241 (2001) | 1 (1) | 1 (0) | 1 (1) |
| typical-10k | 10000 | 6 (4) | 22436 (19983) | 8 (5) | 3 (4) | 8 (6) |
| typical-250k | 250000 | 124 (90) | 561011 (500024) | 199 (144) | 71 (66) | 243 (185) |
| flat-250k | 250000 | 66 (81) | 250075 (499654) | 110 (106) | 59 (57) | 129 (124) |
| deep-250k | 250000 | 234 (94) | 643380 (436202) | 168 (105) | 91 (79) | 246 (139) |
| deepchain-250k | 250000 | 715 (111) | 506679 (250048) | 177 (46) | 142 (92) | 380 (242) |
| wide-250k | 250000 | 101 (86) | 512490 (499758) | 194 (132) | 76 (63) | 260 (184) |
| longpath-250k | 250000 | 243 (102) | 501598 (250038) | 131 (55) | 141 (83) | 168 (99) |
| flatmax-250k | 250000 | 159 (220) | 250068 (500055) | 43 (61) | 140 (172) | 108 (209) |
| longname-1500 | 1500 | 97 (96) | 3444 (3083) | 155 (159) | 94 (95) | 242 (222) |

The trie decoder copies every file into each ancestor's slice (`codec/trie.go:188`, `files = append(files, childFiles...)`): allocations grow with files × depth. Depth-100 chains: **715 MiB allocated** by one decode (166× the declared payload; 2× the allocation count) versus 111 MiB for prefix-delta, yet the live peak was only 142–148 MiB because the GC reclaims the garbage — at 3.8× the decode time (177 vs 46 ms). For shallow shapes the trie decoder allocates +17–37% more bytes (flat: −19%) and takes +4–47% more time; for deep or long-prefix lists it takes 2.4–3.8× the decode time. The prefix-delta decoder makes **two string allocations per path** (`string(rest[:n])` at `prefix.go:77` and the concatenation at `prefix.go:113`; 500,024 mallocs for 250k files), which is why typical shapes allocate ≈ 4.6× the reconstructed path bytes. Neither was touched (instruction: no codec optimisation); both are listed as bottlenecks in §6.

## 5. Weight-model analysis (§7.2; for the C1 decision — no model is replaced here)

Model under test: `W0 = stored + declared + 24·files + P_assumed` with `P_assumed = min(files × MaxPathBytes, MaxAggregatePathBytes)` (= 64 MiB for any list with ≥ 1,000 files). The plan's `index_overhead` term is unspecified; it is shown separately in the observations rather than invented here. Observations: peak heap above the pre-load baseline and process RSS of one decode+A build (max of 5 runs).

| dataset | blob | stored | declared | P actual | P assumed | **W0** MiB | observed peak heap (decode+A) | observed RSS (decode+A)¹ | heap / W0 | RSS / W0 | decode alloc / declared | decode alloc / P | slots at 256 MiB |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| typical-100 | prefix-brotli9 | 0.0 | 0.0 | 0.0 | 6.2 | 6.3 | 0.1 | 2.2 | 0.02 | 0.35 | 15.6× | 15.3× | 40 |
| typical-100 | trie-brotli9 | 0.0 | 0.0 | 0.0 | 6.2 | 6.3 | 0.1 | 2.2 | 0.02 | 0.36 | 17.5× | 16.9× | 40 |
| typical-1k | prefix-brotli9 | 0.0 | 0.1 | 0.1 | 62.5 | 62.6 | 0.5 | 2.5 | 0.01 | 0.04 | 6.4× | 4.5× | 4 |
| typical-1k | trie-brotli9 | 0.0 | 0.1 | 0.1 | 62.5 | 62.6 | 0.6 | 2.6 | 0.01 | 0.04 | 9.2× | 6.5× | 4 |
| typical-10k | prefix-brotli9 | 0.4 | 0.6 | 0.8 | 64.0 | 65.2 | 3.7 | 5.2 | 0.06 | 0.08 | 7.0× | 5.0× | 3 |
| typical-10k | trie-brotli9 | 0.4 | 0.6 | 0.8 | 64.0 | 65.2 | 3.4 | 7.6 | 0.05 | 0.12 | 9.9× | 7.0× | 3 |
| typical-250k | prefix-brotli9 | 9.6 | 14.1 | 19.8 | 64.0 | 93.4 | 86.7 | 93.2 | 0.93 | 1.00 | 6.4× | 4.6× | 2 |
| typical-250k | trie-brotli9 | 9.4 | 14.0 | 19.8 | 64.0 | 93.1 | 90.8 | 96.3 | 0.98 | 1.03 | 8.9× | 6.3× | 2 |
| flat-250k | prefix-brotli9 | 9.0 | 13.4 | 12.4 | 64.0 | 92.1 | 74.8 | 86.3 | 0.81 | 0.94 | 6.1× | 6.6× | 2 |
| flat-250k | trie-brotli9 | 9.2 | 13.8 | 12.4 | 64.0 | 92.8 | 73.2 | 81.8 | 0.79 | 0.88 | 4.8× | 5.3× | 2 |
| deep-250k | prefix-brotli9 | 8.4 | 11.0 | 32.6 | 64.0 | 89.1 | 83.4 | 92.5 | 0.94 | 1.04 | 8.5× | 2.9× | 2 |
| deep-250k | trie-brotli9 | 7.8 | 10.8 | 32.6 | 64.0 | 88.3 | 106.0 | 123.8 | 1.20 | 1.40 | 21.7× | 7.2× | 2 |
| deepchain-250k | prefix-brotli9 | 2.2 | 3.6 | 62.5 | 64.0 | 75.5 | 95.0 | 98.7 | 1.26 | 1.31 | 30.5× | 1.8× | 3 |
| deepchain-250k | trie-brotli9 | 2.3 | 4.3 | 62.5 | 64.0 | 76.3 | 147.7 | 153.4 | 1.94 | 2.01 | 166.4× | 11.4× | 3 |
| wide-250k | prefix-brotli9 | 9.1 | 13.6 | 16.9 | 64.0 | 92.4 | 80.9 | 89.6 | 0.88 | 0.97 | 6.4× | 5.1× | 2 |
| wide-250k | trie-brotli9 | 9.3 | 13.9 | 16.9 | 64.0 | 92.8 | 77.3 | 90.9 | 0.83 | 0.98 | 7.3× | 6.0× | 2 |
| longpath-250k | prefix-brotli9 | 2.0 | 3.3 | 60.6 | 64.0 | 75.0 | 91.4 | 96.3 | 1.22 | 1.28 | 31.3× | 1.7× | 3 |
| longpath-250k | trie-brotli9 | 1.9 | 3.6 | 60.6 | 64.0 | 75.2 | 159.8 | 194.0 | 2.12 | 2.58 | 68.0× | 4.0× | 3 |
| flatmax-250k | prefix-brotli9 | 1.5 | 60.6 | 60.1 | 64.0 | 131.8 | 214.4 | 246.8 | 1.63 | 1.87 | 3.6× | 3.7× | 1 |
| flatmax-250k | trie-brotli9 | 1.6 | 61.7 | 60.1 | 64.0 | 133.1 | 206.1 | 210.1 | 1.55 | 1.58 | 2.6× | 2.6× | 1 |
| adversarial-50k | prefix-brotli9 | 0.5 | 0.7 | 0.6 | 64.0 | 66.4 | 11.3 | 15.5 | 0.17 | 0.23 | 13.3× | 16.9× | 3 |
| longname-1500 | prefix-brotli9 | 19.4 | 28.7 | 28.7 | 64.0 | 112.2 | 120.7 | 127.4 | 1.08 | 1.14 | 3.4× | 3.4× | 2 |
| longname-1500 | trie-brotli9 | 19.4 | 28.7 | 28.7 | 64.0 | 112.2 | 119.6 | 127.4 | 1.07 | 1.14 | 3.4× | 3.4× | 2 |

¹ RSS = kernel peak (`VmHWM`) above the post-load baseline, plus the stored blob.

**Where W0 is accurate:** typical 250k lists (heap/W0 0.93–0.98), 'deep' prefix-delta (0.94), long-names (1.06–1.08).

**Where it underestimates (heap/W0 > 1.15, RSS up to 2.6×):**
- *Flat near-limit names* (`flatmax`: 63 MB of names in one directory): 1.63 heap / 1.87 RSS. `W0` counts the decompressed payload and `P` once; at build time the decoded names *and* A's arena copy (a second `P`) coexist, and the raw payload is garbage the GC has not yet collected.
- *Trie-encoded blobs with depth × files* (`longpath` trie 2.12/2.58; `deepchain` trie 1.94/2.01): allocation amplification (§4) outruns the collector's headroom. Prefix-delta blobs of the same lists are 1.2–1.4 (heap/W0).
- A is the variant measured; B's build adds less transient memory (peak ≈ decode-only), so these are upper readings of the two.

**Where it overestimates:** every list with ≥ 1,000 files but small paths: `typical-1k` predicted 62.6 MiB vs 0.5 MiB observed (125×), `typical-10k` 65.2 vs 3.7 MiB (17×). The cause is `P_assumed` = 64 MiB, which cannot be derived from the payload (the P/declared ratio observed is 0.9–1.4 for typical shapes and **up to 17–18×** for long-prefix prefix-delta blobs, and the format allows more; wire-format limits only bound `P ≤ 64 MiB`). Consequence: a 256 MiB gate admits at most 4 such tiny torrents at once, but each decodes in 1–6 ms, so the gate stays a throughput non-issue; it only wastes the *admission headroom* on mixed workloads.

**How many simultaneous cold builds does the conservative model admit?** `⌊256 MiB / W0⌋`: flatmax 1; typical-250k, flat, deep, wide, longname 2; long-prefix (longpath/deepchain) 3; typical-10k 3; typical-1k 4; typical-100 40 (column "slots" above).

**What actually happens at the admitted concurrency** (builds started together, results kept alive — the worst case, no eviction; `concurrent-admitted.jsonl`):

| dataset | blob | model admits (256 MiB ÷ W0) | K run | W0 each | peak heap MiB (max of reps) | RSS MiB (max of reps)¹ | over 256 MiB? |
|---|---|---:|---:|---:|---:|---:|---|
| deep-250k | prefix-brotli9 | 2 | 2 | 89.1 | 188 | 198 | no |
| deep-250k | trie-brotli9 | 2 | 2 | 88.3 | 227 | 255 | no |
| deepchain-250k | prefix-brotli9 | 3 | 2 | 75.5 | 197 | 202 | no |
| deepchain-250k | prefix-brotli9 | 3 | 3 | 75.5 | 300 | 310 | **yes** |
| deepchain-250k | trie-brotli9 | 3 | 2 | 76.3 | 296 | 315 | **yes** |
| deepchain-250k | trie-brotli9 | 3 | 3 | 76.3 | 429 | 453 | **yes** |
| flatmax-250k | prefix-brotli9 | 1 | 2 (beyond the model) | 131.8 | 409 | 512 | **yes** |
| flatmax-250k | prefix-brotli9 | 1 | 3 (beyond the model) | 131.8 | 686 | 714 | **yes** |
| longname-1500 | prefix-brotli9 | 2 | 2 | 112.2 | 231 | 234 | no |
| longname-1500 | trie-brotli9 | 2 | 2 | 112.2 | 228 | 232 | no |
| longpath-250k | prefix-brotli9 | 3 | 3 | 75.0 | 275 | 286 | **yes** |
| longpath-250k | trie-brotli9 | 3 | 3 | 75.2 | 439 | 508 | **yes** |
| typical-10k | prefix-brotli9 | 3 | 3 | 65.2 | 8 | 11 | no |
| typical-10k | trie-brotli9 | 3 | 3 | 65.2 | 10 | 12 | no |
| typical-1k | prefix-brotli9 | 4 | 4 | 62.6 | 2 | 3 | no |
| typical-1k | trie-brotli9 | 4 | 4 | 62.6 | 2 | 3 | no |
| typical-250k | prefix-brotli9 | 2 | 2 | 93.4 | 174 | 192 | no |
| typical-250k | trie-brotli9 | 2 | 2 | 93.1 | 165 | 193 | no |

- Typical, deep and long-name shapes stay inside the gate (≤ 231 MiB heap, ≤ 255 MiB RSS at the admitted K).
- Long-prefix/deepchain shapes at the model's K=3 exceed it (275–300 MiB heap, 286–310 MiB RSS for prefix-delta; 429–439 MiB heap for trie): the model admitted three because each *looks* like 75 MiB, whereas one decode+A build peaks at ≈ 87–148 MiB.
- Flat near-limit lists are admitted alone (K=1: 214 MiB heap / 247 MiB RSS = 84–96% of the gate); K=2 would reach 409–512 MiB.

**Unbounded concurrency is not viable** (no gate; builds started together, indexes kept): typical 250k reaches ~2 GiB heap at K=32; 4.1 GiB for 32 trie long-path builds:

| dataset | blob | K | peak heap MiB | RSS MiB¹ | wall ms (median) | per-build p50 / p95 ms |
|---|---|---:|---:|---:|---:|---|
| longpath-250k | trie-brotli9 | 2 | 286 | 350 | 218 | 205 / 218 |
| longpath-250k | trie-brotli9 | 4 | 504 | 583 | 302 | 272 / 301 |
| longpath-250k | trie-brotli9 | 8 | 1012 | 1134 | 611 | 546 / 610 |
| longpath-250k | trie-brotli9 | 16 | 1921 | 1980 | 1098 | 984 / 1097 |
| longpath-250k | trie-brotli9 | 32 | 4119 | 4397 | 4875 | 2980 / 4840 |
| typical-250k | prefix-brotli9 | 2 | 154 | 180 | 176 | 170 / 176 |
| typical-250k | prefix-brotli9 | 4 | 297 | 318 | 347 | 301 / 347 |
| typical-250k | prefix-brotli9 | 8 | 533 | 561 | 725 | 596 / 686 |
| typical-250k | prefix-brotli9 | 16 | 1014 | 1067 | 1383 | 1244 / 1373 |
| typical-250k | prefix-brotli9 | 32 | 1984 | 2238 | 2954 | 2637 / 2924 |
| typical-250k | trie-brotli9 | 2 | 169 | 200 | 246 | 241 / 246 |
| typical-250k | trie-brotli9 | 4 | 273 | 320 | 463 | 427 / 463 |
| typical-250k | trie-brotli9 | 8 | 454 | 543 | 950 | 803 / 933 |
| typical-250k | trie-brotli9 | 16 | 965 | 1074 | 1889 | 1726 / 1834 |
| typical-250k | trie-brotli9 | 32 | 1990 | 2254 | 3830 | 3414 / 3728 |

(That file predates a harness fix that records `Files`; the table does not use it.) This confirms that an admission gate is required and that its budget, not the 1 GiB container, is what bounds cold-build memory.

**Memory limit sensitivity (single build, `gomemlimit-*.jsonl`):** with `GOMEMLIMIT=96MiB` the peak heap of every 250k-file build stayed at 81–90 MiB (trie deepchain 426 ms vs 380 ms unconstrained); with 160 MiB the trie long-chain shapes peaked at 136–142 MiB (typical trie 87 MiB). A soft limit therefore *can* flatten the GC-lag part of the underestimation for one build; whether it holds under 2–3 concurrent builds plus the cache (live set ≈ limit → GC thrash) was **not** measured and is a step-8 question.

**Conclusion for C1 (not a decision):** the conservative `W0` is an adequate *typical-case* weight and a poor *bound*. It is wrong in both directions for different reasons that a single constant factor cannot fix: the overestimate is the unknowable `P`, the underestimate is transient copying that depends on the serializer (trie) and on the index variant (A's name copy). Options for the owner, none implemented: (i) keep `W0` and add terms — `+P` for A's copy and a depth-dependent term for trie blobs (the header does not reveal the serializer's depth; `PeekDecodedSize` returns the selection, so the serializer is known); (ii) an *additive codec peek that scans prefix-delta varints to compute `P` exactly* (not started: codec work needs approval) which would remove the 125× overestimate; (iii) calibrated estimate with `GOMEMLIMIT` as the backstop; (iv) lower the gate to 192 MiB. Cached/unchanged-torrent navigation is unaffected by any of them.

## 6. Cache budget and gate: are 256 MiB / 128 MiB reasonable under the codec limits?

- **Cache (128 MiB):** the largest measured retained A index is **64.9 MiB** (flatmax: 63 MB of unshared names), B 76 MiB (deepchain). An analytic A worst case is `P(≤64 MiB) + 20·files + 44·dirs ≈ 64 + 4.8 + ≤10.5 ≈ 80 MiB` (every file in its own directory), so one worst-case entry always fits. With A, typical 250k entries are 17.7 MiB (7 fit) and long-prefix entries 7–8 MiB (16 fit); with B they are 30 MiB (4) and 69–76 MiB (1). *Estimates, not measurements, for the 80 MiB bound.* Entries above the budget are not possible under the default limits for A.
- **Gate (256 MiB) + cache (128 MiB) + baseline:** for typical shapes at the admitted K the total transient + retained is ≈ 190 + 128 + the process baseline (tens of MiB) ≈ 400 MiB, inside the 640 MiB RSS target; for the worst measured admitted case (long-prefix trie, K=3: 439 MiB heap / 508 MiB RSS) plus a full cache it would be ≈ 640–700 MiB. The gate size is therefore *plausible* for typical data and has no margin for adversarial-but-valid blobs unless the weight model is corrected or `GOMEMLIMIT` is set. This was **not** measured with search, HTTP or the real cache running.

## 7. Independent persistence correction

`persistence.Reader.FileBlob` (`persistence/webread.go`) now distinguishes:

| Situation | Result |
|---|---|
| no blob row, or the row version (`xmin`) differs from the identity the caller validated | `ErrBlobChanged` (retryable after refreshing the identity) |
| same version, stored length > `MaxStoredBytes` | **non-retryable** `ErrCorruptBlob` wrapping `*codec.LimitError{Limit: stored_bytes, Actual, Allowed}`; `errors.Is(err, ErrBlobChanged)` is false |
| same version, within limits | blob bytes |

The size check stays **in SQL** (`CASE WHEN xmin::text = $2 AND octet_length(data) <= $3 THEN data END`), so oversized bytes are never transferred; the statement also returns `xmin` and the length so the Go side can classify. One round trip, no retry loop. Regression test `TestReaderFileBlobOversizedIsNotChanged` (PostgreSQL): oversized by one byte, exactly at the limit, stale version while oversized (still retryable), missing blob, unchanged normal blob. The existing `FileBlob` stale-version test and all `persistence` suites pass.

## 8. Independent lossless path presentation (`internal/web/pathfmt`)

Pure functions, no HTTP/templates/cache. `Display(s) []Segment` and `Component(s, maxBytes)`: valid text stays text; each invalid UTF-8 byte is a marker segment `\xNN`; C0/C1 controls (incl. NUL, tab, DEL), U+2028/2029, U+061C, U+200E/F, U+202A–202E, U+2066–2069 and a *real* U+FFFD are `\u{XXXX}` markers; ZWJ and variation selectors are left alone; U+FFFD is never produced; an empty component is one `(empty)` marker; `maxBytes` truncation (optional) ends with an explicit `…(N more bytes)` marker and never splits an escape or a rune. Every `Text` segment is valid UTF-8 free of unsafe runes, so `html/template` escaping is sufficient. `EncodeDir`/`DecodeDir`/`DirQuery` carry a directory prefix as `dir=<url.QueryEscape(bytes)>`: `/ . .. % + & # ? ; =`, NUL, bidi characters and invalid UTF-8 round-trip exactly, no component is normalised, and the root is the absent parameter. Tests: a table of 21 display cases, a losslessness inverse (`parseMarkers`), a dir-encoding table through `url.ParseQuery` and `url.Parse`, and two fuzz targets (`FuzzDisplayLossless`, `FuzzDirRoundTrip`; 15 s each, no failure). Not covered: HTML rendering, templates, styling (W5).

## 9. Benchmark infrastructure

- `internal/web/synth`: deterministic seeded generators; `Corpus()` (the 12 named datasets), `Torrents(base, n)` (n independent torrents with different seeds and ±50% sizes), `Measure`, `Canonical`. Shapes: flat, typical, deep, deepchain, wide, longpath, flatmax, adversarial (duplicates, empty components, leading/trailing `/`, `.`/`..`, invalid UTF-8, NUL, bidi), longname (up to 40 KB components).
- `internal/web/benchkit`: `Describe()` (machine/runtime record), `Hist` (exact percentiles), `Run` (bounded concurrent workers with per-call latency and error classification), `Measure` (allocation deltas), `StartProbe`/`Stop` (peak heap/RSS), `ReadStatus`/`ResetPeakRSS`, `Enabled()` (`MAGNETICO_BENCH_LARGE=1`). Stdlib only; ~330 lines; unit-tested (`-race`, `-count=3`). It is what the later 8/32-concurrent cache and HTTP tests will reuse; nothing here benchmarks a cache or handler that does not exist yet.
- `codec/export_test.go` + `codec/benchblobs_test.go`: test-only hook and a gated dumper (`MAGNETICO_BLOB_DIR`). No encoder/decoder behaviour or format change.
- `filetree/coldbuild_test.go`: the isolated-process matrix and Go benchmarks (§11).

## 10. Decisions requested at C1

1. **Index variant (O2).** Recommended: **A, arena**, built by the scan-then-pack procedure here, held by the cache; B only as the fallback if you prefer lower transient memory over retained size. Rationale: §4 (retained 1–10× smaller; the same query speed; build +10–140 ms, +≤38 MiB peak).
2. **Weight model (O1).** The conservative `W0` is a typical-case weight, not a bound (§5). Choose among: keep as is and rely on `GOMEMLIMIT`; add terms for A's copy and trie-depth; request the additive codec peek that computes `P` for prefix-delta blobs (codec change → needs your approval; not started); lower the gate. Until decided the plan's "reject if W > capacity" and startup assertion stay as written; the worst-case `W0` under the default limits (≈ 132 MiB for flat near-limit names) fits 256 MiB.
3. **Allocation target.** The "≤ 3× uncompressed" target is missed by the *codec's own decode* (§4). Accept as a documented deviation, or authorise codec work (two allocations per path; the trie's per-ancestor append). Not touched.
4. **Index scan speed** (optional, not needed): an LCP shortcut would cut the 160–180 ms on depth-100 chains. Say if you want it.
5. **Next step after C1:** step 6 (cache, single-flight, gate integration) with the decisions above.

## 11. Validation and reproduction

Ran and passing (this branch, in this session):

```
go build ./... && go vet ./...                                   # clean
go test -count=1 -race ./internal/web/... ./codec ./persistence  # all packages ok
go test -count=5 -race ./internal/web/filetree ./internal/web/benchkit ./internal/web/pathfmt
go test -count=1 -run xxx -fuzz FuzzDisplayLossless -fuzztime 15s ./internal/web/pathfmt   # and FuzzDirRoundTrip
go test -count=1 -race -v -run TestReaderFileBlobOversized ./persistence
```

Not run: the full `go test ./...`, `dht`, `crawler`, `e2e`. Benchmarks are explicit:

```
MAGNETICO_BLOB_DIR=$DIR go test -run TestDumpBenchBlobs -timeout 1h ./codec          # writes blobs
MAGNETICO_BENCH_LARGE=1 MAGNETICO_BLOB_DIR=$DIR MAGNETICO_BENCH_OUT=$OUT MAGNETICO_COLD_REPEAT=5 \
    go test -run TestColdBuildReport -timeout 2h ./internal/web/filetree               # isolated matrix
#   MAGNETICO_COLD_CONC='2 3' MAGNETICO_COLD_MODES=arena   concurrent cold builds
#   MAGNETICO_COLD_GOMAXPROCS=8, GOMEMLIMIT=96MiB            sensitivity runs
MAGNETICO_BLOB_DIR=$DIR go test -run '^$' -bench 'ColdBuild|Query' -benchmem ./internal/web/filetree
```

## 12. PostgreSQL search-index policy

Per the owner's direction, search-index optimisation is **deferred until after the 0.8.0 release and migration, using real data**. In this batch the search implementation is unchanged, no index or schema change was made or proposed, and the 52 ms synthetic `name`-sort case was not optimised. The S1 query-plan evidence (`07E-s1-measurements.txt`, `07E-s1-report.md`) is preserved as is. The timeout, cancellation and correctness requirements are unaffected.

## 13. Limitations

- **Synthetic data.** Names are random letters, so stored sizes (Brotli ≈ 0.4–0.7 of the payload for typical shapes) are *not* what real titles give; real blobs are smaller on the wire and the shapes are not real torrents. The index and decode *memory* depends on path bytes and structure, which the shapes cover; the I/O and stored-size terms do not represent production.
- **Serializer coverage.** Real encoders choose the smallest candidate; the forced pairs show behaviour for each decoder, not the proportion in which each occurs.
- **Single idle machine, warm file cache, 5 runs per cell:** the cold times are medians of a few runs, not a request-population p95 under load. `GOMAXPROCS=2` is not a 2-vCPU cgroup; no memory limit was applied, so RSS is not evidence about a 1 GiB container.
- **GC-dependent peaks.** Peak heap/RSS depend on `GOGC`/pacing and on the 2 ms sampler (a shorter peak could be missed; `VmHWM` is the authoritative RSS and agrees with the sampled heap within ~15%).
- **Concurrent runs** start builds together and keep every index alive: a deliberate worst case, not a model of cache eviction. No gate, cache, HTTP handler or PostgreSQL was involved.
- `concurrent-unbounded.jsonl` was produced before the harness recorded `Files` for concurrent runs (those rows have `Files=0`); the report's tables do not use that field.
- **Not measured:** the analytic 80 MiB worst-case A bound; peak behaviour with the real cache and search together; `Index.Bytes()` for B excludes allocator rounding.
- The synthetic corpus checks `codec.Decode` on forced encodings only for the datasets listed; the O(files × depth) scan cost was measured at depth ≤ 100.

## 14. Decisions resolved after C1 (owner, before Steps 6–8)

| Decision | Resolution | Where implemented |
|---|---|---|
| Index variant (O2) | **Arena (A)** in production; Ref (B) kept for comparison/tests, no runtime selection | `internal/web/filecache` uses `filetree.BuildArena` |
| Gate capacity | **192 MiB** (from 256 MiB) | `filecache.DecodeCapacity` |
| Cache budget | **128 MiB** byte-bounded LRU | `filecache.CacheBudget` |
| Weight model (O1) | Keep the conservative `W0` with an index-overhead allowance (`+36·files`); treat it as an admission estimate, not a bound. Options (i)–(iii) of §5 not adopted | `filecache.Weight` |
| Codec work | none; the "≤ 3× uncompressed" allocation target stays a documented target (C1 numbers preserved) | — |
| LCP shortcut | not implemented | — |
| Search-index tuning | deferred until after 0.8.0 release and migration, with real data | — |

**Verification of the 192 MiB assumption.** The largest weight any blob valid under the default codec limits can have is `32 MiB + 64 MiB + 64 MiB + 60 B × 250,000 = 174.3 MiB`, which fits in 192 MiB with 17.7 MiB to spare, so no legal file list is made unbrowsable and no weight is clamped. `filecache.New` refuses to start if the source's limits would allow a larger weight, and `TestMaxWeightFitsDefaultCapacity` pins the number. The weight itself is still only an estimate: §5 measured peaks of up to 2.1× W0 for trie-encoded long-path lists and 1.6–1.9× for flat near-limit names. With the allowance, a typical 250k-file list weighs ≈ 102 MiB, so **the 192 MiB gate admits one such decode at a time** (a second queues for up to 5 s); long-path lists (≈ 84 MiB) admit two, flat near-limit names (≈ 140 MiB) one. This serialisation is visible in the Step 8 latency tails (`07E-web-search-details-report.md` §6). The end-to-end measurements are in that report.
