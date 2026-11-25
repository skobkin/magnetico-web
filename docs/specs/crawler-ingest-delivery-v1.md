# Magnetico 0.8.0 — Crawler → Ingest Delivery Contract (draft v1)

**Status:** DRAFT for owner review (Task 06A, revision 3). Not yet normative: nothing here
may be implemented until the owner approves it. It builds on `wire-format-v1.md` and
`postgresql-persistence-v2.md` and changes neither. Section 15 lists the decisions the
owner still has to confirm.

**Scope split.** Every requirement is **0.8.0 (mandatory)** unless it sits in §14
(post-release). Words MUST/SHOULD are used in their RFC 2119 sense.

## 1. Goals and non-goals

The crawler stops depending on PostgreSQL. It discovers infohashes, fetches metadata and
delivers structured records over authenticated HTTPS to an `ingest` service, which queues them
durably in PostgreSQL and persists them asynchronously through the existing `Store.AddTorrent`.

- Delivery is **at-least-once**; persistence is **idempotent by infohash** (`AddTorrent` returns
  `created=false` for known hashes).
- DHT discovery is **best-effort**. A hash that was only observed, or metadata that was fetched but
  not yet durably enqueued, may be lost. That is not a violation of any guarantee in this document.
- Not in 0.8.0: pending-queue lookup in existence checks, adaptive backpressure, smart queue
  filling, queue compression, more than one queue backend, parallel server consumers,
  advanced cache algorithms.

## 2. Components and guarantees

```
crawl (no PostgreSQL)                                       ingest (same binary, PostgreSQL)
DHT → event loop → precheck batcher ─HTTPS→ POST …/exists → LRU cache → batched SELECT
        │ missing | unknown (fail-open)
        ▼
 admission gate (active-leech semaphore AND local queue not full)
        ▼
 metadata.Sink (bounded, ctx-aware) → drain (fixed cap)
        ▼
 enqueue writer: ValidateIngest → SQLite group commit (synchronous=FULL)
        ▼
 sender: oldest batch ─HTTPS→ POST …/torrents → validate all → INSERT magnetico_ingest.queue
 202 ⇒ delete local rows                          COMMIT (synchronous_commit=on) ⇒ 202
                                                          │
                                  single consumer → AddTorrent → DELETE queue row, promote cache
```

| Boundary | Guarantee |
|---|---|
| Local enqueue | Record is *admitted* only after the SQLite transaction commits with `synchronous=FULL`. |
| HTTP 202 | Every submitted record is **durably committed in the PostgreSQL queue** (or was already pending there). It does **not** mean the torrent is in `magneticod.torrents`. |
| Local delete | The crawler MAY delete a row only after an HTTP 202 status for the request containing it. |
| Consumer | A queue row is deleted only after `AddTorrent` succeeded (created or already present), or moved to quarantine in one transaction. |

**Durability assumptions (explicit).** SQLite: local filesystem that honours `fsync`
(ext4/xfs; not NFS/SMB), device honouring cache flushes. PostgreSQL: `fsync=on` (ingest refuses
to start otherwise unless `MAGNETICO_INGEST_ALLOW_NON_DURABLE_DB=true`, development/tests only)
and `synchronous_commit=on` for queue admission (`SET LOCAL` inside the admission transaction).
With replicas, whether 202 survives primary loss depends on the operator's `synchronous_standby_names`;
that is out of scope here and must be stated in operator docs.

## 3. Record format v1

One JSON object per torrent; the same bytes are stored locally, sent over HTTP and stored in the
PostgreSQL queue (`BYTEA`, never JSONB, which would normalise content):

```json
{"v":1,"info_hash":"<40 lowercase hex>","name":"…","total_size":123,"discovered_on":1700000000,
 "files":[{"path":"…","size":1}]}
```

- No raw bencode and no codec blob. Compression is out of scope; the queue `format` column is reserved.
- `persistence.ValidateIngest` MUST run before marshalling (crawler) and again at admission (server).
  It guarantees valid, NUL-free UTF-8, so JSON encoding is lossless (Go's encoder would otherwise
  silently replace invalid bytes). Marshalling uses `SetEscapeHTML(false)`.
- Decoding is strict: unknown fields, unknown `v`, trailing data and a bad `info_hash` are errors.
- The uint64→int64 `total_size` guard (`torrentFromMetadata`) moves into the record package and runs
  before enqueue.
- **`MaxRecordBytes` = 16 MiB** of serialized JSON. There is **no guaranteed relation** between the
  size of the bencoded metadata (≤ 10 MiB) and its JSON form: the JSON can be smaller, or larger (for
  example control characters escape to `\u00XX`, up to 6×). The explicit limit is therefore the only
  contract. A record whose JSON exceeds it is quarantined locally with reason `record_too_large`; it is
  never sent, truncated, retried or deleted. The measured sample maximum (≈ 9.3 MB, estimated from the
  formula in §11) is informational only.

## 4. Crawler queue — SQLite WAL

The crawler is deployable as a single container with no PostgreSQL or Redis access.

**Driver / pragmas.** `modernc.org/sqlite` (pure Go, keeps `CGO_ENABLED=0`; cost: a large
dependency). `journal_mode=WAL`, `synchronous=FULL`, `auto_vacuum=INCREMENTAL` (set at creation),
`busy_timeout=5000`, `journal_size_limit=64MiB`, explicit `cache_size`.

**WAL growth is not hard-bounded.** `journal_size_limit` only makes SQLite truncate the WAL file
*after* a checkpoint has completed and the WAL has been reset; it is **not** a ceiling on disk usage.
The WAL can grow beyond it while a checkpoint cannot complete: a long-lived reader (a snapshot pins
every frame written after it), a failing or blocked checkpoint, or one very large transaction. Rules:
readers (`Peek`, diagnostics) MUST be short-lived and never held across network I/O; the checkpoint
runs automatically (autocheckpoint) plus a periodic explicit attempt (`PASSIVE`, then `TRUNCATE` when
idle). A failed or incomplete checkpoint is logged (throttled), counted and retried — it is not fatal.
The WAL file size is exported as a counter and a warning is logged above 256 MiB. The byte quota below
counts payload bytes only (not WAL, index overhead or freelist), so operators MUST provision disk
headroom beyond the quota; a full disk surfaces as `SQLITE_FULL`.
**Schema.** `PRAGMA user_version` = queue schema version (mismatch → refuse to start).

