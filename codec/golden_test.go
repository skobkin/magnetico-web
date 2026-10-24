package codec

import (
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// mustHex decodes a spec golden vector; spaces and line breaks in the spec
// are for readability only.
func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex fixture: %v", err)
	}
	return b
}

// goldenVectors are vectors B–G from docs/specs/wire-format-v1.md §6. Vector
// A (the no-blob single-file case) has no serialized bytes and is tested at
// the persistence boundary.
var goldenVectors = []struct {
	name string
	ser  SerializerID
	hex  string
	want []File
}{
	{
		name: "B atypical single file",
		ser:  Flat,
		hex:  "01 10 01 0D 44 69 66 66 65 72 65 6E 74 2E 6D 6B 76 D2 09",
		want: []File{{Path: "Different.mkv", Size: 1234}},
	},
	{
		name: "C flat two episodes",
		ser:  Flat,
		hex:  "01 10 02 09 53 2F 45 30 31 2E 6D 6B 76 E8 07 09 53 2F 45 30 32 2E 6D 6B 76 CC 08",
		want: []File{{Path: "S/E01.mkv", Size: 1000}, {Path: "S/E02.mkv", Size: 1100}},
	},
	{
		name: "C prefix-delta two episodes",
		ser:  PrefixDelta,
		hex:  "01 20 02 00 09 53 2F 45 30 31 2E 6D 6B 76 E8 07 04 05 32 2E 6D 6B 76 CC 08",
		want: []File{{Path: "S/E01.mkv", Size: 1000}, {Path: "S/E02.mkv", Size: 1100}},
	},
	{
		name: "D duplicate paths and zero sizes",
		ser:  PrefixDelta,
		hex:  "01 20 03 00 07 44 2F 61 2E 74 78 74 00 07 00 00 07 00 07",
		want: []File{{Path: "D/a.txt", Size: 0}, {Path: "D/a.txt", Size: 0}, {Path: "D/a.txt", Size: 7}},
	},
	{
		name: "E UTF-8 shared prefix split mid-code-point",
		ser:  PrefixDelta,
		hex:  "01 20 02 00 14 D0 9C D1 83 D0 B7 D1 8B D0 BA D0 B0 2F 30 31 2E 66 6C 61 63 80 01 0E 06 32 2E 66 6C 61 63 80 02",
		want: []File{{Path: "Музыка/01.flac", Size: 128}, {Path: "Музыка/02.flac", Size: 256}},
	},
	{
		name: "F structurally irregular paths",
		ser:  PrefixDelta,
		hex:  "01 20 02 00 06 61 2F 2E 2E 2F 63 02 02 02 2F 62 01",
		want: []File{{Path: "a/../c", Size: 2}, {Path: "a//b", Size: 1}},
	},
	{
		name: "C directory-trie two episodes",
		ser:  DirectoryTrie,
		hex:  "01 40 02 01 01 53 00 02 07 45 30 31 2E 6D 6B 76 E8 07 07 45 30 32 2E 6D 6B 76 CC 08 00",
		want: []File{{Path: "S/E01.mkv", Size: 1000}, {Path: "S/E02.mkv", Size: 1100}},
	},
	{
		// Tree depth-first order (z/ subtree before the root file) differs
		// from the canonical output order; decoding must sort.
		name: "G trie traversal differs from global sorting",
		ser:  DirectoryTrie,
		hex:  "01 40 02 01 01 7A 00 01 05 66 2E 62 69 6E 02 01 05 61 2E 74 78 74 01",
		want: []File{{Path: "a.txt", Size: 1}, {Path: "z/f.bin", Size: 2}},
	},
}

func TestGoldenVectorsDecode(t *testing.T) {
	t.Parallel()
	for _, tt := range goldenVectors {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Decode(mustHex(t, tt.hex), DefaultLimits())
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Decode() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestGoldenVectorsEncode asserts the exact hex output of the encoder: the
// spec vectors are the authority, not our own decoder.
func TestGoldenVectorsEncode(t *testing.T) {
	t.Parallel()
	for _, tt := range goldenVectors {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blob, sel, err := encodeWith(tt.ser, tt.want, DefaultLimits())
			if err != nil {
				t.Fatalf("encodeWith() error = %v", err)
			}
			if sel != (Selection{Serializer: tt.ser, Compressor: None}) {
				t.Errorf("selection = %+v, want serializer 0x%x compressor none", sel, tt.ser)
			}
			want := strings.ToLower(strings.ReplaceAll(tt.hex, " ", ""))
			if got := hex.EncodeToString(blob); got != want {
				t.Errorf("encodeWith() = %s, want %s", got, want)
			}
		})
	}
}
