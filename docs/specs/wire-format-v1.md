# Magnetico 0.8.0 — Torrent File-List Wire Format v1

**Status:** Proposed implementation contract, October 2026. Agreed architectural decisions are reflected here; any compatibility-affecting change must be reviewed before implementation.

**Scope:** one self-describing `BYTEA` containing a torrent's file list. This is **not** a `.torrent` representation, a bencoded info dictionary, or a reversible reconstruction of torrent metadata. The external PostgreSQL schema and migration implementation are specified separately.

## 1. Logical model and invariants

```go
type File struct {
    Path string
    Size int64
}
```

- An input list has at least one file. `Size >= 0`. Repeated `(Path, Size)` pairs are preserved, not deduplicated.
- Paths are opaque byte strings carried in Go `string` values; the codec does **not** normalize Unicode, change separators/case, or call `filepath.Clean`. Ingestion and presentation may have their own UTF-8/security validation policies. Legacy PostgreSQL `TEXT` data is already constrained by PostgreSQL's text encoding and inability to store NUL.
- Encoding sorts a **copy** by `Path` (lexicographic order of bytes), then by `Size` ascending for identical paths. Source file order is intentionally lost. Sorting by size only reorders identical paths and does not change prefix-delta's uncompressed byte count.
- The containing torrent supplies `torrents.file_count` and `torrents.total_size`; successful decoding **must** match both. Checked accumulation must not overflow `int64`.
- Record format version and PostgreSQL schema version are independent.

## 2. Outer envelope

| Offset | Encoding | Definition |
|---|---|---|
| 0 | byte | Version: `0x01` |
| 1 | byte | High nibble = serializer ID; low nibble = compressor ID |
| 2, if compressed | minimal `uvarint` | Uncompressed payload length in bytes, **not** including envelope |
| Next | bytes | Complete raw or compressed serialized payload |

Serializer IDs: `0x1` = flat, `0x2` = prefix-delta, `0x4` = directory-trie. All others are reserved and rejected by the v1 decoder.

Compressor IDs: `0x0` = none, `0x1` = Zstandard, `0x2` = Brotli. All others are reserved and rejected. Compression level is **not** encoded: decompression does not need it. For example `01 22` = prefix-delta + Brotli, `01 41` = trie + Zstd.

For `none`, there is **no** raw-length varint; the payload consumes all bytes remaining in the blob. For compression, a minimal unsigned varint of the **exact** decompressed size precedes the compressed stream. The decoder must enforce both declared and actual size bounds, verify that the stream terminates correctly, and reject unconsumed/trailing compressed input. It must not trust the declared length to permit unconstrained allocations or decompression.

`uvarint` is the Go `encoding/binary` unsigned base-128 little-endian varint: smallest encoding only; overflow, truncated, and overlong representations are invalid. Each numeric field is checked before conversion to Go `int` or `int64`.

## 3. Raw serializers

All include a leading `uvarint(file_count)`; `count > 0`.

### 3.1 Flat (ID 1)

```text
uvarint(file_count)
repeat file_count:
    uvarint(path_byte_length)
    bytes(path)
    uvarint(file_size)
```

The encoder and decoder require canonical pair ordering. Empty or unusual paths are preserved by this serializer subject to independent application limits.

### 3.2 Prefix-delta (ID 2)

```text
uvarint(file_count)
previous_path = empty bytes
repeat file_count:
    uvarint(shared_prefix_byte_length)
    uvarint(suffix_byte_length)
    bytes(suffix)
    uvarint(file_size)
    previous_path = previous_path[:shared_prefix_byte_length] || suffix
```

`shared_prefix_byte_length` must be **exactly the maximal common byte prefix** between the current path and previous path. It may end inside a multi-byte UTF-8 code point; this is valid because the decoder concatenates bytes. `shared` may not exceed the preceding path length. Reconstructed `(Path, Size)` must be canonically ordered. Identical full paths are allowed, including identical sizes.

### 3.3 Directory-trie (ID 4)

Applicable only if splitting every complete path on literal `/` yields **non-empty** components and can be reversed exactly. Paths with leading/trailing `/`, `//`, or an empty path cannot use trie; the encoder must fall back to prefix or flat **without altering the path**. Components such as `.` and `..` are literal names, not normalized.

The root has no name and is encoded as a node. Each node is encoded recursively:

```text
uvarint(subdirectory_count)
for every immediate subdirectory, sorted by component bytes:
    uvarint(component_byte_length)
    bytes(component)
    encode_node(subdirectory)
uvarint(local_file_count)
for each file directly in this directory, sorted by (basename_bytes, size):
    uvarint(basename_byte_length)
    bytes(basename)
    uvarint(file_size)
```

There is also a **leading global `uvarint(file_count)`** before the root node (unlike the provisional benchmark serializer).

The decoder must reject duplicate subdirectory names within a node, empty components, out-of-order subdirectory lists or local file lists, invalid node counts, exceeding path/depth limits, and mismatched global file counts. Duplicate file entries remain valid. There can be a file and directory with the same component name; the codec represents both without loss.

**Important ordering subtlety:** Subdirectories are encoded before files at each node, so tree depth-first traversal can produce a different order from global lexicographic full-path sorting. After decoding trie, sort the resulting `[]File` canonically. Validate **local node ordering**, not the emitted depth-first order. For flat/prefix, validate the serialized global order directly and reject out-of-order records.

## 4. Deterministic encoder, compression profiles

For a canonical input list, encoder output must be deterministic for a chosen serializer, compressor, and library version. The encoder compares **total framed bytes**, including the header and conditional raw-length field; it must keep an uncompressed fallback for records where compression is not beneficial. Stable tie-breaking: prefer uncompressed, then prefix-delta, then trie, then flat (or document any revised, deterministic, implementation-independent priority before first deployment).

Production profiles (preliminary parameterization, agreed direction):

| Profile | Permitted principal candidate(s) | Purpose |
|---|---|---|
| `balanced` | Prefix-delta + Zstd default; uncompressed fallback | Fast writes |
| **`compact` (default)** | Prefix-delta + Brotli q9; trie + Brotli q9; uncompressed fallback | Best observed trade-off |
| `maximum` | Prefix-delta + Brotli q11; trie + Brotli q11; uncompressed fallback | Smaller files, much slower migration |

Trie is omitted for unsafe path structures. No-blob single-file optimization is applied **before** encoding and is defined in the persistence contract, not as a wire-format variant.

Exact compressor library/version and concurrency controls are deployment implementation choices; old compressed blobs must remain readable across encoder upgrades. Do not expect byte-for-byte identity across versions of Zstd/Brotli encoders. Preserve compressed fixtures and assert cross-version **decode compatibility** rather than stable recompression bytes.

## 5. Decode and encode limits (initial configurable defaults)

| Limit | Initial value |
|---|---:|
| Stored blob length including envelope | 32 MiB |
| Uncompressed payload length | 64 MiB |
| File count per torrent | 250,000 |
| Single full path length | 64 KiB |
| Aggregate reconstructed full-path bytes | 64 MiB |
| Trie directory depth | 256 |

These are initial configurable limits, subject to validation on real-world data. Apply limits to both encoding and decoding. Decompress with bounded output, enforce exact decompressed length, apply memory budgets to compressor internals and concurrent workers, reject malformed/unknown frames, and ensure **no partial file list** is returned as success. For compressed streams, enforce exact stream consumption and reject trailing junk or extraneous frames in accordance with the selected decoder API; test this explicitly.

For trie, enforce aggregate decoded file count, total allocated path bytes and depth **as decoding progresses**, not only after constructing the whole tree. For prefix-delta, the compressed raw payload can be small even when reconstructed paths are large; the aggregate-path limit is mandatory.

Error reporting: structured `torrent_id` (if assigned), `info_hash` (if known), category, limit name, actual/allowed value. No full paths in ordinary logs. Failed DHT metadata limit: warning + drop + aggregate metric; stored-data corruption or invariant failure: error and no silent repair. Exception notification is behind an optional interface; no mandatory Sentry or Prometheus dependency.

## 6. Golden vectors (entire uncompressed blob, including header)

The vectors below were independently regenerated according to this spec. Spaces and line breaks are for readability only.

**A. No blob**: `torrent.name = "Film.mkv"`, `total_size = 1234`, `file_count = 1`, and sole file `("Film.mkv", 1234)`. The persistence layer omits the blob; no serialized bytes exist for this case.

**B. Atypical single-file name**: file `("Different.mkv", 1234)` while torrent name is `"Release title"`.