```sql
queue(id INTEGER PRIMARY KEY AUTOINCREMENT, info_hash BLOB NOT NULL UNIQUE, format INTEGER NOT NULL,
      payload_bytes INTEGER NOT NULL, enqueued_at INTEGER NOT NULL, payload BLOB NOT NULL);
quarantine(id INTEGER PRIMARY KEY AUTOINCREMENT, info_hash BLOB NOT NULL, format INTEGER NOT NULL,
      payload_bytes INTEGER NOT NULL, reason_code TEXT NOT NULL, http_status INTEGER,
      quarantined_at INTEGER NOT NULL, payload BLOB NOT NULL);
```

`payload` is deliberately the **last** column: SQLite reads a record's columns in order, so scans that
need only `payload_bytes` (counters at open, `Peek` size planning) never follow the payload's
overflow-page chain. `enqueued_at` and `quarantined_at` are Unix nanoseconds.

`INSERT … ON CONFLICT(info_hash) DO NOTHING` removes local duplicates (queue dedupe only, not an
existence answer).

**Concurrency.** One writer goroutine with group commit: a group holds ≤ 64 records and, optionally,
lingers ≤ 50 ms for more. The default is **no linger** (measured in M2: a lone producer was limited to
~19 records/s by the 50 ms wait, versus ~600 records/s without it): the writer commits what is already
waiting, and groups still form while the previous commit's fsync runs (30–60 records per group with 64
concurrent producers). Ack, quarantine and maintenance run on the same goroutine; short read
transactions. An exclusive `flock` on the queue directory allows one crawler per queue. Periodic
`wal_checkpoint(TRUNCATE)`; `incremental_vacuum` when the freelist exceeds a threshold (file size is
otherwise the high-water mark).

**Startup.** Normal SQLite WAL recovery only — **no unconditional integrity scan**. The optional
diagnostic `magnetico crawl check-queue` runs `quick_check`/`integrity_check` and prints counts. It
takes the directory lock (so it refuses to run against a live crawler), opens the database read-only
and never repairs anything. M2 delivers it as the library function `crawlqueue.Check`; the CLI wiring
lands in M6 together with the queue-directory option.
Corruption is never repaired or deleted automatically.

**Capacity.** `MAGNETICO_CRAWL_QUEUE_MAX_BYTES` (default 1 GiB) bounds `SUM(payload_bytes)` of
`queue` + `quarantine`. The counter is held in memory (safe: single writer, process-exclusive via
`flock`) and initialised from `SUM()` at open.

**Errors.** `SQLITE_FULL` → queue reports full. `SQLITE_IOERR`/corruption at runtime → the process
exits non-zero; committed data stays.

**Storage interface** (replaceable, one implementation): `Append`, `Peek(maxRecords,maxBytes)`,
`Ack(ids)`, `Quarantine(ids, reason, httpStatus)`, `Stats`, `Close`. `Append` never blocks for space;
when full it returns `ErrQueueFull` immediately.

**Queue-full and shutdown lifecycle.** When full, the admission gate stops starting leeches.
Leeches already in flight (≤ active-leech limit) may finish; their metadata is dropped as best-effort
data, counted (`crawl_dropped_queue_full`) and logged throttled. On SIGTERM the writer drains the
`drain` channel for at most a 5 s grace; whatever remains is dropped and counted
(`crawl_dropped_shutdown`). The writer is never awaited indefinitely; commits are bounded by
`busy_timeout`. Rows already committed are unaffected.

**Fail-open precheck.** If the existence API is unavailable, the crawler may fetch metadata without a
successful check and persist it locally, up to the byte limit; after that it stops admitting metadata.
Duplicates created this way are harmless (idempotent server side).

## 5. HTTP protocol (version 1)

Base path `/api/ingest/v1`. TLS is terminated by Traefik; ingest listens on plain HTTP on the
internal network. The crawler requires an `https://` URL, except loopback `http://` (tests).

**Authentication.** `Authorization: Bearer <token>`. Every crawler has its own credential:
`mgc_` followed by at least 32 random bytes (unpadded base64url), generated by the server
(`magnetico ingest crawler add`) and stored only as a SHA-256 digest in `magnetico_ingest.crawlers`
(§6.1). Exactly one token is valid per crawler; rotation replaces it. The presented string is hashed
immediately and looked up by digest through a unique index; the plaintext is never stored, cached or
logged. The crawler's identity (a server-generated UUIDv4 and a unique name) is derived **only** from
the authenticated credential: there is no identity header and no identity file.

The check order is method, then authentication, then content type; the body is not read before
authentication succeeds. A missing, malformed, unknown or disabled credential all answer
`401 unauthorized` with `WWW-Authenticate: Bearer` (no oracle between them). When authentication
cannot be decided — a database error or timeout, or the bounded lookup state is full — the answer is a
retryable `503 storage_unavailable` or `503 busy` (never a false `401`; the crawler retries and never
quarantines or bisects because of it).

