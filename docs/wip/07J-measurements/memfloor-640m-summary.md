# Run `memfloor-640m`

- git: `bc4fced0b328`; machine: go1.27.0 linux/amd64, cpu="AMD Ryzen 9 7950X3D 16-Core Processor" cores=8 GOMAXPROCS=8 GOGC=default(100) GOMEMLIMIT=unset kernel=6.12.117+deb13-amd64 mem=32102 MiB cgroup(mem="" cpu="")
- web cgroup memory.max 640M, GOMEMLIMIT 448MiB; pools {'huge': 24, 'large': 1900, 'medium': 6000, 'small': 12000}

## Segments

| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| memfloor-c32 | 32 | 0 ms | 542 | 968 | 524501 | 0 | 554571 | 0 | 0 | 0 | 0 | 214/602 | 15 | 91 | 74 | 517 | 6 |

## Processes

| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |
|---|---|---|---|---|---|---|---|
| web | 0 | 475 | 538 | 0/0 (high 0, max 0) | 3297 | 626 | 287 |

  `web` shutdown stats: time=2026-11-24T12:56:14.981+04:00 level=INFO msg="file index cache stopped" hits=7908 misses=194071 coalesced=247 builds=193824 build_errors=0 abandoned=0 evictions=193449 expired=0 replaced=0 oversize=0 retries=0 entries=375 bytes=132119124

  `web` WARN/ERROR log lines: {'ERROR: request request_id=': 554571, 'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 1}
| ingest | 0 | 33 | 15 | 0/0 (high 0, max 0) | 0 | 0 | - |

  `ingest` WARN/ERROR log lines: none
