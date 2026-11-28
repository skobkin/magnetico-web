# Verification page query performance — fix for `0.8.0-rc2`

**Found in:** the production `0.8.0-rc1` migration, during `migrate verify-content`.
**Fixed in:** the page query shared by `verify-content` and `verify-decode`
(`decodePageQuery` in `internal/migration/verify_decode.go`), released with `0.8.0-rc2`.
**Measured by:** the owner, with `EXPLAIN (ANALYZE, BUFFERS)` on the production target database. This document records
those measurements as reported. The agent that wrote the fix did not connect to production and did not repeat them.

## 1. Incident

| | |
|---|---|
| Dataset | 34,570,460 torrents and 1,222,033,996 file rows (production, full) |
| Command | `magnetico migrate verify-content`, `0.8.0-rc1` |
| Symptom | Throughput fell steadily, from about **10,000 torrents/s** at the start to a cumulative average of **315.4 torrents/s** at 53 % completion |

The scan did not stall and no finding was wrong. Each page took longer than the one before it, and the time per page
grew with the position of the cursor.

## 2. Root cause

Both verification passes read the target in keyset pages. The page query in `0.8.0-rc1`:

```sql
SELECT t.id, t.info_hash, t.name, t.total_size, t.discovered_on,
       t.file_count, b.torrent_id, octet_length(b.data)
FROM magneticod.torrents t
LEFT JOIN magneticod.torrent_file_blobs b ON b.torrent_id = t.id
WHERE t.id > $1
ORDER BY t.id
LIMIT $2
```

PostgreSQL picked a **Merge Left Join** over the two primary-key indexes. The `torrents` side starts at the cursor
(`Index Cond: id > $1`). The `torrent_file_blobs` side has no condition. PostgreSQL does not infer
`b.torrent_id > $1` from `b.torrent_id = t.id AND t.id > $1`: it propagates equalities, not inequalities, and a
`WHERE` condition on the preserved side of a LEFT JOIN is not pushed into the nullable side anyway. So every page's
blob-index scan started at the first blob and read the **whole already processed prefix** of `torrent_file_blobs`
before it reached the cursor. Each page costs O(cursor), and the full scan costs O(n²).

## 3. Fix

The cursor restriction is repeated on the blob side of the join:

```sql
SELECT t.id, t.info_hash, t.name, t.total_size, t.discovered_on,
       t.file_count, b.torrent_id, octet_length(b.data)
FROM magneticod.torrents t
LEFT JOIN magneticod.torrent_file_blobs b
    ON b.torrent_id = t.id
    AND b.torrent_id > $1
WHERE t.id > $1
ORDER BY t.id
LIMIT $2
```

**The result is the same.** Every selected `t.id` is greater than `$1`, so a blob row that matches it
(`b.torrent_id = t.id`) also satisfies `b.torrent_id > $1`. The extra condition cannot drop a match. Torrents without
a blob still come back with NULL blob columns, because the condition is in the `ON` clause, not in `WHERE`. The selected
columns, the order, the limit and the parameters are unchanged.

## 4. Production measurements (owner, `EXPLAIN (ANALYZE, BUFFERS)`)

Batch size 256 (`LIMIT 256`) at every cursor.

| Query | Cursor ID | Execution time |
|---|---:|---:|
| Original | 1,000 | 1.040 ms |
| Original | 1,000,000 | 102.491 ms |
| Original | 20,000,000 | 2,475.344 ms |
| Original | 30,000,000 | 3,731.701 ms |
| **Corrected** | **30,000,000** | **0.205 ms** |

At cursor ID 30,000,000:

| Metric | Original | Corrected |
|---|---:|---:|
| Execution time | 3,731.701 ms | 0.205 ms |
| Blob rows scanned | 18,229,798 | 177 |
| Shared buffer accesses | 1,209,481 | 22 |

The original query's time grows roughly in proportion to the cursor, as the root cause predicts.

### What these numbers do and do not show

- **Measured:** one execution of the page query at cursor 30,000,000 is about **18,203× faster**
  (3,731.701 ms → 0.205 ms).
- **Not measured:** the end-to-end throughput of `verify-content` or `verify-decode` with the fix. A page also
  fetches and decodes blobs, reads the source (`verify-content`) and commits findings and checkpoints. These costs do not
  change, so the whole pass speeds up by much less than 18,203×. We expect the per-page cost to stop growing with the
  cursor, so throughput should stay close to its starting level instead of degrading. That is a prediction until a
  production or rehearsal run measures it.

## 5. Plans: custom and generic

pgx prepares statements and caches them, so after a few executions PostgreSQL may switch to a **generic plan**, which is
built without the parameter values. The restriction is useful in both cases: an index condition with a parameter
(`torrent_id > $1`) is evaluated when the executor starts, so the blob-side index scan starts at the cursor in a
generic plan too.

`TestDecodePageQueryBlobScanStartsAtCursor` (`internal/migration/verify_page_query_integration_test.go`) pins this on
a small fixture (4,000 torrents, a blob on every even ID, cursor 3,000, limit 64). It forces the production plan shape
(a Merge Left Join over the primary-key index scans) and runs `EXPLAIN (ANALYZE, FORMAT JSON)` under
`plan_cache_mode = force_custom_plan` and `force_generic_plan`. For each mode it checks that:

- the original query, as a control, reads at least half the processed prefix on the blob side (1,532 rows in the local
  run). This shows the forced shape reproduces the incident;
- the corrected query reads at most one page of blob rows, and its blob-side `Index Cond` holds the cursor bound (the
  literal in the custom plan, `$1` in the generic plan);
- the page has the right rows: the next 64 IDs in order, with blob rows and blob-less rows as seeded.

The test counts rows and never measures time. With the fix reverted, it fails: 1,532 blob rows for a 64-row page.

## 6. Resume compatibility

An interrupted `0.8.0-rc1` verification resumes with `0.8.0-rc2`:

- No persisted state changes. The `verification_runs` and `verification_issues` rows, checkpoints, fingerprints, data
  revisions, settings JSON and the adopt-or-new compatibility predicates are untouched. No schema changes.
- The query returns the same rows for the same cursor (§3), so a pass resumed from an RC1 checkpoint visits exactly the
  rows RC1 would have visited, and the page boundaries and checkpoint semantics do not change.
- The CLI flags and exit codes do not change.

The same change adds a one-time log line when `verify-content` or `verify-decode` adopts an interrupted run. The line
names the verification run ID, the last committed target checkpoint, the source checkpoint (`verify-content`; `none`
before the first source commit) and the cumulative counters. It is printed before the first progress line and never
again. It only reads values the runner already had. Resume logic and persisted state are unchanged.
