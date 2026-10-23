package codec

import (
	"bytes"
	"errors"
	"testing"
)

func TestParseEnvelopeRejectsUnknownIDsAndMalformedHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		blob    []byte
		wantErr error
	}{
		{name: "empty blob", blob: nil, wantErr: ErrTruncated},
		{name: "one byte", blob: []byte{0x01}, wantErr: ErrTruncated},
		{
			name:    "wrong version zero",
			blob:    []byte{0x00, 0x10, 0x01, 0x01, 0x61, 0x01},
			wantErr: ErrUnsupportedVersion,
		},
		{
			name:    "wrong version two",
			blob:    []byte{0x02, 0x10, 0x01, 0x01, 0x61, 0x01},
			wantErr: ErrUnsupportedVersion,
		},
		{
			name:    "reserved serializer 0x3",
			blob:    []byte{0x01, 0x30},
			wantErr: ErrUnsupportedSerializer,
		},
		{
			name:    "reserved serializer 0x8",
			blob:    []byte{0x01, 0x80},
			wantErr: ErrUnsupportedSerializer,
		},
		{
			name:    "reserved compressor 0x3",
			blob:    []byte{0x01, 0x13},
			wantErr: ErrUnsupportedCompressor,
		},
		{
			name:    "reserved compressor 0xf",
			blob:    []byte{0x01, 0x1f},
			wantErr: ErrUnsupportedCompressor,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, _, err := parseEnvelope(tt.blob, DefaultLimits())
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("parseEnvelope() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseEnvelopeStoredBytesLimit(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxStoredBytes = 8
	blob := bytes.Repeat([]byte{0x01, 0x10}, 10) // 20 bytes
	_, _, _, _, err := parseEnvelope(blob, limits)

	var lerr *LimitError
	if !errors.As(err, &lerr) || lerr.Limit != LimitStoredBytes {
		t.Errorf("parseEnvelope() error = %v, want LimitError(%s)", err, LimitStoredBytes)
	}
}

func TestParseEnvelopeUncompressedLengthLimit(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxUncompressedBytes = 4
	// Uncompressed payload longer than the limit.
	_, _, _, _, err := parseEnvelope([]byte{0x01, 0x10, 1, 2, 3, 4, 5}, limits)
	var lerr *LimitError
	if !errors.As(err, &lerr) || lerr.Limit != LimitUncompressedBytes {
		t.Errorf("error = %v, want LimitError(%s)", err, LimitUncompressedBytes)
	}

	// Compressed: declared length over the limit must fail before any
	// payload-based allocation.
	declared := appendUvarint(nil, uint64(limits.MaxUncompressedBytes)+1)
	_, _, _, _, err = parseEnvelope(append([]byte{0x01, 0x11}, declared...), limits)
	if !errors.As(err, &lerr) || lerr.Limit != LimitUncompressedBytes {
		t.Errorf("declared: error = %v, want LimitError(%s)", err, LimitUncompressedBytes)
	}
}

func TestParseEnvelopeCompressedDeclaredVarint(t *testing.T) {
	t.Parallel()

	// A nonminimal declared length is rejected even before compression is
	// attempted.
	_, _, _, _, err := parseEnvelope([]byte{0x01, 0x11, 0x80, 0x00}, DefaultLimits())
	var verr *VarintError
	if !errors.As(err, &verr) || verr.Reason != VarintNonminimal {
		t.Errorf("error = %v, want nonminimal VarintError", err)
	}

	// A valid compressed envelope parses through to the payload.
	payload := []byte{0xde, 0xad}
	// Header 0x12: serializer 0x1 (flat), compressor 0x2 (brotli).
	ser, comp, got, declared, err := parseEnvelope([]byte{0x01, 0x12, 0x02, payload[0], payload[1]}, DefaultLimits())
	if err != nil {
		t.Fatalf("parseEnvelope() error = %v", err)
	}
	if ser != Flat || comp != Brotli || declared != 2 || !bytes.Equal(got, payload) {
		t.Errorf("parseEnvelope() = (0x%x, 0x%x, %x, %d), want (0x1, 0x2, dead, 2)", ser, comp, got, declared)
	}
}

func TestBuildEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()

	payload := []byte{0x01, 0x02, 0x03}
	blob, err := buildEnvelope(Flat, None, payload)
	if err != nil {
		t.Fatalf("buildEnvelope() error = %v", err)
	}
	if !bytes.Equal(blob, append([]byte{0x01, 0x10}, payload...)) {
		t.Errorf("uncompressed envelope = %x, want 0110 + payload", blob)
	}
	ser, comp, got, declared, err := parseEnvelope(blob, DefaultLimits())
	if err != nil || ser != Flat || comp != None || declared != 3 || !bytes.Equal(got, payload) {
		t.Errorf("parseEnvelope(buildEnvelope) = (0x%x, 0x%x, %x, %d, %v)", ser, comp, got, declared, err)
	}

	blob, err = buildEnvelope(PrefixDelta, Brotli, payload)
	if err != nil {
		t.Fatalf("buildEnvelope() error = %v", err)
	}
	if !bytes.Equal(blob, []byte{0x01, 0x22, 0x03, 0x01, 0x02, 0x03}) {
		t.Errorf("compressed envelope = %x, want 0122 03010203", blob)
	}

	if _, err := buildEnvelope(SerializerID(0x9), None, payload); !errors.Is(err, ErrUnsupportedSerializer) {
		t.Errorf("reserved serializer: error = %v, want ErrUnsupportedSerializer", err)
	}
	if _, err := buildEnvelope(Flat, CompressorID(0x5), payload); !errors.Is(err, ErrUnsupportedCompressor) {
		t.Errorf("reserved compressor: error = %v, want ErrUnsupportedCompressor", err)
	}

	// The declared length varint must be minimal.
	blob, err = buildEnvelope(Flat, Zstd, make([]byte, 300))
	if err != nil {
		t.Fatalf("buildEnvelope() error = %v", err)
	}
	if want := appendUvarint(nil, 300); !bytes.Equal(blob[2:2+len(want)], want) {
		t.Errorf("declared length = %x, want minimal %x", blob[2:4], want)
	}
}
