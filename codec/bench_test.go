package codec

import (
	"fmt"
	"testing"
)

// BenchmarkEncode measures encode latency vs file-list size per profile.
// The q9/q11 numbers feed the crawler backpressure assessment (synchronous
// encode in the event loop).
func BenchmarkEncode(b *testing.B) {
	for _, n := range []int{10, 1_000, 50_000} {
		files := sampleFiles(n)
		for _, p := range []Profile{ProfileBalanced, ProfileCompact, ProfileMaximum} {
			b.Run(fmt.Sprintf("%s/files-%d", p.name(), n), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(n))
				for i := 0; i < b.N; i++ {
					if _, _, err := Encode(files, p, DefaultLimits()); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkEncodeUncompressed(b *testing.B) {
	for _, n := range []int{10, 1_000, 50_000} {
		files := sampleFiles(n)
		b.Run(fmt.Sprintf("files-%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := encodeWith(PrefixDelta, files, DefaultLimits()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	for _, n := range []int{10, 1_000, 50_000} {
		files := sampleFiles(n)
		for _, p := range []Profile{ProfileCompact, ProfileMaximum} {
			blob, _, err := Encode(files, p, DefaultLimits())
			if err != nil {
				b.Fatal(err)
			}
			b.Run(fmt.Sprintf("%s/files-%d", p.name(), n), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(n))
				for i := 0; i < b.N; i++ {
					if _, err := Decode(blob, DefaultLimits()); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
