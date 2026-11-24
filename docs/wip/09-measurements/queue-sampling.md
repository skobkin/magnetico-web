# Queue-state sampling cost (Task 6 measurement step)

Decides the exact-versus-estimate rule of plan decision D9: is the exact queue count affordable at 10 k, 100 k and 1 M queued rows?

- **Command:** `go test -tags loadtest -run TestMeasureQueueSamplingCost -v ./internal/ingest/stats` (`internal/ingest/stats/sampler_measure_loadtest_test.go`).
- **What is timed:** one complete sampling cycle on the sampler connection: acquire, `BEGIN … REPEATABLE READ READ ONLY`, the `usage` read, the aggregate over `queue` (`count`, `sum(payload_bytes)`, `min(received_at)`), the aggregate over `quarantine`, rollback. Four savepoints, each with its own `SET LOCAL` timeouts. 40 cycles per size after `VACUUM ANALYZE`.
- **Machine:** AMD Ryzen 9 7950X3D, 8 vCPUs; PostgreSQL 17 in a local Docker container (`shared_buffers` 128 MB, default settings), synthetic rows, loopback only. The 1,500-byte payload is the widest row that stays inline (no TOAST), i.e. the worst case for a heap scan: 1 M rows ≈ 1.5 GB. The data is warm in the page cache; a cold cache would be slower, which is why the statement timeout and the flagged fallbacks exist.

| queued rows | payload 1 B: p50 / p95 | payload 1,500 B: p50 / p95 | criterion (p95) |
|---:|---|---|---|
| 10,000 | 1.2 ms / 1.6 ms | 1.6 ms / 2.5 ms | ≤ 100 ms |
| 100,000 | 5.6 ms / 5.8 ms | 19.5 ms / 23.5 ms | ≤ 800 ms |
| 1,000,000 | 25.3 ms / 27.2 ms | 109.8 ms / 123.4 ms | ≤ 2 s |

**Result: all three criteria pass with a wide margin.** The exact path is used; the planner-estimate fallback (counts only, flagged `approximate`), the "bytes are never estimated" rule and the lowest-id age fallback remain as the documented behaviour when a statement exceeds its 2 s budget (a cold cache, an overloaded server). No index and no trigger is added to `magnetico_ingest`.

**Not measured here:** the admission p95 inflation while sampling at 1 M rows (criterion ≤ 10 %) and the cold-cache cost; both belong to the qualification run (Task 11).
