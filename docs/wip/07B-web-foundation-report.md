# Task 07B / W1 — web service foundation: implementation report

Status: **implemented; accepted by the owner with the corrections of §7 (applied).** Milestone W1 of `docs/wip/07A-web-design-plan.md` §9 against `docs/specs/web-v1.md` (still DRAFT). Starting commit `085af7ed`. W2 and later milestones have **not** been started.

## 1. Scope delivered

| Area | Where |
|---|---|
| `web` command dispatch (the stub is gone) and `WebOptions` | `main.go` (`runWeb`), `opflags/web.go`, `opflags/{opflags,config,parsers}.go` |
| HTTP server lifecycle, per-route deadlines, middleware, foundation routes | `internal/web/server/` |
| Embedded templates (generic error page), content-hashed static assets | `internal/web/ui/` |
| Production database backend: bounded pool, startup verification, readiness ping | `internal/web/webdb/` |
| Additive export of the existing read-only torrent-schema verification over a caller-owned pool | `persistence.VerifySchema` (`persistence/postgres.go`; `verifyStartup` now calls it, behaviour unchanged) |
| Web-package import-boundary test | `internal/web/boundary_test.go` |
| Real-binary tests | `main_web_test.go` |

Routes: `GET /healthz`, `GET /readyz`, `GET /robots.txt`, `GET /static/{hashed-file}`. Nothing else is routed, so no data or placeholder page can be reached (unknown paths are `404`).

Not touched: codec, crawler, ingest, migration behaviour, schemas, dependencies (`go.mod`/`go.sum` unchanged), CI, images, `master`.

## 2. Behaviour and decisions

**Configuration (spec §2.3).** Every §2.3 variable has a flag and a `MAGNETICO_WEB_*` variable; precedence CLI > ENV > defaults; empty ENV means unset. Validation covers the pool (4–200), search concurrency (≤ pool − 2), session ordering (idle ≤ absolute ≤ remember ≤ 8760h), the search/request/tracker-TTL bounds, invite quota, SMTP completeness (no plaintext mode; username and password together; valid sender), log level/format and trusted-proxy CIDRs. Session, tracker, invitation and SMTP values are validated only; nothing consumes them in W1. Error text names the option and never echoes a value.

- **Global YAML file.** See §7.1: the file is supported through the existing mechanism (the original W1 refusal was replaced).
- **Secrets.** The SMTP password is environment-only (no flag exists). A database URL **containing a password is refused on `--database`** (it would show in `ps`); use `MAGNETICO_WEB_DATABASE`. The default database URL is the same development default as `ingest`. Nothing logs a URL: `main.go` prints only scheme/host/path (`redactDatabaseURL`) and a controlled error class.
- **Public URL.** Required; `https://`, or `http://` to a loopback host (development mode); no credentials, query, fragment or path prefix. Development mode additionally **requires a loopback listen address**.
- **Added setting.** `--shutdown-timeout` / `MAGNETICO_WEB_SHUTDOWN_TIMEOUT` (default `30s`, 1s–10m). §2.5 requires a bounded grace period but §2.3 does not name the variable.

**Middleware (outermost first):** in-flight counter, request ID, access log, panic recovery, security/indexing headers, client-IP resolution, cross-origin protection, body limit (1 MiB), then the mux; each route additionally wraps its deadline class.
- *Deviation from spec §3.1 ordering:* the access log wraps the recoverer, so a recovered panic is logged with its final `500`. Headers are set before the handler runs, so recovered and error responses still carry them.
- *Request ID* is always generated (16 hex); an incoming `X-Request-ID` is never trusted into logs. It is returned as a response header and shown on the error page.
- *Access log* records request ID, method, **route pattern**, status, bytes, duration and resolved client IP. It never records the path, query, `Authorization`, `Cookie` or `Referer`, so token-bearing URLs (`feed_token`, `api-token`) and code-bearing path segments cannot reach it. `/healthz` and `/readyz` log at debug (5xx at error).
- *Panics* produce a generic page with the request ID; the log carries only the panic **type** and stack, never the value.
- *Headers on every response class* (200, redirects, 404/405, 403, 500, static, `robots.txt`): `X-Robots-Tag: noindex, nofollow`, `X-Content-Type-Options`, `Referrer-Policy: same-origin`, CSP (`default-src 'self'`, `frame-ancestors 'none'`, `base-uri 'none'`, `form-action 'self'`, `object-src 'none'`), `Permissions-Policy`, COOP, `Cache-Control: no-store` (static assets override with `immutable`), HSTS only for an `https://` public URL.
- *Cross-origin protection* is `http.CrossOriginProtection` with the public origin added as trusted (a proxy may rewrite `Host`). Non-browser clients without `Origin`/`Sec-Fetch-Site` pass. The synchronizer token and pre-session CSRF cookie belong to W3.

