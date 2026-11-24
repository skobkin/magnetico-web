package app

// Explicit load and soak measurements of the completed W4 HTTP/cache path
// against the real binary (docs/wip/07E-web-search-details-report.md §Step 8).
// Nothing here runs in the ordinary suite: it needs MAGNETICO_BENCH_LARGE=1 and
// MAGNETICO_LOAD_DSN (the owner DSN of a disposable PostgreSQL database).
//
//	MAGNETICO_BENCH_LARGE=1 \
//	MAGNETICO_LOAD_DSN='postgres://postgres:…@127.0.0.1:55432/bench' \
//	MAGNETICO_LOAD_OUT=/home/herdr/w4bench/out/run.jsonl \
//	MAGNETICO_LOAD_SCENARIOS=cold,conc8,conc32 \
//	go test -run TestWebLoad -timeout 3h -v .
//
// Knobs: MAGNETICO_LOAD_{SCENARIOS,DURATION,CGROUP_MEM,CPU_PERCENT,GOMEMLIMIT,
// GOMAXPROCS,LABEL,SKIP_SEED}. The server runs as a child process (optionally
// inside a systemd user scope with a hard memory limit) so peak RSS, cgroup
// peak and OOM kills are the process's own, not the test's.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/benchkit"
	"github.com/skobkin/magnetico-web/internal/web/synth"
)

const loadPW = "load-test-password-1"

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- data -----------------------------------------------------------------------

type loadData struct {
	heavy    map[string]int // corpus name → torrent id
	pool     []int          // mixed pool ids (eviction churn)
	treePool []int          // tree-eligible torrents (full-tree endpoint)
	words    []string       // search words that exist
}

var heavySpecs = []string{"typical-250k", "longpath-250k", "flatmax-250k", "deep-250k", "wide-250k", "adversarial-50k", "longname-1500", "typical-10k", "typical-1k"}

const poolSize = 24

func seedTorrent(ctx context.Context, conn *pgx.Conn, name string, files []codec.File) (int, error) {
	h := sha256.Sum256([]byte(name))
	var total int64
	for _, f := range files {
		total += f.Size
	}
	var id int
	if err := conn.QueryRow(ctx, `INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
	    VALUES ($1,$2,$3,1700000000,$4) RETURNING id`, h[:20], name, total, len(files)).Scan(&id); err != nil {
		return 0, err
	}
	blob, _, err := codec.Encode(files, codec.ProfileCompact, codec.DefaultLimits())
	if err != nil {
		return 0, err
	}
	_, err = conn.Exec(ctx, `INSERT INTO magneticod.torrent_file_blobs (torrent_id, data) VALUES ($1,$2)`, id, blob)
	return id, err
}

