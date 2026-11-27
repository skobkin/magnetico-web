# Running the crawler on a separate machine

`magnetico crawl` discovers torrents on the BitTorrent DHT and has **no database access**. It queues what it finds in a local SQLite file and delivers it over authenticated HTTPS to `magnetico ingest`, which runs next to the main PostgreSQL server. This guide deploys that split: one crawler machine (or several) in a place with a good, DHT-reachable network connection, and the ingest service on the main server behind Traefik.

Normative design: [`docs/specs/crawler-ingest-delivery-v1.md`](specs/crawler-ingest-delivery-v1.md). Release notes: [`UPGRADE.md`](../UPGRADE.md). Everything below was checked against the implementation (`opflags/`, `internal/crawler`, `internal/ingest/server`, `dht/`, `metadata/`). The two services were run together against local test data and a disposable PostgreSQL. **A live-DHT acceptance run is still open** (neither the RC0 acceptance nor the RC1 migration rehearsal included one), so the port notes in §5 come from the code, not from observing a live crawler.

> **Each crawler has its own credential.** `magnetico ingest crawler add <name>` generates it (`mgc_…`) and prints it once; ingest stores only its digest in PostgreSQL. It is **not** a user, API or RSS token, and it is never shown again (the administrator Crawlers page lists crawlers, not credentials). Rotate or disable one crawler without touching the others (§3). Upgrading from the legacy version? The old crawler is retired; set up the new one after the migration ([`docs/migration_0.8.0.md`](migration_0.8.0.md) §10).

## 1. Architecture and network flow

```
 crawler machine                                              main server
┌─────────────────────────────┐                      ┌──────────────────────────────────────┐
│ magnetico crawl             │   HTTPS (443)        │ Traefik (TLS termination)            │
│  DHT ◄── UDP ──► Internet   │ ───────────────────► │   │ plain HTTP, private network      │
│  peers ◄─ TCP ─► Internet   │  POST /api/ingest/   │   ▼                                  │
│  SQLite queue (volume)      │       v1/torrents[/exists]  magnetico ingest (:8080)         │
└─────────────────────────────┘  Authorization:      │   │ SQL                              │
                                 Bearer <token>      │   ▼                                  │
                                                     │ PostgreSQL (magneticod + magnetico_ingest)
                                                     └──────────────────────────────────────┘
```

- The crawler **initiates every connection to the main server**; the main server never connects to the crawler, and PostgreSQL is **never** exposed to the crawler machine.
- Delivery is at-least-once and idempotent by infohash. A `202` means the batch is durably committed in the PostgreSQL *queue*; a single consumer inside `ingest` then stores it with the production persistence code.
- If ingest, Traefik or the network is down, records stay in the crawler's SQLite queue (up to `--queue-max-bytes`) and are sent when delivery works again.
- Delivery is batched for throughput, not latency: existence checks go out when 1,024 hashes are waiting or 1 s after the first one, and metadata goes out in time-based batches (the oldest record waits at most 1 s; a batch is sent at once when it holds 256 records or the next record would not fit in 8 MiB). Expect about one request per second per endpoint, over one reused connection (HTTP/2 over TLS).
- The ingest API has exactly two routes, both `POST`, both requiring `Authorization: Bearer <token>`: `/api/ingest/v1/torrents/exists` (existence pre-check, at most 1,024 hashes, 64 KiB body) and `/api/ingest/v1/torrents` (at most 256 records, **17 MiB** body). There is no health endpoint (§7 shows how to check the service).

## 2. Prepare the main server

All of this happens on the main server, with a database you have already initialized.