**Trusted proxies (spec §3.2).** Forwarding data is used only when the direct TCP peer is inside `--trusted-proxies`. The chain is walked right-to-left skipping trusted hops; the first untrusted address is the client. A malformed or zoned entry, an over-long chain (>64 hops) or an all-trusted chain falls back to the **peer address**, so garbage can only make clients share a bucket, never impersonate another. IPv4-mapped IPv6 addresses are normalized. **Only `X-Forwarded-For` is read in W1**; `Forwarded`, `X-Real-IP` and `X-Forwarded-Proto/Host` are ignored (absolute URLs come from the public URL anyway). `Forwarded` is deliberately unsupported (owner decision, §7.2).

**Deadlines (spec §2.9).** `http.Server` has `ReadHeaderTimeout` 10 s, `ReadTimeout` 60 s, `IdleTimeout` 120 s, 16 KiB headers and **no `WriteTimeout`**, no `TimeoutHandler`. `Router.Handle(pattern, kind, handler)` applies, per route, a context deadline and a `ResponseController` write deadline: search class = `SEARCH_TIMEOUT` (default 60 s) + 10 s margin; ordinary class = `REQUEST_TIMEOUT` + a **5 s margin** (a small local addition, so a handler whose context just expired can still write its error response; §2.9 says exactly `REQUEST_TIMEOUT`). No search handler exists yet.
- *Operational requirement (not verified):* reverse-proxy response and idle timeouts must exceed the search write deadline (**70 s with defaults**). The server logs this at startup. Whether Traefik's defaults for the deployed version satisfy it was **not checked**.

**Health.** `/healthz` always `200 ok`. `/readyz` is `200 ready` only if the process is not draining and `Ping` succeeds within 2 s; otherwise `503 not ready`. Bodies carry no version, count, configuration or database detail; the failure reason goes to the log as an error type only.

**Startup verification.** `webdb.Backend.Verify` runs `persistence.VerifySchema` (exact torrent schema objects and version, `pg_trgm`, no legacy shape) and then requires the `magnetico_web` schema and its `schema_version` table. It never creates, alters or migrates anything and fails **before the listener opens**. Exit status 1, with the fixed message that the `magnetico_web` schema is missing.

**Shutdown.** SIGINT/SIGTERM: `/readyz` turns `503`, the listener stops, in-flight requests get `--shutdown-timeout`; then request contexts are cancelled, connections closed, in-flight handlers awaited ≤ 2 s, the pool closed (≤ 5 s). Clean stop exits 0; any forced step returns `ErrShutdownIncomplete` and exit status **3** (the `crawl`/`ingest` convention).

**Static/templates.** `html/template` pages are parsed with a shared layout, rendered to a buffer first (a template error never emits a half page). Assets are served only under content-hashed names (`/static/app.<12hex>.css`, `immutable`); the un-hashed name is `404`. The W1 stylesheet and error page are placeholders; the design belongs to W5.

**Import boundary.** The test lists `go list -deps ./...` **inside `internal/web`** and rejects `internal/crawler`, `internal/crawlqueue`, `internal/ingest`, `modernc.org/sqlite` and, additionally, `dht` (web code may never speak BitTorrent). It deliberately does not inspect the unified executable, whose `main` package links the crawler and ingest by design. Spec §1.2 and plan §9 were clarified accordingly. No binary split or build tags were introduced.

## 3. Tests run (this session, after the final code change)

Environment: the shared `internal/pgtest` harness (a disposable PostgreSQL; `MAGNETICO_TEST_POSTGRES_DSN` unset, so Docker or local binaries were used, never a production server). All web tests bind loopback only.

| Command | Result |
|---|---|
| `go build ./...` | OK |
| `go vet ./...` | OK |
| `go test -count=1 -race ./internal/web/... ./opflags` | OK (5 packages) |
| `go test -count=1 ./persistence` | OK |
| `go test -count=1 -race -run TestWebBinary .` | OK; 3 passed, 1 skipped (below) |
| `go test -count=1 .` (whole root package, including the pre-existing migration smoke tests) | OK (66 s) |

Packages **not run** (unchanged by W1): `dht/...`, `internal/crawler`, `internal/crawlqueue`, `internal/ingest/...`, `internal/migration/...`, `internal/e2e/...`, `codec`, `metainfo`, `stats`. I read them for public-network risk first: the `dht` tests use loopback addresses only, but running them was unnecessary and the `e2e` suite spawns crawler binaries, so they were left alone on this VM. **Full `go test ./...` was therefore not run**; no claim of full-repository coverage is made.

