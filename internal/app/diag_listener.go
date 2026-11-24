//go:build diag

package app

// Test-only diagnostic build (task 07J): `go build -tags diag`. It serves
// non-destructive runtime statistics (goroutine counts grouped by creation
// site, memory statistics, file-cache and connection-pool statistics) as JSON on
// a loopback listener chosen by MAGNETICO_DIAG_ADDR, so a soak can compare
// goroutine and heap numbers of ONE process before, during and after the load
// without SIGQUIT (which would end it) and without any pprof endpoint in the
// production binary. The listener refuses non-loopback addresses and the output
// carries no names, paths, queries or secrets. A normal build excludes this file
// (TestBinaryDoesNotLinkTestSupport and TestDiagTagIsAbsentFromTheDefaultBuild).

import (
	"bytes"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
)

func init() { diagHook = startDiag }

var creatorRe = regexp.MustCompile(`(?m)^#\s+0x[0-9a-f]+\s+(\S+)`)

func startDiag(src diagSources) {
	addr := os.Getenv("MAGNETICO_DIAG_ADDR")
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatalln("MAGNETICO_DIAG_ADDR must be a loopback address")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalln("diagnostic listener:", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/diag", func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		var buf bytes.Buffer
		_ = pprof.Lookup("goroutine").WriteTo(&buf, 1)
		// Group the goroutines by the function at the bottom of their stack: where
		// each one was started.
		groups := map[string]int{}
		for _, block := range strings.Split(buf.String(), "\n\n") {
			var n int
			if _, err := fmtSscanf(block, &n); err != nil || n == 0 {
				continue
			}
			ms := creatorRe.FindAllStringSubmatch(block, -1)
			if len(ms) > 0 {
				groups[ms[len(ms)-1][1]] += n
			}
		}
		type kv struct {
			Where string `json:"where"`
			N     int    `json:"n"`
		}
		var gl []kv
		for k, v := range groups {
			gl = append(gl, kv{k, v})
		}
		sort.Slice(gl, func(i, j int) bool { return gl[i].N > gl[j].N })
		ps := src.Backend.Pool().Stat()
		out := map[string]any{
			"goroutines": runtime.NumGoroutine(), "goroutine_groups": gl,
			"heap_alloc": ms.HeapAlloc, "heap_inuse": ms.HeapInuse, "heap_sys": ms.HeapSys, "sys": ms.Sys, "num_gc": ms.NumGC,
			"pause_total_ns": ms.PauseTotalNs, "next_gc": ms.NextGC, "gc_cpu_fraction": ms.GCCPUFraction,
			"file_cache": src.FileCache.Stats(),
			"pool":       map[string]any{"acquired": ps.AcquiredConns(), "idle": ps.IdleConns(), "total": ps.TotalConns(), "max": ps.MaxConns(), "acquire_count": ps.AcquireCount(), "empty_acquire": ps.EmptyAcquireCount(), "canceled_acquire": ps.CanceledAcquireCount()},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	// In-use heap by allocation site (after a forced GC, so the figures are the
	// live heap): the leaf function of each allocation stack, largest first.
	mux.HandleFunc("/heap", func(w http.ResponseWriter, r *http.Request) {
		runtime.GC()
		var buf bytes.Buffer
		_ = pprof.Lookup("heap").WriteTo(&buf, 1)
		type site struct {
			Func    string `json:"func"`
			Bytes   int64  `json:"inuse_bytes"`
			Objects int64  `json:"inuse_objects"`
		}
		agg := map[string]*site{}
		var cur *site
		var curBytes, curObjs int64
		headerRe := regexp.MustCompile(`^(\d+): (\d+) \[\d+: \d+\] @`)
		frameRe := regexp.MustCompile(`^#\s+0x[0-9a-f]+\s+(\S+)`)
		for _, line := range strings.Split(buf.String(), "\n") {
			if m := headerRe.FindStringSubmatch(line); m != nil {
				curObjs, _ = strconv.ParseInt(m[1], 10, 64)
				curBytes, _ = strconv.ParseInt(m[2], 10, 64)
				cur = nil
				continue
			}
			if m := frameRe.FindStringSubmatch(line); m != nil && cur == nil && (curBytes > 0 || curObjs > 0) {
				if agg[m[1]] == nil {
					agg[m[1]] = &site{Func: m[1]}
				}
				cur = agg[m[1]]
				cur.Bytes += curBytes
				cur.Objects += curObjs
			}
		}
		var top []*site
		for _, s := range agg {
			top = append(top, s)
		}
		sort.Slice(top, func(i, j int) bool { return top[i].Bytes > top[j].Bytes })
		if len(top) > 25 {
			top = top[:25]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"top_inuse": top})
	})
	go func() { _ = http.Serve(ln, mux) }()
	log.Println("diag build: diagnostics on", ln.Addr())
}
