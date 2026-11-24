# Task 09 — Ingestion Statistics and Read-Only Administration UI: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** plan for owner review (2026-11-24). **No implementation has started.** Nothing in this plan authorises a tag, a push, a publication, a production database or a live-DHT run.

**Goal:** First operational-observability slice for 0.8.0: a stable crawler identity, bounded in-memory ingest instrumentation, a periodic writer into a new versioned PostgreSQL statistics schema (hourly/daily/monthly aggregates, a current-state snapshot, a bounded crawler registry), a cached read model, and a read-only administrator section (dashboard with five uPlot chart groups, crawlers, quarantine, users, invitations, trackers).

**Architecture:** `crawler → (X-Magnetico-Crawler-ID) → ingest` records events into lock-free in-process accumulators (no I/O on the request or consumer path). One background writer per ingest process swaps the accumulators on a tick and, in one transaction, adds the deltas into `magnetico_stats` (additive upserts, so several writers cannot corrupt aggregates; a per-process sequence number resolves ambiguous commits). `magnetico web` reads that schema through a coalescing, stale-while-error cache and renders admin pages server-side; charts are vendored uPlot driven by first-party vanilla JS. Single binary, separate deployments, no new infrastructure.

**Tech stack:** Go ≥ 1.27.4 (standard library, `pgx/v5` already in use, `github.com/google/uuid` **not** added: UUIDv4 from `crypto/rand`), PostgreSQL 17, html/template, vendored uPlot (MIT).

**Specs read (binding authority, in this order):** this task's brief; `AGENTS.md`; `docs/engineering-standards.md`; `docs/specs/crawler-ingest-delivery-v1.md` (§4–§8); `docs/specs/web-v1.md` (§2, §4, §5, §6); `docs/specs/postgresql-persistence-v2.md`; `docs/wip/07J-*` (accepted); `docs/wip/08-ci-oci-report.md`; `docs/wip/rc0-acceptance-checklist.md`. Forgejo: #48 (Admin UI proposal), #50 (statistics collector), #49 (invitation graph, deferred). Design ideas reused from `skobkin/meshmap-lite` (`internal/stats` job shape: catch-up on start, retry delay, write-time gating, UTC calendar buckets; `internal/api/http/cache.go`: typed TTL cache invalidated by the writer).

## What was inspected (facts the design rests on)

| Fact | Where | Consequence |
|---|---|---|
| `Queue.Admit` returns `AdmitResult{Inserted, Duplicates}` from `RowsAffected()` of one `INSERT … ON CONFLICT DO NOTHING`; the 202 follows `COMMIT` | `internal/ingest/pgqueue/queue.go` | "Enqueued" and "duplicate" are exact at the commit boundary; bytes of inserted rows need `RETURNING payload_bytes` (small additive change) |
| The consumer's `AddTorrent` returns `created bool`; counters are incremented **after** the queue-row `DELETE` succeeded; a failure of that `DELETE` re-runs the row and the retry reports `already` | `consumer.go` `processRow` | New-torrent counts can under-report by the number of ambiguous `DELETE` failures (documented, not "fixed") |
| Existing counters: `server.Counters`, `existence.Stats` (cache hits only, aggregated), `cache.Stats` (hits/misses not split by positive/negative), `pgqueue.ConsumerStats` | `internal/ingest/*` | Reuse; split cache hits into positive/negative (additive) |
| `usage.used_bytes` is exact and trigger-maintained; `queue` PK is `id` (identity), `quarantine` is append-only until an operator acts; no index on `received_at` | `pgqueue/schema.go` | Queue/quarantine bytes and counts need a bounded `count(*)`/`sum()`; oldest age = `ORDER BY id LIMIT 1` (PK index) |
| Ingest has **no** trusted-proxy setting; it logs no client IP | `opflags/ingest.go`, `server.go` | Add `MAGNETICO_INGEST_TRUSTED_PROXIES`; extract the web service's `ResolveClientIP` into a shared package |
| Crawler queue directory is exclusive through `flock` on `LOCK`; `queue.db` has `user_version=1`; `Check` is a read-only diagnostic that takes the lock | `internal/crawlqueue` | Identity file next to `queue.db`, created inside `Open` after the lock; `Check` never creates |
| Crawler client sends one header set in `decorate()` for both operations; the sender quarantines only the three recognised record-specific codes, everything else on 400/413 is "unrecognised → keep, back off 60 s" | `internal/crawler/client.go`, `sender.go` | A new non-record-specific code plus a dedicated systemic outcome |
| Web router has access classes `Public`, `PublicForm`, `Session`, `SessionJSON`, `APIToken`, `Feed`; `Principal.Role` comes from the same lookup that checks `active` | `internal/web/server/server.go`, `auth/guards.go` | Add `Admin` and `AdminJSON` classes; role changes and disabling take effect on the next request |
| Web CSP is `default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'` (no inline script or style attribute) | `server/middleware.go` | Charts must set styles through the CSSOM only; no inline `<script>`/`style=` |
| `internal/web/**` must not import `internal/ingest|crawler|crawlqueue` (boundary test) | `internal/web/boundary_test.go` | The shared statistics model lives in a neutral package `internal/opstats` |
| The web DB role has `SELECT` on `magnetico_web.users` (table-level) and `invites`; **no** access to `magnetico_ingest` | `webschema/privileges.go` | Admin queries select explicit safe columns; the quarantine viewer needs column-level grants |
| The repository already has an upstream `stats` package (Prometheus, Pyroscope) used by the DHT code | `stats/`, `dht/…` | Untouched; the new package is `internal/opstats`; the crawler's DHT path gets no statistics code |
| Swagger UI is vendored unmodified with a checksum test | `internal/web/ui/static`, `apidocs_test.go` | Same practice for uPlot |

## Global Constraints

Copied from the brief (every task includes them):

