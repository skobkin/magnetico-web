package pipeline

// Neutral record model and per-record outcome taxonomy. The pipeline is
// source-agnostic: adapters convert their rows into Records (or pre-settled
// Failures) and hand them to Run.

import (
	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// Record is one source torrent in neutral form, independent of any source
// schema. SourceFileID is retained for forensic export (04C) and is never
// mapped into the codec.
type Record struct {
	SourceID     int32
	InfoHash     []byte
	Name         string
	TotalSize    int64
	DiscoveredOn int64
	Files        []FileEntry
}

// FileEntry is one file of a Record.
type FileEntry struct {
	Path         string
	Size         int64
	SourceFileID int32
}

// Weight is the in-flight memory the record charges against the byte
// semaphore: its decoded path bytes plus the torrent name.
func (r *Record) Weight() int64 {
	w := int64(len(r.Name))
	for _, f := range r.Files {
		w += int64(len(f.Path))
	}
	return w
}

// Success is a settled record that will be written to the target. Blob is
// nil exactly when the record is no-blob eligible (single typical file).
type Success struct {
	Record *Record
	Blob   []byte
}

// Pipeline stages for the error registry (the errors.stage column). The
// pipeline produces read, validate, encode and decode_verify; write and
// preflight belong to the target layer.
const (
	StageRead         = "read"
	StageValidate     = "validate"
	StageEncode       = "encode"
	StageDecodeVerify = "decode_verify"
)

// Error taxonomy: stable codes stored in the error registry.
const (
	CodeInvalidMetadata   = "invalid_metadata"
	CodeCodecLimit        = "codec_limit"
	CodeMissingFiles      = "missing_files"
	CodeFileCountExceeded = "file_count_exceeded"
	CodeDecodeMismatch    = "decode_mismatch"
	CodeDecodeFailure     = "decode_failure"
	CodeSourceIntegrity   = "source_integrity"
)

// Failure is a settled per-record error (scope record, category integrity).
// Every scanned record produces exactly one Success or one Failure.
type Failure struct {
	SourceID int64
	InfoHash []byte
	Stage    string
	Code     string
	Details  map[string]any
}

// ErrorRecord renders the failure for the migration error registry.
func (f *Failure) ErrorRecord() state.ErrorRecord {
	id := f.SourceID
	return state.ErrorRecord{
		Scope:          state.ScopeRecord,
		SourceID:       &id,
		SourceInfoHash: f.InfoHash,
		Stage:          f.Stage,
		Category:       state.CategoryIntegrity,
		Code:           f.Code,
		Details:        f.Details,
	}
}
