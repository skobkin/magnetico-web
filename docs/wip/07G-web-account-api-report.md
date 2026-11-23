# 07G — W6 + W7 report: account management, email, credentials, API v2, RSS, magnet links, API documentation

Branch `refactor/0.8.0-unify-and-new-storage`, based on `880fb785` (W5 follow-ups). Status: **implemented in stages S1–S5, awaiting the combined 07G review**. Nothing was pushed. W8 and W9 were not started.

**What could not be done in this environment (read this first).** There is no browser on the development VM. **No page of the real application was rendered in a browser**: the new account, registration, recovery and documentation pages were checked as HTML through the real binary and in-process handlers only, and the vendored Swagger UI was never executed. In particular **not verified**: that the Swagger UI pages run cleanly under the content-security policy (no `unsafe-eval`, no `unsafe-inline`; only `img-src 'self' data:` was added), that "Try it out" works, and the light/dark and 360 px presentation of every new page. These are owner items for the separate browser test. No real SMTP server, production database or public network was used; the only network access was the three approved development downloads (§7).

## 1. Commits (oldest first, all local), grouped by stage

| Stage | Commit | Subject |
|---|---|---|
| S1 | `7290c61a` | `accounts`: self-service password change, token listing/owner-scoped revocation/throttled use time/creation cap, owner-scoped invitations, email-verification links, recovery lookup |
| S1 | `9e02fd3b` | `server`: several background tasks (`AddBackground`), `APIToken` and `Feed` access classes; `ratelimit.KeyOf` |
| S2 | `6df9c563` | `mail`: strict-TLS SMTP sender, bounded queue, templates |
| S2 | `07871162` | registration, setup/reset/forgot/verify pages and the account page (handlers, templates, CSS, wiring in `runWeb`) |
| S4 (first) | `94dff271` | `trackers`: snapshot cache, magnet builder, `/magnet/{infohash}` |
| S4 (first) | `e3bd15eb` | magnets carry the trackers; details page short link; wiring |
| S3 | `5cf63517` | `/api/v2` (Bearer guard, four torrent/file endpoints, shared `torrents.FileAPI`), embedded OpenAPI v2 |
| S4 | `ac299a91` | RSS v1 and v2 feeds, `persistence.LatestTorrents`, OpenAPI v1 and the v2 feed path |
| S5 | `0c7023bf` | vendored Swagger UI, documentation pages, OpenAPI 3.2.0 and its validation |
| S5 | `0a9bc954` | real-binary 07G integration test and measurements |
| S5 | (this commit) | specification revision 7, deployment guide, plan and README, this report |

Ordering deviations from the plan, all deliberate: the tracker cache (planned for S4) came before the API (S3) because `magnet_uri` needs the snapshot; the two S2 handler commits are one commit (the handlers share `Install`); the two S1 account commits are one (the files interleave). Each stage was built and its packages tested before the commit; the final tree passed the full run of §4. (Each intermediate commit was not re-tested individually.)

## 2. Implemented routes and features

* **Account** (`GET /account?tab=profile|invitations|rss|api`, Session + CSRF on every `POST`): password change (current password, per-account limiter, other sessions deleted, current session rotated with the same absolute expiry, optional separate "revoke all API tokens" / "revoke all RSS tokens"), email add/change/resend (current password for a change; the previous verified address stays until the new one is confirmed; notice to the old address), API and RSS tokens (create with label and 30/90-day expiry, shown once, ready-to-copy v2 RSS URL, list with last use, legacy RSS tokens masked, revoke), invitations (list, copy link, revoke; no creation).
* **Public pages**: `/register/{code}` (one generic message for unknown, used, revoked, expired and suspended-owner invitations), `/password/setup/{token}`, `/password/reset/{token}` (confirmation page names API tokens and RSS tokens/URLs separately), `/password/forgot`, `/email/verify/{token}`. Setup and reset never reactivate an account and do not consume a disabled owner's link.
* **Mail**: strict-TLS sender (implicit TLS or STARTTLS; no plaintext, no fallback, no verification bypass), bounded non-durable queue (32 slots, 2 workers, 30 s send bound, 15 min job age limit, drain at shutdown), plain-text templates.
* **API v2** (`Authorization: Bearer mgt_…` only): `GET /api/v2/torrents` (search, keyset cursors, `limit` 1–100, exact `info_hash`), `/torrents/{id}` (`magnet_uri`, `full_tree_available`), `/torrents/{id}/files/tree`, `/torrents/{id}/files?dir=&offset=&limit=`.
* **Feeds**: `GET /api/v1/feed/rss/last` (deprecated; RSS token in `api-token` header, cookie or query; legacy envelope) and `GET /api/v2/feeds/latest.rss?feed_token=` (problem+json).
* **Magnet**: `GET /magnet/{infohash}` (307, trackers in stored order, no `dn`, no database access); details page "Copy short link"; magnets everywhere carry the trackers.
* **Documentation**: `/api`, `/api/v1`, `/api/v2`, `/api/v1/openapi.yaml`, `/api/v2/openapi.yaml`.

