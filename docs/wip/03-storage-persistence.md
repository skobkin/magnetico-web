# Agent task 03 — Magnetico 0.8.0: implement compact torrent file-list storage and PostgreSQL persistence

**Status:** Implementation assignment, version 2, **phase 3 of 3**. **Language:** Go. **Target:** the unified Go source in **`refactor/0.8.0-unify-and-new-storage` in `magnetico-web`** after successful review of phase 1 (fork import) and phase 2 (upstream cleanup). Do **not** create a new repository, overwrite `master`, re-import upstream or repeat the PHP cleanup.

## Objective

Implement the **production storage codec v1** and the **PostgreSQL persistence changes** agreed for Magnetico 0.8.0, with exhaustive automated tests **on the prepared Go branch**, using the historical tgragnato code as a reference where helpful. This is a development phase, **not** a production deployment or one-time legacy data migration. Preserve working crawler behavior and keep the repository buildable throughout the work.

The owner intends to implement changes on this development branch and perform **one coordinated production cutover** only after the crawler, migrator and replacement web UI are complete. Do not perform any migration against a running production database.

## Read and preserve the contracts

The agreed specifications are already committed as:

- `docs/specs/wire-format-v1.md`
- `docs/specs/postgresql-persistence-v2.md`

Do not re-copy, rename, or silently modify either contract.

Read both documents in full before planning implementation. The wire format is **normative**: do not alter the header, IDs, varints, canonical sorting, three serializers, golden vectors or decoding semantics for convenience. If you find a contradiction, security flaw, or impossible requirement, document it and request a design decision rather than silently choosing a different on-disk format. Implement any changed design only after it is explicitly agreed.

Read the current branch packages, tests, cleanup report, and remaining schema/persistence code before editing; the upstream snapshot is already present in this branch. Keep useful crawler functionality, statistics, metadata handling, logging and test coverage. Treat `boramalper/magnetico`, the pinned `tgragnato/magnetico` commit, and the old PHP `master` as **read-only Git references** for old database structures and behavior; the new schema is intentionally incompatible with their `files` table.

## Non-goals for this assignment

- No one-time **legacy torrent migrator**, no Symfony user migration, no migration error export CLI yet (requirements documented below for the later workstream).
- No Go replacement web UI or PHP template port.
- No full `.torrent` reconstruction, no restoration of original file order.
- No optional search by filenames/paths; search **torrent names only**.
- No SQLite/CockroachDB/RabbitMQ/other persistence implementations; leave an appropriate small Go storage interface for future extension.
- No Sentry/Shoutrrr integration required; expose an optional minimal error/exception notification seam, keep it inactive by default.
- No Prometheus dependency required by this storage feature (preserve existing metrics code if it is already part of the fork).
- No storage repack CLI in 0.8.0.

## Work package A — design and handoff audit (do not repeat phases 1 or 2)

1. Confirm the existing `magnetico-web` feature branch, phase-1 pinned upstream SHA, phase-2 cleanup report (`docs/wip/upstream-cleanup-report.md`) and Go/toolchain version. Do not fetch or overwrite the branch as an upstream import.
2. Verify assumptions about `persistence.File`, DHT ingestion, BitTorrent v1 vs v2-only/hybrid metadata extraction, statistics, query filters, PostgreSQL schemas/search paths, error handling, and existing source database migrations.
3. In particular, **audit v2-only and hybrid extraction**. The existing `metadata/util.go` uses `info.Files`; do not claim v2-only support because BEP 52 types exist elsewhere. Classify actual behavior, add tests, and clearly mark any unsupported cases or necessary separately approved fixes.
4. Confirm that phase 2 already isolated or removed SQL references to `files`, filename search, file-count subqueries, file-based aggregate statistics and the automatic tgragnato `v0 -> v1` migration. If work remains, complete only the pieces necessarily replaced by new persistence; flag unexpected gaps.
5. Record the minimal implementation plan and how you will keep phase-2 build targets working; no large unrelated refactoring. Verify that the current source matches the prepared handoff rather than assuming an unmodified upstream tree.

## Work package B — codec v1

Implement an isolated Go package that accepts/returns the logical `[]File{Path string, Size int64}` and does not depend on PostgreSQL.

**Wire contract:**

