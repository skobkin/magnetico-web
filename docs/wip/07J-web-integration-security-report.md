# Task 07J report — W9 integration, security, regression and release preparation

**Status:** in progress (working draft, completed in stage S9). The approved plan is [`07J-web-integration-security-plan.md`](07J-web-integration-security-plan.md).

This file is extended stage by stage; sections marked *pending* are filled in by the stage named next to them.

## 1. Findings and fixes

Severity: **P0/P1** blocks the release candidate, **P2** is a real defect or gap with limited impact, **P3** is a documentation or contract nit.

| # | Sev | Finding | Evidence | Impact | Fix |
|---|---|---|---|---|---|
| F1 | P3 | The embedded OpenAPI v2 document declared `400 invalid-parameter` for `GET /torrents/{id}`, but the service never produces it: a non-integer or non-positive id is answered `404 not-found`, exactly like an unknown torrent (one shared `ParseID` for the UI and the API). | `TestW9RealBinaryResponsesMatchOpenAPI`: `GET /api/v2/torrents/abc` → 404 where the document declared 400 | A generated client could handle a status that never occurs; the documented contract disagreed with the implementation | OpenAPI corrected (the `400` removed, the 404 rule stated in the operation description). Behaviour unchanged. |

## 2. Stage S2 — unverified email UX and email removal (done)

Implemented as planned (commits `0df962b2`, `d4532e29`; spec §4.9.8a and §14.9).

- `accounts.Store.RemoveEmail`, `POST /account/email/remove` (session, CSRF, per-account limit, current password; works without SMTP), a removed verified address gets a best-effort notice.
- Removing a verified address, **and** replacing it with a different verified one, deletes every outstanding `setup`/`reset` link of the account. Password links carry no origin or address, so an administrator-issued link cannot be told from an email-issued one and is cancelled too (documented; the operator re-issues it). Removing an unverified address leaves password links alone.
- All writers (removal, replacement, recovery-link issuance, verification, link completion, verification-link issuance) take the user row lock first. `internal/web/accounts/email_removal_test.go` drives **both operation orders** for each pair by pausing one transaction just before COMMIT (removal vs recovery issuance; replacement vs recovery issuance, including issuance by the old and by the new address; removal vs verification, verification-link issuance and password-link completion), plus a concurrent stress test under `-race`.
- The profile page separates the address state from the service state; ten state × SMTP combinations are asserted (`internal/web/account/email_ux_test.go`), including that a server without SMTP never says "verify your email address" and offers no verification action.
- Real binary: a migrated user (historical unverified address) sees the explanation and removes the address (`TestMigrateUsersBinaryEndToEnd`).

## 3. Stage S3 — cross-service integration (done)

Real binaries on one disposable PostgreSQL database, driven through their public interfaces only. **Every service and CLI command in these tests connects with one non-superuser account that owns the schemas it created**, the default deployment of `docs/web-deployment.md` §4 (this also proves that the web startup privilege verification passes for the schema-owning account).

| Test (`main_w9_integration_test.go`) | What it proves |
|---|---|
| `TestW9IngestToWebAndAPI` | Records POSTed to the real `ingest` HTTP interface (hostile names, bidi, markup, duplicate/odd paths, a 4,100-file torrent) become durable and are visible in UI search/details (escaped), UI JSON file endpoints, API v2 (search, details, info-hash lookup, tree, directory pages) and both RSS feeds; the file lists equal what was submitted; UI and API documents are **byte-identical**; the backend selects tree vs directory mode; the magnet redirect works; a resubmission is idempotent; both services stop on SIGTERM with exit 0. |
| `TestW9TorrentMigrationToWeb` | A legacy v0.12.0 source → `migrate torrents` + `verify-decode` + `verify-content` → web/API with the legacy ids (gaps included) and file lists (duplicates, `..`) intact; ingest then adds a torrent with a higher id. |
| `TestW9DevFixturesToWeb` | `dev seed-torrents` → all 101 fixtures listed through the keyset cursor, every file list equals the dataset, and the mode split is exactly the documented 40 single / 60 tree / 1 paginated. |
| `TestW9RealBinaryResponsesMatchOpenAPI` | Responses received over TCP from the binary match the embedded v1 and v2 OpenAPI documents (documented status, media type, `Retry-After`, JSON schema), every documented operation is exercised, including 400/401/404/422/429. Helper: `internal/oastest` (test-only; `TestW9BinaryDoesNotLinkTestSupport` proves the shipped binary links neither it nor the JSON Schema library nor `net/http/pprof`). |
| `TestW9ServicesFailClosedWithoutTheirSchemas` | `ingest` and `web` exit 1, open no listener and print no credential when their schema is missing. |
| `TestW9GracefulShutdownUnderLoad` | SIGTERM to web and ingest while clients keep reading and submitting: both exit 0, no panic output, ingest quota accounting exact, nothing quarantined. |
| `TestMigrateUsersBinaryEndToEnd` (extended) | user migration → login → rehash → legacy RSS credentials, plus the migrated-email page behaviour. |

`main.go`: the inline wiring of `runWeb` moved into `assembleWeb`, with no behaviour change (the existing web binary tests pass), so root tests can assemble the production router.

## 4. Stage S4 — security regression (done)

