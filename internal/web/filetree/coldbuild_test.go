package filetree_test

// Explicit, expensive cold-build measurements for the C1 review (plan 07E §8).
// None of this runs in the ordinary suite:
//
//	# 1. write the synthetic blobs (forced serializer × compressor pairs)
//	MAGNETICO_BLOB_DIR=$DIR go test -run TestDumpBenchBlobs -timeout 1h ./codec
//	# 2. isolated-process matrix (one child process per measurement)
//	MAGNETICO_BENCH_LARGE=1 MAGNETICO_BLOB_DIR=$DIR MAGNETICO_BENCH_OUT=$OUT \
//	    go test -run TestColdBuildReport -timeout 2h -v ./internal/web/filetree
//	# 3. in-process Go benchmarks (ns/op, B/op, allocs/op)
//	MAGNETICO_BLOB_DIR=$DIR go test -run '^$' -bench ColdBuild -benchmem ./internal/web/filetree
//
// Each measurement runs in its own child process (the test binary re-executed
// with MAGNETICO_COLD_CHILD set) so that peak RSS (VmHWM) and peak heap are not
// polluted by an earlier case.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/benchkit"
	"github.com/skobkin/magnetico-web/internal/web/filetree"
)

type blobMeta struct {
	Dataset   string         `json:"dataset"`
	Files     int            `json:"files"`
	PathBytes int            `json:"path_bytes"`
	TotalSize int64          `json:"total_size"`
	Variants  map[string]int `json:"stored_bytes"`
}

// The three measured modes: the codec alone, then each index variant on top.
const (
	modeDecode = "decode"
	modeArena  = "arena"
	modeRef    = "ref"
)

type coldResult struct {
	Dataset, Blob, Mode string
	Stored              int
	Declared            uint64
	Files, Dirs         int
	PathBytes           int
	GOMAXPROCS          int
	Conc                int
	LatP50Ms, LatP95Ms  float64 // per-build latency under Conc>1
	Reps                int
	ColdMinMs           float64
	ColdMaxMs           float64

	// Peak run: decode + build back to back, no forced GC in between.
	ColdWallMs     float64
	BaseHeap       uint64 // live heap before the blob was read
	PeakHeap       uint64 // max sampled HeapAlloc (includes the blob)
	PeakHeapInuse  uint64
	PeakSys        uint64
	PeakRSSSampled uint64
	HWM            uint64 // kernel peak RSS during the run
	BaseRSS        uint64

	// Phase run: each phase measured separately after a forced GC.
	DecodeMs      float64
	DecodeAlloc   uint64
	DecodeMallocs uint64
	IndexMs       float64
	IndexAlloc    uint64
	IndexMallocs  uint64
	Retained      uint64 // live heap after the build minus the baseline (blob excluded)
	IndexBytes    int    // the index's own accounting
}

type coldChildSpec struct {
	Dataset, Blob, Mode, Path string
	Conc                      int // >1: that many simultaneous cold builds in one process (peak run only)
}

func peekDeclared(blob []byte) uint64 {
	_, d, err := codec.PeekDecodedSize(blob[:min(len(blob), codec.PeekHeaderLen)], len(blob), codec.DefaultLimits())
	if err != nil {
		return 0
	}
	return d
}

func buildMode(mode string, files []codec.File) (filetree.Index, error) {
	switch mode {
	case modeArena:
		return filetree.BuildArena(context.Background(), files)
	case modeRef:
		return filetree.BuildRef(context.Background(), files)
	}
	return nil, nil
}

