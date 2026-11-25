# Task 09A raw measurements

Every file here was produced by the commands named below on the development VM (8 cores, 31 GiB, local PostgreSQL 17 in Docker, no live network of any kind), against **disposable** databases and synthetic data. Nothing was published and no tag exists. Interpretation, caveats and the criteria are in [`../09A-pre-rc-qualification-report.md`](../09A-pre-rc-qualification-report.md).

| File or directory | What it is | Produced by |
|---|---|---|
| `raw/g1-*`, `raw/test-census.txt`, `raw/race-census.txt` | the clean-checkout gates: build, vet, lint, the ordinary and race-enabled suites (`go test -json`, skip census), `govulncheck` | `run-g1.sh` (see the report) |
| `raw/g2-*`, `raw/goreleaser*.log`, `raw/native-*`, `raw/image-*`, `raw/trivy-*`, `raw/attestations.log` | real native archives (pinned GoReleaser image), the OCI image (amd64 + arm64, SBOM and provenance), the smoke tests, the vulnerability scans with freshly downloaded data | `run-g2.sh` |
| `raw/sabotage_rate.jsonl` | the locked-statistics sabotage at a production-like rate, one line per run | `go test -tags loadtest -run TestQualifySabotageAtProductionRate ./internal/e2e/cli` |
| `raw/soak09a*.jsonl`, `raw/soak09a-summary.md` | the combined-service soak: events, web rounds, admin overview samples, monitors, heap checkpoints, per-process reports, the end-state checks | profile `soak09a` of `TestLoad` |
| `raw/stress-*.txt` | the stress runs behind the deterministic-test findings (#52, #53) | see the report, section 6 |

No file contains a credential, a DSN with a password, a token or a payload: crawler tokens exist only in the memory of the test processes.
