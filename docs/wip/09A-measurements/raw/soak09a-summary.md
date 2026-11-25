# soak09a summary

## Events

| t (min) | event | details |
|---|---|---|
| 20.1 | web_restart | down_s=0.254663743, exit_code=0, label=cold-1 |
| 60.4 | rotate_token | crawler=soak5, new_token_works_at_once=True, old_token_accepted_for_s=24.039722804, old_token_refused=True, since_s=24.043182304 |
| 94.7 | disable_enable | accepted_again=True, accepted_again_after_s=0.000569762, crawler=soak4, disabled_for_s=240, refused_after_s=40.56379151 |
| 120.0 | ingest_restart | exit_code=0, ready=True, ready_after_s=0.000681371, stop_s=0.008813425 |
| 150.0 | stats_outage_begin | hold_s=600 |
| 160.0 | stats_outage_end | dashboard_fresh=True, dashboard_fresh_after_s=0.000599517, first_flush_after_release_s=1.001429166, flushed=True, held_s=601.002866722, torrents_added_during=18056 |
| 180.1 | web_restart | down_s=0.273454438, exit_code=0, label=cold-2 |

## Web load by phase (rounds of two minutes; percentiles are the median and the worst of the rounds)

| phase | rounds | requests | req/s (median) | ok | busy 503 | timeouts | other 5xx | unexpected 4xx | 429 | transport |
|---|---|---|---|---|---|---|---|---|---|---|
| interactive-warm | 10 | 57388 | 47.6 | 57369 | 19 | 0 | 0 | 0 | 0 | 0 |
| interactive-cold | 16 | 82734 | 46.0 | 82666 | 68 | 0 | 0 | 0 | 0 | 0 |
| interactive | 102 | 590045 | 46.9 | 589770 | 275 | 0 | 0 | 0 | 0 | 0 |
| closed-loop | 15 | 176213 | 100.1 | 176213 | 0 | 0 | 0 | 0 | 0 | 0 |

### Latency by request class and phase (p50 / p95 / p99 in ms: median of rounds; max = worst p99 of any round)

| phase | class | n | p50 | p95 | p99 | worst p99 |
|---|---|---|---|---|---|---|
| interactive-warm | api | 4663 | 14.1 | 32.8 | 91.6 | 1572.6 |
| interactive-warm | apifiles | 906 | 1.7 | 7.0 | 156.1 | 282.4 |
| interactive-warm | details | 9089 | 1.2 | 1.9 | 6.8 | 16.9 |
| interactive-warm | dirs | 5468 | 1.6 | 4.7 | 147.1 | 235.6 |
| interactive-warm | magnet | 1381 | 0.2 | 0.4 | 1.2 | 5.2 |
| interactive-warm | rss | 1318 | 1.5 | 8.7 | 11.1 | 76.5 |
| interactive-warm | search | 20494 | 10.2 | 98.4 | 598.3 | 1776.9 |
| interactive-warm | searchpage | 11754 | 9.1 | 133.4 | 812.4 | 1586.6 |
| interactive-warm | tree | 2315 | 2.2 | 5.3 | 9.9 | 27.5 |
| interactive-cold | api | 6633 | 89.1 | 172.5 | 275.4 | 1949.1 |
| interactive-cold | apifiles | 1205 | 1.8 | 12.9 | 172.2 | 515.8 |
| interactive-cold | details | 13161 | 1.2 | 2.2 | 6.6 | 20.9 |
| interactive-cold | dirs | 8016 | 1.7 | 4.9 | 160.8 | 279.2 |
| interactive-cold | magnet | 2031 | 0.2 | 0.4 | 1.2 | 2.8 |
| interactive-cold | rss | 1907 | 1.4 | 8.4 | 10.2 | 74.6 |
| interactive-cold | search | 29500 | 11.8 | 176.2 | 622.8 | 2002.5 |
| interactive-cold | searchpage | 17048 | 9.3 | 125.7 | 714.6 | 1676.3 |
| interactive-cold | tree | 3233 | 2.3 | 5.5 | 9.0 | 30.3 |
| interactive | api | 47150 | 111.0 | 145.3 | 215.3 | 1932.3 |
| interactive | apifiles | 9513 | 1.8 | 4.9 | 156.2 | 374.0 |
| interactive | details | 93977 | 1.2 | 2.0 | 5.5 | 15.5 |
| interactive | dirs | 56118 | 1.7 | 4.7 | 142.8 | 179.0 |
| interactive | magnet | 14032 | 0.2 | 0.4 | 1.5 | 9.6 |
| interactive | rss | 14153 | 1.3 | 8.3 | 10.4 | 21.2 |
| interactive | search | 210815 | 10.8 | 146.0 | 582.4 | 2165.8 |
| interactive | searchpage | 120917 | 9.1 | 112.8 | 615.6 | 1646.6 |
| interactive | tree | 23370 | 2.3 | 5.2 | 8.3 | 29.8 |
| closed-loop | api | 13816 | 306.4 | 524.5 | 674.3 | 2059.5 |
| closed-loop | apifiles | 2707 | 1.9 | 6.5 | 157.9 | 184.0 |
| closed-loop | details | 27901 | 1.2 | 3.3 | 6.6 | 10.1 |
| closed-loop | dirs | 16932 | 1.8 | 6.4 | 159.9 | 182.6 |
| closed-loop | magnet | 4231 | 0.1 | 0.3 | 0.9 | 5.5 |
| closed-loop | rss | 4093 | 1.2 | 3.5 | 10.4 | 11.9 |
| closed-loop | search | 63299 | 215.8 | 476.6 | 736.5 | 2406.0 |
| closed-loop | searchpage | 36284 | 193.5 | 409.6 | 760.3 | 1822.9 |
| closed-loop | tree | 6950 | 2.5 | 6.5 | 10.3 | 30.7 |

