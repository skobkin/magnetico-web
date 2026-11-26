# RC0 automated acceptance — measurements and tooling

Evidence for [`../rc0-automated-acceptance-report.md`](../rc0-automated-acceptance-report.md). Everything was produced on
the development VM against disposable containers and databases with the published image
`docker.io/skobkin/magnetico-web:0.8.0-rc0@sha256:8f4ee86d…`. **No file contains a credential, a token, a password hash, a
DSN with a password or a real torrent name**: the directory was scanned against every secret value of the run and every
torrent name (≥ 16 bytes) of the sample before it was committed. Torrent ids and generic format words (`1080p`, `flac`, …)
appear; they are not private. Screenshots of real data were kept out of the repository.

## Tooling (`tools/`, `deploy/`)

| File | What it does |
|---|---|
| `tools/legacy-fingerprint.sql` | order-independent digest of the legacy source (run before/after: the source is unchanged) |
| `tools/legacy-audit.sql` | read-only anomaly audit of a legacy `magneticod` database (aggregates only; predicts migrator errors) |
| `tools/schema-checksums.sql` | row counts and digests of every table cleanup must not touch |
| `tools/sample-containers.sh` | cgroup v2 sampler for containers (CPU, memory, events, pids, RSS) |
| `tools/interrupt-resume.sh` | SIGTERM / SIGKILL / resume exercise of `migrate torrents` with the image |
| `tools/synthetic-legacy-users.py` | a synthetic legacy PHP account database (schema fixture of 07H + fake accounts) |
| `tools/functional-check.py` | functional acceptance through the TLS proxy, incl. source equality of migrated torrents through API v2 |
| `tools/search-latency.py`, `tools/explain-real.sh` | end-to-end search latency and EXPLAIN of the service's query shapes on real names |
| `tools/smtp-check.py` | email verification and recovery against a synthetic SMTP server |
| `tools/browser/driver-rc0.js` | headless Chrome (CDP) checks of the web UI under the production CSP |
| `tools/magnetico-image-cli` | the operator CLI through the image (`MAGNETICO_BENCH_BIN` of the harness's container mode) |
| `tools/traefik-log-check.sh` | which probe credentials reach Traefik's access log and own log, per access-log configuration (follow-up, D3) |
| `tools/rc0-soak.sh` | start / status / stop / collect of the published-image soak (profile `soak09a`, container mode) |
| `deploy/compose.yaml`, `traefik.yml`, `dynamic.yml` | the disposable deployment: web, ingest ×2, Traefik with TLS, Mailpit (secrets in env files outside the repository) |

The container mode of the load harness is `internal/e2e/cli/container_harness_test.go` (opt-in, `MAGNETICO_BENCH_IMAGE`).

## Raw results (`raw/`)

| File(s) | Content |
|---|---|
| `image-smoke-amd64.log`, `trivy-*.log`, `trivy-version.log`, `forgejo-checksums.txt` | image qualification |
| `dump-sha256-before.txt`, `legacy-fingerprint-*.txt`, `legacy-tupstats-*.txt`, `legacy-audit.txt` | source integrity and audit |
| `restore.*`, `migrate.*`, `verify-decode.*`, `verify-content.*` (`.log`, `.time`, `.exit`, `.start`, `.end`) | restore, migration and verification runs |
| `migrate-cgroup.jsonl`, `verify-cgroup.jsonl` | 5 s cgroup samples during migration and verification (`rc0-pg` memory includes the page cache) |
| `target-sizes.txt`, `target-*schema-init.log`, `ingest-stats-schema-init.log` | target sizes and initialisation |
| `interrupt-resume/steps.txt`, `resume-vs-primary.txt` | interruption/resumption and equality with the uninterrupted target |
| `cleanup-a.log`, `cleanup-b.log`, `cleanup-*-checksums-*.txt` | cleanup positive and negative cases |
| `users-*.log`, `admin-promote.log`, `short-password-refused.log` | user migration with synthetic accounts |
| `functional-3.txt`, `browser-run-2.txt`, `smtp-check.txt`, `smtp-unreachable.txt` | functional, browser and SMTP checks (final runs) |
| `xff-spoof.txt`, `search-timeout-through-proxy.txt`, `traefik-redact-evidence.txt` | proxy checks (D3) |
| `term-cooccurrence.txt`, `explain-real/`, `search-latency-e2e.md` | real-data search plans and latency (F5) |
| `duplicate-submission.txt`, `consumer-handover.txt`, `overview-instance-excluded.txt`, `credential-events-primary.txt` | operational exercises |
| `soak-seed.log`, `soak-smoke/` | the soak dataset and the 15-minute smoke run of the soak workload (JSONL and summary) |
| `functional-4.txt` | the functional re-run of the follow-up, with file-by-file and root-summary-only counts reported separately |
| `d1-plugin-verification.txt` | the buildx plugin run with Woodpecker's encoding: mapping, rebuild path and the rc0 list form (follow-up) |
| `d2-ghcr-retry.txt` | anonymous GHCR access and the manifest comparison with Docker Hub after the visibility change |
| `traefik-log-check/` | summary (with a validity column), field lists, CLF lines and probe hits of `tools/traefik-log-check.sh` (probe values are fake; `Set-Cookie` values redacted) |
| `alpine-cves-recheck.txt` | Trivy of the rc0 image and a fresh `alpine:3.24`, upgradable packages, who links the affected libraries |
| `activity-log.txt` | when heavy work ran (to separate overlapping measurements) |

The 24-hour soak's own output stays in the workspace (`~/tmp/magnetico/tmp/rc0/soak/out/rc0soak.jsonl`) until it is collected
(report §8).