A positive cache keeps healthy crawlers off the database: key = the digest, entry life **60 s measured
from the start of the lookup that produced it** (fixed, never extended by use or by a database failure),
at most 4,096 entries, concurrent lookups of one digest coalesced. Lookup coordination is bounded: at
most 8 concurrent queries, 64 distinct pending digests and 1,024 waiting requests, each lookup with a 2 s
deadline and a ≤ 250 ms wait for a slot; requests beyond the bounds get `503 busy` at once. **A reserved lane
protects a known crawler from a flood of forged credentials:** a digest that has authenticated before (a hint
kept beyond the cache entry's life, bounded like the cache) may use all of the bounds, while an unknown digest
may use at most 6 of the 8 queries, 48 of the 64 pending digests and 768 of the 1,024 waiting requests, so
the re-validation of a legitimate crawler whose 60 s entry just expired is never starved (measured with a
90 s flood of forged tokens: no legitimate request failed). A disabled,
rotated-away or removed credential is therefore accepted by a running ingest for **at most 60 s** after
the change (**accepted and documented**); a new credential works immediately. The crawler reads its own
token from `MAGNETICO_CRAWL_INGEST_TOKEN` (environment only: no CLI flag, no YAML field; never logged or
echoed in errors).

### 5.1 `POST /api/ingest/v1/torrents/exists`

Request `{"info_hashes":["<40 hex>", …]}` — ≤ 1024 hashes, body ≤ 64 KiB. Response 200:
`{"exists":[…],"missing":[…],"unknown":[…]}`. `exists` only for hashes **confirmed in PostgreSQL**;
`missing` only for confirmed absence; a database error (or an inconclusive lookup) yields `unknown`,
never `missing`. Pending queue rows are not consulted. Malformed input → 400; whole-request
database failure → 503 `storage_unavailable`.

Server flow: deduplicate → cache → join in-flight lookups for concurrent misses → one
`SELECT info_hash FROM magneticod.torrents WHERE info_hash = ANY($1)` through a new **additive**
Store method `ExistingInfoHashes` (§15 O-3). Concurrency cap 16.

### 5.2 `POST /api/ingest/v1/torrents`

Request `{"records":[<record v1>, …]}` — ≤ 256 records, body limit 17 MiB (`MaxRecordBytes` +
1 MiB envelope). The sender packs ≤ 8 MiB per request; a record between 8 and 16 MiB travels alone.

**All-or-nothing admission.** The server decodes and validates *every* item (strict decode, known
`v`, size ≤ `MaxRecordBytes`, `ValidateIngest`). Any failure rejects the **whole request** and
**nothing is written** (the error codes are defined in §5.3). Otherwise, in one transaction:

1. `SET LOCAL synchronous_commit = on`.
2. One multi-row `INSERT … ON CONFLICT (info_hash) DO NOTHING`, rows sorted by `info_hash` (no
   deadlock between concurrent batches).
3. Statement-level triggers update `magnetico_ingest.usage` (§6.2); its row lock serialises admissions.
4. `used_bytes` above `MAGNETICO_INGEST_QUEUE_MAX_BYTES` → ROLLBACK → `503 queue_full`.
5. COMMIT, then `202`.

**HTTP 202 is the complete acknowledgment.** Status 202 alone means every item of the request was
durably queued or already pending. The server sends `{"accepted":<n>}` as an informational body; the
client MUST NOT parse, validate or compare it — an empty, unparsable or "mismatched" body on a 202 is
still an acknowledgment. Any other 2xx status (200, 204, …) is **not** an acknowledgment. Duplicates of
rows already in `torrents` are resolved by the consumer (idempotent).

### 5.3 Status codes and error body

200, 202, 400, 401, 405, 413, 415, 503 (`queue_full`, `storage_unavailable`, `busy`; with
`Retry-After`), 500. Error body `{"error":{"code":"…","retryable":true|false}}`; never contains record
contents, tokens or driver text.

**Rejection codes for `POST …/torrents`.** The code depends on the number of records in the request so
that a permanently invalid record is always identified individually:

| Request | Failure | Status / code |
|---|---|---|
| > 1 record | any item fails validation | `400 invalid_batch` (diagnostic item indexes/codes only) |
| > 1 record | any item exceeds `MaxRecordBytes` | `413 payload_too_large` |
| exactly 1 record | unknown `v` | `400 unsupported_version` |
| exactly 1 record | record fails strict decode or `ValidateIngest` | `400 invalid_record` |
| exactly 1 record | the record exceeds `MaxRecordBytes` | `413 record_too_large` |
| any | request body > 17 MiB (the record count is unknown when reading stops) | `413 payload_too_large` |
| any | envelope itself malformed (not JSON, missing `records`, > 256 records) | `400 invalid_request` |

Precedence for a single record: `unsupported_version` > `record_too_large` > `invalid_record`. For a
multi-record request, 413 wins over 400 when both kinds of failure are present. `413` is also what
`http.MaxBytesReader` yields when the body limit is exceeded.

**Record-specific codes.** Exactly three codes blame one record and are the only evidence on which the
crawler quarantines automatically: `invalid_record`, `unsupported_version` and `record_too_large`, each
returned for a request that contained exactly one record, as the documented JSON error body
(`Content-Type: application/json`, `{"error":{"code":…}}`). Everything else — `invalid_request`,
`invalid_batch`, `payload_too_large`, any other code, and any 400/413 that is not a well-formed ingest
error body (for example a reverse proxy's own 413 page) — says nothing about the record's content.

### 5.4 Client behaviour (deterministic)

| Response | Action |
|---|---|
| Status 202 (body ignored) | delete every row of the request |
| 400 or 413 (any body), request has > 1 record | **bisect**: resubmit each half as its own request, recursively (≤ 8 levels for 256 records). Splitting is always safe, so it does not depend on recognising the response |
| 400 `invalid_record` / `unsupported_version`, or 413 `record_too_large` — **recognised record-specific error** (§5.3) — request has exactly 1 record | **permanent per-record failure**: move that record to **local quarantine** (payload preserved byte-for-byte) with reason = the server's code and the HTTP status; continue with the next record |
| Any other 400 or 413 on exactly 1 record: `invalid_request`, `payload_too_large`, an unknown code, a malformed/empty/non-JSON body, an HTML page from a proxy | **unrecognised rejection**: the record is **kept** (never quarantined), the sender backs off (60 s) and emits an actionable diagnostic (below); it retries after the backoff |
| Retryable result (5xx, 429, timeout, reset) during a bisect | abort the bisect, back off, restart from the head of the queue |
| Any other status, timeout, connection reset, 2xx other than 202 | **ambiguous / not acknowledged**: keep rows and resend later (safe: idempotent) |
| 401 / 403 | keep rows, long backoff, loud throttled log |

Properties: (a) a permanently invalid record is always isolated by bisection to a single-record request,
where the server's code is specific, so the same rule applies at any recursion depth; (b) quarantine is
durable (one SQLite transaction moves the row), so after a crash during a bisect the already-quarantined
records are not retried and the remaining records restart from the head; (c) a record is quarantined only
on a recognised, record-specific application error from ingest — a generic `400 invalid_request`, a
reverse proxy's 413, or any unknown or malformed response proves nothing about the record and never
quarantines it, so a misconfigured proxy or an incompatible server cannot move good records out of the
queue; (d) a single-record request is never quarantined for 401/403/429/5xx/timeouts.

**Diagnostics for unrecognised rejections.** One throttled (≤ 1 per minute) log line carries: HTTP status,
`Content-Type`, whether the body parsed as the ingest error envelope, the error code if any, the number of
records in the request, and at most 200 printable bytes of the body with control characters replaced
(never the request, the token or record contents), plus the hint "the server or a proxy in front of it
rejected a request the crawler believes is valid: check the ingest version, `client_max_body_size`-style
proxy limits and the TLS terminator". A counter `crawl_send_unrecognised_rejections` is exported with the
other delivery counters.

**Systemic-failure guard.** If ≥ 8 consecutive records are quarantined by recognised record-specific
errors without an intervening 202, the sender stops quarantining, backs off (60 s) and logs loudly (a
server that is wrong about every record — for example after a deploy with a different `MaxRecordBytes` —
would otherwise empty the queue into quarantine; records are retained either way). Quarantined records
can be returned to the queue by an operator after the cause is fixed (SQL documented; CLI is
post-release).

Timeouts: exists 10 s, submit 60 s. Backoff 1 s → 60 s with full jitter. A lost 202 after a durable
commit is resolved by resending (`ON CONFLICT DO NOTHING`).

**Batching and transport (client).** The crawler is a background bulk deliverer, tuned for fewer requests
rather than per-record latency; the durable-delivery guarantees above are unchanged.

- *Existence precheck:* a batch is sent when it holds **1,024** results (the server maximum; 1,024 hashes
  are ≈ 44 KiB of JSON, below the 64 KiB body limit, and the response stays below the 64 KiB the client
  reads) or **1 s** after its first result, whichever is first; an empty batch is never sent. The linger is
  a monotonic in-process timer.
- *Metadata submission:* time-based batching with early dispatch. The oldest queued record waits at most
  **1 s** (`SendLinger`) for company; a batch is sent **at once** when it cannot grow: it holds 256
  records (a hard protocol limit, not a batching target), or the *next* queued record would not fit in the
  8 MiB budget (however small the batch still is), or the first record alone is at least 8 MiB (a
  singleton, never split). During backlog recovery full batches follow each other back to back with one
  request in flight. In steady state this is about one request per second per endpoint. An append no
  longer sends immediately. The linger is armed once per oldest record on the monotonic clock; because
  `EnqueuedAt` is persisted wall-clock time, its age is clamped to `[0, SendLinger]`, so no clock step
  can make a record wait longer than `SendLinger`.
- *Shutdown:* a pending linger is cut short and one last dispatch is attempted within the existing grace
  period; the rows stay queued if it fails.
- *Connections:* bodies are drained and closed, so consecutive requests reuse one connection; the client
  closes an idle connection after 90 s, before the server's 120 s idle timeout. Over TLS the default
  transport negotiates HTTP/2 automatically (ALPN); plain `http://` (loopback only) uses HTTP/1.1
  keep-alive. Cleartext HTTP/2 is not used (see the Task 09 report for the measurement).

### 5.5 Server limits

`ReadHeaderTimeout` 10 s, `ReadTimeout`/`WriteTimeout` 60 s, `IdleTimeout` 120 s,
`MaxHeaderBytes` 16 KiB, `http.MaxBytesReader` on every handler. At most 8 concurrent submit
handlers → raw request bodies ≤ 8 × 17 MiB = **136 MiB** (this excludes JSON decoding, Go allocations
and processing overhead and is **not** a bound on process memory); further requests get `503 busy`.

### 5.6 Precheck outage

Unknown results pause lookups for a backoff window while leech admission continues below the local
byte limit (bounded fail-open). Best-effort discovery and durable delivery are separate: only records
already committed to SQLite are protected.

## 6. Server queue — PostgreSQL

### 6.1 Schema `magnetico_ingest`

```sql
ingest_version(version INT)                      -- exactly {1}
queue(id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, info_hash BYTEA NOT NULL UNIQUE,
      format SMALLINT NOT NULL, payload BYTEA NOT NULL, payload_bytes INT NOT NULL,
      received_at TIMESTAMPTZ NOT NULL DEFAULT now(), attempts INT NOT NULL DEFAULT 0,
      next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(), last_error_code TEXT)
quarantine(id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, info_hash BYTEA NOT NULL, format SMALLINT NOT NULL,
      payload BYTEA NOT NULL, payload_bytes INT NOT NULL, reason_code TEXT NOT NULL, detail TEXT,
      attempts INT NOT NULL, received_at TIMESTAMPTZ NOT NULL, quarantined_at TIMESTAMPTZ NOT NULL DEFAULT now())
usage(id SMALLINT PRIMARY KEY CHECK (id = 1), used_bytes BIGINT NOT NULL CHECK (used_bytes >= 0))
crawlers(crawler_id UUID PRIMARY KEY,                -- server-generated UUIDv4
      name TEXT NOT NULL,                            -- 1-64 chars [A-Za-z0-9][A-Za-z0-9._-]*, UNIQUE on lower(name)
      token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),   -- SHA-256 of the token
      enabled BOOLEAN NOT NULL DEFAULT true, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
      token_rotated_at TIMESTAMPTZ, disabled_at TIMESTAMPTZ)
```

The crawler registry (`crawlers`, no triggers) is part of the **initial** version-1 definition: the
schema had not been released when per-crawler credentials replaced the shared token, so there is no
version bump and no upgrade path. An older development schema without `crawlers` fails the structural
verification at startup (`missing table crawlers`) and is **never** repaired or altered automatically;
the manual reinitialisation (which touches only `magnetico_ingest`) is in `UPGRADE.md`. The ingest role
needs `SELECT` on `crawlers`; the operator role of `ingest crawler …` needs `SELECT`, `INSERT` and
`UPDATE`; a web role, if it reads the registry, needs only column-level `SELECT` on every column except
`token_hash`.

Aggressive autovacuum storage parameters on `queue`. **Table names MUST NOT be `torrents`, `files`,
`torrent_file_blobs` or `schema_version`**: `persistence/schema.go` classifies tables with those
names in *any* schema as legacy markers.

### 6.2 Transactional quota

Statement-level `AFTER INSERT`/`AFTER DELETE` triggers with transition tables on `queue` and
`quarantine` add/subtract `SUM(payload_bytes)` in a single `UPDATE usage`; a `BEFORE UPDATE OF
payload_bytes` trigger rejects mutation. Accounting is thus exact for every path, including manual
operator SQL:

| Path | Effect on `used_bytes` |
|---|---|
| Admission insert | + |
| Consumer acknowledgement (DELETE) | − |
| Queue → quarantine (INSERT + DELETE, one transaction) | net 0 |
| Operator deletes quarantine rows | − |

Because admissions serialise on the `usage` row, concurrent requests and multiple ingest instances
cannot exceed the quota. A read-only startup check logs a warning if `used_bytes` differs from
`SUM(queue)+SUM(quarantine)`. Quarantine counts toward the quota; retention is until an operator acts
(0.8.0: documented SQL and counters; CLI is post-release).

### 6.3 Initialisation and migration ordering

- New explicit command **`magnetico migrate ingest-schema-init`**: non-destructive, idempotent, under
  `pg_advisory_xact_lock`, requires a verified `magneticod` schema at version 1, creates only
  `magnetico_ingest`. `migrate schema-init` is unchanged; nothing is created at startup.
- Order: `schema-init` → `migrate torrents` → verify → (optional `cleanup`) → `ingest-schema-init` →
  start `ingest`.
- `ingest` startup is read-only: it refuses to start while an import run is in progress or incomplete
  (`migrate torrents` requires an empty target) and logs that new inserts make earlier verification
  verdicts stale.
- Tests (M4) prove the presence of `magnetico_ingest` changes neither persistence startup
  verification nor migrate preflight/verify.

### 6.4 Single consumer and advisory lock

One sequential consumer for 0.8.0 (expected load 10–50 torrents/s — an estimate, not a guarantee).

**Ownership.** A session-level `pg_try_advisory_lock(key)` held on a **dedicated pinned connection**
(`pgx.Connect` with the queue DSN, outside any pool) for the whole consumer lifetime. It is never
acquired through a pooled query.

- *Acquire:* open the connection, `pg_try_advisory_lock`; `false` → standby, retry every 10 s.
- *Verify:* before each batch, `SELECT 1` on the pinned connection and a `pg_locks` check for our
  `pg_backend_pid()`; any failure means ownership is lost.
- *Connection lost:* stop immediately (in-flight work is idempotent), close, back off, reacquire from
  scratch. Queue work runs on the pool; the pinned connection only proves ownership.
- *Graceful shutdown:* finish the current record, `pg_advisory_unlock`, close the connection.

**Loop.** `SELECT … FROM queue WHERE next_attempt_at <= now() ORDER BY id LIMIT 32`; per row:
`AddTorrent` (own transaction, unchanged contract) → on success (created or already present)
`DELETE` the row and promote the cache entry. A crash between the two repeats the row harmlessly.

| Error | Action |
|---|---|
| `ErrInvalidMetadata` / record decode failure | quarantine (INSERT+DELETE, one transaction) |
| `ErrStorageIO` / connection loss | global backoff 1 s → 60 s, row stays |
| Anything else | `attempts++`, backoff via `next_attempt_at`; after 10 attempts quarantine `max_attempts` |

A poison record cannot block the queue. Deleted tuples are reused after autovacuum; disk returns to the
OS only with `VACUUM FULL` (documented).

### 6.5 Connections

The Store keeps its pool. The queue uses one small separate pool (max 4) plus the pinned lock
connection. `MAGNETICO_INGEST_QUEUE_DATABASE` optionally supplies a different DSN/role (DML on
`magnetico_ingest` only); default is the same DSN. A separate server is not required.

### 6.2 Statistics and the administrator section (Task 09)

The ingest service counts its own operation in memory and persists hourly, daily and monthly totals into the optional schema `magnetico_stats` (created by `migrate stats-schema-init`; `ingest` starts without it and never depends on it). Admission, authentication and the durability contract of §5–§6 do not change and cannot be changed by a statistics failure; the contract of what is counted, its boundaries, the flush protocol and its failure behaviour is [`statistics-v1.md`](statistics-v1.md). The `web` service shows the result in a read-only administrator section ([`web-v1.md`](web-v1.md) §6.1.2).

## 7. Existence cache

One bounded, process-local LRU (array-backed list + `map[[20]byte]int32`) behind an
`ExistenceCache` interface so an implementation can be replaced later without protocol changes.

- Budget `MAGNETICO_INGEST_CACHE_MAX_BYTES`, default 8 MiB; entry count derived from a documented
  per-entry size, verified by a heap test.
- **Positive entries**: no TTL (until eviction/restart). Valid because the torrent store is append-only
  in normal operation (no deletion mechanism is planned); documented assumption.
- **Negative entries**: TTL 20 s (new torrents arrive continuously).
- `MAGNETICO_INGEST_CACHE_DISABLED=true` disables it.
- A confirmed `AddTorrent` promotes the entry to positive. Database errors are never cached as
  negative. Counters: hits, misses, DB lookups, DB errors.
- A late "missing" answer never downgrades a positive entry (a lookup that began before the insert).
- **Implementation (M3).** `internal/ingest/cache` holds the LRU (`ExistenceCache`, `LRU`, `Disabled`);
  capacity = budget / `EntryBytes` (96 B documented; measured ≈ 83 B/entry at the default 8 MiB →
  87,381 entries, see `TestHeapBudget`); the map and node array are pre-sized, so memory is flat.
  `internal/ingest/existence` is the lookup service behind `/exists`: deduplicate → cache → join
  in-flight lookups → one batched `ExistingInfoHashes` per request (≤ 16 concurrent) → classify into
  exists / missing / **unknown**; an error, a cancelled context or an abandoned join is `unknown`, is
  never cached and never reported as missing; a joined request sees the leader's error as `unknown`.
  `Service.MarkExisting` is the consumer's promotion hook.

## 8. Concurrency and shutdown

- All channels are fixed-capacity; every goroutine has one owner that stops it. No unbounded queues.
- Active leeches are limited by `--leech-max-active` / `MAGNETICO_CRAWL_LEECH_MAX_ACTIVE`
  (default **32**); `LeechMaxN` keeps its per-hash peer meaning. Worst-case in-flight metadata memory
  32 × 10 MiB.
- **`metadata.Sink` rewrite (minimal):** context-aware; `TrySink` with a semaphore; the address policy
  also applies to the first peer; leeches close their connection on cancel; `drain` is closed only after
  all leeches have returned (no send on a closed channel, no panic); no `terminated` data race.
- The DHT event loop hands results to the batcher without blocking; when the batcher is full the result is
  dropped and counted.
- **Crawler SIGTERM:** stop DHT → stop batcher → cancel leeches → writer commits what is left within its
  5 s grace (rest dropped and counted) → sender finishes the current request (30 s grace) → checkpoint and
  close SQLite.
- **Bounded queue close (M6).** `crawlqueue.Close()` waits unconditionally for the writer goroutine, whose
  database operations are not interruptible by SQLite's busy timeout (that timeout bounds lock waits, not file
  I/O or `fsync`). The process therefore uses `CloseWithin(grace)`: (1) stop accepting work and wait `grace`
  (10 s) for the operation in flight; (2) cancel the writer's database context (interrupting running statements;
  an interrupted transaction rolls back, committed rows are untouched) and wait `grace/2`; (3) if the writer
  still has not returned — for example it is blocked in the kernel — return `ErrCloseTimeout`, set
  `Abandoned()`, and **neither close the database handles nor release the directory lock** (an abandoned writer
  could still use them). The crawler then exits with status 3; process exit releases the lock and the next start
  runs SQLite WAL recovery. Wrapping `Close()` in a goroutine is deliberately not used. Tested with a writer held
  in a non-cancellable stall, a cancellable stall, and a full queue (`internal/crawlqueue/closewithin_test.go`,
  `internal/crawler/pipeline_test.go`).
