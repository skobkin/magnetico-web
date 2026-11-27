# Running Magnetico Web in a container

One universal image holds the single `magnetico` executable and supports every subcommand (`crawl`, `ingest`, `web`,
`migrate`, `dev`). You choose the service by the command you pass.

> **Status.** `0.8.0-rc1` is published on both registries and was used for a real-data migration rehearsal
> ([report](wip/real-data-migration-report-0.8.0-rc1.md)). How releases are built and verified:
> [`release-engineering.md`](release-engineering.md). The examples below use `0.8.0` for the release you run.

| | |
|---|---|
| Docker Hub | [`docker.io/skobkin/magnetico-web`](https://hub.docker.com/r/skobkin/magnetico-web) |
| GitHub Container Registry | [`ghcr.io/skobkin/magnetico-web`](https://github.com/skobkin/magnetico-web/pkgs/container/magnetico-web) |
| Platforms | `linux/amd64`, `linux/arm64` (one multi-platform image; the two registries carry the identical digest) |
| Tags | every release under its exact version (`0.8.0-rc1`, `0.8.0`, …); each published release also moves `latest`; there are no `0.8`, `stable` or `edge` tags |
| Deployment stack | [`docker-stacks/magnetico-web`](https://git.skobk.in/skobkin/docker-stacks/src/branch/master/magnetico-web) still describes the **old PHP** deployment and is **incompatible with 0.8.0**; it will be updated separately. Use the examples on this page |

## What is in the image

| | |
|---|---|
| Base | Alpine (static, `CGO_ENABLED=0` executable at `/usr/local/bin/magnetico`; no compiler, no source tree, no test harness) |
| User | **`10001:10001`** (`magnetico`), no login shell. Fixed and documented so volumes can be provisioned |
| Entrypoint | `["/usr/local/bin/magnetico"]` (exec form: the executable is PID 1 and receives `SIGTERM` directly). **No default command**: started without arguments it prints its usage and exits 1 — it never starts a crawler, initialises a database or migrates anything on its own |
| `/data` | the crawler's persistent queue (`MAGNETICO_CRAWL_QUEUE_DIR=/data` is the image default); owned by `10001:10001` |
| `/reports` | an empty directory owned by `10001:10001`: mount point for migration reports and forensic exports |
| Working directory | `/data` |
| Not declared | `VOLUME` (an anonymous volume would be silently reused across re-creations), `HEALTHCHECK` (see below) |
| Certificates | the system CA bundle. Custom trust roots: mount a PEM bundle and set `SSL_CERT_FILE` (it **replaces** the system roots for the whole process) or `SSL_CERT_DIR`. A derived image can `apk add ca-certificates` for `update-ca-certificates` |
| Time zones | the IANA database is embedded in the executable (`migrate users --source-timezone Europe/Moscow` works without any zoneinfo package) |
| Embedded assets | the web UI templates and static files, the OpenAPI documents and the vendored Swagger UI |
| Licences | `/usr/share/licenses/magnetico/` (AGPL-3.0 `COPYING`, Swagger UI licence and notice) |
| Labels | `org.opencontainers.image.{title,description,source,url,documentation,licenses,version,revision}` |

```sh
docker run --rm docker.io/skobkin/magnetico-web:0.8.0 --version
# magnetico 0.8.0 (commit 0123456789ab, go1.27.4, linux/amd64)   — printed without any configuration, database or network
```

The container runs as a non-root user with a read-only root filesystem, all capabilities dropped and
`no-new-privileges` (`docker run --read-only --cap-drop ALL --security-opt no-new-privileges …`): that is how the smoke
test runs it. Writable paths are only the volumes you mount (`/data` for the crawler; `/reports` for exports).

**Bind mounts must be writable by the container user**: owned by `10001:10001` (`chown 10001:10001 ./reports`), or
the one-shot command runs as your own user (`--user "$(id -u):$(id -g)"`, see below). A root-owned bind mount is refused
with `Permission denied` (the smoke test checks both directions). Named volumes created on first use inherit the
ownership of `/data` and `/reports` from the image.

## Web and ingest behind a TLS-terminating reverse proxy

Both services speak plain HTTP and must sit behind a proxy that terminates TLS (Traefik is the documented default;
timeouts, forwarded headers and what the proxy must not log are in [`web-deployment.md`](web-deployment.md) §2–3 and
[`crawler.md`](crawler.md) §4). Listen on `0.0.0.0` **inside the container network only** and publish no host port.

Initialise the schemas once, with the same image (non-destructive and idempotent; they refuse a legacy or incompatible
schema). `migrate.env` holds `MAGNETICO_MIGRATE_DATABASE=postgres://…` (see "Passing credentials" below). When you
migrate from the PHP version, follow [`migration_0.8.0.md`](migration_0.8.0.md) instead: it runs these at specific
points of the procedure.

```sh
docker run --rm --network db --env-file ./migrate.env docker.io/skobkin/magnetico-web:0.8.0 migrate schema-init
docker run --rm --network db --env-file ./migrate.env docker.io/skobkin/magnetico-web:0.8.0 migrate web-schema-init
docker run --rm --network db --env-file ./migrate.env docker.io/skobkin/magnetico-web:0.8.0 migrate ingest-schema-init
docker run --rm --network db --env-file ./migrate.env docker.io/skobkin/magnetico-web:0.8.0 migrate stats-schema-init   # optional: statistics for the administrator section
```

An older **development** `magnetico_ingest` schema without the `crawlers` table is refused, never repaired; see
`UPGRADE.md` ("Reinitialising an older development ingest schema").

```yaml
# docker-compose.yml — web and ingest (placeholders; secrets live in the env files, chmod 600, never committed)
services:
  web:
    image: docker.io/skobkin/magnetico-web:0.8.0
    command: ["web"]
    restart: unless-stopped
    environment:
      MAGNETICO_WEB_LISTEN: 0.0.0.0:8081
      MAGNETICO_WEB_PUBLIC_URL: https://magnet.example.org     # must be an https:// URL
      MAGNETICO_WEB_TRUSTED_PROXIES: 172.20.0.0/16             # the narrowest set your proxy connects from
      GOMEMLIMIT: 768MiB
    env_file: [./web.env]                                      # MAGNETICO_WEB_DATABASE=postgres://… (and SMTP settings)
    mem_limit: 1g
    stop_grace_period: 45s                                     # MAGNETICO_WEB_SHUTDOWN_TIMEOUT defaults to 30s
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    healthcheck:                                               # service-specific: the image has none
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8081/readyz"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 30s
    networks: [traefik, db]
    labels:
      - traefik.enable=true
      - traefik.docker.network=traefik
      - traefik.http.routers.magnetico-web.rule=Host(`magnet.example.org`)
      - traefik.http.routers.magnetico-web.entrypoints=websecure
      - traefik.http.routers.magnetico-web.tls.certresolver=letsencrypt
      - traefik.http.services.magnetico-web.loadbalancer.server.port=8081
  ingest:
    image: docker.io/skobkin/magnetico-web:0.8.0
    command: ["ingest"]
    restart: unless-stopped
    environment:
      MAGNETICO_INGEST_LISTEN: 0.0.0.0:8080
      GOMEMLIMIT: 256MiB
    env_file: [./ingest.env]                                   # MAGNETICO_INGEST_DATABASE=…  (crawler tokens live in PostgreSQL: `ingest crawler add`)
    mem_limit: 1g
    stop_grace_period: 60s
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    healthcheck:
      test: ["CMD", "nc", "-z", "127.0.0.1", "8080"]           # TCP only: ingest has no health endpoint (see below)
      interval: 30s
      timeout: 5s
      retries: 3
    networks: [traefik, db]
    labels:
      - traefik.enable=true
      - traefik.docker.network=traefik
      - traefik.http.routers.magnetico-ingest.rule=Host(`ingest.example.org`) && PathPrefix(`/api/ingest/v1`)
      - traefik.http.routers.magnetico-ingest.entrypoints=websecure
      - traefik.http.routers.magnetico-ingest.tls.certresolver=letsencrypt
      - traefik.http.services.magnetico-ingest.loadbalancer.server.port=8080
networks:
  traefik: { external: true }
  db: { external: true }
```

Both services verify their schema read-only at start and **fail closed** when it is missing, unsupported or the role
lacks privileges (exit 1, a fixed message, no credentials). An unknown `MAGNETICO_WEB_*` / `MAGNETICO_INGEST_*` variable
stops the service at start with the list of known ones. By default both connect with the same account, the owner of
the database ([`web-deployment.md`](web-deployment.md) §4).

**Testing before the public cutover.** The web service accepts a loopback `http://` public URL (development mode) only
with a loopback listen address. With the bridge network above the listener is not on the host's loopback, so the service
refuses to start; for a private test run one container with `network_mode: host` (or `--network host`),
`MAGNETICO_WEB_LISTEN=127.0.0.1:PORT` and `MAGNETICO_WEB_PUBLIC_URL=http://127.0.0.1:PORT`, and reach it through an SSH
tunnel, or use a private TLS hostname behind the proxy ([`migration_0.8.0.md`](migration_0.8.0.md) §9.2).

## Crawler with a persistent local queue and a UDP port

The crawler has no database: it queues metadata in SQLite on `/data` and delivers it to ingest over HTTPS. It needs
**inbound UDP on the indexer port** (publish it and open it on the firewall/NAT) and outbound UDP/TCP. The full
network, token and operations guide is [`crawler.md`](crawler.md); the minimal form:

```sh
docker volume create magnetico-queue      # local, fsync-honouring storage (ext4/xfs), not NFS/SMB
docker run -d --name magnetico-crawl --restart unless-stopped --init \
  -p 6881:6881/udp -v magnetico-queue:/data \
  -e MAGNETICO_CRAWL_INGEST_URL=https://ingest.example.org \
  -e MAGNETICO_CRAWL_INDEXER_ADDR=0.0.0.0:6881 \
  --env-file ./crawler.env \                       # MAGNETICO_CRAWL_INGEST_TOKEN=… (≥ 32 bytes; chmod 600)
  --stop-timeout 60 \
  docker.io/skobkin/magnetico-web:0.8.0 crawl
```

`MAGNETICO_CRAWL_QUEUE_DIR=/data` is already the image default. One crawler per queue (the directory is locked).

## Migration and administrative commands

One-shot containers. The complete migration procedure is [`migration_0.8.0.md`](migration_0.8.0.md); this section
shows how to run its commands with the image.

**Passing credentials.** Keep connection strings and passwords out of command arguments: an argument such as
`-e MAGNETICO_MIGRATE_DATABASE=postgres://user:password@…` is visible in the process list of the host (`ps`) and stays in
the shell history. Use either an env file or a name without a value:

- `--env-file ./migrate.env`: one `NAME=value` per line, no quotes (Docker takes the value literally), `chmod 600`,
  never committed;
- `-e MAGNETICO_MIGRATE_DATABASE` (no `=`): Docker copies the value from your shell's environment, for example after
  `set -a; . ./migrate.sh; set +a`.

Inside the container the values are environment variables, which the process never logs. Anyone who may run
`docker inspect` on a running container (root or the `docker` group) can read them, so treat Docker access as access to
the credentials.

```sh
# migrate.env (chmod 600). The user migration reads only MAGNETICO_MIGRATION_USERS_SOURCE_DSN, never the torrent source.
MAGNETICO_MIGRATE_DATABASE=postgres://magnetico:PASSWORD@db:5432/magnetico?sslmode=require
MAGNETICO_MIGRATION_SOURCE_DSN=postgres://magnetico_legacy_ro:PASSWORD@legacy-db:5432/LEGACY_TORRENT_DB?sslmode=require
MAGNETICO_MIGRATION_USERS_SOURCE_DSN=postgres://magnetico_legacy_ro:PASSWORD@legacy-db:5432/LEGACY_APP_DB?sslmode=require
```

**Reports.** Mount a directory on durable storage (not `/tmp`, which may be a RAM disk) at `/reports`. Run the command
as your own user so that you can read what it writes; the files are created with mode `0600`:

```sh
mkdir -p /srv/magnetico/reports && chmod 700 /srv/magnetico/reports

# the torrent import (resumable: re-run the same command after any interruption; exit 2 = interrupted)
docker run --rm --network db --env-file ./migrate.env \
  docker.io/skobkin/magnetico-web:0.8.0 migrate torrents --defer-secondary-indexes

# the forensic export of the migration's operational state, kept OUTSIDE the container
docker run --rm --network db --env-file ./migrate.env --user "$(id -u):$(id -g)" -v /srv/magnetico/reports:/reports \
  docker.io/skobkin/magnetico-web:0.8.0 migrate errors export --output /reports/export.jsonl --manifest

# the user migration's dry run, with its secret-free report
docker run --rm --network db --env-file ./migrate.env --user "$(id -u):$(id -g)" -v /srv/magnetico/reports:/reports \
  docker.io/skobkin/magnetico-web:0.8.0 migrate users --source-timezone Europe/Moscow --dry-run --report-json /reports/users-dry-run.json
```

Without `--user`, the directory must be owned by `10001:10001` and the files belong to uid `10001`; read them as root
or through the image: `docker run --rm -v /srv/magnetico/reports:/reports:ro --entrypoint cat
docker.io/skobkin/magnetico-web:0.8.0 /reports/users-dry-run.json`.

**Interactive password entry.** `web user set-password` reads the password from the terminal with echo off (twice)
when stdin is a terminal, and one line from stdin otherwise. Never put a password in an argument or an environment
variable:

```sh
# web.env (chmod 600): MAGNETICO_WEB_DATABASE=postgres://… and MAGNETICO_WEB_PUBLIC_URL=https://magnet.example.org
docker run --rm -it --network db --env-file ./web.env docker.io/skobkin/magnetico-web:0.8.0 web user set-password alice   # interactive, echo off
printf '%s\n' "$PW" | docker run --rm -i --network db --env-file ./web.env docker.io/skobkin/magnetico-web:0.8.0 web user set-password alice
```

`web user add` and the other link-printing commands need `MAGNETICO_WEB_PUBLIC_URL` (the printed setup link points at
it). `magnetico dev seed-torrents` is for disposable development databases only (`docs/development-fixtures.md`).

## Service-specific healthchecks

The image has **no** `HEALTHCHECK`: crawl, ingest, web and migrate need different ones, and a universal one would be
wrong for most of them. Use the check of the service you run:

| Service | Check | Note |
|---|---|---|
| `web` | `GET /healthz` (liveness) and `GET /readyz` (readiness: the process is not draining and PostgreSQL answers) on the listen port; both are public and carry no detail. The image has BusyBox `wget`: `wget -q -O /dev/null http://127.0.0.1:8081/readyz` | exit status 0 only for a 200 |
| `ingest` | **no health endpoint exists** (none was added). TCP connect: `nc -z 127.0.0.1 8080` proves the listener only, not PostgreSQL. A deeper check is the crawler-side path check in `crawler.md` §7 (an authenticated empty `exists` request) | |
| `crawl` | **no listener and no endpoint.** Watch the logs (`leech started`, `delivered`, `queue records=`). Do **not** use `crawl check-queue`: it takes the queue lock and is for a stopped crawler | |
| `migrate`, `dev` | one-shot: judge them by their exit status (`0` ok, `1` failure or refusal, `2` interrupted — re-run to resume) | |

## Graceful shutdown

Every service stops cleanly on `SIGTERM` (`docker stop`, `docker compose stop`); give it a grace period longer than its
own drain budget or Docker kills it with `SIGKILL` first:

| Service | Drain | Give Docker | Exit status |
|---|---|---|---|
| `web` | `MAGNETICO_WEB_SHUTDOWN_TIMEOUT` (default 30 s) | `stop_grace_period: 45s` | `0`; `3` when the grace period expired with requests still running |
| `ingest` | consumer grace + abort grace + HTTP drain (see `crawler.md` §8) and, last, one bounded flush of the statistics (≤ 5 s, plus ≤ 2 s to close its connections): 35 s worst case | `60s` | `0`; `3` when a drain budget was exceeded |
| `crawl` | stops the DHT, cancels leeches, commits what is buffered (5 s), lets the sender finish (30 s), closes the queue (≈ 45 s worst case) | `60s` (`--stop-timeout 60`) | `0`; `3` when the SQLite writer is stuck in the operating system (SQLite recovers the queue at the next start; committed records survive any exit) |
| `migrate torrents` and the other long runs | the run is checkpointed and resumable: an interruption commits the settled work | a generous `--stop-timeout` (the grace period only has to cover the current batch) | `2` interrupted (resume by re-running the same command), `1` failure or refusal |

## Resource recommendations

Measured, not guessed; see the reports for the dataset and configuration of each number.

| Service | Container limit | `GOMEMLIMIT` | Source |
|---|---|---|---|
| `web` | **1 GiB** (512–640 MiB survived the typical-shape test but leave no margin for adversarial file lists and are not recommended) | `768MiB` (a soft Go heap target, a different number from the container limit) | `docs/wip/07J-web-integration-security-report.md` §6, `web-deployment.md` §7 |
| `ingest` | **1 GiB** (512 MiB is not recommended: OOM-killed in the worst case without `GOMEMLIMIT`) | `256MiB` (observed 139 MiB resident at 20 records/s) | `docs/wip/06-crawler-ingest-results.md`, `crawler.md` §2 |
| `crawl` | no value was measured under a limit; resident memory is ≈ 30 MiB. Provision disk beyond `MAGNETICO_CRAWL_QUEUE_MAX_BYTES` (WAL and indexes are extra) | not needed | `crawler.md` §6 |

`GOMEMLIMIT` is a target, not a limit: the process can exceed it. The recommendations are for synthetic data of the
documented shape and are not a claim about every workload; the RC0 checklist asks for confirmation with the real image.

## Building the image yourself

```sh
docker buildx build --build-arg VERSION=0.8.0-rc1 --build-arg COMMIT=$(git rev-parse HEAD) -t magnetico-web:local .
# multi-platform, written to an OCI layout instead of the local store (no emulation needed: the final stage has no RUN):
docker buildx build --platform linux/amd64,linux/arm64 --sbom=true --provenance=mode=max \
  --output type=oci,tar=false,dest=./image,name=magnetico-web:local .
```

The build needs Go 1.27.4 or newer (the compiler stage uses `GOTOOLCHAIN=local`, so an older toolchain image fails the
build instead of silently shipping an older standard library). `ci/image-smoke.sh <image-or-digest>` runs the same
checks the release pipeline runs. Native Linux binaries (amd64, arm64) are attached to every release on Forgejo; building
from source: `go build -o magnetico ./cmd/magnetico`.
