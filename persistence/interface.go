// Package persistence defines the narrow storage interface required by the
// crawler.
//
// PostgreSQL is the only backend of release 0.8.0. It will be implemented in
// phase 3 on top of docs/specs/postgresql-persistence-v2.md (context-aware
// Store, torrent file lists stored as encoded blobs). Until then no backend
// exists and MakeDatabase fails closed for every URL scheme.
//
// Derived from tgragnato/magnetico persistence/interface.go (AGPL-3.0, see
// COPYING). The removed backends (SQLite3, legacy PostgreSQL with its
// automatic v0->v1 files migration, RabbitMQ, ZeroMQ, Bitmagnet) and the
// JSONL export/import machinery remain recoverable from Git history.
package persistence

import (
	"errors"
	"fmt"
	"net/url"
)

// File is a single file entry of a torrent, as extracted from its metainfo.
type File struct {
	Size int64  `json:"size"`
	Path string `json:"path"`
}

// Database is the write surface required by the crawler. The read surface for
// the future web process will be added in phase 3 alongside the PostgreSQL
// implementation; see docs/specs/postgresql-persistence-v2.md, section 3.
type Database interface {
	DoesTorrentExist(infoHash []byte) (bool, error)
	AddNewTorrent(infoHash []byte, name string, files []File) error
	Close() error
}

// ErrStorageNotImplemented is returned by MakeDatabase until phase 3 implements
// the new PostgreSQL storage. It is fail-closed by design: no legacy schema is
// read, created or migrated.
var ErrStorageNotImplemented = errors.New(
	"storage backends are not implemented yet " +
		"(phase 3, docs/wip/03-storage-persistence.md); refusing to touch any existing database",
)

// MakeDatabase refuses every database URL until the phase-3 storage
// implementation exists. It never opens a connection and never performs any
// schema operation. The scheme is parsed only for diagnostics.
func MakeDatabase(rawURL string) (Database, error) {
	scheme := ""
	if parsed, err := url.Parse(rawURL); err == nil {
		scheme = parsed.Scheme
	}
	return nil, fmt.Errorf("%w (requested scheme %q)", ErrStorageNotImplemented, scheme)
}
