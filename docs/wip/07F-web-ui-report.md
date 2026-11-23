# 07F — W5 implementation report: approved UI (Demo 3.1) and JSON file browser

Branch `refactor/0.8.0-unify-and-new-storage`, based on `a4e8b8c2`. Status: **implemented, awaiting owner review (W5/S2)**. W6, W7 and W8 were not started, nothing was pushed.

**What could not be done in this environment (read this first).** There is no Chromium on the VM and the only available headless host (obscura) refuses loopback and `file://` addresses, so **no page of the real application was rendered in a browser, no screenshot exists, and no light/dark or 360 px layout was inspected visually.** What was verified instead: the full HTML of every implemented page through the real binary (HTTP), the complete `app.js` file-browser logic in a DOM host (48 in-page checks, all passing; the host is not Chromium), static CSS/markup invariants, and computed contrast ratios. Visual review, real-device behaviour and Chromium runs are owner items; `TestWebDemoServer` and `TestFrontendSelfTestsInBrowser` below make them one command each.

## 1. Commits (oldest first, all local)

| Commit | Subject |
|---|---|
| `d0821fda` | `filetree`: complexity limits (250,000 directories, depth 256), typed `ErrIndexComplexity`, `Child`/`DirName` for positional lookup; `filecache` no longer reclassifies the typed error as corruption |
| `f8939fac` | shared `fileview` service (eligibility, exact size cap, positional directory pages, encode gate, RFC 8259 writer) and the `problem` (RFC 9457) package |
| `9afec825` | `SessionJSON` access class: GET/HEAD only, `401` problem+json for anonymous requests |
| `d8af4625` | `/torrents/{id}/files/tree` and `/torrents/{id}/files?dir=&offset=&limit=` replace the HTML directory browser; details carries the mode; `pathfmt` directory helpers and `files.html` removed; shared Go/JS name vectors |
| `c04c5215` | tree-size and index-complexity measurements |
| `39dc24d6` | Demo 3.1 UI: templates, CSS, `app.js`, `theme.js`, front-end tests, demo-server test, contrast and browser-timing measurements |
| `3f4266ad` | specification revision 6, deployment, plan and README updates, new load scenarios |
| (this commit) | report, load results, final polish |

## 2. Backend and API changes

**Shared service `internal/web/fileview`** (the single implementation; the UI adapter is `internal/web/torrents`, the future `/api/v2` adapter is W7):

- `Decide(torrent)`: stage-1 eligibility. Single-file torrents are eligible; more than 12,000 files is not; otherwise one 12-byte header peek and `estimate = 96·files + declared_bytes ≤ 3 MiB`. Never decodes. A failed peek yields mode `dirs` (details still render).
- `Tree(torrent)`: re-runs `Decide`, then builds the document in a bounded buffer and fails with a typed error if the **exact** size would exceed 4 MiB. Nothing is returned, so nothing can be sent, in that case (test: no body on the refusal path, budget returned).
- `Dir(id, path, page)`: positional path `2_5_6` (grammar `^(0|[1-9][0-9]{0,5})(_…)*$`, ≤ 256 components), `offset` ≥ 0, `limit` 1–1,000 (default 160); subdirectories first (name bytes), then files (canonical order); a page never exceeds 1 MiB (it is cut, never below one entry, and `next_offset` says where to continue). Works for every torrent.
- Encode admission is a separate weighted gate (8 × 4 MiB, 32 waiters, 2 s), distinct from the 192 MiB decode gate; waiting honours the request context; saturation → `503` + `Retry-After`; large buffers are not pooled.
- JSON is written by a dedicated writer with `encoding/json` escaping rules (`appendJSONString`), not Go literals; names are `"name"` or `"name": null` + `"name_base64"`.

**Index limits.** `filetree.Limits{MaxDirs: 250000, MaxDepth: 256}` are checked inside `scan()` before the arrays grow. `filecache.flight.build` now classifies: complexity → returned typed (not corruption, not cached), cancellation → `ErrClosed`, anything else → `ErrCorruptBlob`. Both modes use the same Arena, so `index-too-complex` is final in the UI.