// ensureLoadData creates the schemas, roles and synthetic torrents once.
func ensureLoadData(t *testing.T, bin, dsn string) (rtDSN, opDSN string, d loadData) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	var have *string
	_ = conn.QueryRow(ctx, `SELECT to_regclass('magneticod.torrents')::text`).Scan(&have)
	if have == nil {
		if out, code := runInit(t, bin, dsn, "schema-init"); code != 0 {
			t.Fatalf("schema-init: %d %s", code, out)
		}
	}
	var web *string
	_ = conn.QueryRow(ctx, `SELECT to_regclass('magnetico_web.users')::text`).Scan(&web)
	if web == nil {
		if out, code := runWebSchemaInit(t, bin, dsn, "web-schema-init"); code != 0 {
			t.Fatalf("web-schema-init: %d %s", code, out)
		}
	}
	// Roles with the documented grants.
	u, _ := url.Parse(dsn)
	var roleExists bool
	_ = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='load_rt')`).Scan(&roleExists)
	if !roleExists {
		for _, r := range []string{"load_rt", "load_op"} {
			if _, err := conn.Exec(ctx, "CREATE ROLE "+r+" LOGIN PASSWORD 'role-secret-pw'"); err != nil {
				t.Fatal(err)
			}
		}
		grants := strings.NewReplacer("magnetico_web_rt", "load_rt", "magnetico_web_op", "load_op").Replace(docsGrants(t))
		if _, err := conn.Exec(ctx, grants); err != nil {
			t.Fatalf("grants: %v", err)
		}
	}
	as := func(role string) string {
		ru := *u
		ru.User = url.UserPassword(role, "role-secret-pw")
		return ru.String()
	}
	rtDSN, opDSN = as("load_rt"), as("load_op")
	var users int
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM magnetico_web.users WHERE username='alice'`).Scan(&users)
	if users == 0 {
		for _, c := range []struct {
			stdin string
			args  []string
		}{{"", []string{"user", "add", "alice"}}, {loadPW + "\n", []string{"user", "set-password", "alice"}}} {
			if out, errOut, code := runWebAdminBinary(t, bin, opDSN, c.stdin, nil, c.args...); code != 0 {
				t.Fatalf("%v: %d %s%s", c.args, code, out, errOut)
			}
		}
	}

	d.heavy = map[string]int{}
	var seeded int
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM magneticod.torrents WHERE name LIKE 'load/%'`).Scan(&seeded)
	if seeded == 0 && os.Getenv("MAGNETICO_LOAD_SKIP_SEED") == "" {
		t.Log("seeding synthetic torrents (once)")
		byName := map[string]synth.Spec{}
		for _, s := range synth.Corpus() {
			byName[s.Name] = s
		}
		for _, n := range heavySpecs {
			s := byName[n]
			files := synth.Generate(s)
			if _, err := seedTorrent(ctx, conn, "load/"+n, files); err != nil {
				t.Fatal(err)
			}
		}
		base := synth.Spec{Name: "pool", Shape: synth.Typical, Files: 150_000, Seed: 77}
		for _, s := range synth.Torrents(base, poolSize) {
			if _, err := seedTorrent(ctx, conn, "load/"+s.Name, synth.Generate(s)); err != nil {
				t.Fatal(err)
			}
		}
	}
	var haveTree int
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM magneticod.torrents WHERE name LIKE 'load/tree#%'`).Scan(&haveTree)
	if haveTree == 0 && os.Getenv("MAGNETICO_LOAD_SKIP_SEED") == "" {
		// Full-tree workloads: torrents the backend serves as one JSON tree
		// (1,300–3,900 files each, under the 4,000-file tree limit), plus one hierarchy beyond the index limits.
		base := synth.Spec{Name: "tree", Shape: synth.Typical, Files: 2600, Seed: 88}
		for _, s := range synth.Torrents(base, 24) {
			if _, err := seedTorrent(ctx, conn, "load/"+s.Name, synth.Generate(s)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := seedTorrent(ctx, conn, "load/toodeep", []codec.File{{Path: strings.Repeat("d/", 300) + "f", Size: 1}, {Path: "x", Size: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	var filler int
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM magneticod.torrents WHERE name NOT LIKE 'load/%'`).Scan(&filler)
	if filler == 0 && os.Getenv("MAGNETICO_LOAD_SKIP_SEED") == "" {
		start := time.Now()
		// Search population: 300k small torrents without blobs (one file each).
		if _, err := conn.Exec(ctx, `INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
		    SELECT substr(decode(md5(i::text)||md5((i+7)::text),'hex'),1,20),
		           (ARRAY['ubuntu','debian','fedora','archive','movie','album','linux','release','source','data'])[1+i%10]
		           ||' '||(ARRAY['alpha','bravo','charlie','delta','echo','foxtrot','golf','hotel'])[1+i%8]||' '||md5(i::text),
		           (i*7919)%100000000000, 1600000000+i*13, 1
		      FROM generate_series(1::bigint,300000) AS i`); err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Exec(ctx, `ANALYZE magneticod.torrents`)
		t.Logf("seeded in %v", time.Since(start).Round(time.Second))
	}
	rows, err := conn.Query(ctx, `SELECT id, name FROM magneticod.torrents WHERE name LIKE 'load/%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int
		var name string
		_ = rows.Scan(&id, &name)
		n := strings.TrimPrefix(name, "load/")
		if strings.HasPrefix(n, "pool#") {
			d.pool = append(d.pool, id)
		} else if strings.HasPrefix(n, "tree#") {
			d.treePool = append(d.treePool, id)
		} else {
			d.heavy[n] = id
		}
	}
	rows.Close()
	d.words = []string{"ubuntu", "debian", "fedora", "archive", "movie", "album", "linux", "release", "source", "data", "alpha delta", "bravo", "charlie echo"}
	return
}

func runInit(t *testing.T, bin, dsn, sub string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, "migrate", sub)
	cmd.Env = webEnv("MAGNETICO_MIGRATE_DATABASE=" + dsn)
	var buf bytes.Buffer
	cmd.Stderr, cmd.Stdout = &buf, &buf
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return buf.String(), ee.ExitCode()
		}
		return err.Error(), 1
	}
	return buf.String(), 0
}

// ---- server under test --------------------------------------------------------------

type sample struct {
	T         float64 `json:"t"`
	RSSMiB    float64 `json:"rss_mib"`
	CgroupMiB float64 `json:"cgroup_mib,omitempty"`
	FDs       int     `json:"fds"`
	Threads   int     `json:"threads"`
	PGConns   int     `json:"pg_conns"`
}

type serverRun struct {
	w        *runningWeb
	pid      int
	cgroup   string // /sys/fs/cgroup/... of the scope, "" without one
	scope    string
	start    time.Time
	samples  []sample
	stopSamp chan struct{}
	wg       sync.WaitGroup
	maxRSS   atomic.Uint64
	maxCg    atomic.Uint64
}

func readStatusOf(pid int) (rss, hwm uint64, threads int) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "VmRSS:":
			rss = v << 10
		case "VmHWM:":
			hwm = v << 10
		case "Threads:":
			threads = int(v)
		}
	}
	return
}

