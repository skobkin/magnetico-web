package codec

import (
	"errors"
	"math"
	"testing"
)

func TestReadUvarint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		b          []byte
		want       uint64
		wantN      int
		wantReason string // empty: no error expected
	}{
		{name: "zero", b: []byte{0x00}, want: 0, wantN: 1},
		{name: "single byte max", b: []byte{0x7f}, want: 127, wantN: 1},
		{name: "two bytes", b: []byte{0xe8, 0x07}, want: 1000, wantN: 2},
		{
			name:  "max uint64",
			b:     []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01},
			want:  math.MaxUint64,
			wantN: 10,
		},
		{name: "truncated", b: []byte{0xe8}, wantReason: VarintTruncated},
		{name: "empty", b: nil, wantReason: VarintTruncated},
		{
			name:       "overflow",
			b:          []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01},
			wantReason: VarintOverflow,
		},
		{name: "nonminimal zero", b: []byte{0x80, 0x00}, wantReason: VarintNonminimal},
		{name: "nonminimal 1000", b: []byte{0xe8, 0x87, 0x00}, wantReason: VarintNonminimal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, n, err := readUvarint(tt.b)
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("readUvarint() error = %v", err)
				}
				if got != tt.want || n != tt.wantN {
					t.Errorf("readUvarint() = (%d, %d), want (%d, %d)", got, n, tt.want, tt.wantN)
				}
				return
			}
			var verr *VarintError
			if !errors.As(err, &verr) {
				t.Fatalf("readUvarint() error = %v, want *VarintError", err)
			}
			if verr.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", verr.Reason, tt.wantReason)
			}
		})
	}
}

func TestAppendUvarintMatchesGoldenBytes(t *testing.T) {
	t.Parallel()

	// Sizes from golden vectors B, C.
	tests := []struct {
		v    uint64
		want []byte
	}{
		{1234, []byte{0xd2, 0x09}},
		{1000, []byte{0xe8, 0x07}},
		{1100, []byte{0xcc, 0x08}},
		{128, []byte{0x80, 0x01}},
		{256, []byte{0x80, 0x02}},
	}
	for _, tt := range tests {
		if got := appendUvarint(nil, tt.v); string(got) != string(tt.want) {
			t.Errorf("appendUvarint(%d) = %x, want %x", tt.v, got, tt.want)
		}
	}
}