**Routes** (`SessionJSON`, 401 problem+json when anonymous): `GET /torrents/{id}/files/tree`, `GET /torrents/{id}/files`. Headers: `Cache-Control: private, no-store`, `nosniff`, `X-Robots-Tag`, `Content-Length`. Errors (`urn:magnetico:problem:*`): `unauthenticated` 401, `not-found` 404, `invalid-parameter` 400, `full-tree-unavailable` 422 (+ `alternate` URL), `index-too-complex` 422, `temporarily-unavailable` 503 (+ `Retry-After`), `internal-error` 500. Logs carry only request ID, torrent ID, operation, outcome category and byte count.

**Specification.** `web-v1.md` revision 6: §3.4, §4.1, §4.5.3, §6.1, §6.4.1–6.4.13 (new §6.4.7–6.4.13), §7.2–7.6 (the API v2 parity contract, `full_tree_available`, the `422` problem, the OpenAPI plan), §11.1 (explicit JavaScript exception for the file browser), §12.10/12.12, §13.2. `07A` §4.2/§7.5/§8, `web-deployment.md` §7, the W4 report (pointer), `README.md`.

**API v2 (W7) is not exposed.** `TestNoPublicAPIv2RoutesYet` asserts that no `/api/` route exists and the three planned paths answer 404. No schema, codec, storage-format or dependency change (`go.mod` untouched), no Redis, no temporary IDs.

## 3. Front end

Demo 3.1's CSS tokens, layout, tables, forms, tree and responsive rules were imported through a script that removed account/token/invitation/scenario-picker rules and adapted by hand; `layout/torrents/torrent/login/error/message` templates follow the demo's markup. `app.js` (33 KB, readable, no build) provides theme toggle, copy, `/` shortcut, selector auto-submit and the file browser; `theme.js` (0.5 KB) applies the stored theme before first paint. Icons are an inline sprite (no extra request). No third-party request exists (asserted by source tests). Search sorting works through plain links (`aria-sort`), the mobile sort and page size through a GET form (`by=sort:order`); the file browser needs JavaScript and shows a notice otherwise.

## 4. Tests and results

Commands actually run on the final tree (formatting, build and vet over the whole module; test suites restricted to packages that cannot generate DHT/BitTorrent traffic — `dht/...`, `internal/crawler`, `internal/e2e` and the root crawler/migration tests were **not** run, and `go test ./...` was deliberately avoided):

- `gofmt -l .`, `go build ./...`, `go vet ./...`: clean.
- `go test -race -count=1 ./codec/... ./persistence/... ./internal/web/... ./internal/pgtest`: **passed** (all 22 packages, ≈ 2 min)
- `go test -count=1 -run TestWeb .` (real binary, disposable PostgreSQL): **passed** (after updating two assertions that matched the old interim "Signed in as …" navbar text in `main_webauth_test.go`)
- Fuzz: `FuzzStr` (writer vs `encoding/json`) 10 s, 380,000 executions, no finding; `FuzzDisplayLossless` unchanged.

New automated coverage (all against real PostgreSQL where data is involved):

