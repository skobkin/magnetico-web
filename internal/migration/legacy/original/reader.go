package original

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// TorrentRow is one source `magneticod.torrents` row. Source-only type:
// never confused with persistence.Torrent.
type TorrentRow struct {
	ID           int32
	InfoHash     []byte
	Name         string
	TotalSize    int64
	DiscoveredOn int64 // source column is INTEGER; kept as int64 for the target
}

// FileRow is one source `magneticod.files` row. SourceFileID (the source
// files.id) is retained for the later forensic export (04C); it never maps
// into the codec.
type FileRow struct {
	ID        int32
	TorrentID int32
	Path      string
	Size      int64
}

// WindowLimits bounds the reader's retention. They derive from the
// production codec limits and the runner's in-flight budget — never from
// invented migration-only caps:
//
//   - MaxFiles (codec Limits.MaxFiles): per-torrent retained file-row count.
//   - MaxPathBytes (codec Limits.MaxAggregatePathBytes): per-torrent
//     retained aggregate path bytes. A torrent over either cap cannot
//     survive codec validation anyway; the reader drains its remaining rows
//     and the record is rejected with a categorized error instead of
//     pretending a complete list was read.
//   - WindowMaxBytes (the runner's --max-inflight-bytes): the window stops
//     early once its retained WEIGHT reaches this budget — enforced during
//     BOTH scans, torrent metadata and file rows. The torrents beyond the
//     cut simply arrive in the next keyset window, so read-side buffering
//     stays bounded by the budget plus one torrent: an occupied window
//     never retains a torrent that would push it past the budget, and an
//     empty window always admits its first torrent whatever it weighs (the
//     read-side mirror of the pipeline's oversized-solo admission). The
//     weight mirrors the pipeline's memory model (path/name bytes plus the
//     structural overhead below), because the buffered rows, not their
//     string bytes, dominate real memory for torrents of many short paths.
type WindowLimits struct {
	MaxFiles       int
	MaxPathBytes   int
	WindowMaxBytes int64
}

// Retention-weight constants mirroring the pipeline's memory model
// (pipeline.RecordWeightBase/FileWeightOverhead): the window budget bounds
// the same retained data the in-flight semaphore bounds post-dispatch, so
// both budgets charge structure, not just string bytes.
const (
	// retainedTorrentOverhead covers one retained TorrentRow: the struct,
	// the info-hash slice (header + 20-byte hash), the name header and its
	// share of the window's map/bookkeeping.
	retainedTorrentOverhead = 128
	// retainedFileOverhead covers one retained FileRow: the struct (32 B)
	// plus slice-append growth slack.
	retainedFileOverhead = 64
)

// Overflow reason codes for Window.Overflow.
const (
	// OverflowFileCount: more file rows than WindowLimits.MaxFiles.
	OverflowFileCount = "files"
	// OverflowPathBytes: more aggregate path bytes than
	// WindowLimits.MaxPathBytes.
	OverflowPathBytes = "path_bytes"
)

// Overflow describes a torrent whose file rows were drained without
// retention after a cap was hit. Observed is the row count for
// OverflowFileCount and the path-byte total at the moment of the overrun
// for OverflowPathBytes; it feeds the per-record error details.
type Overflow struct {
	Observed int64
	Reason   string
}

// Window is one keyset page of source data: the torrents with
// afterID < id <= lastID, their file rows grouped by torrent ID (duplicates
// preserved in source order), plus the aggregate accounting for rows that
// must not be retained. FetchByIDs reuses the type for a sparse ID set.
type Window struct {
	Torrents []TorrentRow
	Files    map[int32][]FileRow

	// OrphanFiles counts file rows in the window whose torrent_id has no
	// owning torrent. They are excluded from every record and accounted as
	// run-scope integrity errors.
	OrphanFiles int64

	// Overflow maps torrent IDs whose file rows exceeded a WindowLimits cap:
	// the reader drained their rows without retention, and the pipeline must
	// reject these records with a categorized error instead of pretending
	// it read a complete list.
	Overflow map[int32]Overflow

	// RetainedBytes is the retention WEIGHT the window kept: path and name
	// bytes plus the structural overhead of every retained row (see the
	// WindowLimits budget discussion). The window truncates early once it
	// reaches WindowLimits.WindowMaxBytes.
	RetainedBytes int64

	truncated bool
}

// Truncated reports whether the window stopped before its full torrent
// range was read (window byte budget hit); the remaining torrents arrive
// in the next keyset window.
func (w *Window) Truncated() bool { return w.truncated }

