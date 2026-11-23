# Task 07E / W4 — Search, torrent details and file browser: implementation plan

**Base:** `8a069dfd` on `refactor/0.8.0-unify-and-new-storage` (W1–W3 complete and reviewed).
**Inputs read:** `docs/specs/web-v1.md` rev. 5 (§§2.8–2.9, 3, 4.1, 6, 10, 12), `07A` §§4.3, 6, 7.3–7.5 and the W4 row, `wire-format-v1.md`, `postgresql-persistence-v2.md`, the W3 report, `persistence/{interface,postgres,validate}.go`, `codec/{codec,compress,prefix,trie,errors}.go`, and `internal/web/{server,auth,ui,webdb}`, `opflags/web.go`, `main.go:306-358`.
**Status:** approved by the owner at S0 (revisions 1–3 applied: statement-timeout budget, anchor cursor direction, admission-estimate wording, TOAST identity/header split, traversal-dependent links, `kids[]` packing, `pg_statio_user_tables`). Decisions O1–O7 are recorded in §11. Implementation proceeds in the steps of §10, stopping at S1, C1 and S2.

## Context

W1 built the HTTP foundation (including the `KindSearch` deadline class), W2 the schema and stores, W3 sessions and CSRF. A signed-in user still sees only a placeholder at `/`. W4 delivers the product's core read path:

- authenticated search and listing (`GET /torrents`);
- torrent details (`GET /torrents/{id}`);
- a file browser (`GET /torrents/{id}/files`).

W4 is also where the file-tree memory risk is measured. Display limits (500 rows) do not limit decode memory (up to 250,000 files and 64 MiB), so the design is gated by a review checkpoint (C1) before the cache and handlers are built.

---

## 1. Current implementation findings

Verified in the code on this branch.

**Persistence (`persistence/`)**

1. `Store.GetTorrent` and `Store.GetFiles` take the **info hash**, but the routes carry the **integer `id`**. There is no read by ID. (`interface.go:89,94`, `postgres.go:240,254`)
2. `GetFiles` joins the blob and runs `codec.Decode` **inline** with `s.limits`, then checks count and sum. It cannot be used as-is: it has no weighted decode semaphore (spec §6.4.3), and it fetches the whole blob before any admission decision. (`postgres.go:254-301`)
3. `QueryTorrents` has one `SearchString`, two orderings (`discovered_on DESC`, `total_size DESC`), `LIMIT` only, and no cursor, no AND of terms and no name sort. Its only callers are persistence tests, so it stays untouched. (`postgres.go:303-356`)
4. `escapeLikePattern` already escapes `\`, `%` and `_`, but is unexported. (`postgres.go:360`)
5. `pgxStore` owns its own pool and an encoder. The web service has its own bounded pool (`webdb.Backend.Pool()`), so W4 needs a read-only type over a caller-owned pool, like the existing `persistence.VerifySchema(ctx, pool)`.
6. The schema has `idx_torrents_total_size`, `idx_torrents_discovered_on`, and a GIN trigram index on `name`. There is no composite index and no index that supports name ordering.
7. Ingest rejects invalid UTF-8 and NUL in names and paths (`validate.go:44,57`). Tests that need invalid-UTF-8 paths must therefore insert blobs by SQL, not through `AddTorrent`.

**Codec (`codec/`)**

8. `Decode` returns the whole `[]File`. There is no streaming API and no context. Cancellation can only happen at phase boundaries.
9. Allocation sources visible in the source:
   - `decompressExact` allocates the declared length up front (≤ 64 MiB) (`compress.go:133`).
   - `decodePrefix` creates one new string per path and grows the slice by `append` (`prefix.go:53,107,120`).
   - `trie.node` does `files = append(files, childFiles...)` at **every level**, so each file is copied once per ancestor (O(n × depth)) (`trie.go` near the end of `node`).
   
   These are candidate bottlenecks for the "allocations ≤ 3× uncompressed" target. They are measured, not changed.
10. Existing benchmarks (`BenchmarkDecode`) stop at 50,000 files. Nothing measures 250,000 files, an index, concurrency or RSS.

**Web (`internal/web/`)**

11. `Router.Handle(pattern, Kind, Access, handler)` is the single registration point. `KindSearch` already gives handler context = `SearchTimeout` and write deadline = `SearchTimeout + 10 s` (`middleware.go:217-263`). W4 inherits the HTTP-deadline correctness of §2.9.
12. `WebOptions.SearchConcurrency` (1 ≤ n ≤ pool − 2) and `PoolMaxConns` are validated, but **nothing consumes `SearchConcurrency`** and `main.go` does not pass it on.
13. `webdb.Open` sets only `MaxConns`. There is no statement timeout.
14. `auth` has no exported session form-token accessor: `sessionCSRF` is unexported. The W3 plan promised `auth.SessionFormToken(r)`; W4 adds it for the logout form in the page chrome.
15. `GET /{$}` is the interim W3 homepage. `ui.Renderer` has only the `asset` template function. `layout.html` has no navigation.
16. Pagination `pgx` behaviour: on context cancel, pgx v5.11 marks the connection closed, sends a cancel request asynchronously and closes the socket (`pgconn.go:768-785`). A cancelled search therefore costs a reconnect, and the server-side `statement_timeout` is the backstop.
17. The `pgtest` cluster uses the default `initdb` or Docker locale. Non-ASCII `ILIKE` behaviour depends on `lc_ctype`, so tests must record the locale and branch on it.
18. `SearchConcurrency`'s upper bound (pool − 2) plus a single shared pool is how the spec keeps auth alive. W4 relies on that and adds a rule: never hold a pool connection while waiting on a semaphore or decoding.

**Discrepancies between documents (not silently resolved; see §11)**

| # | Discrepancy | Plan default |
|---|---|---|
| X1 | 07A §4.3 lists persistence additions; the spec adds that decoding must be bounded by a weighted semaphore. The existing `GetFiles` cannot satisfy this | Split into a metadata read, a blob fetch and an exported `DecodeFileList`; refactor `GetFiles` onto it (behaviour-preserving) |
| X2 | 07A §6.4 says the decode semaphore is "weighted by stored blob size". Stored size is a poor proxy (Brotli expands 5–10×; prefix-delta paths expand more) | Weight by an upper bound on peak memory (§7.2) |
| X3 | 07A §6.4: "every directory is a contiguous range". True for subtree membership, but **sibling display order differs from range order** (`a.txt` < `a/x`; `a-b/x` < `a/x`) | The index sorts siblings explicitly (§6.2) |
| X4 | Spec §6.1 has a separate `GET /torrents/{id}/files`; the 07A mock-up shows files on the details page | Follow the spec: details is metadata-only (no blob), files are a separate page |
| X5 | Spec §8.3.1 says the details page MUST show the short link; `/magnet` and the tracker snapshot are milestone W7 | Interim: info hash and a tracker-less magnet URI on details; short link and trackers arrive in W7 (O7) |
| X6 | Spec §6.2.5 calls the 60 s the *SQL* timeout; §2.9 fixes the handler context at exactly `SEARCH_TIMEOUT` | Queue wait and SQL share the one budget; `statement_timeout` = remaining budget (§5.2) |
| X7 | Spec §6.3.1 gives no direction for `size`/`name` and no `order` for the UI; 07A mock-up has `aria-sort` header links | Support both directions (O4) |
| X8 | Spec §6.1: `/` redirects to `/torrents` when signed in. W3 left an interim page | W4 makes `GET /{$}` redirect (W3 tests updated) |

---

## 2. Scope and non-goals

**In scope:**

- `GET /torrents` (search, listing, sorts, keyset pagination); `GET /torrents/{id}`; `GET /torrents/{id}/files` (directory browsing and pagination).
- Read-only persistence additions: no schema change, no new index.
- Search timeout (context plus server-side statement timeout), bounded search concurrency, friendly busy / timeout / invalid-query responses.
- Weighted decode gate, compact in-memory directory index, size-bounded cache with single-flight.
- Lossless rendering of unusual paths and invalid UTF-8.
- Minimal functional server-rendered HTML (§4.3); `GET /{$}` redirect.
- Synthetic benchmarks, `EXPLAIN` review, report, and a `GOMEMLIMIT` recommendation.

**Non-goals:** styling or UX (W5), account pages (W6), API v2, RSS, `/magnet` and the tracker snapshot (W7), user migration (W8), new indexes, codec or schema changes, new configuration variables (constants only, O9), production or legacy database access, public DHT traffic.

---

## 3. SQL and persistence design

### 3.1 New code in `persistence` (additive, read-only; the crawler's `Store` interface is not modified)

```go
// webread.go
type Reader struct{ pool *pgxpool.Pool; limits codec.Limits }
func NewReader(pool *pgxpool.Pool, limits codec.Limits) *Reader