1. **Schemas.** The torrent schema must exist and, if you are migrating from the legacy version, the torrent import must be complete (`ingest` refuses to start while an import is running or incomplete). Then create the queue schema (idempotent, non-destructive):

   ```bash
   export MAGNETICO_MIGRATE_DATABASE='postgres://USER:PASSWORD@DB_HOST:5432/DB_NAME?sslmode=require'
   magnetico migrate schema-init          # only on a fresh database
   magnetico migrate ingest-schema-init
   ```

   Optionally, `magnetico migrate stats-schema-init` creates the statistics schema that the administrator section of the web service shows (idempotent; `ingest` runs without it and never depends on it, `MAGNETICO_INGEST_STATS_DISABLED=true` turns the collection off). What it counts and how to read it: [`docs/operator-statistics.md`](operator-statistics.md).

   If the database is the result of a legacy migration, start ingest only at the cutover, after `verify-content` passed ([`docs/migration_0.8.0.md`](migration_0.8.0.md) §10). `magnetico_migration` stays in place by default and ingest works with it. The optional `magnetico migrate cleanup` is only possible **before** ingest stores its first torrent (the guide's Appendix B); afterwards keep the schema.

2. **PostgreSQL must run with `fsync = on`**; ingest refuses to start otherwise (`MAGNETICO_INGEST_ALLOW_NON_DURABLE_DB=true` exists for development only).
3. **Run ingest** (`magnetico ingest`). Its settings are environment variables (no config-file layer; flags `--listen`, `--database`, … also exist, see `magnetico ingest --help`):

   | Variable | Meaning | Default |
   |---|---|---|
   | `MAGNETICO_INGEST_DATABASE` | PostgreSQL URL (contains a password: keep it in the environment) | development default |
   | `MAGNETICO_INGEST_QUEUE_DATABASE` | optional second URL for the `magnetico_ingest` queue (a role with DML on that schema only) | the main URL |
   | `MAGNETICO_INGEST_LISTEN` | listen address, **plain HTTP** | `127.0.0.1:8080` |
   | `MAGNETICO_INGEST_QUEUE_MAX_BYTES` | quota of queued + quarantined payload bytes; above it admissions get `503 queue_full` | `2147483648` |
   | `MAGNETICO_INGEST_CACHE_MAX_BYTES` / `_CACHE_DISABLED` | existence cache | 8 MiB / off |
   | `MAGNETICO_INGEST_TRUSTED_PROXIES` | CIDR list of reverse proxies whose `X-Forwarded-For` is believed; decides the client address recorded as a crawler's last address (the TCP peer when empty) | empty |
   | `MAGNETICO_INGEST_STATS_DISABLED` | `true` turns the statistics off (no writer, no extra database connections); authentication and ingestion are unaffected | off |

   Set `GOMEMLIMIT=256MiB` and give the container a 1 GiB memory limit. Measured on synthetic load (40 records/s for two hours plus bursts), `GOMEMLIMIT=256MiB` lowered mean RSS from 459 to 304 MiB and the peak from 621 to 354 MiB, with no measurable throughput or backlog cost; with it, a 1 GiB container survived every tested case, including eight simultaneous near-limit (16 MiB) submissions (peak 518 MiB). 512 MiB is not recommended: without `GOMEMLIMIT` the worst case was OOM-killed there (`docs/wip/06-crawler-ingest-results.md`). `GOMEMLIMIT` is a target for the Go runtime, not a hard limit, and these numbers are from synthetic data. When ingest runs in a container behind Traefik, listen on `0.0.0.0:8080` **inside the container network only** and publish no host port. Any variable starting with `MAGNETICO_INGEST_` that is not in the table above is rejected at startup. `MAGNETICO_INGEST_TOKEN` (the former shared secret) is refused with a message: crawler credentials are per crawler and live in PostgreSQL (§3).

## 3. Register crawlers and distribute their credentials

Every crawler has its **own** credential, stored in the `magnetico_ingest.crawlers` table of the ingest database (only a SHA-256 digest is stored). The ingest service no longer has a token of its own. A crawler's identity (a stable, server-generated UUID and a unique name) comes **only** from the credential it presents; there is no identity header and no identity file.

```bash
# the database URL comes first (flag or MAGNETICO_INGEST_DATABASE; the queue role works too: --queue-database)
magnetico ingest crawler add paris-1        # prints the id and the token ONCE
magnetico ingest crawler list               # id, name, enabled, created, token rotated, disabled; never a token
magnetico ingest crawler rotate-token paris-1
magnetico ingest crawler disable paris-1    # and: enable paris-1
```

- `add` and `rotate-token` print `token: mgc_…`; it cannot be shown again. Names are 1–64 characters of `A-Z a-z 0-9 . _ -`, unique case-insensitively. There is no automatic registration, no second token and no expiry: **exactly one** valid token per crawler; rotating replaces it.
- Give the printed token to that crawler as `MAGNETICO_CRAWL_INGEST_TOKEN`. Store it in a secret manager or a root-only file (`chmod 600`); **do not** commit it, put it literally in `docker-compose.yml`, pass it as an argument or paste it into tickets and logs. No stray whitespace: a trailing newline or space is part of the value. With Docker, use an `--env-file` entry `NAME=value` with no quotes.
- **A new token works at once.** A disabled, rotated-away or removed token keeps working on a running ingest for **up to 60 seconds** (the positive authentication cache); after that the crawler gets `401`, keeps everything queued and retries slowly (§9). Nothing is lost. Rotate immediately if a token leaks, and disable the crawler if you need it refused.
- A missing, malformed, unknown or disabled token all answer the same `401`. If the database cannot be read during authentication, ingest answers a retryable `503` (never a false `401`).
- After a fresh `migrate ingest-schema-init` **no crawler exists**: ingest starts, logs one warning (`no enabled crawler is registered`), and every request gets `401` until `ingest crawler add` has been run. No restart is needed.
- Permissions: the ingest role needs `SELECT` on `magnetico_ingest.crawlers`; the role running `ingest crawler …` needs `SELECT`, `INSERT` and `UPDATE` on it (the default single account has everything). An older **development** `magnetico_ingest` without the `crawlers` table is refused and must be reinitialised by hand ([`UPGRADE.md`](../UPGRADE.md)).

## 4. Traefik on the main server

The ingest service speaks plain HTTP; Traefik terminates TLS. The crawler accepts only an `https://` ingest URL (plain `http://` only to a loopback host) and **never follows redirects**: a redirect (for example an HTTP→HTTPS redirect hit by a wrong URL) is treated as an unexpected status and the records stay queued.

Recommendations:

- Use a **dedicated hostname** for ingest (`ingest.example.org`), separate from the web UI, with a valid public certificate (for example from Let's Encrypt via Traefik's ACME resolver). The crawler verifies the certificate against the system roots with the host name from the URL; there is no option to skip verification. For a private CA, mount the CA bundle into the crawler container and point `SSL_CERT_FILE` at it (this replaces the system roots for the whole process).
- Route only the ingest API: `Host(ingest.example.org) && PathPrefix(/api/ingest/v1)`. If the crawler URL carries a path prefix (`https://example.org/ingest`), the crawler appends `/api/ingest/v1/...` to it, so your router must strip or map that prefix accordingly; a dedicated host avoids this.
- **Allow request bodies of at least 17 MiB** on `/api/ingest/v1/torrents` (the largest permitted batch). Do not add a smaller body-size middleware, and watch any WAF or CDN in front of Traefik.
- The crawler gives a submit 60 s and an existence lookup 10 s. Traefik's entry point `respondingTimeouts` (`readTimeout`, `writeTimeout`, `idleTimeout`) must not cut a 17 MiB upload from a slow link earlier than that. The option names and your Traefik version's defaults **were not verified here**: check the documentation of your version and set them explicitly (`web-deployment.md` §3 has the same caveat).
- Restrict who can reach it if you can: the crawler's public IP(s) via Traefik's IP allow-list middleware (the name depends on the version: `ipAllowList` in v3, `ipWhiteList` in v2), or a private network or VPN between the machines. The token is the authentication; network restriction is defence in depth. Keep access logs free of the `Authorization` header: Traefik's access log is off by default and logs no header unless `accessLog.fields.headers.defaultMode` is `keep`, in which case it logs `Authorization` in full (see `web-deployment.md` §3, "Proxy access logs", for the tested configuration).

<details>
<summary>Example: ingest and Traefik labels on the main server (Docker Compose; a complete web + ingest example is in <code>docs/container.md</code>)</summary>

```yaml
# docker-compose.ingest.yml — main server (the image runs as 10001:10001 and its entrypoint is `magnetico`)
services:
  ingest:
    image: docker.io/skobkin/magnetico-web:0.8.0    # ghcr.io/skobkin/magnetico-web carries the same image
    command: ["ingest"]
    restart: unless-stopped
    environment:
      GOMEMLIMIT: 256MiB
      MAGNETICO_INGEST_LISTEN: 0.0.0.0:8080
    env_file:
      - ./ingest.env          # MAGNETICO_INGEST_DATABASE=... (chmod 600, never committed)
    mem_limit: 1g
    stop_grace_period: 60s
    networks: [traefik, db]
    labels:
      - traefik.enable=true
      - traefik.docker.network=traefik
      - traefik.http.routers.magnetico-ingest.rule=Host(`ingest.example.org`) && PathPrefix(`/api/ingest/v1`)
      - traefik.http.routers.magnetico-ingest.entrypoints=websecure
      - traefik.http.routers.magnetico-ingest.tls.certresolver=letsencrypt   # your resolver's name
      - traefik.http.services.magnetico-ingest.loadbalancer.server.port=8080
networks:
  traefik:
    external: true
  db:
    external: true            # a network that can reach PostgreSQL
```

</details>

## 5. Network exposure: ports, NAT and firewall (crawler machine)

| Direction | Protocol / port | Purpose | Notes |
|---|---|---|---|
| **Outbound** | TCP 443 to the ingest host | HTTPS delivery and existence checks | The only connection to the main server. Honours `HTTPS_PROXY`/`NO_PROXY` from the environment. |
| **Outbound** | UDP to the bootstrap nodes: `dht.tgragnato.it` ports 80, 443, 1337, 6969, 6881, 25401 (the defaults of `--bootstrap-node`) | DHT bootstrap | Plus DNS. Override with `MAGNETICO_CRAWL_BOOTSTRAP_NODE` (comma separated `host:port`). |
| **Outbound** | UDP to arbitrary DHT nodes | DHT traffic | Rate-limited by `--max-rps` (default 500 requests per second). |
| **Outbound** | TCP to arbitrary peers and ports | fetching torrent metadata from peers | Bounded by `--leech-max-active` (default 32 concurrent metadata downloads); `--leech-max-n` (default 1000) caps the peers tried per infohash. Do not restrict outbound TCP to a port list. |
| **Inbound** | **UDP on the indexer port** | DHT queries and replies (including `announce_peer`, which is how infohashes reach the crawler) | See below. |

**There is no inbound TCP listener and no HTTP port on the crawler.** The crawler opens one UDP socket per `--indexer-addr` entry (`MAGNETICO_CRAWL_INDEXER_ADDR`, comma separated). The default is `0.0.0.0:0`: the operating system picks a **random UDP port at every start**, which cannot be opened or forwarded in advance. Behind a firewall or NAT, give it a fixed port and allow/forward that UDP port:

- `MAGNETICO_CRAWL_INDEXER_ADDR=0.0.0.0:6881` (any free UDP port works; 6881 is only an example), and allow inbound UDP on that port from anywhere.
- **Docker** (default bridge network): publish it, `ports: ["6881:6881/udp"]`. Outbound traffic works through Docker's NAT, but nothing reaches an unpublished UDP port. `network_mode: host` is the alternative.
- **Home or office NAT:** forward UDP 6881 (the same port externally) to the crawler machine, and give that machine a fixed LAN address.
- A host firewall: allow UDP 6881 in, established/related traffic back, and all the outbound traffic above.
- Without inbound reachability the crawler can still send queries, but you should expect far fewer discoveries; check the counters (§7) after the first hour.
- For several crawlers on one IP, give each its own port; each crawler needs its **own queue directory** (it is locked: one crawler per queue).
- The bootstrap hosts and the DHT depend on outbound UDP; some VPS providers filter it, and many residential connections cap UDP flows. If `leech started` stays at 0 while the process is healthy, suspect the network first.

The main server needs inbound TCP 443 (Traefik) only; PostgreSQL stays on the private network.

## 6. The crawler container and its persistent queue

Image: `docker.io/skobkin/magnetico-web` (also `ghcr.io/skobkin/magnetico-web`, the identical digest), one multi-platform image for every subcommand; see [`container.md`](container.md). `0.8.0` in this guide stands for the release you run:

```bash
docker pull docker.io/skobkin/magnetico-web:0.8.0
```

`latest` follows the most recently published release. The image's entrypoint is `magnetico` (pass the subcommand as the command) and it runs as **`10001:10001`**: a named volume at `/data` is writable out of the box, a **bind mount must be `chown 10001:10001`**. `MAGNETICO_CRAWL_QUEUE_DIR=/data` is the image default.

Settings (environment, which is the recommended way in containers; every setting except the token also has a flag and a YAML key; `magnetico crawl --help` lists them all):

| Variable | Meaning | Default |
|---|---|---|
| `MAGNETICO_CRAWL_INGEST_URL` | `https://ingest.example.org` | required |
| `MAGNETICO_CRAWL_INGEST_TOKEN` | this crawler's own token from `magnetico ingest crawler add` (`mgc_…`; ≥ 32 bytes, **environment only**) | required |
| `MAGNETICO_CRAWL_QUEUE_DIR` | directory of the SQLite queue | `magnetico-crawl-queue` (relative to the working directory: set an absolute path, e.g. `/data`) |
| `MAGNETICO_CRAWL_QUEUE_MAX_BYTES` | limit of queued + quarantined payload bytes; at the limit new metadata is dropped and counted | `1073741824` |
| `MAGNETICO_CRAWL_LEECH_MAX_ACTIVE` | concurrent metadata downloads | `32` |
| `MAGNETICO_CRAWL_INDEXER_ADDR` | UDP bind address(es), see §5 | `0.0.0.0:0` |

**The queue volume.** Mount a persistent volume at the queue directory (`/data` below). It must be a **local filesystem that honours `fsync`** (ext4 or xfs; not NFS or SMB). The directory is created `0700`, holds `queue.db` (+ WAL) and a `LOCK` file, and must be writable by the container's user. Provision disk beyond the byte limit: the limit counts payload bytes only, not WAL or index overhead. The crawler's memory use is small (about 30 MiB RSS measured); it needs no `GOMEMLIMIT`.

<details>
<summary>Example: Docker Compose for the remote crawler</summary>

```yaml
# docker-compose.crawler.yml — crawler machine
services:
  crawler:
    image: docker.io/skobkin/magnetico-web:0.8.0
    command: ["crawl"]
    restart: unless-stopped
    environment:
      MAGNETICO_CRAWL_INGEST_URL: https://ingest.example.org
      MAGNETICO_CRAWL_QUEUE_DIR: /data
      MAGNETICO_CRAWL_INDEXER_ADDR: 0.0.0.0:6881
    env_file:
      - ./crawler.env           # MAGNETICO_CRAWL_INGEST_TOKEN=<this crawler's token>  (chmod 600, never committed)
    ports:
      - "6881:6881/udp"         # the DHT socket; allow/forward the same UDP port on your firewall/NAT
    volumes:
      - crawler-queue:/data     # persistent, local, fsync-honouring storage
    stop_grace_period: 60s      # a clean shutdown needs up to ~45 s (see §8)
    init: true
volumes:
  crawler-queue:
```

</details>

Keep `crawler.env` out of version control (it contains only `MAGNETICO_CRAWL_INGEST_TOKEN=…`). Do **not** add a `MAGNETICO_CRAWL_DATABASE` line or any other `MAGNETICO_CRAWL_*` name that is not a crawler setting: unknown names stop the crawler at startup with a list of the known ones (the table above lists the common ones; `magnetico crawl --help` has all flags).

**Configuration file (optional).** Outside containers the same settings can come from a YAML file passed with the
global option `--config-file-path`, which must precede the subcommand (`magnetico --config-file-path config.yaml
crawl …`). Crawl keys: `ingestURL`, `queueDir`, `queueMaxBytes`, `leechMaxActive`, `pyroscopeURL`, `indexerAddrs`,
`indexerMaxNeighbors`, `leechDeadline`, `leechMaxN`, `maxRPS`, `bootstrappingNodes`, `filterNodesCIDRs`. The token is
never read from the file or a flag, and the former `databaseURL` key was removed. The other crawler flags are unchanged
from upstream.

## 7. Start, verify and operate

**Order:** (1) PostgreSQL and the schemas (§2), (2) ingest and Traefik, (3) check the path from the crawler machine, (4) start the crawler.

**Check the path before starting the crawler.** Run both requests from the crawler machine, with the real token in `$TOKEN`. There is no health endpoint on ingest, and this guide does not add one.

*1. Connectivity and authentication.* An empty hash list is a valid request. It never reaches PostgreSQL, so it proves only DNS, TLS, Traefik, the token and that the ingest process answers:

```bash
curl -sS -i -X POST https://ingest.example.org/api/ingest/v1/torrents/exists \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"info_hashes":[]}'
```

`200` with `{"exists":[],"missing":[],"unknown":[]}` is the expected answer. `401` is a token mismatch, `405` means the request was not a `POST`, a TLS error means certificate or name trouble, `502/503/504` come from Traefik (ingest is down or unreachable) or are ingest's own `503` (`queue_full`, `storage_unavailable`, `busy`).

*2. A real lookup, which exercises PostgreSQL.* Ask about at least one infohash (40 hex characters). Use a **random** one that nothing has stored, so the answer cannot come from the cache:

```bash
HASH=$(openssl rand -hex 20)
curl -sS -i -X POST https://ingest.example.org/api/ingest/v1/torrents/exists \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"info_hashes\":[\"$HASH\"]}"
```

Expected: `200` with `{"exists":[],"missing":["<that hash>"],"unknown":[]}`. `missing` is a confirmed answer from a successful database lookup: the hash is not in `magneticod.torrents`. Records that are only waiting in the ingest queue are deliberately not consulted, so `missing` does not mean "not received yet".

How to read the three lists:

- `exists`: confirmed stored in PostgreSQL.
- `missing`: confirmed absent.
- `unknown`: the lookup failed or was abandoned (database error, timeout, shutdown). An error is never reported as `missing`. The crawler treats `unknown` as "not known to be stored" and sends the record anyway, which is safe because delivery is idempotent. If **every** hash of a request is `unknown`, ingest answers `503 storage_unavailable` instead of `200`, so a `200` with some `unknown` entries means only part of the batch could be answered.

**Cached answers are a limit of this check.** Ingest keeps a process-local existence cache. A positive entry (`exists`) never expires until it is evicted or ingest restarts, and a negative entry (`missing`) lasts about 20 seconds. A hash answered from the cache did not touch PostgreSQL, so asking twice about the same hash, or about a torrent that is already stored, says nothing about the database being reachable right now. Use a fresh random hash for each check (as above) and read the first answer; a repeated `missing` within 20 seconds is not new evidence. A `200` here also does not prove that the consumer writes to PostgreSQL or that the queue drains: for that, watch the ingest summary lines and the counters below after the crawler has started.

**Start:** `docker compose -f docker-compose.crawler.yml up -d`. There is no health endpoint on the crawler either; use the logs.

- At start the crawler logs `crawler started: delivering to https://ingest.example.org, local queue /data (limit … bytes), 32 active leeches at most` (credentials and query strings are never included).
- Every 5 minutes it logs `crawl summary: …`. After the first hour look for `leech started` and `joined` growing, `enqueue appended` growing, `send acked` growing, **`send … unauthorized=0`**, and `queue records=` staying small (it grows while delivery is blocked and drains afterwards). `quarantine records=` should stay 0 (§9).
- On the main server, ingest logs `ingest summary: …` (also every 5 minutes: `submit requests/accepted/rejected`, `auth_failures`, `consumer stored`) and the queue state. `SELECT count(*) FROM magneticod.torrents;` rises; `SELECT count(*) FROM magnetico_ingest.queue;` stays near 0 while the consumer keeps up.

**Shutdown.** Stop with `docker compose stop` (SIGTERM; give it `stop_grace_period: 60s`). The crawler stops the DHT, cancels leeches, commits what is buffered (5 s), lets the sender finish its request (30 s) and closes the queue (10 s wait). Queued records are never discarded on shutdown. Exit status 3 means the SQLite writer was stuck in the operating system: the process exits and SQLite recovers the queue at the next start.

**Queue recovery and diagnostics.**

- After a crash, kill, power loss or exit 3, simply start the crawler again; committed records are on disk and are delivered (idempotently).
- To check the queue file, **stop the crawler first** (the directory is locked), then run the diagnostic against the same volume, for example `docker compose -f docker-compose.crawler.yml run --rm --no-deps crawler crawl check-queue --queue-dir /data` (add `--full` for the slower `integrity_check`). Exit 0: healthy; exit 1: integrity check failed (committed data is never repaired automatically: keep a copy of the volume and see below); exit 2: it could not run (the directory is locked by a running crawler, or it holds no `queue.db`).
- If the queue is corrupt, copy the volume somewhere safe before doing anything else. Starting with an empty volume loses only the records that were still queued: the DHT keeps announcing the same torrents, so they will be rediscovered over time. The crawler holds no other state.
- Moving a crawler to another machine: stop it, copy the volume, start it there with the same token.

**Quarantine.** Both sides keep records that are permanently invalid in a quarantine table: `quarantine` in the crawler's SQLite file and `magnetico_ingest.quarantine` in PostgreSQL (the administrator section shows it too, [`docs/operator-statistics.md`](operator-statistics.md)). Nothing is deleted automatically, and quarantined bytes count toward the quota. Inspect the ingest side with SQL, for example `SELECT reason_code, count(*) FROM magnetico_ingest.quarantine GROUP BY 1;`. To retry records after fixing the cause, move them back in one transaction: `INSERT INTO magnetico_ingest.queue (info_hash, format, payload, payload_bytes) SELECT info_hash, format, payload, payload_bytes FROM magnetico_ingest.quarantine WHERE …;` followed by the matching `DELETE FROM magnetico_ingest.quarantine WHERE …;` (triggers maintain the usage counter).

## 8. Upgrades and token rotation

Upgrade with `docker compose pull` and `docker compose up -d` (change the image tag first if you use a version tag). Deploy ingest first when a release changes the ingest API (check `UPGRADE.md`), then the crawlers; a newer crawler that the server rejects stops quarantining after a run of consecutive rejections and keeps the records (`guard_trips` in the summary). For token rotation (`magnetico ingest crawler rotate-token`, effective on a running ingest within 60 seconds) see §3.

## 9. Troubleshooting

Log lines are quoted as the crawler prints them; the crawler's log lines never include the token, a URL query or record content.

| Symptom | Likely cause and what to do |
|---|---|
| Crawler exits at start: `MAGNETICO_CRAWL_INGEST_TOKEN is required and must be at least 32 bytes long` | Token missing in the container's environment, too short, or not passed (`env_file` path). Check with `docker compose config` (careful: it prints values; do not paste its output anywhere). |
| Crawler exits at start: `unknown environment variable MAGNETICO_CRAWL_…` | A misspelt or invented `MAGNETICO_CRAWL_*` name (the message lists the known ones). `MAGNETICO_CRAWL_DATABASE` is refused too, with a pointer to `UPGRADE.md`. |
| `send: ingest answered HTTP 401: check the ingest token on both sides; records stay queued` (and `unauthorized=` rising) | The crawler's token is not a valid registered credential: whitespace or a quote character in the env file, a token that was rotated away, or a disabled or unknown crawler (`magnetico ingest crawler list`). Ingest counts them as `auth_failures`. Retried with a long backoff; nothing is lost. |
| `send: ingest unavailable (status 0)` | Status 0 is a transport failure: DNS, connection refused/timeout, TLS handshake or certificate error. Run the §7 `curl` from the crawler machine (`curl -v` shows the certificate and the failing step). Check the system clock (certificate validity), the certificate chain (Traefik must serve the full chain), `HTTPS_PROXY` in the container, a custom CA (`SSL_CERT_FILE`). Records stay queued and are retried with a backoff of 1 s growing to 60 s. |
| `send: ingest unavailable (status 502/503/504)` | Traefik cannot reach ingest (container down, wrong port or network label) or ingest answered `503` (`queue_full`: the PostgreSQL queue passed `MAGNETICO_INGEST_QUEUE_MAX_BYTES`, so check that the consumer is running and PostgreSQL is healthy; `storage_unavailable`; `busy`). |
| `send: unexpected HTTP status 301/302/307/308 …` | A redirect: wrong scheme or host in the URL, or Traefik redirecting. The crawler does not follow redirects. Use the exact final `https://` URL. |
| `send: a rejection that does not identify the record was received (HTTP 413 …)` or `HTTP 400 …` with a non-ingest body | A proxy or the server rejected a request the crawler considers valid: a body limit below 17 MiB in Traefik or in front of it, or a version mismatch. The record is kept. |
| `send: … consecutive records were rejected as invalid … quarantining is suspended` | The server is rejecting everything: wrong ingest version or limits. Records are kept; fix the deployment. |
| `quarantine records=` above 0 | A record was permanently rejected (`send: ingest rejected a record as …; it was moved to the local quarantine`). Nothing is deleted automatically; quarantined bytes count toward the quota. Inspect with `sqlite3 /data/queue.db 'SELECT reason_code, count(*) FROM quarantine GROUP BY 1'` while the crawler is stopped; the ingest-side quarantine is described in §7. |
| `could not open the local queue in /data: …` | The directory is not writable by the container's user, the volume is missing, `queue.db` belongs to another schema version (refused to start), or another crawler holds the directory (`ErrLocked`: one crawler per queue; a second container or a `docker run` against the same volume). |
| `enqueue: the local queue is full; new metadata is dropped until the sender frees space` | Delivery has been blocked long enough to fill `QUEUE_MAX_BYTES`. Fix the delivery problem; raise the limit only with matching disk headroom. Dropped records are rediscovered later. |
| `the local queue failed (…); committed records stay on disk. Shutting down.` (exit 1) | A disk error (full disk, I/O error, a filesystem that does not honour `fsync`). Fix the storage, run `check-queue`, restart. |
| `crawl summary:` shows `leech started=0` for hours, process healthy | No DHT traffic reaches the crawler or it cannot fetch metadata: inbound UDP port not published or forwarded (§5), outbound UDP/TCP filtered by the provider, bootstrap host not resolving. |
| `precheck: the ingest existence lookup failed; continuing without it (fail-open…)` | The existence endpoint is unreachable or erroring; the crawler keeps working (more duplicate downloads) and retries. Same causes as `status 0` above. |
| Ingest refuses to start (`import … incomplete`, `fsync`, schema missing) | Finish and verify the torrent import first (`docs/migration_0.8.0.md`), use PostgreSQL with `fsync=on`, run `magnetico migrate ingest-schema-init`. |
| Torrents never appear although the queue is delivered | `SELECT count(*) FROM magnetico_ingest.queue` high: the single consumer is blocked or erroring (read ingest's log); `quarantine` on the ingest side non-empty: see §7 ("Quarantine"). |