Flat, **19 bytes**:

```hex
01 10 01 0D 44 69 66 66 65 72 65 6E 74 2E 6D 6B 76 D2 09
```

**C. Two episode files**: `(S/E01.mkv, 1000)`, `(S/E02.mkv, 1100)`.

Flat, **27 bytes**:

```hex
01 10 02 09 53 2F 45 30 31 2E 6D 6B 76 E8 07 09 53 2F 45 30 32 2E 6D 6B 76 CC 08
```

Prefix-delta, **25 bytes**:

```hex
01 20 02 00 09 53 2F 45 30 31 2E 6D 6B 76 E8 07 04 05 32 2E 6D 6B 76 CC 08
```

Directory-trie, **29 bytes**:

```hex
01 40 02 01 01 53 00 02 07 45 30 31 2E 6D 6B 76 E8 07 07 45 30 32 2E 6D 6B 76 CC 08 00
```

**D. Duplicate paths and zero sizes**: `(D/a.txt,0)`, `(D/a.txt,0)`, `(D/a.txt,7)`.

Prefix-delta, **19 bytes**:

```hex
01 20 03 00 07 44 2F 61 2E 74 78 74 00 07 00 00 07 00 07
```

**E. UTF-8**: `(Музыка/01.flac,128)`, `(Музыка/02.flac,256)`.

Prefix-delta, **37 bytes**:

```hex
01 20 02 00 14 D0 9C D1 83 D0 B7 D1 8B D0 BA D0 B0 2F 30 31 2E 66 6C 61 63 80 01 0E 06 32 2E 66 6C 61 63 80 02
```

**F. Structurally irregular paths**: `(a/../c, 2)`, `(a//b, 1)`. Trie must be excluded; prefix-delta is allowed.

Prefix-delta, **17 bytes**:

```hex
01 20 02 00 06 61 2F 2E 2E 2F 63 02 02 02 2F 62 01
```

**G. Trie traversal differs from global sorting**: `(a.txt,1)`, `(z/f.bin,2)`. The trie encodes the `z/` subtree **before** the root file `a.txt`; decoding must still return files sorted by full path.

Trie, **23 bytes**:

```hex
01 40 02 01 01 7A 00 01 05 66 2E 62 69 6E 02 01 05 61 2E 74 78 74 01
```

For Zstd and Brotli, maintain separately stored compressed **decode fixtures** produced with pinned encoder/library versions. Test envelope, declared raw size, stream consumption, corruption, unsupported IDs, overlong varints, numeric overflow and decompression bombs. Do not demand deterministic compressed bytes across library versions.

## 7. Required tests before implementation is accepted

1. Golden hex tests for B–G; A is tested at persistence boundary.
2. Property-based and fuzz round-trips for valid/pathological paths, duplicates, UTF-8 and sizes; ensure no input mutation.
3. Reject noncanonical flat/prefix order; reject nonmaximal prefix; reject malformed trie node ordering, duplicate directory entries and trailing bytes.
4. Verify exact `file_count` and `total_size` against torrent metadata after decode.
5. Overflow, untrusted input size and aggregate reconstructed-path limits, compressed-stream trailing bytes, truncation, corruption and bogus raw lengths.
6. Decode fixtures for both compressors, independent of the current configured compression profile.
7. Compare every allowed candidate by **total envelope bytes** and test deterministic tie-breaking.

## 8. Versioning and future maintenance

- Unknown format versions/IDs fail closed; do not guess.
- Changing compression quality **does not** create a new wire version.
- Future `magnetico storage repack` may rewrite valid blobs after deployment, but is **not in 0.8.0**.
- Persisting raw `.torrent` or original file ordering is explicitly **not** a goal.
- The resulting 0.8.0 application supports PostgreSQL as its only database backend, with an internal storage interface that can be extended later.

## 9. Open implementation checks (do not silently alter the wire contract)

- Verify decoder library behavior for concatenated compressed streams and trailing bytes; choose strict supported APIs or explicitly document an exception before release.
- Verify application ingestion's treatment of invalid UTF-8 and NUL paths separately from byte-exact codec semantics, especially for DHT v2/hybrid metadata.
- Rebenchmark exact final framing/added `file_count` relative to provisional benchmark codec; expected byte-level differences must be reflected in tests.
