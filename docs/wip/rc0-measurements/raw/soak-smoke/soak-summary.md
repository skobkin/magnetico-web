# soak09a summary

## Events

| t (min) | event | details |
|---|---|---|
| 1.1 | web_restart | down_s=0.428664088, exit_code=0, label=cold-1 |
| 3.0 | rotate_token | crawler=soak5, new_token_works_at_once=True, old_token_accepted_for_s=2.5037824349999998, old_token_refused=True, since_s=2.50443376 |
| 5.3 | disable_enable | accepted_again=True, accepted_again_after_s=0.000628323, crawler=soak4, disabled_for_s=12, refused_after_s=35.062893216 |
| 6.0 | ingest_restart | exit_code=0, ready=True, ready_after_s=0.501107948, stop_s=0.063999488 |
| 7.5 | stats_outage_begin | hold_s=30 |
| 8.0 | stats_outage_end | dashboard_fresh=True, dashboard_fresh_after_s=0.00464396, first_flush_after_release_s=1.001610378, flushed=True, held_s=31.007350566, torrents_added_during=953 |
| 9.1 | web_restart | down_s=0.449961748, exit_code=0, label=cold-2 |

## Web load by phase (rounds of two minutes; percentiles are the median and the worst of the rounds)

| phase | rounds | requests | req/s (median) | ok | busy 503 | timeouts | other 5xx | unexpected 4xx | 429 | transport |
|---|---|---|---|---|---|---|---|---|---|---|
| interactive-warm | 1 | 2697 | 43.5 | 2690 | 7 | 0 | 0 | 0 | 0 | 0 |
| interactive-cold | 2 | 3810 | 41.0 | 3791 | 19 | 0 | 0 | 0 | 0 | 0 |
| interactive | 7 | 29370 | 46.5 | 29330 | 40 | 0 | 0 | 0 | 0 | 0 |
| closed-loop | 1 | 8397 | 96.2 | 8397 | 0 | 0 | 0 | 0 | 0 | 0 |

### Latency by request class and phase (p50 / p95 / p99 in ms: median of rounds; max = worst p99 of any round)

| phase | class | n | p50 | p95 | p99 | worst p99 |
|---|---|---|---|---|---|---|
| interactive-warm | api | 227 | 10.0 | 998.3 | 1847.7 | 1847.7 |
| interactive-warm | apifiles | 44 | 2.4 | 75.2 | 223.9 | 223.9 |
| interactive-warm | details | 422 | 1.4 | 3.7 | 77.0 | 77.0 |
| interactive-warm | dirs | 228 | 2.0 | 77.8 | 153.7 | 153.7 |
| interactive-warm | magnet | 80 | 0.3 | 1.6 | 5.5 | 5.5 |
| interactive-warm | rss | 58 | 2.2 | 14.6 | 82.4 | 82.4 |
| interactive-warm | search | 946 | 11.0 | 677.6 | 2002.4 | 2002.4 |
| interactive-warm | searchpage | 559 | 10.3 | 340.2 | 1603.2 | 1603.2 |
| interactive-warm | tree | 133 | 2.8 | 7.6 | 83.7 | 83.7 |
| interactive-cold | api | 318 | 14.2 | 1316.9 | 1845.7 | 1986.7 |
| interactive-cold | apifiles | 63 | 2.4 | 65.0 | 94.9 | 126.6 |
| interactive-cold | details | 597 | 1.4 | 5.4 | 50.1 | 50.6 |
| interactive-cold | dirs | 348 | 2.1 | 39.5 | 179.0 | 182.6 |
| interactive-cold | magnet | 75 | 0.3 | 2.1 | 6.3 | 9.6 |
| interactive-cold | rss | 90 | 2.3 | 13.5 | 44.7 | 75.4 |
| interactive-cold | search | 1363 | 15.1 | 1155.8 | 2631.5 | 2936.5 |
| interactive-cold | searchpage | 791 | 11.1 | 346.0 | 1475.0 | 1759.6 |
| interactive-cold | tree | 165 | 2.9 | 33.8 | 75.4 | 88.8 |
| interactive | api | 2409 | 15.6 | 45.3 | 149.6 | 1776.0 |
| interactive | apifiles | 422 | 1.8 | 7.8 | 164.3 | 189.8 |
| interactive | details | 4573 | 1.3 | 3.2 | 11.4 | 17.9 |
| interactive | dirs | 2905 | 1.9 | 9.3 | 137.6 | 157.4 |
| interactive | magnet | 660 | 0.2 | 0.7 | 2.4 | 11.8 |
| interactive | rss | 678 | 1.7 | 9.0 | 12.6 | 14.7 |
| interactive | search | 10575 | 11.1 | 116.0 | 1773.6 | 1923.1 |
| interactive | searchpage | 5956 | 10.2 | 126.8 | 997.6 | 1625.5 |
| interactive | tree | 1192 | 2.6 | 6.9 | 17.0 | 36.9 |
| closed-loop | api | 647 | 137.3 | 576.2 | 1647.6 | 1647.6 |
| closed-loop | apifiles | 109 | 2.6 | 9.6 | 99.9 | 99.9 |
| closed-loop | details | 1305 | 1.4 | 5.3 | 9.0 | 9.0 |
| closed-loop | dirs | 791 | 2.4 | 11.6 | 185.0 | 185.0 |
| closed-loop | magnet | 188 | 0.2 | 0.8 | 5.8 | 5.8 |
| closed-loop | rss | 175 | 2.3 | 9.6 | 18.8 | 18.8 |
| closed-loop | search | 3079 | 143.9 | 795.5 | 2106.4 | 2106.4 |
| closed-loop | searchpage | 1781 | 141.9 | 792.3 | 1992.2 | 1992.2 |
| closed-loop | tree | 322 | 3.2 | 10.1 | 13.0 | 13.0 |