## 3. Schema and configuration changes

* **Database schema: none.** `magnetico_web`, its structural verification, the roles and the documented grants are unchanged (the integration test runs everything under the documented grants).
* **Configuration: none new.** `INVITE_QUOTA`, `TRACKER_CACHE_TTL` and the `SMTP_*` variables, validated since W1, are now used. `SSL_CERT_FILE` (a Go/OS variable, not a service option) is how a private SMTP CA is trusted.
* **Dependencies:** `go.mod` gains `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3` (**test-only**, with its dependency `github.com/dlclark/regexp2 v1.11.0` in `go.sum`; correction to the plan, which said it had no transitive dependencies). `internal/web/boundary_test.go` fails if either appears in the dependency graph of any shipped package. Vendored static assets: `swagger-ui-bundle.js` and `swagger-ui.css` (5.34.1, Apache-2.0) with licence files under `third_party/`. No other runtime dependency; SMTP is standard library only.
* **API/server interfaces:** `Server.SetBackground` → `AddBackground`; new access classes `APIToken` and `Feed`; `Router.SetGuard`; problem slug `rate-limited`; `persistence.Reader.LatestTorrents`; `torrents.FileAPI` (the shared JSON file-view adapter) and `torrents.TrackerList`.

## 4. Authentication, CSRF and credential-separation test results

All run with `-race` against a disposable PostgreSQL unless noted. Final run of the whole safe set: `go test -race -count=1 ./codec/... ./persistence/... ./internal/web/... ./internal/pgtest` — **all packages passed**; `go test -count=1 -run TestWeb .` (W1–W5 real-binary regression plus the two 07G binary tests) — **passed**. `gofmt`, `go build ./...` and `go vet ./...` clean.