// Window reads the next keyset page: torrents with id > afterID, ordered by
// id, at most limit rows, plus their file rows via the files(torrent_id)
// index (one set-oriented query per window — never one query per torrent,
// no SQL-side aggregates).
//
// afterID is the keyset cursor one below the next ID to read. It is int64,
// NOT int32: a fresh run starts at source_min_id-1, and for a source whose
// minimum ID is MinInt32 that cursor is MinInt32-1 — converting it to int32
// would wrap to MaxInt32 and silently skip the whole source.
//
// maxFiles/limits semantics: see WindowLimits. limit is the keyset page
// size (the batch), independent of the retention caps.
func (s *Source) Window(ctx context.Context, afterID int64, limit int, limits WindowLimits) (*Window, error) {
	return s.readWindow(ctx, afterID, limit, limits, false)
}

// WindowSnapshot is Window read inside ONE `REPEATABLE READ READ ONLY`
// transaction, so a window's metadata and file rows come from a single
// consistent per-database snapshot (the verification content pass, 04B,
// reads the source page by page this way). Snapshots are per-window: two
// windows are never cross-atomic, and no cross-database snapshot pair is
// ever atomic — operator-enforced source immutability stays essential.
func (s *Source) WindowSnapshot(ctx context.Context, afterID int64, limit int, limits WindowLimits) (*Window, error) {
	return s.readWindow(ctx, afterID, limit, limits, true)
}

// queryer (the package-level pool/tx read interface, original.go) covers
// both read paths below.

func validateWindowLimits(limit int, limits WindowLimits) error {
	if limit <= 0 {
		return fmt.Errorf("window limit must be positive, got %d", limit)
	}
	if limits.MaxFiles <= 0 {
		return fmt.Errorf("MaxFiles must be positive, got %d", limits.MaxFiles)
	}
	if limits.MaxPathBytes <= 0 {
		return fmt.Errorf("MaxPathBytes must be positive, got %d", limits.MaxPathBytes)
	}
	return nil
}

