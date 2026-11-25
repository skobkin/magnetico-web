# Ingest statistics v1 — normative contract

Status: implemented in Task 09 (0.8.0, unreleased). This document is the contract between the ingest instrumentation (`internal/ingest/stats`), the schema `magnetico_stats` (`internal/opstats`) and the read model of the administrator section (`internal/web/admin/stats`). Operator guidance is in [`docs/operator-statistics.md`](../operator-statistics.md); the measured behaviour is in [`docs/wip/09-statistics-admin-report.md`](../wip/09-statistics-admin-report.md).

The statistics describe **operation**, not content. They never store a payload, an info hash, a torrent name, a file path, a credential, a token digest or a request body, and they never associate a crawler with a torrent.

## 1. Scope and non-goals

- Collected by the `ingest` service only, in memory, and persisted by one writer per process into the schema `magnetico_stats` of the **same PostgreSQL database**. No new infrastructure, no exposed metrics endpoint, no database connection from the crawler, no write per request or per processed torrent.
- Statistics failures never interrupt ingestion, change an admission decision, delay a response beyond the recorder's critical section, or affect authentication. `MAGNETICO_INGEST_STATS_DISABLED=true` turns the whole subsystem off (no writer, no connections).
- Out of scope in 0.8.0: crawler-side statistics, per-crawler time series, retention or deletion of old aggregates (hourly, daily and monthly rows are kept indefinitely), live updating, exporters.

## 2. Boundaries and conservation

All times are UTC. An event is attributed to the UTC **hour of its occurrence**, decided inside the recorder's critical section from the process clock: `hour = max(hourOf(now), lastHour)`; a backward clock step never produces an older hour, a forward step leaves hours without a row (a missing observation), and a step of more than 26 hours either way sets `clock_anomaly`. Day and month are derived from the hour.

Byte counts are HTTP **request-body** bytes actually read (both endpoints, authenticated requests only); TLS, headers and responses are not counted.

Two boundaries exist for the processing of a record:

- the **creation boundary**: `torrents_created` is counted the moment `AddTorrent` reports that the torrent was created, whatever happens to the queue row afterwards;
- the **settlement boundary**: `settled_stored_new`, `settled_stored_existing` and `settled_quarantined` are counted **after** the queue row left the queue (deleted or moved to quarantine), and are mutually exclusive. "Existing" is what the settling attempt's `AddTorrent` reported, so it includes a torrent created by an earlier attempt whose queue-row delete failed.

**Conservation.**

1. At every instant, over every drained delta plus the live accumulator: `records_received − records_acknowledged − records_refused = in_flight`, where `in_flight` is the number of validated records whose admission has not finished. Every drained delta additionally satisfies `records_acknowledged = records_enqueued + admission_duplicates` exactly.
2. At **quiescence** — no handler running, for example after the graceful shutdown's final flush — `records_received = records_acknowledged + records_refused` exactly.
3. Event time is kept, so one bucket can show a residual of at most *concurrent requests × 256 records* (an admission that starts in hour H and ends in hour H+1 contributes `received` to H and its outcome to H+1). The residuals cancel in neighbouring buckets; no user interface presents the identity per bucket.
4. After a crash the records that were received but never acknowledged or refused stay as a permanent residual of that period (the crawler resends them, and the resend is a new `received` event). It is shown, not repaired.

Counters saturate at 2^62 in memory and in SQL.

## 3. Counters

Every counter is a monotone `bigint` per hour bucket (`agg_hour`, summed into `agg_day` and `agg_month`). A request rejected before authentication succeeded has no crawler and counts globally only.

