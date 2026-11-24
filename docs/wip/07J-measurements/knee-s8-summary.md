# Run `knee-s8`

- git: `5e7ae22fcf28`; machine: go1.27.0 linux/amd64, cpu="AMD Ryzen 9 7950X3D 16-Core Processor" cores=8 GOMAXPROCS=8 GOGC=default(100) GOMEMLIMIT=unset kernel=6.12.117+deb13-amd64 mem=32102 MiB cgroup(mem="" cpu="")
- web cgroup memory.max 1G, GOMEMLIMIT 768MiB; pools {'huge': 24, 'large': 1900, 'medium': 6000, 'small': 12000}

## Segments

| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| knee-u150 | 150 | 1000 ms | 185 | 137 | 25354 | 0 | 250 | 0 | 0 | 0 | 0 | 15/552 | 6 | 14 | 11 | 483 | 10 |
| knee-u400 | 400 | 1000 ms | 185 | 349 | 64635 | 0 | 3023 | 0 | 0 | 0 | 0 | 92/286 | 20 | 72 | 32 | 269 | 12 |
| knee-u800 | 800 | 1000 ms | 187 | 501 | 93635 | 0 | 42282 | 0 | 0 | 0 | 0 | 192/368 | 57 | 117 | 97 | 348 | 22 |

## Processes

| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |
|---|---|---|---|---|---|---|---|
| web | 0 | 620 | 713 | 0/0 (high 0, max 0) | 1048 | 602 | 281 |

  `web` shutdown stats: time=2026-11-24T13:15:06.527+04:00 level=INFO msg="file index cache stopped" hits=1614 misses=41327 coalesced=16 builds=41201 build_errors=110 abandoned=0 evictions=40864 expired=0 replaced=0 oversize=0 retries=0 entries=337 bytes=127210182

  `web` WARN/ERROR log lines: {'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 1, 'WARN: request request_id=': 45555}
| ingest | 0 | 31 | 13 | 0/0 (high 0, max 0) | 0 | 0 | - |

  `ingest` WARN/ERROR log lines: none
