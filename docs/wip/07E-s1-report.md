# Task 07E / W4 — status report at S1 (steps 1–4)

Plan: [07E-web-search-details-plan.md](07E-web-search-details-plan.md). Raw evidence: [07E-s1-measurements.txt](07E-s1-measurements.txt). Stopped at **S1**; step 5 (index prototype), C1 and later are not started. Nothing was pushed.

## Delivered

| Step | Commit | Content |
|---|---|---|
| 1 | `20b069e3` | W4 plan (with the three outstanding corrections) and the README row |
| 2 | `0637bfe6` | `internal/web/gate`: weighted FIFO gate, bounded waiters, bounded wait; a weight above capacity is refused (`ErrTooHeavy`), never clamped |
| 3 | `196085f4` | `persistence.Reader` (by id, by info hash, keyset `SearchTorrents`, `FileIdentity`/`FileHeader`/`FileBlob`), `DecodeFileList`/`SingleFileList` shared with `GetFiles`, `StatementTimeout` budget, additive `codec.PeekDecodedSize`, the S1 measurements |
| 4 | (this commit) | `internal/web/search`: query parser, params, opaque cursors, service with gate and outcome mapping |

Plan corrections applied in step 1: traversal-dependent Next/Previous (§3.4, with a return-trip test), `kids[]` two-phase packing (§6.2), `pg_statio_user_tables` (§3.1).

## Design as implemented (and deviations from the plan)

- **Statement timeout** (`persistence.StatementTimeout`): `margin = clamp(remaining/10, 50 ms, 500 ms)`; no statement is started when `remaining ≤ margin + 100 ms` (`ErrBudgetExhausted`, which also matches `context.DeadlineExceeded`); otherwise the timeout is strictly below the remaining budget. It is computed **after `BEGIN`**, so the pool wait and round trip are subtracted (a first version computed it before and failed under load).
- **Cursors:** kinds `f`/`b` (value) and `F`/`B` (anchor-only, names > 4 KiB); the direction is part of the checksummed payload. Links follow the direction table of plan §3.4. A stale cursor that yields an empty page restarts from the first page; a deleted anchor is `ErrCursorExpired`, never an empty page.
- **Deviation 1 — `FileIdentity` has no blob length.** The plan's identity was (id, file_count, total_size, blob length, xmin). Implemented: (id, file_count, total_size, xmin); the row version already identifies the stored value, and the stored length is returned by `FileHeader` on the miss path, where it is needed for the weight. Measured cost is the same either way (below), so this is a simplification, not a cost avoidance.
- **Deviation 2 — round trips.** Each Reader call is `BEGIN READ ONLY`, `SET LOCAL statement_timeout`, the query and `COMMIT` (four round trips), not one pipelined batch.
- The info-hash lookup also passes through the search gate.

## Measurements (PostgreSQL 17.12, `en_US.utf8`, default settings: `shared_buffers` 128 MB, `work_mem` 4 MB; 300,000 synthetic rows, seeded Zipf vocabulary; page size 50; warm)

Execution times from `EXPLAIN (ANALYZE, BUFFERS)`. Terms: common matches 41,669 rows, medium 2,244, rare 99.

| Case | Plan | Time |
|---|---|---|
| newest first page | backward index scan on `idx_torrents_discovered_on` + incremental sort | 0.026 ms |
| deep cursor (50 %), **row-comparison** | same | **0.027 ms** |
| deep cursor, OR-expanded form | same plan, but the index scan starts far earlier | **25.8 ms** |
| backward from the same cursor: row-comparison / OR-expanded | | 0.027 ms / 26.7 ms |
| rare term, newest / size desc / name asc | bitmap trigram scan + sort | 1.4 / 1.5 / 1.5 ms |
| medium term, newest | bitmap trigram scan + top-N | 3.9 ms |
| common term, newest | index walk on `discovered_on`, filter | 0.22 ms |
| common + rare | bitmap trigram scan | 3.5 ms |
| three common terms, newest | index walk, filter (scans until 51 matches) | 31.5 ms |
| common term, size desc | index walk on `total_size` | 0.18 ms |
| **common term, name asc** | bitmap trigram scan of 41,669 rows + top-N heapsort (no index can order by name) | **52.3 ms** |
| info hash | `torrents_info_hash_key` | 0.009 ms |

- **Keyset form:** the row-value comparison is the implemented form (the plan said to keep the better measured one): the OR-expanded form is ~1000× slower on a deep page.
- **Generic-plan risk:** pgx caches prepared statements and PostgreSQL may switch a prepared statement to a generic plan after five executions. For the deep-cursor and common-term cases, nine executions on one prepared statement stayed at 0.1–0.9 ms and the plan was unchanged on the following `EXPLAIN EXECUTE`. Only these two cases were checked.
- **No index is needed or proposed at this scale.** The name sort over a broad match is the slowest measured path (52 ms at 300k rows); it grows with the match count and has no supporting index. Whether this is acceptable at 1M/5M rows is left to the step-8 measurements; nothing is proposed now.

### TOAST (24 MiB blobs, warm; one dedicated connection, statistics flushed explicitly)

