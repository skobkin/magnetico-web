package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Ingest memory investigation (Task 06 review correction 5).
//
// Opt-in: runs only when MAGNETICO_E2E_PROFILE_DIR names an output directory.
// It starts the test-only ingestd wrapper (production server.Run plus a
// loopback pprof listener and a MEM line every 2 s), drives the synthetic
// workload through the production crawler delivery components, captures heap /
// allocs / goroutine profiles at fixed points and reports live heap vs.
// runtime-retained memory vs. RSS.
//
//	MAGNETICO_E2E_PROFILE_DIR=/tmp/prof \
//	MAGNETICO_E2E_PROFILE_SECONDS=90 \
//	MAGNETICO_E2E_PROFILE_ENV="GOMEMLIMIT=128MiB" \
//	go test -count=1 -p 1 -timeout 900s -run TestE2EIngestMemoryProfile -v ./internal/e2e/

func buildIngestd(t *testing.T) string {
	t.Helper()
	binaries(t) // ensures binDir
	out := filepath.Join(binDir, "ingestd")
	if _, err := os.Stat(out); err == nil {
		return out
	}
	root, _ := filepath.Abs("../..")
	cmd := exec.Command("go", "build", "-o", out, "./internal/e2e/ingestd")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ingestd: %v\n%s", err, b)
	}
	return out
}

func fetchTo(t *testing.T, url, path string) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		t.Fatal(err)
	}
}

var memRe = regexp.MustCompile(`MEM t=(\d+) rss_kib=(\d+) rss_anon_kib=(\d+) rss_file_kib=(\d+) hwm_kib=(\d+) heap_alloc=(\d+) heap_inuse=(\d+) heap_idle=(\d+) heap_released=(\d+) heap_sys=(\d+) stack_inuse=(\d+) sys=(\d+) live=(\d+) goal=(\d+) total_classes=(\d+) unused=(\d+) num_gc=(\d+) pause_total_ms=(\d+) goroutines=(\d+)`)

type memSample struct {
	t                                                                          int
	rss, anon, file, hwm, alloc, inuse, idle, released, sys, stack, live, goal int // KiB (heap_* converted)
	numGC, pauseMs, goroutines                                                 int
}

func lastMem(out string) (memSample, bool) {
	all := memRe.FindAllStringSubmatch(out, -1)
	if len(all) == 0 {
		return memSample{}, false
	}
	m := all[len(all)-1]
	v := func(i int) int { n, _ := strconv.Atoi(m[i]); return n }
	k := func(i int) int { return v(i) >> 10 }
	return memSample{t: v(1), rss: v(2), anon: v(3), file: v(4), hwm: v(5),
		alloc: k(6), inuse: k(7), idle: k(8), released: k(9), sys: k(12), stack: k(11),
		live: k(13), goal: k(14), numGC: v(17), pauseMs: v(18), goroutines: v(19)}, true
}

func (m memSample) String() string {
	return fmt.Sprintf("rss=%dMiB (anon %d, file %d) heap_live=%dMiB heap_alloc=%dMiB heap_inuse=%dMiB heap_idle=%dMiB (released %d) goal=%dMiB stack=%dMiB gc=%d pause=%dms goroutines=%d",
		m.rss>>10, m.anon>>10, m.file>>10, m.live>>10, m.alloc>>10, m.inuse>>10, m.idle>>10, m.released>>10, m.goal>>10, m.stack>>10, m.numGC, m.pauseMs, m.goroutines)
}

