# Task 09 measurements

Raw results (one JSON object per line) of the qualification run of the statistics subsystem. The criteria were fixed **before** any measurement in [`../09-statistics-admin-plan.md`](../09-statistics-admin-plan.md) ("Acceptance criteria"); the interpretation is in [`../09-statistics-admin-report.md`](../09-statistics-admin-report.md) §3.

**Machine and configuration.** AMD Ryzen 9 7950X3D (8 vCPUs given to the VM, 32 GiB), Go 1.27.4, PostgreSQL 17 in a local Docker container (default settings, trust authentication), the real `magnetico` binary built from this branch, synthetic records, loopback only, no DHT and no BitTorrent traffic. Each load run starts a fresh disposable database and a fresh `ingest` process; the load generator, `ingest` and PostgreSQL share the machine. Baselines: the same binary with `MAGNETICO_INGEST_STATS_DISABLED=true`; for the delivery changes the crawler client built from commit `8f339306` (the commit before the batching change) with the harness-only `-stamp` patch.

**Commands** (`MAGNETICO_STATS_OUT` is this `raw/` directory; `MAGNETICO_STATS_BASELINE_CRAWLERD` the pre-batching client):

```sh
go test -tags loadtest -count=1 -v -run 'TestQualifyOverheadAndSabotage'  ./internal/e2e/cli   # 5 alternating runs of 60 s per condition (+10 s warm-up)
go test -tags loadtest -count=1 -v -run 'TestQualifySteadyStateMemory'    ./internal/e2e/cli   # 5 alternating pairs, 200 records/s, GOMEMLIMIT=256MiB
go test -tags loadtest -count=1 -v -run 'TestQualifyWriteFrequency'       ./internal/e2e/cli   # 180 s windows at 1x and 10x
go test -tags loadtest -count=1 -v -run 'TestQualifyBatching'             ./internal/e2e/cli   # sparse, does-not-fit, sustained 150 s, recovery (3 repetitions)
go test -tags loadtest -count=1 -v -run 'TestQualifyAuthFlood'            ./internal/e2e/cli
go test -tags loadtest -count=1 -v -run 'TestQualifyQueueSamplingAdmissionInflation' ./internal/e2e/cli   # 1,000,000 queued rows, 3 alternating pairs of 90 s
go test -tags loadtest -count=1 -v -run 'TestQualifyStorageGrowth|TestQualifyDashboardLatency|TestQualifySabotageControl' ./internal/e2e/cli
go test -tags loadtest -count=1 -v -run 'TestMeasureCachedVersusUncached|TestMeasureAuthCacheHeap' ./internal/ingest/crawlerauth
go test -tags loadtest -count=1 -v -run 'TestMeasureStatisticsHeap|TestMeasureQueueSamplingCost' ./internal/ingest/stats
go test -run xxx -bench 'BenchmarkRecorder|BenchmarkAuthenticateCacheHit' -benchmem -count=5 ./internal/ingest/stats ./internal/ingest/crawlerauth
```

| File | Content |
|---|---|
| `overhead.jsonl`, `overhead_summary.jsonl` | every load run (throughput, CPU per record, latency percentiles, RSS) and the medians relative to the baseline; runs with `iter` ≥ 100 belong to the sabotage control |
| `sabotage_control_summary.jsonl` | the control that separates the statistics code from the sabotage method |
| `memory.jsonl`, `memory_summary.jsonl`, `memory_heap.jsonl` | steady-state RSS (with `GOMEMLIMIT`) and the in-process heap of the recorder, the writer's carry and the authentication cache; `memory_first_attempt_*` is the earlier run without a memory limit (kept as evidence) |
| `write_frequency.jsonl` | writes per minute per table at 1x and 10x load, connections |
| `batching.jsonl` | requests, records per request, latency, connection reuse, drain speed |
| `auth_flood.jsonl`, `auth_throughput.jsonl` | the invalid-credential flood; cached versus uncached authentication |
| `queue_admission*.jsonl`, `queue-sampling.md` | admission with 1 M queued rows; the sampling cost of Task 6 |
| `storage_growth.jsonl`, `dashboard.jsonl` | relation sizes for 1/5/10 years and the extrapolation check; dashboard latency |

`storage_growth.jsonl` contains the 1/5/10-year rows twice (the churn check was re-run; the figures are identical). The first two recovery rows of `batching.jsonl` (recovery time without the drain split) were superseded and removed.
