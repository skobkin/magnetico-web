package codec

import (
	"errors"
	"testing"
)

// PeekDecodedSize must agree with the envelope parse Decode uses, for every
// serializer/compressor combination.
func TestPeekDecodedSizeMatchesEnvelope(t *testing.T) {
	files := sampleFiles(2000)
	for _, p := range []Profile{ProfileBalanced, ProfileCompact, ProfileMaximum} {
		blob, sel, err := Encode(files, p, DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		ser, comp, _, declared, err := parseEnvelope(blob, DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		h := blob[:min(len(blob), PeekHeaderLen)]
		got, d, err := PeekDecodedSize(h, len(blob), DefaultLimits())
		if err != nil {
			t.Fatalf("%v: %v", sel, err)
		}
		if got.Serializer != ser || got.Compressor != comp || d != declared {
			t.Fatalf("peek (%v,%d) != envelope (%x,%x,%d)", got, d, ser, comp, declared)
		}
	}
}

func TestPeekDecodedSizeUncompressedUsesStoredLength(t *testing.T) {
	blob := []byte{0x01, 0x10, 0x01, 0x0D, 'D', 'i', 'f', 'f', 'e', 'r', 'e', 'n', 't', '.', 'm', 'k', 'v', 0xD2, 0x09} // vector B
	sel, d, err := PeekDecodedSize(blob[:PeekHeaderLen], len(blob), DefaultLimits())
	if err != nil || sel.Compressor != None || d != uint64(len(blob)-2) {
		t.Fatalf("got %v %d %v", sel, d, err)
	}
}

func TestPeekDecodedSizeRejections(t *testing.T) {
	l := DefaultLimits()
	cases := []struct {
		name   string
		header []byte
		stored int
		want   error
	}{
		{"short", []byte{0x01}, 1, ErrTruncated},
		{"version", []byte{0x02, 0x22, 0x05}, 3, ErrUnsupportedVersion},
		{"serializer", []byte{0x01, 0x32, 0x05}, 3, ErrUnsupportedSerializer},
		{"compressor", []byte{0x01, 0x23, 0x05}, 3, ErrUnsupportedCompressor},
		{"truncated varint", []byte{0x01, 0x22, 0x80}, 3, ErrTruncated},
		{"nonminimal varint", []byte{0x01, 0x22, 0x80, 0x00}, 4, nil},
	}
	for _, c := range cases {
		_, _, err := PeekDecodedSize(c.header, c.stored, l)
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	// declared length over the limit
	big := appendUvarint([]byte{0x01, 0x22}, uint64(l.MaxUncompressedBytes)+1)
	var le *LimitError
	if _, _, err := PeekDecodedSize(big, len(big)+10, l); !errors.As(err, &le) || le.Limit != LimitUncompressedBytes {
		t.Errorf("declared limit: %v", err)
	}
	// stored length over the limit
	if _, _, err := PeekDecodedSize([]byte{0x01, 0x22, 0x05}, l.MaxStoredBytes+1, l); !errors.As(err, &le) || le.Limit != LimitStoredBytes {
		t.Errorf("stored limit: %v", err)
	}
}
