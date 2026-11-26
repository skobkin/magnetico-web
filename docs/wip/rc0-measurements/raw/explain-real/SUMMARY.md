# EXPLAIN (ANALYZE, BUFFERS, SETTINGS) summary (real-data sample)

- container: rc0-pg, database: rc0_magnetico, taken: 2026-11-26T01:09:57Z, cold restarts: 0
- server: PostgreSQL 17.12 (Debian 17.12-1.pgdg13+1) on x86_64-pc-linux-gnu, compiled by gcc (Debian 14.2.0-19) 14.2.0, 64-bit
- effective_cache_size = 5242888kB
- jit = on
- max_parallel_workers_per_gather = 2
- plan_cache_mode = auto
- random_page_cost = 4
- shared_buffers = 655368kB
- work_mem = 4096kB
- torrents rows (estimate) 3457331, table 444 MB, indexes 793 MB

| shape | first (ms) | warm 1 | warm 2 | warm 3 | rows | top plan node (last warm) | buffers (last warm) |
|---|---|---|---|---|---|---|---|
| 01-empty-newest | 0.251 | 0.259 | 0.254 | 0.252 | 51 | Incremental Sort | Buffers: shared hit=65 |
| 02-empty-oldest | 0.347 | 0.098 | 0.132 | 0.098 | 51 | Incremental Sort | Buffers: shared hit=14 |
| 03-empty-size-desc | 0.358 | 0.357 | 0.328 | 0.340 | 51 | Incremental Sort | Buffers: shared hit=65 |
| 05-deep-keyset-newest | 0.320 | 0.081 | 0.084 | 0.120 | 51 | Incremental Sort | Buffers: shared hit=14 |
| 06-deep-keyset-backward | 0.130 | 0.121 | 0.095 | 0.085 | 51 | Incremental Sort | Buffers: shared hit=14 |
| 07-deep-keyset-size | 0.511 | 0.370 | 0.359 | 0.514 | 51 | Incremental Sort | Buffers: shared hit=65 |
| 08-common-term-newest | 5.662 | 5.652 | 5.812 | 5.788 | 51 | Incremental Sort | Buffers: shared hit=2235 |
| 09-common-two-terms-together | 8.033 | 8.119 | 7.912 | 10.773 | 51 | Incremental Sort | Buffers: shared hit=2334 |
| 10-common-term-size | 3.271 | 3.119 | 3.085 | 3.482 | 51 | Incremental Sort | Buffers: shared hit=842 |
| 11-common-term-name | 235.055 | 227.775 | 240.181 | 227.905 | 51 | Sort | Buffers: shared hit=57225 |
| 12-mid-term | 10.472 | 7.772 | 8.311 | 7.340 | 51 | Incremental Sort | Buffers: shared hit=2325 |
| 13-selective-term | 8.002 | 8.256 | 8.060 | 8.106 | 51 | Incremental Sort | Buffers: shared hit=2319 |
| 15-no-hit | 1.915 | 0.861 | 1.301 | 1.124 | 0 | Sort | Buffers: shared hit=128 |
| 16-cyrillic-term | 19.017 | 16.026 | 15.946 | 16.178 | 51 | Sort | Buffers: shared hit=3681 |
| 17-common-term-deep-keyset | 0.859 | 0.324 | 0.494 | 0.329 | 51 | Incremental Sort | Buffers: shared hit=23 |
| 18-infohash-lookup | 0.635 | 0.037 | 0.055 | 0.040 |  | Index Scan using torrents_info_hash_key on torrents | Buffers: shared hit=4 |
| 19-anchor-only-lookup | 0.392 | 0.053 | 0.070 | 0.087 |  | Index Scan using torrents_pkey on torrents | Buffers: shared hit=4 |
| 20-rss-max-offset | 8.867 | 5.836 | 5.827 | 8.336 | 1000 | Index Scan Backward using torrents_pkey on torrents | Buffers: shared hit=2449 |
| 21-rare-pair-mp4-flac | 2497.754 | 2071.287 | 1972.868 | 2033.203 | 20 | Incremental Sort | Buffers: shared hit=101783 |
| 22-rare-pair-mp3-x265 | 10.381 | 6.290 | 5.458 | 7.076 | 4 | Sort | Buffers: shared hit=307 |
| 23-rare-pair-pdf-1080p | 1748.306 | 1748.244 | 1789.981 | 1797.876 | 31 | Incremental Sort | Buffers: shared hit=101820 |
| 24-rare-pair-mp4-flac-size | 5581.564 | 4609.773 | 4359.055 | 4253.928 | 20 | Incremental Sort | Buffers: shared hit=3467060 read=9257 written=2 |
| 25-rare-pair-epub-720p | 1860.723 | 1954.374 | 1896.151 | 1920.424 | 31 | Incremental Sort | Buffers: shared hit=101959 |


