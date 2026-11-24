# EXPLAIN (ANALYZE, BUFFERS, SETTINGS) summary

- container: w9-bench-pg, database: bench, taken: 2026-11-24T04:10:11Z
server: PostgreSQL 17.12 (Debian 17.12-1.pgdg13+1) on x86_64-pc-linux-gnu, compiled by gcc (Debian 14.2.0-19) 14.2.0, 64-bit
effective_cache_size = 5242888kB
fsync = on
jit = on
max_parallel_workers_per_gather = 2
random_page_cost = 4
shared_buffers = 655368kB
synchronous_commit = on
work_mem = 4096kB
torrents rows (estimate) 3499803, table 338 MB, indexes 537 MB
magneticod.torrents_pkey 75 MB
magneticod.torrents_info_hash_key 175 MB
magneticod.idx_torrents_total_size 95 MB
magneticod.idx_torrents_discovered_on 54 MB
magneticod.idx_torrents_name_gin_trgm 137 MB

| shape | cold (ms) | warm 1 | warm 2 | warm 3 | rows | buffers hit / read (last warm) |
|---|---|---|---|---|---|---|
| 01-empty-newest | 4.453 | 3.767 | 2.703 | 3.083 | 51 | Buffers: shared hit=413 |
| 02-empty-oldest | 0.482 | 0.135 | 0.137 | 0.129 | 51 | Buffers: shared hit=70 |
| 03-empty-size-desc | 0.183 | 0.082 | 0.113 | 0.078 | 51 | Buffers: shared hit=41 |
| 04-empty-size-asc | 18.522 | 2.697 | 2.869 | 3.537 | 51 | Buffers: shared hit=2893 |
| 05-deep-keyset-newest | 0.494 | 0.119 | 0.109 | 0.142 | 51 | Buffers: shared hit=75 |
| 06-deep-keyset-backward | 0.431 | 0.118 | 0.171 | 0.179 | 51 | Buffers: shared hit=65 |
| 07-deep-keyset-size | 0.431 | 0.168 | 0.133 | 0.094 | 51 | Buffers: shared hit=65 |
| 08-common-term-newest | 9.207 | 7.174 | 7.537 | 7.538 | 51 | Buffers: shared hit=414 |
| 09-common-two-terms | 1952.220 | 1809.894 | 1773.547 | 1772.251 | 51 | Buffers: shared hit=3494180 |
| 10-common-term-size | 0.412 | 0.257 | 0.266 | 0.285 | 51 | Buffers: shared hit=259 |
| 11-common-term-name | 366.114 | 319.949 | 317.997 | 316.439 | 51 | Buffers: shared hit=44096 |
| 12-mid-term | 12.327 | 8.249 | 8.358 | 8.266 | 51 | Buffers: shared hit=2579 |
| 13-selective-term | 3.153 | 2.316 | 2.548 | 2.282 | 14 | Buffers: shared hit=159 |
| 14-selective-plus-common | 3.696 | 2.839 | 2.964 | 2.819 | 5 | Buffers: shared hit=249 |
| 15-no-hit | 0.111 | 0.082 | 0.077 | 0.081 | 0 | Buffers: shared hit=16 |
| 16-unicode-term | 12.398 | 8.790 | 8.788 | 9.185 | 51 | Buffers: shared hit=3694 |
| 17-common-term-deep-keyset | 1.228 | 0.311 | 0.411 | 0.285 | 51 | Buffers: shared hit=177 |
| 18-infohash-lookup | 0.046 | 0.026 | 0.027 | 0.027 |  | Buffers: shared hit=4 |
| 19-anchor-only-lookup | 0.053 | 0.053 | 0.030 | 0.044 |  | Buffers: shared hit=4 |
| 20-rss-max-offset | 2.472 | 1.099 | 1.198 | 1.084 | 1000 | Buffers: shared hit=197 |
