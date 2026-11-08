package pipeline

// Per-record processing: production-identical validation and encoding.
// Validation and no-blob eligibility come from the exported persistence
// seam (single source of ingestion truth); encoding goes through one shared
// codec.Encoder whose internal semaphore is the CPU bound. Each produced
// blob is immediately decoded and compared by canonical CONTENT — re-encode
// byte equality is forbidden (pooled-Brotli determinism caveat,
// wire-format-v1 §6).

import (
	"cmp"
	"errors"
	"slices"
	"strings"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/persistence"
)

type Processor struct {
	limits codec.Limits
	enc    *codec.Encoder
	verify bool
}

// NewProcessor builds the record Processor. The encoder is shared by all
// worker goroutines and bounds encode concurrency internally.
func NewProcessor(limits codec.Limits, profile codec.Profile, workers int, verify bool) *Processor {
	return &Processor{
		limits: limits,
		enc:    codec.NewEncoder(profile, limits, workers),
		verify: verify,
	}
}

// process validates, encodes and (optionally) verifies one record. It
// returns exactly one of (success, nil) or (nil, failure).
func (p *Processor) Process(rec *Record) (*Success, *Failure) {
	files := make([]codec.File, len(rec.Files))
	for i, f := range rec.Files {
		files[i] = codec.File{Path: f.Path, Size: f.Size}
	}
	meta := persistence.Torrent{
		ID:           rec.SourceID,
		InfoHash:     rec.InfoHash,
		Name:         rec.Name,
		TotalSize:    rec.TotalSize,
		DiscoveredOn: rec.DiscoveredOn,
	}

	if err := persistence.ValidateIngest(meta, files, p.limits); err != nil {
		return nil, &Failure{
			SourceID: int64(rec.SourceID),
			InfoHash: rec.InfoHash,
			Stage:    StageValidate,
			Code:     CodeInvalidMetadata,
			Details:  map[string]any{"reason": err.Error()},
		}
	}

	// A single typical file (path == name, size == total) is persisted
	// without a blob; atypical singles keep theirs (spec §4.2).
	if persistence.NoBlobEligible(meta, files) {
		return &Success{Record: rec}, nil
	}

	blob, _, err := p.enc.Encode(files)
	if err != nil {
		details := map[string]any{"reason": err.Error()}
		var le *codec.LimitError
		if errors.As(err, &le) {
			details["limit"] = le.Limit
			details["actual"] = le.Actual
			details["allowed"] = le.Allowed
		}
		return nil, &Failure{
			SourceID: int64(rec.SourceID),
			InfoHash: rec.InfoHash,
			Stage:    StageEncode,
			Code:     CodeCodecLimit,
			Details:  details,
		}
	}

	if p.verify {
		decoded, derr := codec.Decode(blob, p.limits)
		if derr != nil {
			details := map[string]any{"reason": derr.Error()}
			var le *codec.LimitError
			if errors.As(derr, &le) {
				details["limit"] = le.Limit
				details["actual"] = le.Actual
				details["allowed"] = le.Allowed
			}
			return nil, &Failure{
				SourceID: int64(rec.SourceID),
				InfoHash: rec.InfoHash,
				Stage:    StageDecodeVerify,
				Code:     CodeDecodeFailure,
				Details:  details,
			}
		}
		if !EqualCanonical(decoded, files) {
			return nil, &Failure{
				SourceID: int64(rec.SourceID),
				InfoHash: rec.InfoHash,
				Stage:    StageDecodeVerify,
				Code:     CodeDecodeMismatch,
				Details: map[string]any{
					"source_files":  len(files),
					"decoded_files": len(decoded),
				},
			}
		}
	}

	return &Success{Record: rec, Blob: blob}, nil
}

// CompareCanonical mirrors codec's canonical order (path bytes ascending,
// then size ascending) so the verification compare is exact even for
// duplicate paths.
func CompareCanonical(a, b codec.File) int {
	if c := strings.Compare(a.Path, b.Path); c != 0 {
		return c
	}
	return cmp.Compare(a.Size, b.Size)
}

// EqualCanonical reports whether decoded (already canonically sorted by
// codec.Decode) equals the source file list under canonical ordering.
func EqualCanonical(decoded, source []codec.File) bool {
	if len(decoded) != len(source) {
		return false
	}
	sorted := make([]codec.File, len(source))
	copy(sorted, source)
	slices.SortFunc(sorted, CompareCanonical)
	for i := range decoded {
		if decoded[i] != sorted[i] {
			return false
		}
	}
	return true
}
