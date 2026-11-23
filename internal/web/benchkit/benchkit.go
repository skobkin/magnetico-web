// Package benchkit is the small measurement toolkit shared by the explicit,
// expensive W4 benchmarks (cold index builds now; the 8/32-concurrent cache
// and HTTP load tests later). It is test-support code, not a benchmark
// framework: a machine/runtime record, exact latency percentiles, a bounded
// concurrent runner, allocation deltas and a peak heap/RSS probe. Nothing in
// the serving path imports it, and it has no dependencies beyond the stdlib.
//
// Expensive workloads are never run by plain `go test`: callers gate them on
// Enabled (MAGNETICO_BENCH_LARGE=1) or on an explicit -bench pattern.
package benchkit

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EnvLarge enables the expensive workloads.
const EnvLarge = "MAGNETICO_BENCH_LARGE"

// Enabled reports whether expensive workloads were explicitly requested.
func Enabled() bool { return os.Getenv(EnvLarge) == "1" }

// ---- machine record -----------------------------------------------------------

// Machine records where and how a measurement ran.
type Machine struct {
	GoVersion  string
	GOOS       string
	GOARCH     string
	CPUModel   string
	NumCPU     int
	GOMAXPROCS int
	GOGC       string
	GOMEMLIMIT string
	Kernel     string
	MemTotal   uint64 // bytes, 0 if unknown
	CgroupMem  string // memory.max, "" if unavailable
	CgroupCPU  string // cpu.max, "" if unavailable
}

func firstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return s
}

// scanKV returns the value of the first "key:" or "key" line in a /proc file.
func scanKV(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// kb parses "123 kB".
func kb(v string) uint64 {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseUint(f[0], 10, 64)
	return n << 10
}

// Describe collects the machine record.
func Describe() Machine {
	m := Machine{
		GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		NumCPU: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0),
		GOGC: os.Getenv("GOGC"), GOMEMLIMIT: os.Getenv("GOMEMLIMIT"),
		CPUModel:  scanKV("/proc/cpuinfo", "model name"),
		Kernel:    firstLine("/proc/sys/kernel/osrelease"),
		MemTotal:  kb(scanKV("/proc/meminfo", "MemTotal")),
		CgroupMem: firstLine("/sys/fs/cgroup/memory.max"),
		CgroupCPU: firstLine("/sys/fs/cgroup/cpu.max"),
	}
	if m.GOGC == "" {
		m.GOGC = "default(100)"
	}
	if m.GOMEMLIMIT == "" {
		m.GOMEMLIMIT = "unset"
	}
	return m
}

func (m Machine) String() string {
	return fmt.Sprintf("%s %s/%s, cpu=%q cores=%d GOMAXPROCS=%d GOGC=%s GOMEMLIMIT=%s kernel=%s mem=%d MiB cgroup(mem=%q cpu=%q)",
		m.GoVersion, m.GOOS, m.GOARCH, m.CPUModel, m.NumCPU, m.GOMAXPROCS, m.GOGC, m.GOMEMLIMIT,
		m.Kernel, m.MemTotal>>20, m.CgroupMem, m.CgroupCPU)
}

// ---- latency distribution -----------------------------------------------------

// Hist records durations and answers exact percentiles (it keeps every sample;
// the workloads here are thousands of requests, not millions). Not safe for
// concurrent use: use one per worker and Merge.
type Hist struct{ d []time.Duration }

func (h *Hist) Add(d time.Duration) { h.d = append(h.d, d) }
func (h *Hist) Len() int            { return len(h.d) }
func (h *Hist) Merge(o *Hist)       { h.d = append(h.d, o.d...) }

// Summary is a latency summary.
type Summary struct {
	N                  int
	Min, Mean, Max     time.Duration
	P50, P90, P95, P99 time.Duration
}

// Summary computes the distribution (nearest-rank percentiles).
func (h *Hist) Summary() Summary {
	n := len(h.d)
	if n == 0 {
		return Summary{}
	}
	s := slices.Clone(h.d)
	slices.Sort(s)
	var sum time.Duration
	for _, v := range s {
		sum += v
	}
	pct := func(p float64) time.Duration {
		i := int(p*float64(n)+0.999999999) - 1
		return s[min(max(i, 0), n-1)]
	}
	return Summary{N: n, Min: s[0], Mean: sum / time.Duration(n), Max: s[n-1],
		P50: pct(0.50), P90: pct(0.90), P95: pct(0.95), P99: pct(0.99)}
}

func (s Summary) String() string {
	return fmt.Sprintf("n=%d min=%v mean=%v p50=%v p90=%v p95=%v p99=%v max=%v",
		s.N, s.Min, s.Mean, s.P50, s.P90, s.P95, s.P99, s.Max)
}