func countFDs(pid int) int {
	e, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return -1
	}
	return len(e)
}

func readUint(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v
}

func cgroupEvents(cg string) map[string]uint64 {
	out := map[string]uint64{}
	if cg == "" {
		return out
	}
	b, _ := os.ReadFile(cg + "/memory.events")
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 {
			out[f[0]], _ = strconv.ParseUint(f[1], 10, 64)
		}
	}
	return out
}

func pgConns(dsn string) int {
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		return -1
	}
	defer c.Close(context.Background())
	var n int
	_ = c.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE usename='load_rt'`).Scan(&n)
	return n
}

var scopeSeq atomic.Int64

// startServer launches the binary, optionally inside a memory-limited user scope.
func startServer(t *testing.T, bin, rtDSN, adminDSN string, extra []string) *serverRun {
	t.Helper()
	addr := freeLoopbackAddr(t)
	env := webEnv(append([]string{"MAGNETICO_WEB_DATABASE=" + rtDSN, "MAGNETICO_WEB_LISTEN=" + addr,
		"MAGNETICO_WEB_PUBLIC_URL=http://" + addr, "GODEBUG=gctrace=1"}, extra...)...)
	args := []string{bin, "web"}
	sr := &serverRun{}
	if mem := os.Getenv("MAGNETICO_LOAD_CGROUP_MEM"); mem != "" {
		sr.scope = fmt.Sprintf("magnetico-w4-bench-%d-%d", os.Getpid(), scopeSeq.Add(1))
		sd := []string{"systemd-run", "--user", "--scope", "--quiet", "--collect", "--unit=" + sr.scope,
			"-p", "MemoryMax=" + mem, "-p", "MemorySwapMax=0"}
		if cpu := os.Getenv("MAGNETICO_LOAD_CPU_PERCENT"); cpu != "" {
			sd = append(sd, "-p", "CPUQuota="+cpu+"%")
		}
		args = append(sd, args...)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = env
	out := &syncBuf{}
	cmd.Stderr, cmd.Stdout = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := &runningWeb{addr: addr, cmd: cmd, exited: make(chan error, 1), out: out}
	go func() { w.exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	sr.w, sr.pid, sr.start = w, cmd.Process.Pid, time.Now()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if resp, err := http.Get("http://" + addr + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		select {
		case err := <-w.exited:
			t.Fatalf("server exited early: %v\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sr.scope != "" {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", sr.pid))
		sr.cgroup = "/sys/fs/cgroup" + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "0::"))
	}
	sr.stopSamp = make(chan struct{})
	sr.wg.Add(1)
	go func() {
		defer sr.wg.Done()
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-sr.stopSamp:
				return
			case <-tk.C:
				rss, _, th := readStatusOf(sr.pid)
				s := sample{T: time.Since(sr.start).Seconds(), RSSMiB: float64(rss) / (1 << 20), FDs: countFDs(sr.pid), Threads: th, PGConns: pgConns(adminDSN)}
				if sr.cgroup != "" {
					cur := readUint(sr.cgroup + "/memory.current")
					s.CgroupMiB = float64(cur) / (1 << 20)
					for {
						old := sr.maxCg.Load()
						if cur <= old || sr.maxCg.CompareAndSwap(old, cur) {
							break
						}
					}
				}
				for {
					old := sr.maxRSS.Load()
					if rss <= old || sr.maxRSS.CompareAndSwap(old, rss) {
						break
					}
				}
				sr.samples = append(sr.samples, s)
			}
		}
	}()
	return sr
}

type serverReport struct {
	PeakRSSMiB, HWMMiB, CgroupPeakMiB float64
	OOMKills, OOMEvents, High         uint64
	GCs                               int
	GCPauseMs, MaxHeapMiB             float64
	ExitCode                          int
	ShutdownMs                        int64
	CacheLine                         string
	FDsStart, FDsEnd, PGStart, PGEnd  int
	ThreadsMax                        int
	Series                            []sample
}

var gcRe = regexp.MustCompile(`gc \d+ @[\d.]+s \d+%: ([\d.]+)\+([\d.]+)\+([\d.]+) ms clock, .*?, (\d+)->(\d+)->(\d+) MB`)

// finish stops sampling, SIGTERMs the server and collects its report.
func (sr *serverRun) finish(t *testing.T) serverReport {
	t.Helper()
	close(sr.stopSamp)
	sr.wg.Wait()
	rep := serverReport{}
	_, hwm, _ := readStatusOf(sr.pid)
	rep.HWMMiB = float64(hwm) / (1 << 20)
	rep.PeakRSSMiB = float64(sr.maxRSS.Load()) / (1 << 20)
	if sr.cgroup != "" {
		rep.CgroupPeakMiB = float64(readUint(sr.cgroup+"/memory.peak")) / (1 << 20)
		ev := cgroupEvents(sr.cgroup)
		rep.OOMKills, rep.OOMEvents, rep.High = ev["oom_kill"], ev["oom"], ev["high"]
	}
	if n := len(sr.samples); n > 0 {
		rep.FDsStart, rep.FDsEnd = sr.samples[0].FDs, sr.samples[n-1].FDs
		rep.PGStart, rep.PGEnd = sr.samples[0].PGConns, sr.samples[n-1].PGConns
		for _, s := range sr.samples {
			rep.ThreadsMax = max(rep.ThreadsMax, s.Threads)
		}
		step := max(1, n/240)
		for i := 0; i < n; i += step {
			rep.Series = append(rep.Series, sr.samples[i])
		}
	}
	stopAt := time.Now()
	_ = sr.w.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-sr.w.exited:
		if ee, ok := err.(*exec.ExitError); ok {
			rep.ExitCode = ee.ExitCode()
		} else if err != nil {
			rep.ExitCode = -1
		}
	case <-time.After(60 * time.Second):
		rep.ExitCode = -2
		_ = sr.w.cmd.Process.Kill()
	}
	rep.ShutdownMs = time.Since(stopAt).Milliseconds()
	for _, l := range strings.Split(sr.w.out.String(), "\n") {
		if m := gcRe.FindStringSubmatch(l); m != nil {
			rep.GCs++
			a, _ := strconv.ParseFloat(m[1], 64)
			c, _ := strconv.ParseFloat(m[3], 64)
			rep.GCPauseMs += a + c
			h, _ := strconv.ParseFloat(m[4], 64)
			rep.MaxHeapMiB = max(rep.MaxHeapMiB, h)
		}
		if strings.Contains(l, "file index cache stopped") {
			rep.CacheLine = l
		}
	}
	return rep
}

// ---- load driver ----------------------------------------------------------------------

type classResult struct {
	N      int              `json:"n"`
	Codes  map[string]int   `json:"codes"`
	Errors int              `json:"transport_errors"`
	OK     benchkit.Summary `json:"ok_latency"`
	All    benchkit.Summary `json:"all_latency"`
	PerSec float64          `json:"req_per_sec"`
}

type classAcc struct {
	mu    sync.Mutex
	ok    benchkit.Hist
	all   benchkit.Hist
	codes map[string]int
	errs  int
}

func newAcc() *classAcc { return &classAcc{codes: map[string]int{}} }

func (a *classAcc) add(d time.Duration, code int, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.all.Add(d)
	if err != nil {
		a.errs++
		return
	}
	a.codes[strconv.Itoa(code)]++
	if code == 200 {
		a.ok.Add(d)
	}
}

func (a *classAcc) result(wall time.Duration) classResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.errs
	for _, v := range a.codes {
		n += v
	}
	return classResult{N: n, Codes: a.codes, Errors: a.errs, OK: a.ok.Summary(), All: a.all.Summary(), PerSec: float64(n) / wall.Seconds()}
}

type driver struct {
	base   string
	cookie string
	hc     *http.Client
}

func newDriver(t *testing.T, addr string) *driver {
	t.Helper()
	hc := &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 128, MaxConnsPerHost: 0},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	d := &driver{base: "http://" + addr, hc: hc}
	d.cookie = d.login(t)
	return d
}

func (d *driver) login(t *testing.T) string {
	t.Helper()
	b := &webBrowser{t: t, base: d.base, c: &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	b.c.Jar, _ = newJar()
	if resp := b.login("alice", loadPW); resp.StatusCode != 303 {
		t.Fatalf("login: %d %s", resp.StatusCode, b.body)
	}
	u, _ := url.Parse(d.base)
	var parts []string
	for _, c := range b.c.Jar.Cookies(u) {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// get issues one request, optionally abandoning it after cancelAfter.
func (d *driver) get(path string, cancelAfter time.Duration) (code int, body []byte, dur time.Duration, err error) {
	ctx := context.Background()
	if cancelAfter > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cancelAfter)
		defer cancel()
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", d.base+path, nil)
	req.Header.Set("Cookie", d.cookie)
	t0 := time.Now()
	resp, err := d.hc.Do(req)
	if err != nil {
		return 0, nil, time.Since(t0), err
	}
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, body, time.Since(t0), err
}

// crawlDirs collects directory-page URLs up to a limit (breadth first),
// following the positional "path" of every directory entry.
func (d *driver) crawlDirs(t *testing.T, id int, limit int) []string {
	t.Helper()
	root := fmt.Sprintf("/torrents/%d/files", id)
	queue := []string{root}
	var all []string
	for len(queue) > 0 && len(all) < limit {
		p := queue[0]
		queue = queue[1:]
		code, body, _, err := d.get(p, 0)
		if err != nil || code != 200 {
			t.Fatalf("crawl %s: %d %v", p, code, err)
		}
		all = append(all, p)
		var page struct {
			Entries []struct {
				Type string `json:"type"`
				Path string `json:"path"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("crawl %s: %v", p, err)
		}
		for _, e := range page.Entries {
			if e.Type == "dir" {
				queue = append(queue, root+"?dir="+e.Path)
			}
		}
	}
	return all
}

