# Task 07J report — W9 integration, security, regression and release preparation

**Status:** implemented; awaiting owner review. Approved plan: [`07J-web-integration-security-plan.md`](07J-web-integration-security-plan.md). Raw measurements: [`07J-measurements/`](07J-measurements/README.md). Nothing was deployed, published or run against production; no schema, grant, wire-format or index change; no live DHT, BitTorrent or tracker traffic. Observed results, assumptions and recommendations are labelled as such.

## 1. Implementation summary

| Stage | What | Main commits |
|---|---|---|
| S0 | plan committed | `6a6137dc` |
| S1 | 07I wording corrections (network contract, full-tree conditions, complete file-list comparison); documentation and milestone housekeeping (07A–07I accepted, README/AGENTS.md refreshed, single PostgreSQL account as the default, credential-logging guidance for the v1 header/cookie, file-path-search claim corrected) | `6989634b`, `828bc61e` |
| S2 | unverified email UX and email removal; recovery links cancelled when a verified address is removed or replaced; race tests for both operation orders | `0df962b2`, `d4532e29`, `ec88e8a3` |
| S3 | `assembleWeb` refactor; cross-service real-binary integration; real-binary OpenAPI validation (`internal/oastest`) | `6d404b12` |
| S4 | full-router access/CSRF/credential/session sweeps; forged-header and trusted-proxy tests; fuzz targets; secret-leak scanner; malformed-input table; SMTP failure containment; search SQL/boundary/insert tests; RSS v2 author fix; log level fix | `77485e98`, `e853c676`, `988bd026`, `6a99f477`, `d14056c7`, `84c8f280`, `cc6517aa`, `5e7ae22f` |
| S5 | dataset seeder, search plans (`explain.sh`) | `6e94f54b`, `07J-measurements/explain/` |
| S6 | load/soak harness, steps, soak, diagnostic run, memory floor, knee | `6e94f54b`, `0efd2b7d`, `cc7f6bf0`, `bc4fced0`, measurements commits |
| S7 | browser and accessibility checks | **not run** (see §3) |
| S8 | `docs/projects_differences.md`, README link, `AGENTS.md` maintenance rule | `fa29adb8` |
| S9 | RC0 checklist additions, presentation topics, final verification, this report | see §10 |

### Deviations from the approved plan (all deliberate, all recorded)

1. **S7 (real browser) was not run.** The available headless browser (obscura) refuses loopback addresses through its own SSRF guard (`Access to private/internal IP address 127.0.0.1 is not allowed`). Enabling it means starting the tool with `--allow-private-network` or `OBSCURA_ALLOW_PRIVATE_NETWORK=1`, which reconfigures a tool outside this repository and was not done. No HTTP-level test is presented as browser acceptance. Every browser, keyboard, theme, narrow-layout and Swagger UI Try-it-out check (including the authenticated request sequence of the approved plan) is in the RC0 checklist.
2. **The 5 % busy bound needed a load model.** The plan bounded gate-saturation 503s at 5 % for the 32/64-worker steps. Closed-loop workers without think time turn an instant "busy" answer into an instant retry, so that bound cannot hold by construction once the four search slots are exceeded. The 32/64-worker steps are therefore an explicit **overload test** (only bounded 503s allowed, every other outcome class zero, a clean recovery segment afterwards), and the 5 % bound applies to an **interactive** model (users with a mean think time of 1 s). Both are reported in full.
3. **Soak heap trend.** The heap-after-GC slope of the soak exceeded the 2 %/h screening threshold, which the plan routes to an investigation with the diagnostic run (see §6, finding F9).
4. **Added runs:** memory-floor probes at 768, 640 and 512 MiB (the plan named one), and a "knee" profile that varies the number of search slots.
5. `internal/oastest` lives outside `internal/web` (the web import-boundary test forbids the JSON Schema library in any web package); `assembleWeb` takes no context.
6. The diagnostic run's goroutine grouping by creation site came out empty (a regular expression expected spaces where the profile uses tabs; fixed afterwards and not re-run). The goroutine **counts** and the in-use heap by allocation site were captured and are what the conclusions rest on.
7. `govulncheck` is pinned to `v1.10.0` (released 2026-11-17, seven days before use: the only stable release newer than `v1.9.0`).

## 2. Coverage matrix

"Existing" tests predate 07J and were reviewed, not duplicated; **new** tests are 07J's. `root` = package `main` (real binary or the production wiring in-process), paths are repository-relative.