- Use the existing PostgreSQL deployment; no Redis, TimescaleDB, Prometheus, external metrics service or other required infrastructure. **No new externally exposed metrics endpoint.**
- **No new database connection from the crawler.** No database writes per request or per processed torrent solely for statistics. No persistent event log. No persistent association of a crawler identity with a torrent or info hash. No torrent names, file paths, metadata payloads, credentials or request bodies in statistics.
- Bounded memory, predictable database growth, bounded query cost. Statistics failures must not interrupt ingestion, change admission decisions, compromise durability or prevent crawler delivery. No new dependency in the crawler's critical DHT path.
- Versioned statistics schema or explicitly isolated extension; explicit initialisation and verification; **no automatic destructive migration**; the default shared PostgreSQL account deployment stays supported.
- Crawler identity: UUIDv4 generated on first normal start, persisted in the existing durable queue directory; header `X-Magnetico-Crawler-ID` on **both** ingest operations; strict validation; atomic crash-safe creation under the queue lock; never silently replace a corrupt identity; read-only diagnostics never create or mutate; **no compatibility fallback for unidentified crawlers**; ingest rejects missing, malformed or ambiguous identifiers on both endpoints; identity failures are systemic, never quarantine; metadata JSON unchanged; identity is not authentication (the shared token remains it).
- UTC for storage and bucket boundaries; hourly, daily and monthly aggregates kept **indefinitely** (no retention, no deletion); counts and sums for counters, sum+count for weighted averages, min/max/last for gauges, coverage information; never average averages or percentages without weights.
- Intervals: defaults as suggested in the brief (queue sampling 5–10 s, snapshot 10 s, aggregate persistence 60 s, storage sampling 5 min, web snapshot cache ≈ 10 s, chart cache ≈ 60 s); few knobs.
- Admin UI is server-rendered, custom CSS, vanilla JS; `Admin` link next to the account link, administrators only; every route, JSON endpoint and data-access function enforces the role server-side; read-only (no mutation, unsupported methods refused); no password or credential hashes, session secrets or raw tokens; unused invitation codes are never shown; quarantine payloads are never shown.
- uPlot vendored (JS, CSS, licence, version, checksum) like Swagger UI; no CDN, no framework, no Node build; compatible with the existing strict CSP; light and dark themes; no-data and error states; textual summaries. Live updating is deferred to 0.9.0.
- Go ≥ 1.27.4; all of Task 08's gates stay green (`golangci-lint` 0 issues, `go vet` with tags, the skip census, `internal/repocheck`); no release build passes `-tags`; production code exports nothing only for tests.
- No live DHT, no production database, no tags, pushes or publication. Commit as the configured git identity with the `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>` trailer.
- If a normative specification and this task disagree, the owner decides: see **Owner-directed amendments** below (the crawler-ingest spec is amended by the brief itself).

## Owner-directed amendments to normative text (recorded, not silent)

1. `crawler-ingest-delivery-v1.md` §5 gains the mandatory header and the error code `invalid_crawler_id`; §4 gains the identity file; §10 gains the trusted-proxy variable and the statistics kill switch. The protocol stays "v1" (the spec is a draft, 0.8.0 is unreleased, and the owner refuses a fallback). Applied in Task 2.
2. `web-v1.md` §4.2.3 says "`admin` grants no web capability (the Admin UI is a future release)"; §6.1 gains the admin routes; §2.7 gains the grants. Applied in Task 6/8/10.
3. `postgresql-persistence-v2.md` is **not** changed: the statistics schema is a separate schema, like `magnetico_ingest` and `magnetico_web`.

## Design decisions and trade-offs

Each decision lists what it costs. Rulings I make during execution are ledgered as `Ruling:`; these are made now so the owner can overturn them before code exists.

**D1. A separate schema `magnetico_stats` (version table `stats_version`), created by `magnetico migrate stats-schema-init`.** Not an extension of `magnetico_ingest`: that schema's version is "exactly {1}", its role may be DML-on-`magnetico_ingest`-only (`MAGNETICO_INGEST_QUEUE_DATABASE`), and the web role must not gain access to queue tables. Same pattern as `ingest-schema-init`: explicit, idempotent, `pg_advisory_xact_lock`, requires a verified `magneticod` v1, structural verification at ingest and web start (fail closed, no repair). *Cost:* a third schema and a third grant set; operators must run one more init command. Startup refusal when the schema is missing would make the statistics a hard dependency, so: **`ingest` and `web` start without it**, log one clear warning, and the dashboard says "statistics schema not initialised" (see D11). Verification failure on an *existing but wrong* schema also disables only the statistics.

**D2. Where events are recorded.**

| Metric | Recorded at | Definition |
|---|---|---|
| `records_received` | `handleSubmit`, after `Admit` returned nil (the 202 path) | records of 202-acknowledged requests, **including repeats**; `received = enqueued + admission_duplicates` exactly |
| `records_enqueued`, `admission_duplicates` | same, from `AdmitResult` | rows inserted / already pending |
| `bytes_body` | counting reader around `http.MaxBytesReader` on both endpoints | HTTP **request-body** bytes actually read for authenticated, identified requests; not TLS/headers/responses; labelled so |
| `bytes_accepted_payload` | `Admit` (`RETURNING payload_bytes`) | payload bytes of **inserted** rows; average record size = this ÷ `records_enqueued` |
| `requests_rejected[class]` | `writeError` call sites | nine fixed classes (below) |
| `processing_attempts` | `processRow` entry after the row was read | one per attempt |
| `processed_created`, `processed_existing` | `processRow`, after the queue row `DELETE` succeeded | exactly the existing `stored`/`already` counters |
| `processing_retries` | `retryRow` after its `UPDATE` | attempts that will be retried |
| `processing_storage_errors` | `processOwned` global backoff | deferred attempts |
| `processed_quarantined` | `quarantineRow` after success | newly quarantined (all reasons) |
| `processing_ms_sum/count` | `processRow` success (created or existing) | claim-to-delete wall time; mean = sum ÷ count |
| `exists_requests`, `exists_hashes`, `exists_found/missing/unknown` | `handleExists` | requests; hashes checked; outcome counts per hash |
| `cache_hit_positive/negative`, `cache_miss` | `existence.Service.Check` per hash (cache returns the kind) | hit rate = (pos+neg) ÷ (pos+neg+miss) |

Reject classes (fixed ordinals; a new class is a schema bump): `auth` (401), `identity` (400 `invalid_crawler_id`), `malformed` (405, 415, `invalid_request`, `invalid_batch`), `too_large` (`payload_too_large`, `record_too_large`), `invalid_record` (`invalid_record`, `unsupported_version`), `queue_full`, `storage_unavailable`, `busy`, `internal`. Requests rejected before identity validation (`auth`, method, media type) have no crawler and count only globally. Metadata records, HTTP requests, attempts and outcomes are separate counters and never mixed.

