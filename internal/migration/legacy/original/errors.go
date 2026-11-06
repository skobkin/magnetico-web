// Package original is the source adapter for the original
// boramalper/magnetico v0.12.0 PostgreSQL layout: an unpartitioned
// `magneticod` schema with torrents/files/migrations and schema version 0.
// It owns the frozen source record types (deliberately separate from
// persistence.Torrent and codec.File), the read-only catalog probe, the
// sanitized fingerprint and the bounded keyset reader.
//
// Every query against the source is SELECT-only and schema-qualified; the
// adapter's connections additionally run with default_transaction_read_only
// so an adapter bug cannot write to the source. Migration of a v1 fork
// (tgragnato layout) is explicitly refused here.
package original

import "errors"

var (
	// ErrSourceNotMagnetico reports a database with no magneticod schema or
	// none of the magnetico tables: there is nothing to migrate.
	ErrSourceNotMagnetico = errors.New(
		"source database is not a magnetico database (no magneticod schema or tables found)")

	// ErrSourceV1Fork reports a magnetico v1 fork layout (partitioned files
	// or schema version 1). This migrator imports only the original v0.12.0
	// layout; fork migration is a separate, unimplemented adapter.
	ErrSourceV1Fork = errors.New(
		"source database is a magnetico v1 fork (partitioned files or schema_version 1); " +
			"this tool migrates only the original v0.12.0 layout")

	// ErrSourceUnsupported reports a detected layout that is neither absent,
	// nor a v1 fork, nor exactly the original v0.12.0 schema. Details name
	// the offending catalog objects.
	ErrSourceUnsupported = errors.New(
		"source database layout is not the original magnetico v0.12.0 schema")
)

// SourceStateError reports a refused source state, naming the detected
// catalog objects. It unwraps to one of the package sentinels.
type SourceStateError struct {
	Err     error
	Details []string
}

func (e *SourceStateError) Error() string {
	out := e.Err.Error()
	for _, d := range e.Details {
		out += "; " + d
	}
	return out
}

func (e *SourceStateError) Unwrap() error { return e.Err }
