package original

// Forensic snapshot reads (task 04C): complete source records for
// `migrate errors export --include-records`, beyond the codec/retention
// caps that Window and FetchByIDs apply. A malformed oversized source
// record needs its complete file list — drained overflow is unusable for
// forensics, so these reads stream without any retention policy.

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Snapshot is a restricted read handle over ONE `REPEATABLE READ READ
// ONLY` source transaction: both forensic queries of a record batch run
// against the same per-database snapshot. It exposes only the two reads
// below — no escapes to the underlying transaction or pool, so the
// adapter's read-only session policy and exact-schema probe stay the only
// access path (no second connection route is introduced).
type Snapshot struct {
	tx pgx.Tx
}

// WithSnapshot runs fn inside one REPEATABLE READ READ ONLY transaction on
// the source pool (the WindowSnapshot precedent). The transaction commits
// after fn returns nil and is rolled back on any error or cancellation —
// the source is never written either way (the session default is
// read-only). One snapshot per call: two calls are never cross-atomic, and
// no cross-database snapshot pair is ever atomic — operator-enforced
// source immutability stays essential, as documented for WindowSnapshot.
func (s *Source) WithSnapshot(ctx context.Context, fn func(ctx context.Context, snap *Snapshot) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err := fn(ctx, &Snapshot{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// FetchTorrentRows fetches a sparse ID set set-oriented (one query, ordered
// by id), with NO retention caps — forensic metadata is one row per
// torrent. Requested IDs absent from the source are simply absent from the
// result: the caller may report proven absence only because this query
// succeeded.
func (snap *Snapshot) FetchTorrentRows(ctx context.Context, ids []int64) ([]TorrentRow, error) {
	ids = normalizedIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := snap.tx.Query(ctx, `SELECT id, info_hash, name, total_size, discovered_on
        FROM magneticod.torrents
        WHERE id = ANY($1::bigint[])
        ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TorrentRow
	for rows.Next() {
		var t TorrentRow
		if err := rows.Scan(&t.ID, &t.InfoHash, &t.Name, &t.TotalSize, &t.DiscoveredOn); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StreamFileRows streams EVERY file row of the given torrent IDs in
// (torrent_id, id) order, invoking fn per row. There are no codec caps and
// no truncation: a malformed record with far more files than any production
// limit still exports completely, duplicates and source file IDs preserved.
// Rows stream from the driver one at a time — memory stays O(1) in file
// count. Orphan rows (NULL torrent_id) fail the ANY() match and are
// structurally excluded: the forensic selection is by torrent ID.
func (snap *Snapshot) StreamFileRows(ctx context.Context, torrentIDs []int64, fn func(FileRow) error) error {
	torrentIDs = normalizedIDs(torrentIDs)
	if len(torrentIDs) == 0 {
		return nil
	}
	rows, err := snap.tx.Query(ctx, `SELECT id, torrent_id, size, path
        FROM magneticod.files
        WHERE torrent_id = ANY($1::bigint[])
        ORDER BY torrent_id, id`, torrentIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f FileRow
		if err := rows.Scan(&f.ID, &f.TorrentID, &f.Size, &f.Path); err != nil {
			return err
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	return rows.Err()
}
