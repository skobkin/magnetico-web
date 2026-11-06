package original

import (
	"context"
	"time"
)

// Fingerprint is the sanitized source identity recorded in migration run
// state and revalidated on every resume. It contains server identifiers and
// aggregate counts only — no torrent names, no file paths.
//
// It documents that the source stayed unchanged only as far as these weak
// anchors go; the operator must keep the source offline, and verify-content
// (04B) is the real equality gate. Aliased endpoints of one server cannot be
// proven equal or distinct from the address/port (NULL over Unix sockets) —
// the authoritative same-database decision lives in pgidentity.
type Fingerprint struct {
	// ServerAddress/ServerPort are informational (empty over Unix sockets);
	// they never feed the same-database decision.
	ServerAddress   string    `json:"server_address,omitempty"`
	ServerPort      string    `json:"server_port,omitempty"`
	PostmasterStart time.Time `json:"postmaster_start_time"`
	Database        string    `json:"database"`

	Torrents          int64  `json:"torrents"`
	MinID             int64  `json:"min_id"`
	MaxID             int64  `json:"max_id"`
	AnchorMinInfoHash string `json:"anchor_min_info_hash,omitempty"`
	AnchorMaxInfoHash string `json:"anchor_max_info_hash,omitempty"`

	FileRows       int64 `json:"file_rows"`
	NullOwnerFiles int64 `json:"null_owner_files"`
	OrphanFiles    int64 `json:"orphan_files"`
}

// Fingerprint aggregates the source state: identity, torrent/file counts,
// ID bounds, and the orphan/null-owner counts the integrity policy needs.
func (s *Source) Fingerprint(ctx context.Context) (Fingerprint, error) {
	var fp Fingerprint

	identity, err := s.Identity(ctx)
	if err != nil {
		return Fingerprint{}, err
	}
	fp.ServerAddress = identity.ServerAddress
	fp.ServerPort = identity.ServerPort
	fp.PostmasterStart = identity.PostmasterStart
	fp.Database = identity.Database

	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*), COALESCE(MIN(id), 0), COALESCE(MAX(id), 0)
        FROM magneticod.torrents`).Scan(&fp.Torrents, &fp.MinID, &fp.MaxID); err != nil {
		return Fingerprint{}, err
	}
	if fp.Torrents > 0 {
		if err := s.pool.QueryRow(ctx, `SELECT
              (SELECT encode(info_hash, 'hex') FROM magneticod.torrents
                WHERE id = (SELECT MIN(id) FROM magneticod.torrents)),
              (SELECT encode(info_hash, 'hex') FROM magneticod.torrents
                WHERE id = (SELECT MAX(id) FROM magneticod.torrents))`,
		).Scan(&fp.AnchorMinInfoHash, &fp.AnchorMaxInfoHash); err != nil {
			return Fingerprint{}, err
		}
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM magneticod.files`).Scan(&fp.FileRows); err != nil {
		return Fingerprint{}, err
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM magneticod.files WHERE torrent_id IS NULL`).Scan(&fp.NullOwnerFiles); err != nil {
		return Fingerprint{}, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM magneticod.files f
        LEFT JOIN magneticod.torrents t ON t.id = f.torrent_id
        WHERE f.torrent_id IS NOT NULL AND t.id IS NULL`).Scan(&fp.OrphanFiles); err != nil {
		return Fingerprint{}, err
	}
	return fp, nil
}
