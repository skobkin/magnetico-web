# Task 06 — Implementation plan (Task 06A deliverable)

**Status:** M0–M5 implemented on branch `refactor/0.8.0-unify-and-new-storage` (see git log); M6–M7 in
progress (authorized after review of `9f62f51f`).

M5 notes: packages `internal/ingest/server` (handlers, `Run` lifecycle) and `opflags/ingest.go`. The `ingest`
command has CLI flags and `MAGNETICO_INGEST_*` variables but **no config-file layer** (deviation: YAML keys for
ingest are not supported; the token is ENV-only by design). `/exists` answers 503 `storage_unavailable` when
every requested hash is `unknown` (nothing could be answered) and 200 with an `unknown` list otherwise.
`record.CheckVersion` was exported so single-record precedence `unsupported_version > record_too_large >
invalid_record` also holds for oversize records.

M4 notes: package `internal/ingest/pgqueue`. `ingest-schema-init` verifies the 0.8.0 schema through
`persistence.MakeDatabase` before creating `magnetico_ingest`. The usage triggers additionally reject
`UPDATE` of `payload`/`payload_bytes` and `TRUNCATE` of both tables. `Open` also refuses to start while a
`magnetico_migration.progress` row is not `import_complete*`. An admission batch consisting only of
duplicates never fails on quota. A quarantined row records the true attempt count (10 for `max_attempts`).

Design: [`docs/specs/crawler-ingest-delivery-v1.md`](../specs/crawler-ingest-delivery-v1.md) (draft).
**No milestone below may start before the owner approves the specification.** Each milestone is
independently buildable and testable, ends with `go build ./... && go vet ./... && go test -race ./...`,
and stops for review. Disposable PostgreSQL only (`internal/pgtest`); never production. Commits stay local
until the owner pushes.

Release priority: restore production crawling on the new architecture. Correctness, durability and
failure-recovery tests are required; performance experiments are not (see "Post-release").

## M0 — DHT and metadata correctness fixes

- `dht/managers.go`: port the minimal upstream fix (5092b922, adapted): swap under the write lock,
  drain without blocking, close the old channel; read `m.output` under the lock.
  Tests (`-race`): overflow path preserves every result and order, no goroutine leak, concurrent producers
  and a concurrent consumer.
- `dht/mainline/indexingService.go`: sample slicing `[i*20:(i+1)*20]`; test with 3 samples.
- `dht/mainline/routingTable.go`: `dump` increments its counter; test that the 100-node cap holds.
- Hostile bencode nesting (subprocess tests): deep `l` under an unknown info key, deep `d` in `file tree`,
  deep nesting at 10 MiB. Record crash / wall time / peak heap in the report.
  **If confirmed**, add `bencode.CheckDepth(b, 256)` (iterative, O(n)) called before `unmarshalMetainfo`;
  over-limit metadata is invalid (counter + throttled warning). No decoder rewrite.
- Acceptance: new tests fail on the old code and pass on the new; report states which hostile cases crashed.

## M1 — `internal/ingest/record`

Record v1 type, strict encode/decode (unknown fields/`v`/trailing data rejected), `MaxRecordBytes` = 16 MiB,
conversion from `metadata.Metadata` including the uint64→int64 guard moved from `main.go`,
`persistence.ValidateIngest` reuse, `SetEscapeHTML(false)`.
Tests: round trip for ordinary, single-file, duplicate-path, zero-size-member and non-ASCII records; NUL and
invalid UTF-8 rejection (no U+FFFD substitution); escape-expansion record near the limit; oversize detection.

## M2 — `internal/crawlqueue` (SQLite WAL)

