package codec

import (
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestPrefixRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files []File
	}{
		{
			name:  "shared directory prefixes",
			files: []File{{Path: "S/E01.mkv", Size: 1000}, {Path: "S/E02.mkv", Size: 1100}},
		},
		{
			name:  "identical paths share the full prefix",
			files: []File{{Path: "D/a.txt", Size: 0}, {Path: "D/a.txt", Size: 0}, {Path: "D/a.txt", Size: 7}},
		},
		{
			name:  "deep nesting",
			files: []File{{Path: "a/b/c/d/e/f.bin", Size: 1}, {Path: "a/b/c/d/g.bin", Size: 2}},
		},
		{
			name:  "unrelated paths share nothing",
			files: []File{{Path: "alpha", Size: 1}, {Path: "zeta", Size: 2}},
		},
		{
			name:  "unicode shared prefix",
			files: []File{{Path: "Музыка/01.flac", Size: 128}, {Path: "Музыка/02.flac", Size: 256}},
		},
		{
			name:  "empty path first",
			files: []File{{Path: "", Size: 0}, {Path: "a", Size: 1}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blob, _, err := encodeWith(PrefixDelta, tt.files, DefaultLimits())
			if err != nil {
				t.Fatalf("encodeWith() error = %v", err)
			}
			got, err := Decode(blob, DefaultLimits())
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.files) {
				t.Errorf("Decode() = %#v, want %#v", got, tt.files)
			}
		})
	}
}

// rawPrefix builds an uncompressed prefix-delta payload: count, then records
// of (shared, suffixLen, suffix, size).
func rawPrefix(records ...[]byte) []byte {
	out := appendUvarint(nil, uint64(len(records)))
	for _, r := range records {
		out = append(out, r...)
	}
	return out
}

func recPrefix(shared uint64, suffix string, size uint64) []byte {
	out := appendUvarint(nil, shared)
	out = appendUvarint(out, uint64(len(suffix)))
	out = append(out, suffix...)
	return appendUvarint(out, size)
}

func TestPrefixDecodeRejections(t *testing.T) {
	t.Parallel()

	// "ab" then canonical successor "abc": shared must be 2. A shared of 1
	// ("a") with suffix "bc" is canonical but nonmaximal — the first new
	// byte 'b' duplicates prev[1].
	nonmaximal := rawPrefix(recPrefix(0, "ab", 1), recPrefix(1, "bc", 1))
	// shared beyond the previous path length is structurally impossible.
	outOfRange := rawPrefix(recPrefix(0, "ab", 1), recPrefix(3, "c", 1))
	// A path that is a strict prefix of the previous one sorts BEFORE it,
	// so shared < len(prev) with an empty suffix is non-canonical.
	strictPrefix := rawPrefix(recPrefix(0, "ab", 1), recPrefix(1, "", 1))

	tests := []struct {
		name    string
		payload []byte
		wantErr error
	}{
		{name: "count zero", payload: []byte{0x00}, wantErr: ErrInvalidCount},
		{name: "nonmaximal shared prefix", payload: nonmaximal, wantErr: ErrNonMaximalPrefix},
		{name: "shared exceeds previous path", payload: outOfRange, wantErr: ErrPrefixOutOfRange},
		{name: "strict prefix of previous path", payload: strictPrefix, wantErr: ErrNonCanonicalOrder},
		{
			name:    "trailing byte",
			payload: append(rawPrefix(recPrefix(0, "ab", 1)), 0x00),
			wantErr: ErrTrailingBytes,
		},
		{
			name:    "suffix longer than payload",
			payload: append(appendUvarint(nil, 1), appendUvarint(appendUvarint(nil, 0), 9)...),
			wantErr: ErrTruncated,
		},
		{
			name:    "size overflows int64",
			payload: rawPrefix(recPrefixBytes(0, "ab", appendUvarint(nil, math.MaxInt64+1))),
			wantErr: ErrOverflow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodePrefix(tt.payload, DefaultLimits())
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("decodePrefix() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// recPrefixBytes builds a prefix record with an explicit pre-encoded size.
func recPrefixBytes(shared uint64, suffix string, sizeVarint []byte) []byte {
	out := appendUvarint(nil, shared)
	out = appendUvarint(out, uint64(len(suffix)))
	out = append(out, suffix...)
	return append(out, sizeVarint...)
}

func TestPrefixMaximalPrefixIsByteWise(t *testing.T) {
	t.Parallel()

	// A shared prefix ending mid-code-point is valid (vector E pins this);
	// the maximality check must compare bytes, never runes.
	blob, _, err := encodeWith(PrefixDelta, []File{
		{Path: "Музыка/01.flac", Size: 128},
		{Path: "Музыка/02.flac", Size: 256},
	}, DefaultLimits())
	if err != nil {
		t.Fatalf("encodeWith() error = %v", err)
	}
	if _, err := Decode(blob, DefaultLimits()); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
}

func TestPrefixAggregateLimitAbortsMidDecode(t *testing.T) {
	t.Parallel()

	// Prefix-delta can reconstruct large paths from a tiny payload; the
	// aggregate budget must abort during decoding, not afterwards.
	limits := DefaultLimits()
	limits.MaxAggregatePathBytes = 12
	payload := rawPrefix(
		recPrefix(0, "abcdefgh", 1), // 8 bytes
		recPrefix(8, "ijkl", 1),     // 12 bytes: at the budget
		recPrefix(12, "mnop", 1),    // 16 bytes: over
	)
	_, err := decodePrefix(payload, limits)
	var lerr *LimitError
	if !errors.As(err, &lerr) || lerr.Limit != LimitAggregatePathBytes {
		t.Errorf("decodePrefix() error = %v, want LimitError(%s)", err, LimitAggregatePathBytes)
	}
}

func TestPrefixMaximalPrefixEncodingMatchesGoldenLengths(t *testing.T) {
	t.Parallel()

	// The encoder emits shared = maximal common prefix. Parse the second
	// record's varints programmatically and assert shared=4, suffixLen=5
	// for the golden vector C pair (the full bytes are pinned exactly in
	// golden_test.go).
	files := []File{{Path: "S/E01.mkv", Size: 1000}, {Path: "S/E02.mkv", Size: 1100}}
	if got := commonPrefixLen(files[0].Path, files[1].Path); got != 4 {
		t.Errorf("commonPrefixLen = %d, want 4", got)
	}
	payload, err := encodePrefix(files)
	if err != nil {
		t.Fatalf("encodePrefix() error = %v", err)
	}
	// Skip count and the whole first record: shared(1)+len(1)+suffix(9)+size(2).
	rec := payload[1+1+1+9+2:]
	shared, n1 := binary.Uvarint(rec)
	suffixLen, _ := binary.Uvarint(rec[n1:])
	if shared != 4 || suffixLen != 5 {
		t.Errorf("second record = shared %d suffix %d, want 4 and 5", shared, suffixLen)
	}
}