// TestColdBuildChild is the child half; it only runs when re-executed by the
// parent and prints one "RESULT {json}" line.
func TestColdBuildChild(t *testing.T) {
	raw := os.Getenv("MAGNETICO_COLD_CHILD")
	if raw == "" {
		t.Skip("child of TestColdBuildReport")
	}
	var spec coldChildSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	lim := codec.DefaultLimits()
	res := coldResult{Dataset: spec.Dataset, Blob: spec.Blob, Mode: spec.Mode, GOMAXPROCS: runtime.GOMAXPROCS(0)}

	res.BaseHeap = benchkit.HeapLive()
	blob, err := os.ReadFile(spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	res.Stored = len(blob)
	res.Declared = peekDeclared(blob)
	benchkit.HeapLive()
	benchkit.ResetPeakRSS()
	res.BaseRSS = benchkit.ReadStatus().RSS

	res.Conc = max(spec.Conc, 1)
	if spec.Conc > 1 {
		// Simultaneous cold builds with no gate: what an unbounded burst costs.
		// Finished indexes are kept alive until the end (worst case: nothing is
		// evicted or garbage-collected early).
		keep := make([]filetree.Index, spec.Conc)
		probe := benchkit.StartProbe(2 * time.Millisecond)
		t0 := time.Now()
		rr := benchkit.Run(context.Background(), spec.Conc, spec.Conc, func(_, j int) error {
			f, err := codec.Decode(blob, lim)
			if err != nil {
				return err
			}
			if j == 0 {
				res.Files, res.PathBytes = len(f), 0
				for _, x := range f {
					res.PathBytes += len(x.Path)
				}
			}
			keep[j], err = buildMode(spec.Mode, f)
			return err
		})
		res.ColdWallMs = float64(time.Since(t0).Microseconds()) / 1000
		pk := probe.Stop()
		st := benchkit.ReadStatus()
		res.PeakHeap, res.PeakHeapInuse, res.PeakSys, res.PeakRSSSampled, res.HWM = pk.HeapAlloc, pk.HeapInuse, pk.Sys, pk.RSS, st.HWM
		res.LatP50Ms, res.LatP95Ms = float64(rr.Latency.P50.Microseconds())/1000, float64(rr.Latency.P95.Microseconds())/1000
		if rr.OK != spec.Conc {
			t.Fatalf("%d of %d builds failed: %v", spec.Conc-rr.OK, spec.Conc, rr.Errors)
		}
		runtime.KeepAlive(keep)
		runtime.KeepAlive(blob)
		b, _ := json.Marshal(res)
		fmt.Printf("RESULT %s\n", b)
		return
	}

	// ---- peak run ----
	probe := benchkit.StartProbe(2 * time.Millisecond)
	t0 := time.Now()
	files, err := codec.Decode(blob, lim)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Mode != modeDecode {
		if _, err := buildMode(spec.Mode, files); err != nil {
			t.Fatal(err)
		}
	}
	res.ColdWallMs = float64(time.Since(t0).Microseconds()) / 1000
	pk := probe.Stop()
	st := benchkit.ReadStatus()
	res.PeakHeap, res.PeakHeapInuse, res.PeakSys, res.PeakRSSSampled, res.HWM = pk.HeapAlloc, pk.HeapInuse, pk.Sys, pk.RSS, st.HWM
	res.Files = len(files)
	for _, f := range files {
		res.PathBytes += len(f.Path)
	}
	files = nil

	// ---- phase run ----
	var dec []codec.File
	a := benchkit.Measure(func() { dec, err = codec.Decode(blob, lim) })
	if err != nil {
		t.Fatal(err)
	}
	res.DecodeMs, res.DecodeAlloc, res.DecodeMallocs = float64(a.Wall.Microseconds())/1000, a.TotalAlloc, a.Mallocs
	var ix filetree.Index
	if spec.Mode != modeDecode {
		a = benchkit.Measure(func() { ix, err = buildMode(spec.Mode, dec) })
		if err != nil {
			t.Fatal(err)
		}
		res.IndexMs, res.IndexAlloc, res.IndexMallocs = float64(a.Wall.Microseconds())/1000, a.TotalAlloc, a.Mallocs
		res.IndexBytes, res.Dirs = ix.Bytes(), ix.Dirs()
	}
	if spec.Mode != modeRef && spec.Mode != modeDecode {
		dec = nil // the arena index must not need the decoded list
	}
	live := benchkit.HeapLive()
	res.Retained = live - res.BaseHeap - uint64(len(blob))
	if live < res.BaseHeap+uint64(len(blob)) {
		res.Retained = 0
	}
	runtime.KeepAlive(ix)
	runtime.KeepAlive(dec)
	runtime.KeepAlive(blob) // the baseline subtracts the blob, so it must stay live until measured

	b, _ := json.Marshal(res)
	fmt.Printf("RESULT %s\n", b)
}

func listBlobs(t testing.TB, dir string) map[string]*blobMeta {
	out := map[string]*blobMeta{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("MAGNETICO_BLOB_DIR unreadable: %v", err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m blobMeta
		if json.Unmarshal(b, &m) == nil {
			out[m.Dataset] = &m
		}
	}
	return out
}

// TestColdBuildReport runs the isolated matrix and writes tables.
func TestColdBuildReport(t *testing.T) {
	if !benchkit.Enabled() {
		t.Skipf("expensive: set %s=1", benchkit.EnvLarge)
	}
	dir := os.Getenv("MAGNETICO_BLOB_DIR")
	if dir == "" {
		t.Skip("set MAGNETICO_BLOB_DIR (see the file comment)")
	}
	outDir := os.Getenv("MAGNETICO_BENCH_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}
	procs := os.Getenv("MAGNETICO_COLD_GOMAXPROCS")
	if procs == "" {
		procs = "2"
	}
	dsRe := regexp.MustCompile(envOr("MAGNETICO_COLD_DATASETS", "."))
	vRe := regexp.MustCompile(envOr("MAGNETICO_COLD_VARIANTS", "."))
	modes := strings.Fields(envOr("MAGNETICO_COLD_MODES", "decode arena ref"))

	metas := listBlobs(t, dir)
	var names []string
	for n := range metas {
		names = append(names, n)
	}
	sort.Strings(names)

	reps := 1
	if v := os.Getenv("MAGNETICO_COLD_REPEAT"); v != "" {
		fmt.Sscan(v, &reps)
	}
	concs := []int{1}
	if v := os.Getenv("MAGNETICO_COLD_CONC"); v != "" {
		concs = nil
		for _, f := range strings.Fields(v) {
			var c int
			fmt.Sscan(f, &c)
			concs = append(concs, c)
		}
	}

	var all []coldResult
	var raw bytes.Buffer
	for _, ds := range names {
		if !dsRe.MatchString(ds) {
			continue
		}
		var variants []string
		for v := range metas[ds].Variants {
			variants = append(variants, v)
		}
		sort.Strings(variants)
		for _, v := range variants {
			if !vRe.MatchString(v) {
				continue
			}
			for _, mode := range modes {
				for _, conc := range concs {
					for rep := 0; rep < reps; rep++ {
						spec, _ := json.Marshal(coldChildSpec{Dataset: ds, Blob: v, Mode: mode, Path: filepath.Join(dir, ds, v+".blob"), Conc: conc})
						cmd := exec.Command(os.Args[0], "-test.run=^TestColdBuildChild$", "-test.v=false")
						cmd.Env = append(os.Environ(), "MAGNETICO_COLD_CHILD="+string(spec), "GOMAXPROCS="+procs)
						var stdout, stderr bytes.Buffer
						cmd.Stdout, cmd.Stderr = &stdout, &stderr
						if err := cmd.Run(); err != nil {
							t.Errorf("%s/%s/%s x%d: %v\n%s", ds, v, mode, conc, err, stderr.String()+stdout.String())
							continue
						}
						var found bool
						for _, line := range strings.Split(stdout.String(), "\n") {
							if rest, ok := strings.CutPrefix(line, "RESULT "); ok {
								var r coldResult
								if err := json.Unmarshal([]byte(rest), &r); err != nil {
									t.Fatal(err)
								}
								all = append(all, r)
								raw.WriteString(rest + "\n")
								found = true
							}
						}
						if !found {
							t.Errorf("%s/%s/%s x%d: no result:\n%s", ds, v, mode, conc, stdout.String())
						}
					}
				}
			}
		}
	}
	results := medians(all)
	if err := os.WriteFile(filepath.Join(outDir, "coldbuild.jsonl"), raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	machine := "# machine: " + benchkit.Describe().String() + fmt.Sprintf("\n# child GOMAXPROCS=%s\n", procs)
	if err := os.WriteFile(filepath.Join(outDir, "coldbuild.md"), []byte(machine+renderTable(results)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d results)", outDir, len(results))
}

// medians keeps, per (dataset, blob, mode, conc), the repetition with the
// median cold wall time and records the min and max cold time of the group.
func medians(all []coldResult) []coldResult {
	type key struct {
		d, b, m string
		c       int
	}
	groups := map[key][]coldResult{}
	var order []key
	for _, r := range all {
		k := key{r.Dataset, r.Blob, r.Mode, r.Conc}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	var out []coldResult
	for _, k := range order {
		g := groups[k]
		sort.Slice(g, func(i, j int) bool { return g[i].ColdWallMs < g[j].ColdWallMs })
		m := g[len(g)/2]
		m.Reps, m.ColdMinMs, m.ColdMaxMs = len(g), g[0].ColdWallMs, g[len(g)-1].ColdWallMs
		out = append(out, m)
	}
	return out
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func mib(b uint64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<20)) }

func renderTable(rs []coldResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "| dataset | blob | mode | conc | reps | files | dirs | stored MiB | declared MiB | path MiB | cold ms (med [min–max]) | decode ms | index ms | decode alloc MiB | decode mallocs | index alloc MiB | index mallocs | retained MiB | index.Bytes MiB | peak heap MiB | HWM-Δ MiB |")
	fmt.Fprintln(&b, "|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, r := range rs {
		hwm := uint64(0)
		if r.HWM > r.BaseRSS {
			hwm = r.HWM - r.BaseRSS
		}
		peak := uint64(0)
		if r.PeakHeap > r.BaseHeap {
			peak = r.PeakHeap - r.BaseHeap
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d | %s | %s | %s | %.0f [%.0f–%.0f] | %.0f | %.0f | %s | %d | %s | %d | %s | %s | %s | %s |\n",
			r.Dataset, r.Blob, r.Mode, r.Conc, r.Reps, r.Files, r.Dirs, mib(uint64(r.Stored)), mib(r.Declared), mib(uint64(r.PathBytes)),
			r.ColdWallMs, r.ColdMinMs, r.ColdMaxMs, r.DecodeMs, r.IndexMs, mib(r.DecodeAlloc), r.DecodeMallocs, mib(r.IndexAlloc), r.IndexMallocs,
			mib(r.Retained), mib(uint64(r.IndexBytes)), mib(peak), mib(hwm))
	}
	return b.String()
}

// ---- in-process Go benchmarks ---------------------------------------------------

// BenchmarkColdBuild measures decode and each index variant in-process
// (ns/op, B/op, allocs/op). Datasets come from $MAGNETICO_BLOB_DIR; the blob
// variant is $MAGNETICO_BENCH_BLOB (default prefix-brotli9). Memory peaks are
// the child-process matrix's job.
func BenchmarkColdBuild(b *testing.B) {
	dir := os.Getenv("MAGNETICO_BLOB_DIR")
	if dir == "" {
		b.Skip("set MAGNETICO_BLOB_DIR")
	}
	variant := envOr("MAGNETICO_BENCH_BLOB", "prefix-brotli9")
	re := regexp.MustCompile(envOr("MAGNETICO_BENCH_DATASETS", "typical-250k|deep-250k|wide-250k|longpath-250k|adversarial-50k"))
	lim := codec.DefaultLimits()
	for ds := range listBlobs(b, dir) {
		if !re.MatchString(ds) {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(dir, ds, variant+".blob"))
		if err != nil {
			continue
		}
		files, err := codec.Decode(blob, lim)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(ds+"/decode", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := codec.Decode(blob, lim); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(ds+"/index-arena", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := filetree.BuildArena(context.Background(), files); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(ds+"/index-ref", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := filetree.BuildRef(context.Background(), files); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(ds+"/cold-arena", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				f, err := codec.Decode(blob, lim)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := filetree.BuildArena(context.Background(), f); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkQuery measures the warm paths on a built index (Lookup and a
// 500-row page), the cost a cached navigation pays on top of the cache.
func BenchmarkQuery(b *testing.B) {
	dir := os.Getenv("MAGNETICO_BLOB_DIR")
	if dir == "" {
		b.Skip("set MAGNETICO_BLOB_DIR")
	}
	for ds := range listBlobs(b, dir) {
		if ds != "wide-250k" && ds != "typical-250k" && ds != "deep-250k" {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(dir, ds, "prefix-brotli9.blob"))
		if err != nil {
			continue
		}
		files, err := codec.Decode(blob, codec.DefaultLimits())
		if err != nil {
			b.Fatal(err)
		}
		a, _ := filetree.BuildArena(context.Background(), files)
		r, _ := filetree.BuildRef(context.Background(), files)
		// The deepest directory and the biggest one.
		deepest, biggest := "", ""
		var bigN uint32
		seen := map[string]bool{}
		for _, f := range files {
			if i := strings.LastIndexByte(f.Path, '/'); i >= 0 {
				d := f.Path[:i+1]
				if len(d) > len(deepest) {
					deepest = d
				}
				if !seen[d] {
					seen[d] = true
					if id, ok := a.Lookup(d); ok && a.Info(id).NFiles > bigN {
						bigN, biggest = a.Info(id).NFiles, d
					}
				}
			}
		}
		for _, v := range []struct {
			name string
			ix   filetree.Index
		}{{"arena", a}, {"ref", r}} {
			for _, dir := range []struct{ name, path string }{{"root", ""}, {"deepest", deepest}, {"biggest", biggest}} {
				b.Run(fmt.Sprintf("%s/%s/%s/lookup", ds, v.name, dir.name), func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						if _, ok := v.ix.Lookup(dir.path); !ok {
							b.Fatal("lookup")
						}
					}
				})
				b.Run(fmt.Sprintf("%s/%s/%s/page500", ds, v.name, dir.name), func(b *testing.B) {
					id, _ := v.ix.Lookup(dir.path)
					buf := make([]filetree.Entry, 0, 500)
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						buf = v.ix.Page(id, 0, 500, buf[:0])
					}
				})
			}
		}
	}
}
