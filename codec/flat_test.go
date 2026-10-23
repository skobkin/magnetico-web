package codec

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestFlatRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files []File
	}{
		{name: "single file", files: []File{{Path: "a.bin", Size: 1}}},
		{name: "empty path sorts first", files: []File{{Path: "", Size: 5}, {Path: "b", Size: 1}}},
		{
			name:  "duplicate identical pairs",
			files: []File{{Path: "dup", Size: 7}, {Path: "dup", Size: 7}},
		},
		{
			name:  "duplicate paths ordered by size",
			files: []File{{Path: "dup", Size: 0}, {Path: "dup", Size: 2}, {Path: "dup", Size: 2}},
		},
		{name: "zero size file", files: []File{{Path: "empty", Size: 0}}},
		{name: "max int64 size", files: []File{{Path: "big", Size: math.MaxInt64}}},
		{
			name:  "unicode and irregular paths",
			files: []File{{Path: "a/../c", Size: 2}, {Path: "a//b", Size: 1}, {Path: "Музыка/01.flac", Size: 128}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blob, _, err := encodeWith(Flat, tt.files, DefaultLimits())
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

// rawFlat builds an uncompressed flat payload from raw record bytes.
func rawFlat(records ...[]byte) []byte {
	out := appendUvarint(nil, uint64(len(records)))
	for _, r := range records {
		out = append(out, r...)
	}
	return out
}

// recFlat builds a flat record with a uint64 size.
func recFlat(path string, size uint64) []byte {
	out := appendUvarint(nil, uint64(len(path)))
	out = append(out, path...)
	return appendUvarint(out, size)
}

// recFlatBytes builds a flat record with an explicit pre-encoded size varint.
func recFlatBytes(path string, sizeVarint []byte) []byte {
	out := appendUvarint(nil, uint64(len(path)))
	out = append(out, path...)
	return append(out, sizeVarint...)
}

func TestFlatDecodeRejections(t *testing.T) {
	t.Parallel()

	overflow := appendUvarint(nil, math.MaxInt64+1)

	tests := []struct {
		name    string
		payload []byte
		wantErr error
	}{
		{
			name:    "count zero",
			payload: []byte{0x00},
			wantErr: ErrInvalidCount,
		},
		{
			name:    "decreasing paths",
			payload: rawFlat(recFlat("b", 1), recFlat("a", 1)),
			wantErr: ErrNonCanonicalOrder,
		},
		{
			name:    "equal path decreasing size",
			payload: rawFlat(recFlat("a", 2), recFlat("a", 1)),
			wantErr: ErrNonCanonicalOrder,
		},
		{
			name:    "trailing byte",
			payload: append(rawFlat(recFlat("a", 1)), 0xff),
			wantErr: ErrTrailingBytes,
		},
		{
			name:    "path longer than payload",
			payload: append(appendUvarint(nil, 1), appendUvarint(nil, 5)...),
			wantErr: ErrTruncated,
		},
		{
			name:    "missing size",
			payload: []byte{0x01, 0x01, 'a'},
			wantErr: ErrTruncated,
		},
		{
			name:    "size overflows int64",
			payload: rawFlat(recFlatBytes("a", overflow)),
			wantErr: ErrOverflow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeFlat(tt.payload, DefaultLimits())
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("decodeFlat() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestFlatLimitErrors(t *testing.T) {
	t.Parallel()

	t.Run("file count", func(t *testing.T) {
		t.Parallel()
		limits := DefaultLimits()
		limits.MaxFiles = 2
		payload := rawFlat(recFlat("a", 1), recFlat("b", 1), recFlat("c", 1))
		_, err := decodeFlat(payload, limits)
		var lerr *LimitError
		if !errors.As(err, &lerr) || lerr.Limit != LimitFiles {
			t.Errorf("decodeFlat() error = %v, want LimitError(%s)", err, LimitFiles)
		}
	})

	t.Run("path bytes", func(t *testing.T) {
		t.Parallel()
		limits := DefaultLimits()
		limits.MaxPathBytes = 4
		payload := rawFlat(recFlat("toolong", 1))
		_, err := decodeFlat(payload, limits)
		var lerr *LimitError
		if !errors.As(err, &lerr) || lerr.Limit != LimitPathBytes {
			t.Errorf("decodeFlat() error = %v, want LimitError(%s)", err, LimitPathBytes)
		}
	})

	t.Run("aggregate path bytes aborts mid-decode", func(t *testing.T) {
		t.Parallel()
		limits := DefaultLimits()
		limits.MaxAggregatePathBytes = 10
		// The first file fits; the second crosses the aggregate budget.
		payload := rawFlat(recFlat("abcdef", 1), recFlat("ghijkl", 1))
		_, err := decodeFlat(payload, limits)
		var lerr *LimitError
		if !errors.As(err, &lerr) || lerr.Limit != LimitAggregatePathBytes {
			t.Errorf("decodeFlat() error = %v, want LimitError(%s)", err, LimitAggregatePathBytes)
		}
	})

	t.Run("file count over limit at encode", func(t *testing.T) {
		t.Parallel()
		limits := DefaultLimits()
		limits.MaxFiles = 1
		_, _, err := encodeWith(Flat, []File{{Path: "a", Size: 1}, {Path: "b", Size: 1}}, limits)
		var lerr *LimitError
		if !errors.As(err, &lerr) || lerr.Limit != LimitFiles {
			t.Errorf("encodeWith() error = %v, want LimitError(%s)", err, LimitFiles)
		}
	})
}

func TestUnboundedCountDoesNotAllocate(t *testing.T) {
	t.Parallel()

	// A hostile payload advertises a huge count but carries almost no data;
	// the count limit must reject it before a large slice is allocated.
	huge := appendUvarint(nil, 1<<40)
	payload := append(append([]byte{}, huge...), recFlat("a", 1)...)
	_, err := decodeFlat(payload, DefaultLimits())
	var lerr *LimitError
	if !errors.As(err, &lerr) || lerr.Limit != LimitFiles {
		t.Errorf("decodeFlat() error = %v, want LimitError(%s)", err, LimitFiles)
	}
}

func TestEncodeRejectsNegativeSize(t *testing.T) {
	t.Parallel()

	_, _, err := encodeWith(Flat, []File{{Path: "a", Size: -1}}, DefaultLimits())
	if !errors.Is(err, ErrNegativeSize) {
		t.Errorf("encodeWith() error = %v, want ErrNegativeSize", err)
	}
}

func TestEncodeDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	input := []File{{Path: "b.txt", Size: 2}, {Path: "a.txt", Size: 1}, {Path: "a.txt", Size: 0}}
	saved := make([]File, len(input))
	copy(saved, input)

	if _, _, err := encodeWith(PrefixDelta, input, DefaultLimits()); err != nil {
		t.Fatalf("encodeWith() error = %v", err)
	}
	if !reflect.DeepEqual(input, saved) {
		t.Errorf("encodeWith() mutated its input:\n got %#v\nwant %#v", input, saved)
	}
}