| Contract / invariant | Existing | New in 07J | Gap left |
|---|---|---|---|
| Schema init and start-up verification (web, ingest) | `main_web_test.go` `TestMigrateWebSchemaInitBinary`, `TestWebBinaryFailsClosedWithoutWebSchema`, `TestSchemaDiagnosticsDoNotLeakCatalogContentsBinary`; `webschema/schema_test.go`; `ingest/pgqueue` `TestOpenVerifiesSchemaVersion` | `TestW9ServicesFailClosedWithoutTheirSchemas` (ingest and web: exit 1, no listener, no credential), `newW9World` runs all three `*-schema-init` as the application account | — |
| Development fixtures → web | `devseed` suite | `TestW9DevFixturesToWeb` (all 101 listed, every file list exact, 40/60/1 mode split) | — |
| Torrent migration → web | 04A–04D suites | `TestW9TorrentMigrationToWeb` (ids, gaps, duplicates, `..`, verifications, then ingest) | — |
| User migration → authentication | `TestMigrateUsersBinaryEndToEnd` | extended: unverified historical email explanation and removal on the migrated account | — |
| Ingest → torrent visible in web/API | e2e stops at the database | `TestW9IngestToWebAndAPI` (real ingest HTTP, UI + JSON + API + both feeds + magnet; byte-identical UI/API documents; secret scan of both processes) | — |
| Account management, credential lifecycle | `accounts/lifecycle_test.go`, `selfservice_test.go`, `apiv2` and `feed` revocation tests | `TestW9AccountStateAndCredentialLifecycle` (disable/enable/revoke/revoke-tokens/reset over sessions, tokens, legacy token, invitations), `email_removal_test.go` | — |
| Search, details, both file-browser modes | `torrents`, `fileview`, `filecache` suites, `TestWebBinaryBrowseSearchDetailsFiles` | `TestW9IngestToWebAndAPI`, `TestW9DevFixturesToWeb` | no real-browser rendering (see RC0) |
| REST API v2 contract | `apidocs`, `apiv2/schema_test.go` (in-process) | `TestW9RealBinaryResponsesMatchOpenAPI` (every v1/v2 operation, status, media type, `Retry-After`, schema, over TCP) | 500/503 problem types cannot be produced deterministically from outside; covered in-process |
| RSS v1/v2: XML, escaping, GUID, dates, paging, credentials | `feed_test.go` (golden structure, hostile names, precedence) | channel author per version; XML 1.0 character handling; credential never reflected in body or header; every transport and precedence rule (`TestW9CredentialSeparationMatrix`) | — |
| Tracker management, public magnet redirect | `trackers`, `TestWebBinaryAccountAPIFeedsDocsAndEmail` | magnet redirect in the ingest→web scenario; `/magnet` echo check in the secret scan | — |
| Graceful shutdown | `server_test.go`, `TestWebBinary…SIGTERM`, ingest shutdown tests | `TestW9GracefulShutdownUnderLoad` (both services, with clients and a producer running; exit 0, exact quota accounting) | — |
| Authentication failures, disabled, unsupported legacy hash | `auth/login_test.go`, `password_test.go` | `TestW9AuthenticationFailuresAreIndistinguishable` | — |
| Legacy hash verify + rehash | `fixtures_test.go`, `TestMigrateUsersBinaryEndToEnd` | — | — |
| Session expiry, idle, remember-me, rotation, logout | `auth` suites (fake clock) | `TestW9SessionExpiryIdleAndRemember` (real service, database timestamps), logout in `TestW9RouteTableCSRFAndCrossOrigin` | — |
| Concurrent login vs account-state change | `accounts/login_test.go`, `auth/more_test.go` | — | — |
| Rate limiting, resource-exhaustion resistance | `ratelimit`, `auth/more_test.go`, hashing gate | `TestW9TrustedProxyBoundaryDecidesTheRateLimitIdentity`; overload + recovery segments of the load run | — |
| Cookie attributes (production / development) | `TestWebBinaryProductionCookieAttributes`, `TestDevelopmentModeCookies` | development attributes in the real service; forwarding headers do not change them | — |
| Anonymous access to HTML / JSON / API / feeds | per-package route tests | `TestW9RouteTableAnonymousAndMethodRestrictions` over the **full** route table (46 routes) | — |
| User vs administrator | — | `TestW9AdministratorHasNoExtraWebCapability` | — |
| Isolation of account data, invitations, credentials between users | `TestTokensAndInvitationsOfOtherUsersAreInvisibleAndUntouchable` | other-user pages scanned for another user's secrets (`TestW9NoSecretsInUnintendedLocations`) | — |
| API tokens ≠ RSS tokens ≠ legacy ≠ sessions | `apiv2`, `feed`, `accounts/tokens_test.go` | `TestW9CredentialSeparationMatrix` (every credential × every transport × every surface) | — |
| CSRF on every state-changing route; cross-origin | `TestEveryPostRouteRequiresCSRF` (account package only), `server_test.go` | `TestW9RouteTableCSRFAndCrossOrigin` on the full route table | — |
| Forged `Host`, `X-Forwarded-*`, trusted-proxy boundary, canonical URLs | `clientip_test.go`, `TestWebBinaryRateLimitIgnoresSpoofedForwardedFor` | `TestW9ForgedHostAndForwardedHeadersDoNotChangeLinks`, `TestW9TrustedProxyBoundaryDecidesTheRateLimitIdentity` | — |
| Malformed / oversized bodies, cursors, directory ids, pagination, credentials, HTTP methods | package tests (`TestRequestBodyIsLimited`, `TestCursorRejections`, `TestDirPositionalErrors`, …) | `TestW9MalformedAndOversizedRequests` (~150 cases + raw TCP), `TestW9SlowHeaderConnectionsAreClosed`, `FuzzDecodeCursor`, `FuzzParsePath` | — |
| Enumeration resistance, token expiry/replay, concurrent consumption | `account/public_test.go`, `accounts/links_test.go`, `expiry_test.go` | forgot-password identical under SMTP failure (`TestW9SMTPFailures…`) | — |
| SMTP failure, queue saturation, shutdown, strict TLS, header injection | `mail/sender_test.go` | binary level: refused, no STARTTLS, untrusted certificate, dropped connection; shutdown stays clean | queue saturation is package-level only |
| Untrusted metadata in HTML / JSON / XML | `torrents`, `pathfmt`, `apiv2`, `feed`, `fileview` suites | `TestW9HostileTorrentNamesRenderSafely`; XML 1.0 pinned (`TestXMLKeepsAllowedCharactersAndReplacesOnlyProhibitedOnes`) | JS rendering of file names (`app.js`) is covered by the gated in-browser self-test only |
| Sensitive information in logs, errors, headers | package log tests | `TestW9NoSecretsInUnintendedLocations` (scanner with intentional channels and negative controls), binary output scans in three tests | — |
| Resource limits: hashing, HTTP concurrency, search, file-list decode/index, tree serialization, cache, RSS, mail queue, ingest admission; cancellation, gate release, recovery | `gate`, `filecache`, `search/service_test.go`, `fileview`, `feed`, `mail`, `ingest` suites | load run: overload segments (32/64 workers) must shed only bounded 503s and a recovery segment must be clean | no HTTP-connection cap exists (only gates and the pool) |
| Search: keyset both directions, ties, `LIMIT n+1`, no count, cursor binding, inserts during paging, exact info-hash boundary | `TestSearchKeysetWalksAllSortsAndDirections`, `TestBuildSearchSQLShape`, `TestCursorRejections`, `TestInfoHashLookup` | `TestKeysetWalkWhileTorrentsAreInserted`, `TestNameSearchSQLHasNoInfoHashPredicate` (query tracer), `TestOnlyAnExactTrimmedFortyHexQueryTakesTheLookupPath`, fuzz | — |
| Swagger UI offline, GET-only Try-it-out | `apidocs_test.go` (CSP, vendored hashes, initializer source) | — | real-browser Try-it-out (RC0) |
| Unverified email UX | — | `email_ux_test.go` (ten state × SMTP cells), `email_removal_test.go`, binary check on a migrated account | real-browser rendering (RC0) |


