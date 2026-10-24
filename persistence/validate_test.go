package persistence

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/codec"
)

func validMeta() Torrent {
	return Torrent{
		InfoHash:     []byte("01234567890123456789"), // 20 bytes
		Name:         "release",
		TotalSize:    300,
		DiscoveredOn: 1700000000,
	}
}

func validFiles() []File {
	return []File{
		{Path: "a/one.bin", Size: 100},
		{Path: "a/two.bin", Size: 200},
	}
}

func TestValidateIngestAcceptsValidInput(t *testing.T) {
	t.Parallel()

	meta := validMeta()
	if err := validateIngest(meta, validFiles(), codec.DefaultLimits()); err != nil {
		t.Errorf("validateIngest() error = %v, want nil", err)
	}
	if noBlobEligible(meta, validFiles()) {
		t.Error("multi-file list reported no-blob eligible")
	}
}

func TestValidateIngestRejections(t *testing.T) {
	t.Parallel()

	limits := codec.DefaultLimits()
	// TotalSize must remain 1 for the count case to fail on count, not sum:
	// one file of size 1 plus zero-size files sums to 1.
	tooManySum := make([]File, limits.MaxFiles+1)
	tooManySum[0] = File{Path: "f0", Size: 1}

	tests := []struct {
		name         string
		meta         Torrent
		files        []File
		wantEncoding bool // also matches ErrInvalidTextEncoding (UTF-8/NUL bucket)
	}{
		{
			name:  "empty infohash",
			meta:  func() Torrent { m := validMeta(); m.InfoHash = nil; return m }(),
			files: validFiles(),
		},
		{
			name:  "19-byte infohash",
			meta:  func() Torrent { m := validMeta(); m.InfoHash = m.InfoHash[:19]; return m }(),
			files: validFiles(),
		},
		{
			name:  "21-byte infohash",
			meta:  func() Torrent { m := validMeta(); m.InfoHash = append(m.InfoHash, 0); return m }(),
			files: validFiles(),
		},
		{
			name:  "32-byte infohash",
			meta:  func() Torrent { m := validMeta(); m.InfoHash = make([]byte, 32); return m }(),
			files: validFiles(),
		},
		{
			name:  "zero total size",
			meta:  func() Torrent { m := validMeta(); m.TotalSize = 0; return m }(),
			files: validFiles(),
		},
		{
			name:  "negative total size",
			meta:  func() Torrent { m := validMeta(); m.TotalSize = -1; return m }(),
			files: validFiles(),
		},
		{
			name:  "zero discovered-on",
			meta:  func() Torrent { m := validMeta(); m.DiscoveredOn = 0; return m }(),
			files: validFiles(),
		},
		{
			name:  "negative discovered-on",
			meta:  func() Torrent { m := validMeta(); m.DiscoveredOn = -42; return m }(),
			files: validFiles(),
		},
		{
			name:         "name with NUL",
			meta:         func() Torrent { m := validMeta(); m.Name = "rele\x00ase"; return m }(),
			files:        validFiles(),
			wantEncoding: true,
		},
		{
			name:         "name with invalid UTF-8",
			meta:         func() Torrent { m := validMeta(); m.Name = "rele\xffase"; return m }(),
			files:        validFiles(),
			wantEncoding: true,
		},
		{
			name: "empty file list",
			meta: validMeta(),
		},
		{
			name:  "file count over the limit",
			meta:  func() Torrent { m := validMeta(); m.TotalSize = 1; return m }(),
			files: tooManySum,
		},
		{
			name:  "negative file size",
			meta:  func() Torrent { m := validMeta(); m.TotalSize = 99; return m }(),
			files: []File{{Path: "a", Size: 100}, {Path: "b", Size: -1}},
		},
		{
			name:         "path with NUL",
			meta:         validMeta(),
			files:        []File{{Path: "a\x00b", Size: 300}},
			wantEncoding: true,
		},
		{
			name:         "path with invalid UTF-8",
			meta:         validMeta(),
			files:        []File{{Path: "a\xffb", Size: 300}},
			wantEncoding: true,
		},
		{
			name:  "sum does not match total size",
			meta:  func() Torrent { m := validMeta(); m.TotalSize = 301; return m }(),
			files: validFiles(),
		},
		{
			name:  "sum overflows int64",
			meta:  func() Torrent { m := validMeta(); m.TotalSize = math.MaxInt64; return m }(),
			files: []File{{Path: "a", Size: math.MaxInt64}, {Path: "b", Size: 1}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateIngest(tt.meta, tt.files, codec.DefaultLimits())
			if !errors.Is(err, ErrInvalidMetadata) {
				t.Errorf("validateIngest() error = %v, want ErrInvalidMetadata", err)
			}
			if got := errors.Is(err, ErrInvalidTextEncoding); got != tt.wantEncoding {
				t.Errorf("validateIngest() matches ErrInvalidTextEncoding = %v, want %v", got, tt.wantEncoding)
			}
		})
	}
}

