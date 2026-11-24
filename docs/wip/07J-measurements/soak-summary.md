# Run `soak`

- git: `cc7f6bf05204`; machine: go1.27.0 linux/amd64, cpu="AMD Ryzen 9 7950X3D 16-Core Processor" cores=8 GOMAXPROCS=8 GOGC=default(100) GOMEMLIMIT=unset kernel=6.12.117+deb13-amd64 mem=32102 MiB cgroup(mem="" cpu="")
- web cgroup memory.max 1G, GOMEMLIMIT 768MiB; pools {'huge': 24, 'large': 1900, 'medium': 6000, 'small': 12000}

## Segments

| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| warmup | 16 | 0 ms | 1801 | 296 | 532356 | 0 | 0 | 0 | 0 | 0 | 0 | 68/214 | 4 | 7 | 7 | 172 | 3 |
| steady | 16 | 0 ms | 5401 | 252 | 1362017 | 0 | 0 | 0 | 0 | 0 | 0 | 88/228 | 3 | 7 | 7 | 199 | 2 |
| idle | 0 | 0 ms | 1200 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | -/- | - | - | - | - | - |

Ingest during `steady`: sent 108035, acknowledged (202) 108035, refused {}.

Drain: {'acked': 108035, 'drain_seconds': 0.004117709, 'missing': 0, 'quarantined': 0, 'queue_left': 0}

## Processes

| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |
|---|---|---|---|---|---|---|---|
| web | 0 | 600 | 730 | 0/0 (high 0, max 0) | 10669 | 1703 | 280 |

  `web` shutdown stats: time=2026-11-24T11:25:08.965+04:00 level=INFO msg="file index cache stopped" hits=13527 misses=341370 coalesced=67 builds=341303 build_errors=0 abandoned=0 evictions=340958 expired=0 replaced=0 oversize=0 retries=0 entries=345 bytes=132501260

  `web` WARN/ERROR log lines: {'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 1}
| ingest | 0 | 139 | 122 | 0/0 (high 0, max 0) | 0 | 0 | - |

  `ingest` WARN/ERROR log lines: none

## Memory windows (web), steady window 30–120 min

- RSS MiB: block medians [422, 446, 429, 425, 429, 431, 450, 423, 430]; slope +1.2 MiB/h = +0.3 % of the plateau per hour
- cgroup MiB: block medians [502, 570, 550, 529, 532, 540, 540, 555, 561]; slope +18.0 MiB/h = +3.3 % of the plateau per hour
- heap-after-GC MiB: block medians [175, 183, 178, 176, 179, 176, 194, 182, 184]; slope +6.3 MiB/h = +3.5 % of the plateau per hour
- file descriptors 7–55, threads 8–9 (whole run)
- idle phase: RSS 464 → 257 MiB, cgroup 594 → 464 MiB, fds 33 → 7, threads 9 → 9
- ingest queue depth in the steady window: max 1, median 0, slope +0.0 rows/h