## 3. Browser and accessibility (S7)

**Not run**, for the reason given in §1. What exists instead is source-level and in-process evidence, reported as such: no inline script and strict `default-src 'self'` policy on every page (asserted for the details page, hostile names included), `textContent`-only rendering of file names (source test), shared Go/JS name vectors (the in-browser self-test is gated by `MAGNETICO_TEST_BROWSER` and was not run), the contrast computation of `07F-w5-measurements/contrast.txt` (not repeated for the new notice and status styles, which reuse existing tokens in both themes), `role="status"` removed from the persistent recovery notices, and the Swagger UI source tests. The outstanding checks are listed in `rc0-acceptance-checklist.md`.

## 4. Security findings and fixes

Severity: **P0/P1** blocks the release candidate; **P2** a real defect or gap with limited impact; **P3** a documentation, contract or sizing note.

| # | Sev | Finding | Evidence | Impact | Remediation |
|---|---|---|---|---|---|
| F1 | P3 | The embedded OpenAPI v2 document declared `400 invalid-parameter` for `GET /torrents/{id}`, which the service never produces (a malformed id is `404`, one shared `ParseID` for UI and API). | `TestW9RealBinaryResponsesMatchOpenAPI` | a generated client could handle a status that never occurs | OpenAPI corrected (the `400` removed, the rule stated); behaviour unchanged. |
| F2 | P3 | A request whose `Origin` equals its own forged `Host` is treated as same-origin by the standard library's cross-origin protection. | `TestW9ForgedHostAndForwardedHeadersDoNotChangeLinks` | none in the described deployments: the CSRF token and the host-bound `__Host-` cookie still refuse it (asserted); a rewriting proxy relies on this behaviour | documented in `docs/web-deployment.md` §2 (route only the public host name to the service); no code change. |
| F3 | P2 | A password link outstanding for an account (email-issued **or** administrator-issued) survived the removal or replacement of the verified address it was mailed to, leaving the former address as an indirect recovery channel. | token-model review; `email_removal_test.go` | a link mailed to an address the user had just given up stayed usable until it expired (1 h reset, 72 h setup) | removal of a verified address and replacement by a different verified one delete every `setup`/`reset` link of the account in the same user-row-locked transaction; administrator links are cancelled too (documented; re-issue with `reset-password`); no schema change. |
| F4 | P3 | The v2 RSS channel carried a channel-level `<author>`, invalid in RSS 2.0. | owner review of spec §8.2.1 | cosmetic for torrent clients, flagged by validators | removed from the **v2** feed only (owner decision); v1 unchanged; spec and both OpenAPI descriptions updated. |
| F5 | P2 | **Search plan degeneration for two very common words that rarely occur together**: the planner expects ~44 k matches (independence assumption), walks the date index backward and filters, reading 3.5 M rows for 51 results (1.77 s warm, 3.49 M buffer hits). | `07J-measurements/explain/09-common-two-terms.txt` | latency (bounded by the 60 s timeout and the search gate). Data-dependent: in the synthetic names each row carries one common word, which exaggerates it; real names may behave differently | **open, needs an owner decision on indexes**: no index may be added without approval, and a proposal needs production-like data (RC0 checklist). Evidence only; no proposal with numbers is made here. |
| F6 | P3 | Every deliberate 503 "busy" answer (backpressure with `Retry-After`) was logged at `ERROR`: 554,756 error lines for 554,756 sheds in a 9-minute overload. | memory-floor run (`memfloor-768m`) | an alert on `ERROR` would be drowned by load shedding | **fixed**: a 503 with `Retry-After` is logged at `WARN`, every other 5xx stays `ERROR` (`TestBackpressureIsLoggedAsAWarningAndFaultsAsErrors`); the 8-slot knee run afterwards had no `ERROR` lines; documented in `web-deployment.md`. |
| F7 | P3 | **Search capacity knee.** All gate saturation comes from the search gate (four slots by default), not from file decoding (which shed < 0.1 % even at 800 users). With four slots the 5 % busy bound holds to between 150 and 400 interactive users; with eight it holds at 400 (4.5 %) and breaks at 800. | steps and knee runs (§5) | sizing: an instance with more than ~150 simultaneous heavy interactive users sheds searches | **documented** as a tuning knob (`MAGNETICO_WEB_SEARCH_CONCURRENCY`, at most the pool size minus 2); the default is unchanged (the right value depends on the database and CPU of the deployment). |
| F8 | P3 | Rendered feed pages stay allocated after their 60 s TTL until overwritten: ~25 MiB retained (13 objects) at the end of the load and after the idle phase. | diagnostic run, in-use heap by site | bounded (at most 10 pages × 2 feeds); no growth | none; recorded. |
| F9 | P3 | **Soak heap trend, inconclusive.** Over the 90-minute steady window of the soak, heap-after-GC rose by +6.3 MiB/h (least squares, +3.5 %/h) / +5.5 MiB/h (Theil–Sen, +3.1 %/h), above the 2 %/h screening threshold; RSS was flat (+0.3 %/h, Theil–Sen +0.7 %/h). The diagnostic run did **not** reproduce it (−0.3 %/h over 45 min) and showed goroutines flat at 43 (12 at start, **11 at the end of idle**) and a retained heap fully explained by the bounded caches. | `soak-summary.md`, `diag-summary.md` | cannot exclude growth of a few MiB per hour; the block medians (175–194 MiB) are noisy | not a leak verdict either way; a ≥ 24 h soak with the real image is in the RC0 checklist. |