func (r *Reader) TorrentByID(ctx, id int32) (Torrent, error)              // ErrTorrentNotFound
func (r *Reader) TorrentByInfoHash(ctx, h []byte) (Torrent, bool, error)   // exact lookup for the 40-hex query
func (r *Reader) SearchTorrents(ctx, SearchSpec) (SearchPage, error)

// File list, in three steps so identity, admission and the large read are separate:
func (r *Reader) FileIdentity(ctx, id int32) (FileIdentity, error)   // cheap: row + blob length/xmin, NO header, NO blob
func (r *Reader) FileHeader(ctx, id int32) ([]byte, error)           // cache miss only: first 16 bytes of the blob
func (r *Reader) FileBlob(ctx, id int32, ver string) ([]byte, error) // errors if version changed

func DecodeFileList(t Torrent, blob []byte, l codec.Limits) ([]File, error) // codec.Decode + count/sum checks
```

- **Identity and header are separate statements** (revision 2). `FileIdentity{Torrent; HasBlob bool; BlobLen int; Version string}` comes from
  `SELECT t.*, b.torrent_id IS NOT NULL, octet_length(b.data), b.xmin::text FROM torrents t LEFT JOIN torrent_file_blobs b ON b.torrent_id = t.id WHERE t.id = $1`.
  It runs on every files-page request, including cache hits. `octet_length` on a TOASTed `bytea` should be answerable from the TOAST pointer (raw size) without detoasting, **but this is an assumption to be measured, not relied on** (below).
- `FileHeader` (`SELECT substring(data from 1 for 16) …`) runs **only on a cache miss**, to compute the admission weight. For a compressed or external TOAST value `substring` may need to fetch (and decompress) only the first chunk (`bytea` columns use `EXTERNAL`-style slicing when stored uncompressed, but compressed values may need decompression of a prefix); again measured, and it is off the hit path by construction.
- **Measurement before commitment (S1):** a focused benchmark / script against a disposable database holding a large (≈ 20–32 MiB, incompressible and compressible variants) TOASTed blob, comparing, over repeated runs (warm and cold `shared_buffers`): (a) `FileIdentity` as written, (b) the same without `octet_length` (fallback: identity from `file_count`/`total_size`/`xmin` only), (c) `FileHeader`, (d) `FileBlob`. Record latency, `EXPLAIN (ANALYZE, BUFFERS)` (look for TOAST buffer reads) and `pg_statio_user_tables.toast_blks_hit` / `toast_blks_read` deltas (the I/O view, not `pg_stat_user_tables`). If (a) touches TOAST pages or costs more than a bounded fraction of the 50 ms cached-navigation target, the identity drops `BlobLen` and relies on `xmin` (+ the 10-minute age); the plan records the evidence either way.
- No-blob shape: `HasBlob=false` and `file_count==1` give the synthetic single `File{name, total_size}`; `file_count>1` gives `ErrIntegrityMissingBlob`. This is the existing `GetFiles` logic, moved, not changed.
- `DecodeFileList` holds the exact count / checked-sum / sum-equals-`total_size` verification now inside `GetFiles`. `pgxStore.GetFiles` is refactored onto it. The existing persistence tests prove that nothing changed.
- The header (`FileHeader`, miss path only) lets the caller learn the declared uncompressed length without reading the blob. This needs a small **additive** exported codec helper (O3):
  `codec.PeekDecodedSize(header []byte, storedLen int) (serializer, compressor, declared uint64, err)`. It wraps the existing envelope parse and changes no behaviour. If the owner prefers not to touch the codec, the fallback is a pessimistic weight (§7.2).
- `Version` is the blob row's `xmin` as text. Blobs are immutable in normal operation, so it is a cheap identity token. If it changes (a freeze can also change it), the cost is one extra decode. `FileBlob` re-checks it and returns a retryable `ErrBlobChanged`.
- **Every Reader statement** runs as `BEGIN READ ONLY; SET LOCAL statement_timeout = '<ms>'; …; COMMIT`, where `<ms>` is derived from the context deadline (§5.2). The value is an integer formatted by Go (never user input). The tail of the transaction is one pgx batch round trip; the cost is negligible next to a search.

### 3.2 Search parsing (`internal/web/search`, pure)

- Trim, then split with `strings.Fields` (Unicode white space, so NBSP also separates). Document this as the reading of "split on whitespace".
- Reject invalid UTF-8 and NUL in the query with `ErrInvalidQuery`. PostgreSQL would refuse them with an error, so they must not become a 500. They are never "repaired".
- Drop terms shorter than 3 code points (`utf8.RuneCountInString`). If no usable term remains, the listing is the newest torrents, and the UI shows "words shorter than 3 characters were ignored".
- Caps (O6, not in the spec): query ≤ 512 bytes and ≤ 8 usable terms; beyond that, `ErrInvalidQuery` ("query too long").
- After trimming, if the whole query is exactly 40 hex characters (case-insensitive), it is an **info-hash lookup**: `WHERE info_hash = $1` (unique index), zero or one row, no sort, no cursor. A 40-hex word together with other words is an ordinary term.
- Sort: `discovered` (default), `size`, `name`. `name` without a usable term falls back to the default sort, like any unknown value (spec §6.3.1). Order: `order=asc|desc`. Defaults: discovered desc, size desc, name asc. Page size: 20, 50, 100, default 50; anything else falls back to the default.

### 3.3 Keyset SQL

For sort column `c ∈ {discovered_on, total_size, name}` and effective direction `d`. The tie-breaker `id` always uses the **same** direction as `c`, so row-value comparison is valid.

```sql
SELECT id, info_hash, name, total_size, discovered_on, file_count
  FROM magneticod.torrents
 WHERE name ILIKE $1 AND name ILIKE $2 …          -- 0..8 terms, pattern '%'||escape(term)||'%'
   AND (c, id) < ($a, $i)                         -- '>' when d = ASC; omitted on the first page
 ORDER BY c d, id d
 LIMIT $n                                         -- page size + 1