- **SIGKILL/power loss:** committed rows survive; sent-but-unacknowledged rows are resent.
- **Ingest SIGTERM (bounded):** the consumer and the HTTP server stop concurrently. The consumer stops taking
  rows and may finish the record it is on for `ConsumerGrace` (10 s); that record is then interrupted
  (`Consumer.Abort` cancels its context) and given `AbortGrace` (5 s) to return. The HTTP server drains in-flight
  requests for `HTTPGrace` (20 s) and then drops their connections (cancelling the request contexts); handlers
  get `HandlerGrace` (3 s) to return. Guarantee: `Run` returns within
  `max(HTTPGrace, ConsumerGrace+AbortGrace) + HandlerGrace` plus ≤ 5 s for the final summary — **28 s** with
  the defaults — plus ≤ 7 s for the statistics (a final flush bounded by 5 s that drains the sealed batch and the
  carry behind it, then ≤ 2 s to close the three statistics connections; skipped when something was abandoned):
  **35 s** worst case. If the consumer or a handler is still running after its budget (a call that ignores its context),
  it is **abandoned**: the database pools are *not* closed (closing blocks on checked-out connections and would
  pull them from under the stuck goroutine), `Run` returns `ErrShutdownIncomplete` and the process exits with
  status 3. Interrupted work needs no repair: the queue row is deleted only after `AddTorrent` succeeded, so the
  record is reprocessed after the next start and `AddTorrent` is idempotent. The advisory-lock connection is
  released at process exit. Container stop timeout for ingest: **≥ 40 s**. Tested with a stalled interruptible
  and a stalled uninterruptible `AddTorrent` (`internal/ingest/server/service_shutdown_test.go`).
