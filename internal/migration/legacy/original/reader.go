package original

import (
	"context"
	"fmt"
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
//     early once its retained path bytes reach this budget — the torrents
//     beyond the cut simply arrive in the next keyset window, so read-side
//     buffering stays bounded by the budget plus one capped torrent.
type WindowLimits struct {
	MaxFiles       int
	MaxPathBytes   int
	WindowMaxBytes int64
}

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
// must not be retained.
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

	// RetainedBytes is the aggregate path-byte weight the window kept; the
	// window truncates early once it reaches WindowLimits.WindowMaxBytes.
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
	if limit <= 0 {
		return nil, fmt.Errorf("window limit must be positive, got %d", limit)
	}
	if limits.MaxFiles <= 0 {
		return nil, fmt.Errorf("MaxFiles must be positive, got %d", limits.MaxFiles)
	}
	if limits.MaxPathBytes <= 0 {
		return nil, fmt.Errorf("MaxPathBytes must be positive, got %d", limits.MaxPathBytes)
	}

	w := &Window{Files: map[int32][]FileRow{}}
	// $1 is cast to bigint explicitly: uncast, PostgreSQL reports the
	// parameter as int4 (from the id column context) and pgx refuses to
	// encode the MinInt32-1 start cursor into it.
	rows, err := s.pool.Query(ctx, `SELECT id, info_hash, name, total_size, discovered_on
        FROM magneticod.torrents
        WHERE id > $1::bigint
        ORDER BY id
        LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t TorrentRow
		if err := rows.Scan(&t.ID, &t.InfoHash, &t.Name, &t.TotalSize, &t.DiscoveredOn); err != nil {
			rows.Close()
			return nil, err
		}
		w.Torrents = append(w.Torrents, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(w.Torrents) == 0 {
		return w, nil
	}
	lastID := w.Torrents[len(w.Torrents)-1].ID

	scanned := make(map[int32]bool, len(w.Torrents))
	for _, t := range w.Torrents {
		scanned[t.ID] = true
	}

	// One bounded, set-oriented files query for the whole window; NULL
	// torrent_id rows fail the range comparison and are therefore excluded
	// here (they are counted at preflight and in the fingerprint).
	frows, err := s.pool.Query(ctx, `SELECT id, torrent_id, size, path
        FROM magneticod.files
        WHERE torrent_id > $1::bigint AND torrent_id <= $2
        ORDER BY torrent_id, id`, afterID, lastID)
	if err != nil {
		return nil, err
	}
	defer frows.Close()

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
	for frows.Next() {
		var f FileRow
		if err := frows.Scan(&f.ID, &f.TorrentID, &f.Size, &f.Path); err != nil {
			return nil, err
		}
		if f.TorrentID != current {
			finishGroup()
			if budgetHit {
				// Window byte budget reached: finish the torrent whose group
				// just completed and stop reading. Torrents beyond the cut
				// are re-read by the next keyset window (the caller's cursor
				// stops at the last kept torrent), so nothing is lost — the
				// window just got smaller under backpressure.
				budgetHit = false
				break
			}
			current, currentCount, currentBytes, currentKnown, currentOverflow, currentReason = f.TorrentID, 0, 0, false, false, ""
		}
		if !scanned[f.TorrentID] {
			// Orphan: no owning torrent in this window (the window covers a
			// contiguous ID range, so absence here means no row). Excluded
			// from every record, accounted as run-scope integrity state.
			w.OrphanFiles++
			continue
		}
		currentKnown = true
		currentCount++
		currentBytes += int64(len(f.Path))
		w.RetainedBytes += int64(len(f.Path))
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
	if err := frows.Err(); err != nil {
		return nil, err
	}
	finishGroup()
	if budgetHit {
		// The budget was hit inside the final group: that group is complete
		// and kept; everything after it was never read and moves to the next
		// window. Truncate the torrent list to the read range.
		w.truncated = true
		w.truncateAfter(current)
	}
	return w, nil
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
