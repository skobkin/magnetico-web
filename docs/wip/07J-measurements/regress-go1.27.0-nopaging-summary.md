# Run `regress-go1.27.0-nopaging`

- git: `40c01770d8d2`; machine: go1.27.0 linux/amd64, cpu="AMD Ryzen 9 7950X3D 16-Core Processor" cores=8 GOMAXPROCS=8 GOGC=default(100) GOMEMLIMIT=unset kernel=6.12.117+deb13-amd64 mem=32102 MiB cgroup(mem="" cpu="")
- web cgroup memory.max 1G, GOMEMLIMIT 768MiB; pools {'huge': 24, 'large': 1900, 'medium': 6000, 'small': 12000}

## Segments

| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| regress-c4 | 4 | 0 ms | 62 | 140 | 8601 | 0 | 0 | 0 | 0 | 0 | 0 | 8/94 | 2 | 4 | 5 | 51 | 5 |
| regress-c16 | 16 | 0 ms | 61 | 118 | 7223 | 0 | 0 | 0 | 0 | 0 | 0 | 152/714 | 3 | 6 | 6 | 538 | 4 |
| regress-c32-overload | 32 | 0 ms | 60 | 895 | 54141 | 0 | 59394 | 0 | 0 | 0 | 0 | 280/1059 | 16 | 108 | 88 | 995 | 7 |
| regress-recovery-c4 | 4 | 0 ms | 31 | 169 | 5228 | 0 | 0 | 0 | 0 | 0 | 0 | 8/95 | 2 | 5 | 5 | 50 | 1 |
| regress-interactive-u50 | 50 | 1000 ms | 63 | 47 | 2987 | 0 | 9 | 0 | 0 | 0 | 0 | 9/85 | 2 | 6 | 5 | 55 | 8 |
| regress-interactive-u150 | 150 | 1000 ms | 68 | 129 | 8812 | 0 | 136 | 0 | 0 | 0 | 0 | 20/182 | 3 | 9 | 6 | 173 | 4 |
| regress-ingest-c16 | 16 | 0 ms | 122 | 212 | 25807 | 0 | 0 | 0 | 0 | 0 | 0 | 107/251 | 3 | 6 | 7 | 242 | 3 |

Ingest during `regress-ingest-c16`: sent 2435, acknowledged (202) 2435, refused {}.

Drain: {'acked': 2435, 'drain_seconds': 0.003967606, 'missing': 0, 'quarantined': 0, 'queue_left': 0, 'unverified': 0}

## Processes

| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |
|---|---|---|---|---|---|---|---|
| web | 0 | 590 | 690 | 0/0 (high 0, max 0) | 719 | 253 | 273 |

  `web` shutdown stats: time=2026-11-24T17:21:48.549+04:00 level=INFO msg="file index cache stopped" hits=1246 misses=31132 coalesced=31 builds=31101 build_errors=0 abandoned=0 evictions=30697 expired=0 replaced=0 oversize=0 retries=0 entries=404 bytes=129257972

  `web` WARN/ERROR log lines: {'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 1, 'WARN: request request_id=': 59539}
| ingest | 0 | 42 | 24 | 0/0 (high 0, max 0) | 0 | 0 | - |

  `ingest` WARN/ERROR log lines: none