```

- All user data are bind parameters; the SQL text depends only on (term count, sort, direction, cursor present).
- **Backward** navigation uses the reversed direction and the same row comparison, fetches `n+1`, and reverses the slice in Go.
- **No `COUNT(*)`, no `OFFSET`.** `More = (rows == n+1)`.
- `BuildSearchSQL(spec) (string, []any)` is a pure function so a unit test can assert the properties above (spec §12.7).
- **Index use** (to be verified with `EXPLAIN`, not assumed): the planner can use the leading column of a row comparison against a single-column btree, and `idx_torrents_discovered_on` plus an incremental sort on `id` serves the default ordering. S1 compares the row-comparison form with the expanded form `(c < a OR (c = a AND id < i))` and keeps the one with the better measured plan. **If a plan proves unacceptable, the plan stops and asks about an index; none is added without evidence and approval (spec §6.2.6).**
- Plan expectations to be confirmed by S1/benchmarks: the newest listing and `info_hash` lookups are index scans with `LIMIT`; a rare term uses the trigram bitmap; a very common term and `name` / `size` sorts over broad matches can scan widely. The statement timeout bounds that.

### 3.4 Cursor (opaque, in `search`)

`base64url( v1 | kind('f'|'b'|'F'|'B') | ctx-hash(8) | sortKey | id(4) | crc32(4) )` with `sortKey` = 8-byte integer for `discovered` / `size`, the length-prefixed name bytes for `name`, or (anchor kinds `F`/`B`) empty.

- **Direction is always explicit** (revision 2): lower case `f`/`b` = value cursors (forward / backward); upper case `F`/`B` = anchor-only cursors (forward / backward). The direction is part of the checksummed payload, so it cannot be lost or flipped.

- `ctx-hash` = hash of (sorted unique lower-cased terms, sort, direction). A cursor used with a different query is rejected (`ErrInvalidCursor`, 400 "this page link no longer matches your search"). Page size is deliberately outside the hash.
- Cursors are **not signed**: they carry no authority, only a position, and every field is validated and bound as a parameter. Forging one lets a signed-in user pick a starting point, nothing more.
- Maximum 4 KiB of name bytes in a value cursor. For a boundary row with a longer name, an anchor cursor (`F`/`B`) carries the anchor `id` only and the SQL looks the anchor up. It uses the **same** effective direction, operator, `ORDER BY` and Go-side reversal as the value cursors; only the source of `(c, id)` differs: `WHERE (name, id) < (SELECT name, id FROM torrents WHERE id = $i)` (operator `<` / `>` chosen by the effective direction, exactly as in §3.3). The query shape is otherwise identical, so one builder handles both.
- **Deleted anchor:** if the subselect finds no row, the row comparison would silently become NULL and return nothing. The builder therefore fetches the anchor in a preceding statement of the same read-only transaction (or `WITH a AS (…) … JOIN a`) and treats zero rows as `ErrCursorExpired` (400 "this page link has expired, start again"), never as an empty page.
- Tests: oversized-name (> 4 KiB) keys in both directions (`F` and `B`) across `asc` and `desc`, walking a full set forward and backward with no duplicates or gaps; a deleted anchor in both directions → `ErrCursorExpired`; a flipped direction byte or bad CRC → invalid cursor.
- **Link availability depends on the traversal direction** (revision 3). Let `cursor` = the request carried an input cursor and `extra` = the `n+1`-th row was returned (then dropped):

  | Traversal | Next link | Previous link |
  |---|---|---|
  | Forward (`f`/`F`) or no cursor | `extra` | `cursor` |
  | Backward (`b`/`B`) | `cursor` (the anchor row itself lies ahead) | `extra` |

  - Next = `f`/`F` cursor of the **last** row of the page (after backward results are reversed into display order); Previous = `b`/`B` cursor of its **first** row.
- **Test — return trip:** page 1 → Next → page 2 → Previous → must equal page 1 exactly → Next again → must equal page 2. Run for every sort × direction, with ties on the sort key, and with a row inserted at the head between the steps (no row duplicated or lost across the walk).

### 3.5 Collation and concurrent inserts

- `ILIKE` folding depends on the database `lc_ctype`: in a `C` locale non-ASCII letters do not fold. This is documented behaviour (07A §6.1), and tests record `datcollate`/`datctype` and branch (O5 note).
- `ORDER BY name` uses the column's collation. `ORDER BY` and the row comparison share it, so keysets are self-consistent. Deterministic collations tie-break bytewise. Order may differ between servers, and a `glibc` upgrade can reorder (documented). `COLLATE "C"` is faster and fully deterministic but sorts by byte value (O5).
- **Inserts during paging:** each page is its own `READ COMMITTED` statement. Moving forward never duplicates or skips a row that existed before the walk began; new rows appear only on the side of the cursor where they sort. With `discovered_on DESC` they appear on earlier pages. A row committed late with an older `discovered_on` (out-of-order commits) can be missed: inherent to keyset paging, documented, acceptable.

---

## 4. HTTP routes and minimal HTML

### 4.1 Routes (all registered through `Router.Handle`; all `Session` class)

| Route | Kind | Behaviour |
|---|---|---|
| `GET /torrents` | `KindSearch` | `q`, `sort`, `order`, `size`, `cursor`. Renders the list, or an empty state, or the 400 / 503 pages below |
| `GET /torrents/{id}` | `KindRequest` | Torrent row only (no blob). `{id}` must be canonical decimal int32 ≥ 1; anything else is 404 |
| `GET /torrents/{id}/files` | `KindRequest` | `dir` (exact path prefix; empty = root), `from` (position), fixed 500 rows per page |
| `GET /{$}` | `KindRequest` | Signed in → `303 /torrents` (replaces the W3 interim page) |

Non-GET methods return 405 from the mux. Anonymous requests get the W3 `303 /login?next=…` behaviour.

### 4.2 Response mapping (distinguishable by status **and** text)

| Situation | Status | Notes |
|---|---|---|
| Invalid query / invalid cursor | 400 | Form re-rendered with an inline message; never "timeout" wording |
| Search timeout | 503 | "The search did not finish in the configured time and may be retried." Never claims the query is invalid or too broad. `Retry-After: 30` |
| Search or decode queue busy / full | 503 | "The server is busy; try again shortly." `Retry-After: 5` |
| Client cancelled | none | Debug log only |
| Unknown torrent | 404 | Generic page |
| `ErrIntegrityMissingBlob`, `ErrCorruptBlob` | 500 | `ERROR` log: `torrent_id`, category and `LimitError` fields; no paths, names or queries. No partial list |
| `ErrStorageIO`, pool or context failures | 503 | `Retry-After`; never rendered as an empty result |

Logs never contain query text, terms, names or paths (spec §10.2). A structured search record carries the outcome, term count, sort, row count and duration.

### 4.3 Minimal HTML (templates `torrents`, `torrent`, `files`; page chrome in `layout`)

- **Chrome:** link to `/torrents` and a logout form (POST, session CSRF token through the new exported `auth.SessionFormToken(r)`).
- **List:** search form (`q`, sort and page-size selects); a table or list of name (`<bdi>`), size (IEC), file count, discovered (UTC `<time>`); links "Newer" / "Older" carrying cursors; a hint about the 3-character minimum; empty, busy and timeout states. **No total count.**
- **Details:** name, size (human and bytes), file count, discovered, info hash (hex), a tracker-less `magnet:?xt=urn:btih:<hash>&dn=<name>` link (interim, X5), and a "Browse files" link.
- **Files:** breadcrumbs; per directory a page of entries (directories first, then files), each with name, size, and for directories file count and aggregate size; Prev / Next by position; duplicate-path markers; explicit markers for empty components.
- **Path rendering:** a typed segment list, no `template.HTML`. Invalid UTF-8 bytes are `\xNN` in a marked span; C0/C1 controls, bidi overrides and isolates (U+202A–202E, U+2066–2069, U+200E/F), U+2028/2029 and a real U+FFFD are shown as `\u{XXXX}` markers. ZWJ and other format characters are left alone so emoji still render. U+FFFD is never substituted.
- **Directory links** use a query parameter (`?dir=` + `url.QueryEscape` of the raw bytes), so dot segments (`.`, `..`) are never normalised by a browser or the mux, and arbitrary bytes round-trip.

**Left to W5:** all styling and layout, theming, responsive behaviour, JS (copy buttons, in-place expansion), icons, accessibility audit, and the details-page presentation of files.

---

## 5. Search concurrency and timeouts

### 5.1 Resource ordering rule

Acquire the **gate first, then a pool connection, and release the connection before any waiting or CPU-heavy work.** No code path holds a pool connection while it waits on a semaphore or decodes. With `SearchConcurrency` K ≤ pool − 2 (validated in `opflags`), at least two connections are always available to sessions, details, blob fetches and the purger. `main.go` starts passing `SearchConcurrency` through.

### 5.2 Budget and timeouts

- Handler context deadline = `SEARCH_TIMEOUT` (W1, unchanged). It covers **queue wait + SQL**.
- **Statement timeout never exceeds the remaining budget** (revision 2). After the gate is acquired (and again immediately before each statement), compute `remaining = time.Until(ctx.Deadline())` and `margin = clamp(remaining/10, 50 ms, 500 ms)`.
  - If `remaining ≤ margin + minStatement` (`minStatement` = 100 ms), **do not start the SQL statement**: release the permit and return the search-timeout outcome directly.
  - Otherwise `statement_timeout = remaining − margin`, which is strictly positive and strictly below the remaining deadline. There is no `max(1 s, …)` floor.
  - The server-side timeout then fires **before** the context does, so PostgreSQL cancels the statement cleanly and the pooled connection stays usable.
  - The same helper serves every Reader statement (details, `FileIdentity`, `FileHeader`, `FileBlob`); an exhausted budget there maps to the 503 path.
- Tests (injected clock for the budget computation, plus a real gate): remaining 10 s, 600 ms, 150 ms, 60 ms and ≤ 0 after a queue wait that consumed nearly the whole budget; assert no statement is issued when it cannot fit (counting fake / `pg_stat_statements`-free check via a recording wrapper), the timeout outcome is returned, the permit is released, and for the fitting cases the timeout is > 0 and < remaining.
- The context remains the second line of defence. pgx closes the connection on a context cancel (finding 16), and PostgreSQL receives the cancel request.
- Write deadline (W1) = timeout + 10 s margin; rendering a timeout page is always possible. Test with a short general timeout and a longer search timeout (spec §12.7).

### 5.3 Gate (`internal/web/gate`, stdlib, ~60 lines)

A weighted semaphore with **FIFO waiters, a bounded waiter count and a bounded wait**, honouring the context. Weight 1 for search; bytes for decode (§7). It returns typed `ErrBusy` (queue full or wait expired) and the context's error otherwise. Constants (O9):

| Constant | Search | Decode |
|---|---|---|
| Capacity | `SearchConcurrency` (default 4) | 256 MiB |
| Max wait | 5 s | 5 s |
| Max waiters | 4 × capacity | 32 |

`golang.org/x/sync/semaphore` would promote an indirect dependency and has no waiter bound, so the in-house gate is preferred.

### 5.4 Error classification (in `search.Service`)

- `ErrBusy` → busy page.
- SQLSTATE `57014` while the request context is **not** done → timeout.
- `context.DeadlineExceeded` → timeout.
- `context.Canceled` → client cancellation: no response, debug log.
- Anything else → `ErrStorageIO` → 503.

### 5.5 Testing time-dependent behaviour without 60 s waits

- **Handler / service level:** fake `Searcher` that blocks until its context ends; configurable `SearchTimeout` (300 ms), queue wait (100 ms), `K=2`. Covers saturation, timeout page, busy page, cancellation and the permit being released.
- **Database level (deterministic, no data volume):** a second connection takes `ACCESS EXCLUSIVE` on `magneticod.torrents`, so the real search statement blocks. Run it with a 100 ms `statement_timeout` and assert SQLSTATE 57014 mapped to timeout. Cancel the context and assert prompt return plus no waiting backend in `pg_stat_activity`. Release the lock and assert that the pool recovered.
- **Starvation:** real pool of 4 with `K=2`; hold two searches on the lock; a session lookup and a login still complete within a bound (spec §12.7).
- **Cancellation backstop:** verify with `pg_stat_activity` that a cancelled query's backend ends (pgx sends a cancel request); `statement_timeout` is the backstop.

---

## 6. File-tree indexing and cache design

### 6.1 Flow (details in §6.2–6.4)

`FileIdentity` (cheap, every request) → cache lookup by identity → on a miss, a **single-flight** per torrent runs: `FileHeader` → gate admission by estimated weight → `FileBlob` → `DecodeFileList` → `BuildIndex` → cache insert. The connection is released after the blob fetch. Requesters wait on the flight or on their own context.

### 6.2 Index (`internal/web/filetree`, pure; no HTTP, no SQL)

Built from the canonical `[]File` in two phases (scan, then pack), retaining **no per-node heap objects**. `kids` is *not* filled by one append-only traversal (a parent's children are discovered interleaved with its descendants', so they are not contiguous during the scan); see "Construction" below.

- `arena []byte`: component bytes (directory names, file base names), copied so the decoded strings can be freed.
- `files []fileRec{off, n uint32; size int64}` (16 B) in **canonical order**. The file's position is its identity, so duplicates (same path, same or different size) stay separate and adjacent, and positional cursors are stable.
- `dirs []dirRec{parent, nameOff, nameLen, files uint32; size int64; kidLo, nDirs, nFiles uint32}` with directory 0 as the root.
- `kids []uint32`: per directory, first its subdirectory ids **sorted by name bytes**, then its file ids in canonical order, as one contiguous block per directory.

**Construction:**
1. **Scan** (one pass, stack of open directories as working state): split each path on `/`, pop to the common prefix, push new components. Each new directory gets a `dirs` record (parent, name copied to `arena`) in first-appearance order; each file gets a `files` record plus a temporary parent id; per-directory `nDirs`/`nFiles` counters and recursive `files`/`size` aggregates (checked add) accumulate. `kids` is not touched.
2. **Pack:** prefix-sum `nDirs+nFiles` to assign each directory its `kidLo` block. Child directory ids are gathered per parent, **sorted by name bytes** and written at the start of the block (first-appearance order is `name+"/"` order, so `a-b/` precedes `a/`); then one pass over `files` in canonical order appends each file id to its parent's block, preserving canonical order and duplicates.
3. Free the temporaries; they count toward transient decode weight, not retained cache size.
- Tests: a 100k-child directory, `a-b/` vs `a/` vs `a.txt`, same-name file and directory, empty components, and a map-based reference listing compared with the index on random path sets.

**Why siblings are sorted explicitly (X3):** the subtree of directory `a/` is a contiguous range of the canonical order (all strings sharing a prefix are contiguous), but the *order of siblings* inside that range is not by name (`a.txt` < `a/x`; `a-b/x` < `a/x`). A file `a` and a directory `a/` are distinct entries; both are listed.

- **Components:** a path is split on `/` for display grouping only. The empty component (`a//b`, leading `/`) is a real directory with an empty name. A directory is addressed by its exact prefix string through the trailing `/`; the root is `""`. A prefix `p` resolves by splitting `p[:len-1]` on `/` and walking the sorted child lists by binary search; `"/"` is the directory with an empty name under the root, so it never collides with the root. `.` and `..` are literal names.
- **Aggregates:** file counts and sizes per directory are recursive. They use checked addition. The total is already verified against `torrents.total_size` and sizes are non-negative, so partials cannot overflow; the check stays as a defence (spec §6.4.5).
- **Pagination:** the list of a directory is `kids[kidLo : kidLo+nDirs+nFiles]`; the page is `[from, from+500)`. Duplicate markers compare each file with its predecessor in the same directory, including across a page boundary. `from` is validated against the length.
- **Accounting:** `Index.Bytes()` = exact sum of the slice capacities plus fixed overhead.
- **Alternative to be measured at C1:** keep the decoded `[]File` and index only offsets. That avoids copying the arena but retains per-string headers and full path bytes, so it retains more. The prefix-delta decode produces one new string per path either way.

