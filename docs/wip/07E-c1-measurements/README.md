# 07E C1 measurements — raw data

Produced on the machine in `machine.txt` with the explicit commands of [`../07E-c1-report.md`](../07E-c1-report.md) §11. Synthetic data only; no database, network or DHT traffic.

| File | Content |
|---|---|
| `main-gomaxprocs2.jsonl` | isolated-process matrix: every dataset × forced blob variant × {decode, arena, ref}, 5 repetitions each, `GOMAXPROCS=2` (one JSON object per run; fields documented by `coldResult` in `internal/web/filetree/coldbuild_test.go`) |
| `gomaxprocs8-subset.jsonl` | same, `GOMAXPROCS=8`, 3 repetitions, prefix/trie+Brotli, 250k/adversarial/longname datasets |
| `concurrent-unbounded.jsonl` | K = 1…32 simultaneous cold builds without a gate, indexes kept alive (rows predate the harness recording `Files` for K>1) |
| `concurrent-admitted.jsonl` | K at/near the number the conservative model admits, 3–5 repetitions |
| `gomemlimit-96MiB.jsonl`, `gomemlimit-160MiB.jsonl` | single-build sensitivity to a soft memory limit |
| `bench-cold.txt`, `bench-query.txt` | `go test -bench` output (`ColdBuild`, `Query`) |
| `blob-meta.json` | per-dataset file counts, path bytes and stored sizes of each forced serializer × compressor blob (skipped pairs are recorded in the dumper log, not here) |
| `analyze.py` | the helper that turned the JSON lines into the report's weight table (usage: `analyze.py FILE weight`) |

The report's tables were generated from these files by scripts that are not committed; `analyze.py` prints a similar (not identical) weight table.