**D3. Crawler identity = a file `crawler-id` in the queue directory** (canonical lowercase UUIDv4 + `\n`, mode 0600), created inside `crawlqueue.Open` after the directory lock: temp file → `fsync` → `rename` → `fsync` of the directory. Anything else in the file (wrong length, case, version/variant nibble, trailing data) is `ErrCorruptIdentity` and **refuses to start**; a leftover temp file is ignored and removed. `Check` reads it if present and never writes. *Alternative rejected:* a row in `queue.db` would need a `user_version` bump and migration of existing queues. *Cost:* one more file to move with the volume (documented in `docs/crawler.md`). Server side: `Header.Values` must hold **exactly one** value that is canonical UUIDv4; checked after method, authentication and content type and before the body is read. Failure: `400 {"error":{"code":"invalid_crawler_id","retryable":false}}`. Client: new outcome `SubmitIdentityRejected` (checked **before** the generic 400 rule): rows kept, 60 s backoff, loud throttled log, counter; for `Exists` a typed error → fail-open precheck as today plus the same log. Never quarantine; never bisect.

**D4. Schema `magnetico_stats` v1** (fixed-width numeric columns, no JSON, no labels):

| Table | Key | Purpose / bound |
|---|---|---|
| `stats_version` | one row `{1}` | version |
| `ingest_instances` | `instance_id uuid` (random per process start) | `last_flush_seq`, `started_at`, `last_flush_at`; rows older than 30 days pruned (bookkeeping, not statistics) |
| `snapshot` | `instance_id` | current state: cumulative counters since start, windowed counts, queue/quota/oldest-age gauges, cache stats, consumer ownership; **one row per instance, updated in place every 10 s**; rows older than 24 h pruned |
| `crawlers` | `crawler_id uuid` | first/last seen, last IP (`inet`), last submit time, cumulative received/enqueued records, received bytes, exists requests/hashes, rejected requests; **cap 1,000 rows** |
| `agg_hour`, `agg_day`, `agg_month` | `bucket_start timestamptz` (UTC boundary, `CHECK` aligned) | counters (sum), weighted pairs (sum,count), gauges (sum,count,min,max,last,last_at), `observed_seconds` |
| `storage_hour`, `storage_day`, `storage_month` | `bucket_start` | eight storage measures (`last`, `max`) + sample count |

Growth: one `agg_hour` row ≈ 0.6 KB → ≈ 5 MB/year plus index; daily and monthly are negligible. The estimate for 1/5/10 years is **measured** with generated rows and `pg_total_relation_size` (Task 9), not asserted. Missing vs zero: a bucket **with no row** is a missing observation; a row with `observed_seconds > 0` and zero counters is a measured zero; `observed_seconds` below the bucket length marks a partial bucket. `CHECK` constraints keep counters ≥ 0 and `min ≤ max`; the reader rejects (and reports, not renders) rows that violate invariants.

**D5. Flush protocol (additive and idempotent).** Accumulators are fixed-size structs of atomics, **swapped** (not copied) on each tick. A tick is aligned so that no flush spans an hour boundary (an extra tick fires at each UTC hour boundary); every delta therefore belongs to exactly one hour/day/month bucket. One transaction: (1) `UPDATE ingest_instances SET last_flush_seq = $seq … WHERE instance_id=$id AND last_flush_seq = $seq-1`; (2) upsert the hour, day and month rows: `col = LEAST(col + EXCL.col, 2^62)` for counters and sums, `LEAST/GREATEST` for min/max, `last` only when `EXCL.last_at >= row.last_at`; (3) upsert crawlers. After **any** commit error the writer first reads `last_flush_seq`: equal to the attempted sequence ⇒ the commit happened, drop the delta; one lower ⇒ it did not, retry the **same** batch (merged with newer swaps, keyed by hour, at most 24 hourly deltas; older ones are dropped and counted in `stats_dropped_intervals`). Several ingest instances are therefore safe: counters are commutative, `last` is time-guarded, gauges sampled twice only double `sample_count` (means unaffected), each instance has its own snapshot row (the dashboard sums fresh rows and shows the instance count). *Cost:* ≤ 60 s of measurements are lost on `SIGKILL`; during a long outage hourly detail beyond 24 h is dropped (counted, shown).

**D6. Rates.** The snapshot carries counts for four counters (received, enqueued, processed, created) over three **completed-slot windows** from in-memory rings (60 × 1 s and 60 × 1 min): last 60 s (shown per second), last 15 min (per minute), last 60 min (per hour), each with `covered_seconds` (how long this process has observed; a rate is count ÷ covered, never ÷ nominal). The current partial hour is shown as a labelled partial count; the last **completed** hour comes from `agg_hour`. Incomplete-period counters are never presented as rates of a completed period.

**D7. Queue and storage sampling.** Every 10 s the writer reads, on its **own one-connection pool** (so a slow statistics query cannot starve admission or the consumer; same DSN as the queue, no new setting), with `statement_timeout = 3 s`: `used_bytes` (O(1)); `count(*)`/`sum(payload_bytes)` of `queue` and `quarantine`; oldest `received_at` by `ORDER BY id LIMIT 1`. If a sample takes > 2 s the interval doubles up to 60 s and the snapshot is flagged `queue_state_approx_interval`. Whether exact counts are affordable at 100 k / 1 M queued rows is **measured in Task 4**; the fallback is the planner estimate flagged approximate — **no trigger or index is added to `magnetico_ingest`** without an owner decision. Every 5 min the storage sample runs `pg_class`/`pg_namespace` catalog queries with `lock_timeout = 2 s`: heap = `pg_relation_size` of the main fork plus FSM and VM forks (included, tiny, documented); TOAST = `pg_total_relation_size(reltoastrelid)` (its own index included); indexes = `pg_indexes_size` (excludes TOAST indexes, so nothing is counted twice); schema total = Σ over relation kinds `r`,`m` of `pg_total_relation_size` + sequences; database = `pg_database_size`. Measures: torrents heap, blobs heap+TOAST, all indexes of `magneticod`, `magneticod` total, `magnetico_ingest` total, database total, indexed-torrent estimate (`n_live_tup`, else `reltuples`, else unknown; **always labelled approximate**). No payload is read. Not WAL, backups, other databases or total server disk, and the UI says so. Required privileges: catalog read and `CONNECT` only.

**D8. Instance election for DB-derived gauges:** none. Duplicate sampling by a second instance is harmless (D5). A second ingest process also starts a second consumer, which the existing advisory lock already serialises.