### 6.3 Cache (`filetree.Cache`)

- **Key** = torrent id; **identity** = (id, `file_count`, `total_size`, blob length, blob `xmin`). A hit requires identical identity; otherwise the entry is replaced. Maximum age 10 min bounds staleness from anything the identity cannot see.
- **LRU by bytes**, budget 128 MiB (constant). Entries above budget are not cached and are served once, with a logged counter, so repeated decodes are explicit and never silent (spec §6.4.3). With the codec's hard limits the worst-case retained index must stay below the budget; C1 verifies this with measurements.
- **Single-flight** per torrent: the flight has its own context, cancelled only when **all** waiters have left **before decode starts**. Once the (uninterruptible) decode is running it completes and its result is cached even if every requester has gone, because the work is already paid for. `BuildIndex` checks the context periodically.
- **Failure handling:** errors are delivered to every waiter and **not cached** (no negative cache in W4; revisit after the W9 soak). A failed or cancelled flight leaves no entry and releases its gate weight.
- **Shutdown:** flights do not touch the database after the blob fetch, and requests are drained by the W1 grace logic before the pool closes.

### 6.4 Request path on the files page

1. `FileIdentity` (one cheap statement; no header, no blob). 404 / 500 mapping happens here.
2. No-blob single-file torrents skip the cache: a synthetic one-entry list.
3. Hit: `dir` resolution, page, render. No decode, no blob read.
4. Miss: single-flight as in 6.1. Gate `ErrBusy` → busy page.