| Requirement | Where |
|---|---|
| Template rendering, escaping, no inline script/style, icons exist, themes define the same tokens | `ui/frontend_test.go`, `ui_test.go` |
| Authentication on both JSON routes; 401 problem+json, no redirect; GET-only registration; no API v2 routes | `auth/sessionjson_test.go`, `torrents/torrents_test.go`, `torrents/fileview_test.go` |
| Search and pagination after the UI change; sort links, `by`, magnets, escaping | existing W4 tests + `TestSearchPageUiMarkupSortingAndMagnets` |
| Duplicates, unusual paths, byte-exact names, both endpoints | `TestUnusualPathsAndDuplicatesByteExact`, `fileview` round-trip tests |
| Positional navigation of every directory without the tree; malformed/out-of-range paths, offset, limit | `TestPositionalNavigationEveryDirectory`, `TestPaginationLimitOffsetAndMalformedValues`, `fileview` tests |
| Payload-size and mode boundaries (file count, estimate, exact cap at cap and cap−1, no partial write) | `TestExactCapOverridesEstimate`, `TestExactSizeCapGives422AndNoPartialBody`, `TestEstimateRefusal`, `TestLargeTorrentRefusesTree…` |
| Large-directory pagination incl. byte-capped pages | `TestDirPageByteCapTrimsAndContinues` |
| Cache reuse (100 navigations → 1 decode; replacement) | `TestHundredNavigationsOneDecodeAndReplacement` |
| Index complexity: boundaries, adversarial lists, early rejection, no corruption classification | `filetree/complexity_test.go`, `filecache` test, `TestIndexComplexityEverywhereButDetails` |
| RFC 8259 validity, round trips, fuzzing | `fileview/encode_test.go` (NUL, controls, quotes, U+2028/2029, U+FFFD, invalid UTF-8, random bytes) |
| 8/32 concurrent trees within the encode budget; encode saturation → 503 | `TestConcurrentTreesStayWithinEncodeBudget`, `TestEncodeSaturationIsBusyAndRecovers` |
| Client-side behaviour: tree build, chunking, filter, restore, collapse; retry, timeout, network failure, bad JSON, wrong version, 401, `index-too-complex`, fallback to directory mode; supersession/cancellation; URL/history; name vectors | `ui/testdata/selftest.js` (48 checks) |
| W3/W4 security, log-safety, saturation and shutdown regressions | unchanged tests, updated only for the new routes |

**Browser checks.** `selftest.js` ran in the obscura DOM host: **48 of 48 passed** after fixing 7 mistakes in the test itself on the first run (wrong row-limit arithmetic, a harness that did not propagate rejections, a scenario that did not hang). The product code needed no change in response. History/URL behaviour is tested through an injected recording `location`/`history` because that host has no real URL. `TestFrontendSelfTestsInBrowser` runs the same file in headless Chromium when `MAGNETICO_TEST_BROWSER=<binary>` is set; **it has never run** (no Chromium here). Not verified at all: rendering, layout, 360 px overflow, focus rings, screen-reader output, reduced motion, real-device performance.

## 5. Measurements

Files: `docs/wip/07F-w5-measurements/` (`tree-size.txt`, `index-complexity.txt`, `browser-js-timing.txt`, `load-summary.txt`, `load-campaign.jsonl`, `demo-datasets.txt`, `contrast.txt`). Machine: Ryzen 9 7950X3D (8 visible cores), Go 1.27.0, server at `GOMAXPROCS=2`.

**Serialized tree size** (exact, per shape; estimate ratio = actual / estimate):

| Shape | Files | JSON (exact) | Encode | Ratio |
|---|---:|---:|---:|---:|
| ISO collection | 40 | 5.0 KiB | 7 µs | 0.79 |
| discography | 300 | 34 KiB | 48 µs | 0.77 |
| series (10 seasons) | 2,400 | 284 KiB | 0.29 ms | 0.78 |
| typical | 12,000 | 1.39 MiB | 4.3 ms | 0.79 |
| typical | 50,000 | 5.8 MiB | 17 ms | 0.79 |
| typical | 250,000 | 29.4 MiB | 66 ms | 0.80 |
| long shared prefix | 12,000 | 0.87 MiB | 0.8 ms | 0.68 |
| deep chains | 12,000 | 1.07 MiB | 6 ms | 0.83 |
| adversarial (17,347 directories) | 12,001 | 1.96 MiB | 26 ms | **1.54** |
| 1,500 very long names | 1,500 | 28.8 MiB | 45 ms | 1.00 |

