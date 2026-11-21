package codec

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkEncoderStress is an opt-in sustained-load benchmark: four
// concurrent callers encode a large file list through one 4-worker Encoder
// for a fixed wall-clock duration, while a sampler records process memory
// once per second. It exists to expose transient Brotli writer allocation
// and GC pressure that a short benchmark loop hides.
//
// It is skipped unless CODEC_STRESS is set, and is meant to be run with a
// single iteration in its own process so VmHWM is the peak of this workload:
//
//	CODEC_STRESS=compact CODEC_STRESS_SECONDS=120 \
//	  go test -run '^$' -bench '^BenchmarkEncoderStress$' -benchtime 1x ./codec/
//
// CODEC_STRESS is "compact" or "maximum" (the profile). CODEC_STRESS_SECONDS
// defaults to 60. CODEC_STRESS_FILES defaults to 50000 files (a payload well
// above one Brotli block). Per-second samples are logged (-v); the reported
// metrics are:
//
//	ops/s            completed Encode calls per second
//	peak-rss-MiB     VmHWM at the end (peak resident set, Linux only)
//	rss-warm-MiB     max VmRSS sampled in the second quarter of the run
//	rss-late-MiB     max VmRSS sampled in the last quarter; a value well above
//	                 rss-warm-MiB means memory keeps growing after warm-up
//	heap-sys-MiB     max HeapSys sampled
//	gc/s             GC cycles per second
//	gc-pause-ms      total stop-the-world GC pause
//	mallocMiB/op     bytes allocated per Encode
func BenchmarkEncoderStress(b *testing.B) {
	mode := os.Getenv("CODEC_STRESS")
	if mode == "" {
		b.Skip("set CODEC_STRESS=compact|maximum to run the sustained-load benchmark")
	}
	var p Profile
	switch mode {
	case "compact":
		p = ProfileCompact
	case "maximum":
		p = ProfileMaximum
	default:
		b.Fatalf("CODEC_STRESS=%q, want compact or maximum", mode)
	}
	seconds := envInt(b, "CODEC_STRESS_SECONDS", 60)
	nfiles := envInt(b, "CODEC_STRESS_FILES", 50_000)
	const workers = 4

	files := sampleFiles(nfiles)
	enc := NewEncoder(p, DefaultLimits(), workers)
	defer enc.Close()
	if _, _, err := enc.Encode(files); err != nil {
		b.Fatal(err)
	}

	type sample struct {
		t            float64
		rss, heapSys uint64
		heapInuse    uint64
		numGC        uint32
		ops          int64
	}
	var (
		ops     atomic.Int64
		stop    atomic.Bool
		wg      sync.WaitGroup
		samples []sample
	)

	var base runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&base)

	start := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if _, _, err := enc.Encode(files); err != nil {
					b.Error(err)
					return
				}
				ops.Add(1)
			}
		}()
	}

	b.ResetTimer()
	tick := time.NewTicker(time.Second)
	deadline := time.After(time.Duration(seconds) * time.Second)
sampling:
	for {
		select {
		case <-tick.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			rss, _ := procStatusKB("VmRSS")
			samples = append(samples, sample{
				t: time.Since(start).Seconds(), rss: rss << 10, heapSys: m.HeapSys,
				heapInuse: m.HeapInuse, numGC: m.NumGC, ops: ops.Load(),
			})
		case <-deadline:
			break sampling
		}
	}
	tick.Stop()
	stop.Store(true)
	wg.Wait()
	b.StopTimer()
	elapsed := time.Since(start).Seconds()

	var end runtime.MemStats
	runtime.ReadMemStats(&end)
	hwm, ok := procStatusKB("VmHWM")

	var warmMax, lateMax, sysMax uint64
	n := len(samples)
	for i, s := range samples {
		switch {
		case i >= n/4 && i < n/2:
			warmMax = max(warmMax, s.rss)
		case i >= n*3/4:
			lateMax = max(lateMax, s.rss)
		}
		sysMax = max(sysMax, s.heapSys)
		b.Logf("t=%3.0fs ops=%-5d rss=%4d MiB heap-inuse=%4d MiB heap-sys=%4d MiB gc=%d",
			s.t, s.ops, s.rss>>20, s.heapInuse>>20, s.heapSys>>20, s.numGC-base.NumGC)
	}

	total := ops.Load()
	b.ReportMetric(float64(total)/elapsed, "ops/s")
	if ok {
		b.ReportMetric(float64(hwm)/1024, "peak-rss-MiB")
	}
	b.ReportMetric(float64(warmMax)/(1<<20), "rss-warm-MiB")
	b.ReportMetric(float64(lateMax)/(1<<20), "rss-late-MiB")
	b.ReportMetric(float64(sysMax)/(1<<20), "heap-sys-MiB")
	b.ReportMetric(float64(end.NumGC-base.NumGC)/elapsed, "gc/s")
	b.ReportMetric(float64(end.PauseTotalNs-base.PauseTotalNs)/1e6, "gc-pause-ms")
	if total > 0 {
		b.ReportMetric(float64(end.TotalAlloc-base.TotalAlloc)/float64(total)/(1<<20), "mallocMiB/op")
	}
}

func envInt(tb testing.TB, name string, def int) int {
	tb.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		tb.Fatalf("%s=%q, want a positive integer", name, v)
	}
	return n
}

// procStatusKB reads a kB field (VmRSS, VmHWM) from /proc/self/status.
func procStatusKB(field string) (uint64, bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, found := strings.CutPrefix(sc.Text(), field+":")
		if !found {
			continue
		}
		fs := strings.Fields(rest)
		if len(fs) == 0 {
			return 0, false
		}
		v, err := strconv.ParseUint(fs[0], 10, 64)
		return v, err == nil
	}
	return 0, false
}