**D9. Web read model.** `internal/web/admin/stats.Reader` (queries) behind `Service` with: an immutable snapshot cache (TTL 10 s), a series cache keyed by `(granularity, period)` (TTL 60 s, ≤ 64 entries, evicting least recently used), `singleflight`-style coalescing of concurrent refreshes, last-known-good on refresh failure with explicit `FetchedAt`/`Stale`, and typed measurements `Measured[T]{Value T; Valid bool}` so *unavailable*, *stale* and *zero* are distinct values. Ingest availability: snapshot younger than 30 s = `up`, younger than 5 min = `stale`, older/absent = `unavailable` (never rendered as an empty queue). No user- or role-dependent data is cached (the cache holds only aggregates, and every handler checks the role before touching it).

**D10. Admin access.** New access classes `Admin` (HTML) and `AdminJSON`, session-cookie only (API and RSS tokens are not read on these routes), `GET`/`HEAD` only (registration panics otherwise); anonymous → `/login` as for `Session`; authenticated non-admin → `403`; disabled → anonymous. Data-access functions take the role-checked principal (`admin.Principal`, constructible only by the guard) so a forgotten check does not compile.

**D11. Failure behaviour.**

| Event | Behaviour |
|---|---|
| PostgreSQL outage | accumulators keep counting in fixed memory; writer retries with 10→60 s backoff; hourly deltas buffered ≤ 24 h; snapshot not updated ⇒ web shows `stale`, then `unavailable`; ingestion unaffected |
| Statistics schema missing/invalid/unwritable | one warning, writer disabled or backing off, ingestion unaffected; dashboard says so |
| Ingest restart | in-memory counters restart at 0 (new `instance_id`); persisted aggregates continue; ≤ 60 s lost on `SIGKILL` |
| Graceful shutdown | after HTTP drain and consumer stop, one final flush bounded by 5 s inside the existing shutdown budget, before pools close |
| Web restart | caches empty, nothing persisted |
| Ambiguous commit | sequence check (D5): never double-counted, never silently dropped |
| Counter overflow | saturating add at 2^62 in memory and in SQL |
| Registry full | the 1,001st crawler id is not tracked individually; its requests are valid and counted globally and in `crawlers_overflow` (shown) |

**D12. Knobs.** Exactly two new ingest variables: `MAGNETICO_INGEST_TRUSTED_PROXIES` (CIDR list, needed for a correct client IP) and `MAGNETICO_INGEST_STATS_DISABLED` (boolean kill switch, so an operator can switch the subsystem off without a downgrade). Intervals are constants. The web service gets none.

**D13. Client IP.** `ResolveClientIP` moves (behaviour and tests unchanged) to `internal/clientip`; web and ingest import it. Only the right-most-untrusted rule applies; without trusted proxies the TCP peer is the address; the stored value is the resolved address as `inet`, never a raw header.

**D14. Charts.** Five groups: (1) throughput: grouped bars received/enqueued/processed with series toggles; (2) outcomes: stacked bars created / already-existing / quarantined (retries are a separate line chart-less table column); (3) queue: depth as average with max (lines) and quota utilisation on a second, labelled axis; (4) traffic and efficiency: bytes, mean record size, cache hit rate, rejected-request trend (rate series are weighted ratios computed from sums); (5) storage: stacked area of table / file blobs / indexes with the schema and database totals as lines. Views: day (24 hourly bars), month (≤ 31 daily), year (12 monthly); previous/next navigation limited to periods that have rows; ranges are bounded server-side (≤ 31 days, ≤ 24 hours, ≤ 12 months per response). Missing buckets are `null`, drawn as gaps; partial buckets get a marker. Every chart has an adjacent `<details>` text/table summary generated with `textContent`.

**D15. uPlot.** Pin the newest stable release after checking its tag, licence (MIT) and checksum at implementation time (not asserted here); files `uPlot.iife.min.js`, `uPlot.min.css`, `LICENSE`, plus `VENDOR.md` with source URL, version and SHA-256, verified by a test like Swagger's; the vendored script must contain no `eval(`/`new Function(`. Styles are applied through the CSSOM; the real-browser verification under the strict CSP **cannot run on this VM** (07J: the headless browser refuses loopback) and goes to the RC0 checklist.

## Review Focus

Failure modes the stage tests below must pin (each has its test in the owning task):

