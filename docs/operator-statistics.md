# Operating the ingest statistics and the administrator section

This is the operator's guide to what the dashboard shows, what it cannot show, and the crawler credentials behind it. The contract is [`docs/specs/statistics-v1.md`](specs/statistics-v1.md); the deployment grants are in [`docs/web-deployment.md`](web-deployment.md).

## What is collected, and where it is read

The `ingest` service counts what it does (requests, records, bytes, outcomes, existence checks, authentication, processing) in memory and writes hourly, daily and monthly totals into the schema `magnetico_stats` of the same PostgreSQL database, once a minute. It also publishes a heartbeat every 10 seconds and, on one elected instance, samples the queue (every 10 s) and the storage sizes (every 5 min). Nothing about a torrent, a payload or a credential is stored. The `web` service reads these tables (read-only) for the section **Admin** (visible to administrators only):

| Page | Shows |
|---|---|
| Overview (`/admin`) | ingest instances; registered, enabled and recently active crawlers; rolling rates (records, new torrents, submit and existence requests, authentication lookups, cache hit rate, busy refusals and storage failures); queue and quota; storage; account counts including disabled users; the charts (throughput as grouped bars, outcomes as stacked bars, queue, traffic and efficiency, storage) by day, month and year |
| Crawlers | every registered crawler, including those that never connected and disabled ones, with last seen, last address, last submit and its totals |
| Quarantine | what the ingest queue set aside as permanently invalid: reason, attempts, size, age (never the payload or the hash) |
| Users, Invitations, Trackers | read-only listings; no password, no invitation code |

Every page is read-only. Changes are made with the command line (`magnetico web user …`, `magnetico web tracker …`, `magnetico ingest crawler …`).

## Setting it up

1. `magnetico migrate stats-schema-init` once (idempotent, uses the default account like the other `*-schema-init` commands). Without it `ingest` and `web` still start; the dashboard says "statistics schema not initialised".
2. Give the `web` role the read grants of [`docs/web-deployment.md`](web-deployment.md) (statistics tables, column-level on the crawler registry and the quarantine). Without them only the affected pages say what is missing.
3. Register crawlers (below). The default single-account deployment needs nothing else.

`MAGNETICO_INGEST_STATS_DISABLED=true` is the kill switch: no writer and none of its three database connections. The statistics use three PostgreSQL connections per `ingest` process (flush, heartbeat, sampler); count them in `max_connections`.

## Reading the numbers

**Received, acknowledged, enqueued, duplicates, settled.** A crawler sends records; every record in a valid request is *received*. Admission answers each one *acknowledged* (the 202) — either *enqueued* (a new row) or a *duplicate* (already pending) — or *refused* (queue full, storage error). The consumer then turns queue rows into torrents and *settles* each row as *stored new*, *stored existing* or *quarantined*. Therefore:

- **Backlog versus duplicate traffic.** A growing queue while `enqueued` exceeds `settled` is a **backlog**: the consumer is slower than admission. A high `duplicates` share with a flat queue is **duplicate traffic**: several crawlers discover the same torrents, which is normal and costs only the admission check.
- **Received minus acknowledged minus refused equals the records in flight** at every instant and zero when nothing is running. A small non-zero residual in one hour bucket is an admission that crossed the hour boundary; it cancels in the neighbouring bucket. After a crash the lost in-flight records stay as a residual of that period.
- **Existing versus new.** "Stored existing" includes torrents created by an earlier attempt whose row deletion failed; the separate line `torrents created` is the exact creation count.

**Stale, unavailable, missing and zero are four different things.**

| Shown | Means |
|---|---|
| a value | measured and fresh |
| **stale** (with its age) | the last good value; the sampler or the heartbeat is late |
| **unavailable** | no usable value (never rendered as zero) |
| a **gap** in a chart | no observation for that period (the process was not running, or it was not recorded) |
| a zero | measured zero (the process was observing and nothing happened) |

The queue, the storage figures and the instances have separate ages: a failed queue sample does not make an instance look down.

**Partial buckets.** The current hour, and any hour or day during which the service was not running the whole time, is marked partial: its count is what was observed, and rates are computed over the observed time (**coverage**), not over the nominal hour.

**Approximate counts.** The indexed-torrent count is the planner's estimate and lags after bulk imports until autovacuum or `ANALYZE`; it is always labelled approximate. If counting a very large queue exceeds its budget, the queue row count falls back to an estimate and the oldest age to the lowest-id row, both flagged; queue **bytes** are never estimated, and **quota use** (exact, kept by the database) stays available.