---

## 7. Resource limits and memory accounting

### 7.1 Constants (no new configuration variables, O9)

| Item | Value | Source |
|---|---|---|
| Display page size (files) | 500 | spec §6.4.3 (proposal) |
| Decode gate capacity | 256 MiB | spec §6.4.4 (proposal) |
| Cache budget | 128 MiB | spec §6.4.4 (proposal) |
| Decode and search queue wait | 5 s | spec §6.4.4 |
| Codec limits | `codec.DefaultLimits()` (32 MiB stored, 64 MiB uncompressed, 250,000 files, 64 KiB path, 64 MiB aggregate paths) | wire-format §5 |

### 7.2 Decode weight model (decided at C1, O1)

Peak transient memory of one cold request:

`W = stored_len + declared_uncompressed + 24·file_count + P + index_overhead`

where `P` is the retained path byte volume. `P` **cannot be derived** from the declared payload length: prefix-delta payloads are small while the reconstructed paths are large. The only hard bound is `MaxAggregatePathBytes` (64 MiB).

- **Initial model = a conservative admission estimate, not a proven peak-memory bound** (revision 2). `P = min(file_count × MaxPathBytes, 64 MiB)` bounds only the *retained* path bytes. The model ignores transient garbage (slice growth by `append`, the O(n × depth) trie copies, strings freed only at the next GC) and Go heap overhead, so actual peak can exceed `W`. Nothing in W4 claims otherwise; the benchmark reports measured peak versus `W` for every shape.
- **When the calculated `W` exceeds gate capacity (256 MiB)** (revision 2): the request is **not** silently clamped and admitted as "safe". The rule is:
  1. Compute `W` unclamped. The codec limits (32 MiB stored + 64 MiB declared + 24 × 250,000 + 64 MiB paths + index overhead) put the worst case near 170–180 MiB, which fits; the rule is for configurations or blobs where it does not.
  2. If `W ≤ capacity`: admit with weight `W`.
  3. If `W > capacity`: **reject without reading the blob** with a typed `ErrTooLargeToIndex`, rendered as a 500-class "this file list is too large to browse" page with an `ERROR` log (`torrent_id`, `W`, capacity; no paths). A blob can only get here if limits were raised above the defaults, so it is a configuration invariant violation, not a routine path. A startup assertion checks `W_max(codec limits) ≤ capacity` and fails startup otherwise.
  4. Tests: weights just below, at and just above capacity; the over-capacity request issues no blob fetch; the startup assertion trips for raised limits.
