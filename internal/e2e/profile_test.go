package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
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
//	MAGNETICO_E2E_PROFILE_DIR=/var/tmp/prof \
//	MAGNETICO_E2E_PROFILE_SECONDS=90 \
//	MAGNETICO_E2E_PROFILE_ENV="GOMEMLIMIT=128MiB" \
//	go test -count=1 -p 1 -timeout 900s -run TestE2EIngestMemoryProfile -v ./internal/e2e/
//
// Qualification runs (Task 06 / VM qualification) extend it, all test-only:
//
//	MAGNETICO_E2E_PROFILE_RATE        records per second in the steady phase (default 40)
//	MAGNETICO_E2E_PROFILE_BURST       burst size after the steady phase (default 3000)
//	MAGNETICO_E2E_PROFILE_SAMPLE_SECONDS  time-series period (default 10)
//	MAGNETICO_E2E_PROFILE_PROFILE_EVERY_MIN  extra heap/goroutine profiles during the steady phase (default 0 = off)
//	MAGNETICO_E2E_PROFILE_WINDOW_MIN  trend window of the growth stop conditions (default 20)
//	MAGNETICO_E2E_PROFILE_RSS_STOP_MIB  hard stop when ingest anonymous RSS exceeds it (default 1536)
//	MAGNETICO_E2E_PROFILE_MIN_FREE_GIB  hard stop when the profile directory's filesystem has less free space (default 12)
//
// Every sample is appended to <dir>/timeseries.csv. The stop conditions below
// end the run early (with diagnostics preserved) instead of letting a failing
// soak continue; they are documented in docs/wip/06-crawler-ingest-results.md.

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

const (
	warmupWindows     = 1                // first trend window is warm-up and never compared
	backlogStopSec    = 300              // steady-phase backlog above its limit for this long -> stop
	crawlerQueueLimit = 500              // records; steady-phase crawler queue depth limit
	pgBacklogLimit    = 2000             // rows; steady-phase PostgreSQL backlog limit
	drainTimeout      = 15 * time.Minute // burst drain must finish within this
	stallStopSec      = 120              // no persistence progress with a backlog for this long -> stop
	gcShareStopPct    = 60               // GC share of ingest CPU (sustained over gcShareWindow)
	gcShareWindow     = 10 * time.Minute
	goroutineStop     = 200
	errLineRe         = `(?i)(^|[^_a-z])(error|panic|fatal|fail(ed|ure)?|abandon(ed)?)`
)

var (
	cpuRe   = regexp.MustCompile(`cpu_user_ms=(\d+) cpu_sys_ms=(\d+) gc_cpu_ms=(\d+)`)
	statsRe = regexp.MustCompile(`STATS queue_records=(\d+) queue_bytes=(\d+) quarantine_records=(\d+) appended=(\d+) acked=(\d+)`)
	errRe   = regexp.MustCompile(errLineRe)
)

func envIntDefault(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// row is one time-series sample.
type row struct {
	phase                                    string
	m                                        memSample
	cpuUser, cpuSys, gcCPU                   int // ms, cumulative, ingest process
	crawlerQ, crawlerQB, appended, acked     int
	crawlerRSS, crawlerCPU                   int // KiB, ms
	torrents, pgQueue, quarantine, drift     int
	queueRel, queueHeap, torrentsRel, dbSize int // bytes
	nLive, nDead, autovac, vac, autoanalyze  int
	sqliteDB, sqliteWAL                      int // bytes
	freeGiB                                  int
	wall                                     time.Time
}

func (r row) csv() string {
	return fmt.Sprintf("%s,%s,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d",
		r.wall.Format(time.RFC3339), r.phase, r.m.t, r.m.rss, r.m.anon, r.m.file, r.m.hwm,
		r.m.alloc, r.m.inuse, r.m.idle, r.m.released, r.m.sys, r.m.stack, r.m.live, r.m.goal,
		r.m.numGC, r.m.pauseMs, r.m.goroutines, r.cpuUser, r.cpuSys, r.gcCPU,
		r.crawlerQ, r.crawlerQB, r.appended, r.acked, r.crawlerRSS, r.crawlerCPU,
		r.torrents, r.pgQueue, r.quarantine, r.drift,
		r.queueRel, r.queueHeap, r.torrentsRel, r.dbSize, r.nLive, r.nDead, r.autovac, r.vac, r.autoanalyze,
		r.sqliteDB)
}

const csvHeader = "wall,phase,t,rss_kib,rss_anon_kib,rss_file_kib,hwm_kib,heap_alloc_kib,heap_inuse_kib,heap_idle_kib,heap_released_kib,sys_kib,stack_kib,live_kib,goal_kib," +
	"num_gc,pause_total_ms,goroutines,cpu_user_ms,cpu_sys_ms,gc_cpu_ms," +
	"crawler_queue,crawler_queue_bytes,appended,acked,crawler_rss_kib,crawler_cpu_ms," +
	"torrents,pg_queue,quarantine,usage_drift," +
	"queue_rel_bytes,queue_heap_bytes,torrents_rel_bytes,db_bytes,queue_live_tup,queue_dead_tup,queue_autovacuum,queue_vacuum,queue_autoanalyze," +
	"sqlite_db_bytes,sqlite_wal_bytes,free_gib\n"

func (r row) csvLine() string { return r.csv() + fmt.Sprintf(",%d,%d\n", r.sqliteWAL, r.freeGiB) }

// procCPUms returns utime+stime of a process in milliseconds (100 Hz ticks).
func procCPUms(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+2:]))
	if len(f) < 13 {
		return 0
	}
	u, _ := strconv.Atoi(f[11])
	st, _ := strconv.Atoi(f[12])
	return (u + st) * 10
}

