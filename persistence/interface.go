// Package persistence defines the storage contracts of release 0.8.0.
//
// Store (postgresql-persistence-v2 §3) is the context-aware interface backed
// by PostgreSQL in phase 3: torrent metadata lives in `magneticod.torrents`,
// file lists in encoded wire-format-v1 blobs (`magneticod.torrent_file_blobs`)
// via the codec package. MakeDatabase still fails closed for every URL until
// the PostgreSQL implementation lands; no legacy schema is ever read, created
// or migrated.
//
// Derived from tgragnato/magnetico persistence/interface.go (AGPL-3.0, see
// COPYING). The removed backends (SQLite3, legacy PostgreSQL with its
// automatic v0->v1 files migration, RabbitMQ, ZeroMQ, Bitmagnet) and the
// JSONL export/import machinery remain recoverable from Git history.
package persistence

import (
	"context"
	"errors"

	"github.com/skobkin/magnetico-web/codec"
)

// File is a single file entry of a torrent. It is the codec's file record:
// identical JSON representation (`path`, `size`) to the previous
// persistence-local struct, so nothing observable changes.
type File = codec.File

// SchemaVersion is the schema version the 0.8.0 store requires in
// `magneticod.schema_version` — exactly one row carrying this value.
const SchemaVersion = 1

// Torrent is one row of `magneticod.torrents`. On input to AddTorrent only
// InfoHash, Name, TotalSize and DiscoveredOn are meaningful; ID and FileCount
// are derived by the store.
type Torrent struct {
	ID           int32
	InfoHash     []byte
	Name         string
	TotalSize    int64
	DiscoveredOn int64
	FileCount    int32
}

// TorrentOrder selects the primary sort of QueryTorrents. Every ordering is
// made deterministic by the stable secondary key `id DESC`.
type TorrentOrder int

const (
	// ByDiscoveredOnDesc is the default ordering: newest first.
	ByDiscoveredOnDesc TorrentOrder = iota
	// ByTotalSizeDesc orders by total size, largest first.
	ByTotalSizeDesc
)

// TorrentQuery bounds and orders a metadata-only search. Substring search is
// case-insensitive and must never require decoding blobs.
type TorrentQuery struct {
	// SearchString is a literal substring of the torrent name. Empty means
	// no filtering. The caller's value is escaped and passed as a bound
	// parameter — it is never interpolated into SQL text.
	SearchString string
	// OrderBy selects the primary sort; zero value is ByDiscoveredOnDesc.
	OrderBy TorrentOrder
	// MaxTuples caps the number of returned rows; it must be positive.
	MaxTuples int
}

// Statistics aggregates the torrents table only — no blob decoding and no
// per-file join (spec §5).
type Statistics struct {
	Torrents  uint64 // COUNT(*)
	Files     uint64 // SUM(file_count)
	TotalSize uint64 // SUM(total_size)
}

// Store is the context-aware storage surface of release 0.8.0. Implementations
// must be safe for concurrent use. Error classification: ErrTorrentNotFound /
// ErrIntegrityMissingBlob / ErrCorruptBlob / ErrUnsupportedSchema /
// ErrMissingExtension / ErrInvalidMetadata / ErrStorageIO (see errors.go).
type Store interface {
	// AddTorrent inserts a torrent and, unless the no-blob single-file shape
	// applies, its encoded file list. created reports whether this call
	// inserted the row; for an already-known infohash it returns
	// (false, existingID, nil) without touching the stored data.
	AddTorrent(ctx context.Context, meta Torrent, files []File) (created bool, id int32, err error)

	// GetTorrent reads one torrents row by infohash. It never joins or
	// decodes blobs.
	GetTorrent(ctx context.Context, infoHash []byte) (Torrent, error)

	// GetFiles returns the complete file list of a torrent: the stored blob
	// decoded, or the (name, total_size) single-file representation for the
	// intentional no-blob shape. It never returns a partial list as success.
	GetFiles(ctx context.Context, infoHash []byte) ([]File, error)

	// QueryTorrents runs a bounded metadata-only search with deterministic
	// ordering (primary key per TorrentOrder, secondary `id DESC`).
	QueryTorrents(ctx context.Context, query TorrentQuery) ([]Torrent, error)

	// GetStatistics returns the torrents-table aggregates.
	GetStatistics(ctx context.Context) (Statistics, error)

	// DoesTorrentExist is a performance hint only; AddTorrent remains the
	// concurrency authority.
	DoesTorrentExist(ctx context.Context, infoHash []byte) (bool, error)

	Close() error
}

// Database is the legacy write surface still consumed by the crawler until
// the phase-3 rewiring replaces it with Store.
type Database interface {
	DoesTorrentExist(infoHash []byte) (bool, error)
	AddNewTorrent(infoHash []byte, name string, files []File) error
	Close() error
}

// ErrStorageNotImplemented is returned by MakeDatabase for every
// non-postgres scheme. It is fail-closed by design: no legacy schema is
// read, created or migrated.
var ErrStorageNotImplemented = errors.New(
	"storage backends other than postgres/postgresql are not supported " +
		"(0.8.0 is PostgreSQL-only); refusing to touch any existing database",
)

// ErrStorageNotImplemented is returned by MakeDatabase for every
// non-postgres scheme (declared in postgres.go together with the new
// MakeDatabase). It is fail-closed by design: no legacy schema is read,
// created or migrated.