func TestE2EIngestMemoryProfile(t *testing.T) {
	dir := os.Getenv("MAGNETICO_E2E_PROFILE_DIR")
	if dir == "" {
		t.Skip("set MAGNETICO_E2E_PROFILE_DIR to run the ingest memory investigation")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	secs := 90
	if v, err := strconv.Atoi(os.Getenv("MAGNETICO_E2E_PROFILE_SECONDS")); err == nil && v > 0 {
		secs = v
	}
	const rate = 40
	total := rate * secs
	burst := 3000

	w := newWorld(t)
	ingestd := buildIngestd(t)
	addr, pprofAddr := freePort(t), freePort(t)
	env := []string{
		"MAGNETICO_INGEST_TOKEN=" + token,
		"MAGNETICO_INGEST_DATABASE=" + w.dsn,
		"MAGNETICO_INGEST_LISTEN=" + addr,
		"INGESTD_PPROF=" + pprofAddr,
	}
	if extra := strings.Fields(os.Getenv("MAGNETICO_E2E_PROFILE_ENV")); len(extra) > 0 {
		env = append(env, extra...)
		t.Logf("ingestd extra environment: %v", extra)
	}
	ing := start(t, env, ingestd)
	t.Cleanup(func() { ing.term(); ing.wait(t, 40*time.Second) })
	eventually(t, "ingestd ready", 60*time.Second, func() bool {
		_, err := http.Get("http://" + pprofAddr + "/debug/pprof/")
		return err == nil
	})

	snap := func(label string) memSample {
		t.Helper()
		time.Sleep(2500 * time.Millisecond) // one fresh MEM line
		m, ok := lastMem(ing.output())
		if !ok {
			t.Fatal("no MEM line from ingestd")
		}
		t.Logf("%-28s %s", label, m)
		return m
	}
	prof := func(name string) {
		fetchTo(t, "http://"+pprofAddr+"/debug/pprof/heap?gc=1", filepath.Join(dir, name+".heap.pb.gz"))
		fetchTo(t, "http://"+pprofAddr+"/debug/pprof/allocs", filepath.Join(dir, name+".allocs.pb.gz"))
		fetchTo(t, "http://"+pprofAddr+"/debug/pprof/goroutine?debug=1", filepath.Join(dir, name+".goroutines.txt"))
	}

	idle := snap("idle after start")

	// Phase 1: expected rate (40/s, measured size mix).
	cq := t.TempDir()
	cr := w.crawlerd(cq, "http://"+addr, "-count", strconv.Itoa(total), "-rate", strconv.Itoa(rate))
	peak := idle
	half := time.Now().Add(time.Duration(secs/2) * time.Second)
	profiledMid := false
	for {
		select {
		case <-cr.done:
			goto phase1done
		default:
		}
		time.Sleep(2 * time.Second)
		if m, ok := lastMem(ing.output()); ok && m.rss > peak.rss {
			peak = m
		}
		if !profiledMid && time.Now().After(half) {
			prof("steady-mid")
			profiledMid = true
		}
	}
phase1done:
	if code := cr.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("crawler exit %d:\n%s", code, tail(cr.output()))
	}
	w.settle(total)
	afterLoad := snap("after load (settled)")
	prof("after-load")

	// Phase 2: burst, as fast as the pipeline allows.
	cr2 := w.crawlerd(t.TempDir(), "http://"+addr, "-from", "1000001", "-count", strconv.Itoa(burst))
	for {
		select {
		case <-cr2.done:
			goto phase2done
		default:
		}
		time.Sleep(time.Second)
		if m, ok := lastMem(ing.output()); ok && m.rss > peak.rss {
			peak = m
		}
	}
phase2done:
	w.settle(total + burst)
	afterBurst := snap("after burst (settled)")
	prof("after-burst")

	// Runtime-retained vs live: give unused heap back to the OS and see what
	// RSS remains.
	fetchTo(t, "http://"+pprofAddr+"/debug/freeos", filepath.Join(dir, "freeos.txt"))
	afterFree := snap("after FreeOSMemory")

	t.Logf("SUMMARY idle_rss=%dMiB peak_rss=%dMiB after_load_rss=%dMiB after_burst_rss=%dMiB after_freeos_rss=%dMiB "+
		"after_freeos_live_heap=%dMiB peak_hwm=%dMiB",
		idle.rss>>10, peak.rss>>10, afterLoad.rss>>10, afterBurst.rss>>10, afterFree.rss>>10, afterFree.live>>10, afterFree.hwm>>10)
	if err := os.WriteFile(filepath.Join(dir, "ingestd-mem.log"), []byte(ing.output()), 0o644); err != nil {
		t.Fatal(err)
	}
}