func freeGiB(path string) int {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	return int(uint64(st.Bavail) * uint64(st.Bsize) >> 30)
}

func dirSizes(dir string) (db, wal int) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		switch {
		case strings.HasSuffix(e.Name(), "-wal"):
			wal += int(info.Size())
		case strings.HasSuffix(e.Name(), "-shm"):
		default:
			db += int(info.Size())
		}
	}
	return
}

func median(v []int) int {
	if len(v) == 0 {
		return 0
	}
	c := append([]int(nil), v...)
	sort.Ints(c)
	return c[len(c)/2]
}

// profileRun carries the state of one memory-profile run.
type profileRun struct {
	t                     *testing.T
	w                     *world
	dir                   string
	ing                   *proc
	pprof                 string
	crawler               *proc  // current crawler (nil between phases)
	qdir                  string // current crawler queue directory
	start                 time.Time
	rows                  []row
	csvf                  *os.File
	phase                 string
	steady                bool // growth/backlog stop conditions apply
	rate                  int
	rssStop               int
	minFree               int
	window                time.Duration
	backSince, stallSince time.Time
	lastTorrents          int
	driftBad              int
	stopped               bool
}

func (p *profileRun) prof(name string) {
	p.t.Helper()
	fetchTo(p.t, "http://"+p.pprof+"/debug/pprof/heap?gc=1", filepath.Join(p.dir, name+".heap.pb.gz"))
	fetchTo(p.t, "http://"+p.pprof+"/debug/pprof/allocs", filepath.Join(p.dir, name+".allocs.pb.gz"))
	fetchTo(p.t, "http://"+p.pprof+"/debug/pprof/goroutine?debug=1", filepath.Join(p.dir, name+".goroutines.txt"))
}