type workload struct {
	name    string
	workers int
	pick    func(rng *rand.Rand) string
	cancel  func(rng *rand.Rand) time.Duration // optional
	acc     *classAcc
}

// drive runs the workloads concurrently for dur and returns per-class results.
func (d *driver) drive(dur time.Duration, ws []workload) map[string]classResult {
	stop := time.Now().Add(dur)
	var wg sync.WaitGroup
	start := time.Now()
	for _, w := range ws {
		for i := 0; i < w.workers; i++ {
			wg.Add(1)
			go func(w workload, seed uint64) {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(seed, 99))
				for time.Now().Before(stop) {
					var ca time.Duration
					if w.cancel != nil {
						ca = w.cancel(rng)
					}
					code, _, dur, err := d.get(w.pick(rng), ca)
					if ca > 0 && err != nil {
						w.acc.mu.Lock()
						w.acc.codes["client-abandoned"]++
						w.acc.mu.Unlock()
						continue
					}
					w.acc.add(dur, code, err)
				}
			}(w, uint64(i)+1)
		}
	}
	wg.Wait()
	wall := time.Since(start)
	out := map[string]classResult{}
	for _, w := range ws {
		out[w.name] = w.acc.result(wall)
	}
	return out
}

func newJar() (http.CookieJar, error) { return cookiejar.New(nil) }