About 120 bytes per file for ordinary names. **Limits of prediction:** the estimate is conservative for ordinary and long-prefix lists, **under-predicts directory-heavy lists by about 1.5×** (it cannot know the directory count without decoding) and cannot see name length when the payload is compressed differently; the 4 MiB hard cap is therefore authoritative, and a rare miss costs one typed `422`, never an oversized response. The 1,500-long-name list shows why the file-count test alone is not enough (it is far under 12,000 files while being 29 MiB).

**Chosen thresholds** (initial candidates kept; owner review item): 12,000 files, 3 MiB estimate, 4 MiB cap. Reasons: a 12,000-file torrent is ≈ 1.4 MiB; ordinary multi-season series, discographies and ISO collections stay in tree mode; client CPU was not the constraint (obscura engine, JSON parse + build + first render: 11 ms at 12,000 files, 37 ms at 50,000; filter ≈ 30 ms), so the limits protect transfer size and **low-end-phone memory, which was not measured**. The prototype's 4,000 files / 1 MiB suggestion would also have worked technically; it would move torrents of 4,000–12,000 files into paging.

**Index complexity limits** (`index-complexity.txt`): 250,000 one-file directories → index 16.7 MiB, 67 MiB allocated, 29 ms; 1,000 files in 250-deep unique chains (250,000 directories) → 10 MiB; 250,000 files in one 256-deep directory → 6.5 MiB. Rejection just beyond the limits costs up to 56 MiB of allocation (directory count) and nothing for depth (a 20,000-deep path is rejected with 0 bytes allocated). Everything stays far inside the 128 MiB cache budget, so the limits are about adversarial structure, not about legitimate torrents.

**Load** (`load-summary.txt`, 20 s each, no cgroup limit):

