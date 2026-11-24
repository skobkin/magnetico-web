# Run `diag`

- git: `f96be7fce3da`; machine: go1.27.0 linux/amd64, cpu="AMD Ryzen 9 7950X3D 16-Core Processor" cores=8 GOMAXPROCS=8 GOGC=default(100) GOMEMLIMIT=unset kernel=6.12.117+deb13-amd64 mem=32102 MiB cgroup(mem="" cpu="")
- web cgroup memory.max 1G, GOMEMLIMIT 768MiB; pools {'huge': 24, 'large': 1900, 'medium': 6000, 'small': 12000}

## Segments

| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| warmup | 16 | 0 ms | 902 | 206 | 185835 | 0 | 0 | 0 | 0 | 0 | 0 | 108/267 | 3 | 7 | 7 | 240 | 4 |
| steady | 16 | 0 ms | 2702 | 223 | 602376 | 0 | 0 | 0 | 0 | 0 | 0 | 105/239 | 3 | 6 | 7 | 220 | 3 |
| idle | 0 | 0 ms | 600 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | -/- | - | - | - | - | - |

Ingest during `steady`: sent 54005, acknowledged (202) 54005, refused {}.

Drain: {'acked': 54005, 'drain_seconds': 0.004147875, 'missing': 0, 'quarantined': 0, 'queue_left': 0}

## Processes

| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |
|---|---|---|---|---|---|---|---|
| web | 0 | 586 | 712 | 0/0 (high 0, max 0) | 4565 | 635 | 261 |

  `web` shutdown stats: time=2026-11-24T12:36:59.667+04:00 level=INFO msg="file index cache stopped" hits=5703 misses=142206 coalesced=34 builds=142172 build_errors=0 abandoned=0 evictions=141677 expired=0 replaced=0 oversize=0 retries=0 entries=495 bytes=133441821

  `web` WARN/ERROR log lines: {'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 1}
| ingest | 0 | 42 | 25 | 0/0 (high 0, max 0) | 0 | 0 | - |

  `ingest` WARN/ERROR log lines: none

## Memory windows (web), steady window 15–60 min

- RSS MiB: block medians [420, 445, 436, 422]; least-squares slope -1.6 MiB/h = -0.4 % of the plateau per hour; Theil-Sen -23.3 MiB/h = -5.4 %
- cgroup MiB: block medians [504, 528, 552, 543]; least-squares slope +84.6 MiB/h = +15.8 % of the plateau per hour; Theil-Sen +110.8 MiB/h = +20.7 %
- heap-after-GC MiB: block medians [177, 181, 174, 179]; least-squares slope -0.6 MiB/h = -0.3 % of the plateau per hour; Theil-Sen -1.0 MiB/h = -0.6 %
- file descriptors 15–57, threads 8–9 (whole run)
- idle phase: RSS 303 → 228 MiB, cgroup 459 → 414 MiB, fds 32 → 15, threads 9 → 9
- ingest queue depth in the steady window: max 0, median 0, slope +0.0 rows/h

## Diagnostic readings (one process)

| label | t (min) | goroutines | heap in use MiB | heap alloc MiB | file cache bytes MiB / entries | pool acquired/idle/total |
|---|---|---|---|---|---|---|
| baseline-after-start | 0.0 | 12 | 71 | 70 | 0 / 0 | 0/1/1 |
| during-load | 2.0 | 43 | 236 | 230 | 122 / 340 | 5/11/16 |
| during-load | 4.0 | 43 | 235 | 227 | 124 / 447 | 4/12/16 |
| during-load | 6.0 | 43 | 373 | 365 | 128 / 462 | 3/13/16 |
| during-load | 8.0 | 43 | 163 | 147 | 122 / 298 | 5/11/16 |
| during-load | 10.0 | 44 | 237 | 225 | 126 / 546 | 4/12/16 |
| during-load | 12.0 | 43 | 328 | 318 | 126 / 285 | 3/13/16 |
| during-load | 14.0 | 43 | 341 | 333 | 119 / 253 | 4/12/16 |
| during-load | 16.0 | 59 | 256 | 245 | 122 / 517 | 4/12/16 |
| during-load | 18.0 | 43 | 222 | 211 | 125 / 438 | 4/12/16 |
| during-load | 20.0 | 43 | 216 | 200 | 128 / 495 | 3/13/16 |
| during-load | 22.0 | 43 | 248 | 237 | 127 / 398 | 4/12/16 |
| during-load | 24.0 | 43 | 344 | 335 | 126 / 515 | 4/12/16 |
| during-load | 26.0 | 43 | 264 | 253 | 126 / 345 | 3/13/16 |
| during-load | 28.0 | 43 | 394 | 387 | 121 / 305 | 4/12/16 |
| during-load | 30.0 | 43 | 188 | 177 | 127 / 372 | 4/12/16 |
| during-load | 32.0 | 43 | 347 | 341 | 116 / 356 | 5/11/16 |
| during-load | 34.0 | 43 | 315 | 303 | 122 / 257 | 4/12/16 |
| during-load | 36.0 | 44 | 193 | 184 | 126 / 400 | 4/12/16 |
| during-load | 38.0 | 43 | 252 | 246 | 121 / 374 | 4/12/16 |
| during-load | 40.0 | 43 | 315 | 310 | 128 / 467 | 5/11/16 |
| during-load | 42.0 | 43 | 414 | 401 | 114 / 386 | 3/13/16 |
| during-load | 44.0 | 43 | 283 | 274 | 121 / 246 | 3/13/16 |
| during-load | 46.0 | 43 | 265 | 260 | 119 / 286 | 4/12/16 |
| during-load | 48.0 | 43 | 253 | 245 | 128 / 390 | 4/12/16 |
| during-load | 50.0 | 41 | 268 | 247 | 128 / 332 | 6/10/16 |
| during-load | 52.0 | 43 | 316 | 308 | 119 / 517 | 5/11/16 |
| during-load | 54.0 | 43 | 280 | 266 | 128 / 377 | 4/12/16 |
| during-load | 56.0 | 44 | 331 | 326 | 125 / 303 | 4/12/16 |
| during-load | 58.0 | 43 | 181 | 168 | 128 / 352 | 4/12/16 |
| during-load | 60.0 | 43 | 280 | 275 | 126 / 472 | 4/2/6 |
| after-load | 60.1 | 27 | 222 | 214 | 127 / 495 | 0/7/7 |
| idle-start | 60.1 | 27 | 228 | 215 | 127 / 495 | 0/7/7 |
| idle-end | 70.1 | 11 | 171 | 158 | 127 / 495 | 0/7/7 |

Goroutines by creation site, first reading: []

Goroutines by creation site, last reading: []

## In-use heap by allocation site (after a forced GC)

**baseline-after-start** (t = 0.0 min)

| leaf function | in-use MiB | objects |
|---|---|---|
| `embed.FS.ReadFile` | 1.8 | 2 |
| `text/template/parse.(*Tree).newText` | 0.0 | 1 |
| `runtime.mallocgc` | 0.0 | 1 |
| `github.com/prometheus/client_model/go.init` | 0.0 | 1 |

**after-load** (t = 60.1 min)

| leaf function | in-use MiB | objects |
|---|---|---|
| `internal/bytealg.MakeNoZero` | 78.1 | 34 |
| `github.com/skobkin/magnetico-web/internal/web/feed.render` | 24.7 | 13 |
| `github.com/skobkin/magnetico-web/internal/web/filetree.BuildArenaLimits` | 23.4 | 23 |
| `github.com/skobkin/magnetico-web/internal/web/filetree.(*skeleton).pack` | 4.8 | 7 |
| `embed.FS.ReadFile` | 1.8 | 2 |
| `github.com/skobkin/magnetico-web/internal/web/filetree.scan` | 0.2 | 2 |
| `github.com/jackc/pgx/v5/internal/stmtcache.NewLRUCache` | 0.0 | 1 |
| `text/template/parse.(*Tree).newText` | 0.0 | 1 |
| `runtime.mallocgc` | 0.0 | 1 |
| `github.com/prometheus/client_model/go.init` | 0.0 | 1 |

**idle-end** (t = 70.1 min)

| leaf function | in-use MiB | objects |
|---|---|---|
| `internal/bytealg.MakeNoZero` | 78.1 | 34 |
| `github.com/skobkin/magnetico-web/internal/web/feed.render` | 24.7 | 13 |
| `github.com/skobkin/magnetico-web/internal/web/filetree.BuildArenaLimits` | 23.4 | 23 |
| `github.com/skobkin/magnetico-web/internal/web/filetree.(*skeleton).pack` | 4.8 | 7 |
| `embed.FS.ReadFile` | 1.8 | 2 |
| `github.com/skobkin/magnetico-web/internal/web/filetree.scan` | 0.2 | 2 |
| `github.com/jackc/pgx/v5/internal/stmtcache.NewLRUCache` | 0.0 | 1 |
| `text/template/parse.(*Tree).newText` | 0.0 | 1 |
| `runtime.mallocgc` | 0.0 | 1 |
| `github.com/prometheus/client_model/go.init` | 0.0 | 1 |

