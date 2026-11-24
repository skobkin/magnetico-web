# Run `knee-s4`

- git: `bc4fced0b328`; machine: go1.27.0 linux/amd64, cpu="AMD Ryzen 9 7950X3D 16-Core Processor" cores=8 GOMAXPROCS=8 GOGC=default(100) GOMEMLIMIT=unset kernel=6.12.117+deb13-amd64 mem=32102 MiB cgroup(mem="" cpu="")
- web cgroup memory.max 1G, GOMEMLIMIT 768MiB; pools {'huge': 24, 'large': 1900, 'medium': 6000, 'small': 12000}

## Segments

| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| knee-u150 | 150 | 1000 ms | 184 | 120 | 22079 | 0 | 2773 | 0 | 0 | 0 | 0 | 76/749 | 3 | 10 | 7 | 599 | 6 |
| knee-u400 | 400 | 1000 ms | 188 | 269 | 50527 | 0 | 18178 | 0 | 0 | 0 | 0 | 137/347 | 6 | 26 | 11 | 326 | 6 |
| knee-u800 | 800 | 1000 ms | 188 | 440 | 82892 | 0 | 57562 | 0 | 0 | 0 | 0 | 150/347 | 26 | 80 | 42 | 321 | 12 |

## Processes

| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |
|---|---|---|---|---|---|---|---|
| web | 0 | 607 | 700 | 0/0 (high 0, max 0) | 936 | 248 | 288 |

  `web` shutdown stats: time=2026-11-24T13:05:43.899+04:00 level=INFO msg="file index cache stopped" hits=1677 misses=42195 coalesced=15 builds=42059 build_errors=121 abandoned=0 evictions=41683 expired=0 replaced=0 oversize=0 retries=0 entries=376 bytes=126899985

  `web` WARN/ERROR log lines: {'ERROR: request request_id=': 78513, 'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 1}
| ingest | 0 | 31 | 13 | 0/0 (high 0, max 0) | 0 | 0 | - |

  `ingest` WARN/ERROR log lines: none
