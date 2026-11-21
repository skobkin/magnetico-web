package codec

import (
	"fmt"
	"runtime"
	"testing"
)

// BenchmarkEncoderReuse measures a long-lived single-worker Encoder, the
// steady-state shape of the migration and crawler writers. BenchmarkEncode
// (package-level Encode, fresh state per call) is the one-shot reference.
func BenchmarkEncoderReuse(b *testing.B) {
	for _, n := range []int{10, 1_000, 50_000} {
		files := sampleFiles(n)
		for _, p := range []Profile{ProfileCompact, ProfileMaximum} {
			b.Run(fmt.Sprintf("%s/files-%d", p.name(), n), func(b *testing.B) {
				enc := NewEncoder(p, DefaultLimits(), 1)
				defer enc.Close()
				if _, _, err := enc.Encode(files); err != nil { // warm the state
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := enc.Encode(files); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkEncoderReuseParallel drives a 4-worker Encoder from parallel
// goroutines and reports memory-pressure metrics next to ns/op and B/op:
// heap-inuse-MiB is HeapInuse after a forced GC at the end (retained heap),
// heap-sys-MiB is HeapSys at the end of the timed run (heap footprint the
// runtime obtained from the OS), and gc/op is completed GC cycles per
// operation.
func BenchmarkEncoderReuseParallel(b *testing.B) {
	const workers = 4
	for _, n := range []int{1_000, 50_000} {
		files := sampleFiles(n)
		for _, p := range []Profile{ProfileCompact, ProfileMaximum} {
			b.Run(fmt.Sprintf("%s/files-%d", p.name(), n), func(b *testing.B) {
				enc := NewEncoder(p, DefaultLimits(), workers)
				defer enc.Close()
				if _, _, err := enc.Encode(files); err != nil {
					b.Fatal(err)
				}

				var before runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&before)
				b.SetParallelism(workers)
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						if _, _, err := enc.Encode(files); err != nil {
							b.Error(err)
							return
						}
					}
				})
				b.StopTimer()

				var after runtime.MemStats
				runtime.ReadMemStats(&after)
				peak := after.HeapSys // highest heap footprint the runtime requested from the OS
				runtime.GC()
				var settled runtime.MemStats
				runtime.ReadMemStats(&settled)
				b.ReportMetric(float64(settled.HeapInuse)/(1<<20), "heap-inuse-MiB")
				b.ReportMetric(float64(peak)/(1<<20), "heap-sys-MiB")
				if b.N > 0 {
					b.ReportMetric(float64(after.NumGC-before.NumGC)/float64(b.N), "gc/op")
				}
			})
		}
	}
}