func TestValidateIngestMaxFilesBoundaryAccepted(t *testing.T) {
	t.Parallel()

	// The inclusive MaxFiles case from the table above, verified to succeed.
	meta := validMeta()
	meta.TotalSize = 1
	files := make([]File, codec.DefaultLimits().MaxFiles)
	files[0] = File{Path: "first", Size: 1}
	if err := validateIngest(meta, files, codec.DefaultLimits()); err != nil {
		t.Errorf("validateIngest() error = %v, want nil", err)
	}
}

func TestNoBlobEligibility(t *testing.T) {
	t.Parallel()

	typical := Torrent{
		InfoHash:     []byte("01234567890123456789"),
		Name:         "film.mkv",
		TotalSize:    1234,
		DiscoveredOn: 1700000000,
	}
	if !noBlobEligible(typical, []File{{Path: "film.mkv", Size: 1234}}) {
		t.Error("typical single file not reported no-blob eligible")
	}
	// Atypical single file (vector B): name differs -> blob required.
	atypical := []File{{Path: "Different.mkv", Size: 1234}}
	if noBlobEligible(typical, atypical) {
		t.Error("atypical single file reported no-blob eligible")
	}
	// Size mismatch -> blob required.
	sizes := []File{{Path: "film.mkv", Size: 1233}}
	if noBlobEligible(typical, sizes) {
		t.Error("size-mismatched single file reported no-blob eligible")
	}
}

// TestEncodeErrorClassification pins the owner-mandated classification: a
// codec encode failure caused by invalid incoming metadata is wrapped as
// ErrInvalidMetadata with the underlying typed codec error preserved (dual
// %w), so errors.Is matches both and the record follows the WARN + drop +
// metric path — never the database-failure path.
func TestEncodeErrorClassification(t *testing.T) {
	t.Parallel()

	limits := codec.DefaultLimits()
	limits.MaxFiles = 2
	_, _, err := codec.Encode([]File{
		{Path: "a", Size: 1}, {Path: "b", Size: 2}, {Path: "c", Size: 3},
	}, codec.DefaultProfile(), limits)
	var limitErr *codec.LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("codec.Encode() error = %v, want a codec LimitError", err)
	}

	wrapped := wrapEncodeError(err)
	if !errors.Is(wrapped, ErrInvalidMetadata) {
		t.Errorf("wrapped error %v does not match ErrInvalidMetadata", wrapped)
	}
	if !errors.Is(wrapped, err) {
		t.Errorf("wrapped error %v does not match the underlying codec error", wrapped)
	}
	var got *codec.LimitError
	if !errors.As(wrapped, &got) || got.Limit != limitErr.Limit {
		t.Errorf("wrapped error lost the structured codec detail: %v", wrapped)
	}
	if strings.Contains(wrapped.Error(), "\x00") {
		t.Error("wrapped error text unexpectedly contains NUL")
	}
}