| Scenario | Result |
|---|---|
| warm directory pages (300 directories of a 250k-file torrent) | 42,967 × 200, p50 0.45 ms, p95 0.58 ms, p99 0.70 ms; 1 decode; HWM 157 MiB |
| 8 tree clients (24 torrents of 3.5–10.5k files) | 12,621 × 200, p50 12.4 ms, p95 17.4 ms; HWM 159 MiB |
| 32 tree clients | 12,528 × 200 (no 503), p50 50.7 ms, p95 60.8 ms; HWM 159 MiB — memory stays flat, latency grows with queueing on 2 CPUs |
| tree + cold builds + eviction churn + search (16 + 16 + 2) | tree p95 1.23 s, directory pages p95 1.08 s, search p95 163 ms; 0 errors; 443 builds, 417 evictions; HWM 606 MiB |
| existing 32-client cold-heavy directory workload (W4's `conc32`) | 2,830 × 200, p95 1.33 s; HWM 635 MiB (W4 recorded 683–750 MiB) |
| index-complexity torrent hammered (8 clients) next to 8 tree clients | 12,300 × 422 and 11,176 × 200, tree p95 19.6 ms; 0 errors |
| details with / without the header peek | peek p50 0.72 ms, p95 0.93 ms; no peek p50 0.49 ms, p95 0.64 ms: **the peek adds ≈ 0.25 ms** |

Demo datasets through the real binary (`demo-datasets.txt`): small 12 files, standard 86 files and the series (2,400 files, 284 KiB) and 8,000 long paths (590 KiB) choose tree mode; the 120,000-file torrent chooses directory mode and its tree request is a typed `422`, first directory page 92 ms cold; the 300-deep torrent is tree-eligible by size but both endpoints answer `422 index-too-complex` while details render.

## 6. Differences from Demo 3.1

| Difference | Reason |
|---|---|
| No Account nav item, no Short link button, no forgot-password/invitation/RSS/API screens | W6/W7 features are not implemented; no non-functional controls |
| Row magnet icon opens the (tracker-less) magnet link instead of copying; details has Copy magnet (JS) **and** Open magnet | Works without JavaScript; the demo's copy needs JS |
| Sign-out stays visible at 360 px (a POST button with an icon) | The demo hid it on narrow screens, which would leave no way to sign out |
| Column headers are links, mobile sort and page size are a GET form with `by=sort:order`, an Apply button only without JavaScript | The demo's buttons are JavaScript-only |
| "Showing N torrents" (no range or total) | Keyset pagination has no counts (§6.3) |
| No preview-fixture selector, "Simulate an error", scenario notes, demo footer | Prototype tooling |
| `--border-strong` darkened in both themes, light `--muted` darkened (`#617d7b` → `#587270`) | Control borders (2.1 : 1 dark, ~1.7 : 1 light) and light muted text (4.0–4.4 : 1) were below WCAG 1.4.11/1.4.3; now ≥ 3.3 : 1 and ≥ 4.6 : 1 (`contrast.txt`) |
| Skip link, `caption`, `aria-sort`, polite live regions, focus moved to the breadcrumbs after a directory change, 30 s request timeout | Accessibility and failure handling the prototype did not need |
| Mode chosen by the real backend (12,000 files / 3 MiB), not a fixture map: the 8,000 long-path example is tree mode | The prototype forced it to paging only to illustrate fallback |
| Large-torrent warning card replaced by the file count in the status line | Same information, less chrome |
| Inline icon sprite in every page | No extra request, no asset route |

## 7. Remaining issues that matter before 0.8.0

1. **No real-browser verification.** Rendering, 360 px layout, dark/light appearance, keyboard order, focus visibility, screen-reader output and phone performance were not observed. Run `MAGNETICO_W5_DEMO=<dir> go test . -run TestWebDemoServer -timeout 4h` (serves seven synthetic datasets, sign in as `alice` / `binary-browse-password-1`) and `MAGNETICO_TEST_BROWSER=<chromium> go test ./internal/web/ui -run SelfTests`.
2. **Thresholds are proposals** (12,000 / 3 MiB / 4 MiB; encode budget 8 × 4 MiB; complexity 250,000 / 256). They were chosen from server-side measurements; client memory on low-end phones is unmeasured.
3. **Repeated requests for a torrent beyond the complexity limits redo the decode and scan every time** (errors are deliberately not cached, as for corrupt blobs); in the `complex` run 12,300 requests produced 2,333 build attempts (the rest coalesced). Cost per attempt is bounded by the decode gate; a short negative cache keyed by the blob identity would remove it. Not added to keep W4's "errors are not cached" rule.
4. **The estimate under-predicts directory-heavy lists by ≈ 1.5×**; those rely on the exact cap and cost one `422` round trip.
5. **File browser limits:** no virtual scrolling (chunked rendering, 120 rows), no filter in directory mode, not an ARIA `tree`, no keyboard arrow navigation.
6. **JavaScript is required for the file browser** (approved exception); search, details and sign-in work without it.
7. **Obscura is not a browser engine of record:** its results show the logic works, not that Chromium, Firefox or Safari behave identically (e.g. `Intl`/`toLocaleString`, `AbortController` timing).
8. W4's open items are unchanged (RSS peak above its target in the worst workload, codec allocation targets, search tuning).

## 8. Environment and cleanup

Disposable PostgreSQL only (`internal/pgtest` and one benchmark container with its data under `/home/herdr/w5bench`, removed with its volume). No DHT, tracker or peer traffic: the web service never imports the crawler (boundary test), and only loopback listeners were used. The unpacked demo (`/home/herdr/tmp/w5-demo`), scratch scripts and benchmark data were deleted; the original archive `/home/herdr/tmp/magnetico-web-ui-v3-1.tar.zst` was left in place as it was the owner's. Only adapted work was committed; the raw demo (mock `app.js`, fixtures, screenshots) was not imported.

Docker: the benchmark container and its volume, and one leaked anonymous PostgreSQL test volume created during this session, were removed; seven older anonymous volumes (created before this session, 2026-11-22 and earlier on 2026-11-23) were left alone because they are not this task's.
