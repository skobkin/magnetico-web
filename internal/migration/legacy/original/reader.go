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

	// Overflow maps torrent IDs whose file-row count exceeded maxFiles: the
	// reader drained their rows without retention, and the pipeline must
	// reject these records with a codec-limit error instead of pretending
	// it read a complete list.
	Overflow map[int32]int64
}

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
// maxFiles is the production codec's MaxFiles: a torrent with more file
// rows than that cannot survive validation anyway, so the reader drains and
// counts those rows instead of retaining an uncontrolled slice.
func (s *Source) Window(ctx context.Context, afterID int64, limit int, maxFiles int) (*Window, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("window limit must be positive, got %d", limit)
	}
	if maxFiles <= 0 {
		return nil, fmt.Errorf("maxFiles must be positive, got %d", maxFiles)
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
		currentKnown    bool
		currentOverflow bool
	)
	// finishGroup finalizes the group just ended: an overflowing torrent
	// loses its retained rows and is recorded exactly once.
	finishGroup := func() {
		if currentKnown && currentOverflow {
			delete(w.Files, current)
			w.OverflowRecord(current, currentCount)
		}
	}
	for frows.Next() {
		var f FileRow
		if err := frows.Scan(&f.ID, &f.TorrentID, &f.Size, &f.Path); err != nil {
			return nil, err
		}
		if f.TorrentID != current {
			finishGroup()
			current, currentCount, currentKnown, currentOverflow = f.TorrentID, 0, false, false
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
		if currentCount > int64(maxFiles) {
			// Drain without retention: stop keeping rows for this torrent.
			currentOverflow = true
			continue
		}
		w.Files[f.TorrentID] = append(w.Files[f.TorrentID], f)
	}
	if err := frows.Err(); err != nil {
		return nil, err
	}
	finishGroup()
	return w, nil
}

// OverflowRecord records a torrent whose file rows exceeded maxFiles. The
// rows were drained; the count is preserved for the per-record error.
func (w *Window) OverflowRecord(torrentID int32, observedCount int64) {
	if w.Overflow == nil {
		w.Overflow = map[int32]int64{}
	}
	w.Overflow[torrentID] = observedCount
}
