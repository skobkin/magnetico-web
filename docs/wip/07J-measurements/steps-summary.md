# Run `steps`

- git: `0efd2b7d8b57`; machine: go1.27.0 linux/amd64, cpu="AMD Ryzen 9 7950X3D 16-Core Processor" cores=8 GOMAXPROCS=8 GOGC=default(100) GOMEMLIMIT=unset kernel=6.12.117+deb13-amd64 mem=32102 MiB cgroup(mem="" cpu="")
- web cgroup memory.max 1G, GOMEMLIMIT 768MiB; pools {'huge': 24, 'large': 1900, 'medium': 6000, 'small': 12000}

## Segments

| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| series1-c4 | 4 | 0 ms | 181 | 235 | 42477 | 0 | 0 | 0 | 0 | 0 | 0 | 8/84 | 2 | 5 | 5 | 16 | 3 |
| series1-c8 | 8 | 0 ms | 181 | 261 | 47111 | 0 | 0 | 0 | 0 | 0 | 0 | 24/129 | 4 | 7 | 7 | 87 | 3 |
| series1-c16 | 16 | 0 ms | 181 | 255 | 46111 | 0 | 0 | 0 | 0 | 0 | 0 | 73/253 | 4 | 8 | 7 | 206 | 3 |
| series1-c32-overload | 32 | 0 ms | 180 | 1006 | 181268 | 0 | 179859 | 0 | 0 | 0 | 0 | 153/417 | 16 | 89 | 72 | 355 | 6 |
| series1-c64-overload | 64 | 0 ms | 181 | 1081 | 195689 | 0 | 202857 | 0 | 0 | 0 | 0 | 178/387 | 54 | 192 | 201 | 355 | 13 |
| series1-recovery-c4 | 4 | 0 ms | 30 | 293 | 8823 | 0 | 0 | 0 | 0 | 0 | 0 | 8/75 | 2 | 5 | 5 | 17 | 1 |
| series1-interactive-u50 | 50 | 1000 ms | 184 | 47 | 8695 | 0 | 5 | 0 | 0 | 0 | 0 | 8/79 | 2 | 4 | 5 | 15 | 4 |
| series1-interactive-u150 | 150 | 1000 ms | 184 | 144 | 26445 | 0 | 225 | 0 | 0 | 0 | 0 | 9/111 | 2 | 5 | 6 | 47 | 3 |
| series1-interactive-u400 | 400 | 1000 ms | 187 | 322 | 60051 | 0 | 9622 | 0 | 0 | 0 | 0 | 60/234 | 5 | 16 | 10 | 190 | 4 |
| series2-c4 | 4 | 0 ms | 180 | 240 | 43206 | 0 | 0 | 0 | 0 | 0 | 0 | 8/81 | 2 | 4 | 5 | 15 | 2 |
| series2-c8 | 8 | 0 ms | 181 | 283 | 51163 | 0 | 0 | 0 | 0 | 0 | 0 | 24/120 | 3 | 6 | 7 | 81 | 3 |
| series2-c16 | 16 | 0 ms | 181 | 247 | 44722 | 0 | 0 | 0 | 0 | 0 | 0 | 75/270 | 4 | 8 | 7 | 221 | 3 |
| series2-c32-overload | 32 | 0 ms | 180 | 1002 | 180803 | 0 | 178486 | 0 | 0 | 0 | 0 | 151/381 | 16 | 91 | 69 | 332 | 6 |
| series2-c64-overload | 64 | 0 ms | 180 | 1094 | 197208 | 0 | 202636 | 0 | 0 | 0 | 0 | 172/378 | 55 | 183 | 191 | 347 | 13 |
| series2-recovery-c4 | 4 | 0 ms | 30 | 239 | 7266 | 0 | 0 | 0 | 0 | 0 | 0 | 8/82 | 2 | 4 | 5 | 14 | 1 |
| series2-interactive-u50 | 50 | 1000 ms | 184 | 48 | 8808 | 0 | 0 | 0 | 0 | 0 | 0 | 8/78 | 2 | 4 | 5 | 18 | 3 |
| series2-interactive-u150 | 150 | 1000 ms | 185 | 143 | 26457 | 0 | 211 | 0 | 0 | 0 | 0 | 9/143 | 3 | 6 | 6 | 71 | 3 |
| series2-interactive-u400 | 400 | 1000 ms | 186 | 328 | 60916 | 0 | 9081 | 0 | 0 | 0 | 0 | 57/231 | 7 | 22 | 10 | 186 | 5 |

## Processes

| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |
|---|---|---|---|---|---|---|---|
| web | 0 | 626 | 657 | 0/0 (high 0, max 0) | 3404 | 574 | 291 |

  `web` shutdown stats: time=2026-11-24T08:39:42.589+04:00 level=INFO msg="file index cache stopped" hits=7609 misses=181763 coalesced=269 builds=179911 build_errors=1583 abandoned=0 evictions=179559 expired=0 replaced=0 oversize=0 retries=0 entries=352 bytes=133161828
| ingest | 0 | 33 | 15 | 0/0 (high 0, max 0) | 0 | 0 | - |
| web | 0 | 623 | 650 | 0/0 (high 0, max 0) | 3481 | 506 | 297 |

  `web` shutdown stats: time=2026-11-24T09:04:34.905+04:00 level=INFO msg="file index cache stopped" hits=7326 misses=182016 coalesced=282 builds=180865 build_errors=869 abandoned=0 evictions=180434 expired=0 replaced=0 oversize=0 retries=0 entries=431 bytes=120786783
| ingest | 0 | 32 | 13 | 0/0 (high 0, max 0) | 0 | 0 | - |
