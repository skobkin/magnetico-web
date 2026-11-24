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

## 4. Pending

Coverage matrix, security findings of S4, measurements (S5, S6), browser checks (S7), verification results (S9), limitations, RC items and the commit list.
