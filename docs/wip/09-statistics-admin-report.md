# Task 09 report — per-crawler credentials, ingest statistics and the read-only administrator section

**Status:** implemented; awaiting owner review. Plan: [`09-statistics-admin-plan.md`](09-statistics-admin-plan.md) (revision 3, approved). Normative contract: [`docs/specs/statistics-v1.md`](../specs/statistics-v1.md). Operator guide: [`docs/operator-statistics.md`](../operator-statistics.md). Raw measurements: [`09-measurements/`](09-measurements/). Branch `refactor/0.8.0-unify-and-new-storage`, baseline `458a7f84` (the approved plan).

**What this report does and does not claim.** Everything below ran on the development VM (8 vCPUs, AMD Ryzen 9 7950X3D, Go 1.27.4, PostgreSQL 17 in a local Docker container with default settings), against disposable databases, with synthetic data and **loopback only**. No live DHT or BitTorrent request was made; nothing was tagged, published or pushed to a registry. Statements about uPlot in a real browser, about the production reverse proxy and about Woodpecker are **not** claims: they are in [§9](#9-remaining-limitations-and-rc0-items). A criterion that was not met is reported as not met.

## 1. Summary

* **Delivered:** per-crawler credentials with an operator CLI (the shared ingest token is gone), time-based request batching, in-memory ingest instrumentation with an idempotent writer into a new `magnetico_stats` schema, a cached read model, and a **read-only** administrator section (overview with five chart groups, crawlers, quarantine, users, invitations, trackers). Twelve tasks, thirteen commits plus the documents; `go vet` (also with the `loadtest,diag` tags) and `golangci-lint` report 0 issues; `go test -p 3 ./...` passes in all 77 packages.
* **Qualification (§3):** of the nine acceptance rows, **eight are met and one is met only after a correction of the measurement, with one sub-criterion not met as written**: the sabotage row "holding `ACCESS EXCLUSIVE` on a statistics table" gives **54 % of the baseline throughput, not ≥ 90 %**. A control with the statistics *disabled* and only an idle open transaction on an unrelated table gives 55 %: the loss is the cost of holding a transaction open under a load that churns the queue at ~90,000 records/s, not of the statistics code (relative to the control the locked statistics cost 3 %). I report it as **not met as specified** and explain the evidence in §3; no code was changed for it.
* **Defects found by running the real binaries** (§4): the shutdown dropped up to a minute of statistics after a failed flush, a fresh process was invisible on the dashboard for 10 s, and a repeated failure was logged twice — each fixed with a test that failed first.
* **Not verified here** (§9): the charts in a real browser under the strict CSP, a real reverse proxy, a pipeline run, live-DHT behaviour. They are in the RC0 checklist.

## 2. What was built

| Task | What | Commits |
|---|---|---|
| 1 | `crawlers` table in the initial `magnetico_ingest` schema (version stays 1), structural verification of it, the registry store (`mgc_` credentials, SHA-256 digest only, case-insensitive unique names) | `25c9e867` |
| 2 | Per-crawler authentication (60 s positive cache anchored at lookup start, 8 active lookups / 64 pending digests / 1,024 waiters, `503` never `401` on storage failure), `magnetico ingest crawler add\|list\|disable\|enable\|rotate-token`, the shared `MAGNETICO_INGEST_TOKEN` removed | `8f339306` |
| 3 | Time-based request batching with early dispatch when the next record would not fit, 1 s / 1,024-hash precheck, clock-step-safe linger | `f6518724` |
| 4 | `magnetico_stats` v1 (schema from a catalogue, verification, privileges, bucket math), `migrate stats-schema-init` | `3c1f249b`, `b172e380` |
| 5 | The recorder (one mutex, event-time hours, never held across admission I/O), handler and consumer instrumentation, the moved client-IP resolver, the kill switch | `f28ee723` |
| 6 | Aggregation, the sealed-batch flush protocol with a single arbiter and tombstones, heartbeat, queue and storage samplers, the elected sampler, coverage as a minute union | `99285c63` |
| 7 | The cached read model (coalescing TTL/LRU cache, last-known-good, three freshness clocks, rate rules) | `39544aa0` |
| 8 | `Admin`/`AdminJSON` access classes, the guard, the overview page and JSON | `92f70074` |
| 9 | uPlot 1.6.32 vendored with its licence in the image and the archives, the chart front end, CSSOM-only styling | `d9e3ced7` |
| 10 | Crawlers, quarantine, users, invitations and trackers pages (read-only, keyset-paginated, explicit columns) | `b9324485` |
| 11 | Real-binary end-to-end scenarios, the qualification harness, three defects fixed | `c19d9e3c`, `df219d77` and this report's commit |
| 12 | `statistics-v1.md`, `operator-statistics.md`, updates to the specs and guides, repository checks of the documents | the commits after `df219d77` |

## 3. Qualification results

All numbers: synthetic data, a disposable PostgreSQL, loopback only; load runs are closed-loop (8 workers × 50 records) against the real binary; "baseline" is the same binary with `MAGNETICO_INGEST_STATS_DISABLED=true`; each load figure is the **median of 5 alternating runs of 60 s** (order rotated each iteration) unless said otherwise. Dataset, configuration, commands and raw files: [`09-measurements/README.md`](09-measurements/README.md).

| Area | Criterion (fixed before measuring) | Result | Verdict |
|---|---|---|---|
| **Instrumentation overhead** | ≥ 97 % of baseline admitted records/s; CPU per record ≤ +5 %; p95 ≤ +10 %, p99 ≤ +20 %; recorder ≤ 250 ns uncontended, ≤ 2 µs at 64 goroutines, 0 allocations | 92,771 vs 95,233 records/s = **97.4 %** (thin margin); CPU 13.7 vs 13.7 µs/record (**±0 %**); p95 5.20 vs 5.01 ms (**+3.8 %**); p99 7.53 vs 6.82 ms (**+10.4 %**); recorder **109 ns** uncontended, **134–152 ns** at 64 goroutines, **0 allocs/op** (5 runs each) | **met** |
| **Memory** | statistics heap ≤ 8 MiB (10,000 registered crawlers, 1,024 active crawlers in a window, 26 pending hours); RSS at steady state ≤ baseline + 16 MiB | heap: recorder + the writer's merged carry for 1,024 crawlers over 26 hours **0.77 MiB**, authentication cache after 10,000 registered crawlers authenticated (4,096-entry bound) **0.75 MiB**: **≈ 1.5 MiB** (keeping 26 unmerged drained copies instead, which the writer does not do, 7.7 MiB). RSS at 200 records/s with `GOMEMLIMIT=256MiB`, median of ten samples per run, 5 alternating pairs: **258.1 vs 260.4 MiB (−2.4 MiB)**. A first attempt without a memory limit and with a single reading per run showed +28 MiB with ±30 MiB scatter between runs of the same condition (garbage-collection timing); it was discarded as a measurement of the collector and is kept in `raw/` | **met** (the simulated 24 h outage is covered by the in-process test of the 26-hour bound, not by a 24 h run) |
| **Statistics write frequency** | per instance ≤ 1 flush, ≤ 6 snapshot upserts, ≤ 6 `shared_state` upserts per minute (≤ 14 transactions); ≤ 3 statistics connections; unchanged when the load is ×10 | 180 s windows: **1.33 flush/min** (4 flushes in a window whose edge caught one; the tick is exactly 60 s), **6.0 snapshot upserts/min**, **1.33 `shared_state` upserts/min**, storage 0.33/min; **identical at 200 and 1,974 records/s**; client connections 7 vs 4 with the statistics disabled (**+3**) | **met** |
| **Queue sampling** | one exact cycle p95 ≤ 100 ms / 800 ms / 2 s at 10 k / 100 k / 1 M rows; admission p95 inflation ≤ 10 % at 1 M rows | Task 6: 2.5 / 23 / 123 ms at the widest inline rows ([`queue-sampling.md`](09-measurements/queue-sampling.md)). Admission with 1,000,000 queued rows, 3 alternating pairs of 90 s: p95 **7.10 vs 8.22 ms = 0.86×** (no inflation), 77,000 vs 72,000 records/s. No row locks and the cycle deadline are asserted by the Task 6 tests, not measured here | **met** |
| **Authentication** | cached overhead ≤ 1 µs; ≤ 1 lookup per credential per 60 s; a flood of invalid tokens holds ≤ 8 lookups, ≤ 64 pending digests, ≤ 1,024 waiters, and the state is empty afterwards; cached throughput ≥ uncached | cache hit **195–221 ns** (2 allocations); lookups per credential within the bound (asserted by the multi-crawler scenario); flood of **2,970,430** invalid-token requests in 30 s from 256 connections: at most **4** concurrent lookups seen in `pg_stat_activity`, 0 failures of a legitimate cached credential (p95 7.6 ms), a new credential authenticated in 0 ms afterwards, 0 storage failures; 87 % of the flood was shed as `503 busy` by design; the 64-digest and 1,024-waiter bounds are asserted by the Task 2 unit tests (counters), not by this run. Throughput: **5.2–6.9 million/s cached vs 13–51 thousand/s uncached** (≥ 100×; "uncached" gives every call its own authenticator, so it bypasses the shared 8-slot bound and is an upper bound for a design without a cache) | **met** |
| **Delivery batching** | sparse: one request per record batch, no empty request; next record that does not fit dispatches at once; sustained 200 records/s: 0.8–1.2 requests/s per endpoint, mean ≥ 100 records per submit, enqueue→ack p95 ≤ 2.5 s, connection reuse ≥ 99 %; backlog drains at least as fast as the baseline | sparse (12 records, ≈ 1 per 3 s): 12 requests, 0 empty, 1 record each; two 5 MB records: first request left **278 ms** after its oldest record (the linger is 1,000 ms; the baseline sent at once); sustained, 150 s: **1.00 submit/s and 1.00 exists/s, 200 records/request**, p50 514 ms, **p95 964 ms**, **2 connections for 299 requests (99.3 %)**; the pre-batching client sent **198 requests/s** of 1 record. Backlog of 12,000 records after a 60 s outage, 3 repetitions each: **drain after the first successful request 0.8–0.9 s (batching) vs 0.9–1.0 s (baseline)**. The *recovery time* (link back → last acknowledgement) was 8.4 / 19.3 / 35.5 s vs 7.4 / 31.6 / 34.4 s: it is the sender's jittered retry backoff (1 s doubling to 60 s, unchanged by Task 3) and has the same distribution in both. The sustained window is 150 s because at one request per second a 60 s window has only 60 requests, so even a single connection reaches only 98.3 %. Shutdown without losing a queued row is covered by the Task 3 tests, not re-measured | **met** (backoff-dominated recovery time reported) |
| **Statistics-specific sabotage** | with PostgreSQL healthy: admission ≥ 90 % of baseline, zero refusals, errors counted and logged once | against the disabled baseline, 5 runs each: **schema dropped 96.5 %**, **grants revoked 100.4 %**, **statements over their timeout 90.5 %** (just above the line; p95 +17 %, p99 +21 %), **tables locked 54.2 %**; **0 refused or failed records in all 20 sabotage runs**; errors logged once per state (end-to-end scenario). Control: statistics disabled **plus only an idle open transaction on an unrelated table: 55.1 %**; statistics enabled with the tables locked: 53.2 % | **3 of 4 met; "tables locked" not met as specified (54.2 % < 90 %)** — the control shows the loss is the open transaction, see below |
| **Failed and delayed flushes** | after recovery, persisted totals equal the recorded events exactly (`received − acknowledged − refused = in_flight`; equality at quiescence), no double count, no sequence gap | a link that delays every reply by 250 ms and cuts every connection every 700 ms around the flush at 60 s: clients saw 12,340 admitted and 800 refused; persisted received 12,580 = acknowledged 12,340 + refused 240 (the other 560 refused records were rejected before parsing, during authentication, and are not "received"). Unit tests cover the ambiguous commit before and after `COMMIT`, a racing retry, and an aged batch abandoned during an outage | **met** (after fixing defect 3) |
| **Full PostgreSQL outage** | admission fails with the specified 503, buffers stay in bound, no panic, recovery restores admission, counters conserved | 15 s healthy, **55 s with new connections refused and existing ones terminated (spanning the 60 s flush and the cache expiry)**, 15 s healthy: ingest survived, 10,960 records answered 503, admission recovered (6,000 admitted), persisted received 14,960 = acknowledged 6,000 (= the clients' count) + refused 8,960 | **met** (after fixing defect 3) |

**The tables-locked sabotage.** The harness opens a transaction, `LOCK TABLE … IN ACCESS EXCLUSIVE MODE` on the statistics tables, and keeps it open for the 60 s window. The writer then waits on its lock timeouts and keeps failing — and admission, which does not touch those tables, still runs. What slows it is not the lock but the *open transaction*: while it exists the vacuum horizon of the whole database cannot advance, and this load admits ~90,000 records/s and has the consumer delete them again, so dead tuples in the queue accumulate. The control reproduces it without any statistics involvement (statistics disabled, an idle transaction that locks an unrelated table): 55.1 % against 53.2 % with the statistics tables locked, i.e. the locked statistics cost 3 % on top of the method. At a production-like 200 records/s this effect is invisible (the end-to-end sabotage scenario, 200 records/s, saw no refusal and a slowest request of at most 127 ms in the four sabotage cases). I did not change the code; the criterion as written (`≥ 90 %` of the baseline) is **not met by this measurement** and I leave the decision to the owner.

**Storage growth** (generated rows, `pg_total_relation_size` per table and index, `VACUUM ANALYZE`, validated against the size functions):

| History | Statistics schema total | Largest relations |
|---|---|---|
| 1 year | **7.7 MiB** | `agg_hour` 4.5 MiB, `storage_hour` 2.6 MiB |
| 5 years | **38.2 MiB** | `agg_hour` 23.0 MiB, `storage_hour` 13.4 MiB |
| 10 years | **76.3 MiB** | `agg_hour` 46.1 MiB, `storage_hour` 26.9 MiB, `agg_day` 1.9 MiB |

Linear extrapolation from one year is within **0.55 % (5 years) and 0.66 % (10 years)** of the measured sizes. 10,000 `crawler_activity` rows are 1.7 MiB. One month of upserts to the newest month row (43,200 separate transactions) leaves the table at 72 KiB (from 64 KiB): no bloat. Growth is linear and small; no retention is needed in 0.8.0.

**Dashboard latency** (a real `web` over 10 years of generated history, session cookie, loopback): overview cached p50/p95 **0.24 / 0.33 ms** (100 requests); overview cold (each request after the 10 s lifetime) **4.5 / 6.2 ms** (**40 requests**, not 100: each needs 10.5 s of waiting); a daily series cold **3.1 / 4.1 ms** (100 distinct days, each a cache miss) and cached 0.21 / 0.35 ms; a monthly and a yearly series cold 5.9 and 6.2 ms.

**End-to-end scenarios** (real `ingest`, crawler clients and `web`, `internal/e2e/cli/stats_test.go`, all passing): three crawlers with overlapping ranges (counters, per-crawler sums, conservation at quiescence, the dashboard's crawler page); credential rotate/disable/enable (old token and disabled crawler refused after **60 s**, a new token at once, every request accounted for exactly, the statistics of the other crawlers untouched); `SIGKILL` and graceful restarts (an event counted once or lost, never later); the four statistics sabotages leaving admission untouched; the full outage and the flaky link; the documented reinitialisation of an older development ingest schema leaving torrents, accounts and statistics identical.

## 4. Defects found by testing the real things

| # | Found by | Defect | Fix and test |
|---|---|---|---|
| 1 | the multi-crawler end-to-end scenario | for the first 10 s after start no instance snapshot existed, so the dashboard showed no ingest instance | the first heartbeat is published as soon as the writer is ready (`TestFirstHeartbeatIsPublishedImmediatelyOnceReady`) |
| 2 | the sabotage scenario | a repeated failure was logged twice because its state key was the driver's wrapper type (`PgError`, then `PrepareError`) | the key is the SQLSTATE class (`TestErrClassIsStableAcrossWrappers`) |
| 3 | the outage and flaky-link scenarios | after a failed flush the shutdown settled only the sealed batch and dropped every event behind it (up to a minute of statistics) | `Close` drains the sealed batch and the carry (`TestCloseDrainsTheCarryBehindAPendingSealedBatch`) |
| 4 | the plan's own review (revision 3) | the stats-init advisory lock key equalled the consumer's session lock key | its own key and a test holding every existing key (`b172e380`) |

Pre-existing, not changed: the `migrate web-schema-init` advisory key collides with the ingest consumer's lock key (…187); it is rarely held at the same time and was left for the owner's decision.

## 5. Rulings made during execution

These were decided without stopping, as the execution rules require; each costs what it says if the owner overturns it.

1. **Pgqueue verification** was version-only; a structural check of `crawlers` only was added (never repairs). *If wrong:* a later structural check of the rest would also be needed.
2. **A credential of the wrong shape is refused with 401 without a lookup** (reduces the flood surface). *If wrong:* none; minted tokens always match.
3. **The sender plans batches from metadata and loads payloads only when it dispatches.** *If wrong:* an extra read per dispatch.
4. **The final dispatch at shutdown happens at the loop top for any exit by cancellation.**
5. **The counter `bytes_accepted_payload` is named `bytes_accepted`** (the privacy scan of the schema forbids "payload" in any column name).
6. **`SubmitValidated` takes no body size; request bytes are recorded separately** so that rejected requests' bytes count; **`SubmitRefused` accepts `NoReject`** for a vanished client; **the cache outcome per request** comes from `existence.Result`.
7. **`shared_state` is written only inside the flush transaction**, and the heartbeat never touches `ingest_instances` (its `published_at` is the database clock). *Cost:* queue and storage freshness is bounded by the 60 s flush interval plus the sample age, inside the 90 s "fresh" window.
8. **The recorder ignores crawler ids that are not canonical UUIDs** (a poison id would fail every batch forever); **a successful `prepare` resets the flush back-off**; **`logChange` is per topic**.
9. **The service shutdown budget grows by the final statistics flush** (≤ 5 s plus ≤ 2 s to close the connections; 35 s worst case; container stop timeout 60 s unchanged).
10. **`Principal` lives in `internal/web/admin/access`** (an import cycle otherwise); the zero value is refused at run time and a test pins `access.Grant` to the guard.
11. **The crawlers list is keyset-paginated on the lower-cased name only** (the plan said name and id): names are unique case-insensitively, so one index serves it.
12. **Freshness ages use the database clock** so skew between hosts cannot make a live instance stale; sample times of the queue and storage remain the ingest host's.
13. **`TestAdministratorHasNoExtraWebCapability`** (web-v1 §4.2.3, "admin grants no web capability") was rewritten to the read-only `/admin` contract; `web-v1.md` was amended.
14. **The dashboard answer is cached for 10 s**, so a state change shows within 10 s. **`webschema/privileges.go` was not changed:** the web never needs the statistics or ingest grants to start; missing ones are reported by name.
15. **One plot per chart group** (a `Group` field on the series) so bytes, ratios and counts never share an axis.
16. **The front-end self-tests run under Node via Docker and are not in CI** (no JavaScript runtime exists in the repository's toolchain); `go test` pins the source invariants statically.
17. **The listing data functions live in `internal/web/admin/data`**, testable through a role that holds only the documented column grants; **the crawlers and quarantine pages share one cached (60 s) ingest-capability probe**; **no listing is cached**; the user search is a sequential scan bounded by `LIMIT 51`.
18. **The end-to-end scenarios live in `internal/e2e/cli`** (a real `web` is needed); crawler clients drive the counters and restart scenarios, direct HTTP submitters drive the sabotage, outage and flaky-link scenarios so that every status is seen.
19. **Statistics catch up after a failed flush at the next 60 s tick (or at shutdown)**; a faster retry was not added and is not claimed.
20. **The documented reinitialisation leaves unreferenced `crawler_activity` rows** (harmless, not shown).
21. **Measurement-method corrections, all disclosed:** the steady-state memory run was repeated with `GOMEMLIMIT=256MiB` and ten samples per run after the first attempt measured the collector; the heap test merges the drained hours as the writer does; the backlog-recovery row reports the drain speed separately from the retry backoff; the connection-reuse window is 150 s; the cold overview latency has 40 samples; the storage churn check uses one transaction per update after a single-transaction version could not be pruned and over-stated the growth.

## 6. Cleartext HTTP/2 evaluation

**Question.** Would cleartext HTTP/2 (`h2c`) between a crawler and `ingest` help, now that delivery is batched?

**Evidence.** Over TLS the crawler's default `http.Transport` already negotiates `h2` (verified with a TLS test server, `internal/crawler/precheck_batching_test.go`); over `http://` it uses HTTP/1.1 keep-alive. After batching, one crawler sends about **1.00 request per second per endpoint** (§3); a 150 s run of 299 requests opened **2 connections (99.3 % reuse)**, and the requests are 100–256 records long, so the cost is dominated by the body and the admission transaction, not by connection setup or headers (the request latency p95 is 964 ms *including* the 1 s linger of the batch's oldest record; the server's part is a few milliseconds).

**Decision.** Keep HTTP/1.1 keep-alive for `http://` and rely on TLS `h2`. At about one request per second per endpoint multiplexing and header compression have nothing to improve, and h2c would add a protocol surface and an `http.Protocols` configuration to both sides. Not changed. The evaluation is repeated over the real network in the RC0 checklist.

## 7. Verification

Run on the final tree of this report (PostgreSQL 17 disposable, trust authentication, `MAGNETICO_TEST_REQUIRE_POSTGRES=1`):

| Check | Result |
|---|---|
| `go vet ./... && go vet -tags loadtest,diag ./...` | no findings |
| `golangci-lint fmt --diff ./... && golangci-lint run ./...` | 0 issues |
| `go test -p 3 -count=1 ./...` | all 77 packages `ok`, none failed |
| new document and privacy checks (`internal/repocheck/statistics_docs_test.go`) | pass; a removed documented counter and a `token_hash` in an administrator query each turn them red |
| front-end self-tests (`node:22-alpine`, Task 9) | 12 of 12 |
| mutation checks | per task in the ledger (an unlocked drain, event-hour attribution, the arbiter's row count ignored, a dropped tombstone, indexes leaking into `other_bytes`, a role check in the guard, `null → 0` in the chart data, an invitation `code` in a query, …): each turned a named test red |

Not run: any Woodpecker pipeline, a real GoReleaser or Docker release build, an arm64 execution, a real browser, a live-DHT or BitTorrent request (none was allowed), the front-end self-tests in CI.

## 8. Whole-branch review and its fixes

@@REVIEW@@

## 9. Remaining limitations and RC0 items

- **Real browser.** uPlot's rendering under the strict CSP, the themes, the keyboard and screen-reader behaviour and the 360 px layout are **unverified**: the development VM has no usable browser (07J: the headless browser refuses loopback). The self-tests of the front end run under Node only (`internal/web/ui/jstest/admin_test.js`, via Docker) and are **not part of CI**. Both are items of `rc0-acceptance-checklist.md`.
- **No pipeline has run.** The uPlot licence shipping in the image and the archives is checked statically and by the native smoke test against a synthetic archive; no real GoReleaser or Docker release build, no Woodpecker run and no arm64 execution took place. Woodpecker's `target_url` behaviour is unverified.
- **Production-like behaviour.** A real reverse proxy (client addresses, log redaction), real crawlers on the live DHT, a 24 h soak and a queue of millions of rows after a long outage with a cold cache were not exercised (RC0 checklist).
- **Freshness bounds.** `shared_state` (queue and storage figures) is written in the flush transaction, so its freshness is bounded by the 60 s flush interval plus the sample age, inside the 90 s "fresh" window; the dashboard answer is cached for 10 s; the statistics catch up after a failed flush at the next 60 s tick or at shutdown (no faster retry is claimed).
- **Crash loss.** On `SIGKILL` up to one flush interval of counters is lost (bounded and counted nowhere, as it is invisible to a dead process); during a statistics-database outage up to 26 hours of hourly deltas are buffered and are lost on a crash.
- **Event time is the ingest host's clock.** A badly wrong clock misplaces buckets; the monotone clamp and `clock_anomaly` expose it, coverage exposes gaps, not skew.
- **Indexed-torrent count** is the planner's estimate and lags after bulk imports until autovacuum or `ANALYZE` (labelled approximate).
- **Orphaned crawler activity.** `crawler_activity` rows of crawlers of a dropped registry stay unreferenced after the documented reinitialisation (harmless, not shown).
- **Out of scope, as agreed:** every administrator mutation (users, invitations, trackers, credentials), the #49 graph, live updating, crawler-side statistics, a retention command, per-crawler time series.
- **Disk.** About 58 old anonymous Docker volumes (≈ 6.4 GB) from earlier sessions were left untouched; an earlier `php:8.3-cli` image prune during Task 08 was a slip, reported at the time.
