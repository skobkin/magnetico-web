package persistence

// Parity tests for the exported ingestion-validation seam: the exported
// wrappers must behave exactly like the unexported implementations used by
// AddTorrent, because the offline migrator relies on them as the single
// source of validation truth.
import (
	"errors"
	"testing"

	"github.com/skobkin/magnetico-web/codec"
)

func seamCaseTorrents() []struct {
	name  string
	meta  Torrent
	files []File
} {
	ok := Torrent{InfoHash: make([]byte, 20), Name: "t", TotalSize: 3, DiscoveredOn: 1}
	return []struct {
		name  string
		meta  Torrent
		files []File
	}{
		{"valid multi-file", ok, []File{{Path: "a", Size: 1}, {Path: "b", Size: 2}}},
		{"valid no-blob single", ok, []File{{Path: "t", Size: 3}}},
		{"valid atypical single", ok, []File{{Path: "x", Size: 3}}},
		{"bad infohash", Torrent{InfoHash: []byte{1}, Name: "t", TotalSize: 3, DiscoveredOn: 1},
			[]File{{Path: "a", Size: 3}}},
		{"zero total", Torrent{InfoHash: make([]byte, 20), Name: "t", TotalSize: 0, DiscoveredOn: 1},
			[]File{{Path: "a", Size: 0}}},
		{"non-positive discovered-on", Torrent{InfoHash: make([]byte, 20), Name: "t", TotalSize: 3, DiscoveredOn: 0},
			[]File{{Path: "a", Size: 3}}},
		{"name with NUL", Torrent{InfoHash: make([]byte, 20), Name: "a\x00b", TotalSize: 3, DiscoveredOn: 1},
			[]File{{Path: "a", Size: 3}}},
		{"empty file list", ok, nil},
		{"negative file size", ok, []File{{Path: "a", Size: -1}, {Path: "b", Size: 4}}},
		{"path with NUL", ok, []File{{Path: "a\x00", Size: 3}}},
		{"sum mismatch", ok, []File{{Path: "a", Size: 1}, {Path: "b", Size: 1}}},
	}
}

func TestValidateIngestSeamParity(t *testing.T) {
	t.Parallel()

	limits := codec.DefaultLimits()
	for _, tc := range seamCaseTorrents() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wantErr := validateIngest(tc.meta, tc.files, limits)
			gotErr := ValidateIngest(tc.meta, tc.files, limits)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("ValidateIngest() error = %v, internal = %v", gotErr, wantErr)
			}
			if wantErr != nil && wantErr.Error() != gotErr.Error() {
				t.Fatalf("ValidateIngest() error = %q, internal = %q", gotErr, wantErr)
			}

			wantEligible := noBlobEligible(tc.meta, tc.files)
			gotEligible := NoBlobEligible(tc.meta, tc.files)
			if wantEligible != gotEligible {
				t.Fatalf("NoBlobEligible() = %v, internal = %v", gotEligible, wantEligible)
			}
		})
	}
}

func TestWrapEncodeErrorSeamParity(t *testing.T) {
	t.Parallel()

	inner := errors.New("codec failure")
	wrapped := WrapEncodeError(inner)
	if !errors.Is(wrapped, ErrInvalidMetadata) || !errors.Is(wrapped, inner) {
		t.Fatalf("WrapEncodeError() = %v, want dual-wrapping of ErrInvalidMetadata and the inner error", wrapped)
	}
	if wrapped.Error() != wrapEncodeError(inner).Error() {
		t.Fatalf("WrapEncodeError() text = %q, internal = %q", wrapped, wrapEncodeError(inner))
	}
}

func TestNoBlobEligibleSeamExactPredicate(t *testing.T) {
	t.Parallel()

	meta := Torrent{InfoHash: make([]byte, 20), Name: "n", TotalSize: 5, DiscoveredOn: 1}
	cases := []struct {
		name  string
		files []File
		want  bool
	}{
		{"exact single", []File{{Path: "n", Size: 5}}, true},
		{"atypical single", []File{{Path: "other", Size: 5}}, false},
		{"wrong size single", []File{{Path: "n", Size: 4}}, false},
		{"two files", []File{{Path: "n", Size: 2}, {Path: "n", Size: 3}}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NoBlobEligible(meta, tc.files); got != tc.want {
				t.Fatalf("NoBlobEligible() = %v, want %v", got, tc.want)
			}
		})
	}
}