## Administrator requests

ok 288, unexpected 0.

| request | n | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| admin /admin | 27 | 7.4 | 38.2 | 3001.5 | 3001.5 |
| admin /admin/api/overview | 27 | 0.6 | 6.5 | 3001.1 | 3001.1 |
| admin /admin/api/series | 104 | 1.6 | 15.3 | 28.1 | 1107.6 |
| admin /admin/crawlers | 26 | 8.8 | 15.7 | 15.8 | 15.8 |
| admin /admin/invitations | 26 | 1.2 | 2.7 | 2.7 | 2.7 |
| admin /admin/quarantine | 26 | 1.2 | 2.5 | 3.7 | 3.7 |
| admin /admin/trackers | 26 | 1.3 | 2.1 | 2.6 | 2.6 |
| admin /admin/users | 26 | 1.8 | 2.4 | 2.6 | 2.6 |

## CPU and memory by service (10 s samples, 30-minute windows)


### web

| window (min) | CPU % of one core | RSS MiB (median) | RSS max | cgroup max MiB | threads max | fds median | fds max |
|---|---|---|---|---|---|---|---|
| 0-30 | 9.5 | 407.2 | 515.4 | 653.7 | 9 | 75 | 108 |

### ingest

| window (min) | CPU % of one core | RSS MiB (median) | RSS max | cgroup max MiB | threads max | fds median | fds max |
|---|---|---|---|---|---|---|---|
| 0-30 | 2.5 | 187.2 | 222.1 | 221.2 | 8 | 22 | 26 |

## Goroutines, heap and PostgreSQL (monitor, 30-minute windows, medians)

| window (min) | web goroutines | ingest goroutines | web heap_alloc MiB | ingest heap_alloc MiB | PG conns | lock waiters max | oldest tx s max | queue rows max | stats dead tuples max | ingest dead tuples max |
|---|---|---|---|---|---|---|---|---|---|---|
| 0-30 | – | – | – | – | 31 | 0 | 3.0 | 23 | 98 | 2148 |

Monitor readings with an error field: none.

cgroup memory events at the end: web {'high': 0, 'low': 0, 'max': 0, 'oom': 0, 'oom_group_kill': 0, 'oom_kill': 0}, ingest {'high': 0, 'low': 0, 'max': 0, 'oom': 0, 'oom_group_kill': 0, 'oom_kill': 0}.


## Heap after GC (gctrace) and shutdown

| process | GCs | heap after GC max MiB | median | median, first third | median, last third | cgroup peak MiB | exit | WARN/ERROR lines |
|---|---|---|---|---|---|---|---|---|
| ingest | 49 | 74.0 | 74.0 | 73.0 | 74.0 | 237.31640625 | 0 | {} |
| web | 450 | 232.0 | 148.5 | 57.0 | 160.0 | 653.6953125 | 0 | {'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 3, 'WARN: request request_id=': 66} |

## End-state checks (after the graceful stop of ingest)

| check | value |
|---|---|
| acknowledged | 36874 |
| agg_hour_rows | 1 |
| covered_minutes | 16 |
| duplicates | 0 |
| enqueued | 36874 |
| received | 36874 |
| refused | 0 |
| rollup_mismatches | [] |
| run_minutes | 15.56251690125 |
| settled | 36874 |
| torrents_created | 36874 |
| torrents_stored_by_crawlers | 36874 |
