# Task 07J — W9 integration, security, regression and RC preparation: plan (approved revision)

## Context

W1–W8 and 07I are accepted (`ec973a18`). W9 closes the web milestone. It has five parts:

1. Prove that the separately built pieces work together on the real binaries.
2. Close the security-test gaps and fix what they find.
3. Measure search, load and memory on a dataset larger than the 101 fixtures.
4. Ship the owner-approved unverified-email UX, including email removal.
5. Write the architecture comparison document and tidy the docs and milestone records.

`docs/wip/07J-web-integration-security-plan.md` is created from this file as the first commit.

Owner answers (2026-11-24):
- **RSS:** drop the invalid channel `<author>` from the **v2 feed only**. v1 keeps the legacy structure.
- **Browser:** obscura is **pre-approved**, including clicks and key presses, against the loopback dev server with a disposable database only. No Chromium image.
- **Tools:** govulncheck (`go run …@pinned`) and a read-only shallow clone of `boramalper/magnetico` in the scratchpad. Mermaid CLI is not approved, so diagrams are checked by hand and that is recorded as unavailable tooling.
- **Remove email:** allowed for unverified **and verified** addresses.

Revision 2 incorporates the owner's 15 review points. They are referenced as (R1)…(R15) below.

## Execution order

`S0 → S1 → S2 → S3 → S4 → S7 → S5 → S6 → S8 → S9`.

- **Dataset generation (S5, first half)** may run in the background while S2–S4 are developed. It does not touch runtime code.
- **Browser stage (S7)** runs before the final load campaign, so that any UI or handler fix lands first.
- **Measurements run once** (R15), on the final runtime code, and every artifact records the git SHA it ran on. If a later fix changes runtime behaviour (handlers, queries, middleware, caches, gates, template rendering, assembly), the affected scenarios are re-run:
  - step runs for request-path changes
  - the soak only for memory-relevant changes
  - documentation-only commits never trigger a re-run

## Existing coverage and main gaps

| Area | Covered by | Gap |
|---|---|---|
| Auth, sessions, CSRF, rate limits, lifecycle | `internal/web/auth`, `accounts`, `account`; binary `main_webauth_test.go`, `main_web07g_test.go` | Session expiry is not tested at binary level. The CSRF and anonymous sweeps cover only per-package test servers, not the router assembled in `main.go`. Nothing asserts that `admin` has no extra web powers. |
| Ingest | `internal/e2e`, `internal/ingest/*` | **No ingest → PostgreSQL → web test.** No binary test of `schema-init`, `ingest-schema-init` or ingest fail-closed. No ingest-binary SIGTERM test or log secret scan. |
| Request security | `server`, `clientip` tests | No binary test with `TRUSTED_PROXIES` set. No test that forged `Host`/`X-Forwarded-*` leave email, feed and magnet links unchanged. |
| Untrusted metadata | JSON, XML and pathfmt tests; search-page XSS | No XSS test of the details-page name or of HTML file names. No bidi test in feeds. The in-browser JS self-test does not run by default. |
| OpenAPI | in-process schema validation; route↔doc check over a hand-built server | No validation of real binary responses. The route↔doc check does not run on the full router. |
| Search | keyset in both directions with ties; `LIMIT n+1` shape; cursor binding; info-hash `callCount()==0` | No test that inserts rows while paging. No SQL assertion that a name search has no info-hash predicate. |
| Email UX | resend and change flows | **No remove-email action.** An unverified address reads only as "Not verified". |