// ---- concurrent runner -----------------------------------------------------------

// RunResult is the outcome of Run.
type RunResult struct {
	Wall    time.Duration
	Latency Summary
	OK      int
	Errors  map[string]int // by error text; callers pass classified errors
}

// Run executes total jobs on `workers` goroutines. fn(worker, job) returns an
// error (nil = success); each call's latency is recorded whether or not it
// failed. It stops early when ctx is done.
func Run(ctx context.Context, workers, total int, fn func(worker, job int) error) RunResult {
	if workers < 1 {
		workers = 1
	}
	var next atomic.Int64
	hists := make([]*Hist, workers)
	errs := make([]map[string]int, workers)
	oks := make([]int, workers)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		hists[w], errs[w] = &Hist{}, map[string]int{}
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for ctx.Err() == nil {
				j := int(next.Add(1)) - 1
				if j >= total {
					return
				}
				t0 := time.Now()
				err := fn(w, j)
				hists[w].Add(time.Since(t0))
				if err != nil {
					errs[w][err.Error()]++
				} else {
					oks[w]++
				}
			}
		}(w)
	}
	wg.Wait()
	res := RunResult{Wall: time.Since(start), Errors: map[string]int{}}
	all := &Hist{}
	for w := range hists {
		all.Merge(hists[w])
		res.OK += oks[w]
		for k, v := range errs[w] {
			res.Errors[k] += v
		}
	}
	res.Latency = all.Summary()
	return res
}

// ---- allocation and memory -------------------------------------------------------

// Alloc is the allocation cost of one measured call.
type Alloc struct {
	Wall       time.Duration
	TotalAlloc uint64 // bytes allocated (cumulative delta; includes garbage)
	Mallocs    uint64
}

// Measure runs fn once and reports its wall time and allocation deltas. The
// heap is collected first so earlier garbage does not skew the delta.
func Measure(fn func()) Alloc {
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	t0 := time.Now()
	fn()
	d := time.Since(t0)
	runtime.ReadMemStats(&b)
	return Alloc{Wall: d, TotalAlloc: b.TotalAlloc - a.TotalAlloc, Mallocs: b.Mallocs - a.Mallocs}
}

// HeapLive forces a collection (twice, to settle finalizers) and returns the
// live heap in bytes (HeapAlloc).
func HeapLive() uint64 {
	runtime.GC()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// Status is the process memory counters of /proc/self/status, in bytes
// (zero where unavailable, e.g. off Linux).
type Status struct{ RSS, HWM uint64 }

func ReadStatus() Status {
	return Status{
		RSS: kb(scanKV("/proc/self/status", "VmRSS")),
		HWM: kb(scanKV("/proc/self/status", "VmHWM")),
	}
}

// ResetPeakRSS resets the kernel's peak-RSS watermark (VmHWM) so a later read
// covers only what follows; best effort (needs /proc/self/clear_refs).
func ResetPeakRSS() bool {
	return os.WriteFile("/proc/self/clear_refs", []byte("5"), 0o200) == nil
}

// Peak is what a Probe saw.
type Peak struct {
	HeapAlloc uint64 // max live+uncollected heap objects sampled
	HeapInuse uint64
	Sys       uint64 // max bytes obtained from the OS by the runtime
	RSS       uint64 // max sampled VmRSS
	Samples   int
}

// Probe samples the Go heap and RSS at a fixed interval until Stop. Sampling
// uses runtime.ReadMemStats, which briefly stops the world, so keep the
// interval at 2 ms or more; a peak shorter than the interval can be missed
// (the kernel's VmHWM, read by ReadStatus, is the authoritative RSS peak).
type Probe struct {
	stop chan struct{}
	done chan Peak
}

func StartProbe(interval time.Duration) *Probe {
	p := &Probe{stop: make(chan struct{}), done: make(chan Peak, 1)}
	go func() {
		var pk Peak
		var m runtime.MemStats
		sample := func() {
			runtime.ReadMemStats(&m)
			pk.HeapAlloc = max(pk.HeapAlloc, m.HeapAlloc)
			pk.HeapInuse = max(pk.HeapInuse, m.HeapInuse)
			pk.Sys = max(pk.Sys, m.Sys)
			pk.RSS = max(pk.RSS, ReadStatus().RSS)
			pk.Samples++
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				sample()
				p.done <- pk
				return
			case <-t.C:
				sample()
			}
		}
	}()
	return p
}

// Stop ends sampling and returns the peaks.
func (p *Probe) Stop() Peak {
	close(p.stop)
	return <-p.done
}