## Administrator requests

ok 5669, unexpected 0.

| request | n | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| admin /admin | 516 | 4.9 | 10.8 | 8001.0 | 8001.6 |
| admin /admin/api/overview | 516 | 0.5 | 4.2 | 8000.7 | 8002.4 |
| admin /admin/api/series | 2062 | 3.3 | 11.1 | 17.6 | 16003.8 |
| admin /admin/crawlers | 515 | 5.9 | 10.5 | 14.9 | 20.6 |
| admin /admin/invitations | 515 | 0.9 | 1.5 | 3.1 | 20.6 |
| admin /admin/quarantine | 515 | 0.8 | 1.4 | 2.7 | 18.8 |
| admin /admin/trackers | 515 | 0.8 | 1.3 | 3.1 | 16.4 |
| admin /admin/users | 515 | 1.2 | 2.0 | 4.0 | 23.4 |

## CPU and memory by service (10 s samples, 30-minute windows)


### web

| window (min) | CPU % of one core | RSS MiB (median) | RSS max | cgroup max MiB | threads max | fds median | fds max |
|---|---|---|---|---|---|---|---|
| 0-30 | 9.0 | 418.6 | 514.3 | 594.7 | 9 | 76 | 92 |
| 30-60 | 8.5 | 427.9 | 531.5 | 657.8 | 9 | 76 | 116 |
| 60-90 | 8.5 | 421.2 | 515.0 | 648.1 | 10 | 76 | 91 |
| 90-120 | 8.6 | 430.9 | 522.1 | 638.5 | 10 | 77 | 86 |
| 120-150 | 8.6 | 432.8 | 545.4 | 672.7 | 10 | 77 | 92 |
| 150-180 | 8.3 | 420.6 | 551.3 | 627.1 | 10 | 78 | 91 |
| 180-210 | 8.7 | 433.4 | 567.2 | 669.3 | 10 | 78 | 120 |
| 210-240 | 8.7 | 420.2 | 529.5 | 655.3 | 9 | 79 | 87 |
| 240-270 | 18.3 | 427.1 | 525.6 | 646.5 | 9 | 58 | 83 |
| 270-300 | 5.8 | 410.6 | 527.9 | 673.7 | 9 | 60 | 86 |
| 300-330 | 0.0 | 477.7 | 489.9 | 466.9 | 9 | 12 | 28 |

### ingest

| window (min) | CPU % of one core | RSS MiB (median) | RSS max | cgroup max MiB | threads max | fds median | fds max |
|---|---|---|---|---|---|---|---|
| 0-30 | 2.4 | 191.0 | 224.1 | 273.8 | 8 | 27 | 29 |
| 30-60 | 2.3 | 255.5 | 286.0 | 292.9 | 8 | 26 | 28 |
| 60-90 | 2.4 | 249.6 | 272.7 | 319.2 | 8 | 24 | 26 |
| 90-120 | 2.3 | 255.8 | 284.7 | 321.1 | 8 | 23 | 25 |
| 120-150 | 2.4 | 190.7 | 252.4 | 256.8 | 8 | 23 | 24 |
| 150-180 | 2.4 | 198.7 | 261.6 | 265.6 | 8 | 23 | 24 |
| 180-210 | 2.4 | 241.7 | 261.2 | 264.5 | 8 | 23 | 23 |
| 210-240 | 2.4 | 241.3 | 272.0 | 275.8 | 8 | 23 | 24 |
| 240-270 | 2.7 | 244.9 | 263.8 | 266.6 | 8 | 24 | 25 |
| 270-300 | 1.4 | 241.8 | 261.7 | 294.8 | 8 | 25 | 26 |
| 300-330 | 0.1 | 169.2 | 171.9 | 150.6 | 8 | 14 | 19 |