**Facts confirmed:**
- An info-hash lookup happens only when the whole trimmed query is exactly 40 hex characters (`internal/web/search/query.go:60`). A name search goes straight to `SearchTorrents`.
- Every flag and environment variable in the docs exists. `MAGNETICO_INGEST_CACHE_DISABLED` is undocumented.
- The web service has no pprof or expvar. Cache stats are logged only at shutdown.
- The lab dump is **not on this VM** (`/home/herdr/w4bench` is gone).
- `/home` has 101 GB free. `/` (Docker's root) has 7 GB free and `/tmp` is RAM.
- There is no `sudo`, so the OS page cache cannot be dropped (R9).

## Conflicts with specs and owner decisions (identified, not silently changed)

1. **RSS author.** `web-v1.md` §8.2.1 vs RSS 2.0 validity. Resolved by the owner (v2 only). Spec §8.2.1, §8.2.8 and §14 get a revision-8 record, the v2 OpenAPI text is updated if it mentions the author, and the structural golden test follows.
2. **Remove email is a new route.** `POST /account/email/remove` goes into the §6.1 route table and the §14 record. No schema or grant change: `UPDATE users` and `DELETE one_time_tokens` are already granted.
3. **Deployment policy vs docs.** `docs/web-deployment.md` §4 starts web as a restricted runtime role, but the owner's default is one PostgreSQL account for ingest and web. Only the docs change: the single account becomes the default, and the documented minimum grants become optional hardening. Grants and verification contracts stay. A binary test proves that web and ingest start with the same account.
4. **No implementation conflicts** were found with the other owner clarifications. They are wording changes only.

## Stages and commit boundaries

Each stage ends green (`gofmt`, `go build`, `go vet`, relevant package tests) and is committed on the branch. Fixes for findings get **separate `fix:` commits with regression tests**.

### S0 — Plan document
- `docs/wip/07J-web-integration-security-plan.md`
- Commit: `docs: 07J plan`

### S1 — 07I corrections and documentation housekeeping
- **Seeder network wording.** Replace the inaccurate "no network" statements with the real contract: PostgreSQL access only, and no DHT, BitTorrent, tracker or unrelated external activity. Places: `internal/devseed/dataset.go:5`, **`internal/devseed/seed.go:77-78`** (reword to the exact contract "PostgreSQL access only; no DHT, BitTorrent, tracker or unrelated external activity"), `opflags/opflags.go:359` (add "tracker"), `docs/wip/README.md` row 18. A final grep over `internal/`, `opflags/`, `docs/` (excluding historical reports) for "network" confirms that no inaccurate statement is left. The 07I report stays as a historical record.
- **Full-tree wording.** State that availability depends on the file count and on the estimate, data-size and exact-serialization limits, not only on the 4,000-file threshold. Places: `internal/devseed/dataset.go:441`, `docs/development-fixtures.md:17,61`.
- **Fixture checking.** `docs/development-fixtures.md` states that the complete `(path,size)` multiset, duplicates and multiplicity included, is compared for an existing fixture.
- **`docs/web-deployment.md`:**
  - the single-account default
  - drop the "as I know it" wording
  - redaction guidance covering the v1 `api-token` header and cookie as well as query strings
  - recheck the 90 s timeout guidance
  - the "W1–W7" title
- **`docs/crawler.md`:** document `MAGNETICO_INGEST_CACHE_DISABLED`.
- **`docs/migration_0.8.0.md`:** verify the "file-path search removed" claim against PHP `master`, and keep the `magnetico_migration` policy.
- **Milestone records:**
  - `docs/wip/README.md`: 07A–07I accepted, plus a row for 07J
  - `AGENTS.md`: update stale "future task" text without rewriting history
- **Not touched:** the fixture dataset, digest and version, the cleanup gates and the seeder safety contract.
- Commits: `docs: 07I wording corrections`, `docs: W9 documentation and milestone housekeeping`

### S2 — Unverified email UX, email removal, recovery-token hygiene

**Token model review (R1).** `one_time_tokens` has `(token_hash, user_id, purpose ∈ setup|reset|email_verify, email, …)`:
- Password links (`setup`/`reset`) carry **no origin and no address**. An administrator-issued link (CLI `reset-password`) cannot be told apart from an email-issued one.
- A schema change (an origin column) is out of scope and not approved. So the rule is conservative and uniform.

**Store operations** (`internal/web/accounts/selfservice.go`). Every one takes `FOR UPDATE` on the user row as its first statement.

- **`RemoveEmail(ctx, userID)`**, one transaction:
  - lock the user
  - clear `email` and `email_verified_at`
  - delete the user's `email_verify` link (so an old verification link cannot re-add the address)
  - **if the removed address was verified**, also delete **all** outstanding `setup`/`reset` links of that user
  - report whether the address was verified, and what it was
- **`CompleteEmailVerify`**, when it **replaces a different verified address** (the existing `PreviousEmail != ""` case, case-only changes excluded), also deletes the outstanding `setup`/`reset` links in the same transaction. This is the "replacement" risk: reset links mailed to the former address must not survive its replacement.
- **Accepted side effect:** an administrator-issued link outstanding at that moment is also deleted. The operator re-issues it with `web user reset-password`. This is documented in `web-v1.md` §14 and `docs/web-deployment.md`.
- **Unverified removal** deletes only the `email_verify` link. An unverified address was never a recovery channel, so an outstanding setup link (typically administrator-issued) stays valid.
- **Left alone:** sessions, the password, API and RSS tokens, invitations.

**Concurrency (R2).** All writers share one lock, the user row:

| Race | Why it is safe | Test (both operation orders) |
|---|---|---|
| Removal vs `IssueRecoveryLink` (the forgot-password worker) | Look-up and issue are one transaction (`SELECT … WHERE verified AND active FOR UPDATE`). After removal commits, the locked re-check no longer matches, so `ErrNotFound`. If issuance commits first, removal deletes the link in its own transaction. | Hold the user lock in a test transaction, start the other operation, release; assert the final token table and the outcome. |
| Removal vs `CompleteEmailVerify` | Completion locks the user, then deletes the token with `RowsAffected()==1`. If removal deleted the token first, completion is `ErrLinkInvalid`. If completion goes first, removal then clears the new address. | Same lock-hold harness. |
| **Verified-address replacement** (`CompleteEmailVerify`) vs `IssueRecoveryLink` | Both lock the user row. Order A: issuance commits first → the replacement then deletes the just-issued `reset`/`setup` link. Order B: the replacement commits first → issuance **by the old address** no longer matches a verified row (`ErrNotFound`, no token), while issuance by the new address is valid and goes to the new address. | Same lock-hold harness, **both orders**; assert the token table, the returned error and the address the link is bound to. |
| Replacement vs link-completing reset / `RemoveEmail` | Both lock the user. | Same, both orders. |
| Removal vs `IssueEmailVerifyLink` | Both lock the user. | Same. |
| Removal vs link-completing reset | Both lock the user. | Same. |
| A worker that resolved the old address before removal | The worker has no pre-resolved state: the address→account lookup and the link insert are one transaction. The only gap left is the SMTP send, which carries an already-deleted token and is harmless. | Assert the token row is absent after the removal, even though the mail was already handed over. |

Each test also runs repeated concurrent iterations under `-race`.

**Handler.** `POST /account/email/remove` in `internal/web/account/page.go`, modelled on `postEmail` but **without `requireMail`**:
- session CSRF guard
- per-account limiter
- `verifyCurrent` (current password, through the hashing limiter)
- redirect `?done=email-removed`
- logs `user_id` only

When a verified address is removed and SMTP is configured, a best-effort notice goes to it, like the existing change notice.

**Template states (R3).** The page now distinguishes the **address state** from the **service state** (`MailEnabled`). The generic "verify your address to enable recovery" text appears only when verification can actually be done. The other cases have distinct wording. Styling reuses `.notice` and `.status`, so light and dark both work.

| Address state | SMTP configured | SMTP not configured |
|---|---|---|
| none | "Password recovery is unavailable" + add-address form | "Password recovery is unavailable. This server cannot send email; ask an administrator." No form. |
| unverified (incl. migrated historical) | **"Email address — Unverified" / "Password recovery is unavailable"**, owner-approved text ("Verify your email address to enable password recovery. If you do not intend to verify it, you can remove it from your account."). Actions: **Send verification email**, **Remove email**. | "Unverified" / "Password recovery is unavailable", with the reason: "This server cannot currently send email, so this address cannot be verified." **No** verify instruction or button. Action: **Remove email**. |
| verified | "Password recovery is available". Change; Remove, with a warning that recovery becomes unavailable and outstanding reset links are cancelled. | "Verified" + "Email-based recovery is not available on this server because it cannot send email". Remove still offered. |
| verified + pending replacement | Recovery stays **available** through the current address; the pending row is shown without implying unavailability. | as above, with no pending row to act on |
| unverified + pending | unavailable; resend and remove | unavailable; remove |

"Recovery available" is computed on the server as `EmailVerified && MailEnabled`, which matches the forgot-password worker's eligibility.

**Spec and tests.**
- `web-v1.md` §6.1, §4.9.8 and §14 revision 8.
- Store (PostgreSQL): the table above, plus isolation between users and a case-only "replacement".
- Handler: CSRF, wrong password, rate limit, without SMTP, one test per template cell, the old address gets the notice.
- Enumeration: the forgot-password page is unchanged.
- Binary: a migrated user (`phpFixture` → `migrate users`) sees the explanation and removes the address.
- Commits: `feat(accounts): remove email and cancel recovery links`, `feat(web/account): unverified email explanation`

### S3 — Cross-service integration (real binaries, disposable PostgreSQL)
- **Refactor:** move the inline wiring of `runWeb` (`main.go:351-491`) into `assembleWeb(...)`. No behaviour change. This lets root tests sweep the **full** route table.
- **New `main_w9_integration_test.go`**, reusing `buildBinary`, `startWeb`, `webBinaryFixture`, `startSMTPSink`, `phpFixture` and `pgtest`:
  - **Binary schema initialization:** `schema-init`, `ingest-schema-init` and `web-schema-init`; ingest and web fail closed on a missing schema.
  - **Ingest to web:** start the `ingest` and `web` binaries on one database with **the same account**. POST synthetic records, including hostile names and paths, through `/api/ingest/v1/torrents`. Wait until the queue is empty and the torrents are durable. Check every surface against the submitted data:
    - search, details HTML, both file-browser JSON modes
    - API v2 search, details, tree and dirs
    - RSS v1 and v2
    - `/magnet`
  - **Fixtures:** `dev seed-torrents` → web. For all 101 fixtures: API v2 files equal the dataset, the mode split holds (60 tree, 1 dirs, 40 single), and UI↔API bytes match.
  - **Torrent migration:** a synthetic legacy source (existing 04A–04D builders) → `migrate torrents`, `verify-decode`, `verify-content` → visible in web.
  - **User migration:** legacy hash → login → rehash → the legacy RSS token works on v1 and is rejected on `/api/v2`.
  - **Shutdown:** ingest SIGTERM with exit code and drain, and both services stopped while requests are in flight.
  - **OpenAPI validation of real-binary responses.** The in-process validator in `internal/web/apiv2/schema_test.go` (the test-only `jsonschema/v6` dependency, with the vendored official OAS 3.2 schemas) is extracted into a test-support package, `internal/web/apidocs/oastest`. Only `_test.go` files import it, so it never reaches the shipped binary, and `boundary_test.go` is checked for it. The root tests then validate **responses received over TCP from the built binary**, taking them from the data of the ingest→web scenario and the fixtures:
    - every `/api/v2` operation: success bodies, plus every problem type the contract documents that can be produced (`invalid-parameter` 400, `unauthenticated` 401, `not-found` 404, `full-tree-unavailable` 422, `index-too-complex` 422, `rate-limited` 429, `temporarily-unavailable` 503), against the embedded v2 document
    - for each response: the status code and media type must be declared by the operation, and the body must match its schema
    - RSS v1 and v2: the documented status codes, content types and the v1 `401` envelope, and both feeds parsed as XML
    - the sweep walks the embedded document's operations and fails if one has no real-binary exercise
- Commit: `test(e2e): W9 cross-service integration and real-binary OpenAPI validation`

### S4 — Security regression sweep
Root tests use `assembleWeb` and a disposable database. Package tests are added only where a unit is the natural seam.

- **CSRF and authorization sweep (R4).** The expectation is derived from each route's **access class and the verified middleware order**, never "403 everywhere".
  1. First read `server/middleware.go` and `auth` to record the real order (cross-origin protection → authentication → CSRF).
  2. The test builds an expectation table from `Router().Routes()` and the recorded access class.
  3. Expectations per class:

  | Class | Request | Expected |
  |---|---|---|
  | public POST (login, register, forgot, reset, setup, email-verify) | no token or invalid token | CSRF refusal (403) |
  | session POST | anonymous | the authentication result (redirect to `/login`, or 401 for session-JSON) **before** CSRF |
  | session POST | authenticated, no or invalid token | 403 |
  | any unsafe request | cross-origin | refused by cross-origin protection |
  | `APIToken` / `Feed` | unsafe method | 405 |

  The table itself is asserted to cover every registered route, so a new route without an expectation fails the test. A `POST` with a valid token succeeds (or reaches the handler's validation).
- **Other authorization:** session cookie refused on `/api/v2`; RSS token refused on the API; API token refused on feeds; `admin` has no extra web capability; route↔OpenAPI consistency on the full router.
- **Session lifecycle (binary):** idle, absolute and remember expiry via database timestamp moves; logout and reset invalidation; disable/enable over sessions, tokens and invitations; cookie attributes in production and development.
- **Headers and proxies:**
  - forged `Host`, `X-Forwarded-Host`, `X-Forwarded-Proto` and `Forwarded` do not change any absolute URL (SMTP-sink links, feed `link`/`comments`/self, setup links)
  - with `TRUSTED_PROXIES=127.0.0.1/32`: XFF is honoured from a trusted peer, ignored otherwise, and a malformed chain falls back
- **Input:** oversized and malformed bodies (login, account forms, ingest); Go fuzz targets for the cursor decoder and the positional directory parser, with seed corpora run in the normal suite; malformed bearer and feed credentials.
- **Metadata rendering (R6).** Three representations stay **separate**, each tested for its own contract:
  - **HTML:** auto-escaping plus `pathfmt` markers for control, bidi and invalid-UTF-8, on search results, the details page title and name, and file names.
  - **JSON:** lossless (`name`/`name_base64`), no normalisation.
  - **XML (RSS):** preserve every character XML 1.0 allows, **bidi formatting characters included, byte for byte**. Replace only the characters the existing XML contract prohibits (control characters outside tab/LF/CR, with U+FFFD, per `web-v1.md` §14.4). I will first read the writer to confirm exactly which set it replaces and whether any code point is wrongly caught. A parse round-trip asserts that bidi characters survive and that prohibited ones are replaced. v1 and v2 differ only in the owner-approved v2 author change.
- **Secret scanning (R5).** A scan helper with an explicit model of **intentional delivery channels**:
  - Every secret is a unique random value created by the test and registered with its **allowed channels**:

    | Secret | Allowed channels |
    |---|---|
    | setup/reset link, API/RSS token | the CLI stdout that issued it, and the one response that creates it |
    | invitation code | the owner's invitations page |
    | link tokens | the SMTP message |
    | session id | `Set-Cookie` only |
    | user password (login, change, registration, reset) | the request body of its own form only; **never** in any response, header, log or diagnostic |
    | CSRF token | a hidden form field (and the pre-session cookie) in the HTML pages delivered to **its own** client only; never in logs, error bodies, other headers, JSON/API responses or another client's pages |
    | database and SMTP passwords | none |

  - The scan then covers only **unintended** locations:
    - web, ingest and CLI **stderr and logs**
    - error response bodies
    - `Location`, `Referer`-like and other headers
    - failure diagnostics
    - ordinary page bodies, which must not contain another user's secrets
  - Intentional channels are not scanned, so there are no false positives from legitimate delivery.
  - **Negative controls:** a planted secret in each scanned location must be detected, so the scanner cannot silently pass.
  - **Artifact hygiene:** generated credentials are never written to repository files, measurement artifacts or screenshots (see S5–S7).
- **SMTP at binary level:** a sink that refuses or drops the connection gives a fixed message and no secrets in logs.
- **Resource limits:** saturation and recovery of the hash, search, decode, encode and RSS gates, plus ingest admission. Cancellation releases the gate.
- **Search (R7):**
  - **Persistence-level SQL assertion.** With a pgx query tracer on a **dedicated pool**, call `Reader.SearchTorrents` for ordinary name queries and assert:
    - **exactly one statement** for that call (scoped to the persistence search operation; authentication, session touches and other operations are excluded by construction)
    - its **WHERE clause has no predicate on `info_hash`**. The `info_hash` column in the select list is expected and allowed.
    - the existing `TestBuildSearchSQLShape` keeps the `ORDER BY`/`LIMIT n+1` assertions
  - **Service level:** a name search makes no `TorrentByInfoHash` call (the existing `callCount()==0`).
  - **40-hex boundaries:** surrounding whitespace, 39 and 41 characters, 40 hex plus another word, mixed case. Only the exact trimmed 40-hex query reaches `TorrentByInfoHash`.
  - **Pagination under inserts:** both directions and every sort, with no duplicates and no skipped pre-existing rows.
- Commits by area: `test(web): full-router access and CSRF expectations`, `test(web): request-security and secret-leak regression`, `test(web): untrusted metadata rendering`, `test(search): SQL shape and pagination under inserts`, `fix(feed): v2 channel author`, plus `fix:` commits for findings.

### S7 — Browser and accessibility (obscura, pre-approved)
- **Target:** the loopback dev server (development cookie mode) with a disposable database seeded with fixtures, a user and an admin, all with throwaway passwords.
- **Checks:**
  - login, logout, every account tab, and the new email states (including the no-SMTP rendering)
  - search pagination and filters
  - both file-browser modes on long and unusual names
  - loading, error and retry
  - keyboard navigation and visible focus
  - light, dark and system themes
  - a narrow 360 px viewport
  - **Swagger UI, an actual authenticated Try-it-out**, in the browser against the running dev server:
    1. Signed in, open `/api/v2`. Mint an API token and an RSS token for a throwaway account through the harness (an API call with the session), and pass them to the browser steps by typing them into the Authorize dialog, as a user would. They never go into a file, a screenshot or a report.
    2. **Without Authorize:** execute `GET /torrents`. Expect `401` problem+json displayed.
    3. **Authorize** with the API token, then execute `GET /torrents` (a search), `GET /torrents/{id}`, `GET /torrents/{id}/files/tree` and `GET /torrents/{id}/files?dir=…` against fixture torrents (including the 10,000-file one for the `422` tree response). Expect `200` with the body shown in the UI, and the documented `422`.
    4. From the browser's network log: the request is same-origin, carries `Authorization: Bearer …` and **no `Cookie` header** (the docs initializer omits cookies), and there is no request to any other host.
    5. The feed endpoint: Try-it-out with the RSS token as `feed_token` returns the feed; the API token fails on it; the RSS token fails on `/torrents`.
    6. Operations that are not `GET` offer no Try-it-out.
    7. A reload clears the authorization (`persistAuthorization: false`).
    8. The v1 page: the RSS operation works with the RSS token.
  - console errors, and the network log showing no external hosts on any page
- **Accessibility:** labels, semantic structure and contrast (the `contrast.txt` method).
- **Artifact hygiene (R5):** screenshots are taken only of pages that carry no secrets, never of token-creation, invitation-link or setup-link reveal pages. The set is reviewed before it is saved. No cookie jar, storage state or network log is kept.
- **Limits recorded:** obscura only, no real Firefox, Safari or phone. Those go to the RC0 checklist. HTTP-level tests are never counted as browser acceptance.
- Fixes, if any: `fix(web/ui): …`

### S5 — Large dataset, search plans (R8, R9)

**Dataset `w9-large-v1`:** synthetic and deterministic, generated by a gated seeder in the root load test (`MAGNETICO_BENCH_LARGE=1`).

| Population | Count | Files per torrent | Aggregate files | Written how |
|---|---|---|---|---|
| single-file, **no blob** | **3,480,000** | 1 | 3.48 M | SQL `generate_series` (as in W4); names from a Zipf-distributed vocabulary: common (`1080p`, `x264`), selective, Unicode, no-hit terms; `discovered_on` with ties |
| small multi-file | 12,000 | 10–50 (mean ≈ 25) | ≈ 0.30 M | `persistence.Store.AddTorrent` with `internal/web/synth` |
| medium multi-file | 6,000 | 51–500 (mean ≈ 200) | ≈ 1.2 M | same |
| large multi-file | 1,900 | 501–4,000 (mean ≈ 1,500) | ≈ 2.85 M | same |
| huge | 24 | 75 k–250 k (mean ≈ 150 k), W4 shapes | ≈ 3.6 M | same |
| subtotal with blobs | **≈ 19,924** (call it 20,000) | | ≈ 7.95 M | |
| **Total** | **3,499,924 ≈ 3.5 M torrents** | | **≈ 11.4 M files** | the exact counts are fixed by the generator and printed by the pilot |

This is a representative blob population, deliberately far smaller than "150 k torrents × up to 4,000 files". **Limitation:** only ~0.6 % of rows have a blob, against ~65 % in the real 3.46 M-row lab sample (2,247,594 of 3,457,046). Search never reads blobs, so search plans are unaffected, but heap/TOAST size, vacuum behaviour and the blob-read share of the mixed load are **not** representative.

**Estimates (to be replaced by pilot results):**
- Encode time: about 5 µs per file in the compact profile (271 ms for 50 k files in `storage-implementation-report.md`). For 8 M files that is ≈ 40–60 s of codec CPU. With synthetic generation, one transaction per torrent and fsync **on**, the wall time is ≈ 10–20 minutes.
- Disk: blobs ≈ 0.4–0.7 of the raw payload (07E-C1). At about 60 B of path per file that is ≈ 0.25–0.35 GB. The 3.5 M rows plus the trigram GIN and size/time indexes are the bulk; for comparison the real 3.46 M-row target was 2.9 GB. Planned reservation: **15 GB** for data and WAL.
- **Pilot first:** a 1 % run (35 k rows, 200 blobs, one huge torrent) records wall time, `pg_database_size`, WAL volume and index sizes, and extrapolates. The full run proceeds only if the extrapolation fits the safeguards below.

**Safeguards:**
- PostgreSQL 17 data is bind-mounted under `/home/herdr/w9bench/pgdata`, and `TMPDIR` under `/home/herdr/w9bench/tmp`. Nothing goes to `/` or the `/tmp` RAM disk.
- The generator checks `df` before each phase and aborts if `/home` has less than **40 GB** free. It also reports `pg_database_size`.
- The container runs with `--log-driver local --log-opt max-size=10m --log-opt max-file=3`, so Docker logs stay bounded on the small `/`. PostgreSQL logging is limited to errors and slow statements.
- Settings: `fsync=on`, `synchronous_commit=on`, `shared_buffers=512MB`. The settings are recorded.
- The container and data directory are removed at the end (the data directory only after the measurements are archived).

**Methodology (R9):**
- Run **`VACUUM (ANALYZE)`** after the bulk load, so statistics and the visibility map are current. Record `pg_stat_user_tables` and the index sizes.
- **Cache temperature is reported by what was actually established:**

  | Label | How it is produced | Claim |
  |---|---|---|
  | cold application cache | fresh `magnetico web` process | established |
  | cold PostgreSQL shared buffers | container restart (clears shared buffers) | established |
  | warm | repeated runs after warm-up | established |
  | **cold filesystem cache** | would need `drop_caches` or `fadvise`, which need root (`sudo` is not available) | **not established, and not claimed.** After the load the ≈ 4 GB dataset sits in the 31 GB page cache. Every "cold" figure is therefore "cold PostgreSQL buffers, OS-warm". |

- **`EXPLAIN (ANALYZE, BUFFERS)`** (with `SETTINGS`) for the important query shapes:
  - an empty query on each sort and direction, and deep cursor pages
  - a common term, a selective term, multiple terms and no hits
  - the name sort
  - an anchor-only cursor
  - the info-hash lookup
  - RSS `LatestTorrents` at the maximum offset

  Each is run once cold-buffers and three times warm; the median and spread are reported. Output goes to `docs/wip/07J-measurements/explain/`.
- If a new index looks justified, it is written up only as a separate proposal (evidence, estimated size, write amplification, expected gain). No DDL is changed.
- **Pool stats:** a logging-only addition puts the `pgxpool` stats in the drain-time log line next to the file-cache stats. It is reported, and nothing new is exposed.
- Commits: `test(load): W9 large dataset seeder and explain capture`, `chore(web): pool stats in shutdown log`

### S6 — Load steps and soak (R10–R13)

**Client population and credentials (R10).** The generator builds its identities up front through the real interfaces, and holds them in memory only (nothing written to disk or artifacts):
- **Users:** 32, created with the admin CLI, each with one real login through the login form. All logins come through a configured trusted proxy (`TRUSTED_PROXIES=127.0.0.1/32`), each with its own `X-Forwarded-For`, which exercises the proxy path and keeps the per-IP login limit (burst 20, refill 1/30 s) out of the measurement. One session per client, reused.
- **API tokens:** the API limiter allows 120 burst and **2 requests/s per token**. The generator therefore sizes the pool from the target rate: `tokens ≥ ceil(peak_api_rps / (2 × 0.5))`. With 8 owner accounts creating up to 10 tokens each (the creation limit: burst 10, +1 per 6 min) that gives 80 tokens, and requests are spread round-robin so no token exceeds half of its refill rate.
- **RSS tokens:** the feed use limiter has burst 30; the pool is sized the same way, with a lower feed share in the mix.
- **Classification of outcomes**, reported separately and never merged:
  1. 2xx
  2. **429 limiter** (distinguished by the `rate-limited` problem slug or the legacy envelope)
  3. **503 resource-gate saturation** (the `temporarily-unavailable` slug with `Retry-After`)
  4. 503 search timeout
  5. other 5xx
  6. transport errors and client timeouts
- **Validity rule:** a measurement run is valid only if 429s are below 0.1 % of requests. Otherwise the pool is enlarged and the run repeated. A separate, small **limiter-behaviour phase** with a single token checks that 429s behave correctly; it is not part of the performance numbers.

**Workload mix:** 45 % search (common, selective, empty, paged), 20 % details, 12 % directory pages (warm and cold), 5 % full tree, 10 % API v2, 5 % RSS v1/v2, 3 % magnet.

**Environment and resource limits (which process each limit applies to):**

| Process | Scope | Limits |
|---|---|---|
| `magnetico web` | **its own** systemd user scope (cgroup v2) | `memory.max` 1 GiB with `GOMEMLIMIT=768MiB` (the memory variants below), `cpu.max` 200 %, `GOMAXPROCS=2`, `GODEBUG=gctrace=1` |
| `magnetico ingest` | **a separate scope** | `memory.max` 1 GiB with `GOMEMLIMIT=256MiB` (the Task 06 recommendation), `cpu.max` 100 % |
| PostgreSQL (Docker, bind-mounted) | outside both | the Docker defaults; `fsync=on` |
| load generator and ingest producer | outside both | none; their own CPU and RSS are sampled and reported |

The limits apply to **web alone** and to **ingest alone**, never to the two combined. The web-specific container memory recommendation therefore comes from the web scope only; the ingest figure is reported separately, and the report does not add the two. PostgreSQL, the generator and both services share one 8-core host, so CPU contention between them is a stated limitation. The S5 dataset is used, web and ingest share one PostgreSQL account, and the binaries are the `main.go`-assembled production builds.

**Step runs:** concurrency 4, 8, 16, 32 and 64, three minutes each, once from a cold start and once after a restart with cold application caches. Throughput, latency percentiles (per request class) and the six outcome classes are recorded per step.

**Soak timeline (R13), one continuous run:**

| Phase | Time | Load | Ingest |
|---|---|---|---|
| warm-up | 0–30 min | 16 read clients | **off** (cache fill) |
| steady | 30–120 min | 16 read clients | **on**, 20 records/s from the real ingest binary |
| drain | from 120 min | **both off** | the producer stops; time until `magnetico_ingest.queue` is empty is recorded |
| idle | the 20 minutes after the queue drains | **no HTTP requests at all** (no `/readyz` probe, no diag polling); sampled only from `/proc`, the cgroup files and `pg_stat_activity`. The access-log count is verified to stay flat | none |

Ingest metrics, measured **without any scan of the 3.5 M-row `torrents` table** (no unrestricted `COUNT(*)` while the benchmark runs):
- producer side: records sent, `202` counts and non-202 responses by class, from the generator itself
- queue depth every 10 s: `SELECT count(*) FROM magnetico_ingest.queue`. That table is bounded by the admission quota (a few thousand rows at most), so it is cheap, and the `usage` row is read as well. The first pilot measures the cost of this query, and if it is not negligible the interval is lengthened.
- consumer throughput = records acknowledged by the producer minus the change in queue depth, over the sampling window. If the ingest binary already logs consumer counters or batch outcomes (to be checked in the pilot), those are used as a cross-check.
- **Single verification after the drain:** the generator's infohashes are looked up in batches (`info_hash = ANY($1)`, 1,000 per statement, PK/unique-index lookups, about 100 statements for 20/s × 90 min) to prove that every record is stored. A sample is also searched through the web UI and API.
- the backlog slope over the steady window, the maximum depth as a share of the admission quota, and the drain time

**Drain deadline:** the queue must be empty **within 10 minutes** of the producer stopping (hard gate), and the steady-window depth must stay below 50 % of the admission quota with a slope not above zero by the pilot-calibrated noise. A sustained positive backlog slope or a missed deadline **fails the run**, and the mixed load must not hide it.

**Collected every 10 s:**
- request rate and latency per class
- outcome classes
- CPU (`cpu.stat`)
- RSS and VmHWM
- cgroup `memory.peak`, `memory.events`
- GC count, pause and heap-after-GC (from gctrace)
- threads, file descriptors, PostgreSQL connections

**Goroutine and heap-detail measurement (R11).** `SIGQUIT` kills a Go process, so it is **not** used on the measured instance, and no production pprof endpoint is added.
- The performance and soak numbers come from the **production build**, ended with a graceful SIGTERM so that the shutdown statistics (cache and pool) are collected.
- A **separate diagnostic run** (same workload, 45 minutes plus a 15-minute idle phase) uses a binary built with a **test-only build tag** `w9diag`:
  - the tagged file adds a loopback-only listener (`127.0.0.1`, random port, enabled only by an environment variable) that serves JSON from `runtime.NumGoroutine`, `runtime.ReadMemStats` and a goroutine-profile summary aggregated by creation site, plus the file-cache and pool stats through a small `diagnostics` hook variable that is nil in normal builds
  - baseline and post-load goroutine counts then come from the same process, non-destructively, and the heap profile can be taken for any growth that needs explaining
  - a test asserts the default build contains **no** diagnostic listener (`go list -deps` for the default build excludes `net/http/pprof`, and the binary starts no extra listener)
  - the endpoint output contains no names, paths, queries or secrets
- The diagnostic run is **not** used for the performance numbers because its polling perturbs the idle phase.

**Memory acceptance (R12).** No single threshold decides; the evidence is evaluated together. Windows are fixed in advance:
- **Excluded warm-up:** the first 30 minutes, until the file-index cache (128 MiB cap) has filled and started evicting.
- **Steady window:** minutes 30–120. Each series is reduced to 10-minute medians, giving nine points that dampen GC-pacing noise.
- **Judged together:**
  - heap-after-GC trend over the steady window (slope with its confidence interval)
  - cache occupancy against its cap (from the diag run and the shutdown log)
  - RSS trend
  - cgroup `memory.events` (`high`/`max`/`oom_kill`) and `memory.pressure`
  - goroutine and file-descriptor counts
  - whether growth plateaus
- **Screening thresholds, not leak verdicts:**
  - a heap-after-GC slope above **2 % of the plateau per hour** or an RSS slope above **5 % per hour** triggers an investigation with the heap profile from the diag run
  - the numbers come from the measured noise of a 20-minute pilot (the noise is calibrated and reported, and the thresholds are set at about twice that noise)
  - they are heuristics. A violation is explained or fixed, and a pass is not proof of no leak
- **Idle phase.** RSS is **not** required to return to its pre-load level, because the bounded caches intentionally retain memory and the Go scavenger releases pages lazily. Instead:
  - heap-after-GC after idle must be explained by retained cache bytes (the cache's occupancy accounts for it)
  - the RSS-release behaviour is reported
  - goroutines and file descriptors must return to the baseline
- **Hard gates (outcome classes).** Every request has an expected-status set. A violation fails the run; it is never only "reported".

  | Category | Gate |
  |---|---|
  | OOM kill, crash, deadlock, starvation (no-progress watchdog) | none allowed |
  | other 5xx | **0** |
  | search timeouts (503 `search-timeout`) | **0** in the soak and in steps ≤ 32; at step 64 at most 0.1 % and each one investigated (query shape and plan) |
  | transport errors and client timeouts (client timeout 90 s, above the 70 s search write deadline) | **0**; the clients are stopped before the servers, so shutdown artifacts are excluded by construction |
  | unexpected 4xx (any status outside the request's expected set, other than 429) | **0** |
  | 429 | below 0.1 % (the run-validity rule); otherwise the run is repeated with a larger credential pool |
  | 503 gate saturation | **0** in the 16-client soak; allowed only in the 32/64 steps, under 5 % per step, with `Retry-After` and recovery after the load drops |
  | ingestion failures | **0** non-202 producer responses during the steady phase at 20/s (well below capacity), **0** quarantine rows, **0** consumer error log lines, **every** produced infohash stored after the drain, and the drain deadline above |
- **Reported, not gated:** latency against the W4 and W5 targets.
- **Memory recommendation:**
  - runs: a 1 GiB cgroup with `GOMEMLIMIT=768MiB` (the W4 recommendation), and a short 768 MiB cgroup with `GOMEMLIMIT=512MiB` to locate the floor
  - the report states the `GOMEMLIMIT` value and the container limit **separately**, and does not extrapolate to production scale
- **Outputs:** raw JSONL and summaries go to `docs/wip/07J-measurements/`. Each file records the git SHA and machine. They contain no credentials (R5).
- Commits: `test(load): W9 mixed load, soak and diagnostic build tag`, `docs: 07J measurements`

### S8 — `docs/projects_differences.md`
- **Sources:** `tgragnato/magnetico@f80660653e7d…` (in local history); the read-only clone of `boramalper/magnetico` (cite the tag or commit); `master:` of the PHP project; the specs and reports here.
- **Content:**
  - a last-updated date
  - Mermaid lineage: boramalper (magneticod + magneticow) → tgragnato Go fork. PHP Magnetico Web **replaced magneticow only**, with the crawler left in place. 0.8.0 absorbs and modifies the tgragnato crawler and replaces the PHP runtime. The owner's real migration source (the legacy Go crawler with the v0.12.0 PostgreSQL layout, paired with PHP web) is shown separately from the lineage.
  - a comparison table, then what/why/trade-offs for each difference
  - diagrams: lineage, service architecture, direct persistence vs durable ingest, file rows vs blobs, the two browser modes, positional addressing
  - non-goals vs deferred items, worded as the brief requires
- **Topics:** one binary; PostgreSQL only; codec profiles and the no-blob single-file case; the SQLite crawler queue → ingest → PostgreSQL queue; the ingest cache (plain LRU, positive entries kept until eviction, 20 s negative TTL, and why); the offline migrator; the private UI and the invitation rationale (owner's text verbatim); keyset search and `LIMIT n+1`; the file-browser modes and positional paths; credential separation; deployment behind a reverse proxy.
- **Storage figures (R14).** Before writing them I re-read `04A`/`04B-verification-recovery-report.md`, `storage-implementation-report.md` and `05-codec-determinism-report.md`, and cite the **exact values and units** from the original text. The document distinguishes:
  - **total PostgreSQL footprint** (the `17 GB` source and the `2.9 GB` target in the 04B real-sample run, including whatever the report says those figures cover)
  - from **compressed payload size** (blob bytes)

  It states the **codec revision/profile** benchmarked (profile `compact` and the commit current when the measurement ran, found from `git log` and the report date). If a report does not record the revision, the document says so, and does not infer it. The "~5.9×" figure is quoted with its context: the lab sample of 3,457,046 torrents, and the 1,209,452 single-file torrents with no blob.
- **Also:**
  - link the document from `README.md`
  - add the `AGENTS.md` maintenance rule: update comparisons, rationale, diagrams and the date; ground comparisons in versions or commits; separate implemented from planned
- **Mermaid check:** by hand (no CLI was approved), recorded as unavailable tooling.
- Commit: `docs: projects_differences.md and maintenance rule`

### S9 — RC checklist, presentation topics, final verification and report
- `rc0-acceptance-checklist.md`: the browser and device items, real-proxy timeout and log-redaction checks, live DHT, image-based memory confirmation, and anything open from S4–S7.
- `docs/wip/0.8.0-release-presentation-topics.md`: the ten listed topics, as a list only.
- **Final verification (R15):**
  - `gofmt -l .`
  - `go build ./...`
  - `go vet ./...`
  - `golangci-lint run`
  - `go test ./... -count=1`
  - `go test -race ./... -count=1`
  - PostgreSQL and e2e suites (checked as not skipped)
  - OpenAPI tests
  - `go run golang.org/x/vuln/cmd/govulncheck@<pinned> ./...`
  - a link check script over `docs/`, `README.md`, `AGENTS.md`
  - **The S5/S6 campaign is not repeated here.** Its results stand, and the report records, per artifact, the SHA it ran on and whether any later commit touched runtime code. If so, only the affected scenarios were re-run, as described under *Execution order*.
- Flaky tests are investigated, never re-run until they pass. Skips and missing tools are listed.
- `docs/wip/07J-web-integration-security-report.md`: summary, coverage matrix, findings (severity, evidence, impact, fix), exact commands and results, load methodology and data, the memory recommendation, limitations, RC items and the commit list.
- Commit: `docs: 07J report`

## Safety constraints during the unattended run
- Only disposable databases (`pgtest`, the `w9bench` container). No production, no DHT, BitTorrent or tracker traffic (`crawl` is never run), and no push, force-push or change to `master`.
- External network use is limited to the approved govulncheck download and the `boramalper` clone.
- Stop and ask only for:
  - a genuine architecture conflict
  - a schema, grant, wire-format or index change
  - any destructive operation
  - a P0/P1 finding whose fix needs an owner decision

## Verification summary (end to end)
S3 and S4 tests are part of the normal `go test ./...`; the race run covers them. The S5/S6 gated campaigns (`MAGNETICO_BENCH_LARGE=1`, `MAGNETICO_LOAD_*`) write to `docs/wip/07J-measurements/`. S7 produces obscura session notes and reviewed secret-free screenshots, summarised in the report.