func (s *Source) readWindow(ctx context.Context, afterID int64, limit int, limits WindowLimits, snapshot bool) (*Window, error) {
	if err := validateWindowLimits(limit, limits); err != nil {
		return nil, err
	}

	var q queryer = s.pool
	var tx pgx.Tx
	if snapshot {
		var err error
		tx, err = s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // no-op after Commit
		q = tx
	}

	w := &Window{Files: map[int32][]FileRow{}}
	// $1 is cast to bigint explicitly: uncast, PostgreSQL reports the
	// parameter as int4 (from the id column context) and pgx refuses to
	// encode the MinInt32-1 start cursor into it.
	rows, err := q.Query(ctx, `SELECT id, info_hash, name, total_size, discovered_on
        FROM magneticod.torrents
        WHERE id > $1::bigint
        ORDER BY id
        LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	// scanned marks the torrents retained in this window: the files pass
	// uses it to attach rows and to recognize orphans.
	scanned, truncated, err := scanMetadata(rows, w, limits)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if truncated {
		w.truncated = true
	}
	if len(w.Torrents) == 0 {
		return w, commitIfSnapshot(ctx, tx, snapshot)
	}
	lastID := w.Torrents[len(w.Torrents)-1].ID

	// One bounded, set-oriented files query for the whole window; NULL
	// torrent_id rows fail the range comparison and are therefore excluded
	// here (they are counted at preflight and in the fingerprint).
	frows, err := q.Query(ctx, `SELECT id, torrent_id, size, path
        FROM magneticod.files
        WHERE torrent_id > $1::bigint AND torrent_id <= $2
        ORDER BY torrent_id, id`, afterID, lastID)
	if err != nil {
		return nil, err
	}
	lastKept, filesStopped, err := readFiles(frows, w, scanned, limits)
	frows.Close()
	if err != nil {
		return nil, err
	}
	if filesStopped {
		// The read stopped early at a group boundary: that group is complete
		// and kept; everything after it was never read and moves to the next
		// window. Truncate the torrent list to the read range.
		w.truncated = true
		w.truncateAfter(lastKept)
	}
	return w, commitIfSnapshot(ctx, tx, snapshot)
}

func commitIfSnapshot(ctx context.Context, tx pgx.Tx, snapshot bool) error {
	if !snapshot {
		return nil
	}
	return tx.Commit(ctx)
}

// FetchByIDs fetches a sparse, possibly unordered ID set set-oriented: one
// metadata query by ID list, one files query for the kept IDs — never one
// query per torrent. The returned Window has the same overflow-drain and
// byte-budget semantics as Window: when the retained weight would exceed
// WindowLimits.WindowMaxBytes, the scan stops at a torrent boundary and the
// requested IDs beyond the cut come back as `remaining` for a follow-up
// call (an empty result always admits its first torrent whatever it
// weighs). Torrents whose file rows exceed the caps are drained and
// reported in Window.Overflow.
//
// Requested IDs absent from the source are simply absent from the result —
// the caller decides what missing means (a retry pass reports
// missing_source; it never guesses). File rows of an ID dropped by the
// budget cut are neither fetched nor counted: they belong to the follow-up
// batch, so they can never masquerade as orphans.
func (s *Source) FetchByIDs(ctx context.Context, ids []int64, limits WindowLimits) (*Window, []int64, error) {
	ids = normalizedIDs(ids)
	if len(ids) == 0 {
		return &Window{Files: map[int32][]FileRow{}}, nil, nil
	}
	if err := validateWindowLimits(len(ids), limits); err != nil {
		return nil, nil, err
	}

	w := &Window{Files: map[int32][]FileRow{}}
	rows, err := s.pool.Query(ctx, `SELECT id, info_hash, name, total_size, discovered_on
        FROM magneticod.torrents
        WHERE id = ANY($1::bigint[])
        ORDER BY id`, ids)
	if err != nil {
		return nil, nil, err
	}
	scanned, truncated, err := scanMetadata(rows, w, limits)
	rows.Close()
	if err != nil {
		return nil, nil, err
	}

	var remaining []int64
	if truncated && len(w.Torrents) > 0 {
		lastKept := int64(w.Torrents[len(w.Torrents)-1].ID)
		remaining = idsAfter(ids, lastKept)
	}
	if len(w.Torrents) == 0 {
		// None of the requested IDs exists (or the first alone tripped the
		// budget — impossible: an empty window admits its first torrent).
		return w, remaining, nil
	}

	kept := make([]int64, 0, len(w.Torrents))
	for _, t := range w.Torrents {
		kept = append(kept, int64(t.ID))
	}
	frows, err := s.pool.Query(ctx, `SELECT id, torrent_id, size, path
        FROM magneticod.files
        WHERE torrent_id = ANY($1::bigint[])
        ORDER BY torrent_id, id`, kept)
	if err != nil {
		return nil, nil, err
	}
	lastKept, filesStopped, err := readFiles(frows, w, scanned, limits)
	frows.Close()
	if err != nil {
		return nil, nil, err
	}
	if filesStopped {
		// Groups after lastKept were never read: drop the retained metadata
		// (it would masquerade as a file-less torrent) and return those IDs
		// for the follow-up batch.
		w.truncated = true
		remaining = append(remaining, idsAfter(ids, int64(lastKept))...)
		w.truncateAfter(lastKept)
	}
	return w, remaining, nil
}

// normalizedIDs returns a sorted, deduplicated copy of ids.
func normalizedIDs(ids []int64) []int64 {
	out := make([]int64, len(ids))
	copy(out, ids)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	dedup := out[:0]
	for i, id := range out {
		if i == 0 || id != out[i-1] {
			dedup = append(dedup, id)
		}
	}
	return dedup
}

func idsAfter(ids []int64, lastKept int64) []int64 {
	var remaining []int64
	for _, id := range ids {
		if id > lastKept {
			remaining = append(remaining, id)
		}
	}
	return remaining
}

// scanMetadata consumes a torrent-metadata iterator (ordered by id),
// applying the retention-budget admission policy (see WindowLimits): the
// weight is charged before retention, an occupied window stops before any
// torrent that would push it past the budget, and an empty window admits
// its first torrent whatever it weighs. It returns the map marking every
// retained torrent and whether the budget stopped the scan before the
// iterator was exhausted.
func scanMetadata(rows pgx.Rows, w *Window, limits WindowLimits) (map[int32]bool, bool, error) {
	scanned := make(map[int32]bool)
	for rows.Next() {
		var t TorrentRow
		if err := rows.Scan(&t.ID, &t.InfoHash, &t.Name, &t.TotalSize, &t.DiscoveredOn); err != nil {
			return nil, false, err
		}
		// The window budget bounds the METADATA scan too, not just the file
		// rows: with a keyset page far larger than the budget, torrents with
		// long names — above all torrents with no files, which never reach
		// the file-row pass — would otherwise be retained in full before any
		// budget check.
		weight := retainedTorrentOverhead + int64(len(t.Name))
		if limits.WindowMaxBytes > 0 && len(w.Torrents) > 0 &&
			w.RetainedBytes+weight > limits.WindowMaxBytes {
			return scanned, true, nil
		}
		w.Torrents = append(w.Torrents, t)
		scanned[t.ID] = true
		w.RetainedBytes += weight
	}
	return scanned, false, rows.Err()
}

// readFiles consumes a file-row iterator ordered by (torrent_id, id),
// attaching rows to w for the scanned torrents, draining capped torrents
// into Overflow, counting orphans and enforcing the byte budget. It returns
// the ID of the last torrent whose group was fully read and kept, and
// whether reading stopped early at a group boundary because the budget ran
// out — the torrents whose groups come after it were never read and the
// caller must drop their metadata (they move to the next window/batch).
func readFiles(rows pgx.Rows, w *Window, scanned map[int32]bool, limits WindowLimits) (int32, bool, error) {
	var (
		current         int32
		currentCount    int64
		currentBytes    int64
		currentKnown    bool
		currentOverflow bool
		currentReason   string
	)
	// finishGroup finalizes the group just ended: an overflowing torrent
	// loses its retained rows and is recorded exactly once.
	finishGroup := func() {
		if currentKnown && currentOverflow {
			delete(w.Files, current)
			w.OverflowRecord(current, Overflow{Observed: overflowObserved(currentCount, currentBytes, currentReason), Reason: currentReason})
		}
	}
	budgetHit := false
	stopped := false
	for rows.Next() {
		var f FileRow
		if err := rows.Scan(&f.ID, &f.TorrentID, &f.Size, &f.Path); err != nil {
			return 0, false, err
		}
		if f.TorrentID != current {
			finishGroup()
			if budgetHit {
				// Window byte budget reached: finish the torrent whose group
				// just completed and stop reading. Torrents beyond the cut are
				// re-read by the next keyset window or follow-up batch (the
				// caller's cursor stops at the last kept torrent), so nothing
				// is lost — the window just got smaller under backpressure.
				stopped = true
				break
			}
			current, currentCount, currentBytes, currentKnown, currentOverflow, currentReason = f.TorrentID, 0, 0, false, false, ""
		}
		if !scanned[f.TorrentID] {
			// Orphan: no owning torrent in this window (both queries cover
			// exactly the retained set, so absence here means no row).
			// Excluded from every record, accounted as run-scope integrity
			// state.
			w.OrphanFiles++
			continue
		}
		currentKnown = true
		currentCount++
		// currentBytes stays PURE path bytes: it mirrors the codec's own
		// aggregate cap and must never reject a torrent the codec would
		// accept. The retention WEIGHT below is the only place structural
		// overhead is charged, and it only shapes the window budget.
		currentBytes += int64(len(f.Path))
		w.RetainedBytes += int64(len(f.Path)) + retainedFileOverhead
		switch {
		case currentCount > int64(limits.MaxFiles):
			// Drain without retention: stop keeping rows for this torrent.
			currentOverflow = true
			currentReason = OverflowFileCount
			continue
		case currentBytes > int64(limits.MaxPathBytes):
			// Same policy for aggregate path bytes: the codec would reject
			// the torrent anyway, so drain instead of buffering.
			currentOverflow = true
			currentReason = OverflowPathBytes
			continue
		}
		w.Files[f.TorrentID] = append(w.Files[f.TorrentID], f)
		if limits.WindowMaxBytes > 0 && w.RetainedBytes >= limits.WindowMaxBytes {
			budgetHit = true
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	finishGroup()
	return current, stopped, nil
}

// overflowObserved reports the diagnostic figure for an overflow: the row
// count for file-count overruns, the path-byte total for byte overruns.
func overflowObserved(count, bytes int64, reason string) int64 {
	if reason == OverflowPathBytes {
		return bytes
	}
	return count
}

// truncateAfter drops torrents beyond lastID from the window: they were
// never read (window byte budget) and belong to the next keyset window.
func (w *Window) truncateAfter(lastID int32) {
	kept := w.Torrents[:0]
	for _, t := range w.Torrents {
		if t.ID > lastID {
			break
		}
		kept = append(kept, t)
	}
	w.Torrents = kept
}

// OverflowRecord records a torrent whose file rows exceeded a reader cap.
// The rows were drained; the observed figure and reason are preserved for
// the per-record error.
func (w *Window) OverflowRecord(torrentID int32, ov Overflow) {
	if w.Overflow == nil {
		w.Overflow = map[int32]Overflow{}
	}
	w.Overflow[torrentID] = ov
}
