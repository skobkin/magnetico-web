# Run `memfloor-512m`

- git: `5e7ae22fcf28`; machine: go1.27.0 linux/amd64, cpu="AMD Ryzen 9 7950X3D 16-Core Processor" cores=8 GOMAXPROCS=8 GOGC=default(100) GOMEMLIMIT=unset kernel=6.12.117+deb13-amd64 mem=32102 MiB cgroup(mem="" cpu="")
- web cgroup memory.max 512M, GOMEMLIMIT 384MiB; pools {'huge': 24, 'large': 1900, 'medium': 6000, 'small': 12000}

## Segments

| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| memfloor-c32 | 32 | 0 ms | 541 | 943 | 509512 | 0 | 537842 | 0 | 0 | 0 | 0 | 217/617 | 16 | 95 | 80 | 538 | 6 |

## Processes

| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |
|---|---|---|---|---|---|---|---|
| web | 0 | 414 | 478 | 0/0 (high 0, max 0) | 4266 | 953 | 290 |

  `web` shutdown stats: time=2026-11-24T13:24:33.250+04:00 level=INFO msg="file index cache stopped" hits=7593 misses=188652 coalesced=243 builds=188409 build_errors=0 abandoned=0 evictions=188047 expired=0 replaced=0 oversize=0 retries=0 entries=362 bytes=125196620

  `web` WARN/ERROR log lines: {'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 1, 'WARN: request request_id=': 537842}
| ingest | 0 | 33 | 15 | 0/0 (high 0, max 0) | 0 | 0 | - |

  `ingest` WARN/ERROR log lines: none