- Small torrents weigh little; any torrent of ≥ ~1,000 files weighs ~64 MiB more, so at most about three large cold decodes run at once.
- **At C1:** measure the real `P/declared` ratio on realistic shapes. If the pessimistic weight makes concurrency unusable, the owner may choose an *estimated* weight (factor `k` from measurement) with `GOMEMLIMIT` as the backstop.
- **Without the codec peek (O3):** `declared` is unknown; use `stored_len` for uncompressed blobs and the 64 MiB cap for compressed ones. This is strictly more pessimistic.

### 7.3 Memory arithmetic and `GOMEMLIMIT`

The in-flight budget is decode gate (256 MiB) + cache (128 MiB) + runtime and request baseline + GC headroom. Go's heap can reach about twice the live set at `GOGC=100`, so the target of ≤ 640 MiB RSS in a 1 GiB container is not guaranteed by the budgets alone. The benchmark procedure (§8) runs the 32-concurrent workload under several `GOMEMLIMIT` values and reports `VmHWM`. **No number is claimed before measurement**; the report ends with a recommended `GOMEMLIMIT` for the container, with the evidence.

---

## 8. Benchmarks and measurement procedure

Fast tests run in the normal suite. Expensive work is **explicitly invoked**: Go benchmarks (never run by `go test`) and an env-gated load test (`MAGNETICO_BENCH_LARGE=1`). PostgreSQL-backed benchmarks skip without a `pgtest` cluster.

### 8.1 Synthetic data (`internal/web/synth`, seeded, deterministic, test-support only)

- **File lists:** flat; deep tree (depth ~8); realistic mix; adversarial (duplicates, empty components, invalid UTF-8, one directory with 100k children, very long components). Sized to ≈ 250,000 files and ≈ 64 MiB uncompressed. Encoded with `codec.Encode` (compact profile) for realistic blobs, and with trie / prefix / flat variants to expose serializer differences.
- **Torrent table** (for `EXPLAIN` and search benchmarks): 1M and 5M rows by `COPY` (not `AddTorrent`). Names from a seeded Zipf-like vocabulary giving common and rare terms. Mixed sizes and monotonically increasing `discovered_on` with jitter. Blobs only for the handful of large torrents. `ANALYZE` after load.
- No real, production or network-sourced data. The report records the generator seed and parameters.

### 8.2 Measurements against spec §6.4.4 (targets, not gates)

| Target | How |
|---|---|
| Cold decode + index, ~250k files, p95 ≤ 2 s, allocations ≤ 3× uncompressed | `BenchmarkColdBuild` (`-benchmem`; `runtime.MemStats` deltas), per serializer and shape, reported separately for decode and index build |
| Cached navigation p95 ≤ 50 ms / p99 ≤ 150 ms, ≤ 10,000 allocs and ≤ 1 MiB per request | HTTP-level handler benchmark against a warm cache, plus latency histogram |
| 100 navigations → exactly 1 decode | counted in a test (decode counter), not a benchmark |
| 8 / 32 concurrent, mixed cold/cached | env-gated load test: workers, mixed torrent set, success / busy / error counts, p50/p95/p99 |
| Peak RSS ≤ 640 MiB at 32 concurrent | `VmHWM` from `/proc/self/status`; repeated for several `GOMEMLIMIT` values |
| Search plans | `EXPLAIN (ANALYZE, BUFFERS)` for: newest first page, deep cursor page, backward page, rare term, common term, rare + common, all-common AND, size sort + term, name sort + term, info hash. Both row-comparison forms. Cold and warm cache |

### 8.3 Reporting

The report states the host (CPU model, cores, kernel, RAM, how "2 vCPU / 1 GiB" was approximated, e.g. `GOMAXPROCS=2`, `taskset`, cgroup limits if available), PostgreSQL version and effective settings (`shared_buffers`, `work_mem`, `random_page_cost`, locale), dataset sizes and seeds, and every measured number against its target. A missed target gets the **bottleneck analysis and feasible alternatives** (07A §6.4 A–D plus any found, e.g. the O(n × depth) trie append, finding 9). **The codec, schema, storage format and indexes are not changed automatically.**

---

## 9. Test matrix

All data synthetic; all databases disposable (`internal/pgtest`); no network beyond loopback.