| Counter | Recorded when | Definition |
|---|---|---|
| `submit_requests` | authentication succeeded | HTTP submit requests, any outcome |
| `submit_requests_validated` | the envelope decoded and every record validated | denominator of "records per request" |
| `records_received` | the same moment | records in validated requests, before admission |
| `records_enqueued` | `Admit` returned without error | rows inserted into the queue |
| `admission_duplicates` | the same | records that were already pending in the queue, or repeated within the same request |
| `records_acknowledged` | the same | `records_enqueued + admission_duplicates` |
| `records_refused` | `Admit` returned an error | records whose admission ended without an acknowledgement |
| `bytes_body` | the request body was read | request-body bytes of authenticated requests |
| `bytes_accepted` | the same as `records_enqueued` | payload bytes of inserted rows |
| `exists_requests` | an existence request was handled | requests |
| `exists_hashes` | the same | hashes checked |
| `exists_found` | per hash | already stored |
| `exists_missing` | per hash | not stored |
| `exists_unknown` | per hash | outcome not determinable |
| `cache_hit_positive` | per checked hash | answered from the positive existence cache |
| `cache_hit_negative` | per checked hash | answered from the negative existence cache |
| `cache_miss` | per checked hash | looked up in PostgreSQL |
| `auth_cache_hits` | authentication | credential found in the 60 s cache |
| `auth_cache_misses` | authentication | absent or expired, including coalesced waiters |
| `auth_db_lookups` | authentication | queries actually executed |
| `auth_storage_failures` | authentication | lookups that failed (error, timeout) |
| `auth_busy` | authentication | requests refused because the bounded lookup state was full or a slot wait expired |
| `rejected_auth` | a 401 was written | requests refused as unauthorised |
| `rejected_unavailable_auth` | a 503 raised by the authenticator | storage failure or load shedding during authentication |
| `rejected_malformed` | a 405, 415 or malformed-request error was written | requests |
| `rejected_too_large` | `payload_too_large` or `record_too_large` | requests |
| `rejected_invalid_record` | `invalid_record` or `unsupported_version` | requests |
| `rejected_queue_full` | the queue quota refused the batch | requests |
| `rejected_storage_unavailable` | admission failed on a storage error | requests |
| `rejected_busy` | the endpoint's concurrency limit refused the request (`busy`) | requests |
| `rejected_internal` | any other server error | requests |
| `processing_attempts` | the consumer started an attempt | attempts |
| `processing_retries` | an attempt was rescheduled | retries |
| `processing_storage_errors` | the consumer backed off globally | storage errors |
| `torrents_created` | creation boundary | torrents created |
| `settled_stored_new` | settlement boundary | rows settled after a creation |
| `settled_stored_existing` | settlement boundary | rows settled although the torrent existed |
| `settled_quarantined` | settlement boundary | rows moved to quarantine |
| `processing_ms_sum` | settled-stored | claim-to-delete wall time, milliseconds |
| `processing_ms_count` | settled-stored | number of timed settlements |

The `rejected_*` counters count **requests**; their records are counted only where a decoded count exists (`records_refused`). The nine reject classes are fixed; a new class is a schema version bump.

## 4. Gauges and storage measures

Gauges describe the **one database**, not an instance; they are sampled by the elected instance only (§9) and **never summed across instances**. Each hour bucket stores `sum`, `n`, `min`, `max` and `last` per gauge, so a mean is `sum ÷ n` and nothing averages averages.

| Gauge | Meaning |
|---|---|
| `queue_rows` | rows in `magnetico_ingest.queue` |
| `queue_bytes` | payload bytes of the queue rows (no estimate exists; unavailable when the exact count cannot finish) |
| `quota_used_bytes` | `usage.used_bytes`: queue and quarantine bytes together, exact |
| `quarantine_rows` | rows in `magnetico_ingest.quarantine` |
| `quarantine_bytes` | payload bytes of the quarantine rows |
| `oldest_age_s` | age in seconds of the oldest queue row (`min(received_at)`; the lowest-`id` row when the scan times out, flagged approximate) |

Storage is sampled every 5 minutes into `storage_hour`, `storage_day` and `storage_month` (`last`, `max`, `sum`, `n` per measure). The categories do not overlap and reconcile: measure 5 equals measures 1 + 2 + 3 + 4 exactly (a difference is flagged, not hidden).

| Storage measure | Definition |
|---|---|
| `torrents_table_bytes` | `pg_table_size('magneticod.torrents')` (heap, FSM, VM, TOAST relation and TOAST index; no ordinary index) |
| `blobs_table_bytes` | `pg_table_size('magneticod.torrent_file_blobs')`, the same composition |
| `indexes_bytes` | sum of `pg_indexes_size` over every table and materialised view in `magneticod` (all ordinary indexes) |
| `other_bytes` | sum of `pg_table_size` over every other table and materialised view in `magneticod`, plus every sequence; their ordinary indexes are in `indexes_bytes`, never here |
| `magneticod_total_bytes` | sum of `pg_total_relation_size` over the relation kinds `r`, `m` and `S` of the schema, by an independent expression |
| `ingest_total_bytes` | the same sum for `magnetico_ingest` |
| `database_total_bytes` | `pg_database_size(current_database())` |
| `indexed_torrents_estimate` | `n_live_tup`, else `reltuples`; always labelled approximate |

`bytes_per_torrent` is derived, never stored: `magneticod_total_bytes ÷ indexed_torrents_estimate` from the same sample, unavailable when the estimate is 0 or unknown; for a period it is the ratio of sums. WAL, backups, other databases and server disk are not counted.

## 5. Rates, windows, coverage and freshness