Covered by the new tests: CLI parsing and precedence; invalid values and unknown variables; strict YAML configuration and its precedence (§7); public-URL and strict development-mode rules; database-password-on-CLI refusal; SMTP validation; secret redaction (log never contains query tokens, cookies, `Authorization`, `Referer`, invite codes, panic values; configuration errors never echo values); trusted-proxy spoofing, multi-hop, malformed and IPv6 forwarding; middleware order, panic recovery (also after a partial write, and `ErrAbortHandler`); security and robots headers on every response class; health/readiness statuses and absence of detail; startup-verification failure cases against real PostgreSQL (fresh, schema absent, version table absent, wrong torrent version, success with a **test-local stand-in**) and the bound on the pool; per-route deadlines, including proof that a search-class route outlives an ordinary route's write deadline; ordinary and timeout-driven graceful shutdown; real-binary fail-closed startup, serve + SIGTERM, bad configuration; import boundaries.

## 4. Skipped or untested, and known limitations

- **Skipped:** `TestWebBinaryStartsWithProductionWebSchema` — blocked on W2 (the approved DDL and its explicit initialization command do not exist). The success-path binary test (`TestWebBinaryServesAndStopsOnSIGTERM`) and `webdb`'s success case use a **stand-in** `magnetico_web.schema_version` table created inside a disposable test database only. No production code creates it, and the production verifier was not weakened for it.
- **Until W2, `magnetico web` cannot start against any real database** (fail closed, by design).
- The web-schema verification checks presence only (schema + `schema_version` table); the exact version/row check and privilege check (§2.4) are W2 work because they depend on the unwritten DDL.
- Not tested: the `listen` failure through the binary, HSTS behind a real TLS proxy, any real reverse proxy. (Exit status 3 and the backend-close bound are now covered, §7.7.)
- No performance measurements, load tests or Traefik timeout verification were done.
- No SMTP connection, session, CSRF token, rate-limit or authentication code exists (W3/W6).

## 5. Dependencies on W2

The `magnetico_web` schema and version table (and its explicit, non-destructive, idempotent init command); the exact schema-version and privilege verification in `webdb.Verify`; replacement of the stand-in in the two tests and enabling the skipped test; the tracker seed (for the tracker cache settings validated here).

## 6. Owner decisions requested in the first review

All were answered in the review; the outcomes are in §7. None is open.

## 7. Review corrections (follow-up commit)

1. **Optional YAML configuration.** `--config-file-path` / `MAGNETICO_CONFIG_FILE_PATH` now work for `web` through the existing `goccy/go-yaml` strict decoding (no new library). Precedence **CLI > ENV > YAML > defaults**. Settings live in a nested `web:` section with camelCase keys (full table in `docs/web-deployment.md`), so they cannot collide with existing flat keys; the section is accepted by the other commands and files without it are unchanged. Unknown `web.*` keys, wrong types, bad values and out-of-range values are rejected without echoing the value; `smtpPassword` has no key; a `databaseURL` containing a password is refused (`MAGNETICO_WEB_DATABASE` stays the place for it, and the CLI refusal remains). A missing file is an error. ENV remains the recommended container deployment.
2. **Proxy compatibility.** `X-Forwarded-For` only, unchanged logic. Spec §3.2 now says `Forwarded`, `X-Real-IP`, `X-Forwarded-Proto/Host` are not read; operator guidance on choosing trusted proxies is in `docs/web-deployment.md` §2.
3. **Deadlines.** Spec §2.9 now records handler context = `REQUEST_TIMEOUT` / `SEARCH_TIMEOUT` and write deadline = +5 s / +10 s. New test: with a 150 ms request timeout and a 2 s margin, the context is cancelled at ~150 ms (not 150 ms + margin) and the 503 written after cancellation reaches the client.
4. **Middleware order.** Kept (access log wraps recovery); spec §3.1 updated.
5. **`MAGNETICO_WEB_SHUTDOWN_TIMEOUT`.** Kept; added to spec §2.3 and the configuration documentation.
6. **Reverse-proxy timeouts.** `docs/web-deployment.md` §3: the search write deadline is 70 s with defaults; at least 90 s recommended for upstream response/idle timeouts; Traefik (default), Nginx and Caddy notes. **Proxy option names are from memory of upstream documentation and no proxy default was verified**; the page says so.
7. **Shutdown tests.** New real-binary test `TestWebBinaryExitsWithStatus3WhenGraceExpires` (a half-sent request keeps the server busy; with a 1 s grace the process exits **3** and logs the incomplete shutdown) and unit test `TestBackendCloseIsBounded` (a `Close` that never returns no longer blocks shutdown; `ErrShutdownIncomplete`). The close bound is now a field (`closeWait`, default 5 s) so it is testable.

Tests after the corrections: see §3 table below (re-run).