| Area | Level | Cases |
|---|---|---|
| Query parsing | unit | whitespace split (incl. NBSP); < 3 code points ignored with multi-byte text (`ab`, `日本`, `日本語`); AND; duplicates; caps; invalid UTF-8 and NUL → `ErrInvalidQuery`; 40-hex (case, surrounding space, 39/41 chars, hex + word) |
| Escaping | unit + PG | `%`, `_`, `\` literal; a title containing them matches, a wildcard-looking query does not over-match |
| Unicode | PG | case-insensitive ASCII always; non-ASCII only when `datctype` supports it (the test records the locale) |
| SQL shape | unit | `BuildSearchSQL`: no `COUNT(`, no `OFFSET`, `LIMIT $n`, all user input in args, placeholder count; every sort × direction × forward/backward |
| Ordering and cursors | PG | deterministic order with many equal `discovered_on` / `total_size`; walk forward and backward over the full set, no duplicates or gaps; cursor from another query rejected; malformed, truncated, wrong-version, bad-CRC and oversized cursors → 400; name > 4 KiB boundary; inserts between pages |
| Info-hash lookup | PG | hit, miss, uppercase; no pagination |
| Empty search | PG | newest listing; sort `name` falls back |
| Timeout | PG + HTTP | table-lock blocked statement → 57014 → timeout page (not "invalid"); handler vs write deadlines with a short general timeout and a longer search timeout |
| Saturation | HTTP + PG | `K=2`: third request busy after the wait; busy page distinct from timeout; login unaffected on a real 4-connection pool |
| Cancellation | HTTP + PG | client disconnect → no response body, permit released, backend ends, pool recovers |
| Authentication | HTTP | each W4 route anonymous → 303 login; DB error → 503; route table: all `Session`; `/torrents` is `KindSearch`; non-GET → 405 |
| Details errors | HTTP + PG | unknown id → 404; `0`, `-1`, `007`, `1e3`, overflow → 404; missing blob (`file_count>1`) → 500; corrupt blob; limit violation; count / sum mismatch; no partial list |
| Path fidelity | unit + HTTP | valid, invalid UTF-8, mixed, controls and bidi markers, real U+FFFD; no U+FFFD substitution; duplicates (same and different size) kept and marked; empty components, leading `/`, `.` / `..`, long components |
| Navigation round trip | HTTP | property test: random byte paths (including `%`, `+`, `&`, `#`, `?`, NUL, invalid bytes) → generated link → real request resolves the same directory; `a-b/` vs `a/` vs `a.txt` ordering |
| Index | unit | aggregates (count, size, overflow guard), sibling order, file-vs-dir same name, pagination boundaries, memory accounting equals measured retained size within a tolerance |
| Cache | unit | identity change invalidates; LRU eviction by bytes; oversize entry uncached and counted; TTL; 100 navigations → 1 decode |
| Concurrency | unit + race | same-torrent misses collapse to one decode; all-waiters-cancelled before decode cancels the flight; after decode starts the result is still cached; error not cached; no goroutine leak (`goleak`-style count) |
| Decode limits | unit + PG | 250,000-file and 64 MiB blobs decode within limits; over-limit fixtures are `ErrCorruptBlob`; gate admission by weight; weight cap admits one worst-case decode; queue wait → busy |
| HTTP integration | httptest + PG | full list → details → files → deep directory → logout; `/` redirects; CSRF on logout |
| Real binary | `main_web_test.go` | disposable PG, `migrate web-schema-init`, `web user add/set-password`, seeded torrents and blobs by SQL; real HTTP login → search → details → files; timeout via a short `--search-timeout` with a held table lock; stderr contains no names, paths or queries |
| Logging | HTTP | log capture over all of the above: no query text, names, paths, cursors, tokens |
| Boundary | existing | `TestWebPackagesDoNotImportForbiddenPackages` stays green; `internal/web/**` does not import `dht`, crawler, ingest or SQLite |
| Persistence regression | PG | existing `GetFiles` / `QueryTorrents` suites unchanged and green after the `DecodeFileList` refactor |

Validation sequence (narrowest first): `gate` → `search` → `filetree` → `persistence` → `torrents` handlers → `./internal/web/...` → `-run TestWeb .`. The report states that a full `go test ./...` was not run, as in W1–W3.

---

## 10. Implementation steps and review checkpoints

| Step | Content | Commit |
|---|---|---|
| **S0 — STOP (now)** | Owner reviews this plan and answers the decisions in §11 | — |
| 1 | `docs/wip/07E-web-search-details-plan.md` and the README row; only owner-approved spec / 07A deltas | `docs: Task 07E — W4 plan` |
| 2 | `internal/web/gate` + tests | `feat(web): bounded weighted gate` |
| 3 | `persistence` Reader (by id, search SQL builder, `FileIdentity` / `FileHeader` / `FileBlob`, `DecodeFileList` + `GetFiles` refactor, statement-timeout wrapper); optional `codec.PeekDecodedSize` (O3); PG tests incl. the lock-based timeout / cancel / starvation tests; `EXPLAIN` on a ~200k-row synthetic set | `feat(persistence): read-only web queries` |
| 4 | `internal/web/search`: parsing, cursor, service (gate + error classification) + tests | `feat(web): search service and keyset cursors` |
| **S1 — STOP** | Review the SQL, the cursor, the persistence API, the statement-timeout budget tests, the **first `EXPLAIN` evidence** and the **TOAST identity/header measurement (§3.1)**. If a plan is unacceptable, the owner decides on an index here | — |
| 5 | `internal/web/synth` + `internal/web/filetree` **index only** (no cache, no HTTP) + unit tests + cold-build benchmarks at 250k files (both index variants) | `feat(web): directory index prototype and benchmarks` |
| **C1 — STOP: memory-heavy design checkpoint** | Report: index build time and allocations, retained bytes per shape, the `P/declared` ratio, comparison of the two index variants, the trie-append effect, and the proposed weight model. The owner decides O1 / O2 (and whether 07A §6.4 alternatives A–D need discussing) **before** the cache and handlers are built | — |
| 6 | Cache, single-flight, decode gate integration, weight model as approved + concurrency tests under `-race` | `feat(web): file index cache` |
| 7 | `internal/web/torrents` handlers, templates, path rendering, `auth.SessionFormToken`, `GET /{$}` redirect, `main` wiring (`SearchConcurrency`, `Reader`, services), W3 test updates; HTTP tests | `feat(web): torrent search, details and file browser` |
| 8 | Real-binary tests; env-gated load and `EXPLAIN` benchmarks at 1M / 5M rows; `docs/web-deployment.md` (constants, `GOMEMLIMIT`, proxy timeouts, locale note); `docs/wip/07E-web-search-details-report.md` | `test/docs: W4 integration, measurements and report` |
| **S2 — STOP** | Final W4 review. No push; W5 does not start | — |

Each commit uses the configured Git identity, does not bypass hooks, and ends with the attribution line from the session.

**Safety (all steps):** disposable PostgreSQL only; no production or legacy database; no DHT or BitTorrent traffic; no push or force-push; no W5–W9 functionality.

---

## 11. Risks, alternatives and genuine owner decisions

### Owner decisions (resolved at the S0 review)

- **O1 — Decode weight model** (decided at **C1**, after measurements). *Recommended:* the rigorous upper bound (§7.2) first; switch to a measured estimate only if the concurrency it allows is unusable. *Alternative:* estimate with factor `k` plus `GOMEMLIMIT`.
  - **Decided:** conservative admission model initially (§7.2); final choice at C1.
- **O2 — Final file-index solution** (07A §6.4; decided at **C1**). *Recommended:* compact arena index in a bounded cache (alternative A) if the measurements show the 128 MiB budget holds the worst case. *Other options:* B (streaming or partial decode in the codec), C (stored directory index; schema change), D (browse only up to N files). B, C and D need separate approval and are not started.
  - **Decided:** compact arena index is the preferred candidate; final choice at C1.
- **O3 — A tiny additive codec helper** (`PeekDecodedSize`) so memory can be admitted before the blob is read. *Recommended:* yes (no behaviour change; wraps the existing envelope parse). *Alternative:* no codec change and pessimistic weights, which reduces cold-decode concurrency.
  - **Decided:** add the small additive `codec.PeekDecodedSize` helper (no other codec change, no memory optimization authorized).
- **O4 — Sort directions.** *Recommended:* both directions for all three sorts (defaults discovered desc, size desc, name asc). *Alternative:* default direction only, with no `order` parameter.
  - **Decided:** support both sort directions.
- **O5 — Name-sort collation.** *Recommended:* the column's default collation (useful for people), documented as server-dependent, with `COLLATE "C"` kept as the fallback if `EXPLAIN` / timing shows the default is too slow. *Alternative:* `COLLATE "C"` from the start (fast and deterministic, byte order).
  - **Decided:** use the column's existing collation (`COLLATE "C"` is not adopted).
- **O6 — Query caps** (not in the spec): 512 bytes, 8 usable terms. *Recommended:* yes. *Alternative:* no caps (each extra term adds an ILIKE predicate; risk of expensive plans).
  - **Decided:** 512-byte query limit, at most 8 usable terms.
- **O7 — Magnet on details before W7.** *Recommended:* info hash plus a tracker-less magnet URI now; the short link and the tracker snapshot arrive with `/magnet` in W7. *Alternative:* pull `/magnet` and the tracker snapshot into W4 (scope creep).
  - **Decided:** tracker-less magnet URI in W4; short link and tracker snapshot remain W7.

### Defaults taken without asking (override if you disagree)

- O8: details and files are separate pages (spec §6.1 over the 07A mock-up).
- O9: cache, gate, wait and display sizes are constants, not new environment variables; adding variables would amend spec §2.3.
- O10: `strings.Fields` (Unicode white space) and trimmed exact-40-hex lookup.
- O11: blob identity by `(length, xmin)` plus a 10-minute maximum age.
- O12: `GET /{$}` redirects to `/torrents`; W3 tests that expect the interim page are updated.
- O13: no negative caching of failed decodes.

### Risks

- **R1 — Broad searches can be slow** (very common terms, `name` / `size` sorts over wide matches); bounded by the timeout and K concurrent searches; real behaviour is the S1 / step 8 evidence, and a needed index is an owner decision.
- **R2 — Memory:** worst-case weights are pessimistic, so large cold decodes serialize and excess requests get bounded-wait 503 (as the spec's 32-concurrent target describes). A decode is uninterruptible once it starts.
- **R3 — Trie decode copies files O(n × depth)** (finding 9); may miss the allocation target for trie-encoded blobs. Reported, not changed.
- **R4 — Cursor staleness:** a late commit with an older `discovered_on` can be missed by a forward walk.
- **R5 — `xmin` identity** is not an API guarantee (freeze can change it); the consequence is one extra decode. The 10-minute age bounds anything it misses.
- **R6 — Connection churn:** pgx closes a connection when a search is cancelled; a burst of cancellations costs reconnects. Server-side `statement_timeout` and the gate bound the damage.
- **R7 — Locale dependence** of non-ASCII `ILIKE` and of name ordering; documented and tested per locale.
- **R8 — Rendering of unusual paths** relies on marker spans; plain-text copy of a marker is ambiguous with literal `\x41` text until W5 styles it.

---

## Critical files

- **New:** `internal/web/{gate,search,filetree,synth,torrents}/`, `internal/web/ui/templates/{torrents,torrent,files}.html`, `persistence/webread.go`, `docs/wip/07E-web-search-details-{plan,report}.md`.
- **Modified:** `persistence/postgres.go` (`GetFiles` onto `DecodeFileList`), `codec/` (optional additive `PeekDecodedSize`, O3), `internal/web/auth/` (`SessionFormToken`; `home` becomes a redirect), `internal/web/ui/{ui.go,templates/layout.html,static/app.css}` (minimal chrome only), `main.go` (wiring), `main_web_test.go`, `docs/web-deployment.md`, `docs/wip/README.md`.
- **Reused:** `server.Router/Kind/Access`, `routeDeadline`, `auth.Install` pattern and `auth.Current`, `ui.Renderer`, `webdb.Backend.Pool()`, `persistence.VerifySchema` pattern, `escapeLikePattern` logic, `codec.DefaultLimits/Decode/Encode`, `internal/pgtest`.

## Verification (end to end, after approval and implementation)

```
go build ./... && go vet ./...
go test -count=1 -race ./internal/web/gate ./internal/web/search ./internal/web/filetree
go test -count=1 -race ./persistence                     # incl. refactor regression
go test -count=1 -race ./internal/web/...
go test -count=1 -race -run 'TestWeb' .                  # real binary
go test -run=^$ -bench . -benchmem ./internal/web/filetree           # explicit benchmarks
MAGNETICO_BENCH_LARGE=1 go test -run TestLoad -v ./internal/web/filetree   # 8 / 32 concurrent, RSS
```

`dht/...`, `internal/crawler` and `internal/e2e/...` are not run, and the report states that a full `go test ./...` was not run.
