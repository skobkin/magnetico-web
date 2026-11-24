# Run `regress-final-pins-nopaging`

- git: `40c01770d8d2`; machine: go1.27.4 linux/amd64, cpu="AMD Ryzen 9 7950X3D 16-Core Processor" cores=8 GOMAXPROCS=8 GOGC=default(100) GOMEMLIMIT=unset kernel=6.12.117+deb13-amd64 mem=32102 MiB cgroup(mem="" cpu="")
- web cgroup memory.max 1G, GOMEMLIMIT 768MiB; pools {'huge': 24, 'large': 1900, 'medium': 6000, 'small': 12000}

## Segments

| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| regress-c4 | 4 | 0 ms | 60 | 127 | 7642 | 0 | 0 | 0 | 0 | 0 | 0 | 8/103 | 2 | 5 | 5 | 53 | 6 |
| regress-c16 | 16 | 0 ms | 62 | 134 | 8301 | 0 | 0 | 0 | 0 | 0 | 0 | 147/642 | 3 | 6 | 6 | 459 | 5 |
| regress-c32-overload | 32 | 0 ms | 61 | 931 | 56735 | 0 | 62775 | 0 | 0 | 0 | 0 | 331/979 | 14 | 97 | 80 | 852 | 7 |
| regress-recovery-c4 | 4 | 0 ms | 31 | 191 | 5828 | 0 | 0 | 0 | 0 | 0 | 0 | 8/79 | 2 | 4 | 5 | 48 | 2 |
| regress-interactive-u50 | 50 | 1000 ms | 65 | 46 | 2954 | 0 | 11 | 0 | 0 | 0 | 0 | 9/111 | 2 | 5 | 6 | 53 | 11 |
| regress-interactive-u150 | 150 | 1000 ms | 66 | 133 | 8739 | 0 | 145 | 0 | 0 | 0 | 0 | 34/234 | 4 | 26 | 6 | 212 | 5 |
| regress-ingest-c16 | 16 | 0 ms | 123 | 206 | 25203 | 0 | 0 | 0 | 0 | 0 | 0 | 108/270 | 4 | 9 | 7 | 252 | 3 |

Ingest during `regress-ingest-c16`: sent 2450, acknowledged (202) 2450, refused {}.

Drain: {'acked': 2450, 'drain_seconds': 0.00357141, 'missing': 0, 'quarantined': 0, 'queue_left': 0, 'unverified': 0}

## Processes

| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |
|---|---|---|---|---|---|---|---|
| web | 0 | 608 | 663 | 0/0 (high 0, max 0) | 735 | 392 | 278 |

  `web` shutdown stats: time=2026-11-24T17:13:33.954+04:00 level=INFO msg="file index cache stopped" hits=1230 misses=32095 coalesced=19 builds=32076 build_errors=0 abandoned=0 evictions=31589 expired=0 replaced=0 oversize=0 retries=0 entries=487 bytes=129224966

  `web` WARN/ERROR log lines: {'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 1, 'WARN: request request_id=': 62931}
| ingest | 0 | 40 | 22 | 0/0 (high 0, max 0) | 0 | 0 | - |

  `ingest` WARN/ERROR log lines: none
