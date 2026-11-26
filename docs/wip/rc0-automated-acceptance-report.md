# RC0 automated acceptance — report

**Artifact under test:** `docker.io/skobkin/magnetico-web:0.8.0-rc0` (index digest
`sha256:8f4ee86d92cc5fe42144851d80eb0106729eb9da053186d78c4d14cfc19a2ef7`), built by Woodpecker from tag `0.8.0-rc0`
(commit `47742edf`). **Branch of this report:** `fix/post-merge-quick-fixes`. **Run:** 2026-11-25 23:10 UTC – 2026-11-26
(the 24-hour soak continues after this report; §8 says how to collect it).

Everything ran on the development VM against dedicated, disposable containers, databases, networks and bind mounts under
`~/tmp/magnetico/tmp/rc0` (not the root file system). No production system, no live DHT or BitTorrent traffic, no tag or
image was touched. Every acceptance test exercised the **published image**; locally built helpers are named as such
(`crawlerd` and the web-user generators of the load harness, a seeder, Python/Node clients). Raw evidence:
[`rc0-measurements/`](rc0-measurements/README.md).

## 1. Summary

| Area | Result |
|---|---|
| Published image | runs as documented (non-root `10001`, read-only root, no capabilities, exec-form entrypoint, health checks, graceful `SIGTERM`); 42 of 43 smoke checks pass; Trivy (fresh DB): **0 HIGH/CRITICAL** on amd64 and arm64. **Defect D1: the image carries no version** (`magnetico dev (commit unknown …)`, empty revision label). **D2: the GHCR copy could not be pulled anonymously** (401) |
| Real-data migration (10 % sample: 3,457,046 torrents, 123,460,039 files) | import **4 min 44 s** (12,160 torrents/s) + deferred indexes 48 s, **0 record errors**; `verify-decode` **PASS** (41 min 49 s), `verify-content` **PASS** (43 min 0 s, every row verified); source byte-identical before and after; 17.8 GB legacy → **2.95 GB** |
| Migration resilience | SIGTERM → exit 2, SIGKILL → resume, completion → a target **identical** row-for-row and blob-for-blob to the uninterrupted one; `cleanup` positive case passes and leaves 28 tables unchanged; negative case refused at **G5** with and without `--force-unverified` |
| User migration (synthetic legacy accounts) | dry-run, final run, idempotent re-run, `users verify`: all as documented; unsupported hash counted and gated |
| Functional (published image behind Traefik, TLS) | **50/50** checks pass, including source equality of **415** migrated torrents (438,172 files) through the API (every unusual-name/path category of the audit), both file-browser modes, RSS v1/v2, magnet redirects, authorization; real headless Chrome 159: **25/25** under the production CSP; synthetic SMTP (STARTTLS): **11/11** |
| Search on real names | ordinary shapes ≤ 11 ms warm; **finding F5 confirmed on real data**: two common words that rarely co-occur cost **1.8–2.0 s** (date order) and **4.0–4.3 s** (size order); the generic plan would answer in 7.7 ms. No index added (owner decision) |
| Operations | two ingest instances (election, handover 8 s graceful / 10 s after SIGKILL, statistics aggregation), PostgreSQL outage of 2 min, duplicate submissions, credential rotation/disable/enable, search-timeout chain through the proxy: all as specified. **D3: Traefik logged feed tokens** with the documented-intent setting `redact`; `drop` works |
| Soak (published image, ≥ 24 h) | **running** (`rc0-soak` user unit, scale 5 ≈ 26 h, the full-scale 09A synthetic dataset); 15-minute smoke run of the same workload passed every gate; interim state in §8 |

## 2. Artifact, environment and dataset

