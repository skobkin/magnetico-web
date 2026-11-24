// Command ingestd is a TEST-ONLY wrapper around the production ingest service
// (internal/ingest/server.Run) used for the memory investigation. It adds what
// the shipped binary deliberately does not have: a loopback pprof listener and
// a periodic line of runtime/process memory statistics. The service code path
// is the production one; configuration comes from the same MAGNETICO_INGEST_*
// environment variables. It is never part of the shipped binary.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // G108: test-only harness daemon; /debug/pprof/* is served only on a loopback listener (see loopbackOnly) and only when INGESTD_PPROF is set
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"syscall"
	"time"

	"github.com/skobkin/magnetico-web/internal/ingest/server"
)

func envInt(name string, def int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(name), 10, 64); err == nil {
		return v
	}
	return def
}

var statusRe = regexp.MustCompile(`(?m)^(VmRSS|RssAnon|RssFile|VmHWM):\s+(\d+) kB`)

// procStatus returns VmRSS, RssAnon, RssFile and VmHWM in KiB.
func procStatus() map[string]int {
	out := map[string]int{}
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return out
	}
	for _, m := range statusRe.FindAllSubmatch(b, -1) {
		n, _ := strconv.Atoi(string(m[2]))
		out[string(m[1])] = n
	}
	return out
}

// loopbackOnly rejects any pprof listen address that is not a literal loopback
// IP with an explicit port. Host names, empty hosts (":6060", all interfaces)
// and wildcard or non-loopback addresses are refused, so the profiling
// listener can never be reachable from the network, however the wrapper is
// started.
func loopbackOnly(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("pprof address %q: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("pprof address %q: port required", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("pprof address %q: host must be a literal loopback IP (127.0.0.0/8 or ::1)", addr)
	}
	return nil
}

// cpuMillis returns this process's cumulative user and system CPU time.
func cpuMillis() (user, sys int64) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, 0
	}
	ms := func(t syscall.Timeval) int64 { return int64(t.Sec)*1000 + int64(t.Usec)/1000 }
	return ms(ru.Utime), ms(ru.Stime)
}

func main() {
	var pprofAddr string
	if v := os.Getenv("INGESTD_PPROF"); v != "" {
		pprofAddr = v
	}
	if pprofAddr != "" {
		if err := loopbackOnly(pprofAddr); err != nil {
			log.Fatalf("ingestd: %v", err)
		}
		// A separate mux listener on loopback only; the ingest API itself does
		// not expose pprof.
		ln, err := net.Listen("tcp", pprofAddr)
		if err != nil {
			log.Fatalf("pprof listen: %v", err)
		}
		http.HandleFunc("/debug/freeos", func(w http.ResponseWriter, _ *http.Request) {
			debug.FreeOSMemory()
			fmt.Fprintln(w, "ok")
		})
		srv := &http.Server{ReadHeaderTimeout: 10 * time.Second} // default mux: pprof and /debug/freeos
		go func() { _ = srv.Serve(ln) }()
	}

	interval := time.Duration(envInt("INGESTD_SAMPLE_MS", 2000)) * time.Millisecond
	go func() {
		samples := []metrics.Sample{
			{Name: "/gc/heap/live:bytes"},
			{Name: "/gc/heap/goal:bytes"},
			{Name: "/memory/classes/total:bytes"},
			{Name: "/memory/classes/heap/released:bytes"},
			{Name: "/memory/classes/heap/unused:bytes"},
			{Name: "/cpu/classes/gc/total:cpu-seconds"},
		}
		start := time.Now()
		for range time.Tick(interval) {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			metrics.Read(samples)
			u := func(i int) uint64 {
				if samples[i].Value.Kind() == metrics.KindUint64 {
					return samples[i].Value.Uint64()
				}
				return 0
			}
			ps := procStatus()
			gcCPU := int64(0)
			if samples[5].Value.Kind() == metrics.KindFloat64 {
				gcCPU = int64(samples[5].Value.Float64() * 1000)
			}
			cpuU, cpuS := cpuMillis()
			fmt.Printf("MEM t=%d rss_kib=%d rss_anon_kib=%d rss_file_kib=%d hwm_kib=%d "+
				"heap_alloc=%d heap_inuse=%d heap_idle=%d heap_released=%d heap_sys=%d stack_inuse=%d sys=%d "+
				"live=%d goal=%d total_classes=%d unused=%d num_gc=%d pause_total_ms=%d goroutines=%d mallocs=%d frees=%d "+
				"cpu_user_ms=%d cpu_sys_ms=%d gc_cpu_ms=%d\n",
				int(time.Since(start).Seconds()), ps["VmRSS"], ps["RssAnon"], ps["RssFile"], ps["VmHWM"],
				ms.HeapAlloc, ms.HeapInuse, ms.HeapIdle, ms.HeapReleased, ms.HeapSys, ms.StackInuse, ms.Sys,
				u(0), u(1), u(2), u(4), ms.NumGC, ms.PauseTotalNs/1e6, runtime.NumGoroutine(), ms.Mallocs, ms.Frees,
				cpuU, cpuS, gcCPU)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := server.Run(ctx, server.Options{
		Listen:           os.Getenv("MAGNETICO_INGEST_LISTEN"),
		DatabaseURL:      os.Getenv("MAGNETICO_INGEST_DATABASE"),
		QueueDatabaseURL: os.Getenv("MAGNETICO_INGEST_QUEUE_DATABASE"),
		Token:            os.Getenv("MAGNETICO_INGEST_TOKEN"),
		QueueMaxBytes:    envInt("MAGNETICO_INGEST_QUEUE_MAX_BYTES", 2<<30),
		CacheMaxBytes:    envInt("MAGNETICO_INGEST_CACHE_MAX_BYTES", 8<<20),
	})
	if err != nil {
		log.Printf("ingestd: %v", err)
		os.Exit(1)
	}
}
