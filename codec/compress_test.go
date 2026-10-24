package codec

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"

	"github.com/andybalholm/brotli"
)

func TestCompressedRoundTripAllProfiles(t *testing.T) {
	t.Parallel()

	// Large enough and repetitive enough that compression beats the
	// uncompressed fallback on total framed bytes. Decode returns canonical
	// order, so compare against the canonical form.
	files := make([]File, 0, 200)
	for i := 0; i < 200; i++ {
		files = append(files, File{
			Path: fmt.Sprintf("data/season-%02d/episode-%03d.mkv", i%4, i),
			Size: int64(1_000_000 + i),
		})
	}
	want, err := canonicalize(files, DefaultLimits())
	if err != nil {
		t.Fatalf("canonicalize() error = %v", err)
	}

	for _, p := range []Profile{ProfileBalanced, ProfileCompact, ProfileMaximum} {
		t.Run(p.name(), func(t *testing.T) {
			t.Parallel()

			blob, sel, err := Encode(files, p, DefaultLimits())
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			if sel.Compressor == None {
				t.Fatalf("selection = %+v, want compression for a repetitive list", sel)
			}

			// Decoding is profile-independent: the header carries the IDs.
			got, err := Decode(blob, DefaultLimits())
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Decode() != canonical input")
			}

			// Encoding must be deterministic within a profile.
			blob2, sel2, err := Encode(files, p, DefaultLimits())
			if err != nil || sel2 != sel || !bytes.Equal(blob, blob2) {
				t.Errorf("Encode() not deterministic: (%x, %+v) vs (%x, %+v), err %v", blob, sel, blob2, sel2, err)
			}
		})
	}
}

// TestUncompressedFallbackForSmallLists pins the fallback selection: on
// small lists the compressed frame's fixed overhead exceeds any gain, so the
// uncompressed candidate must win. (Large lists compress even when the file
// data is random: the varint record headers themselves are predictable
// redundancy — verified empirically, see the implementation report.)
func TestUncompressedFallbackForSmallLists(t *testing.T) {
	t.Parallel()

	files := []File{
		{Path: "q7F.bin", Size: 814_248_312_315},
		{Path: "zP2.dat", Size: 91_552_113_004},
		{Path: "mK9.iso", Size: 5_331_712_886},
		{Path: "bX4.rar", Size: 44_092_771_230},
	}
	want, err := canonicalize(files, DefaultLimits())
	if err != nil {
		t.Fatalf("canonicalize() error = %v", err)
	}

	for _, p := range []Profile{ProfileBalanced, ProfileCompact, ProfileMaximum} {
		blob, sel, err := Encode(files, p, DefaultLimits())
		if err != nil {
			t.Fatalf("Encode(%v) error = %v", p, err)
		}
		if sel.Compressor != None {
			t.Errorf("profile %v selected %+v, want the uncompressed fallback", p, sel)
		}
		if got, err := Decode(blob, DefaultLimits()); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("profile %v: round-trip = %#v, %v", p, got, err)
		}
	}
}

func TestSelectionPrefersSmallerTotalFramedBlob(t *testing.T) {
	t.Parallel()

	// A highly repetitive list: compressed must beat the uncompressed
	// fallback by total framed bytes.
	files := []File{
		{Path: "data/0001", Size: 1},
		{Path: "data/0002", Size: 1},
		{Path: "data/0003", Size: 1},
		{Path: "data/0004", Size: 1},
		{Path: "data/0005", Size: 1},
		{Path: "data/0006", Size: 1},
		{Path: "data/0007", Size: 1},
		{Path: "data/0008", Size: 1},
	}

	raw, _, err := encodeWith(PrefixDelta, files, DefaultLimits())
	if err != nil {
		t.Fatalf("encodeWith() error = %v", err)
	}
	blob, sel, err := Encode(files, ProfileCompact, DefaultLimits())
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if sel.Compressor == None {
		t.Fatalf("selection = %+v, want compressed", sel)
	}
	if len(blob) >= len(raw) {
		t.Errorf("compressed blob %d bytes >= uncompressed %d bytes", len(blob), len(raw))
	}
}

// TestBrotliMidPayloadCorruption pins the honest behavior for content-level
// corruption of Brotli streams: Brotli has no integrated checksum, so a
// mid-payload bit flip may decode structurally. The codec's defense is the
// serializer layer (canonical order, counts, prefix maximality) and the
// persistence layer's count/total verification — never silent acceptance of
// the original list.
func TestBrotliMidPayloadCorruption(t *testing.T) {
	t.Parallel()

	files := []File{
		{Path: "a/b/c/one.txt", Size: 1000},
		{Path: "a/b/c/two.txt", Size: 2000},
		{Path: "a/b/d/three.txt", Size: 3000},
	}

	payload, _, err := encodeWith(PrefixDelta, files, DefaultLimits())
	if err != nil {
		t.Fatalf("encodeWith() error = %v", err)
	}

	var buf bytes.Buffer
	w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: 9})
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}

	// Corrupt several distinct mid-frame offsets; for each, the outcome must
	// be either a typed rejection or a different (wrong) list — never the
	// original data.
	for _, offset := range []int{len(buf.Bytes()) / 4, len(buf.Bytes()) / 2, 3 * len(buf.Bytes()) / 4} {
		corrupt := append([]byte{0x01, 0x21}, appendUvarint(nil, uint64(len(payload)))...)
		corrupt = append(corrupt, buf.Bytes()...)
		corrupt[2+2+offset] ^= 0x55

		got, err := Decode(corrupt, DefaultLimits())
		if err != nil {
			continue // typed rejection: acceptable
		}
		if reflect.DeepEqual(got, files) {
			t.Errorf("offset %d: corrupted stream decoded to the original list", offset)
		}
	}
}

func TestCompressStateReuse(t *testing.T) {
	t.Parallel()

	st := newCompressState(ProfileCompact)

	// Two distinct canonical lists through one reused compressor state: both
	// must round-trip exactly, proving the state produces complete frames
	// after Reset and that results are copied out before the next call.
	lists := [][]File{
		{
			{Path: "a/one.bin", Size: 1},
			{Path: "a/two.bin", Size: 2},
			{Path: "a/three.bin", Size: 3},
		},
		{
			{Path: "x/y/z/other.bin", Size: 1000},
			{Path: "x/y/z/second.bin", Size: 2000},
			{Path: "x/y/z/third.bin", Size: 3000},
		},
	}

	for i, files := range lists {
		sorted, err := canonicalize(files, DefaultLimits())
		if err != nil {
			t.Fatalf("canonicalize() error = %v", err)
		}
		raw, err := encodeRaw(PrefixDelta, sorted)
		if err != nil {
			t.Fatalf("encodeRaw() error = %v", err)
		}
		frame, err := st.compress(Brotli, raw)
		if err != nil {
			t.Fatalf("compress() error = %v", err)
		}
		frameCopy := append([]byte(nil), frame...)
		blob, err := buildEnvelope(PrefixDelta, Brotli, uint64(len(raw)), frameCopy)
		if err != nil {
			t.Fatalf("buildEnvelope() error = %v", err)
		}
		got, err := Decode(blob, DefaultLimits())
		if err != nil {
			t.Fatalf("list %d: Decode() error = %v", i, err)
		}
		if !reflect.DeepEqual(got, sorted) {
			t.Errorf("list %d: Decode() = %#v, want %#v", i, got, sorted)
		}
	}
}