// ---- the test -------------------------------------------------------------------------

type row struct {
	Scenario string            `json:"scenario"`
	Label    string            `json:"label"`
	Params   map[string]any    `json:"params"`
	Results  any               `json:"results"`
	Server   serverReport      `json:"server"`
	Machine  string            `json:"machine"`
	Env      map[string]string `json:"env"`
	When     string            `json:"when"`
}

func writeRow(t *testing.T, r row) {
	t.Helper()
	r.Machine = benchkit.Describe().String()
	r.When = time.Now().UTC().Format(time.RFC3339)
	r.Label = os.Getenv("MAGNETICO_LOAD_LABEL")
	r.Env = map[string]string{}
	for _, k := range []string{"MAGNETICO_LOAD_CGROUP_MEM", "MAGNETICO_LOAD_CPU_PERCENT", "MAGNETICO_LOAD_GOMEMLIMIT", "MAGNETICO_LOAD_GOMAXPROCS", "MAGNETICO_LOAD_DURATION"} {
		r.Env[k] = os.Getenv(k)
	}
	b, _ := json.Marshal(r)
	t.Logf("%s", b)
	if out := os.Getenv("MAGNETICO_LOAD_OUT"); out != "" {
		f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		_, _ = f.Write(append(b, '\n'))
	}
}