## Generic-plan check (prepared two-term search)

Six executions with common patterns, then the rare pair; then the same with plan_cache_mode = force_generic_plan.
```
Time: 13.667 ms
Time: 5.247 ms
Time: 2.058 ms
Time: 2.330 ms
Time: 2.495 ms
Time: 10.150 ms
Limit  (cost=12.55..583.35 rows=51 width=97) (actual time=1968.807..1968.812 rows=20 loops=1)
  Buffers: shared hit=95464 read=6741
  I/O Timings: shared read=18.069
  ->  Incremental Sort  (cost=12.55..153870.81 rows=13747 width=97) (actual time=1968.806..1968.809 rows=20 loops=1)
        Sort Key: discovered_on DESC, id DESC
        Presorted Key: discovered_on
        Full-sort Groups: 1  Sort Method: quicksort  Average Memory: 29kB  Peak Memory: 29kB
        Buffers: shared hit=95464 read=6741
        I/O Timings: shared read=18.069
        ->  Index Scan Backward using idx_torrents_discovered_on on torrents  (cost=0.43..153290.21 rows=13747 width=97) (actual time=38.880..1968.779 rows=20 loops=1)
              Filter: ((name ~~* '%mp4%'::text) AND (name ~~* '%flac%'::text))
              Rows Removed by Filter: 3459709
              Buffers: shared hit=95464 read=6741
              I/O Timings: shared read=18.069
Planning:
  Buffers: shared hit=1
Planning Time: 0.205 ms
Execution Time: 1968.825 ms
Limit  (cost=959.32..959.34 rows=9 width=97) (actual time=7.567..7.570 rows=20 loops=1)
  Buffers: shared hit=151 read=247
  I/O Timings: shared read=0.636
  ->  Sort  (cost=959.32..959.53 rows=86 width=97) (actual time=7.566..7.567 rows=20 loops=1)
        Sort Key: discovered_on DESC, id DESC
        Sort Method: quicksort  Memory: 29kB
        Buffers: shared hit=151 read=247
        I/O Timings: shared read=0.636
        ->  Bitmap Heap Scan on torrents  (cost=621.30..956.56 rows=86 width=97) (actual time=7.446..7.555 rows=20 loops=1)
              Recheck Cond: ((name ~~* $1) AND (name ~~* $2))
              Rows Removed by Index Recheck: 30
              Heap Blocks: exact=50
              Buffers: shared hit=151 read=247
              I/O Timings: shared read=0.636
              ->  Bitmap Index Scan on idx_torrents_name_gin_trgm  (cost=0.00..621.28 rows=86 width=0) (actual time=7.434..7.434 rows=50 loops=1)
                    Index Cond: ((name ~~* $1) AND (name ~~* $2))
                    Buffers: shared hit=101 read=247
                    I/O Timings: shared read=0.636
Planning:
  Buffers: shared hit=1
Planning Time: 0.085 ms
Execution Time: 7.712 ms
Limit  (cost=959.32..959.34 rows=9 width=97) (actual time=4.704..4.705 rows=4 loops=1)
  Buffers: shared hit=230 read=93
  I/O Timings: shared read=0.224
  ->  Sort  (cost=959.32..959.53 rows=86 width=97) (actual time=4.703..4.704 rows=4 loops=1)
        Sort Key: discovered_on DESC, id DESC
        Sort Method: quicksort  Memory: 25kB
        Buffers: shared hit=230 read=93
        I/O Timings: shared read=0.224
        ->  Bitmap Heap Scan on torrents  (cost=621.30..956.56 rows=86 width=97) (actual time=4.692..4.700 rows=4 loops=1)
              Recheck Cond: ((name ~~* $1) AND (name ~~* $2))
              Heap Blocks: exact=4
              Buffers: shared hit=230 read=93
              I/O Timings: shared read=0.224
              ->  Bitmap Index Scan on idx_torrents_name_gin_trgm  (cost=0.00..621.28 rows=86 width=0) (actual time=4.685..4.685 rows=4 loops=1)
                    Index Cond: ((name ~~* $1) AND (name ~~* $2))
                    Buffers: shared hit=226 read=93
                    I/O Timings: shared read=0.224
Planning Time: 0.009 ms
Execution Time: 4.714 ms
```