1. **Ambiguous flush commit** (connection dies after `COMMIT` was sent): the next attempt must not double-count and must not drop a failed delta — Task 4, `TestFlushAmbiguousCommit*`.
2. **Hour/day/month/year boundary inside a flush interval**, leap day, restart within the same bucket, clock stepping backwards — Task 4.
3. **Header with two `X-Magnetico-Crawler-ID` values, an uppercase UUID, a UUIDv1, or a valid id with the wrong token** — Task 2 (401 before identity; never an identity oracle).
4. **Statistics schema dropped, revoked or locked while ingest runs** — admissions and the consumer keep working and throughput does not degrade — Task 4/9.
5. **Admin data through a non-session credential** (API token, RSS token, disabled user's old cookie, a user demoted a moment ago) — Task 6.
6. **Pagination cursors and period parameters controlled by an attacker** (huge, negative, malformed) — bounded results, no error text, no SQL — Task 6/8.
7. **Untrusted strings** (usernames, emails, tracker URLs, quarantine `detail`, IP text) in HTML and in JSON consumed by JS — Task 7/8.
8. **Crawler registry flooded** with random valid UUIDs by a token holder — memory and rows bounded, global counters still correct — Task 3/4.

## File structure (new unless marked)

- `internal/clientip/` — moved `ResolveClientIP` (+ its tests).
- `internal/opstats/` — neutral model used by ingest (writer) and web (reader): `model.go` (metric ids, reject classes, `Counters` struct), `bucket.go` (UTC hour/day/month math), `schema.go`/`init.go`/`verify.go` (DDL, `InitSchema`, `Verify`), `agg.go` (additive merge, saturating add), `privileges.go`.
- `internal/ingest/stats/` — instrumentation and writer: `recorder.go` (lock-free accumulators, rings, crawler registry), `writer.go` (tick, swap, flush protocol, pending buffer, lifecycle), `sampler_queue.go`, `sampler_storage.go`, `snapshot.go`.
- `internal/crawlqueue/identity.go` — identity file.
- `internal/web/admin/` — `guard` (principal, classes), `stats/` (reader, cache, service), `pages/` (crawlers, quarantine, users, invitations, trackers), `handlers.go`, templates `admin_*.html`, `static/admin.js`, `static/admin.css`, `static/vendor/uplot/*`.
- Modified: `internal/ingest/server/{server,service}.go`, `internal/ingest/pgqueue/{queue,consumer}.go`, `internal/ingest/existence`, `internal/ingest/cache`, `internal/crawler/{client,sender,precheck}.go`, `internal/crawlqueue/queue.go`, `internal/web/server/server.go`, `internal/web/auth`, `internal/web/assemble`, `internal/web/ui/templates/layout.html`, `internal/web/webschema/privileges.go`, `internal/app/*` (new `migrate stats-schema-init`), `opflags/*`, docs.

## Tasks

Standard step pattern for every task (the plan states the decisions, not the code): **(a)** write the named tests first; **(b)** run them and read the failure, which must be "feature missing", not a typo; **(c)** implement the named signatures; **(d)** run the named tests green; **(e)** run `go vet ./... && go vet -tags loadtest,diag ./...`, `golangci-lint fmt --diff ./... && golangci-lint run ./...` (0 issues) and `go test -p 3 ./...` against the disposable trust-auth PostgreSQL (`MAGNETICO_TEST_REQUIRE_POSTGRES=1`), and for new guards a **mutation check** (a deliberate violation turns a named test red); **(f)** commit with the trailer; ledger line. Heavy suites are never run concurrently with timing-sensitive ones.

### Task 1: Statistics schema `magnetico_stats`

**Files:** Create `internal/opstats/{schema,init,verify,privileges,model}.go` and tests; modify `internal/app/` (`migrate stats-schema-init`), `opflags/` (the command's options mirror `ingest-schema-init`), `docs/web-deployment.md` (grants), `internal/repocheck` if it pins command lists.

**Interfaces:**
- Produces: `opstats.SchemaVersion = 1`; `opstats.InitSchema(ctx, dsn string) error`; `opstats.Verify(ctx, pool) error` returning `ErrSchemaMissing` / `ErrSchemaInvalid{Details []string}`; `opstats.MissingPrivileges(ctx, pool, Role) ([]string, error)` with `RoleIngest` (DML on `magnetico_stats`) and `RoleWeb` (`SELECT` on `magnetico_stats`, column-level `SELECT (id, reason_code, detail, attempts, payload_bytes, received_at, quarantined_at)` on `magnetico_ingest.quarantine`).

Tests (names): `TestInitSchemaIsIdempotentAndNonDestructive`, `TestInitSchemaRequiresVerifiedMagneticodV1`, `TestInitSchemaTakesAdvisoryLock` (two concurrent inits), `TestVerifyDetectsExtraColumnMissingIndexForeignTrigger` (structural, mirrors `webschema` verification), `TestVerifyRefusesWrongVersionRow`, `TestSchemaDoesNotChangeOtherSchemasVerification` (persistence startup, ingest `Open`, web `Verify` unaffected), `TestChecksRejectNegativeCountersAndMinGreaterThanMax`, `TestBucketStartMustBeAligned`, `TestWebRoleCannotReadQuarantinePayloadOrInfoHash` (column grant, with a restricted role), `TestIngestRoleWithoutStatsGrantsGetsNamedMissingPrivileges`.

- [ ] Steps (a)–(f). Table and column names follow D4; no triggers, no PL/pgSQL; `autovacuum` parameters on `snapshot`/`ingest_instances` (frequent small updates).

### Task 2: Crawler identity and protocol amendment

**Files:** Create `internal/crawlqueue/identity.go`; modify `crawlqueue/queue.go` (Open), `check.go`, `internal/crawler/{client,sender,precheck,enqueue}.go` as needed, `internal/ingest/server/server.go`, `docs/specs/crawler-ingest-delivery-v1.md`, `docs/crawler.md`, `docs/container.md`/`web-deployment.md` smoke commands, every test client and example that calls the API (`grep -rn "api/ingest/v1"` is the checklist).

**Interfaces:**
- Produces: `crawlqueue.Queue.CrawlerID() string`; `crawlqueue.ErrCorruptIdentity`; `crawlqueue.ReadIdentity(dir) (id string, present bool, err error)` (read-only, used by `Check`); `crawler.NewClient(rawURL, token, crawlerID string, httpClient)`; header constant `crawler.HeaderCrawlerID = "X-Magnetico-Crawler-ID"` and `server.HeaderCrawlerID` (same string; one definition shared through a neutral constant package or duplicated with a test that they match); `server` error code `invalid_crawler_id`; `crawler.SubmitIdentityRejected`; the server hands the validated id to handlers through a request-scoped value read by `stats.Recorder` (Task 3).

Tests: `TestIdentityCreatedOnFirstOpenAndStableAcrossReopens`, `TestIdentityStableWhenDirectoryIsMovedToAnotherPath`, `TestExistingQueueWithoutIdentityAcquiresOneUnderTheLock`, `TestConcurrentOpenOnlyOneProcessCreates` (second gets `ErrLocked`, id file unchanged), `TestCorruptIdentityRefusesStartAndIsNotReplaced` (empty, short, uppercase, UUIDv1, trailing bytes, directory in place of file; file bytes unchanged afterwards), `TestCrashDuringCreationLeavesNoPartialIdentity` (fault injection between temp write and rename), `TestCheckNeverCreatesOrModifiesTheDirectory` (before/after tree and mtime comparison), `TestGeneratedIdentityIsCanonicalUUIDv4`; server: `TestMissingIdentityIsRejectedOnBothEndpoints`, `TestDuplicateIdentityHeadersAreRejected`, `TestMalformedIdentityVariantsAreRejected` (table), `TestIdentityIsCheckedAfterAuthenticationAndBeforeTheBody` (bad token + bad id ⇒ 401; valid token + bad id ⇒ 400 and the body is not read), `TestInvalidIdentityErrorBodyHasNoEcho`; crawler: `TestBothRequestsCarryTheHeader`, `TestIdentityRejectionOfASingleRecordIsNeverQuarantined`, `TestIdentityRejectionOfABatchDoesNotBisect`, `TestIdentityRejectionBacksOffAndKeepsRows`, `TestExistsIdentityRejectionFailsOpenAndLogsLoudly`.

- [ ] Steps (a)–(f). Update the 06 protocol tests, the real-binary tests (`internal/e2e`), `docs/crawler.md` smoke `curl` commands and OpenAPI-free ingest examples in the same commit that makes the header mandatory.

### Task 3: In-memory instrumentation (no I/O)

**Files:** Create `internal/clientip/*` (moved), `internal/ingest/stats/recorder.go`; modify `internal/ingest/{server,pgqueue,existence,cache}`, `opflags/ingest.go` (the two variables), `internal/web/server/clientip.go` (delegates).

**Interfaces:**
- Consumes (Task 2): validated crawler id in the request context.
- Produces: `stats.Recorder` (concrete, nil-safe: every method on a nil `*Recorder` is a no-op, so `STATS_DISABLED` costs one nil check) with `SubmitAcked(id CrawlerID, ip netip.Addr, records, enqueued, duplicates int, bodyBytes, payloadBytes int64)`, `SubmitRejected(class RejectClass, id CrawlerID, ip netip.Addr, bodyBytes int64)`, `ExistsDone(id, ip, hashes, found, missing, unknown, hitPos, hitNeg, miss int, bodyBytes int64)`, `ProcessCreated/Existing(d time.Duration)`, `ProcessAttempt()`, `ProcessRetry()`, `ProcessStorageError()`, `ProcessQuarantined()`; `Recorder.Swap() Delta` (atomic swap of fixed-size counters + crawler registry delta); `RejectClass` enum; `pgqueue.AdmitResult.InsertedBytes int64`; `cache.Stats.PositiveHits/NegativeHits`; `clientip.Resolve(remoteAddr string, xff []string, trusted []netip.Prefix) netip.Addr`.

Tests: one test per row of the D2 table asserting the exact counter and boundary (`TestReceivedEqualsEnqueuedPlusDuplicates`, `TestDuplicateAdmissionCountsOnlyDuplicates`, `TestRefusedQueueFullCountsRejectNotReceived`, `TestBytesAreBodyBytesNotWireBytes`, `TestAcceptedPayloadBytesCountInsertedRowsOnly`, `TestConsumerCountsCreatedOnlyAfterDeleteCommitted` (existing `beforeDelete` fault hook: ambiguity ⇒ `existing`, documented), `TestProcessingDurationExcludesRejectedRows`, `TestExistsOutcomesAndCacheKindsAreSplit`, `TestRejectClassMappingCoversEveryServerErrorCode` (reflective list of codes ⇒ no code falls into no class)); `TestRecorderConcurrentUpdatesNeverLoseCounts` (`-race`, N goroutines), `TestSwapIsAtomicAndTotalsAreConserved`, `TestRegistryIsBoundedAt1000AndCountsOverflow`, `TestRegistryMemoryIsBounded` (heap measurement like `cache.TestHeapBudget`), `TestCounterSaturatesAtTwoToTheSixtyTwo`, `TestNilRecorderIsNoOp`, `TestRecordingNeverBlocksTheHandler` (no locks held across I/O; handler time with a stalled writer unchanged), `TestTrustedProxyPolicy` (moved cases plus ingest wiring: header ignored from an untrusted peer), `TestNoStatisticsImportInCrawlerDHTPath` (import-graph test: `dht/...`, `metadata/...` do not depend on `internal/ingest/stats` or `internal/opstats`).

- [ ] Steps (a)–(f). Existing handlers only gain calls to the recorder; admission decisions, status codes and ordering are unchanged (the full ingest suite is the regression).

### Task 4: Aggregation, persistence, samplers and writer lifecycle

**Files:** Create `internal/opstats/agg.go`, `internal/ingest/stats/{writer,snapshot,sampler_queue,sampler_storage,windows}.go`; modify `internal/ingest/server/service.go` (start/stop ordering), `internal/app/ingest.go`.

**Interfaces:**
- Consumes: `Recorder.Swap`, `opstats.Verify`, the queue DSN.
- Produces: `stats.NewWriter(dsn string, rec *Recorder, opt WriterOptions) *Writer` (own one-connection pool); `Writer.Run(ctx)`; `Writer.Close(ctx)` (final bounded flush); `Writer.Status() WriterStatus` (for the log summary); `opstats.Merge(dst, src *Counters)` saturating; `opstats.HourOf/DayOf/MonthOf(t time.Time) time.Time` (UTC).

Tests: `TestFlushWritesAllThreeBucketLevelsFromOneBatch`, `TestFlushIsAdditiveAcrossRestartsInTheSameBucket`, `TestTickNeverSpansAnHourBoundary` (fake clock; hour, day, month, year, leap day, DST-irrelevance), `TestClockStepBackwardsDoesNotCorruptBuckets`, `TestWeightedMeansUseSumAndCount`, `TestGaugeMinMaxLastAndLastAtGuard`, `TestObservedSecondsMarkPartialBuckets`, `TestMissingBucketHasNoRowAndZeroBucketHasObservedSeconds`, `TestFlushAmbiguousCommitAfterCommitDoesNotDoubleCount`, `TestFlushAmbiguousCommitBeforeCommitIsRetriedOnce` (proxy that drops the connection around `COMMIT`), `TestOutageBuffersAtMost24HourlyDeltasAndCountsDrops`, `TestTwoWritersProduceTheSumOfBoth`, `TestStatsFailureNeverBlocksAdmissionOrConsumer` (drop the schema, revoke privileges, take `ACCESS EXCLUSIVE` on a stats table: admissions keep their latency), `TestGracefulShutdownFlushesWithinBudget`, `TestWriterUsesItsOwnPool`, `TestSchemaMissingDisablesWriterWithOneWarning`, `TestQueueSamplerBacksOffWhenSlow`, `TestQueueStateIsNotZeroWhenUnavailable` (error ⇒ snapshot not advanced, never a zero row), `TestStorageSizesMatchPostgresFunctionsWithoutDoubleCounting` (fixture with a TOASTed blob table and indexes: heap+toast+indexes ≤ schema total, equality with `pg_total_relation_size` for the fixture, FSM/VM included), `TestTorrentEstimateIsFlaggedApproximate`, `TestRingsAndWindowsCoverageNotNominal`, `TestCountersMalformedStoredDataIsRejectedByReader`.

- [ ] Steps (a)–(f). **Measurement step inside this task:** queue-state query cost at 10 k, 100 k and 1 M queued rows (synthetic, disposable DB) → decides D7's exact-vs-estimate rule; recorded in `docs/wip/09-measurements/`.

### Task 5: Cached read model

**Files:** Create `internal/web/admin/stats/{reader,model,cache,service}.go`.

**Interfaces:**
- Consumes: `opstats.Verify`, the web pool.
- Produces: `stats.Service` with `Overview(ctx, p admin.Principal) (Overview, Freshness, error)`, `Series(ctx, p, Query{Granularity, Period}) (Series, Freshness, error)`, `Crawlers(ctx, p) ([]Crawler, Freshness, error)`; `Freshness{FetchedAt time.Time; Stale bool; IngestState up|stale|unavailable|uninitialised}`; `Measured[T]`.

Tests: `TestSnapshotCacheCoalescesConcurrentRefreshes` (100 goroutines, one query), `TestSnapshotCacheServesLastKnownGoodOnFailureAndMarksItStale`, `TestUnavailableIsNeverZero`, `TestFreshnessClassification` (boundaries 30 s / 5 min), `TestSeriesCacheIsBoundedAndLRU`, `TestSeriesRejectsOutOfRangePeriods` (future, before first row, > bounds), `TestSeriesReturnsNullForMissingBucketsAndZeroForMeasuredZero`, `TestSeriesRatiosAreWeighted`, `TestMultiInstanceSnapshotsAreSummed`, `TestCacheNeverStoresPerUserData`, `TestColdAndCachedLatencyBudget` (soft assertions, numbers recorded in Task 9).

- [ ] Steps (a)–(f).

### Task 6: Admin access, overview JSON and page shell

**Files:** Create `internal/web/admin/{guard,handlers}.go`, `templates/admin_layout.html`, `admin_overview.html`; modify `server/server.go` (`Admin`, `AdminJSON`), `auth/guards.go`, `assemble/assemble.go` (install; grant verification), `layout.html` (`Admin` link beside `Account`, only when the principal is an admin), `webschema/privileges.go` (`magnetico_stats` and quarantine column grants as an **optional** requirement: missing ⇒ the admin section says what is missing; web start is not blocked).

**Interfaces:**
- Produces: `server.Admin`, `server.AdminJSON` access classes; `admin.Principal` (only the guard constructs it); routes `GET /admin`, `GET /admin/api/overview`, `GET /admin/api/series?chart=&g=&period=`; a template function `isAdmin` fed from the session principal.

Tests: `TestAdminRoutesRequireAdminOnEveryRoute` (route-table driven: every route registered with `Admin`/`AdminJSON` for {anonymous → 302/401, user → 403, admin → 200, disabled admin → anonymous, demoted-between-requests → 403 on the very next request, promoted → 200}), `TestAPITokenAndRSSTokenNeverReachAdmin`, `TestAdminRoutesAreGetOnly` (POST/PUT/DELETE/PATCH ⇒ 405 and no handler runs; registering a non-GET admin route panics), `TestAdminLinkVisibleOnlyToAdmins`, `TestNoAdminDataInHealthzReadyzOrErrors`, `TestAdminResponsesAreNoStoreAndKeepSecurityHeaders` (CSP byte-identical to the current header), `TestPeriodAndChartParametersAreStrictlyValidated`, `TestBoundedResponseSizes`, `TestUnauthorisedJSONIsProblemWithoutDetails`, `TestWebStartsWithoutTheStatsSchemaAndSaysSo`.

- [ ] Steps (a)–(f).

### Task 7: Vendored uPlot and the chart front end

**Files:** Create `internal/web/ui/static/vendor/uplot/{uPlot.iife.min.js,uPlot.min.css,LICENSE,VENDOR.md}`, `static/admin.js`, `static/admin.css`; modify the template for assets, `ui.go` static map, `internal/web/ui/frontend_test.go` (self-test harness `testdata/selftest.js` style).

**Interfaces:** `admin.js` fetches `/admin/api/series` (same origin), renders five chart groups from the JSON of Task 5/6, builds the textual summaries with `textContent`, reads colours from CSS variables, re-themes on `prefers-color-scheme`/`theme.js`.

Tests: `TestVendoredUPlotChecksumsMatchVendorMd`, `TestVendoredUPlotHasNoEvalOrFunctionConstructor`, `TestAdminPagesLoadOnlySameOriginAssets` (HTML parse: every `src`/`href` is relative or same-origin; no inline `<script>`, no `style=` attribute, no `on*=`), `TestChartDataIsEmbeddedAsJSONNotAsScript`, front-end self-tests (the existing JS test harness): `nullBucketsRenderAsGaps`, `noDataStateShown`, `errorStateShown`, `untrustedStringsAreTextContent`, `summaryTableMatchesSeries`.

- [ ] Steps (a)–(f). **Not verifiable here:** rendering in a real browser under the strict CSP (RC0 checklist item).

### Task 8: Crawlers, quarantine, users, invitations, trackers pages

**Files:** Create `internal/web/admin/pages/*.go`, templates `admin_crawlers.html`, `admin_quarantine.html`, `admin_users.html`, `admin_invitations.html`, `admin_trackers.html`.

**Interfaces:** routes `GET /admin/crawlers`, `/admin/quarantine?after=`, `/admin/users?q=&after=`, `/admin/invitations?after=`, `/admin/trackers`. Queries select explicit columns: users (`id, username, email, email_verified_at, active, role, created_at, last_login_at`, and "invited by" via `invites.used_by`), invites (`id, owner_id, used_by, used_at, created_at, expires_at, revoked_at`, **never `code`**; derived status unused/used/revoked/expired), quarantine (`id, reason_code, detail, attempts, payload_bytes, received_at, quarantined_at`; keyset on `id DESC`, page 50), trackers (`url, position`). Page size 50, keyset cursors (opaque, strictly parsed), search `q` ≤ 64 characters, case-insensitive substring over username and email with `LIMIT 51`.

Tests: `TestUsersPageNeverSelectsOrRendersCredentialColumns` (query text inspected + rendered output scanned for the fixture's hash/token strings), `TestInvitationsNeverExposeCodes`, `TestQuarantineNeverReadsPayloadOrInfoHash` (executed with the restricted role, so a payload select would fail with a permission error), `TestQuarantineShowsReasonAgeAttemptsSize`, `TestPaginationIsKeysetBoundedAndStable`, `TestCursorAndQueryAbuseIsRejectedWithoutDetail`, `TestUserSearchIsBoundedAndEscapesWildcards`, `TestAllAdminOutputEscapesUntrustedStrings` (names like `<script>`, `"><img`, RTL marks, 25-char limits), `TestTrackersReadOnly`, `TestCrawlersPageShowsAvailableStatsAndOverflowNotice`, `TestAdminRoutesHaveNoMutationControls` (no `<form method=post>`, no state-changing JS).

- [ ] Steps (a)–(f).

### Task 9: Real-binary end-to-end and qualification

**Files:** Create `internal/e2e/cli/stats_test.go` (+ `loadtest`-tagged `stats_load_test.go`), `docs/wip/09-measurements/*`; reuse `internal/testkit`, `internal/pgtest`, `internal/web/benchkit` (no new campaign).

Scenarios (all synthetic, disposable DB, loopback only): (1) **multi-crawler**: three crawler identities (real `crawl` binary is not used; the real client with three queue directories) → real `ingest` → real `web`; assert global and per-crawler counters, the invariants `received = enqueued + duplicates` and `created + existing + quarantined ≤ enqueued + backlog`, and the dashboard JSON; (2) restarts: kill -9 and graceful stop of ingest, bucket continuity; (3) PostgreSQL outage and statistics-schema sabotage with ingestion unaffected; (4) admin access matrix through the real router; (5) offline assets and CSP header on admin pages.

Measurements (recorded with dataset, configuration and command, compared against `MAGNETICO_INGEST_STATS_DISABLED=true` on the same data): ingest throughput and CPU (`rusage`), extra DB writes and statements per minute (`pg_stat_user_tables`, `pg_stat_statements` if available, else counted through the proxy), heap and RSS, statistics storage for generated 1/5/10-year data (`pg_total_relation_size` per table and index, plus the extrapolation), dashboard cold and cached latency (p50/p95, 100 requests), behaviour during storage failure.

- [ ] Steps: tests first for the scenarios; measurement harness behind the `loadtest` tag; results into the report. Add the real-browser and production-like checks to `rc0-acceptance-checklist.md`.

### Task 10: Documentation, report and issue comment

**Files:** Create `docs/specs/statistics-v1.md` (normative: metric definitions and boundaries, byte semantics, windows, schema, collection intervals, failure behaviour, identity protocol, privacy rules, multi-instance behaviour), `docs/wip/09-statistics-admin-report.md`, `docs/operator-statistics.md` (interpretation guide: backlog vs duplicate traffic, stale/unavailable/missing, quota, partial buckets, approximate counts); update `docs/specs/crawler-ingest-delivery-v1.md`, `docs/specs/web-v1.md`, `docs/web-deployment.md`, `docs/crawler.md`, `docs/container.md`, `docs/projects_differences.md` (table, rationale, diagrams, date), `AGENTS.md`, `docs/wip/README.md`, `docs/wip/rc0-acceptance-checklist.md`, `docs/wip/0.8.0-release-presentation-topics.md`, `docs/testing.md` (new tests and tags), `internal/repocheck` if it pins documents.

Tests: `internal/repocheck` extensions: every metric name in `statistics-v1.md` exists in the schema and vice versa; every admin route appears in `web-v1.md`; the privacy rules ("no payload, no hash, no code") are asserted against the DDL (no column named like them); doc links resolve.

- [ ] After the commits exist: **post one comment on Forgejo #48** (implemented: dashboard, crawlers, quarantine viewer, users and invitations read-only, tracker list read-only; deferred: all mutations, invitation grants/revocation, tracker management, #49 graph, live updating), **one on #50** (completed: ingest-side collection, persistence, charts; deferred: crawler-side statistics, retention command, per-crawler time series), with the commit hashes and the documents. **Neither issue is closed.** (The comment is an external write the brief explicitly requests; it is posted only after the final verification.)

## Self-review against the brief

- **Spec coverage:** identity (§3) → Task 2; every metric of §4 → D2 table + Task 3 tests; storage (§5) → D7 + Task 4; per-crawler (§6) → D4 `crawlers`, D5, D11, Task 3/8; aggregates (§7) → D4, D5 (+ the 1/5/10-year measurement, Task 9); cadence, cache, multi-instance (§8) → D5–D9, D12; admin UI (§9) → D10, D14, Tasks 6–8; security and privacy (§10) → Review Focus 3, 5–8 and the Task 6/8 tests; qualification (§11) → Task 9; documents and issue comment (§12) → Task 10; out-of-scope (§13) is respected (no mutation route can be registered; no retention code; no per-crawler series; no token per crawler; no exporter).
- **Placeholders:** the uPlot version and the final interval constants are deliberately fixed at implementation time against evidence (release page; measurement), and the plan says so.
- **Type consistency:** `CrawlerID` (Task 2) → `Recorder` (Task 3) → `Delta` → `Writer` (Task 4) → tables (Task 1) → `Reader` (Task 5) → `admin.Principal` (Task 6) → pages (Task 8); `RejectClass` has nine values in D2, the schema, the recorder and the tests.
- **Proportion:** decisions, signatures and test names only; no function bodies.

## Risks and open points for the owner

1. **Protocol amendment** (D3, amendment 1) makes every pre-existing crawler fail until upgraded; intentional, but 0.8.0 is unreleased so no deployed crawler exists.
2. **Grants:** the ingest role (or the queue-DB role) needs DML on `magnetico_stats`, the web role needs `SELECT` there and column-level `SELECT` on `magnetico_ingest.quarantine`; the default shared account needs nothing. Documented with verbatim `GRANT`s executed by the tests.
3. **Exact queue counts at scale** are unmeasured until Task 4; the fallback is flagged approximation, not a schema change.
4. **uPlot under the strict CSP** and every visual property are unverifiable on this VM; they join the RC0 checklist.
5. **Registry cap of 1,000 crawlers** and **no cleanup command** (rejected for 0.8.0): a flooded registry stays flooded until an operator deletes rows by SQL, which the operator guide documents.
6. **Timestamps of events** are the writer's clock (ingest host); a host with a wrong clock produces misplaced buckets (documented; `observed_seconds` exposes gaps, not skew).
7. **Approximate indexed-torrent count** can lag after bulk imports until autovacuum/analyze runs (labelled approximate).

## Execution handoff

Which approach: **native (inline) with one fresh whole-branch review** is the recommendation, as in Task 08: the ten tasks share interfaces in a single chain (identity → recorder → writer → schema → reader → admin), so a per-task fresh context would re-read the same code ten times, while the mutation-checked tests and the final review carry the risk. Subagent-driven is the alternative if you want a review gate after each of Tasks 2, 4 and 6 (the three that touch the delivery path, the persistence protocol and the privilege boundary).