- Header byte `0x01` followed by one byte containing serializer ID in the high nibble and compressor ID in the low nibble.
- Serializer IDs `1=flat`, `2=prefix-delta`, `4=directory-trie`; compressors `0=none`, `1=zstd`, `2=brotli`.
- If compressed, a **minimal `uvarint` of exact uncompressed payload length** follows the two-byte header. No length varint for uncompressed payloads.
- Each raw serializer encodes the global file count. Preserve byte-exact paths, duplicate entries, zero-length individual files, and stable `(Path bytes, Size)` sorting. Sort a copy; never mutate caller data.
- `flat`: per file path length + path bytes + file size. `prefix-delta`: maximal common **byte** prefix with previous path, suffix length + suffix bytes + size. `directory-trie`: ordered child directories, then ordered immediate files, with a leading global count; tree traversal may differ from global lexical ordering, so canonically sort decoded output.
- Directory-trie may be skipped for paths with empty components (leading/trailing slash or double slash). Do **not** normalize or rewrite paths to make trie usable. `flat` and `prefix-delta` must preserve these paths.
- Strict decoding: unsupported version/IDs, nonminimal varints, overflows, malformed structure, nonmaximal prefix, invalid canonical ordering, incorrect counts, trailing bytes/frames, truncated streams, resource limit violations -> typed error and **no partial success**.
- Separate resource limits for stored blob, uncompressed payload, file count, each full path, aggregate reconstructed paths, depth; include decompressor internal memory/concurrency considerations. Defaults: 32 MiB, 64 MiB, 250,000, 64 KiB, 64 MiB, 256 respectively. Never allocate based only on untrusted advertised lengths.
- Do not require recompressed Zstd/Brotli bytes to remain identical across library versions; retain pinned compressed **decode fixtures**.

**Profiles:** `balanced` = prefix + Zstd default with uncompressed fallback; `compact` (default) = compare prefix/trie + Brotli q9 and uncompressed fallback; `maximum` = prefix/trie + Brotli q11 and uncompressed fallback. Choose smallest **complete framed blob**, not only compressed payload. Use deterministic tie-breaking, and record the selected serializer/compressor in the header. A change in profile affects only subsequent writes; old blobs must remain decodable.

**Concurrency:** no mutable compressor shared unsafely. Prefer bounded encoder workers with per-worker state or reuse/pools via supported `Reset`, avoiding unbounded per-torrent high-quality Brotli allocation. Make the worker count configurable and measure peak memory.

**Tests (required):** exact golden vectors A–G from the spec, fuzz/property round-trip, duplicate paths, UTF-8 including byte prefixes ending inside a code point, suspicious path components, malformed encodings, compressed trailing junk, decompression bombs, count/sum overflow, profile switching, deterministic ordering/ties, no input mutation. Validate golden vectors independently, not by only comparing output to output from the same implementation.

## Work package C — PostgreSQL v1 schema and persistence

Build a **fresh** schema `magneticod` with:

- `torrents(id INTEGER GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, info_hash BYTEA UNIQUE NOT NULL, name TEXT NOT NULL, total_size BIGINT NOT NULL CHECK (>0), discovered_on BIGINT NOT NULL CHECK (>0), file_count INTEGER NOT NULL CHECK (>0))`;
- existing indexes on total size and discovery timestamp, plus `GIN (name gin_trgm_ops)`;
- `torrent_file_blobs(torrent_id INTEGER PRIMARY KEY REFERENCES torrents(id) ON DELETE CASCADE, data BYTEA NOT NULL)` (valid minimum length check permitted).

Preserve the identity of historical `torrents.id` in the *later* migrator, but **do not implement that migrator here**. Check that future explicit ID import and identity reseeding will work. Use schema-qualified SQL and check extension `pg_trgm` at startup; print an actionable `CREATE EXTENSION IF NOT EXISTS pg_trgm` hint and fail startup if absent. Never auto-create a huge legacy partitioned table or run `tgragnato` `v0 -> v1` on the source. New schema initialization and permanent schema-version tracking must be explicit, limited and testable, and must not silently mutate an existing legacy database.

Implement/adjust persistence operations:

- `AddTorrent`: check hash/name/count/sizes/UTF-8 and ingestion policies, use checked size summation; if there is exactly one file with equal `Path == torrent.Name` and `Size == torrent.TotalSize`, omit the blob, otherwise encode; do expensive encoding **before** beginning the DB transaction; atomically insert the torrent and optional blob. If a verified integration conflict makes no-blob impractical, the agent may disable it, but must explain the conflict and the trade-off explicitly in the implementation report.
- Concurrency-safe `ON CONFLICT (info_hash) DO NOTHING RETURNING id` or equivalent, with clearly defined `AlreadyExists` outcome. Never use unprotected `DoesTorrentExist` + insert as the correctness mechanism.
- `GetTorrent` and `QueryTorrents` return metadata including `file_count` without reading blobs. Name-only search/RSS/statistics/sort do not decode blobs.
- `GetFiles`: distinguish unknown torrent from valid empty/no-blob representation; if file_count=1 and no blob, reconstruct `(name,total_size)`; if file_count>1 and no blob, **integrity error**, never empty success; if blob exists, validate decoded count and total size, return canonical results or typed error (no partial data).
- Remove refactor-dependent joins against `files` and the file-path predicate in `QueryTorrents`, rebuild file count and statistics from metadata. Delete legacy partitioning and automatic v0->v1 migration code **from the new application path** before any deployment.
- A missing single-file blob may be a valid optimization, but losing an atypical single-file blob remains undetectable from the new schema alone; acknowledge in docs; future source-to-target verification must catch it.
- If supported backend modules need to be trimmed, avoid breaking unrelated crawler behavior and compile targets; expose one clean database interface without speculative backends.