// sample collects one row, appends it to the CSV and evaluates the stop
// conditions.
func (p *profileRun) sample() row {
	p.t.Helper()
	r := row{phase: p.phase, wall: time.Now()}
	out := p.ing.output()
	if m, ok := lastMem(out); ok {
		r.m = m
	}
	if all := cpuRe.FindAllStringSubmatch(out, -1); len(all) > 0 {
		c := all[len(all)-1]
		r.cpuUser, _ = strconv.Atoi(c[1])
		r.cpuSys, _ = strconv.Atoi(c[2])
		r.gcCPU, _ = strconv.Atoi(c[3])
	}
	if p.crawler != nil {
		if all := statsRe.FindAllStringSubmatch(p.crawler.output(), -1); len(all) > 0 {
			c := all[len(all)-1]
			r.crawlerQ, _ = strconv.Atoi(c[1])
			r.crawlerQB, _ = strconv.Atoi(c[2])
			r.appended, _ = strconv.Atoi(c[4])
			r.acked, _ = strconv.Atoi(c[5])
		}
		r.crawlerRSS = rssKiB(p.crawler)
		r.crawlerCPU = procCPUms(p.crawler.cmd.Process.Pid)
	}
	r.sqliteDB, r.sqliteWAL = dirSizes(p.qdir)
	r.freeGiB = freeGiB(p.dir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := p.w.pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM magneticod.torrents),
       (SELECT count(*) FROM magnetico_ingest.queue),
       (SELECT count(*) FROM magnetico_ingest.quarantine),
       (SELECT used_bytes FROM magnetico_ingest.usage) - (SELECT COALESCE(sum(payload_bytes),0) FROM magnetico_ingest.queue) - (SELECT COALESCE(sum(payload_bytes),0) FROM magnetico_ingest.quarantine),
       pg_total_relation_size('magnetico_ingest.queue'), pg_relation_size('magnetico_ingest.queue'),
       pg_total_relation_size('magneticod.torrents'), pg_database_size(current_database()),
       COALESCE(s.n_live_tup,0), COALESCE(s.n_dead_tup,0), COALESCE(s.autovacuum_count,0), COALESCE(s.vacuum_count,0), COALESCE(s.autoanalyze_count,0)
  FROM (SELECT 1) x LEFT JOIN pg_stat_user_tables s ON s.relid = 'magnetico_ingest.queue'::regclass`).
		Scan(&r.torrents, &r.pgQueue, &r.quarantine, &r.drift, &r.queueRel, &r.queueHeap, &r.torrentsRel, &r.dbSize,
			&r.nLive, &r.nDead, &r.autovac, &r.vac, &r.autoanalyze)
	if err != nil {
		p.stop(r, "PostgreSQL statistics query failed: %v", err)
	}
	p.rows = append(p.rows, r)
	if _, err := p.csvf.WriteString(r.csvLine()); err != nil {
		p.t.Fatalf("timeseries: %v", err)
	}
	p.check(r)
	return r
}

// stop preserves diagnostics and ends the run. It never returns.
func (p *profileRun) stop(r row, format string, a ...any) {
	p.t.Helper()
	if !p.stopped {
		p.stopped = true
		msg := fmt.Sprintf(format, a...)
		_ = os.WriteFile(filepath.Join(p.dir, "EARLY-STOP.txt"), []byte(fmt.Sprintf("%s phase=%s t=%ds\n%s\n", time.Now().Format(time.RFC3339), p.phase, int(time.Since(p.start).Seconds()), msg)), 0o644)
		// Best-effort diagnostics; the loopback listener may be unreachable.
		func() {
			defer func() { _ = recover() }()
			c := &http.Client{Timeout: 20 * time.Second}
			for name, u := range map[string]string{
				"stop.heap.pb.gz": "/debug/pprof/heap?gc=1", "stop.goroutines.txt": "/debug/pprof/goroutine?debug=2"} {
				if resp, err := c.Get("http://" + p.pprof + u); err == nil {
					if f, err := os.Create(filepath.Join(p.dir, name)); err == nil {
						_, _ = io.Copy(f, resp.Body)
						f.Close()
					}
					resp.Body.Close()
				}
			}
		}()
	}
	p.t.Fatalf("EARLY STOP: "+format, a...)
}

func (p *profileRun) check(r row) {
	p.t.Helper()
	select {
	case <-p.ing.done:
		p.stop(r, "ingest process exited unexpectedly:\n%s", tail(p.ing.output()))
	default:
	}
	if r.m.anon>>10 > p.rssStop {
		p.stop(r, "ingest anonymous RSS %d MiB exceeds the %d MiB resource limit", r.m.anon>>10, p.rssStop)
	}
	if r.m.goroutines > goroutineStop {
		p.stop(r, "ingest goroutines %d exceed %d", r.m.goroutines, goroutineStop)
	}
	if r.freeGiB >= 0 && r.freeGiB < p.minFree {
		p.stop(r, "free disk space %d GiB below the %d GiB floor", r.freeGiB, p.minFree)
	}
	// Integrity.
	if r.quarantine > 0 {
		p.stop(r, "quarantine rows appeared (%d): the synthetic workload is always valid", r.quarantine)
	}
	if r.drift != 0 {
		p.driftBad++
		if p.driftBad >= 3 {
			p.stop(r, "usage accounting drifted by %d bytes in 3 consecutive samples", r.drift)
		}
	} else {
		p.driftBad = 0
	}
	for _, src := range []struct{ name, out string }{{"ingest", p.ing.output()}, {"crawler", func() string {
		if p.crawler == nil {
			return ""
		}
		return p.crawler.output()
	}()}} {
		for _, line := range strings.Split(src.out, "\n") {
			if line == "" || strings.HasPrefix(line, "MEM ") || strings.HasPrefix(line, "STATS ") || strings.HasPrefix(line, "DONE ") ||
				strings.Contains(line, "summary:") {
				continue
			}
			if errRe.MatchString(line) {
				p.stop(r, "%s logged an error-like line: %s", src.name, line)
			}
		}
	}
	if p.crawler != nil {
		select {
		case <-p.crawler.done:
			if code := p.crawler.cmd.ProcessState.ExitCode(); code != 0 {
				p.stop(r, "crawler exited %d:\n%s", code, tail(p.crawler.output()))
			}
		default:
		}
	}
	// Persistence progress with a backlog.
	if r.pgQueue > 0 && r.torrents == p.lastTorrents {
		if p.stallSince.IsZero() {
			p.stallSince = r.wall
		} else if r.wall.Sub(p.stallSince) > stallStopSec*time.Second {
			p.stop(r, "no persistence progress for %ds with %d rows queued", stallStopSec, r.pgQueue)
		}
	} else {
		p.stallSince = time.Time{}
	}
	p.lastTorrents = r.torrents

	if !p.steady {
		return
	}
	// Sustained backlog growth without recovery (steady phase).
	if r.crawlerQ > crawlerQueueLimit || r.pgQueue > pgBacklogLimit {
		if p.backSince.IsZero() {
			p.backSince = r.wall
		} else if r.wall.Sub(p.backSince) > backlogStopSec*time.Second {
			p.stop(r, "backlog above limit for %ds (crawler queue %d, PostgreSQL %d)", backlogStopSec, r.crawlerQ, r.pgQueue)
		}
	} else {
		p.backSince = time.Time{}
	}
	p.checkTrends(r)
}

// checkTrends compares medians of consecutive windows after warm-up: a live
// heap or anonymous RSS that grows in two consecutive windows, by more than 50 %
// and 64 MiB (live) / 25 % and 100 MiB (RSS), is sustained growth. GC CPU share
// is the cumulative GC/total CPU ratio of the last gcShareWindow.
func (p *profileRun) checkTrends(r row) {
	p.t.Helper()
	var steady []row
	for _, x := range p.rows {
		if x.phase == "steady" {
			steady = append(steady, x)
		}
	}
	if len(steady) == 0 {
		return
	}
	first := steady[0].wall
	winOf := func(x row) int { return int(x.wall.Sub(first) / p.window) }
	cur := winOf(r)
	if cur >= warmupWindows+3 {
		med := func(w int, f func(row) int) int {
			var v []int
			for _, x := range steady {
				if winOf(x) == w {
					v = append(v, f(x))
				}
			}
			return median(v)
		}
		// Only fully completed windows are compared: cur-1, cur-2, cur-3.
		for _, c := range []struct {
			name        string
			f           func(row) int
			pct, absKiB int
		}{
			{"live heap", func(x row) int { return x.m.live }, 50, 64 << 10},
			{"anonymous RSS", func(x row) int { return x.m.anon }, 25, 100 << 10},
		} {
			a, b, d := med(cur-3, c.f), med(cur-2, c.f), med(cur-1, c.f)
			grew := func(old, nw int) bool { return nw > old+c.absKiB && nw*100 > old*(100+c.pct) }
			if grew(a, b) && grew(b, d) {
				p.stop(r, "sustained %s growth over consecutive %v windows: %d -> %d -> %d MiB", c.name, p.window, a>>10, b>>10, d>>10)
			}
		}
	}
	var base *row
	for i := len(steady) - 1; i >= 0; i-- {
		if r.wall.Sub(steady[i].wall) >= gcShareWindow {
			base = &steady[i]
			break
		}
	}
	if base != nil {
		cpu := (r.cpuUser + r.cpuSys) - (base.cpuUser + base.cpuSys)
		gcc := r.gcCPU - base.gcCPU
		wallMs := int(r.wall.Sub(base.wall).Milliseconds())
		if cpu > wallMs/5 && gcc*100 > cpu*gcShareStopPct {
			p.stop(r, "GC consumed %d%% of ingest CPU over the last %v (%d of %d ms)", gcc*100/cpu, gcShareWindow, gcc, cpu)
		}
	}
}

// drained returns an until-condition that completes when want torrents are
// stored and the PostgreSQL queue is empty. It stops the run if that does not
// happen within drainTimeout, or if the queue is empty while records are
// missing for 60 s (lost records).
func (p *profileRun) drained(want int) func(row) bool {
	deadline := time.Now().Add(drainTimeout)
	var lostSince time.Time
	return func(r row) bool {
		if r.torrents == want && r.pgQueue == 0 {
			return true
		}
		if time.Now().After(deadline) {
			p.stop(r, "backlog did not drain within %v (%d torrents of %d, %d queued)", drainTimeout, r.torrents, want, r.pgQueue)
		}
		if r.pgQueue == 0 && r.torrents < want {
			if lostSince.IsZero() {
				lostSince = time.Now()
			} else if time.Since(lostSince) > 60*time.Second {
				p.stop(r, "records lost: queue empty with %d of %d torrents stored", r.torrents, want)
			}
		} else {
			lostSince = time.Time{}
		}
		return false
	}
}

func (p *profileRun) snapLabel(label string) memSample {
	p.t.Helper()
	time.Sleep(2500 * time.Millisecond) // one fresh MEM line
	r := p.sample()
	p.t.Logf("%-28s %s", label, r.m)
	return r.m
}

// runPhase samples every interval until done is closed (or the crawler exits).
func (p *profileRun) runPhase(name string, every time.Duration, until func(row) bool, extra func(row)) row {
	p.phase = name
	tk := time.NewTicker(every)
	defer tk.Stop()
	var last row
	for range tk.C {
		last = p.sample()
		if extra != nil {
			extra(last)
		}
		if until(last) {
			return last
		}
	}
	return last
}

func TestE2EIngestMemoryProfile(t *testing.T) {
	dir := os.Getenv("MAGNETICO_E2E_PROFILE_DIR")
	if dir == "" {
		t.Skip("set MAGNETICO_E2E_PROFILE_DIR to run the ingest memory investigation")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	secs := envIntDefault("MAGNETICO_E2E_PROFILE_SECONDS", 90)
	rate := envIntDefault("MAGNETICO_E2E_PROFILE_RATE", 40)
	burst := envIntDefault("MAGNETICO_E2E_PROFILE_BURST", 3000)
	total := rate * secs
	sampleEvery := time.Duration(envIntDefault("MAGNETICO_E2E_PROFILE_SAMPLE_SECONDS", 10)) * time.Second
	profEvery := time.Duration(envIntDefault("MAGNETICO_E2E_PROFILE_PROFILE_EVERY_MIN", 0)) * time.Minute

	w := newWorld(t)
	ingestd := buildIngestd(t)
	addr, pprofAddr := freePort(t), freePort(t)
	env := []string{
		"MAGNETICO_INGEST_DATABASE=" + w.dsn,
		"MAGNETICO_INGEST_LISTEN=" + addr,
		"INGESTD_PPROF=" + pprofAddr,
	}
	if extra := strings.Fields(os.Getenv("MAGNETICO_E2E_PROFILE_ENV")); len(extra) > 0 {
		env = append(env, extra...)
		t.Logf("ingestd extra environment: %v", extra)
	}
	ing := start(t, env, ingestd)
	p := &profileRun{t: t, w: w, dir: dir, ing: ing, pprof: pprofAddr, start: time.Now(), rate: rate,
		rssStop: envIntDefault("MAGNETICO_E2E_PROFILE_RSS_STOP_MIB", 1536),
		minFree: envIntDefault("MAGNETICO_E2E_PROFILE_MIN_FREE_GIB", 12),
		window:  time.Duration(envIntDefault("MAGNETICO_E2E_PROFILE_WINDOW_MIN", 20)) * time.Minute}
	var err error
	if p.csvf, err = os.Create(filepath.Join(dir, "timeseries.csv")); err != nil {
		t.Fatal(err)
	}
	defer p.csvf.Close()
	if _, err := p.csvf.WriteString(csvHeader); err != nil {
		t.Fatal(err)
	}
	// Logs are written after the ingest service has shut down so the file
	// includes its final summary; also on early stop and test failure.
	t.Cleanup(func() {
		ing.term()
		ing.wait(t, 40*time.Second)
		_ = os.WriteFile(filepath.Join(dir, "ingestd.log"), []byte(ing.output()), 0o644)
		if p.crawler != nil {
			_ = os.WriteFile(filepath.Join(dir, "crawlerd-last.log"), []byte(p.crawler.output()), 0o644)
		}
	})
	eventually(t, "ingestd ready", 60*time.Second, func() bool {
		resp, err := http.Get("http://" + pprofAddr + "/debug/pprof/")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil
	})

	p.phase = "idle"
	p.qdir = ""
	idle := p.snapLabel("idle after start")

	// Phase 1: the steady rate (measured size mix).
	cq := filepath.Join(dir, "crawler-queue-steady")
	cr := w.crawlerd(cq, "http://"+addr, "-count", strconv.Itoa(total), "-rate", strconv.Itoa(rate))
	p.crawler, p.qdir, p.steady = cr, cq, true
	t0 := time.Now()
	profiledStart, profiledMid := false, false
	nextProf := t0.Add(profEvery)
	half := t0.Add(time.Duration(secs/2) * time.Second)
	var peak = idle
	p.runPhase("steady", sampleEvery, func(r row) bool {
		select {
		case <-cr.done:
			return true
		default:
			return false
		}
	}, func(r row) {
		if r.m.rss > peak.rss {
			peak = r.m
		}
		now := time.Now()
		if !profiledStart && now.After(t0.Add(time.Minute)) {
			p.prof("steady-start")
			profiledStart = true
		}
		if !profiledMid && now.After(half) {
			p.prof("steady-mid")
			profiledMid = true
		}
		if profEvery > 0 && now.After(nextProf) {
			p.prof(fmt.Sprintf("steady-t%04dmin", int(now.Sub(t0).Minutes())))
			nextProf = now.Add(profEvery)
		}
	})
	steadyEnd := time.Now()
	if code := cr.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("crawler exit %d:\n%s", code, tail(cr.output()))
	}
	d1 := parseDone(t, cr.output())
	if d1.appended != total || d1.duplicate != 0 || d1.quarantined != 0 || d1.queue != 0 || d1.quarantine != 0 {
		p.stop(row{}, "steady crawler DONE not clean: %+v (want appended=%d)", d1, total)
	}
	_ = os.WriteFile(filepath.Join(dir, "crawlerd-steady.log"), []byte(cr.output()), 0o644)

	// Settle: the PostgreSQL backlog drains.
	p.steady = false
	p.crawler = nil
	settleStart := time.Now()
	p.runPhase("drain-steady", 2*time.Second, p.drained(total), nil)
	t.Logf("steady backlog drained %.1fs after the producer finished", time.Since(settleStart).Seconds())
	w.settle(total)
	afterLoad := p.snapLabel("after load (settled)")
	p.prof("after-load")

	// Phase 2: burst, as fast as the pipeline allows.
	bq := filepath.Join(dir, "crawler-queue-burst")
	burstStart := time.Now()
	cr2 := w.crawlerd(bq, "http://"+addr, "-from", "1000001", "-count", strconv.Itoa(burst))
	p.crawler, p.qdir = cr2, bq
	var crawlerDoneAt time.Time
	p.runPhase("burst", time.Second, func(r row) bool {
		select {
		case <-cr2.done:
			crawlerDoneAt = time.Now()
			return true
		default:
			return false
		}
	}, func(r row) {
		if r.m.rss > peak.rss {
			peak = r.m
		}
	})
	if code := cr2.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("burst crawler exit %d:\n%s", code, tail(cr2.output()))
	}
	d2 := parseDone(t, cr2.output())
	if d2.appended != burst || d2.duplicate != 0 || d2.quarantined != 0 || d2.queue != 0 || d2.quarantine != 0 {
		p.stop(row{}, "burst crawler DONE not clean: %+v (want appended=%d)", d2, burst)
	}
	_ = os.WriteFile(filepath.Join(dir, "crawlerd-burst.log"), []byte(cr2.output()), 0o644)
	p.crawler = nil
	last := p.runPhase("drain-burst", 2*time.Second, p.drained(total+burst), func(r row) {
		if r.m.rss > peak.rss {
			peak = r.m
		}
	})
	drainedAt := time.Now()
	w.settle(total + burst)
	afterBurst := p.snapLabel("after burst (settled)")
	p.prof("after-burst")

	// Final integrity: every hash stored exactly once, nothing quarantined.
	if n, d := w.n(`SELECT count(*) FROM magneticod.torrents`), w.n(`SELECT count(DISTINCT info_hash) FROM magneticod.torrents`); n != total+burst || d != n {
		t.Errorf("torrents %d, distinct %d, want %d", n, d, total+burst)
	}
	if q := w.pgQuar(); q != 0 {
		t.Errorf("quarantine rows: %d", q)
	}

	// Runtime-retained vs live: give unused heap back to the OS and see what
	// RSS remains.
	fetchTo(t, "http://"+pprofAddr+"/debug/freeos", filepath.Join(dir, "freeos.txt"))
	afterFree := p.snapLabel("after FreeOSMemory")

	t.Logf("SUMMARY idle_rss=%dMiB peak_rss=%dMiB after_load_rss=%dMiB after_burst_rss=%dMiB after_freeos_rss=%dMiB "+
		"after_freeos_live_heap=%dMiB peak_hwm=%dMiB",
		idle.rss>>10, peak.rss>>10, afterLoad.rss>>10, afterBurst.rss>>10, afterFree.rss>>10, afterFree.live>>10, afterFree.hwm>>10)
	summary := p.summarize(steadyEnd, t0, burstStart, crawlerDoneAt, drainedAt, total, burst, last)
	t.Log("\n" + summary)
	if err := os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(summary), 0o644); err != nil {
		t.Fatal(err)
	}
}

// summarize reduces the time series to the figures of the qualification table.
func (p *profileRun) summarize(steadyEnd, t0, burstStart, crawlerDone, drained time.Time, total, burst int, lastRow row) string {
	var b strings.Builder
	phaseRows := func(names ...string) (out []row) {
		for _, r := range p.rows {
			for _, n := range names {
				if r.phase == n {
					out = append(out, r)
				}
			}
		}
		return
	}
	mx := func(rs []row, f func(row) int) (m int) {
		for _, r := range rs {
			if v := f(r); v > m {
				m = v
			}
		}
		return
	}
	mean := func(rs []row, f func(row) int) int {
		if len(rs) == 0 {
			return 0
		}
		s := 0
		for _, r := range rs {
			s += f(r)
		}
		return s / len(rs)
	}
	pct := func(rs []row, f func(row) int, q int) int {
		v := make([]int, 0, len(rs))
		for _, r := range rs {
			v = append(v, f(r))
		}
		sort.Ints(v)
		if len(v) == 0 {
			return 0
		}
		return v[(len(v)-1)*q/100]
	}
	steady := phaseRows("steady")
	if len(steady) < 2 {
		return "no steady samples\n"
	}
	// Steady rates and CPU are measured between the first and last steady rows.
	// Rows taken after the producer exited (crawler gone) would dilute the
	// rates, so the window ends at the last row with a live crawler.
	f, l := steady[0], steady[len(steady)-1]
	for i := len(steady) - 1; i > 0; i-- {
		if steady[i].crawlerRSS > 0 {
			l = steady[i]
			break
		}
	}
	dt := l.wall.Sub(f.wall).Seconds()
	cpu := (l.cpuUser + l.cpuSys) - (f.cpuUser + f.cpuSys)
	fmt.Fprintf(&b, "STEADY duration=%.0fs target=%d/s\n", dt, p.rate)
	fmt.Fprintf(&b, "  accepted(acked)/s=%.1f persisted/s=%.1f\n", float64(l.acked-f.acked)/dt, float64(l.torrents-f.torrents)/dt)
	fmt.Fprintf(&b, "  rss MiB: mean=%d p50=%d p95=%d max=%d | anon max=%d | hwm=%d (at end)\n",
		mean(steady, func(r row) int { return r.m.rss })>>10, pct(steady, func(r row) int { return r.m.rss }, 50)>>10,
		pct(steady, func(r row) int { return r.m.rss }, 95)>>10, mx(steady, func(r row) int { return r.m.rss })>>10,
		mx(steady, func(r row) int { return r.m.anon })>>10, l.m.hwm>>10)
	fmt.Fprintf(&b, "  heap MiB: live mean=%d max=%d | alloc mean=%d max=%d | inuse mean=%d max=%d | released(end)=%d idle(end)=%d | goal max=%d\n",
		mean(steady, func(r row) int { return r.m.live })>>10, mx(steady, func(r row) int { return r.m.live })>>10,
		mean(steady, func(r row) int { return r.m.alloc })>>10, mx(steady, func(r row) int { return r.m.alloc })>>10,
		mean(steady, func(r row) int { return r.m.inuse })>>10, mx(steady, func(r row) int { return r.m.inuse })>>10,
		l.m.released>>10, l.m.idle>>10, mx(steady, func(r row) int { return r.m.goal })>>10)
	fmt.Fprintf(&b, "  gc: cycles=%d (%.2f/s) pause_total=%dms | goroutines max=%d\n",
		l.m.numGC-f.m.numGC, float64(l.m.numGC-f.m.numGC)/dt, l.m.pauseMs-f.m.pauseMs, mx(steady, func(r row) int { return r.m.goroutines }))
	gcc := l.gcCPU - f.gcCPU
	fmt.Fprintf(&b, "  ingest cpu: %.1f s total (%.1f%% of one core; user %.1f s, sys %.1f s) | gc cpu %.1f s = %.1f%% of ingest cpu\n",
		float64(cpu)/1000, float64(cpu)/10/dt, float64(l.cpuUser-f.cpuUser)/1000, float64(l.cpuSys-f.cpuSys)/1000, float64(gcc)/1000, pctf(gcc, cpu))
	fmt.Fprintf(&b, "  crawler: queue depth max=%d p95=%d | rss max=%d MiB | cpu %.1f s | sqlite db max=%d KiB wal max=%d KiB\n",
		mx(steady, func(r row) int { return r.crawlerQ }), pct(steady, func(r row) int { return r.crawlerQ }, 95),
		mx(steady, func(r row) int { return r.crawlerRSS })>>10, float64(l.crawlerCPU-f.crawlerCPU)/1000,
		mx(steady, func(r row) int { return r.sqliteDB })>>10, mx(steady, func(r row) int { return r.sqliteWAL })>>10)
	fmt.Fprintf(&b, "  postgres: backlog max=%d p95=%d | queue relation max=%d KiB (heap max=%d KiB) | dead tuples max=%d | autovacuum runs=%d (vacuum %d, autoanalyze %d)\n",
		mx(steady, func(r row) int { return r.pgQueue }), pct(steady, func(r row) int { return r.pgQueue }, 95),
		mx(steady, func(r row) int { return r.queueRel })>>10, mx(steady, func(r row) int { return r.queueHeap })>>10,
		mx(steady, func(r row) int { return r.nDead }), l.autovac-f.autovac, l.vac-f.vac, l.autoanalyze-f.autoanalyze)
	fmt.Fprintf(&b, "  postgres sizes at end of steady: torrents relation=%d MiB database=%d MiB\n", l.torrentsRel>>20, l.dbSize>>20)
	br := phaseRows("burst", "drain-burst")
	if len(br) > 0 {
		fmt.Fprintf(&b, "BURST records=%d producer_finished=%.1fs after start; backlog fully drained %.1fs after producer finished (%.1fs after start)\n",
			burst, crawlerDone.Sub(burstStart).Seconds(), drained.Sub(crawlerDone).Seconds(), drained.Sub(burstStart).Seconds())
		fmt.Fprintf(&b, "  rss max=%d MiB hwm=%d MiB | heap live max=%d alloc max=%d inuse max=%d MiB | crawler queue max=%d | pg backlog max=%d | queue relation max=%d KiB | dead max=%d\n",
			mx(br, func(r row) int { return r.m.rss })>>10, lastRow.m.hwm>>10, mx(br, func(r row) int { return r.m.live })>>10,
			mx(br, func(r row) int { return r.m.alloc })>>10, mx(br, func(r row) int { return r.m.inuse })>>10,
			mx(br, func(r row) int { return r.crawlerQ }), mx(br, func(r row) int { return r.pgQueue }),
			mx(br, func(r row) int { return r.queueRel })>>10, mx(br, func(r row) int { return r.nDead }))
		bf, bl := br[0], br[len(br)-1]
		fmt.Fprintf(&b, "  burst gc cycles=%d pause=%dms ingest cpu=%.1f s gc cpu=%.1f s\n", bl.m.numGC-bf.m.numGC, bl.m.pauseMs-bf.m.pauseMs,
			float64((bl.cpuUser+bl.cpuSys)-(bf.cpuUser+bf.cpuSys))/1000, float64(bl.gcCPU-bf.gcCPU)/1000)
	}
	fmt.Fprintf(&b, "END torrents=%d (want %d) quarantine=%d usage_drift=%d db=%d MiB queue relation=%d KiB dead=%d autovacuum=%d\n",
		lastRow.torrents, total+burst, lastRow.quarantine, lastRow.drift, lastRow.dbSize>>20, lastRow.queueRel>>10, lastRow.nDead, lastRow.autovac)
	return b.String()
}

func pctf(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) * 100 / float64(b)
}
