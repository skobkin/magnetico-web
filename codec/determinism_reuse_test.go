package codec

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
)

// reuseBases returns raw serializer payloads (prefix-delta and trie, three
// content shapes each) that are long enough to be truncated to any size the
// reuse tests need. Truncated payloads are not valid serializations; they
// are only compressor input, which is all these tests exercise.
func reuseBases(t testing.TB) map[string][]byte {
	t.Helper()
	const minLen = brotliLargeHint + 64
	bases := map[string][]byte{}
	for style, n := range map[int]int{0: 90_000, 1: 90_000, 2: 140_000} {
		files := detFixture(style, n)
		for _, ser := range []SerializerID{PrefixDelta, DirectoryTrie} {
			if ser == DirectoryTrie && !trieEligible(files) {
				continue
			}
			raw, err := encodeRaw(ser, files)
			if err != nil {
				t.Fatalf("encodeRaw() error = %v", err)
			}
			if len(raw) < minLen {
				t.Fatalf("style %d serializer %d payload = %d bytes, want >= %d", style, ser, len(raw), minLen)
			}
			bases[fmt.Sprintf("style%d/ser%d", style, ser)] = raw
		}
	}
	return bases
}

// TestBrotliReuseRandomHistories pushes one shared compressState through
// seeded random orderings of payloads of many sizes, shapes and serializers
// and requires every output to equal a fresh state's output for the same
// payload. The sizes straddle the reuse gate (brotliReuseMaxPayload) and the
// 1 MiB hasher-selection threshold, so every permitted transition is hit:
// small->small, small->large, large->small, prefix->trie, trie->prefix,
// across single-block boundaries. The orderings are fixed by the seed, so a
// failure reproduces exactly.
func TestBrotliReuseRandomHistories(t *testing.T) {
	t.Parallel()

	// The Brotli input block size at quality >= 9. Deliberately a literal,
	// not brotliReuseMaxPayload: the tested sizes must not move when the
	// gate under test is changed.
	const gate = 1 << 18
	type tc struct {
		profile Profile
		sizes   []int
		shapes  int // number of payload shapes (serializer x content) used
		chains  int
		short   bool // keep in -short runs
	}
	// The default sweep is sized for CI, including under the race detector.
	// CODEC_REUSE_SWEEP=full runs the wide sweep used to validate the gate
	// (and should be re-run after changing it or upgrading brotli).
	cases := []tc{
		{ProfileCompact, []int{1, 100, 65535, gate - 1, gate, gate + 128, brotliLargeHint}, 2, 3, false},
		// q11 costs seconds per MiB and always uses the same hasher.
		{ProfileMaximum, []int{1, 100, 4096, gate / 2}, 2, 2, false},
	}
	if os.Getenv("CODEC_REUSE_SWEEP") == "full" {
		cases = []tc{
			{ProfileCompact, []int{1, 7, 100, 4096, 65535, gate - 129, gate - 1, gate, gate + 128,
				3*gate + 77, brotliLargeHint - 1, brotliLargeHint}, 4, 4, false},
			{ProfileMaximum, []int{1, 100, 65535, gate - 1, gate}, 2, 3, false},
		}
	}

	bases := reuseBases(t)
	names := make([]string, 0, len(bases))
	for n := range bases {
		names = append(names, n)
	}
	// Map iteration order is random; sort for reproducible chain contents.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}

	for _, c := range cases {
		t.Run(c.profile.name(), func(t *testing.T) {
			t.Parallel()
			if testing.Short() && !c.short {
				t.Skip("reuse sweep skipped in -short mode")
			}

			type pl struct {
				name string
				data []byte
			}
			var pays []pl
			// Shapes are picked spread across the sorted list so both
			// serializers and several content styles are included.
			for k := 0; k < c.shapes; k++ {
				name := names[k*len(names)/c.shapes]
				for _, sz := range c.sizes {
					pays = append(pays, pl{fmt.Sprintf("%s/%d", name, sz), bases[name][:sz]})
				}
			}
			refs := make([][]byte, len(pays))
			for i, p := range pays {
				refs[i] = compressCopy(t, newCompressState(c.profile), Brotli, p.data)
			}

			rng := rand.New(rand.NewPCG(0x5eed, uint64(c.profile)))
			for chain := 0; chain < c.chains; chain++ {
				order := rng.Perm(len(pays))
				shared := newCompressState(c.profile)
				prev := ""
				for step, i := range order {
					got := compressCopy(t, shared, Brotli, pays[i].data)
					if !bytes.Equal(got, refs[i]) {
						t.Fatalf("chain %d step %d: %s after %s: %d bytes, fresh state gives %d bytes",
							chain, step, pays[i].name, prev, len(got), len(refs[i]))
					}
					prev = pays[i].name
				}
			}
		})
	}
}