All in-process web tests use `assembleWeb`, the same wiring as the binary, so they walk the route table that serves requests (46 routes in six access classes; a route without an expectation fails the sweep).

| File | What it asserts |
|---|---|
| `main_w9_security_test.go` | per access class and the verified middleware order (cross-origin protection → mux 404/405 → class guard → handler): anonymous behaviour of every route; **405 before any guard** for every unregistered method; CSRF (missing, invalid, another session's, query-string token) and cross-origin refusal on **every** unsafe route (session and public form); `GET /logout` does nothing; administrator parity with a user on every session route; hostile torrent names (HTML text/title/attribute contexts, JSON, XML) |
| `main_w9_credentials_test.go` | credential-separation matrix (API token, RSS token, legacy token, session, every transport, header-vs-cookie-vs-query precedence, credential never reflected, `Referrer-Policy` and `Cache-Control`); disable/enable/revoke/`revoke-tokens`/password-reset lifecycle across sessions, tokens and invitations; idle, absolute and remember-me expiry with development cookie attributes; unknown / wrong-password / no-password / unsupported-hash / disabled accounts fail identically |
| `main_w9_proxy_test.go` | forged `Host`, `X-Forwarded-*`, `Forwarded`: no email link, invitation link, feed link or cookie attribute changes; trusted-proxy boundary decides the limiter identity (prepended addresses, trailing trusted hops, malformed and over-long chains) |
| `main_w9_input_test.go` | ~150 malformed/oversized/unsupported requests (bodies, escapes, UTF-8, NUL, chunked, parameters, cursors, directory ids on both file endpoints, credentials, method overrides) and raw-TCP header abuse: always a controlled non-5xx, no state change, service alive; over-long passwords rejected before hashing; slow header connections closed |
| `main_w9_secrets_test.go`, `main_w9_smtp_test.go`, S3 tests | secret-leak scanner with intentional delivery channels (see §7); SMTP refused / no STARTTLS / untrusted certificate / dropped connection: identical forgot-password answer, nothing sent in the clear, log holds the failure category only |
| `internal/web/search`, `persistence` | exact trimmed 40-hex boundary table; SQL-tracer assertions (one data statement, no info-hash predicate, no `COUNT`/`OFFSET`, `LIMIT n+1`); keyset walks while rows are inserted; `FuzzDecodeCursor`, `FuzzParsePath` |
| `internal/web/accounts`, `account` | email removal and the recovery-link rule, both operation orders of every race pair |
| `internal/web/feed` | channel author per feed version; XML 1.0 character handling pinned |

## 5. Findings and fixes (continued)

| # | Sev | Finding | Evidence | Impact | Fix |
|---|---|---|---|---|---|
| F2 | P3 | A request whose `Origin` equals its own (forged) `Host` is treated as same-origin by the standard library's cross-origin protection. | `TestW9ForgedHostAndForwardedHeadersDoNotChangeLinks`: `POST /login` with `Host` and `Origin` `evil.example` passes the cross-origin layer | none in the deployments described: the CSRF token (only the real host's pages carry it) and the host-bound `__Host-` cookie still refuse the request, which the test asserts; the rewriting-proxy case that relies on this behaviour would break if the service insisted on the public host | **documented** in `docs/web-deployment.md` §2 (route only the public host name to the service; neither the token nor the cookie depends on `Host`); no code change. |
| F3 | P2 | A password link outstanding for an account (email-issued **or** administrator-issued) survived the removal or replacement of the verified address it was mailed to, leaving the former address as an indirect recovery channel. (Found while designing the approved email-removal feature.) | design review of `one_time_tokens` (no origin or address on `setup`/`reset` rows); `internal/web/accounts/email_removal_test.go` | a link mailed to an address the user had just given up remained usable until it expired (1 h reset, 72 h setup) | removal of a verified address and replacement by a different verified one delete every `setup`/`reset` link of the account in the same user-row-locked transaction; administrator links are cancelled too (documented; re-issue with `reset-password`). No schema change. |
| F4 | P3 | The v2 RSS channel carried a channel-level `<author>`, invalid in RSS 2.0 (flagged by feed validators). | owner review of `web-v1.md` §8.2.1 | cosmetic for torrent clients; invalid per spec | removed from the **v2** feed only (owner decision); v1 keeps the legacy structure; spec and both OpenAPI descriptions updated. |
| F5 | P2 | **Search plan degeneration for two very common words that rarely occur together.** The planner estimates ~44 k matches (independence assumption), walks `idx_torrents_discovered_on` backward and filters, reading 3.5 M rows (~1.8 s warm, 3.49 M buffer hits) for 51 results. | `docs/wip/07J-measurements/explain/09-common-two-terms.txt` (3,499,924-row synthetic dataset) | latency, bounded by the 60 s search timeout and the 4-slot search gate; data-dependent: in the synthetic names each row carries one common word, so two of them almost never co-occur, which exaggerates the effect; real corpora may behave differently | **not changed**: no index may be added without owner approval. Needs the production-like re-measurement listed in the RC0 checklist; see §9 for the proposal framing. |

No P0 or P1 finding was made. Nothing was left unfixed that has an in-scope fix, except F5 (needs an owner decision on indexes).

## 6. Stage S5 — dataset and search plans (done)

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