**No P0 or P1 finding was made or remains open.** P2: F3 fixed; F5 is open and needs an owner decision (index policy), not an in-scope fix.

### Secret-leak scanning model

A secret is a unique value the test created. Its **intentional delivery channels are not scanned** (a user password: its own form body; a CSRF token: the hidden field of a page delivered to its own client; a session id: `Set-Cookie`; a new API/RSS token: the one response that creates it; an invitation code: the owner's invitations page; a setup/reset link: the CLI output that issued it, the email and the link's own page; database and SMTP passwords: none). **Everything else is scanned**: the service log, error and redirect responses, every response header except the session `Set-Cookie`, the pages of another user, the diagnostics of failing CLI commands, and the output of the ingest and web processes. Negative controls plant a secret in each kind of location (and in the real log) and require the scanner to find it. The scan found nothing. No generated credential was written to a repository file, a measurement file or a screenshot (the measurement files were grepped for token prefixes and the load-test password).

## 5. Load and soak measurements

Method, models, gates and reproduction: [`07J-measurements/README.md`](07J-measurements/README.md). Data: `steps.jsonl`, `soak.jsonl`, `diag.jsonl`, `memfloor-*.jsonl`, `knee-*.jsonl` and their `*-summary.md`. Host: AMD Ryzen 9 7950X3D, 8 logical CPUs, 32 GiB, Linux 6.12, Go 1.27.0, PostgreSQL 17.12 in Docker (`fsync=on`, `shared_buffers=512MB`), all on one machine: PostgreSQL, the generator and both services compete for the CPUs. The web service ran in its own cgroup (`GOMAXPROCS=2`, CPU quota 200 %, memory limit and `GOMEMLIMIT` as stated), ingest in another.