| Requirement | Evidence (test) |
|---|---|
| CSRF on every state-changing browser action | `account.TestEveryPostRouteRequiresCSRF` iterates **every registered POST route**, with a missing and a wrong token, signed in and not (all `403`, nothing changed); `server` access-class tests keep `POST` out of `Public`/`APIToken`/`Feed` |
| Registration and invitation races | `account.TestConcurrentRegistrationsWithOneInvitationHaveOneWinner`; `accounts` registration-vs-disable races (existing) extended by owner-scoped revoke tests; generic message for all five unusable states (`TestEveryUnusableInvitationLooksTheSame`) |
| Password change/reset and revocations | `accounts.TestChangePassword*` (keep by default, separate options, rotated session keeps its expiry, stale hash, disabled account), `account.TestChangePassword*`, `TestPasswordLinksCompleteAndRevokeEverythingWithBothNotices` (sessions, API, RSS and legacy RSS all revoked; both notices on the page and the login notice) |
| Disable/enable vs permanent revocation | `accounts.TestDisableEnableLeavesTokenRowsByteIdentical`; `apiv2.TestRevocationExpiryAndDisableTakeEffectOnTheNextRequest`, `TestConcurrentDisableVersusRequests` (`-race`); `feed.TestRevocationOwnerStateAndExpiryApplyBeforeTheCache`, `TestMigratedLegacyTokenSurvivesDisableEnableAndDiesOnReset` |
| Email verification, recovery, no enumeration | `accounts.TestEmailVerification*`, **`TestConcurrentVerificationOfTheSameAddress`** (12 rounds, exactly one winner, loser's link not consumed, retry still `ErrEmailUnavailable`), `TestEmailVerifyUniquenessConflictRollsBackAndKeepsLink`; `account.TestForgotPasswordIsIndistinguishableAndDoesNoWorkInTheRequest` (identical status, body and headers for valid, unverified, disabled, unknown, malformed; **zero store calls in the request**), `TestForgotPasswordFullFlowMailsOnlyTheVerifiedActiveAccount`, `TestEmailVerificationPagesAndDuplicateAddress` (duplicate looks like an invalid link) |
| API vs RSS credential separation | `apiv2.TestBearerIsTheOnlyCredentialAndFailuresAreIdentical` (18 non-credentials incl. session cookie, RSS token, legacy token, query/cookie/other headers, duplicated header: all `401`, identical bodies); `feed.TestCredentialPoliciesAreIndependent` (28 transport × credential cases, both failure envelopes, never mixed); the binary test repeats the key cases over real HTTP |
| Revocation on the next request | as above; `feed` checks the token **before** the page cache with a warm cache |
| Log redaction | `account.TestLogsContainNoSecrets`, `apiv2.TestLogsCarryNoTokensNamesOrQueries`, `feed.TestLogsCarryNoTokensOrQueries`, `mail` queue test (no address, link or panic text), and the binary test scans the real process log for passwords, tokens, invitation codes, links, emails and the torrent name |
| Headers | `account` (no-referrer/no-store/noindex on every tab), `feed.TestEveryResponseCarriesTheCredentialSafeHeaders`, `apidocs` (noindex, nosniff, CSP, no cookies, ETag/304) |

## 5. SMTP and RSS compatibility test results

* **Fake SMTP server** (in-process, test CA, `mail` package): STARTTLS delivery with the exact command order (`AUTH` and `MAIL` only after TLS), implicit TLS, AUTH LOGIN when only LOGIN is offered, no `AUTH` without credentials, **missing STARTTLS on loopback: nothing beyond `EHLO` sent**, implicit TLS against a plaintext server, **wrong host name and untrusted CA in both modes: no AUTH/MAIL/DATA**, system roots by default, `SSL_CERT_FILE` honoured, **header injection** (CR/LF/NUL in subject, recipient, sender; display names and address lists refused **before dialing**), stalled server bounded by the send timeout, errors carry a category only, source test forbids the verification-bypass option and the one-call standard sender anywhere under `internal/`.
* **Real binary + local SMTP sink**: verification, forgot-password and change-notice mails received, only the verified active account is mailed, every `AUTH` observed inside TLS (the binary trusts the sink's CA through `SSL_CERT_FILE`).
* **RSS**: structural golden test against `internal/web/feed/testdata/feed-golden.xml` (element set, `guid`, `description`, `link`=`enclosure url`, enclosure `length`/`type`, `comments`, dates, namespace, self link without query), identical items on v1 and v2, well-formedness with hostile names, page order and boundaries (1,005 rows: page 1 = 1,000 newest, page 2 = 5; `page=0/abc/-3` → 1; `page=11/99999` → 10), legacy fixture tokens (inserted as SQL as W8 would) on query/header/cookie, disabled/re-enabled with a byte-identical row, killed by a reset. **Not possible:** a byte-level comparison with the old feed (its generator library is not available here); the structure follows spec §8.2.1. Review fixes: the channel carries the legacy author as both `<author>Magnetico Web</author>` and `<dc:creator>Magnetico Web</dc:creator>` (verified by the owner against laminas-feed 2.26.3; asserted explicitly and in the golden); the legacy `setDateModified()` renders as the item `<pubDate>`, already emitted. Password recovery mail goes to the stored verified spelling of the address, never the submitted one.

## 6. API / OpenAPI contract validation

* `apiv2.TestFileViewParityWithTheUIEndpoints`: **bytes of every API file-view response equal the session endpoint's** for five shapes (odd names/duplicates, 100 files, 400 files over the tree limit, 300-deep hierarchy, single file) × 16 parameter sets (the only normalised difference is the `alternate` URL). Plus `TestSmallTorrentUsableThroughEitherEndpointAndLargeOneRefusesTheTree`, `TestExactSizeCapAndIndexComplexityOverTheAPI`, `TestNamesAreByteExactOverTheAPI`, `TestDetailsContract` (`full_tree_available` agrees with the tree endpoint), list/cursor/`info_hash`/parameter validation (21 invalid forms), search failures mapped to problem+json.
* `apidocs.TestEmbeddedDocumentsAreValidOpenAPI32`: both documents validate against the **official OpenAPI 3.2 schema** (`schema-base`, `schema`, `dialect`, `meta`, vendored, loaded offline; the loader refuses any other URL); `TestTheValidatorRejectsAnInvalidDocument` proves the check is real.
* `apidocs.TestRoutesAndDocumentsDescribeEachOtherExactly`: every implemented `/api/v1` and `/api/v2` route is documented and every documented operation exists, both directions, built from the router of a server that installs all three API packages.
* `apiv2.TestResponsesMatchTheDocumentedSchemas`: real responses (list, details, tree, every directory page of two torrents at three page sizes, problem documents) validate against the component schemas, and the schemas reject typical mistakes (found and fixed one: a string `name` must forbid `name_base64`).
* v1 describes only the RSS endpoint (`TestV1DescribesOnlyTheRSSEndpointAndItsAuthentication`); v2 documents the file-view schemas, `full_tree_available`, `next_offset` and the separate `feed_token` scheme.

## 7. Dependencies and downloads (all approved by the owner)

| What | Source | Verification |
|---|---|---|
| `swagger-ui-dist@5.34.1` (newest release older than two weeks on 2026-11-23; published 2026-10-30) | `registry.npmjs.org` | npm `sha512` integrity matched; SHA-256 and per-file hashes in `third_party/swagger-ui-dist-5.34.1/VENDOR.md`; a test checks the vendored files against them; `npm install` never run (its `@scarf/scarf` install-time helper is absent) |
| `github.com/santhosh-tekuri/jsonschema/v6@v6.0.3` | Go module proxy | `go.sum`, `go mod verify` |
| OpenAPI 3.2 `schema-base`, `schema`, `dialect`, `meta` (`2025-09-17`) | `spec.openapis.org` | SHA-256 recorded in `testdata/oas/README.md` and checked by a test |

Nothing is fetched at build, test or run time, and the served pages load nothing from another origin (asserted by test). **Renderer evaluation**: licence Apache-2.0 (compatible with the AGPL project; notices kept); 1.68 MB script + 183 KB stylesheet; supports OpenAPI up to 3.2.0; no `eval(`/`document.write`; `Function(...)` only in short-circuited global-object and `bind` fallbacks; no telemetry; the default external validator address is disabled; the stylesheet needs `data:` images (hence the only CSP relaxation, `img-src 'self' data:`, on `/api/v1` and `/api/v2`). Details: `VENDOR.md`.

## 8. Security and performance measurements

Synthetic data, one local PostgreSQL on the development VM, in-process HTTP (no network latency), indicative only:

| Measurement | Result |
|---|---|
| Feed page of 1,000 items | 967,565 bytes; cold (token check + read + render) 8.5 ms; cached p50 0.8 ms, p95 2.2 ms; rendering alone 5.2 ms (4.7 MB and 17,932 allocations per render) |
| API v2 latency (200 requests each, 5,000 torrents, token lookup included) | details p50 0.65 ms / p95 0.86 ms; list(50) 0.47 / 0.68 ms; search 0.49 / 0.76 ms; directory page 0.37 / 0.54 ms; `401` without token 7.7 µs |
| `GET /magnet/{hash}` (12 trackers, no database) | 4.0 µs and 54 allocations per request, 8 parallel workers |
| Forgot-password handler by account class (300 requests each) | valid 20.6 µs, unverified 17.6 µs, disabled 17.6 µs, unknown 17.4 µs (p50); the test fails if the spread exceeds 4× + 2 ms |
| Mail queue flood (1,000 reservations, capacity 32, stalled workers) | 32 accepted, 968 rejected in 12 µs total; all 32 delivered after release |

Security properties checked by test beyond the table above: constant-shape recovery, tokens stored only as SHA-256 (the binary test searches both token tables for the plaintext), tokens never in documents, logs or Referer, uniform `401` bodies for every non-credential, failure/use rate limits (`429` with `Retry-After`), the file-view limits unchanged for API clients.

## 9. Deliberate deviations from the draft specification

All recorded with their reasons in `web-v1.md` §14. In short: no self-service invitation creation (owner decision); per-type cap of 50 unrevoked tokens and a per-account creation rate limit, **no lifetime cap on revoked rows** (owner decision); the legacy 401 envelope for missing and invalid v1 tokens alike; v2 feed failures are problem+json; `rate-limited` problem type; API responses `private, no-store`; strict API parameter validation (400 instead of silent fallback, except unknown `sort`/`order`); tabs via `?tab=`; no automatic sign-in after registration, setup or reset; rotated session keeps the old absolute expiry; `/password/forgot` stays registered without SMTP; the change notice to the previous address is implemented; OpenAPI 3.2.0; the docs CSP adds `img-src 'self' data:`; magnet encoding is Go's form encoding (`~` is not escaped, unlike PHP's `urlencode`); XML-illegal characters in feed titles become U+FFFD; a per-route header-override hook (planned) turned out unnecessary because handlers may set their own CSP.

## 10. Remaining known issues

1. **No browser verification** of any new page or of the Swagger UI under the CSP (see the top of this report). If the renderer reports a `style-src` or `script-src` violation in a real browser, the answer must not be `unsafe-inline`/`unsafe-eval`; report it.
2. Revoked token rows are never deleted (no `DELETE` grant); growth is rate-limited only. A future operator command could prune them.
3. Rate limits and the mail queue are per process (single instance assumed); queued mail is lost on restart.
4. The v1 feed's exact legacy bytes could not be compared; only structure is guarded.
5. `Disable` does not touch the account's one-time email links, by design; a link for a disabled owner simply cannot be completed until `enable`.
6. The per-IP limiters (registration, link completion, forgot-password, API and feed failures) use the address resolved from `trustedProxies`; behind a proxy without it all clients share one bucket (deployment guide §2).
7. Files list of `go.mod`: `golang.org/x/term` is used directly but still marked `// indirect` (pre-existing, left alone).
