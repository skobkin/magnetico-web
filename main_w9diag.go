//go:build w9diag

package main

// Test-only diagnostic build (task 07J): `go build -tags w9diag`. It serves
// non-destructive runtime statistics (goroutine counts grouped by creation
// site, memory statistics, file-cache and connection-pool statistics) as JSON on
// a loopback listener chosen by MAGNETICO_W9_DIAG_ADDR, so a soak can compare
// goroutine and heap numbers of ONE process before, during and after the load
// without SIGQUIT (which would end it) and without any pprof endpoint in the
// production binary. The listener refuses non-loopback addresses and the output
// carries no names, paths, queries or secrets. A normal build excludes this file
// (TestW9BinaryDoesNotLinkTestSupport and TestW9DiagTagIsAbsentFromTheDefaultBuild).

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
	"strings"
)

func init() { diagHook = startDiag }

var creatorRe = regexp.MustCompile(`(?m)^# +0x[0-9a-f]+ +(\S+)`)

func startDiag(src diagSources) {
	addr := os.Getenv("MAGNETICO_W9_DIAG_ADDR")
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatalln("MAGNETICO_W9_DIAG_ADDR must be a loopback address")
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
	go func() { _ = http.Serve(ln, mux) }()
	log.Println("w9diag build: diagnostics on", ln.Addr())
}