**What the workload is.** 32 users with sessions, 320 API tokens and 320 RSS tokens; 45 % search, 20 % details, 12 % directory pages, 5 % tree, 12 % API, 3 % RSS, 3 % magnet. Requests pick torrents uniformly from ~20,000 blob-carrying torrents, so the file-index cache (128 MiB) mostly **misses** (about 96 % of file requests are cold decodes: 7,609 hits against 181,763 misses in a 25-minute series). This is a deliberately cold, worst-case file mix on **typical-shape** file lists (the adversarial shapes of the W4 corpus were not part of it). No claim is made about production-scale behaviour: the names are synthetic and only ~0.6 % of rows have a blob (§5.1).

### 5.1 Dataset and search plans

Dataset `w9-large-v1` (`TestW9SeedLargeDataset`, `MAGNETICO_BENCH_LARGE=1`): **3,499,924 torrents**, of which 3,480,000 single-file rows without a blob and **19,924 multi-file torrents with real blobs holding 9,842,529 files** (12,000 of 10–50 files, 6,000 of 51–500, 1,900 of 501–4,000, 24 of 75 k–225 k). Generated in **6 min 50 s** on the lab host with `fsync=on`; `pg_database_size` **1,278 MiB** (torrents table 337 MiB, its indexes 536 MiB, blobs 395 MiB); free space guard 40 GiB (93 GiB free afterwards). The 1 % pilot (35,000 rows, 5 s, 26 MiB) predicted the order of magnitude. PostgreSQL 17.12 in Docker with the data directory under `/home/herdr/w9bench/pgdata`, `shared_buffers=512MB`, bounded container logs; `VACUUM (ANALYZE)` ran after the load.

**Limitations of the dataset.** Names are synthetic (Zipf-skewed common words, a mid-frequency word, a long-tail word, ~3 % Unicode); only ~0.6 % of rows have a blob against ~65 % in the real lab sample, so heap/TOAST size and the blob-read share are **not** representative (search never reads blobs); the lab sample itself is not available on this VM. No claim is made about production-scale latency.

**`EXPLAIN (ANALYZE, BUFFERS, SETTINGS)`** of the production search statements, 20 shapes, each once right after a container restart and three times warm (`docs/wip/07J-measurements/explain/`, `explain.sh`). Cache temperature is reported by what was established: *cold application cache* (a fresh process), *cold PostgreSQL shared buffers* (container restart), *warm*; **a cold filesystem cache was not established** (it needs root to drop caches, which this VM does not have), so every "cold" figure is "cold PostgreSQL buffers, OS page cache warm".