| | |
|---|---|
| Image index | `docker.io/skobkin/magnetico-web:0.8.0-rc0@sha256:8f4ee86d92cc5fe42144851d80eb0106729eb9da053186d78c4d14cfc19a2ef7` (OCI index, pushed 2026-11-25 21:38 UTC) |
| Platforms | linux/amd64 `sha256:fa35989cc7f2…`, linux/arm64 `sha256:790dfd930041…`, one attestation manifest each (SBOM by syft 1.51.0 / BuildKit 0.35.1, provenance) |
| `--version` | `magnetico dev (commit unknown, go1.27.4, linux/amd64)` — **D1** |
| Labels | `version=dev`, `revision=""` (others as documented) — **D1** |
| Size | 54.6 MB (amd64, uncompressed), Alpine 3.24.4 base |
| GHCR | `ghcr.io/skobkin/magnetico-web:0.8.0-rc0`: anonymous token request **401** (another of the owner's GHCR packages answers 200) — manifests **not compared**, **D2** |
| Docker Hub tags | `0.8.0-rc0` (above); also `latest` → `sha256:6a0d5f20…` = the legacy PHP **0.7.1** image (see D6) |
| Forgejo release `0.8.0-rc0` | prerelease, `checksums.txt` + linux amd64/arm64 archives (sha256 `c1999b95…`, `a2f5b445…`); body lists **5,223 commits** (484 KB, D4). Archives need authentication (401 anonymously): **not downloaded or executed** |
| Host | AMD Ryzen 9 7950X3D, 8 logical CPUs, 31 GiB RAM (≈ 25 GiB available), Debian 13, Linux 6.12, Docker 29.10.0, Compose v5.8.0; root FS 14 GB (6.7 GB free at the end), `/home` 126 GB (43 GB free) |
| PostgreSQL | `postgres:17@sha256:87c8b42f…` = 17.12, `fsync=on`, `synchronous_commit=on`, `shared_buffers=512MB` (as in 07J/09A), `max_wal_size=8GB`, `maintenance_work_mem=1GB`, `max_connections=200`; primary instance `rc0-pg` (legacy + targets), soak instance `rc0-soak-pg` |
| Proxy | Traefik 3.8.0 `@sha256:00c97349…`, TLS with a throwaway test CA, file provider, `deploy/` |
| Other images | Trivy 0.76.2 (pinned as in `security.yaml`), Mailpit 1.34.0, headless-shell (Chrome 159.0.8154.13) `@sha256:381353d3…`, Node 22 (pinned as in `ci.yaml`), PHP 8.3 (pinned as in the hash fixtures, only to make synthetic legacy hashes) |
| Real-data sample | the owner's `magneticod-sample.sql.gz` (2.41 GB gzip, plain SQL, pg_dump 16.14; SHA-256 `964b05df…`, unchanged at the end): **3,457,046 torrents** (ids 1 – 38,594,156), **123,460,039 files**, original v0.12 layout (`schema_version` 0, unpartitioned `files`), discovered 2019-10-19 … 2026-10-14. Same counts and anchors as the 04D lab sample. Restored with `psql` in **3 min 34 s** into a dedicated database (`ON_ERROR_STOP`, exit 0); 17.8 GB |
| Soak dataset | the frozen 09A seeder at scale 1: 3,499,924 torrents, 9,842,529 files in 19,924 blobs, 1,277 MiB (6 min 50 s, identical to 07J/09A) |

## 3. Published-image qualification

| Check | Result |
|---|---|
| `ci/image-smoke.sh --platform linux/amd64 <index digest>` (`raw/image-smoke-amd64.log`) | 42 ok, **1 FAIL: `org.opencontainers.image.revision` empty** (D1). All runtime checks pass: user `10001:10001`, no default command, exec-form entrypoint, read-only root with all capabilities dropped and `no-new-privileges`, `/data` and `/reports` ownership (a root-owned bind mount is refused), the four schema inits in the container, `ingest crawler add` printing the credential once, web `/healthz` `/readyz`, embedded assets and uPlot, the statistics heartbeat, `SIGTERM` exit 0 for web and ingest. `crawl` was not started (it would open the DHT) |
| `image-smoke.sh --check-version-line "$(… --version)" 0.8.0-rc0 47742edf1ab6` | **FAIL: "the image reports version dev, expected 0.8.0-rc0"** (D1) |
| Trivy 0.76.2, DB downloaded for this run (built 2026-11-25 19:00 UTC), `--severity HIGH,CRITICAL --ignore-unfixed` | **0** on linux/amd64 and linux/arm64 (`raw/trivy-*.log`) |
| Trivy, all severities | 11 fixable MEDIUM/LOW on both platforms, all in Alpine packages the static Go binary does not use: `libcrypto3`/`libssl3` 3.5.9-r0 → r1 (CVE-2026-107496, -95691, -95692 MEDIUM; -88822, -88823 LOW), `zlib` 1.3.2-r1 → 1.4-r0 (CVE-2026-4738 MEDIUM) — D5 |
| Documented configuration in Compose (`deploy/compose.yaml`) | web and ingest with `read_only`, `cap_drop: [ALL]`, `no-new-privileges`, 1 GiB without swap, `GOMEMLIMIT` 768/256 MiB, 2/1 CPUs, the documented health checks (`wget /readyz`, `nc -z`), stop grace 45/60 s: healthy within seconds; unknown `MAGNETICO_*` variables are refused (seen once, with the list of known ones) |
| Initialisation | `schema-init`, `web-schema-init` (12 trackers seeded), `ingest-schema-init`, `stats-schema-init` through the image: idempotent, no credential in any output |
| Startup guards | `MAGNETICO_WEB_PUBLIC_URL=http://…` with a non-loopback listener is refused ("development mode … requires a loopback listen address") |
| Graceful shutdown | web `docker stop`: exit 0, "stopped cleanly", file-index cache statistics; ingest: exit 0 in 0.2 s with the final statistics flush; migrate/verify containers exit 2 on SIGTERM and resume |
| arm64 | built and scanned by the pipeline and here; **not executed** (no arm64 host) |

## 4. Real-data migration rehearsal

### 4.1 Source audit (read-only role, aggregates only: `raw/legacy-audit.txt`, `tools/legacy-audit.sql`)

Prediction from the production validation (`persistence/validate.go`, codec limits): **no record should fail** — no bad
info-hash length, no empty file list, no negative size, no size-sum mismatch, no NULL-owned or orphaned file, nothing above
250,000 files, 64 KiB per path, 64 MiB of paths or depth 256 (max 123,480 files, max path 1,109 bytes, depth 29). Valid
but unusual shapes that must be kept verbatim: 320 empty names, 127 names with control characters, 568 with bidi controls,
2,344 with outer white space, 66 with U+FFFD; 193,424 empty paths, 1,080 paths with control characters, 102 with `.`/`..`
components, 11,226 with empty components, 51 with backslashes, 8,860 with U+FFFD; 4,926 torrents with duplicate paths
(378,015 duplicate rows); 199,292 single-file torrents whose path differs from the name (they keep a blob); 208,441
zero-size files. 1,209,452 torrents are blob-less single files. Discovery years: 2019 1.38 M, then 0.13–0.40 M a year
(2024–2026 have near-identical counts — a property of how the 10 % sample was drawn, not an anomaly).

### 4.2 Procedure and results (published image, documented commands, `--defer-secondary-indexes`)

Roles: source `legacy_ro` (`SELECT` only, `default_transaction_read_only`); target owner `rc0_admin`; DSNs only in the
environment. `migrate schema-init`, `web-schema-init`, then `migrate torrents --defer-secondary-indexes` (defaults: 4
workers, batch 256).

| Measure | RC0 (this run) | 04D lab (same sample) |
|---|---|---|
| Import (scan + encode + store) | **4 min 44 s**, 12,160 torrents/s average (12,250–12,700 steady) | 10–15 min, 5,000–5,800 rows/s |
| Deferred indexes (`total_size`, `discovered_on`, trigram GIN) | 48 s (GIN 37 s) | — |
| Total wall time, exit status | 5 min 41 s container wall, **exit 0**, `import_complete`, 3,457,046 scanned = migrated, **0 record errors, 0 integrity incidents**, 2,247,594 blobs | |
| Migrator memory / CPU | peak RSS **820 MiB** (cgroup peak 985 MiB, no limit set), 946 CPU-s (≈ 2.9 cores); no memory event | ~906 MiB peak RSS |
| PostgreSQL CPU during the import | 157 CPU-s | — |
| `verify-decode` | **PASS** in **41 min 49 s**; 2,247,594 blobs verified, 0 mismatched, 0 unverifiable, 1,209,452 assumed single-file (= the audit's blob-less count); RSS 46 MiB; PostgreSQL ≈ 1 core | ~57 min, 46–72 MiB |
| `verify-content` | **PASS** in **43 min 0 s**; 3,457,046 source and target rows, **all verified**, 0 issues; RSS 52 MiB | ~66 min |
| Database size | legacy **17.84 GB** (`files` 16.6 GB with indexes; 6.82 GB of path text) → target **2.95 GB** (torrents 444 MB + indexes 792 MB; blobs 1,518 MB + 48 MB) | |
| Compression | file lists: 1.425 GB of blob payload for 122.25 M files = **5.5×** against the raw content (path bytes + 8-byte sizes, 7.81 GB) and **10.6×** against the legacy `files` table with its indexes; whole database **6.05×** | |

The verifications overlapped other work for part of their run (the soak seeding, its smoke run and start, the first
functional check; `raw/activity-log.txt`), so their times are upper bounds for this host. Both are PostgreSQL-bound.

**Source unchanged** (`tools/legacy-fingerprint.sql`, run before and after everything): row counts, order-independent row
digests of both tables, sequences and the catalogue digest are identical; `pg_stat_user_tables` shows no insert, update or
delete beyond the restore; the dump's SHA-256 is unchanged.

### 4.3 Interruption and resumption (separate disposable target, `tools/interrupt-resume.sh`, `raw/interrupt-resume/`)

| Step | Result |
|---|---|
| start, `docker stop` after 100 s | exit **2**, "interrupted before completion; the settled prefix is committed", 1,088,364 torrents stored, no secondary index |
| resume, `docker kill` (SIGKILL) after 80 s | exit 137, 1,966,985 torrents |
| resume to completion | exit **0**, `import_complete`, 3,457,046 / 2,247,594 blobs, three indexes |
| equality with the uninterrupted primary | identical row digest over all torrents and identical blob digest (`md5(data)`) over all blobs: the encoding is deterministic across interruptions (`raw/resume-vs-primary.txt`) |

### 4.4 Cleanup (two copies of the verified primary; the primary keeps `magnetico_migration`)

* **Positive (copy A, `raw/cleanup-a.log`):** `errors export --manifest` (5 events, complete coverage) → preview: G1–G7 pass,
  G8/G9 block without the attestations → `cleanup --execute --confirm-backup --confirm-export --evidence-manifest …`
  drops the schema in one transaction → `cleanup` and `cleanup --execute` again: "nothing to clean up" (exit 0). Row
  counts and digests of all 28 tables of `magneticod`, `magnetico_web`, `magnetico_ingest`, `magnetico_stats`: identical
  before and after.
* **Negative (copy B, `raw/cleanup-b.log`):** one torrent stored through a real ingest container (id 38,594,157 = max+1:
  identity reseeded correctly) → `cleanup --execute …` and `… --force-unverified --reason …` both **refused at G5**
  ("target torrents 3457047 != success_count 3457046; target blobs 2247595 != blob_count 2247594"), exit 1, copy unchanged.

### 4.5 User migration (synthetic legacy accounts; real accounts are the owner's rehearsal)

A synthetic legacy PHP database (`tools/synthetic-legacy-users.py`: the five Doctrine migrations verbatim, 28 users, 10
invitations (5 used), 92 legacy API keys; bcrypt-13, argon2id, bcrypt-10 hashes of `test` made by PHP 8.3; one MD5-crypt
hash; a non-ASCII username; one user with 55 keys). Results (`raw/users-*.log`): dry-run refuses with
`needs-reset-unacknowledged` (1, id 26) and warns `roles-not-plain-user` (id 1) and `tokens-over-cap` (id 28); with
`--accept-needs-reset 1`: dry-run ok → final import 28/10/92 → re-run `already-migrated` (nothing written) → `migrate users
verify` exit 0. `web user list` shows `legacy` passwords and `unverified` emails; the migrated hashes sign in (bcrypt and
argon2id) and the unsupported one does not. `web user set-password` refuses `test` ("password is too short"): validation
was not weakened — the `test`/`test` accounts exist only because legacy hashes are imported verbatim (documented
behaviour), and `test` was promoted with `web user set-role`.

## 5. Functional acceptance (published image, Traefik with TLS)

Deployment `rc0` (`deploy/compose.yaml`): `web`, `ingest` (+ `ingest2`), Traefik with only `magnet.rc0.test` and
`ingest.rc0.test` routed, Mailpit; loopback-only ports (18443 TLS). Clients: `tools/functional-check.py` (HTTPS through the
proxy, legacy source through the read-only role), `tools/browser/driver-rc0.js` (headless Chrome over CDP), `tools/smtp-check.py`.

**Functional check (`raw/functional-3.txt`): 50 pass, 0 fail.** Covered: health and readiness through the proxy;
anonymous redirects and 401; the production CSP and HSTS (`max-age=31536000`); an unrouted `Host` is 404 at the proxy;
sign-in of migrated accounts with legacy hashes; `__Host-`, `Secure`, `HttpOnly` session cookie; every `/admin` page
`no-store`, no password hash on the users page; ordinary user 403 on `/admin` and its JSON; wrong password and unsupported
hash refused; API and RSS tokens minted on the account page; API v2 search with keyset pagination for four (q, sort,
order) shapes over four pages each (ordered — name order judged by PostgreSQL's collation —, no duplicates); invalid cursor
400; API token refused by the feed; v2 RSS (1,000 items, `no-store`, `no-referrer`); legacy v1 RSS with a migrated key in
query and header; the legacy key refused by API v2; API documentation behind sign-in with its documented CSP;
**content equality with the legacy source for 415 torrents and 438,172 files** (300 random + 10 of every unusual-shape category of §4.1 +
the three largest + the id edges): name, size, info hash, discovery time, file count, the complete file list with
duplicates (409 in tree mode, 2 by a complete directory-by-directory walk; the 4 torrents above 20,000 files by their
root directory summary, because the API allows 2 requests/s per token — their per-file equality is `verify-content`'s), info-hash lookup and the magnet redirect; tree mode and a full
directory-by-directory walk give the same list for five medium torrents; the torrent pages and the UI search render.

**Real browser (`raw/browser-run-2.txt`): 25/25.** Chrome 159 over HTTPS through Traefik, the certificate trusted by SPKI
pin only: sign-in, search and keyset next page, empty state, tree mode (toggle changes `aria-expanded`, the local filter
narrows), directory mode (breadcrumbs, paging), Swagger UI (5 operations) under its documented `docsCSP`, the admin
overview (five chart groups drawn from live statistics), dark theme, 360 px without horizontal scroll on search, both
torrent modes, account, admin and API docs; **no CSP violation, no console error, no request to another origin**.
Screenshots were kept outside the repository (they show real names).

**Synthetic SMTP (`raw/smtp-check.txt`): 11/11.** STARTTLS to Mailpit with the test CA in `SSL_CERT_FILE`: verification
email delivered, the link confirms a migrated unverified address, recovery email delivered and its page opens (not
completed), an unknown address gets exactly the same answer. With Mailpit down the service logged only `mail delivery
failed category=dial`. Real providers stay with the owner.

**Proxy:** a client's `X-Forwarded-For: 203.0.113.77` is not believed (Traefik replaces it; the service logs the real
peer); a search held 80 s by a table lock is ended **by the application** at 59.5 s with `503` and `Retry-After: 30`
through Traefik (`responseHeaderTimeout` 100 s, `writeTimeout` 120 s); header logging off (`headers.defaultMode: drop`)
works. **D3:** with `accessLog.fields.names.RequestPath: redact` Traefik 3.8.0 logged full paths including
`feed_token=…` (1) and `api-token=…` (2) — `redact` is not a valid mode for field names and is ignored without a warning;
`RequestPath: drop` removes the path (`raw/traefik-redact-evidence.txt`; the synthetic tokens were scrubbed from the kept log).

## 6. Search performance on real names (F5)

`tools/explain-real.sh` (warm; the 07J shapes with real format words; plans only, `raw/explain-real/`) and end-to-end
through the service and the proxy (`tools/search-latency.py`, `raw/search-latency-e2e.md`):

| Shape (51 rows, warm) | EXPLAIN | end to end (median of 3) |
|---|---|---|
| newest / oldest / largest, deep keyset pages, info-hash and anchor lookups | 0.04–0.5 ms | 2 ms |
| one common word (`1080p`, 420 k matches) by date / by size | 5.7 / 3.3 ms | 4 / 3 ms |
| two words that co-occur (`1080p x264`, 70 k) | 8–11 ms | 7 ms |
| a selective word, a mid word, Cyrillic word, no hit | 1–16 ms | 2–6 ms |
| one common word **sorted by name** (sorts all 420 k matches) | 228–240 ms | 215 ms |
| **rare pair of common words** `mp4`+`flac` (484 k / 48 k, **20** together), `pdf`+`1080p` (31), `epub`+`720p` (31) by date | **1.75–2.5 s** (walks `idx_torrents_discovered_on` backwards over all 3.46 M rows, 101 k buffers) | **1.8 s** (`pdf 1080p`, `epub 720p`) |
| `mp4`+`flac` **by size** | **4.3–5.6 s** (walks the size index, 3.47 M heap visits) | **4.0 s** |
| `mp3`+`x265` (4 together) | 5–10 ms (GIN bitmap) | 7 ms |

The planner multiplies the two selectivities as if independent (estimate 13,747 rows, actual 20) and prefers the ordered
index walk. **The generic plan of the same prepared statement is the GIN bitmap: 7.7 ms** for `mp4`+`flac` (forced with
`plan_cache_mode = force_generic_plan`); in `auto` mode the statement kept custom plans after six executions in the script.
Through the service, `mp4 flac` by date took 9 ms while `pdf 1080p` took 1.8 s; that is consistent with pgx's cached statement
on some pool connections having switched to the generic plan after five executions (inference, not proven). Comparison with
07J (synthetic, its co-occurring pair): 07J measured the same plan shape; real data confirms it with real word correlation.
All stay well below the 60 s search deadline, but four concurrent rare-pair searches occupy all four search slots for
seconds. **No index was added**; options for the owner are in §9 (F5).

## 7. Operational acceptance (primary deployment)

| Exercise | Result (`raw/`) |
|---|---|
| Synthetic crawlers through Traefik (TLS, `crawlerd` in containers) | crawler-a 3 rec/s + 20 existence hashes/s, crawler-b 2 rec/s, 2 h; delivered |
| Duplicate submissions (crawler-b re-sending 300 of crawler-a's records) | accepted (202), settled as **`stored_existing`** (110 in the first flush), no new row; `admission_duplicates` counts only hashes already queued (as specified) |
| Two ingest instances behind one Traefik service | both heartbeat; one holds the consumer advisory lock (`7881644658832750187`); the overview lists both, `owns_consumer` true/false, rates summed over `instances_included: 2`, queue and storage shown once |
| Consumer handover | graceful stop of the owner: exit 0 in 0.2 s, the other instance takes the lock **8.1 s** later; SIGKILL of the new owner: the restarted first instance takes it **10.0 s** later; queue empty afterwards; dead instances become `stale` and are excluded from the rates within a heartbeat (`owns_consumer` of the live one catches up on its next heartbeat) |
| PostgreSQL outage 2 min (`docker stop rc0-pg`) | `/readyz` 503, `/healthz` 200; web, ingest ×2 stayed up (0 restarts); ingest's statistics buffered and retried; crawlers queued locally; after the restart web ready in **1 s**, crawler backlog drained in **39 s**; no DSN or password in any log |
| Credential rotation / disable / enable (two instances, through the proxy) | new token works at once, old refused after **6.4 s**; disabled refused after **56.5 s** (limit 60 s); re-enabled accepted at once; the crawler logs `ingest answered HTTP 401 … records stay queued` |
| Statistics failure, service restarts, queue recovery at production-like rates | covered by the soak (§8: a 50-minute lock of the statistics tables at scale 5, web cold restarts, graceful ingest restart) and its smoke run |
| Resource use (primary, real data) | durable 30 s cgroup samples in `meas/primary-cgroup.jsonl` (unit `rc0-primary-sampler`); no memory event observed so far |

## 8. Soak (published image, ≥ 24 h)

**What runs.** Profile `soak09a` of the existing load harness (`internal/e2e/cli/soak09a_test.go`, unchanged), in the new
container mode (`internal/e2e/cli/container_harness_test.go`, commit `d1f380e0`): web and ingest are containers of the
published index digest with web 1 GiB / `GOMEMLIMIT=768MiB` / 2 CPUs and ingest 1 GiB / `256MiB` / 1 CPU, read-only, no
capabilities, `GODEBUG=gctrace=1`; operator commands run the same image (`tools/magnetico-image-cli`). Scale **5**: the
310-minute 09A timeline stretched to ≈ 26 h (290 × 5 min of load, drain, 100 min idle): five crawlers (6 rec/s each, 30 rec/s
bursts of 10 min every 2.5 h, 60 existence hashes/s), 40 interactive web users, an administrator session every 3 s, web cold
restarts (100 and 900 min), rotation (300), disable/enable (450), graceful ingest restart (600), a 50-minute statistics lock
(750), a closed loop of 16 workers (1,200–1,350), then drain and idle; per-minute monitors (PostgreSQL connections, locks,
oldest transaction, queue, dead tuples, cgroup events), per-10-s samples (RSS, cgroup memory, CPU, threads, descriptors read
inside the container), gctrace heap-after-GC, conservation and roll-up checks at the end. Dataset: the full-scale 09A
synthetic set (3.5 M torrents) in its own PostgreSQL.

**Deviation, stated:** the harness drives the web service with a loopback `http://` public URL (as in every earlier harness
run); the published image correctly refuses that on a non-loopback listener, so the soak containers use the **host network
with loopback listeners**. The production-like configuration (HTTPS public URL, bridge networks, Traefik) is what §§5–7
exercised. Diagnostic (`diag`) builds were not needed so far (no published-image measurement is mixed with a diagnostic one).

**Smoke run (scale 0.05, 15.6 min, `raw/soak-smoke/`): PASS**, every gate: conservation exact (36,874 received = acknowledged
= enqueued = settled = created = stored), web and ingest exit 0, no memory event, credential events within limits.

**Interim state (2026-11-26 ≈ 01:45 UTC, 1 h 45 min of 26 h):** 0 gate failures; web cold restart 1 exit 0 (0.44 s down); no
cgroup memory event; 32 PostgreSQL connections; queue 0. Once the verifications and the functional work stopped competing for the
CPUs, interactive search is p50 ≈ 11 ms, p95 ≈ 100 ms, p99 ≈ 600 ms per two-minute round (09A: 10.8 / 146 / 582 ms). Rounds
before ≈ 01:15 UTC overlapped heavy work on the same CPUs (`raw/activity-log.txt`) and are not representative of latency.

**Status, stop, collection** (paths relative to the workspace `~/tmp/magnetico/tmp/rc0`; the script lives in the soak worktree):

```sh
T=~/tmp/magnetico/tmp/rc0/soak-src/docs/wip/rc0-measurements/tools/rc0-soak.sh
$T status                 # unit state, records, newest monitor line and events, gate failures so far, containers
$T stop                   # SIGTERM to the harness: it stops its containers; the JSONL so far stays
$T collect                # soak-summary.md from soak/out/rc0soak.jsonl (09A's summariser)
journalctl --user -u rc0-soak; less ~/tmp/magnetico/tmp/rc0/soak/harness.log   # PASS/FAIL line at the end
```

Expected end ≈ 2026-11-27 02:00 UTC. The verdict (memory plateau, F9 heap-after-GC trend, conservation, every gate) belongs in
an addendum to this report; F9 is investigated with a `diag` build only if a sustained trend appears.

## 9. Findings and defects (most severe first)

| # | Severity | Finding | Evidence | Proposed fix |
|---|---|---|---|---|
| D1 | **High** (release engineering) | The published image is unversioned: `--version` prints `magnetico dev (commit unknown …)`, labels `version=dev`, `revision=""`. `release.yaml` passes `build_args` as a YAML **list**; `plugin-docker-buildx` v6 declares `build_args` as a string **map** (`StringMapFlag`, its docs show the map form), so nothing reached `docker build` and the Dockerfile defaults applied. The image smoke test catches the empty label, but it is not part of the release workflow | `raw/image-smoke-amd64.log`; the version line; plugin source `plugin/plugin.go` (v6.1.2) | Use the map form (`build_args: {VERSION: ${CI_COMMIT_TAG}, COMMIT: ${CI_COMMIT_SHA}}`), pin it in `internal/repocheck`, and publish a corrected RC (`0.8.0-rc1`); check `rebuild.yaml`'s `build_args_from_env` path the same way before relying on it |
| D2 | **High** (distribution, unverified) | `ghcr.io/skobkin/magnetico-web` refuses anonymous pulls (token endpoint 401) | §2 | Check that the push succeeded and make the package public / link it to the mirror; then compare the digest with Docker Hub |
| D3 | **Medium** (deployment documentation, credential exposure) | Traefik's `accessLog.fields.names.<field>: redact` is silently ignored (keep); `RequestPath` then carries RSS tokens and one-time link tokens | §5, `raw/traefik-redact-evidence.txt` | Give the exact Traefik setting in `web-deployment.md` §9 and `crawler.md`: `accessLog.fields.headers.defaultMode: drop` and `accessLog.fields.names.RequestPath: drop` (Traefik cannot mask a query or a path segment selectively); warn that `redact` does not apply to field names |
| F5 | **Medium** (performance, owner decision) | Two common but rarely co-occurring words: 1.8–2.5 s (date order) and 4–5.6 s (size order) on real names, whereas a GIN bitmap answers in < 10 ms. Within the search deadline; can saturate the four search slots | §6 | Options (none applied): (a) let PostgreSQL see the term counts — e.g. a bounded `MATERIALIZED` candidate CTE from the trigram index with an ordered fallback when it overflows; (b) `plan_cache_mode`/generic-plan control per statement; (c) an index decision with the owner; (d) accept and document. A real-data EXPLAIN set for re-measuring is in `raw/explain-real/` |
| D4 | Low (release notes) | The Forgejo release body lists 5,223 commits (484 KB): GoReleaser had no previous Go-line tag to compare with | §2 | `changelog` filters or a fixed range for the first Go-line release; check again with rc1 |
| D5 | Low (image hygiene) | 11 fixable MEDIUM/LOW Alpine CVEs (OpenSSL 3.5.9-r0, zlib 1.3.2) in packages the Go binary does not use; `security.yaml` only fails on HIGH/CRITICAL | `raw/trivy-*` | Rebuild on the refreshed Alpine base with the next RC; optionally trim unused packages from the final stage |
| D6 | Info (distribution) | Docker Hub `latest` still points to the legacy PHP 0.7.1 image | §2 | Owner decision: leave, remove, or document that `latest` is not the Go line |
| D7 | Info | Forgejo release assets require authentication (401 anonymously); the native archives were therefore not verified here | §2 | If anonymous download is intended, check the repository visibility; otherwise verify the archives with credentials |
| O1 | Info (observability) | After a consumer handover the overview's `owns_consumer` reflects the last heartbeat (seconds late); dead instances are marked stale within one heartbeat | §7 | Documented behaviour; no change proposed |

No product defect was found in migration, verification, cleanup, user migration, the web service, the API, the feeds,
ingest, statistics or the image's runtime behaviour. Defects of the acceptance tooling were fixed in the tooling (they are
not product findings): Python 3.13's strict X.509 check needed a CA with key usage; the name-order check now asks
PostgreSQL's collation; the browser checks use the docs page's own policy and the real markup; the generic-plan part of the
EXPLAIN script no longer prints result rows (an earlier local run printed real names into a workspace file; that section was
deleted before anything was copied into the repository, and the copied files were scanned against every real name of the
sample and every secret: no hit).

**Environment incident (not a product finding):** the owner's disposable test PostgreSQL container `m-pg`
(`docs/testing.md`, started with `--rm` and an anonymous volume) was running at the start of this task and is gone now,
together with its volume. No command of this task names it or uses a pattern that matches it (every stop/remove targets an
exact `rc0*`/`smoke<pid>` name; one broad `docker stop` by image ancestry was refused by the permission system and never ran),
and the Docker event buffer no longer holds the event, so the cause could not be established. Recreate it with the command in
`docs/testing.md` if needed.

## 10. RC0 acceptance checklist — status after this run

✅ passed · ❌ failed · ⏳ ongoing · ⬜ untested here (with the reason)

| Checklist item (`rc0-acceptance-checklist.md`) | Status |
|---|---|
| Migration: `migrate cleanup` exercise on a disposable database | ✅ §4.4 |
| Migration: negative case after a new torrent | ✅ §4.4 (G5, both forms) |
| Migration: production policy "keep `magnetico_migration`" | ✅ documented; the primary keeps it |
| Browser: sign-in/out, remember-me, every account tab and email states, with and without SMTP, themes | partly ✅ (sign-in, account tabs at 360 px, dark theme, email verify/recovery with SMTP; §5) · ⬜ remember-me, every email state, "Remove email" panel by hand |
| Browser: search, sort, page size, keyset pagination, empty states, shortcut | ✅ search, keyset next page, empty state · ⬜ page size control, keyboard shortcut |
| Browser: both file-browser modes | ✅ tree (toggle, filter) and directories (breadcrumbs, paging) on real torrents · ⬜ error/retry states, every unusual name by eye |
| Browser: Swagger UI under the real CSP, Authorize, Try it out | ✅ renders, no violation · ⬜ Authorize/Try-it-out by hand |
| Browser: no console errors, no other host | ✅ |
| Browser: keyboard-only, screen reader, contrast | ⬜ needs a person |
| Browser: 360 px and a real phone | ✅ 360 px emulated · ⬜ real phone |
| Proxy: timeouts > 70 s, 60 s search cut by the application | ✅ §5 |
| Proxy: only the configured host is routed | ✅ (404 for another `Host`) |
| Proxy: `X-Forwarded-For` appended, narrow trust | ✅ |
| Proxy: access log keeps no credentials | ❌ with `redact` (D3) · ✅ with `drop` |
| Proxy: no response caching; TLS, HSTS, redirects | ✅ `no-store` on feeds/admin, HSTS, TLS 1.2+/1.3 via Traefik; HTTP→HTTPS redirect configured · ⬜ real certificates |
| Operations: container memory with the real image | ⏳ soak (limits 1 GiB, no event so far); ✅ migration and verification footprints |
| Operations: real SMTP provider | ⬜ owner (synthetic STARTTLS ✅) |
| Operations: search plans on a production-like copy, F5 decision | ✅ measured (§6) · ⏳ owner decision |
| Operations: live-DHT acceptance | ⬜ separate host, not from this VM |
| Operations: user migration dry-run on real accounts | ⬜ owner (synthetic ✅ §4.5) |
| Operations: first admin via `set-role`, email removal by a migrated user | ✅ set-role · ⬜ owner |
| Operations: ≥ 24 h soak with the real image | ⏳ running (§8) |
| Operations: toolchain Go ≥ 1.27.4 | ✅ the image reports `go1.27.4` |
| Statistics: uPlot under the strict CSP in more browsers | ✅ Chrome 159 again · ⬜ Firefox, phone |
| Statistics: keyboard, screen reader, badge contrast | ⬜ |
| Statistics: access with real accounts (admin link, 403, demotion) | ✅ 403 and admin access with migrated accounts · ⬜ demotion on the next request |
| Statistics: ≥ 24 h with real crawlers | ⬜ live DHT · ⏳ synthetic in the soak |
| Statistics: behind the production proxy (client addresses, Authorization not logged, rotation within 60 s) | ✅ §§5, 7 |
| Statistics: queue sampling at production scale with a cold cache | ⬜ not reproduced (queue stayed small) |
| Statistics: two ingest instances | ✅ §7 |
| Statistics: cleartext HTTP/2 evaluation over a real network | ⬜ |
| Release: operator configuration, PR pipeline, first RC tag | ✅ tag built and pushed to Docker Hub · ❌ D1 (unversioned), D2 (GHCR not verifiable) · ⬜ PR pipeline (not observed) |
| Release: image smoke test and Trivy | ✅ (D1 caught) |
| Release: arm64 executed on arm64 | ⬜ no arm64 host |
| Release: `latest`, cache, licence bundling, legacy warning removal, branch cleanup | ⬜ owner decisions (D6) |

## 11. What the owner still has to do by hand

1. **Decide D1/D2** and publish a corrected candidate; re-run `ci/image-smoke.sh --platform linux/amd64 <digest>` and
   `image-smoke.sh --check-version-line "$(docker run --rm IMAGE --version)" 0.8.0-rc1 <commit>`; compare both registries'
   digests (`docker buildx imagetools inspect`).
2. **Real accounts:** run `docs/wip/07H-audit.sql` and `migrate users --dry-run` against a copy of the real PHP database
   (§4.6 of the migration guide); then sign in as a migrated user.
3. **Browse the RC0 deployment** (still running, private): add `127.0.0.1 magnet.rc0.test ingest.rc0.test` to the client's
   hosts file, trust `~/tmp/magnetico/tmp/rc0/deploy/traefik/certs/ca.crt` (test CA, 30 days) or accept the warning, open
   `https://magnet.rc0.test:18443/` and sign in as **`test` / `test` (administrator)** or `rc0user02` … `rc0user25`,
   `Пользователь`, `many-tokens` (password `test`; `unsupported-hash` cannot sign in by design). Mail sent by the service is
   at `http://127.0.0.1:18025` (Mailpit). Check by hand: keyboard-only, screen reader, contrast, remember-me, the email states
   and removal, Swagger *Authorize* / *Try it out*, Firefox, a real phone.
4. **Decide F5** (§6, §9) with the real-data plans in `raw/explain-real/`.
5. **Fix the proxy documentation (D3)** before production; use `RequestPath: drop`.
6. **Real SMTP provider**, **live-DHT acceptance** on a separate host, **arm64 execution**, and the remaining release decisions (D4–D7).
7. **Collect the soak** (§8) and judge it with the 07J/09A criteria; append the verdict.

## 12. Resources left running and cleanup

Kept on purpose: `rc0-pg` (legacy sample `legacy_torrents`, read-only role; **primary migrated database `rc0_magnetico`
with `magnetico_migration` preserved**, the synthetic accounts and the synthetic crawler torrents stored after the
verifications; synthetic legacy accounts `legacy_app`), the deployment `rc0` (`rc0-web`, `rc0-ingest`, `rc0-ingest2`,
`rc0-traefik`, `rc0-mailpit`), the soak (`rc0-soak`, `rc0-soak-pg`, containers `rc0soak-*`), the sampler unit
`rc0-primary-sampler`, networks `rc0-net`, `rc0-edge`, `rc0soak-net`. Data under `~/tmp/magnetico/tmp/rc0`; secrets (database
passwords, crawler tokens, synthetic legacy keys and hashes) in its `secrets/` (mode 700/600), never in the repository.

Removed: the cleanup copies `rc0_cleanup_a`/`_b`, the interruption target `rc0_resume`, the headless Chrome container, the
smoke-test resources (by the script). Full teardown when no longer needed:

```sh
systemctl --user stop rc0-soak rc0-primary-sampler
cd ~/tmp/magnetico/tmp/rc0/deploy && RC0_SECRETS=.. RC0_DEPLOY=. docker compose --profile two-ingest down
docker rm -f $(docker ps -aq --filter name=^rc0) rc0-pg rc0-soak-pg 2>/dev/null
docker network rm rc0-net rc0-edge rc0soak-net
git -C ~/src/magnetico-web worktree remove ~/tmp/magnetico/tmp/rc0/soak-src
rm -rf ~/tmp/magnetico/tmp/rc0        # the original dump in ~/tmp/magnetico is not touched
```
