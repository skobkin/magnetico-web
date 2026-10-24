package persistence

import "errors"

// Typed error categories reported by Store operations (postgresql-persistence
// v2 §5). Match with errors.Is; every error text is fixed and free of file
// paths and credentials — database URLs are redacted by the logging layer.
var (
	// ErrTorrentNotFound reports that no torrent row exists for the given
	// infohash (maps to HTTP 404 in the future web integration, never to a
	// database error).
	ErrTorrentNotFound = errors.New("torrent not found")

	// ErrIntegrityMissingBlob reports a multi-file torrent whose file-list
	// blob row is absent. Never an empty file list as success.
	ErrIntegrityMissingBlob = errors.New("file list blob is missing for a multi-file torrent")

	// ErrCorruptBlob reports a stored blob that fails to decode, violates
	// the wire format, or whose decoded count/total disagree with the
	// torrents row. Never a partial list.
	ErrCorruptBlob = errors.New("stored file list blob is corrupt or inconsistent")

	// ErrUnsupportedSchema reports a present-but-unsupported schema state;
	// the operator hint points at `magnetico migrate schema-init`.
	ErrUnsupportedSchema = errors.New(
		"database schema is not supported (hint: run `magnetico migrate schema-init`)")

	// ErrMissingExtension reports that the pg_trgm extension is not
	// available; the fixed text carries the operator hint.
	ErrMissingExtension = errors.New(
		"PostgreSQL extension pg_trgm is missing (hint: run `CREATE EXTENSION pg_trgm;` as superuser)")

	// ErrInvalidMetadata reports ingestion input that violates the storage
	// contract (infohash shape, sizes, sum, encodings, limits) or a codec
	// encode failure caused by such input. These records follow the
	// throttled WARN + drop + metric path and are never database failures.
	ErrInvalidMetadata = errors.New("torrent metadata is invalid")

	// ErrInvalidTextEncoding reports the UTF-8/NUL bucket of
	// ErrInvalidMetadata: torrent names or file paths that are not valid
	// UTF-8 or contain NUL bytes. It is wrapped beneath ErrInvalidMetadata
	// (dual %w) so the wiring can route these records to the dedicated
	// non-UTF-8 metric while every other check keeps matching only
	// ErrInvalidMetadata.
	ErrInvalidTextEncoding = errors.New("text is not NUL-free valid UTF-8")

	// ErrStorageIO wraps an actual database/transaction failure. It is kept
	// distinct from ErrInvalidMetadata so callers can classify reliably.
	ErrStorageIO = errors.New("storage I/O error")
)