func serverEnv() []string {
	gmp := envOr("MAGNETICO_LOAD_GOMAXPROCS", "2")
	e := []string{"GOMAXPROCS=" + gmp}
	if v := os.Getenv("MAGNETICO_LOAD_GOMEMLIMIT"); v != "" {
		e = append(e, "GOMEMLIMIT="+v)
	}
	return e
}

func TestWebLoad(t *testing.T) {
	dsn := os.Getenv("MAGNETICO_LOAD_DSN")
	if !benchkit.Enabled() || dsn == "" {
		t.Skip("explicit load test: set MAGNETICO_BENCH_LARGE=1 and MAGNETICO_LOAD_DSN")
	}
	dur, _ := time.ParseDuration(envOr("MAGNETICO_LOAD_DURATION", "20s"))
	want := map[string]bool{}
	for _, s := range strings.Split(envOr("MAGNETICO_LOAD_SCENARIOS", "cold,warm,conc8,conc32,mixed32,cancel"), ",") {
		want[strings.TrimSpace(s)] = true
	}
	bin := buildBinary(t)
	rtDSN, _, data := ensureLoadData(t, bin, dsn)

	run := func(name string, fn func(t *testing.T, sr *serverRun, d *driver) (any, map[string]any)) {
		if !want[name] {
			return
		}
		t.Run(name, func(t *testing.T) {
			sr := startServer(t, bin, rtDSN, dsn, serverEnv())
			d := newDriver(t, sr.w.addr)
			res, params := fn(t, sr, d)
			rep := sr.finish(t)
			writeRow(t, row{Scenario: name, Params: params, Results: res, Server: rep})
		})
	}

	typical := data.heavy["typical-250k"]

	run("cold", func(t *testing.T, sr *serverRun, d *driver) (any, map[string]any) {
		type cold struct {
			Dataset        string `json:"dataset"`
			ColdMs, WarmMs float64
			Code           int
			RSSAfterMiB    float64
		}
		var out []cold
		for _, n := range heavySpecs {
			id := data.heavy[n]
			p := fmt.Sprintf("/torrents/%d/files", id)
			code, _, d1, _ := d.get(p, 0)
			rss, _, _ := readStatusOf(sr.pid)
			_, _, d2, _ := d.get(p, 0)
			out = append(out, cold{n, float64(d1.Microseconds()) / 1000, float64(d2.Microseconds()) / 1000, code, float64(rss) / (1 << 20)})
		}
		return out, nil
	})

	run("warm", func(t *testing.T, sr *serverRun, d *driver) (any, map[string]any) {
		dirs := d.crawlDirs(t, typical, 300)
		acc := newAcc()
		res := d.drive(dur, []workload{{name: "nav", workers: 1, acc: acc, pick: func(r *rand.Rand) string { return dirs[r.IntN(len(dirs))] }}})
		return res, map[string]any{"dirs": len(dirs), "duration": dur.String()}
	})

	poolPick := func(dirs []string) func(*rand.Rand) string {
		return func(r *rand.Rand) string {
			switch x := r.IntN(100); {
			case x < 60: // warm navigation in the hot torrent
				return dirs[r.IntN(len(dirs))]
			case x < 95: // pool torrent root (cold/warm mix with eviction churn)
				return fmt.Sprintf("/torrents/%d/files", data.pool[r.IntN(len(data.pool))])
			default: // heavy shapes
				return fmt.Sprintf("/torrents/%d/files", data.heavy[heavySpecs[r.IntN(len(heavySpecs))]])
			}
		}
	}
	searchPick := func(r *rand.Rand) string {
		return "/torrents?q=" + url.QueryEscape(data.words[r.IntN(len(data.words))]) + "&sort=" + []string{"discovered", "size", "name"}[r.IntN(3)]
	}

	for _, n := range []int{8, 32} {
		name := fmt.Sprintf("conc%d", n)
		run(name, func(t *testing.T, sr *serverRun, d *driver) (any, map[string]any) {
			dirs := d.crawlDirs(t, typical, 300)
			res := d.drive(dur, []workload{{name: "files", workers: n, acc: newAcc(), pick: poolPick(dirs)}})
			return res, map[string]any{"clients": n, "duration": dur.String(), "pool": len(data.pool)}
		})
	}

	run("mixed32", func(t *testing.T, sr *serverRun, d *driver) (any, map[string]any) {
		dirs := d.crawlDirs(t, typical, 300)
		loginAcc := newAcc()
		stopLogin := make(chan struct{})
		var lw sync.WaitGroup
		lw.Add(1)
		go func() { // login responsiveness while the rest runs
			defer lw.Done()
			for {
				select {
				case <-stopLogin:
					return
				default:
				}
				t0 := time.Now()
				b := &webBrowser{t: t, base: d.base, c: &http.Client{Timeout: 30 * time.Second,
					CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
				b.c.Jar, _ = newJar()
				req, _ := http.NewRequest("GET", d.base+"/login", nil)
				resp, err := b.c.Do(req)
				if err != nil {
					loginAcc.add(time.Since(t0), 0, err)
					continue
				}
				raw, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				m := binTokenRe.FindSubmatch(raw)
				if m == nil {
					loginAcc.add(time.Since(t0), resp.StatusCode, nil)
					continue
				}
				form := url.Values{"username": {"alice"}, "password": {loadPW}, "csrf_token": {string(m[1])}}
				req2, _ := http.NewRequest("POST", d.base+"/login", strings.NewReader(form.Encode()))
				req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				resp2, err := b.c.Do(req2)
				if err != nil {
					loginAcc.add(time.Since(t0), 0, err)
					continue
				}
				resp2.Body.Close()
				code := resp2.StatusCode
				if code == 303 {
					code = 200
				}
				loginAcc.add(time.Since(t0), code, nil)
				time.Sleep(200 * time.Millisecond)
			}
		}()
		res := d.drive(dur, []workload{
			{name: "files", workers: 32, acc: newAcc(), pick: poolPick(dirs)},
			{name: "search", workers: 4, acc: newAcc(), pick: searchPick},
		})
		close(stopLogin)
		lw.Wait()
		res["login"] = loginAcc.result(dur)
		return res, map[string]any{"file_clients": 32, "search_clients": 4, "duration": dur.String()}
	})

	// Full-tree (W5) scenarios: one JSON document per request, serialized
	// under the encode gate; 200 for eligible torrents.
	treePick := func(r *rand.Rand) string {
		return fmt.Sprintf("/torrents/%d/files/tree", data.treePool[r.IntN(len(data.treePool))])
	}
	for _, n := range []int{8, 32} {
		run(fmt.Sprintf("tree%d", n), func(t *testing.T, sr *serverRun, d *driver) (any, map[string]any) {
			res := d.drive(dur, []workload{{name: "tree", workers: n, acc: newAcc(), pick: treePick}})
			return res, map[string]any{"clients": n, "duration": dur.String(), "tree_pool": len(data.treePool)}
		})
	}
	// Tree serialization concurrent with cold index builds and eviction churn
	// (pool torrents of ~150k files) and search.
	run("treemix", func(t *testing.T, sr *serverRun, d *driver) (any, map[string]any) {
		dirs := d.crawlDirs(t, typical, 300)
		res := d.drive(dur, []workload{
			{name: "tree", workers: 16, acc: newAcc(), pick: treePick},
			{name: "dirs", workers: 16, acc: newAcc(), pick: poolPick(dirs)},
			{name: "search", workers: 2, acc: newAcc(), pick: searchPick},
		})
		return res, map[string]any{"tree_clients": 16, "dir_clients": 16, "duration": dur.String()}
	})
	// Details page latency with and without the mode-decision header peek:
	// tree-pool torrents (peek: ≤ 4,000 files) versus a 250,000-file torrent
	// (decided from the file count alone, no blob access).
	run("details", func(t *testing.T, sr *serverRun, d *driver) (any, map[string]any) {
		res := d.drive(dur/2, []workload{{name: "peek", workers: 1, acc: newAcc(), pick: func(r *rand.Rand) string {
			return fmt.Sprintf("/torrents/%d", data.treePool[r.IntN(len(data.treePool))])
		}}})
		more := d.drive(dur/2, []workload{{name: "nopeek", workers: 1, acc: newAcc(), pick: func(r *rand.Rand) string {
			return fmt.Sprintf("/torrents/%d", typical)
		}}})
		for k, v := range more {
			res[k] = v
		}
		return res, map[string]any{"duration": dur.String()}
	})
	// A hierarchy beyond the index limits answers 422 quickly and does not
	// disturb the other classes.
	run("complex", func(t *testing.T, sr *serverRun, d *driver) (any, map[string]any) {
		deep := data.heavy["toodeep"]
		res := d.drive(dur, []workload{
			{name: "toodeep", workers: 8, acc: newAcc(), pick: func(r *rand.Rand) string {
				return []string{fmt.Sprintf("/torrents/%d/files/tree", deep), fmt.Sprintf("/torrents/%d/files", deep)}[r.IntN(2)]
			}},
			{name: "tree", workers: 8, acc: newAcc(), pick: treePick},
		})
		return res, map[string]any{"duration": dur.String()}
	})

	run("cancel", func(t *testing.T, sr *serverRun, d *driver) (any, map[string]any) {
		dirs := d.crawlDirs(t, typical, 100)
		res := d.drive(dur, []workload{
			{name: "abandoning", workers: 16, acc: newAcc(), pick: poolPick(dirs),
				cancel: func(r *rand.Rand) time.Duration { return time.Duration(1+r.IntN(80)) * time.Millisecond }},
			{name: "steady", workers: 4, acc: newAcc(), pick: poolPick(dirs)},
		})
		time.Sleep(3 * time.Second) // quiesce: decodes that outlived their requesters finish
		return res, map[string]any{"duration": dur.String()}
	})

}