| # | shape | cold buffers (ms) | warm, 3rd run (ms) | buffers (warm) |
|---|---|---|---|---|
| 01 | empty query, newest first | 4.45 | 3.08 | shared hit=413 |
| 02 | empty query, oldest first | 0.48 | 0.13 | shared hit=70 |
| 03 | empty query, size desc | 0.18 | 0.08 | shared hit=41 |
| 04 | empty query, size asc | 18.52 | 3.54 | shared hit=2893 |
| 05 | deep keyset page (row 1,500,000), newest | 0.49 | 0.14 | shared hit=75 |
| 06 | deep keyset page, backward | 0.43 | 0.18 | shared hit=65 |
| 07 | deep keyset page, size desc | 0.43 | 0.09 | shared hit=65 |
| 08 | one common word (1080p), newest | 9.21 | 7.54 | shared hit=414 |
| 09 | two common words (1080p + x264), newest | 1952.22 | 1772.25 | shared hit=3494180 |
| 10 | one common word, size desc | 0.41 | 0.28 | shared hit=259 |
| 11 | one common word, name sort | 366.11 | 316.44 | shared hit=44096 |
| 12 | mid-frequency word (m17) | 12.33 | 8.27 | shared hit=2579 |
| 13 | selective long-tail word (w123456) | 3.15 | 2.28 | shared hit=159 |
| 14 | selective + common word | 3.70 | 2.82 | shared hit=249 |
| 15 | no hit (zzqxj) | 0.11 | 0.08 | shared hit=16 |
| 16 | Unicode word | 12.40 | 9.19 | shared hit=3694 |
| 17 | common word + deep keyset page | 1.23 | 0.28 | shared hit=177 |
| 18 | exact info-hash lookup | 0.05 | 0.03 | shared hit=4 |
| 19 | anchor-only cursor lookup | 0.05 | 0.04 | shared hit=4 |
| 20 | RSS page at the maximum offset (9,000 + 1,000) | 2.47 | 1.08 | shared hit=197 |

