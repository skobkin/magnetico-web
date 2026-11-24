# Run `memfloor-768m`

- git: `bc4fced0b328`; machine: go1.27.0 linux/amd64, cpu="AMD Ryzen 9 7950X3D 16-Core Processor" cores=8 GOMAXPROCS=8 GOGC=default(100) GOMEMLIMIT=unset kernel=6.12.117+deb13-amd64 mem=32102 MiB cgroup(mem="" cpu="")
- web cgroup memory.max 768M, GOMEMLIMIT 512MiB; pools {'huge': 24, 'large': 1900, 'medium': 6000, 'small': 12000}

## Segments

| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| memfloor-c32 | 32 | 0 ms | 540 | 967 | 522418 | 0 | 554756 | 0 | 0 | 0 | 0 | 219/628 | 16 | 89 | 72 | 576 | 6 |

## Processes

| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |
|---|---|---|---|---|---|---|---|
| web | 0 | 537 | 605 | 0/0 (high 0, max 0) | 3034 | 854 | 291 |

  `web` shutdown stats: time=2026-11-24T12:47:06.854+04:00 level=INFO msg="file index cache stopped" hits=7837 misses=193551 coalesced=210 builds=193341 build_errors=0 abandoned=0 evictions=192906 expired=0 replaced=0 oversize=0 retries=0 entries=435 bytes=126967076

  `web` WARN/ERROR log lines: {'ERROR: request request_id=': 554756, 'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 1}
| ingest | 0 | 33 | 15 | 0/0 (high 0, max 0) | 0 | 0 | - |

  `ingest` WARN/ERROR log lines: none