| Query | Latency p50 / p95 | `toast_blks_hit` per call |
|---|---|---|
| `FileIdentity` (as implemented) | 0.27 / 0.38 ms | 0 |
| identity **with** `octet_length(data)` | 0.24 / 0.40 ms | 0 |
| `FileHeader` (`substring` 12 bytes + `octet_length`) | 0.25 / 0.33 ms | 1 |
| `FileBlob` (full transfer) | 28 / 210 ms (incompressible), 17 / 30 ms (pglz) | 3,153 (incompressible), 38 (pglz) |

`EXPLAIN (BUFFERS)` for the identity statements: `shared hit=2` with or without `octet_length`; the header statement `shared hit=5`. So the per-request identity does **not** touch the TOAST data, and the header slice touches one page.

- **A measurement error was found and corrected:** a first version read the statistics view after a fixed sleep and attributed ~95 TOAST hits per call to the first measured query (those were the pending statistics of the 24 MiB INSERT). I briefly concluded `octet_length` was expensive and dropped it from the identity; the per-statement `EXPLAIN` figures contradicted that, the flush was made explicit (`pg_stat_force_next_flush` on a dedicated connection) and the numbers above are from the corrected run. The earlier conclusion was wrong; the decision to leave `BlobLen` out stands for the simplicity reason above.
- Limits: warm cache only (`toast_blks_read` stayed 0; a cold read was not measured, because the harness cannot evict `shared_buffers`/the OS cache); one PostgreSQL version and default settings.

## Tests actually run (all passed)

```
go build ./... && go vet ./...
go test -count=1 -race ./internal/web/... ./persistence          # incl. the import-boundary test
go test -count=1 -race -run 'Peek|Decode' ./codec
go test -count=10 -race ./internal/web/gate
go test -count=5 -race ./internal/web/search                      # unit + fakes
go test -count=3 -race -run 'Service|Slow|Walks' ./internal/web/search
go test -count=3 -race -run 'Reader|Search|StatementTimeout|DoesNotStart' ./persistence
MAGNETICO_MEASURE=1 go test -run TestMeasure ./persistence        # explicitly invoked, writes the evidence file
```

All use disposable PostgreSQL (`internal/pgtest`) and synthetic data. Behaviour proven by tests:

- **Timeouts:** a real search blocked by an `ACCESS EXCLUSIVE` lock fails with the server-side timeout **before** the context deadline (`ErrStatementTimeout` at the persistence layer, `ErrTimeout` in the service), and the pool recovers; near-exhausted deadlines (150 ms, 60 ms, 5 ms left after waiting for a slot, and a deadline consumed while queued) start **no** statement (a nil-pool Reader proves it) and report a timeout.
- **Cancellation:** a client cancel returns `context.Canceled` (not a timeout), the permit is released, pgx's cancel request ends the blocked backend (observed through `pg_stat_activity`, within ~0.1 s) and the pool recovers.
- **Saturation and starvation:** with a 4-connection pool and 2 search slots, two blocked searches and a refused third (`ErrBusy`, distinct from timeout) leave other pool users unaffected.
- **Cursors:** every sort × direction walks forward, backward and the return trip (page 1 → 2 → Previous = page 1 → Next = page 2) equal an unbounded reference query, including ties and oversized (> 4 KiB) names at page boundaries in both directions; a deleted anchor is `ErrCursorExpired` in both directions; flipped direction bytes, bad checksum, wrong version/kind, other search, oversized or truncated cursors are rejected. A mutation of the backward-link rule makes the link and walk tests fail.
- **Search semantics:** AND, case-insensitive substring, literal `%`/`_`/`\`, three-code-point minimum with multi-byte text, 40-hex lookup, empty/all-ignored query lists the newest torrents, caps (512 bytes, 8 terms).
- **Persistence:** reads by id/hash, 404-class errors, no-blob and missing-blob shapes, invalid-UTF-8/duplicate/empty-component paths round-trip through `DecodeFileList`, count/sum mismatch and truncation are `ErrCorruptBlob` with no partial list, stale blob version is `ErrBlobChanged`; the existing `GetFiles` suites pass unchanged after the refactor.

## Not run / limitations

- The full `go test ./...` was not run; `dht`, `crawler` and `e2e` are untouched. No HTTP handler, template, cache or file index exists yet (steps 5–8).
- Measurements are at 300k rows only (1M/5M are step 8), warm only, one PostgreSQL configuration.
- The starvation test uses a plain query as a stand-in for a session lookup; the real login path is exercised in step 7.

## Items for the owner at S1

1. **Accept or reject Deviation 1** (identity without blob length) and Deviation 2 (four round trips per read). Neither changes behaviour.
2. **Name sort on broad terms** is the one path that scales with the match count (52 ms at 300k rows). No action proposed now; it will be measured at 1M/5M in step 8, and any index would need your separate approval.
3. No index, schema or codec-behaviour change was needed or made. The only codec change is the approved additive `PeekDecodedSize`.

## Owner policy update (C1 batch)

The owner prefers to revisit search-index optimisation **after the 0.8.0 release and migration, using real data**. The search implementation and the evidence above are unchanged; no index is added or proposed, and the 52 ms synthetic `name`-sort case is not being optimised. Item 2 above is therefore deferred, not open. Timeout, cancellation and correctness requirements are unaffected.
