# Development fixtures — `magnetico dev seed-torrents`

A deterministic set of **synthetic torrents** for manually testing the web UI (search, details, both file-browser modes, RSS, API). It is a development helper: **never run it against a database you care about** (see [Safety](#safety)).

Nothing is downloaded and the command does no DHT, BitTorrent, tracker or other unrelated external network activity; its only network use is the connection to the PostgreSQL server you name (a remote server is supported, so use a TLS URL when it is not on loopback): infohashes are derived from fixed keys, names, paths, sizes and timestamps come from a fixed-seed generator, and the data is written through the production persistence API (`persistence.Store.AddTorrent`: the real validation, the real wire-format-v1 codec, the real transactions). Nothing runs automatically at startup or during schema initialization.

## The dataset (`magnetico-dev-fixtures-v1`)

101 torrents, 13,279 files, about 89 TB of nominal size:

| Group | Count | Content |
|---|---|---|
| Single-file | 40 | 1 byte to 3 TiB; ASCII, Cyrillic, Japanese and special-character names; one very long name; the *no-blob* shape (file path equals the torrent name) and atypical single files (kept with a blob) |
| Multi-file | 60 | 10 to 100 files each (the bounds occur exactly), in ten layouts repeated six times: flat, seasons, a deep chain (12–20 levels), wide fan-out, Unicode names, special characters (`# ? % & [ ] ( ) ' " ; : \| < >`, leading dot, trailing space), long components (≈ 240 bytes, paths over 1 KiB), duplicate `(path, size)` pairs and the same path with different sizes, file/directory name clashes with literal `.` and `..` components (and, once, leading, trailing and doubled slashes that force the codec's flat/prefix fallback), zero-size files |
| Large | 1 | exactly **10,000** files: 80 directories of 100 files, one flat directory of 1,500 files, 500 files in a six-level hierarchy |

Discovery times are spread over 2019–2026 (some torrents share a timestamp, to exercise tie ordering). The web UI offers the complete file tree only when the torrent has at most 4,000 files **and** passes the size limits (a pre-check estimate of at most 1 MiB and a serialized tree of at most 4 MiB, the `fileview` defaults); otherwise it falls back to paginated directory pages. The file count alone does not decide, so a torrent with long or numerous paths can be paginated below 4,000 files. The dataset test pins only the file-count split (60 below the threshold, 1 above); it does not assert the byte limits, so check the mode in the UI (`full_tree_available`) rather than assuming it from the count, especially for the long-path torrents. By file count alone, the 60 ordinary multi-file torrents fall below the threshold (expected **full-tree** mode) and the one large torrent (10,000 files) is above it (**paginated-directory** mode); the 40 single-file torrents have a single file and no tree to browse. Full-tree availability is the outcome of the file-count limit **and** the estimate and serialized-size limits, so the file count is a necessary condition for the full tree, not a sufficient one.

The dataset is pinned by tests (a SHA-256 over every field of every torrent and file, and the first infohash `59842347fe411e7b5672e64d25cadde5ec4b50d2`). Changing the generator requires a new `Version` and therefore new infohashes.

## Quick start (disposable PostgreSQL in Docker)

Requirements: the `magnetico` binary (`go build -o magnetico .`) and a disposable PostgreSQL 17 server. The password and port below are examples for a throwaway container bound to loopback only.

```bash
# 1. A disposable PostgreSQL (removed when stopped)
docker run -d --rm --name magnetico-dev-pg -e POSTGRES_PASSWORD=dev \
  -p 127.0.0.1:5432:5432 postgres:17

export DEV_DSN='postgres://postgres:dev@127.0.0.1:5432/postgres?sslmode=disable'

# 2. Initialize the schemas (the torrent schema, then the web schema)
export MAGNETICO_MIGRATE_DATABASE="$DEV_DSN"
./magnetico migrate schema-init
./magnetico migrate web-schema-init

# 3. Preview, then seed (the target comes from the environment, so a password
#    never appears in the process list)
export MAGNETICO_DEV_DATABASE="$DEV_DSN"
./magnetico dev seed-torrents --dry-run
./magnetico dev seed-torrents --confirm-development-database

# 4. An administrator account for the web UI (prints a one-time setup link)
export MAGNETICO_WEB_DATABASE="$DEV_DSN"
export MAGNETICO_WEB_PUBLIC_URL=http://127.0.0.1:8081
./magnetico web user add dev-admin --email dev@example.org
./magnetico web user set-role dev-admin admin

# 5. Start the web UI (loopback http:// public URLs run in development mode)
./magnetico web
```

Open the setup link printed in step 4 (it points at `http://127.0.0.1:8081/password/setup/…`), choose a password, sign in, and browse `http://127.0.0.1:8081/torrents`. Stop the server with Ctrl+C and the database with `docker stop magnetico-dev-pg`.

Things worth trying:

- Search for `Классическая`, `桜`, `日本`, `Tom &`, `multi-` or `single-007` (every name ends in a group and a number).
- Sort by size and by date; look for ties in the discovery time.
- Open `multi-000` (10 files) and `multi-001` (100 files): complete tree. Open the *Large archive 10000 files* torrent: directory pages; its `flat-dump` directory spans several pages.
- Open the long-name, special-character, duplicate and `.`/`..` torrents and check escaping and ordering.
- `GET /api/v2/torrents/{id}` shows `full_tree_available` (expected true for the 60 ordinary multi-file torrents and false for the large one; the test pins only the file-count split, so confirm the long-path torrents there).

## Command reference

```text
magnetico dev seed-torrents [--database URL] [--dry-run] [--confirm-development-database]
```

| Option | Meaning |
|---|---|
| `--database URL` / `MAGNETICO_DEV_DATABASE` | The target (prefer the environment variable: a URL on the command line shows up in process listings). **Required, no default**, and never inherited from the `migrate` or `web` settings (flag, environment or config file). The flag wins over the environment variable. |
| `--dry-run` | Run every check and report what would be added; write nothing. |
| `--confirm-development-database` | Attest that the target is a disposable development database. Required for a real run; CLI only. |

Output carries the redacted database URL (scheme, host and path), counts and the state name only. Exit status 0 on success (including a no-op re-run) and 1 on any refusal or failure.

## Safety

- **Never** run it against production or against a database that will receive the legacy import. Fixtures in a database make `migrate torrents` refuse the target (it needs an empty one), and a mixed database is not something you want to clean up by hand. The explicit target, the confirmation flag and the refusals below are guards, not a substitute for using a throwaway database.
- The schema must already be a verified 0.8.0 schema (`migrate schema-init`). A missing, legacy, partial or unsupported schema is refused and **never created, altered or migrated** by this command; nothing is ever updated, deleted or truncated, and no other schema (`magnetico_web`, `magnetico_ingest`, …) is touched.
- **Populated databases are refused.** The database must hold no torrents at all or only fixture torrents (matched by their derived infohashes). Any other torrent — a crawled one, an imported one — refuses the run with `the database already contains torrents that are not part of the development fixture` before a single row is written. A fixture infohash that is already stored with different metadata is refused too. For every fixture already present, the **complete file list** is compared with the dataset: the `(path, size)` entries as a multiset, so duplicates and their multiplicity count (the same path stored twice is not the same as once, and a different size or an extra or missing entry is a mismatch). A mismatch is refused as a conflict, and a missing or corrupt stored file list fails the run; nothing is rewritten either way.
- **Repeated runs are defined.** Re-running on a complete dataset is a reported no-op (`database was seeded … added 0 torrents`). After an interrupted run (only fixture torrents, some missing) a re-run adds exactly the missing ones. Inserts are idempotent per infohash, and existing rows are never rewritten.
- A session-level advisory lock stops two seeders from running at once (`another seed-torrents run is in progress`). The torrent-count check and the inserts are not one transaction, so do not run an `ingest` service against the same database.

## Tests

`go test ./internal/devseed ./opflags .` covers, on disposable PostgreSQL only: the dataset shape and its pinned digest; production validation and codec round-trips of every fixture; the coverage list above; both file-browser modes (the web file index builds for every multi-file torrent); counts, file and byte totals, decoded payloads, determinism across two fresh databases; the no-op re-run, completion after an interruption, refusal of foreign torrents, conflicting fixtures, uninitialized, legacy and wrong-version databases (nothing written, no schema created), other schemas untouched, the advisory lock, secret-free errors; and the real binary end to end (no credential in any output).