**Quota.** Quota use is the queue plus the quarantine, because quarantined payloads count toward the queue limit until an operator acts; a full quarantine is a reason for `queue full` refusals.

**Crawler activity.** *Recently active* means: the crawler is enabled and the statistics last saw an authenticated request from it within the last **15 minutes**. The statistics are persisted once a minute, so the figure can lag by about that much, and the cached page adds up to ten seconds. The Crawlers page tells four situations apart: **active**, **inactive** (connected before, not recently), **never connected** (no request recorded) and **disabled** (refused with 401, whatever its history); when the statistics cannot be read the activity of an enabled crawler is **activity unavailable**, never "never connected". The registry counts are capped at 10,000 (shown as "10,000+") so their cost does not depend on the registered count.

**Charts.** Every chart axis, tick and legend time is UTC whatever the browser's time zone is. The throughput chart draws grouped bars and opens with *enqueued* and *settled* visible (they show a backlog growing); *received* and *acknowledged* are one click away in the legend. The outcome chart stacks the mutually exclusive settlements. Under each chart a caption says when the data was read, and that it is the last known state when a refresh failed. Buckets that rest on estimated queue counts, on an incomplete storage breakdown (a measure was unavailable: no stack is drawn), on a mismatch of the storage categories, or on a reconciliation that could not run are listed under the chart and marked in its text. Quota use in history is the utilisation recorded with the quota in force when each sample was taken, so changing the configured quota never rewrites old buckets.

**Several `ingest` instances.** Event counts are summed. Queue, quota and storage figures describe the one database and are shown once, never summed. The dashboard lists any instance left out of a rate (not fresh, or too young for the window).

## Crawler credentials

Each crawler has exactly one credential, `mgc_` followed by 43 characters, generated by the server. Only its SHA-256 digest is stored. The token is shown **once**, by `add` and `rotate-token`, and cannot be recovered.

```
magnetico ingest crawler add <name>          # registers and prints the id, the name and the token
magnetico ingest crawler list                # id, name, enabled, created, rotated, disabled — never a token
magnetico ingest crawler rotate-token <name> # replaces the token; the old one stops working
magnetico ingest crawler disable <name>      # the crawler is refused (401)
magnetico ingest crawler enable <name>
```

The crawler receives its token in `MAGNETICO_CRAWL_INGEST_TOKEN`. Names are 1–64 characters (`A–Z a–z 0–9 . _ -`, starting alphanumeric) and unique ignoring case.

**The 60-second delay.** A running `ingest` keeps a positive authentication result for exactly 60 seconds from the moment it asked the database, never extended by use. Consequently `rotate-token` and `disable` take effect on every running instance **within 60 seconds** (the commands print this); a new or newly enabled credential works at once. During that window the old token still authenticates. If the database is unavailable, an uncached credential gets `503` (a crawler retries), never `401`.

**A flood of invalid tokens** cannot exhaust the database: lookups are limited to 8 at a time with immediate `503 busy` beyond the bounds. Forged tokens may use only a share of those bounds, so a crawler that has authenticated before is still re-validated every 60 seconds during a flood (measured with a 90 s flood of ten million forged requests: no legitimate request failed); a brand-new credential used for the first time during a flood can still get `503 busy` and retries.

## When something looks wrong

| Symptom | Likely cause |
|---|---|
| "statistics schema not initialised" | run `migrate stats-schema-init`; ingestion is unaffected meanwhile |
| Overview shows the instance **stale** or **unavailable** | the `ingest` process stopped or cannot reach PostgreSQL; check its log (one line per state change) |
| Crawlers page: "ingest schema … not compatible" | an older development `magnetico_ingest` without the registry: reinitialise it as in `UPGRADE.md` (only that schema is dropped) |
| Crawlers page: privileges named | the `web` role lacks the column grants in `docs/web-deployment.md` |
| a crawler is rejected with 401 right after `add` | the token was copied wrongly, or the crawler uses the removed shared variable `MAGNETICO_INGEST_TOKEN`; the service refuses to start with that variable set |
| Queue and storage figures frozen as "up" or shown unavailable after a clock correction on the `ingest` host | an `ingest` clock that was far ahead for a while: since this version a shared sample from a clock ahead of the writer is refused and a pinned marker heals by itself; keep the host clock synchronised (the dashboard shows a source far in the future as unavailable) |
| `ingest` refuses to start naming a missing table or privilege | the ingest schema is incomplete or the role lacks `SELECT` on the registry; nothing is repaired automatically |

The statistics never repair, upgrade or delete anything on their own, and nothing removes old aggregates in 0.8.0: the measured growth is about 8 MiB per year.