Adds `modernc.org/sqlite` (O-1). Queue + quarantine tables, `user_version`, `flock`, group-commit writer,
byte accounting, `Append/Peek/Ack/Quarantine/Stats/Close`, checkpoint/vacuum maintenance,
`crawl check-queue` diagnostic (no startup integrity scan).
Tests: append/peek/ack/quarantine; duplicate hashes; byte accounting across reopen; `SQLITE_FULL`
(tiny `max_page_count`) → `ErrQueueFull`, no corruption; SIGKILL subprocess during writes — every row whose
`Append` returned success is present after reopen; second process on the same directory refused; schema
version mismatch refused; **healthy checkpoint/reclamation**: with only short readers the WAL returns
toward `journal_size_limit` after checkpoints; with an artificially long-lived reader the WAL is observed
to grow past the limit and is reclaimed after the reader closes; a forced checkpoint failure is logged and
counted and the writer keeps working; the file shrinks after `incremental_vacuum`. No test asserts an
unconditional maximum WAL size.
Acceptance: measured disk-per-payload ratio recorded (replaces the spec's estimate); queue throughput
(records/s, group-commit sizes, fsync latency) **measured and reported** for p50/mean/p99-sized payloads —
not a release gate and no fixed records/s threshold on any particular machine.

## M3 — Existence lookup and cache

- `persistence`: additive `ExistingInfoHashes(ctx, hashes) (map or set, error)` (O-3), one
  `= ANY($1)` query; existing methods untouched. PG integration test incl. empty/dup/missing input and DB error.
- `internal/ingest/cache`: LRU behind `ExistenceCache`; positive no-TTL, negative 20 s, injected clock,
  disable switch, promotion, counters.
- `internal/ingest/existence`: the lookup service the `/exists` handler will use (dedupe → cache →
  in-flight join → batched lookup, `unknown` on error, concurrency cap 16). The HTTP handler is M5.
Tests: eviction order, TTL expiry, errors never cached as negative, heap test for the 8 MiB budget.

## M4 — PostgreSQL ingest queue

- `magnetico_ingest` schema, `migrate ingest-schema-init` (advisory-locked, idempotent, non-destructive),
  `ingest_version` check, usage triggers, quarantine.
- Admission (all-or-nothing, sorted insert, quota check, `synchronous_commit=on`), consumer, advisory-lock
  ownership on a dedicated pinned connection, error classification, quarantine transfer.
Tests (disposable PG):
- schema: idempotent init; refused without a verified `magneticod` schema; existing persistence startup
  verification and `migrate` preflight/verify unaffected by `magnetico_ingest` (and vice versa).
- **quota concurrency regression:** many goroutines submit batches totalling just above the limit, mixing
  duplicate and disjoint hashes → `used_bytes ≤ max` at all times, `used_bytes == SUM(queue)+SUM(quarantine)`
  after ack, quarantine transfer and operator delete, no deadlock, surplus requests get `queue_full`.
- all-or-nothing: a batch with one invalid item commits nothing.
- consumer: success deletes; crash between `AddTorrent` and delete → reprocessed, one `torrents` row;
  `ErrInvalidMetadata` → quarantine; transient errors back off; 10 attempts → `max_attempts` quarantine;
  poison record does not block the queue.
- advisory lock: two consumers → exactly one processes; `pg_terminate_backend` on the owner's pinned
  connection → owner stops, standby takes over, nothing lost or duplicated in `torrents`; the lock is a session
  lock on the pinned connection (not a pool query); graceful shutdown releases it; ownership check fails
  closed.
- `fsync=off` refused without the override.

## M5 — `ingest` command

`opflags.IngestOptions` (ENV-first; tokens ENV-only, `yaml:"-"`, no flag), command wiring, HTTP server with
the §5.5 timeouts and limits, bearer authentication, `/exists` (dedupe → cache → in-flight join → batched
lookup, `unknown` on error), `/torrents`, `busy` semaphore, graceful shutdown, periodic counter summary.
Tests: missing/wrong/short token, no token in logs, errors or panics (log capture); 400/405/413/415 paths;
exact rejection code per §5.3 table (1 vs > 1 record, oversize record vs oversize body, precedence), with the
record-specific codes (`invalid_record`, `unsupported_version`, `record_too_large`) returned only for
single-record requests as `application/json` error bodies;
202 response with `{"accepted":n}` body; nothing written on any rejection;
batch and body limits; `unknown` never reported as `missing`; 503 `storage_unavailable` while PG is down and
recovery afterwards; shutdown with in-flight requests; concurrent identical misses collapse to one lookup.

## M6 — Crawler pipeline and removal of the direct-PostgreSQL path

- `metadata.Sink` rewrite (ctx-aware, `TrySink`, active-leech semaphore default 32, first-peer policy,
  conn close on cancel, `drain` closed after all leeches return).
- Precheck batcher + HTTP client (timeouts, backoff with jitter, `unknown` handling, fail-open), enqueue writer,
  sender (§5.4 table incl. bisection and quarantine), admission gate, counters.
- Rewire `runCrawl`; remove `persistence.MakeDatabase`, `DatabaseURL` and its env/yaml keys; add
  `UPGRADE.md`/README notes.
Tests: client classification table (recognised record-specific errors vs everything else, per §5.3/§5.4);
bisection quarantines exactly the bad record while the rest are delivered, at depth 1 and at maximum
depth, with the bad record at head, middle and tail; a crash during bisection leaves quarantined rows
quarantined and restarts the rest from the head; a single-record `invalid_record`, `unsupported_version`
or `record_too_large` response (well-formed ingest error body) quarantines with payload byte-identical,
reason and HTTP status, and the next record is delivered; **unrecognised rejections never quarantine**:
a single-record `invalid_request`, `payload_too_large`, an unknown code, a malformed/empty/non-JSON body,
and a proxy-style 413/400 (HTML, plain text, foreign JSON) keep the record, back off, bump the
`crawl_send_unrecognised_rejections` counter and log the throttled diagnostic (status, content type,
envelope parsed or not, record count, ≤ 200 sanitised body bytes, no token or record contents); a
multi-record 413/400 from a proxy is bisected and then, at one record, kept; a permanently oversized
record is never retried indefinitely (local `record_too_large` → quarantine at enqueue; server
`record_too_large` for one record → quarantine; 413 multi → bisect); the systemic-failure guard stops
quarantining after 8 consecutive recognised record errors and retains records; 202 with empty, garbled or
mismatched body still acknowledges, while 200/204 do not; `record_too_large` quarantined at enqueue,
never sent or truncated; lost 202 → resend → no duplicate; 401 keeps rows; fail-open fills the queue to
the limit then stops admitting; **queue-full shutdown**: full SQLite queue + ≥ 8 fake in-flight leeches →
SIGTERM exits within the grace period, committed rows unchanged, dropped counters match, no panic; Sink
tests under `-race` (no send on closed channel, no leaked goroutines); a stale
`databaseURL`/`MAGNETICO_CRAWL_DATABASE` is rejected.
`crawl check-queue` (the `crawlqueue.Check` diagnostic, delivered as a library in M2) is wired to the CLI
here, together with the queue directory option.

## M7 — End-to-end fault injection and release readiness

- Several simultaneous crawler instances with overlapping hashes against one ingest → every hash exactly once.
- PostgreSQL stop/start while crawling; ingest restart with pending queue rows; lost-ack proxy.
- SIGKILL subprocess crash points (pattern of `main_faults_test.go`): before/after local commit; after server
  commit before response; after `AddTorrent` before queue delete.
- One focused pipeline test at the **expected production load** (10–50 torrents/s, a realistic size mix from
  the measured distribution): the crawler queue depth and the PostgreSQL queue backlog must not grow
  without bound over the run (queue-depth/backlog samples reported), memory and disk stay within the
  configured limits, and records/s through HTTP → queue → consumer is reported. This is an acceptance check
  of acceptable behaviour, not a benchmark and not a fixed records/s gate.
- Loopback smoke: `crawl` + `ingest` binaries with a fake DHT source.
- Release-readiness checklist: defaults documented, operator SQL for quarantine, Traefik/TLS notes,
  PostgreSQL durability settings, backup note, `-race` clean.

## Post-release (not scheduled)

Cache algorithm comparison on production traffic; benchmark scenarios — queue throughput per payload class
(p50/p99/large), group-commit sizing, cache hit rate vs budget and avoided DB queries, `AddTorrent`
throughput vs consumer count; parallel consumers (issue); DHT buffering redesign (issue); parser
optimisation (issue); quarantine CLI; `/metrics`; compression; token rotation.

## Tracker issues (Forgejo `skobkin/magnetico-web`)

1. [#44](https://git.skobk.in/skobkin/magnetico-web/issues/44) `dht: redesign Manager output buffering and backpressure` — non-blocking.
2. [#45](https://git.skobk.in/skobkin/magnetico-web/issues/45) `ingest: support concurrent PostgreSQL queue consumers` — post-0.8.0.
3. [#46](https://git.skobk.in/skobkin/magnetico-web/issues/46) `metadata: adopt upstream linear file-tree parsing and parser optimisations` — non-blocking.
