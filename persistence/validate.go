package persistence

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/skobkin/magnetico-web/codec"
)

// v1InfoHashLength is the SHA-1 infohash length of the v1 pipeline. It is
// enforced in Go validation only — deliberately never as a schema
// constraint — so a future v2/v1+pipeline with 32-byte hashes can lift it
// without a database migration (postgresql-persistence-v2 §2, BYTEA stays
// unconstrained).
const v1InfoHashLength = 20

// maxFileCountInt32 caps file counts at what the torrents.file_count column
// (INTEGER) can hold.
const maxFileCountInt32 = math.MaxInt32

// validateIngest checks an AddTorrent request against the ingestion rules
// before any codec work or database transaction is opened (spec §4.1): the
// infohash shape, torrent metadata, file-list constraints, per-file
// validity, and the checked sum equal to TotalSize. Invalid metadata is
// never silently repaired.
//
// meta.FileCount is output-only (it is derived from len(files) at insert
// time) and is not validated here.
func validateIngest(meta Torrent, files []File, limits codec.Limits) error {
	if len(meta.InfoHash) != v1InfoHashLength {
		return fmt.Errorf("%w: infohash must be exactly %d bytes, got %d",
			ErrInvalidMetadata, v1InfoHashLength, len(meta.InfoHash))
	}
	if meta.TotalSize <= 0 {
		return fmt.Errorf("%w: total size must be positive, got %d",
			ErrInvalidMetadata, meta.TotalSize)
	}
	if meta.DiscoveredOn <= 0 {
		return fmt.Errorf("%w: discovered-on must be a positive unix timestamp, got %d",
			ErrInvalidMetadata, meta.DiscoveredOn)
	}
	if !utf8.ValidString(meta.Name) || strings.ContainsRune(meta.Name, '\x00') {
		return fmt.Errorf("%w: %w: torrent name", ErrInvalidMetadata, ErrInvalidTextEncoding)
	}

	if len(files) == 0 {
		return fmt.Errorf("%w: empty file list", ErrInvalidMetadata)
	}
	if len(files) > limits.MaxFiles || len(files) > maxFileCountInt32 {
		return fmt.Errorf("%w: file count %d exceeds the limit %d",
			ErrInvalidMetadata, len(files), min(limits.MaxFiles, maxFileCountInt32))
	}

	var sum int64
	for _, f := range files {
		if f.Size < 0 {
			return fmt.Errorf("%w: file size is negative", ErrInvalidMetadata)
		}
		if !utf8.ValidString(f.Path) || strings.ContainsRune(f.Path, '\x00') {
			return fmt.Errorf("%w: %w: file path", ErrInvalidMetadata, ErrInvalidTextEncoding)
		}
		if sum > math.MaxInt64-f.Size {
			return fmt.Errorf("%w: file sizes overflow the total size", ErrInvalidMetadata)
		}
		sum += f.Size
	}
	if sum != meta.TotalSize {
		return fmt.Errorf("%w: file sizes sum to %d, but total size is %d",
			ErrInvalidMetadata, sum, meta.TotalSize)
	}
	return nil
}

// noBlobEligible reports the only shape that may be persisted without a
// file-list blob (spec §4.2): a single file whose path is the torrent name
// and whose size is the total size. Atypical single files keep their blob.
func noBlobEligible(meta Torrent, files []File) bool {
	return len(files) == 1 && files[0].Path == meta.Name && files[0].Size == meta.TotalSize
}

// wrapEncodeError classifies a codec encode failure for ingestion. Failures
// caused by the incoming metadata (limit violations, invalid lists, oversized
// blobs) become ErrInvalidMetadata wrapping the underlying typed codec error
// via a dual %w, so errors.Is and errors.As match both the category and the
// codec error. Such records follow the throttled WARN + drop + metric path
// and are never counted as database failures; actual database/transaction
// errors are wrapped as ErrStorageIO and stay distinct.
func wrapEncodeError(err error) error {
	return fmt.Errorf("%w: %w", ErrInvalidMetadata, err)
}