Reading: first pages, deep keyset pages (in both directions), selective terms, the exact info-hash lookup and the anchor lookup are milliseconds or less, including right after a buffer flush. Common words stay in single-digit milliseconds when the date or size index finds 51 matches quickly (#08, #10), and cost ~0.3 s when a common word must be **sorted by name** (#11: a parallel bitmap heap scan over ~1.1 M matches, top-N sort). **#09 is the outlier** (finding F5): the independence assumption makes the planner expect ~44 k matches for two common words, so it walks the date index through the whole table. The synthetic names make that exaggerated (one common word per row), so the figure is evidence of the *mechanism*, not of how often it happens on real names. The `ILIKE '%…%'` plans use `idx_torrents_name_gin_trgm` (137 MiB) where they use an index for the name at all. No index was added or proposed with numbers: a proposal needs production-like data (see the RC0 checklist).

### 5.2 Steps (two series, each 3 min per segment; series 2 after a restart with cold application caches)

Outcome classes are counted separately; **429 = 0, other 5xx = 0, unexpected 4xx = 0, transport errors = 0, search timeouts = 0 in every segment of both series.**

| segment | users / workers | successful req/s | search p50 / p95 (ms) | details p95 | dirs p95 | api p95 | busy 503 |
|---|---|---|---|---|---|---|---|
| closed loop c4 | 4 | 235 / 240 | 8 / 84, 8 / 81 | 2 | 5 | 16 | 0 |
| c8 | 8 | 261 / 283 | 24 / 129, 24 / 120 | 4 | 7 | 87 | 0 |
| c16 | 16 | 255 / 247 | 73 / 253, 75 / 270 | 4 | 8 | 206 | 0 |
| c32 (**overload**) | 32 | 1,006 / 1,002 | 153 / 417, 151 / 381 | 16 | 89 | 355 | 179,859 / 178,486 |
| c64 (**overload**) | 64 | 1,081 / 1,094 | 178 / 387, 172 / 378 | 54 | 192 | 355 | 202,857 / 202,636 |
| recovery (c4, 30 s) | 4 | 293 / 239 | 8 / 75, 8 / 82 | 2 | 5 | 17 | **0** |
| interactive 50 users | 50 | 47 / 48 | 8 / 79, 8 / 78 | 2 | 4 | 15 | 5 / 0 (≤ 0.06 %) |
| interactive 150 users | 150 | 144 / 143 | 9 / 111, 9 / 143 | 2–3 | 5–6 | 47–71 | 225 / 211 (**0.8 %**) |
| interactive 400 users | 400 | 322 / 328 | 60 / 234, 57 / 231 | 5–7 | 16–22 | 186–190 | 9,622 / 9,081 (**13.8 % / 13.0 %**) |

Reading: the closed loop saturates at ~250–280 successful requests/s (the search gate and the database). The overload segments shed instantly (the busy answers are cheap, hence the large counts and the higher "successful" rate, which is the non-search traffic completing faster); the admitted work is unharmed (all other classes clean) and the service **recovers fully**. The 5 % bound holds at 50 and 150 interactive users and **fails at 400 (13–14 %)**: the only gate failure of the campaign, finding F7. Processes: web VmHWM 626 / 623 MiB, cgroup peak 657 / 650 MiB, no memory events, max heap-after-GC 291 / 297 MiB, 3,404 / 3,481 GC cycles with 574 / 506 ms total pause; both services exited 0 on SIGTERM.

**Knee (search slots, cold start, interactive users, 3 min each)** — busy share of all requests:

| users | 4 slots (default) | 8 slots |
|---|---|---|
| 150 | 11.2 % | 1.0 % |
| 400 | 26.5 % | 4.5 % |
| 800 | 41.0 % | 31.1 % |

These are first-segment-after-cold-start figures (in the steps campaign the same tiers followed warm segments and shed less: 0.8 % and 13 %), so cold PostgreSQL and application caches matter in the first minutes. The busy answers are almost all `search` and API search (e.g. 4 slots, 400 users: 14,934 search and 3,244 API answers, none from directory or tree requests).

### 5.3 Soak (one continuous run, 16 closed-loop clients, then a bounded drain and an idle phase)

| phase | duration | load | ingest |
|---|---|---|---|
| warm-up | 30 min | 16 clients | off |
| steady | 90 min | 16 clients | on, 20 records/s |
| drain | until the queue is empty | off | producer stopped |
| idle | 20 min | **no HTTP request at all** | none |

**Gates: all passed.** 532,356 requests in warm-up and 1,362,017 in the steady phase (296 and 252 successful requests/s; search p50/p95 68/214 and 88/228 ms); **zero** 429, busy, timeout, other 5xx, unexpected 4xx and transport errors. Ingest: 108,035 records sent, **108,035 acknowledged (202)**, refusals none; the queue depth never exceeded 1 row (slope 0); the drain took 4 ms (deadline 10 min); a batched index lookup found **all 108,035** stored, none quarantined. Both services exited 0; no cgroup memory event (`high`/`max`/`oom` all 0); WARN/ERROR lines: only the development-mode cookie warning at startup. CPU (average over the steady phase): web 0.41 cores (of 2 allowed), ingest 0.01 cores.

Memory windows (web), steady minutes 30–120, 10-minute block medians:

| series | block medians | least-squares slope | Theil–Sen |
|---|---|---|---|
| RSS (MiB) | 422, 446, 429, 425, 429, 431, 450, 423, 430 | +1.2 MiB/h (+0.3 %/h) | +2.9 MiB/h (+0.7 %/h) |
| heap-after-GC (MiB) | 175, 183, 178, 176, 179, 176, 194, 182, 184 | +6.3 MiB/h (**+3.5 %/h**) | +5.5 MiB/h (+3.1 %/h) |
| cgroup incl. page cache (MiB) | 502, 570, 550, 529, 532, 540, 540, 555, 561 | +18 MiB/h | +34 MiB/h |

Thresholds are **screening heuristics** (2 %/h heap-after-GC, 5 %/h RSS), set from the plan, not leak verdicts: RSS passes, heap-after-GC triggered the investigation (F9). Idle phase: RSS 464 → 257 MiB (the scavenger returns pages; RSS is not required to return to the pre-load value because the bounded caches intentionally retain memory), cgroup 594 → 464 MiB, file descriptors 33 → 7, threads 9 → 9. Peaks: VmHWM 600 MiB, cgroup 730 MiB (RSS plus page cache) in a 1 GiB limit; ingest VmHWM 139 MiB, cgroup 122 MiB.

### 5.4 Diagnostic run (build tag `w9diag`, one process, half-scale timeline)

Goroutines: **12** at start, a constant **43** under load for 60 minutes, **27** after the load, **11** at the end of the idle phase (the baseline). Heap-after-GC slope in the 45-minute steady window: −1.0 MiB/h (Theil–Sen), i.e. not reproduced. In-use heap by allocation site at the end of the load, unchanged after the idle phase: arena cache (`bytealg.MakeNoZero` 78 MiB + `BuildArenaLimits` 23 MiB + `pack` 5 MiB, the file-index cache at its 128 MiB cap), rendered feed pages 25 MiB (F8), templates and static assets 2 MiB. The file cache stayed at 119–128 MiB; the connection pool at 4 acquired / 12 idle of 16. 54,005 ingested records were all stored, no goroutine or descriptor growth.

### 5.5 Memory floor (typical shapes, 9 min of the overload workload, web cgroup alone)

| cgroup limit | `GOMEMLIMIT` | VmHWM | cgroup peak | `memory.events` (`high`/`max`/`oom_kill`) | GC cycles |
|---|---|---|---|---|---|
| 1 GiB | 768 MiB | 626 MiB (steps) | 657 MiB (steps) | 0 / 0 / 0 | 3,404 |
| 768 MiB | 512 MiB | 537 MiB | 605 MiB | 0 / 0 / 0 | 3,034 |
| 640 MiB | 448 MiB | 475 MiB | 538 MiB | 0 / 0 / 0 | 3,297 |
| 512 MiB | 384 MiB | 414 MiB | 478 MiB | 0 / 0 / 0 | 4,266 |

The floor was **not reached**: even 512 MiB survived this workload, at the price of ~25–40 % more GC cycles and no headroom. These runs used typical-shape file lists only.

## 6. Container memory recommendation

*Observed:* on typical-shape lists, with the cold-heavy mix above, the web service peaked at RSS 626 MiB (VmHWM) and 657–730 MiB of cgroup memory (RSS plus page cache) in a 1 GiB limit with `GOMEMLIMIT=768MiB`, with no memory pressure event in any run; the bounded structures that set the footprint are the file-index cache (128 MiB cap), the decode gate (192 MiB weighted), the encode budget (8 × 4 MiB), and ~25 MiB of rendered feed pages. The W4 measurements on the adversarial corpus (deep chains, 63 MB of path bytes in one directory, long paths; 250,000-file lists) peaked higher (cgroup 749 MiB, RSS ≤ 740 MiB at 32 clients) and the weight model under-estimates trie and long-path lists by up to 2.1× (`07E-web-search-details-report.md`).

*Recommendation* (not a proof of out-of-memory safety, and not a claim about production scale):

- **Container (cgroup) memory limit: 1 GiB for the web service**; ingest keeps the Task 06 recommendation (1 GiB container, `GOMEMLIMIT=256MiB`; observed here: VmHWM 139 MiB at 20 records/s).
- **`GOMEMLIMIT=768MiB` for the web service** (75 % of the container limit): a *soft* limit on the Go heap that makes the collector work harder before the *hard* container limit is reached. The two numbers are different things: raising `GOMEMLIMIT` toward the container limit removes the headroom for non-heap memory, page cache accounting and the weight-model error.
- 512–640 MiB containers survived the typical-shape workload and are **not** recommended: they leave no margin for the adversarial shapes.
- Set `MAGNETICO_WEB_POOL_MAX_CONNS` and `MAGNETICO_WEB_SEARCH_CONCURRENCY` together (search slots ≤ pool − 2).

## 7. Verification

@@VERIFY@@

## 8. Remaining limitations and unresolved findings

- **F5 (search plan, two common words that rarely occur together) is open** and needs the owner's index decision on production-like data.
- **No real-browser acceptance** (§3); no real reverse proxy, real SMTP provider, live DHT or real user data was exercised.
- The measurements are single-host, synthetic and typical-shape; the file-list blob share of the dataset (~0.6 % of rows) is far below the real library's (~65 %); cold figures mean "cold PostgreSQL buffers, OS page cache warm" (no root to drop caches); the soak is 2.5 hours, so slow growth below the noise (F9) cannot be excluded.
- The performance campaign ran on commits `0efd2b7d`/`cc7f6bf0`/`f96be7fc`; later runtime changes: the access-log level of 503 backpressure (`5e7ae22f`, measured afterwards by the 8-slot knee run) and the persistent-notice `role` removal. Neither changes the request path or memory behaviour, so the long campaigns were not repeated.
- Pre-existing `golangci-lint` findings (see §7) remain in files this task did not author.
- HTTP connection admission is limited only by the gates and the connection pool (no general connection cap exists); not changed.
- Queue saturation of the mail queue is tested at package level only.

## 9. Release-candidate acceptance items

Added to `rc0-acceptance-checklist.md`: browser/device/accessibility checks (including the authenticated Swagger UI Try-it-out sequence), reverse-proxy timeouts, host routing, log redaction and caching, container memory with the real image, a real SMTP provider, search plans on a production-like copy (F5), live-DHT acceptance, the real user-migration dry run, and — from F9 — a ≥ 24 h soak with the real image.

## 10. Commits (branch `refactor/0.8.0-unify-and-new-storage`, since the accepted `ec973a18`)

@@COMMITS@@