| Quantity | Formula | Unit |
|---|---|---|
| rolling rate, window W ∈ {60 s, 15 min, 60 min} | `Σ count_i(W) ÷ covered_i(W)` over fresh instances with `covered_i ≥ W/2` and a `window_end` within 30 s of the newest; excluded instances are listed | per second / minute / hour |
| completed calendar bucket | the stored count (never a rate) | per hour / day / month |
| average over a completed bucket | `count ÷ (covered_minutes × 60)`, only when `covered_minutes > 0`, flagged "partial" below the full length | per second |
| current (open) hour | count so far, labelled partial | count |
| weighted mean | `Σ sum ÷ Σ count` of stored pairs | per metric |

Rolling windows come from an in-memory ring of 3,600 one-second slots (the 60 s, 15 min and 60 min windows are sums over it) and use only completed seconds. **Coverage** is the set of UTC minutes in which a process was running and recording: `agg_hour.covered_mask` holds it as a bitmask merged with bitwise OR, so two instances in the same minute count once and coverage never exceeds the bucket length; day and month rows count the newly covered minutes (`covered_minutes`). A bucket with no row is a **missing observation**; a row with `covered_minutes > 0` and zero counters is a **measured zero**.

Three independent freshness clocks decide what the web shows: instances (`snapshot.published_at`: up under 30 s, stale under 5 min, else unavailable), queue state (`shared_state.queue_sampled_at`: fresh within 90 s, stale within 15 min, else unavailable) and storage (`storage_sampled_at`: fresh within 15 min, stale within 1 h, else unavailable). A failed queue sample leaves the instance "up" with the queue fields shown unavailable and their age. Nothing unavailable is rendered as zero.

## 6. Schema `magnetico_stats` version 1

Created by `magnetico migrate stats-schema-init` (idempotent, advisory lock, requires a verified `magneticod` v1, structural verification, fail closed, never repaired or upgraded). `ingest` and `web` start without it. The columns are fixed-width numerics and timestamps: **no `bytea`, no `text`, no JSON, no label, no foreign key** (the only `inet` is the last client address of a crawler).

| Table | Key | Content |
|---|---|---|
| `stats_version` | `version` | exactly one row, `1` |
| `ingest_instances` | `instance_id` | `started_at`, `last_flush_seq`, `last_flush_at`, `heartbeat_at`: the arbiter of the flush protocol |
| `snapshot` | `instance_id` | per-instance heartbeat state, updated in place every 10 s: cumulative totals, completed-slot window counts with `covered_s`, in-flight records, flush health counters; pruned after 24 h |
| `shared_state` | `id = 1` | the shared database state: latest gauges, `queue_sampled_at`, storage measures, `storage_sampled_at`, slot markers, approximation flags |
| `crawler_activity` | `crawler_id` | per registered crawler that ever connected: `first_seen_at`, `last_seen_at`, `last_ip`, `last_submit_at`, cumulative records, bytes and requests; bounded by the registry, never by traffic |
| `agg_hour`, `agg_day`, `agg_month` | `bucket_start` | the counters of §3, the gauge columns of §4, coverage |
| `storage_hour`, `storage_day`, `storage_month` | `bucket_start` | the storage measures of §4 |

`CHECK` constraints keep `bucket_start` aligned to its unit in UTC, counters non-negative and `min ≤ max`, so the database refuses a row that violates them; the reader does not re-validate rows.

Crawler-activity merge: counters add; `first_seen_at` is `LEAST`; the pair (`last_seen_at`, `last_ip`) is replaced together only by a strictly newer `last_seen_at`; `last_submit_at` is `GREATEST` on its own, `NULL` ignored, so a delayed batch never moves a newer value back and an existence-only batch never erases a submission time.

Growth is measured, not assumed (§11 of the report): about 7.7 MiB per year of aggregates, linear.

## 7. Collection intervals and the flush protocol

| Activity | Interval | Connection |
|---|---|---|
| queue sample (elected instance) | 10 s (doubles up to 60 s when a cycle exceeds 2 s) | sampler |
| heartbeat (`snapshot` upsert) | 10 s, the first one as soon as the writer is ready | heartbeat |
| aggregate flush | 60 s, plus a drain on shutdown | flush |
| storage sample (elected instance) | 5 min | sampler |
| housekeeping (prune) | at most every 10 min, after a successful flush | flush |

The process owns three one-connection pools. A stalled flush shares no connection, goroutine or lock with the heartbeat, which reports `flush_stalled_s`.