## Goroutines, heap and PostgreSQL (monitor, 30-minute windows, medians)

| window (min) | web goroutines | ingest goroutines | web heap_alloc MiB | ingest heap_alloc MiB | PG conns | lock waiters max | oldest tx s max | queue rows max | stats dead tuples max | ingest dead tuples max |
|---|---|---|---|---|---|---|---|---|---|---|
| 0-30 | 53 | 28 | 272.3 | 115.4 | 31 | 0 | 2.4 | 5 | 117 | 3241 |
| 30-60 | 58 | 27 | 231.6 | 104.6 | 32 | 0 | 1.6 | 7 | 162 | 3299 |
| 60-90 | 60 | 26 | 263.0 | 119.1 | 31 | 0 | 1.1 | 6 | 173 | 3283 |
| 90-120 | 60 | 25 | 270.3 | 113.1 | 31 | 0 | 0.8 | 29 | 196 | 3433 |
| 120-150 | 60 | 25 | 244.3 | 114.8 | 31 | 0 | 1.7 | 8 | 159 | 3271 |
| 150-180 | 63 | 25 | 250.1 | 120.0 | 31 | 2 | 541.6 | 0 | 163 | 35067 |
| 180-210 | 54 | 25 | 256.2 | 121.3 | 31 | 0 | 2.7 | 0 | 208 | 3150 |
| 210-240 | 63 | 25 | 266.4 | 127.9 | 31 | 0 | 2.7 | 0 | 165 | 3063 |
| 240-270 | 55 | 25 | 261.9 | 114.9 | 28 | 0 | 2.4 | 1 | 150 | 2924 |
| 270-300 | 64 | 25 | 240.4 | 126.9 | 33 | 0 | 2.8 | 0 | 172 | 2859 |

Monitor readings with an error field: none.

cgroup memory events at the end: web {'high': 0, 'low': 0, 'max': 0, 'oom': 0, 'oom_group_kill': 0, 'oom_kill': 0}, ingest {'high': 0, 'low': 0, 'max': 0, 'oom': 0, 'oom_group_kill': 0, 'oom_kill': 0}.


## Heap after GC (gctrace) and shutdown

| process | GCs | heap after GC max MiB | median | median, first third | median, last third | cgroup peak MiB | exit | WARN/ERROR lines |
|---|---|---|---|---|---|---|---|---|
| ingest | 738 | 80.0 | 77.0 | 77.0 | 77.0 | 295.51171875 | 0 | {} |
| web | 5679 | 268.0 | 175.0 | 175.0 | 176.0 | 703.48828125 | 0 | {'WARN: development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address': 3, 'WARN: request request_id=': 362} |

## End-state checks (after the graceful stop of ingest)

| check | value |
|---|---|
| acknowledged | 651593 |
| agg_hour_rows | 6 |
| covered_minutes | 311 |
| duplicates | 0 |
| enqueued | 651593 |
| received | 651593 |
| refused | 0 |
| rollup_mismatches | [] |
| run_minutes | 310.0788656671 |
| settled | 651593 |
| torrents_created | 651593 |
| torrents_stored_by_crawlers | 651593 |

## Persisted aggregates (from the benchmark database)

| figure | value |
|---|---|
| authentication cache hit rate | 99.114 % (164024 hits, 1467 misses) |
| authentication database lookups | 1464 for 165491 presented credentials |
| authentication busy refusals / storage failures | 0 / 0 |
| records per submit request (batch utilisation) | 8.2 (651593 records in 79651 requests) |
| hashes per existence request | 60.6 (5203860 hashes in 85821 requests) |
| existence cache hit rate | 64.0 % |

### Hourly aggregates

| hour (UTC) | received | acknowledged | enqueued | settled new | covered minutes |
|---|---|---|---|---|---|
| 11-25 07:00 | 65331 | 65331 | 65331 | 65304 | 30 |
| 11-25 08:00 | 136761 | 136761 | 136761 | 136788 | 60 |
| 11-25 09:00 | 136806 | 136806 | 136806 | 136806 | 60 |
| 11-25 10:00 | 136805 | 136805 | 136805 | 136805 | 60 |
| 11-25 11:00 | 136763 | 136763 | 136763 | 136756 | 60 |
| 11-25 12:00 | 39127 | 39127 | 39127 | 39134 | 41 |

### Statistics tables after the run

| table | size |
|---|---|
| snapshot | 88 kB |
| ingest_instances | 56 kB |
| agg_hour | 56 kB |
| crawler_activity | 32 kB |
| storage_day | 24 kB |
| stats_version | 24 kB |
