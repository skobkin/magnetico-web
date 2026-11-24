# Run `regress-final-pins`

- git: `40c01770d8d2`; machine: go1.27.4 linux/amd64, cpu="AMD Ryzen 9 7950X3D 16-Core Processor" cores=8 GOMAXPROCS=8 GOGC=default(100) GOMEMLIMIT=unset kernel=6.12.117+deb13-amd64 mem=32102 MiB cgroup(mem="" cpu="")
- web cgroup memory.max 1G, GOMEMLIMIT 768MiB; pools {'huge': 24, 'large': 1900, 'medium': 6000, 'small': 12000}

## Segments

| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| regress-c4 | 4 | 0 ms | 60 | 109 | 6550 | 0 | 0 | 0 | 0 | 0 | 0 | 8/97 | 2 | 5 | 6 | 52 | 6 |
| regress-c16 | 16 | 0 ms | 61 | 139 | 8504 | 0 | 0 | 0 | 0 | 0 | 0 | 125/379 | 4 | 8 | 8 | 352 | 8 |
| regress-c32-overload | 32 | 0 ms | 62 | 843 | 52277 | 0 | 57583 | 0 | 0 | 0 | 0 | 292/942 | 16 | 110 | 85 | 975 | 7 |
| regress-recovery-c4 | 4 | 0 ms | 32 | 122 | 3888 | 0 | 0 | 0 | 0 | 0 | 0 | 7/98 | 2 | 4 | 5 | 48 | 1 |
| regress-interactive-u50 | 50 | 1000 ms | 63 | 58 | 3638 | 0 | 7 | 0 | 0 | 0 | 0 | 9/93 | 2 | 32 | 5 | 78 | 9 |
| regress-interactive-u150 | 150 | 1000 ms | 67 | 142 | 9492 | 0 | 810 | 0 | 0 | 0 | 0 | 80/380 | 4 | 18 | 7 | 358 | 6 |
| regress-ingest-c16 | 16 | 0 ms | 121 | 196 | 23636 | 0 | 0 | 0 | 0 | 0 | 0 | 102/242 | 4 | 9 | 7 | 222 | 5 |

Ingest during `regress-ingest-c16`: sent 2410, acknowledged (202) 2410, refused {}.

Drain: {'acked': 2410, 'drain_seconds': 0.003388045, 'missing': 0, 'quarantined': 0, 'queue_left': 0, 'unverified': 0}

## Processes

| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |
|---|---|---|---|---|---|---|---|
| web | 0 | 571 | 643 | 0/0 (high 0, max 0) | 760 | 177 | 256 |

  `web` shutdown stats: time=2026-11-24T17:05:26.199+04:00 level=INFO msg="file index cache stopped" hits=986 misses=27930 coalesced=17 builds=27913 build_errors=0 abandoned=0 evictions=27590 expired=0 replaced=0 oversize=0 retries=0 entries=323 bytes=134215571

  `web` WARN/ERROR log lines: {'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 1, 'WARN: request request_id=': 58400}
| ingest | 0 | 40 | 22 | 0/0 (high 0, max 0) | 0 | 0 | - |

  `ingest` WARN/ERROR log lines: none