**Telemetry:** `slog`/structured errors with `torrent_id` (when known), `info_hash`, operation, category, actual/expected values. On ingestion limit violations, log WARN and drop safely; on persisted storage corruption/invariant failures, log ERROR and return typed error. Throttle/aggregate repeated events. Do not log complete file paths by default. Keep an optional interface for later Sentry/Shoutrrr/ntfy delivery, no mandatory integration.

## Work package D — tests and CI suitability

- Go unit tests for all serializers and adaptive profiles; fuzz-safe decoding.
- PostgreSQL integration tests against disposable PostgreSQL 17+ (prefer Docker/ephemeral Woodpecker service), with deterministic fixtures. Check fresh schema creation, required-extension failure, duplicate concurrent infohash writes, transaction rollback if blob insert fails, FK/cascade behavior, missing/corrupt blobs, very large file counts, v1 single-file no-blob vs atypical single-file with blob, stats/search avoiding blob decode, unsupported schema version.
- Explicitly test that **starting the new process on an untouched legacy source DB does not trigger an automatic data conversion or partition creation**.
- Benchmarks of final framed codec and important `GetFiles`/`AddTorrent` paths; collect CPU, allocations, and peak RAM, but **do not** demand a new exploratory benchmark study if existing measurements suffice.
- Test with `go test -race` for concurrent relevant packages and a clean build. Provide exact reproducible commands and environment requirements; do not invent successful test results if tests cannot run.

## Later migration workstream — record now, do NOT implement here

The one-time offline migration will preserve old IDs, use transactional checkpoints and an error registry in `magnetico_migration`, provide progress with absolute counts/percent, throughput, elapsed and smoothed ETA (TTY UI, structured log output for non-TTY), and separate `verify-decode` and `verify-content` commands. Verification of all target blobs alone does **not** prove source-to-target equality; full source comparison remains the preferred cutover gate. `migrate users` is a separate command and source DB.

**New mandatory diagnostic export requirement from the owner:** error export must support an opt-in self-contained record dump, not only error messages:

```text
magnetico migrate errors export --format jsonl --output errors.jsonl
magnetico migrate errors export --format jsonl --include-records \
    --source-db-env MAGNETICO_MIGRATION_SOURCE_DSN --output failed-records.jsonl
# Optional larger forensic blobs (explicit opt-in only):
magnetico migrate errors export --format jsonl --include-records \
    --include-blobs --source-db-env MAGNETICO_MIGRATION_SOURCE_DSN --output failed-with-blobs.jsonl
```

These are **proposed CLI spellings**, not fixed if the existing CLI suggests a cleaner consistent design. `--include-records` means the actual old source torrent fields and complete old `(path,size)` file records, plus the corresponding new torrent fields and decoded metadata when available; include the failure stage, ID, infohash, category, attempted codec and detailed diagnostic context. `--include-blobs` includes the **exact raw new BYTEA** in a safe reversible encoding (e.g. Base64), even if decoding fails. When possible preserve byte-exact raw fields, marking non-UTF-8 through Base64 representation rather than lossy replacement. If source DB isn't accessible and source records are requested, fail clearly; ordinary error-only export must work without a source connection. Stream results with bounded memory and explicit per-record/output limits; never silently truncate forensic data. Provide a schema/manifest, schema+codec version, source/target context without passwords, SHA-256/checksums if appropriate, and safe file permissions. **Never export API tokens, DB credentials or secrets.** Diagnostic dumps may contain private torrent/file names: label them sensitive and do not attach/publish automatically. Export must be read-only and usable for agent-assisted post-mortem triage. Implement the export **only in the later migrator phase**.

Plan future `migrate ... retry-errors`, `migrate errors export`, `migrate cleanup`, and `storage verify`/`storage repack` separately. Do not implement user migration, export, or repack as a side effect of this task.

## Deliverables

1. Focused implementation commits for codec, new schema, persistence and tests **on the same `magnetico-web` development branch**, ready for review; no production cutover.
2. Agreed normative docs under `docs/specs/*.md` and a short update describing any required deviations or open questions.
3. A concise Markdown implementation report (`docs/wip/storage-implementation-report.md`) listing changed packages/files, verified invariants, test output and benchmark highlights, legacy behavior intentionally removed, known limitations and next workstream (torrent migrator). Include a short Mermaid diagram of the encoding/transaction path if useful.
4. A copyable list of exact test/CI commands and confirmation that no old-data migration was performed.

## Stop/acceptance criteria

The task is complete only when golden vectors and error cases pass, PostgreSQL integration tests prove transactional correctness, name-only metadata searches do not read `files`/blobs, original legacy auto-migration cannot fire from the new code path, and the implementation matches the approved wire format. If a discrepancy requires modifying the public contract, **stop at the design decision; do not invent a wire-incompatible patch**.
