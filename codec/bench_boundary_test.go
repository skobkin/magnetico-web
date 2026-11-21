package codec

import (
	"fmt"
	"sort"
	"testing"
)

// boundaryGate is the Brotli input block size (2^18) that brotliReuseMaxPayload
// is set to. It is a literal so these benchmarks compile and measure the same
// sizes on a tree that does not define the constant (the unfixed baseline).
const boundaryGate = 1 << 18

// boundaryCase is a deterministic file list whose prefix-delta serialization
// has a known size relative to the reuse gate.
type boundaryCase struct {
	label   string
	files   []File
	payload int // actual encodeRaw(PrefixDelta) size in bytes
}

// prefixSize returns the real prefix-delta serialization size of the first n
// files of the style-0 fixture. It is strictly increasing in n.
func prefixSize(b *testing.B, all []File, n int) int {
	b.Helper()
	raw, err := encodeRaw(PrefixDelta, all[:n])
	if err != nil {
		b.Fatalf("encodeRaw() error = %v", err)
	}
	return len(raw)
}

// boundaryCases returns real serialized payloads straddling the gate:
// "below" is the largest list whose payload is < 2^18, "at" the smallest whose
// payload is >= 2^18 (one file apart, tens of bytes of difference), plus a
// half-block, a block+25%, two blocks and 1 MiB reference.
func boundaryCases(b *testing.B) []boundaryCase {
	b.Helper()
	all := detFixture(0, 60_000)
	first := func(target int) int { // smallest n with payload >= target
		n := sort.Search(len(all), func(n int) bool {
			return n > 0 && prefixSize(b, all, n) >= target
		})
		if n == len(all) {
			b.Fatalf("fixture too small for payload %d", target)
		}
		return n
	}
	at := first(boundaryGate)
	counts := []struct {
		label string
		n     int
	}{
		{"half", first(boundaryGate / 2)},
		{"below", at - 1},
		{"at", at},
		{"above", first(boundaryGate + boundaryGate/4)},
		{"double", first(2 * boundaryGate)},
		{"1MiB", first(1 << 20)},
	}
	cases := make([]boundaryCase, 0, len(counts))
	for _, c := range counts {
		files := all[:c.n]
		cases = append(cases, boundaryCase{c.label, files, prefixSize(b, all, c.n)})
	}
	if cases[1].payload >= boundaryGate || cases[2].payload < boundaryGate {
		b.Fatalf("below/at payloads = %d/%d do not straddle %d", cases[1].payload, cases[2].payload, boundaryGate)
	}
	return cases
}

// BenchmarkBrotliReuseBoundary measures the Brotli compression of one
// serialized payload on a warmed, reused compressState, for payload sizes
// around the reuse gate (brotliReuseMaxPayload = 2^18). On the fixed tree
// payloads below the gate reuse the pooled writer and payloads at or above it
// allocate a fresh writer per call; on the unfixed baseline every size reuses.
// The jump between "below" and "at" is therefore the cost of the gate itself.
func BenchmarkBrotliReuseBoundary(b *testing.B) {
	cases := boundaryCases(b)
	for _, p := range []Profile{ProfileCompact, ProfileMaximum} {
		for _, c := range cases {
			raw, err := encodeRaw(PrefixDelta, c.files)
			if err != nil {
				b.Fatal(err)
			}
			b.Run(fmt.Sprintf("%s/%s", p.name(), c.label), func(b *testing.B) {
				st := newCompressState(p)
				if _, err := st.compress(Brotli, raw); err != nil { // warm
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.SetBytes(int64(len(raw)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := st.compress(Brotli, raw); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(len(raw)), "payload-bytes")
			})
		}
	}
}

// BenchmarkEncoderBoundary is the same sweep through the public Encoder
// (one worker, all candidates of the profile, selection included). The 1 MiB
// case is omitted: at q11 it costs seconds per candidate.
func BenchmarkEncoderBoundary(b *testing.B) {
	cases := boundaryCases(b)
	for _, p := range []Profile{ProfileCompact, ProfileMaximum} {
		for _, c := range cases {
			if c.label == "1MiB" {
				continue
			}
			b.Run(fmt.Sprintf("%s/%s", p.name(), c.label), func(b *testing.B) {
				enc := NewEncoder(p, DefaultLimits(), 1)
				defer enc.Close()
				if _, _, err := enc.Encode(c.files); err != nil { // warm
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := enc.Encode(c.files); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(c.payload), "payload-bytes")
			})
		}
	}
}