**Sealed immutable batches.** The writer drains the recorder into a carry delta and seals it as a batch `(prev, seq)`; a sealed batch is never changed after its first attempt, newer events wait in the carry. One transaction per attempt starts with `UPDATE ingest_instances SET last_flush_seq = seq WHERE instance_id = id AND last_flush_seq = prev`; its affected-row count is the **single arbiter**: one row ⇒ apply the hour, day, month and per-crawler upserts and commit; zero rows ⇒ apply nothing and read the sequence (`seq` ⇒ it already committed, `prev` ⇒ a concurrent transaction rolled back, anything else ⇒ drop the batch, count `sequence_anomalies`, never reuse a number). A lost connection around `COMMIT` is therefore resolved by the next attempt without a double count. A batch older than 24 h is **abandoned as a tombstone** that keeps its sequence number and is resolved by the same arbiter, so there is no gap and no double count; buffered hours older than 26 hours are dropped and counted. Hour, day and month rows are locked in ascending order (`shared_state`, hours, days, months, crawlers).

A graceful shutdown makes a **final flush** bounded by 5 s, plus at most 2 s to close the statistics connections (the shutdown budget of `crawler-ingest-delivery-v1.md` §8 grows from 28 s to 35 s), that drains the sealed batch and the carry behind it. Counters lost on `SIGKILL` are bounded by the flush interval **only while persistence succeeds**; during a statistics-database outage up to 24 h of buffered hours are lost on a crash, and anything beyond the bound is dropped and counted.

## 8. Failure behaviour

| Event | Behaviour |
|---|---|
| statistics schema missing, invalid, unwritable or locked, or the kill switch | one warning; the writer backs off or is disabled; authentication and ingestion are unaffected; the dashboard says so |
| ingest schema missing or incompatible, seen by `ingest` or `ingest crawler` | refuses with the missing structure and the documented reinitialisation; never repaired |
| the same condition, seen by `web` | `web` starts; only the crawlers and quarantine pages say what is wrong |
| PostgreSQL unavailable | admission and authentication of uncached credentials answer 503; cached credentials keep authenticating until their 60 s end; accumulators keep counting in fixed memory (≤ 26 hours); heartbeats stop and the web shows stale, then unavailable |
| authentication storage failure | `503 storage_unavailable`, counted in `auth_storage_failures`; never a 401 |
| ingest restart | counters restart under a new `instance_id`; persisted aggregates continue |
| ambiguous commit | never double-counted; dropped only when unresolvable, and then counted |

A repeated failure is logged once per state, where the state is the SQLSTATE class of the error, not the driver's wrapper type; a log line never carries a driver error's text (host, user, database), only the class, or our own schema and privilege messages.

**Clock excursions.** The process clock decides the hour of an event and the slot of a shared sample. A shared sample more than 6 slots (a minute; 2 storage slots) ahead of the writer's clock at flush time is **not applied** (counted in `gauge_batches_skipped_overlap`), and a stored slot marker that far ahead — written by a clock that was wrong — gives way to the next sample that agrees with the writer's clock, so one bad reading cannot freeze the queue and storage figures. The web service shows a source more than 30 s in the future as **unavailable**. In memory, an event hour is clamped so it never goes backwards: after a forward excursion of an hour or more, later events are attributed to that (future) hour until real time catches up or the process restarts (`clock_anomaly` is set above 26 h); the next process start is the repair. The crawler-activity timestamps and `first_seen_at` follow the same clock.

## 9. Multi-instance behaviour

Per-instance data (events, windows, cache, consumer statistics) is **summed** across instances. Shared data (queue, quota, quarantine, oldest age, storage) describes the one database and is **never summed**: only the instance whose queue consumer owns the advisory lock samples it, and a sample set is applied only if its first slot (`unix_seconds ÷ 10` for the queue, `÷ 300` for storage) is greater than the stored marker, so two instances can never contribute the same period; a handover race skips the shared-gauge part of one batch (counted in `gauge_batches_skipped_overlap`, at most about a minute of samples) while its event counters are applied regardless.

## 10. Privacy rules

1. No `bytea` or `text` column exists in `magnetico_stats`; no column holds a payload, info hash, name, path, credential or digest.
2. A crawler is identified by its UUID; its `crawler_activity` row holds counts, byte totals, one address and timestamps, never a reference to a torrent.
3. Only the last client address per crawler is stored, resolved by the right-most-untrusted rule of `MAGNETICO_INGEST_TRUSTED_PROXIES` (the TCP peer when none is configured), never a raw header.
4. The administrator pages show crawler names, never tokens or digests; invitation codes and quarantine payloads and hashes are never shown (§ web-v1.md).

## 11. Qualification

The acceptance criteria that bound the cost of all of the above (instrumentation overhead, memory, write frequency, queue sampling, authentication, delivery batching, sabotage, delayed flushes, outage) were fixed before any measurement in the plan (`docs/wip/09-statistics-admin-plan.md`, "Acceptance criteria") and are answered one by one in the report.