- **Known DHT overflow defects** (confirmed by code review against `dht/managers.go`): `range` over a
  channel that is never closed (producer goroutine blocks forever, result lost); unlocked read of
  `m.output`. M0 ports the minimal upstream fix (`tgragnato/magnetico` 5092b922) with `-race` overflow
  tests. A wider buffering redesign is a separate non-blocking issue.

## 9. Untrusted bencode nesting

The bencode decoder has no depth limit and recursion happens in `parseDict`/`parseList`/`readOneValue`,
`FileTree.UnmarshalBencode` (which re-parses every subtree: O(depth × size)) and `appendTreeFiles`.
A Go stack overflow is fatal. M0 first **proves or refutes** the risk with subprocess hostile-input tests
(deep `l`/`d` nesting, deep file trees, at the 10 MiB limit), recording crash, wall time and peak heap. If
confirmed: an iterative O(n) pre-scan `bencode.CheckDepth(b, 256)` before `unmarshalMetainfo` rejects
over-deep metadata as invalid (counter + throttled warning). The decoder is not rewritten; the upstream
streaming parser is not adopted in 0.8.0.

## 10. Configuration

All operational configuration is available through environment variables (Docker-first); YAML stays
optional; CLI flags exist for non-secret values. Unknown `MAGNETICO_*` variables of the active command are
rejected, as today. Secrets (tokens, DSNs with passwords) are ENV/config-only, never flags.
`crawl`: `MAGNETICO_CRAWL_INGEST_URL`, `_INGEST_TOKEN`, `_QUEUE_DIR`, `_QUEUE_MAX_BYTES`,
`_LEECH_MAX_ACTIVE`. `ingest`: `MAGNETICO_INGEST_LISTEN`, `_DATABASE`, `_QUEUE_DATABASE`,
`_QUEUE_MAX_BYTES`, `_CACHE_MAX_BYTES`, `_CACHE_DISABLED`, `_ALLOW_NON_DURABLE_DB`, `_TRUSTED_PROXIES`
(CIDR list: which peers may name the client address recorded as a crawler's last address) and `_STATS_DISABLED`
(kill switch of the statistics, [`statistics-v1.md`](statistics-v1.md)); `MAGNETICO_INGEST_TOKEN` is removed (§5: credentials
are per crawler, `magnetico ingest crawler …`). The crawl option
`databaseURL` (`--database`, `MAGNETICO_CRAWL_DATABASE`) is **removed**; a stale key fails loudly.

## 11. Resource estimates

**Measured** (read-only pass over the local `magneticod-sample.sql.gz`, 3,457,046 torrents): files per
torrent mean 35.7, p50 3, p90 34, p99 495, max 123,480. **Estimated** record JSON size (formula
110 B + name + Σpath + 26 B/file over measured inputs): mean ≈ 3.1 KB, p50 ≈ 350 B, p90 ≈ 2.5 KB,
p99 ≈ 41 KB, p99.9 ≈ 394 KB, p99.99 ≈ 1.7 MB, max ≈ 9.3 MB. Bencoded metadata is capped at 10 MiB by
the leech; its JSON size is not derivable from that cap and is governed solely by `MaxRecordBytes`.

**Measured (M2, this machine: AMD Ryzen 7 4800HS, btrfs on LUKS/NVMe, SQLite 3.54.1 via
`modernc.org/sqlite` v1.61.0, 4 KiB pages, `synchronous=FULL`).** Database file size relative to payload
bytes after the WAL was reset (`wal_checkpoint(TRUNCATE)`), and the WAL before any checkpoint:

| Payload class | records | db+wal / payload after TRUNCATE | per-row overhead | WAL before checkpoint |
|---|---|---|---|---|
| p50 ≈ 350 B | 4,000 | 1.28 | ~97 B | ~4.1 MB (≈ autocheckpoint) |
| mean ≈ 3.1 KB | 4,000 | 1.34 | ~1,047 B (page-packing waste: one ~3.1 KB row per 4 KiB page) | ~4.3 MB |
| p99 ≈ 41 KB | 800 | 1.014 | ~580 B | ~5.7 MB |
| p99.9 ≈ 394 KB | 120 | 1.003 | ~1,264 B | ~25 MB (one 64-record group of large rows) |

So the practical ratio is **1.0–1.35 × payload** (the earlier 1.25 × estimate holds for small records
and is pessimistic for large ones); the quota counts payload bytes only, so provision ≥ 1.4 × the
quota plus a few tens of MB of WAL headroom. Capacity examples at that ratio:

| Depth | p50 (350 B) | mean (3.1 KB) | p99 (41 KB) |
|---|---|---|---|
| 10,000 | ~4.5 MB | ~42 MB | ~0.4 GB |
| 100,000 | ~45 MB | ~0.42 GB | ~4.2 GB |
| 1,000,000 | ~0.45 GB | ~4.2 GB | ~42 GB |

(The 1 GiB default quota therefore holds ~2.4 M p50 records, ~0.3 M mean-sized records, or ~25 k p99
records.)

**Measured queue throughput (reported, not a gate)**, records/s with 1 / 8 / 64 concurrent producers:
p50 626 / 2,347 / 14,894; mean 569 / 2,242 / 10,447; p99 542 / 1,427 / 3,203; p99.9 231 / 402 / 512.
Average group size 1.0 / 4.0 / ~32 records; commit (BEGIN…COMMIT incl. fsync) 1.6–2.2 ms for small
records, 10 ms at p99 and 58 ms at p99.9 with 64 producers. Peek(256, 8 MiB) + Ack drain of 3.1 KB
records: ~19,000 records/s. The expected production load (10–50 torrents/s) is two to three orders of
magnitude below these figures.

Infohash-only pending rows (not used by this design): ~80–100 B/row → 1 M ≈ 100 MB. PostgreSQL queue:
similar payload plus 23 B tuple header, the unique index, and TOAST (pglz may shrink large rows); its WAL is
managed by the server. In a healthy state (short readers, checkpoints completing) the SQLite WAL stays
near one group-commit batch plus the autocheckpoint size (~4 MiB) and is truncated toward
`journal_size_limit` after checkpoints; it is **not** hard-bounded (see §4) and operators must leave disk
headroom. Deleted rows go to the freelist; size returns to the OS only via
`incremental_vacuum` (SQLite) / `VACUUM FULL` (PostgreSQL). Because record sizes vary by four orders of
magnitude, capacity limits are in **bytes**, not records. Queue throughput (one fsync per group commit)
is measured and reported in M2 but is not a release gate; the gate is that the whole pipeline keeps up
with the expected production load (§12, M7).

## 12. Tests (acceptance for 0.8.0)

Unit, integration and fault-injection cases are listed per milestone in
`docs/wip/06-crawler-ingest-plan.md`. They must cover: several simultaneous crawlers; ingest/PostgreSQL
restart and PostgreSQL outage (503, crawler retains); duplicate infohashes and duplicate delivery; lost
HTTP acknowledgment after durable enqueue; process crashes before/after each acknowledgment; partial and
malformed batches; token authentication and secret leakage; cache staleness; queue limits including the
**quota concurrency regression**; all-or-nothing admission with crawler bisection; `unsupported_version`
and `record_too_large` quarantine (including after recursive bisection), **and no quarantine on
unrecognised rejections** (`invalid_request`, proxy 413/400, malformed bodies: record kept, backoff,
diagnostics);
202 acknowledgment independent of the response body; queue-full shutdown with a full local queue and in-flight leeches;
graceful and ungraceful shutdown; advisory-lock ownership. Disposable PostgreSQL only
(`internal/pgtest`, `MAGNETICO_TEST_POSTGRES_DSN`); no production connections.

## 13. Decision matrices

### 13.1 Crawler queue backend (workload: append, FIFO read, delete after ack, payloads 0.3 KB–10 MB)

| Criterion | **SQLite WAL** | bbolt | Pebble | Badger | Redis Streams |
|---|---|---|---|---|---|
| Extra infrastructure | none | none | none | none | **server required** |
| Durable commit before ack | `synchronous=FULL` | fsync per txn | WAL sync option | `SyncWrites` | AOF `always` / `WAITAOF` |
| Crash recovery | WAL recovery, mature | copy-on-write, good | WAL replay | WAL replay | AOF replay |
| Concurrent reader/writer | 1 writer + readers (WAL) | 1 writer, readers OK | yes | yes | yes |
| Delete-heavy FIFO | freelist reuse, `incremental_vacuum` | file never shrinks without offline compaction | tombstones + compaction | value-log GC | `XDEL`/`XTRIM` |
| Large values (MBs) | overflow pages, fine | poor (page-oriented) | OK | value log | memory-resident |
| Dedup by infohash | `UNIQUE` index | manual | manual | manual | manual |
| Format versioning | `user_version` | manual | manual | manual | manual |
| Inspectability | `sqlite3` CLI | limited | limited | limited | redis-cli |
| Dependency footprint | large (pure-Go driver) | small | large | large | external service |
| Disk-full behaviour | clear `SQLITE_FULL`, committed data safe | ENOSPC mid-commit, reopen safe | compaction can fail | GC can fail | AOF stops / errors |

**Recommendation: SQLite WAL** (owner preference; no material objection found). bbolt is the runner-up.
Pebble/Badger are rejected for a delete-heavy FIFO. Acceptance criteria are in the plan (M2): crash-kill
with no lost committed row, byte quota never exceeded, healthy checkpoint/truncate behaviour (WAL
returns toward `journal_size_limit` once readers close; growth under a long-lived reader is observed and
reclaimed afterwards), file shrinks after `incremental_vacuum`. Queue throughput is measured and reported,
not gated.

### 13.2 Server queue

| Option | Verdict |
|---|---|
| **PostgreSQL tables (chosen)** | Already a hard dependency; one durability domain; transactional quota, quarantine and ack; 503 on outage is accepted. |
| SQLite on the ingest host | Rejected by the owner: a second storage system without sufficient benefit; adds WAL/vacuum maintenance. |
| Redis Streams | Not chosen (extra infrastructure). If ever used: HTTP 202 must follow `XADD` + `WAITAOF 1 0 <timeout>` (or `appendfsync always`); `XADD` success alone is **not** evidence of fsync; a `WAITAOF` timeout is ambiguous (the client resends); consumer groups need `XAUTOCLAIM` recovery of pending entries; `XACK` only after the PostgreSQL commit; `XTRIM MINID` only below the oldest unacked ID. |

### 13.3 Cache

| Option | Verdict |
|---|---|
| **TTL-LRU, split positive/negative expiry (chosen)** | Simple, bounded, predictable memory; fits append-only data. |
| TinyLFU / W-TinyLFU | Possibly better hit rate on skewed traffic; extra complexity and dependency. Post-release comparison using production-observed traffic. |

## 14. Post-release enhancements (not 0.8.0)

Cache-algorithm comparison; pending-hash lookup; adaptive backpressure and queue-filling policies;
queue compression (via record `format`); alternative queue backends; concurrent consumers (`FOR UPDATE
SKIP LOCKED`; separate issue); DHT buffering redesign (separate issue); quarantine CLI; `/metrics`
listener (0.8.0 logs a periodic counter summary every 5 min); token rotation; parser optimisation
(separate issue); benchmark scenarios (queue throughput by payload class, cache hit rate vs budget,
`AddTorrent` throughput vs consumer count).

## 15. Open decisions for owner confirmation (conservative recommendations)

| # | Decision | Recommendation |
|---|---|---|
| O-1 | SQLite driver | `modernc.org/sqlite` (alternatives: `ncruces/go-sqlite3`, CGO `mattn`) |
| O-2 | New command / schema command names | `magnetico ingest`, `magnetico migrate ingest-schema-init`, `magnetico crawl check-queue` |
| O-3 | Additive `Store.ExistingInfoHashes` (persistence contract addition, no existing method changes) | approve |
| O-4 | Defaults: crawler queue 1 GiB, server queue 2 GiB, cache 8 MiB, negative TTL 20 s, leeches 32, submit concurrency 8, `MaxRecordBytes` 16 MiB | approve |
| O-5 | Stale crawl `databaseURL` config | hard error |
| O-6 | New `--leech-max-active` rather than redefining `LeechMaxN` | approve |
| O-7 | Single token, no rotation | approve |
| O-8 | `fsync=off` refused unless explicit dev override | approve |
| O-9 | Legacy unused `persistence.Database` interface | leave untouched |
| O-10 | Ingest refuses to start during an incomplete import | approve |
| O-11 | `/metrics` listener | defer; periodic log summary |
| O-12 | Quarantine retention | operator-managed in 0.8.0, no automatic expiry |

## 16. Dependency and removal map (old direct-PostgreSQL crawler)

| Where | Dependency | Fate |
|---|---|---|
| `main.go` `runCrawl`: `persistence.MakeDatabase` | crawler opens PG, schema verification | removed from `crawl`; used by `ingest` |
| `main.go`: `store.DoesTorrentExist` per DHT result | synchronous PG round trip | replaced by the batched `/exists` call |
| `main.go`: `store.AddTorrent` per drained record | direct write | SQLite enqueue → HTTP → PG queue → consumer → `AddTorrent` |
| `main.go`: `torrentFromMetadata` | size guard | moved to the record package |
| `main.go`: `dropInvalidRecord`, `stats.IncDBError` | metrics | invalid-record path stays; DB-error counter replaced by delivery counters |
| `opflags.CrawlOptions.DatabaseURL` and its env/yaml keys | config | removed; documented in `UPGRADE.md` |
| `metadata/sink.go` → `persistence` | type alias `File` only | kept; no runtime DB access |
| `persistence.ValidateIngest` | validation | reused by crawler and ingest |
| `persistence.Database` (legacy interface) | unused | untouched (O-9) |

## 17. Upstream review (imported `f8066065…` → upstream HEAD `35f22146…`)

| Upstream | Finding | Class |
|---|---|---|
| `5092b922` dht: drain swapped output channels | our copy has the unclosed-range deadlock and unlocked read | **Port in Task 06** (adapt + tests) |
| `1a0ed4b1` metadata: address policy for first peer | first peer bypasses `isAllowed` in our copy | **Port in Task 06** |
| `43e62e3a` dht: sample slicing `[i*20:(i+1)*20]` | still wrong in our copy | **Port in Task 06** |
| `b999eec7` routingTable.dump counter | cap never applies in our copy | **Port in Task 06** |
| `93c5b03a` metadata piece-index bounds | already stricter in our `applyMetadataPiece` | Skip |
| `3b69869a`, `4bc0471c`, `fc91ebed` linear file-tree parsing, bencode perf | optimisation | Follow-up issue |
| `c98cd81a`, `700a6934` piece layers, BuildFromFilePath | not on the crawler path | Skip |

No upstream history is merged and no commit is cherry-picked wholesale.
