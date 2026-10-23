package codec

import (
	"bytes"
	"testing"
)

// FuzzDecode feeds arbitrary bytes to the strict decoder. Every successful
// decode of an uncompressed flat/prefix blob must re-encode to exactly the
// same bytes, because decoder output is canonical and both encoders are
// deterministic.
func FuzzDecode(f *testing.F) {
	for _, tt := range goldenVectors {
		f.Add(mustHex(f, tt.hex))
	}
	f.Fuzz(func(t *testing.T, blob []byte) {
		files, err := Decode(blob, DefaultLimits())
		if err != nil {
			return
		}
		if len(blob) < 2 {
			return
		}
		ser := SerializerID(blob[1] >> 4)
		comp := CompressorID(blob[1] & 0x0f)
		if comp != None || (ser != Flat && ser != PrefixDelta) {
			return
		}
		reblob, _, err := encodeWith(ser, files, DefaultLimits())
		if err != nil {
			t.Fatalf("re-encode of decoded list failed: %v", err)
		}
		if !bytes.Equal(blob, reblob) {
			t.Fatalf("round-trip mismatch:\n got %x\nwant %x", reblob, blob)
		}
	})
}
