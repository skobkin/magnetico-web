//go:build loadtest

package cli

// Load steps and soak. Gated like the other heavy
// harnesses: nothing here runs in the ordinary suite.
//
//	MAGNETICO_BENCH_LARGE=1
//	MAGNETICO_BENCH_DSN       owner DSN of the seeded dataset database (TestSeedLargeDataset)
//	MAGNETICO_BENCH_CONFIRM_DISPOSABLE_DB  the database name from the DSN: confirms it is disposable
//	MAGNETICO_BENCH_OUT       output directory for the JSONL results (created)
//	MAGNETICO_BENCH_PROFILE   "steps" | "soak" | "memfloor" | "knee" | "diag" | "regress"   (what to run)
//
// Optional: MAGNETICO_BENCH_LABEL, MAGNETICO_BENCH_WEB_MEM (cgroup MemoryMax, default
// 1G), MAGNETICO_BENCH_WEB_GOMEMLIMIT (default 768MiB), MAGNETICO_BENCH_STEP_SECONDS
// (default 180), MAGNETICO_BENCH_SOAK_SCALE (multiplies the 30/90/20 minute soak
// phases; 1 by default), MAGNETICO_BENCH_TAGS (build tags, "diag" for the
// diagnostic run).
//
// Isolation: the web service and the ingest service each run in their own
// systemd user scope with their own memory and CPU limits (the web-specific
// memory figure therefore comes from the web scope alone); PostgreSQL and this
// generator are outside both. Every request carries a valid credential through a
// pool large enough that the per-token limits cannot dominate; outcomes are
// classified, never merged, and a run in which 429s exceed 0.1% is invalid.
// Credentials live in memory only: nothing sensitive is written to the output.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/skobkin/magnetico-web/internal/ingest/record"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/testkit"
	"github.com/skobkin/magnetico-web/internal/web/benchkit"
	"github.com/skobkin/magnetico-web/persistence"
)

const (
	harnessTokensPerUser  = 10
	harnessLoadPassword   = "w9-load-password-0001"
	harnessIngestRate     = 20 // records per second during the steady phase
	harnessDrainDeadline  = 10 * time.Minute
	harnessSampleInterval = 10 * time.Second
	harnessQueryTimeout   = 5 * time.Second  // one monitoring query
	harnessVerifyTimeout  = 10 * time.Minute // the post-drain storage verification, as a whole
)

type harnessScoped struct {
	name   string
	cmd    *exec.Cmd
	out    *testkit.SyncBuffer
	exited chan error
	pid    int
	cg     string
	start  time.Time
}

var harnessScopeSeq atomic.Int64

// startScoped runs bin inside its own memory- and CPU-limited systemd user scope.
func startScoped(t *testing.T, name, mem string, cpuPercent int, env []string, bin string, args ...string) *harnessScoped {
	t.Helper()
	unit := fmt.Sprintf("magnetico-w9-%s-%d-%d", name, os.Getpid(), harnessScopeSeq.Add(1))
	sd := []string{"systemd-run", "--user", "--scope", "--quiet", "--collect", "--unit=" + unit,
		"-p", "MemoryMax=" + mem, "-p", "MemorySwapMax=0", "-p", "CPUQuota=" + strconv.Itoa(cpuPercent) + "%", bin}
	cmd := exec.Command(sd[0], append(sd[1:], args...)...)
	cmd.Env = env
	out := &testkit.SyncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &harnessScoped{name: name, cmd: cmd, out: out, exited: make(chan error, 1), pid: cmd.Process.Pid, start: time.Now()}
	go func() { s.exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	// The scope's cgroup directory.
	for i := 0; i < 100 && s.cg == ""; i++ {
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", s.pid)); err == nil && strings.Contains(string(b), unit) {
			s.cg = "/sys/fs/cgroup" + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "0::"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s.cg == "" {
		t.Fatalf("no cgroup for %s", unit)
	}
	return s
}

// stop sends SIGTERM and returns the exit code (-1 on timeout).
func (s *harnessScoped) stop(within time.Duration) int {
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-s.exited:
		if err == nil {
			return 0
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return -2
	case <-time.After(within):
		_ = s.cmd.Process.Kill()
		return -1
	}
}

// ---- sampling -----------------------------------------------------------------------

type harnessSample struct {
	T         float64 `json:"t"`
	Proc      string  `json:"proc"`
	RSSMiB    float64 `json:"rss_mib"`
	CgroupMiB float64 `json:"cgroup_mib"`
	CPUSec    float64 `json:"cpu_sec"`
	Threads   int     `json:"threads"`
	FDs       int     `json:"fds"`
}

func cgCPUSeconds(cg string) float64 {
	b, _ := os.ReadFile(cg + "/cpu.stat")
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "usage_usec" {
			v, _ := strconv.ParseFloat(f[1], 64)
			return v / 1e6
		}
	}
	return 0
}

type harnessSampler struct {
	start   time.Time
	mu      sync.Mutex
	samples []harnessSample
	queue   []harnessPoint // ingest queue depth
	// qTries counts the queue samples attempted; qFails those that produced no
	// reading (no connection, a failed or timed-out query). Both are reported.
	qTries, qFails int
	stop           chan struct{}
	wg             sync.WaitGroup
}

type harnessPoint struct {
	T float64 `json:"t"`
	V float64 `json:"v"`
}

func startSampler(procs []*harnessScoped, dsn string) *harnessSampler {
	s := &harnessSampler{start: time.Now(), stop: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var conn *pgx.Conn
		defer func() {
			if conn != nil {
				conn.Close(context.Background())
			}
		}()
		tk := time.NewTicker(harnessSampleInterval)
		defer tk.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-tk.C:
			}
			now := time.Since(s.start).Seconds()
			s.mu.Lock()
			for _, p := range procs {
				rss, _, th := readStatusOf(p.pid)
				s.samples = append(s.samples, harnessSample{T: now, Proc: p.name, RSSMiB: float64(rss) / (1 << 20),
					CgroupMiB: float64(readUint(p.cg+"/memory.current")) / (1 << 20), CPUSec: cgCPUSeconds(p.cg), Threads: th, FDs: countFDs(p.pid)})
			}
			s.mu.Unlock()
			s.mu.Lock()
			s.qTries++
			s.mu.Unlock()
			qctx, cancel := context.WithTimeout(context.Background(), harnessQueryTimeout)
			if conn == nil {
				conn, _ = pgx.Connect(qctx, dsn)
			}
			got := false
			if conn != nil {
				var n int
				// The queue table is bounded by the admission quota: a cheap count.
				if err := conn.QueryRow(qctx, `SELECT count(*) FROM magnetico_ingest.queue`).Scan(&n); err == nil {
					s.mu.Lock()
					s.queue = append(s.queue, harnessPoint{now, float64(n)})
					s.mu.Unlock()
					got = true
				} else {
					conn.Close(context.Background())
					conn = nil
				}
			}
			cancel()
			if !got {
				s.mu.Lock()
				s.qFails++
				s.mu.Unlock()
			}
		}
	}()
	return s
}

func (s *harnessSampler) finish() {
	close(s.stop)
	s.wg.Wait()
}

// ---- identities and requests --------------------------------------------------------

type harnessProducer struct {
	addr, token string
	hc          *http.Client
	sent, acked atomic.Int64
	refused     map[int]int
	mu          sync.Mutex
	hashes      [][20]byte
	stop        chan struct{}
	wg          sync.WaitGroup
}

func startProducer(addr, token string, rate int) *harnessProducer {
	p := &harnessProducer{addr: addr, token: token, hc: &http.Client{Timeout: 30 * time.Second}, refused: map[int]int{}, stop: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		tk := time.NewTicker(250 * time.Millisecond) // batches of rate/4 records
		defer tk.Stop()
		per := max(1, rate/4)
		seq := 0
		for {
			select {
			case <-p.stop:
				return
			case <-tk.C:
			}
			var raws []json.RawMessage
			var batch [][20]byte
			for i := 0; i < per; i++ {
				seq++
				name := fmt.Sprintf("1080p ingest w9ing %08d", seq)
				var files []persistence.File
				for f := 0; f < 1+seq%4; f++ {
					files = append(files, persistence.File{Path: fmt.Sprintf("d/f%d.bin", f), Size: int64(seq%977 + f + 1)})
				}
				var total int64
				for _, f := range files {
					total += f.Size
				}
				var h [20]byte
				copy(h[:], sha1Sum("w9ing/"+strconv.Itoa(seq)))
				b, err := record.Encode(record.Record{InfoHash: h, Name: name, TotalSize: total, DiscoveredOn: 1_800_000_000 + int64(seq), Files: files}, codec.DefaultLimits())
				if err != nil {
					continue
				}
				raws = append(raws, b)
				batch = append(batch, h)
			}
			body, _ := json.Marshal(map[string]any{"records": raws})
			req, _ := http.NewRequest("POST", "http://"+p.addr+"/api/ingest/v1/torrents", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+p.token)
			req.Header.Set("Content-Type", "application/json")
			p.sent.Add(int64(len(batch)))
			resp, err := p.hc.Do(req)
			code := 0
			if err == nil {
				// An acknowledgement whose body cannot be read to the end is not
				// counted as one: it is recorded under -1 and fails the run's gate.
				if _, rerr := readBody(resp.Body); rerr == nil {
					code = resp.StatusCode
				} else {
					code = -1
				}
				resp.Body.Close()
			}
			p.mu.Lock()
			if code == 202 {
				p.acked.Add(int64(len(batch)))
				p.hashes = append(p.hashes, batch...)
			} else {
				p.refused[code]++
			}
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *harnessProducer) halt() { close(p.stop); p.wg.Wait() }

// ---- gc trace -----------------------------------------------------------------------

var harnessGCRe = regexp.MustCompile(`gc (\d+) @([\d.]+)s (\d+)%: ([\d.]+)\+([\d.]+)\+([\d.]+) ms clock, .*?, (\d+)->(\d+)->(\d+) MB`)

type harnessGC struct {
	T, LiveMB, HeapMB float64
}

func parseGC(out string) (pts []harnessGC, count int, pauseMs float64) {
	for _, l := range strings.Split(out, "\n") {
		if m := harnessGCRe.FindStringSubmatch(l); m != nil {
			t, _ := strconv.ParseFloat(m[2], 64)
			a, _ := strconv.ParseFloat(m[4], 64)
			c, _ := strconv.ParseFloat(m[6], 64)
			h, _ := strconv.ParseFloat(m[7], 64)
			live, _ := strconv.ParseFloat(m[9], 64)
			pts = append(pts, harnessGC{t, live, h})
			count++
			pauseMs += a + c
		}
	}
	return
}

// ---- setup --------------------------------------------------------------------------

func harnessLoadEnv(extra ...string) []string { return webEnv(extra...) }

// harnessPrepare makes the dataset database ready: web and ingest schemas, the load
// users, their sessions and tokens (through the real interfaces), and the id
// pools. webAddr must already serve the database.
func harnessAccounts(t *testing.T, bin, dsn string) {
	t.Helper()
	for _, cmd := range []string{"web-schema-init", "ingest-schema-init"} {
		if out, code := runWebSchemaInit(t, bin, dsn, cmd); code != 0 {
			t.Fatalf("migrate %s: %d %s", cmd, code, out)
		}
	}
	// Tokens of earlier runs (kept, never deleted by design) are revoked so the
	// per-user cap of 50 unrevoked tokens leaves room for this run's pools. Only the
	// exact generated identities and only credentials carrying the harness label are
	// touched.
	names := harnessUserNames()
	for _, tbl := range []string{"api_tokens", "rss_tokens"} {
		sqlExec(t, dsn, `UPDATE magnetico_web.`+tbl+` SET revoked_at = now() WHERE revoked_at IS NULL AND label = $2 AND user_id IN (SELECT id FROM magnetico_web.users WHERE username = ANY($1))`, names, harnessTokenLabel)
	}
	for _, name := range names {
		if sqlCount(t, dsn, `SELECT count(*) FROM magnetico_web.users WHERE username = $1`, name) == 1 {
			continue
		}
		if out, errOut, code := runWebAdminBinary(t, bin, dsn, "", nil, "user", "add", name); code != 0 {
			t.Fatalf("user add: %d %s%s", code, out, errOut)
		}
		if out, errOut, code := runWebAdminBinary(t, bin, dsn, harnessLoadPassword+"\n", nil, "user", "set-password", name); code != 0 {
			t.Fatalf("set-password: %d %s%s", code, out, errOut)
		}
	}
}

func harnessIdentities(t *testing.T, addr, dsn string) *harnessPools {
	t.Helper()
	p := &harnessPools{}
	for i := 0; i < harnessLoadUsers; i++ {
		name := harnessUserNames()[i]
		xff := fmt.Sprintf("10.77.%d.%d", i, 1)
		b := newBrowser(t, addr)
		b.get("/login", "X-Forwarded-For", xff)
		if resp := b.post("/login", url.Values{"username": {name}, "password": {harnessLoadPassword}, "csrf_token": {b.token()}}, "X-Forwarded-For", xff); resp.StatusCode != 303 {
			t.Fatalf("login %s: %d", name, resp.StatusCode)
		}
		id := &harnessIdent{user: name, xff: xff, session: sessionCookieOf(b, addr)}
		for _, kind := range []string{"api", "rss"} {
			for k := 0; k < harnessTokensPerUser; k++ {
				tab, path := "api", "/account/api-tokens"
				if kind == "rss" {
					tab, path = "rss", "/account/rss-tokens"
				}
				b.get("/account?tab="+tab, "X-Forwarded-For", xff)
				if resp := b.post(path, url.Values{"label": {harnessTokenLabel}, "csrf_token": {b.token()}}, "X-Forwarded-For", xff); resp.StatusCode != 200 {
					t.Fatalf("mint %s for %s: %d", kind, name, resp.StatusCode)
				}
				m := tokenInPage.FindStringSubmatch(b.body)
				if m == nil {
					t.Fatalf("no %s token revealed", kind)
				}
				if kind == "api" {
					id.api = append(id.api, m[1])
				} else {
					id.rss = append(id.rss, m[1])
				}
			}
		}
		p.idents = append(p.idents, id)
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(context.Background(), `SELECT id, file_count FROM magneticod.torrents WHERE file_count > 1 AND name LIKE '% multi %'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, n int
		_ = rows.Scan(&id, &n)
		switch {
		case n <= 50:
			p.small = append(p.small, id)
		case n <= 500:
			p.medium = append(p.medium, id)
		case n <= 4000:
			p.large = append(p.large, id)
		default:
			p.huge = append(p.huge, id)
		}
	}
	rows.Close()
	if len(p.small) == 0 || len(p.medium) == 0 || len(p.large) == 0 || len(p.huge) == 0 {
		t.Fatalf("the dataset lacks a class of multi-file torrents: %d/%d/%d/%d", len(p.small), len(p.medium), len(p.large), len(p.huge))
	}
	return p
}

// ---- the run ------------------------------------------------------------------------

type harnessEnv struct {
	t         *testing.T
	bin       string
	dsn       string
	outDir    string
	label     string
	webMem    string
	goMem     string
	web       *harnessScoped
	webAddr   string
	ingest    *harnessScoped
	ingAddr   string
	ingTok    string
	pools     *harnessPools
	samp      *harnessSampler
	failed    []string
	producers []*harnessProducer
	diagAddr  string

	// drainDeadline overrides harnessDrainDeadline (tests only); onFail, when set,
	// receives gate failures instead of the test (tests of the harness itself).
	drainDeadline time.Duration
	onFail        func(string)
}

func (e *harnessEnv) startWeb() {
	e.t.Helper()
	e.webAddr = freeLoopbackAddr(e.t)
	env := harnessLoadEnv("MAGNETICO_WEB_DATABASE="+e.dsn, "MAGNETICO_WEB_LISTEN="+e.webAddr, "MAGNETICO_WEB_PUBLIC_URL=http://"+e.webAddr,
		"MAGNETICO_WEB_TRUSTED_PROXIES=127.0.0.1/32", "MAGNETICO_WEB_POOL_MAX_CONNS=20", "GOMAXPROCS=2", "GODEBUG=gctrace=1", "GOMEMLIMIT="+e.goMem)
	if v := os.Getenv("MAGNETICO_BENCH_SEARCH_CONCURRENCY"); v != "" {
		env = append(env, "MAGNETICO_WEB_SEARCH_CONCURRENCY="+v)
	}
	if strings.Contains(os.Getenv("MAGNETICO_BENCH_TAGS"), "diag") {
		e.diagAddr = freeLoopbackAddr(e.t)
		env = append(env, "MAGNETICO_DIAG_ADDR="+e.diagAddr)
	}
	e.web = startScoped(e.t, "web", e.webMem, 200, env, e.bin, "web")
	e.waitReady("http://" + e.webAddr + "/readyz")
}

func (e *harnessEnv) startIngest() {
	e.t.Helper()
	e.ingAddr = freeLoopbackAddr(e.t)
	tok := make([]byte, 24)
	for i := range tok {
		tok[i] = byte(rand.IntN(256))
	}
	e.ingTok = hex.EncodeToString(tok)
	env := harnessLoadEnv("MAGNETICO_INGEST_TOKEN="+e.ingTok, "MAGNETICO_INGEST_DATABASE="+e.dsn, "MAGNETICO_INGEST_LISTEN="+e.ingAddr, "GOMAXPROCS=2", "GOMEMLIMIT=256MiB")
	e.ingest = startScoped(e.t, "ingest", "1G", 100, env, e.bin, "ingest")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("POST", "http://"+e.ingAddr+"/api/ingest/v1/torrents/exists", strings.NewReader(`{"info_hashes":[]}`))
		req.Header.Set("Authorization", "Bearer "+e.ingTok)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("ingest not ready:\n%s", e.ingest.out.String())
}

func (e *harnessEnv) waitReady(u string) {
	e.t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(u); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("%s never became ready", u)
}

func (e *harnessEnv) write(kind string, v any) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"kind": kind, "label": e.label, "when": time.Now().UTC().Format(time.RFC3339), "data": v})
	e.t.Logf("%s", b)
	f, err := os.OpenFile(filepath.Join(e.outDir, e.label+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func (e *harnessEnv) gate(cond bool, format string, args ...any) {
	if !cond {
		msg := fmt.Sprintf(format, args...)
		e.failed = append(e.failed, msg)
		if e.onFail != nil {
			e.onFail(msg)
			return
		}
		e.t.Errorf("GATE FAILED: %s", msg)
	}
}

// segment runs one load segment and records and gates what it saw.
func (e *harnessEnv) segment(name string, conc int, d time.Duration, ingest bool, gates segGates) {
	e.t.Helper()
	acc := newHarnessAcc()
	load := newHarnessLoad(e.webAddr, e.pools)
	var prod *harnessProducer
	if ingest {
		prod = startProducer(e.ingAddr, e.ingTok, harnessIngestRate)
	}
	start := time.Now()
	tStart := time.Since(e.samp.start).Seconds()
	if conc > 0 {
		load.run(conc, d, gates.think, acc)
	} else {
		time.Sleep(d)
	}
	wall := time.Since(start)
	rec := map[string]any{"segment": name, "concurrency": conc, "seconds": wall.Seconds(), "ingest": ingest,
		"t_start": tStart, "t_end": time.Since(e.samp.start).Seconds()}
	total, classes := acc.stats()
	n := 0
	for _, v := range total {
		n += v
	}
	rec["requests"], rec["rps"], rec["rps_ok"], rec["outcomes"], rec["classes"] = n, float64(n)/wall.Seconds(), float64(total[oOK])/wall.Seconds(), total, classes
	rec["overload"], rec["think_ms"] = gates.overload, gates.think.Milliseconds()
	if prod != nil {
		prod.halt()
		prod.mu.Lock()
		rec["ingest_sent"], rec["ingest_acked"], rec["ingest_refused"] = prod.sent.Load(), prod.acked.Load(), prod.refused
		prod.mu.Unlock()
		e.producers = append(e.producers, prod)
	}
	e.write("segment", rec)
	// Gates (see the plan): every request has an expected status, any other is a failure.
	e.gate(float64(total[o429]) <= 0.001*float64(max(n, 1)), "%s: 429 responses %d of %d exceed 0.1%% (the run is invalid, enlarge the credential pool)", name, total[o429], n)
	e.gate(total[oOther5xx] == 0, "%s: %d other 5xx responses", name, total[oOther5xx])
	e.gate(total[oUnexpect] == 0, "%s: %d unexpected 4xx responses", name, total[oUnexpect])
	e.gate(total[oTransport] == 0, "%s: %d transport errors or client timeouts", name, total[oTransport])
	if gates.noTimeouts {
		e.gate(total[oTimeout] == 0, "%s: %d search timeouts", name, total[oTimeout])
	} else {
		e.gate(float64(total[oTimeout]) <= 0.001*float64(max(n, 1)), "%s: %d search timeouts exceed 0.1%%", name, total[oTimeout])
	}
	switch {
	case gates.noBusy:
		e.gate(total[oBusy] == 0, "%s: %d gate-saturation 503 responses", name, total[oBusy])
	case gates.overload:
		// Informational: shedding is expected; the counts are in the record.
	default:
		e.gate(float64(total[oBusy]) <= 0.05*float64(max(n, 1)), "%s: gate-saturation 503 responses %d exceed 5%%", name, total[oBusy])
	}
	if prod != nil {
		prod.mu.Lock()
		bad := len(prod.refused)
		prod.mu.Unlock()
		e.gate(bad == 0, "%s: ingest producer was refused: %v", name, prod.refused)
	}
}

// segGates are the per-segment expectations.
//
//	noTimeouts  no search timeout is allowed (otherwise at most 0.1%)
//	noBusy      no gate-saturation 503 is allowed
//	overload    the segment deliberately oversubscribes the service (closed-loop
//	            workers without think time): bounded 503 "busy" responses are the
//	            designed behaviour, so only the other classes are gated; the
//	            recovery segment that follows must be clean
//	think       mean think time of an interactive user (0 = closed loop)
type segGates struct {
	noTimeouts, noBusy, overload bool
	think                        time.Duration
}

// drain stops nothing: it waits for the ingest queue to empty within the
// deadline and then verifies that every acknowledged record is stored. Every
// query error is a gate failure (never read as "queue empty"), and every query
// runs under a context, so a blocked statement cannot outlive the deadline.
func (e *harnessEnv) drain() {
	e.t.Helper()
	start := time.Now()
	limit := harnessDrainDeadline
	if e.drainDeadline > 0 {
		limit = e.drainDeadline
	}
	waitCtx, cancelWait := context.WithDeadline(context.Background(), start.Add(limit))
	defer cancelWait()
	conn, err := pgx.Connect(waitCtx, e.dsn)
	if err != nil {
		e.gate(false, "drain: cannot connect: %v", err)
		return
	}
	defer conn.Close(context.Background())
	depth := -1 // unknown until a query succeeds
	for {
		var n int
		if err := conn.QueryRow(waitCtx, `SELECT count(*) FROM magnetico_ingest.queue`).Scan(&n); err != nil {
			if waitCtx.Err() != nil {
				break // the deadline; depth keeps its last known value
			}
			e.gate(false, "drain: the queue depth query failed: %v", err)
			return
		}
		depth = n
		if depth == 0 {
			break
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(time.Second):
		}
		if waitCtx.Err() != nil {
			break
		}
	}
	took := time.Since(start)
	e.gate(depth == 0, "drain: the ingest queue holds %d rows (-1: unknown) after %s (deadline %s)", depth, took.Round(time.Second), limit)
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), harnessVerifyTimeout)
	defer cancelVerify()
	var quarantined int
	if err := conn.QueryRow(verifyCtx, `SELECT count(*) FROM magnetico_ingest.quarantine`).Scan(&quarantined); err != nil {
		e.gate(false, "drain: the quarantine count query failed: %v", err)
		return
	}
	e.gate(quarantined == 0, "drain: %d quarantined records", quarantined)
	// Every acknowledged record is stored: batched index lookups, once.
	var all [][]byte
	for _, p := range e.producers {
		p.mu.Lock()
		for _, h := range p.hashes {
			all = append(all, append([]byte(nil), h[:]...))
		}
		p.mu.Unlock()
	}
	missing, unverified := 0, 0
	for i := 0; i < len(all); i += 1000 {
		chunk := all[i:min(i+1000, len(all))]
		var found int
		if err := conn.QueryRow(verifyCtx, `SELECT count(*) FROM magneticod.torrents WHERE info_hash = ANY($1)`, chunk).Scan(&found); err != nil {
			// The chunk is neither found nor missing: it stays unverified and fails the gate.
			unverified += len(chunk)
			e.gate(false, "drain: the storage verification query failed: %v", err)
			if verifyCtx.Err() != nil {
				unverified += len(all) - min(i+1000, len(all))
				break
			}
			continue
		}
		missing += len(chunk) - found
	}
	e.gate(missing == 0, "drain: %d of %d acknowledged records are not stored", missing, len(all))
	e.gate(unverified == 0, "drain: %d of %d acknowledged records could not be verified", unverified, len(all))
	e.write("drain", map[string]any{"drain_seconds": took.Seconds(), "queue_left": depth, "acked": len(all), "missing": missing, "unverified": unverified, "quarantined": quarantined})
}

// finish stops the services, collects gc and cache information and writes the
// per-process report.
func (e *harnessEnv) finish(prefix string) {
	e.t.Helper()
	e.samp.finish()
	e.samp.mu.Lock()
	samples, queue := append([]harnessSample(nil), e.samp.samples...), append([]harnessPoint(nil), e.samp.queue...)
	qTries, qFails := e.samp.qTries, e.samp.qFails
	e.samp.mu.Unlock()
	// Missing queue samples are reported, and too many of them fail the run: a
	// queue-depth series with holes cannot support the backlog-slope gate.
	e.write("queue_samples", map[string]any{"attempted": qTries, "missing": qFails, "taken": len(queue)})
	e.gate(qTries < 3 || qFails*10 <= qTries, "queue-depth sampling: %d of %d samples are missing", qFails, qTries)
	for _, p := range []*harnessScoped{e.web, e.ingest} {
		rep := map[string]any{"proc": p.name}
		_, hwm, _ := readStatusOf(p.pid)
		rep["vmhwm_mib"] = float64(hwm) / (1 << 20)
		rep["cgroup_peak_mib"] = float64(readUint(p.cg+"/memory.peak")) / (1 << 20)
		ev := cgroupEvents(p.cg)
		rep["memory_events"] = ev
		e.gate(ev["oom_kill"] == 0 && ev["oom"] == 0, "%s: the %s cgroup had OOM events %v", prefix, p.name, ev)
		code := p.stop(60 * time.Second)
		rep["exit_code"] = code
		e.gate(code == 0, "%s: %s exit code %d on SIGTERM", prefix, p.name, code)
		gcs, count, pause := parseGC(p.out.String())
		rep["gc_count"], rep["gc_pause_ms_total"] = count, pause
		var live []float64
		for _, g := range gcs {
			live = append(live, g.LiveMB)
		}
		if len(live) > 0 {
			rep["heap_after_gc_mib_max"] = maxOf(live)
		}
		for _, l := range strings.Split(p.out.String(), "\n") {
			if strings.Contains(l, "file index cache stopped") || strings.Contains(l, "pool") && strings.Contains(l, "stopped") {
				rep["shutdown_stats"] = l
			}
		}
		rep["gc_series"] = thin(gcs, 400)
		// Log lines of level WARN and ERROR, counted by message (the logs carry no
		// names, paths, queries or secrets by design, so the messages are safe to keep).
		lv := map[string]int{}
		msgRe := regexp.MustCompile(`level=(WARN|ERROR) msg="?([^"=]*[^" ])"?`)
		for _, l := range strings.Split(p.out.String(), "\n") {
			if m := msgRe.FindStringSubmatch(l); m != nil {
				lv[m[1]+": "+m[2]]++
			}
		}
		rep["warn_error_lines"] = lv
		for k, n := range lv {
			if strings.HasPrefix(k, "ERROR") {
				e.gate(false, "%s: %d log line(s) at level %s", prefix, n, k)
			}
		}
		e.write("process", rep)
	}
	e.write("samples", map[string]any{"samples": thinS(samples, 2000), "ingest_queue": queue})
}

func maxOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		m = max(m, x)
	}
	return m
}

func thin(g []harnessGC, n int) []harnessGC {
	if len(g) <= n {
		return g
	}
	step := len(g) / n
	var out []harnessGC
	for i := 0; i < len(g); i += step {
		out = append(out, g[i])
	}
	return out
}

func thinS(s []harnessSample, n int) []harnessSample {
	if len(s) <= n {
		return s
	}
	step := len(s) / n
	var out []harnessSample
	for i := 0; i < len(s); i += step {
		out = append(out, s[i])
	}
	return out
}

func newHarnessEnv(t *testing.T) *harnessEnv {
	t.Helper()
	if !benchkit.Enabled() {
		t.Skip("set MAGNETICO_BENCH_LARGE=1 to run the load profiles")
	}
	dsn, out := os.Getenv("MAGNETICO_BENCH_DSN"), os.Getenv("MAGNETICO_BENCH_OUT")
	if dsn == "" || out == "" {
		t.Fatal("MAGNETICO_BENCH_DSN and MAGNETICO_BENCH_OUT are required")
	}
	// Before anything is built, started or mutated: the target must be the
	// confirmed, local, benchmark-shaped database (see benchguard_test.go).
	harnessRequireDisposable(t, dsn)
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	var tags []string
	if v := os.Getenv("MAGNETICO_BENCH_TAGS"); v != "" {
		tags = []string{v}
	}
	bin := testkit.Binary(t, tags...)
	e := &harnessEnv{t: t, bin: bin, dsn: dsn, outDir: out, label: envOr("MAGNETICO_BENCH_LABEL", "w9"),
		webMem: envOr("MAGNETICO_BENCH_WEB_MEM", "1G"), goMem: envOr("MAGNETICO_BENCH_WEB_GOMEMLIMIT", "768MiB")}
	harnessAccounts(t, bin, dsn)
	return e
}

// begin starts (or restarts) the services, creates the identities and starts
// sampling.
func (e *harnessEnv) begin() {
	e.t.Helper()
	e.startIngest()
	e.startWeb()
	e.pools = harnessIdentities(e.t, e.webAddr, e.dsn)
	e.samp = startSampler([]*harnessScoped{e.web, e.ingest}, e.dsn)
	e.write("start", map[string]any{
		// The web process started this long before the sampler's clock: add it to a
		// gctrace timestamp to compare it with a sample time.
		"web_age_at_sampler_start_s": time.Since(e.web.start).Seconds(),
		"machine":                    benchkit.Describe().String(), "web_mem": e.webMem, "web_gomemlimit": e.goMem, "git": gitSHA(),
		"pools": map[string]int{"small": len(e.pools.small), "medium": len(e.pools.medium), "large": len(e.pools.large), "huge": len(e.pools.huge)},
	})
}

// poll records one reading of the diagnostic endpoint (the diag build only).
func (e *harnessEnv) poll(label string) {
	e.t.Helper()
	if e.diagAddr == "" {
		return
	}
	resp, err := http.Get("http://" + e.diagAddr + "/diag")
	if err != nil {
		e.t.Fatalf("diag endpoint: %v", err)
	}
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		e.t.Fatal(err)
	}
	v["label"], v["t"] = label, time.Since(e.samp.start).Seconds()
	e.write("diag", v)
	// The in-use heap breakdown at the points that matter.
	if label == "baseline-after-start" || label == "after-load" || label == "idle-end" {
		hresp, err := http.Get("http://" + e.diagAddr + "/heap")
		if err != nil {
			e.t.Fatalf("diag heap: %v", err)
		}
		defer hresp.Body.Close()
		var h map[string]any
		if err := json.NewDecoder(hresp.Body).Decode(&h); err != nil {
			e.t.Fatal(err)
		}
		h["label"], h["t"] = label, time.Since(e.samp.start).Seconds()
		e.write("heap", h)
	}
}

// pollEvery polls every d until stop is closed.
func (e *harnessEnv) pollEvery(d time.Duration, label string, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(d):
			e.poll(label)
		}
	}
}

func gitSHA() string {
	b, _ := exec.Command("git", "rev-parse", "--short=12", "HEAD").Output()
	return strings.TrimSpace(string(b))
}

func TestLoad(t *testing.T) {
	e := newHarnessEnv(t)
	profile := envOr("MAGNETICO_BENCH_PROFILE", "steps")
	stepSec, _ := strconv.Atoi(envOr("MAGNETICO_BENCH_STEP_SECONDS", "180"))
	step := time.Duration(stepSec) * time.Second
	switch profile {
	case "steps":
		// Two series: from a cold start, then after a restart with cold
		// application caches (the identities are recreated).
		for series := 1; series <= 2; series++ {
			e.begin()
			// Closed loop (no think time): c4-c16 are within capacity, c32 and c64
			// oversubscribe the 4 search slots on purpose (overload).
			for _, c := range []int{4, 8, 16} {
				e.segment(fmt.Sprintf("series%d-c%d", series, c), c, step, false, segGates{noTimeouts: true, noBusy: true})
			}
			for _, c := range []int{32, 64} {
				e.segment(fmt.Sprintf("series%d-c%d-overload", series, c), c, step, false, segGates{noTimeouts: c <= 32, overload: true})
			}
			// Recovery: after the overload the service answers normally again.
			e.segment(fmt.Sprintf("series%d-recovery-c4", series), 4, 30*time.Second, false, segGates{noTimeouts: true, noBusy: true})
			// Interactive users with a mean think time of 1 s: the bound of 5%
			// busy applies here.
			for _, users := range []int{50, 150, 400} {
				e.segment(fmt.Sprintf("series%d-interactive-u%d", series, users), users, step, false, segGates{noTimeouts: users <= 150, think: time.Second})
			}
			e.finish(fmt.Sprintf("series%d", series))
		}
	case "regress":
		// A short, representative re-run on the final dependency pins (07J review):
		// one series of the step profile at a shorter step, the same segments and
		// gates as "steps" for the tiers that matter, plus a short ingest-on segment
		// and the checked drain. Compared against the "steps" baseline by label.
		e.begin()
		for _, c := range []int{4, 16} {
			e.segment(fmt.Sprintf("regress-c%d", c), c, step, false, segGates{noTimeouts: true, noBusy: true})
		}
		e.segment("regress-c32-overload", 32, step, false, segGates{noTimeouts: true, overload: true})
		e.segment("regress-recovery-c4", 4, 30*time.Second, false, segGates{noTimeouts: true, noBusy: true})
		for _, users := range []int{50, 150} {
			e.segment(fmt.Sprintf("regress-interactive-u%d", users), users, step, false, segGates{noTimeouts: true, think: time.Second})
		}
		e.segment("regress-ingest-c16", 16, 2*step, true, segGates{noTimeouts: true, noBusy: true})
		e.drain()
		e.finish("regress")
	case "memfloor":
		e.begin()
		e.segment("memfloor-c32", 32, 3*step, false, segGates{noTimeouts: true, overload: true})
		e.finish("memfloor")
	case "soak":
		scale, _ := strconv.ParseFloat(envOr("MAGNETICO_BENCH_SOAK_SCALE", "1"), 64)
		dur := func(min float64) time.Duration { return time.Duration(min * scale * float64(time.Minute)) }
		e.begin()
		e.segment("warmup", 16, dur(30), false, segGates{noTimeouts: true, noBusy: true})
		e.segment("steady", 16, dur(90), true, segGates{noTimeouts: true, noBusy: true})
		e.drain()
		e.segment("idle", 0, dur(20), false, segGates{})
		e.finish("soak")
	case "knee":
		// How the interactive capacity depends on the number of search slots
		// (MAGNETICO_BENCH_SEARCH_CONCURRENCY): the same interactive tiers once.
		e.begin()
		for _, users := range []int{150, 400, 800} {
			e.segment(fmt.Sprintf("knee-u%d", users), users, step, false, segGates{noTimeouts: true, overload: true, think: time.Second})
		}
		e.finish("knee")
	case "diag":
		// The diagnostic run (build tag diag): the soak timeline, shorter, with
		// non-destructive readings of one process before, during and after the
		// load. Its numbers are not used for performance (the polling would
		// perturb the idle phase); it explains growth and shows leaks.
		scale, _ := strconv.ParseFloat(envOr("MAGNETICO_BENCH_SOAK_SCALE", "0.35"), 64)
		dur := func(min float64) time.Duration { return time.Duration(min * scale * float64(time.Minute)) }
		e.begin()
		if e.diagAddr == "" {
			t.Fatal("the diag profile needs MAGNETICO_BENCH_TAGS=diag")
		}
		e.poll("baseline-after-start")
		stop := make(chan struct{})
		go e.pollEvery(2*time.Minute, "during-load", stop)
		e.segment("warmup", 16, dur(30), false, segGates{noTimeouts: true, noBusy: true})
		e.segment("steady", 16, dur(90), true, segGates{noTimeouts: true, noBusy: true})
		close(stop)
		e.poll("after-load")
		e.drain()
		e.poll("idle-start")
		e.segment("idle", 0, dur(20), false, segGates{})
		e.poll("idle-end")
		e.finish("diag")
	default:
		t.Fatalf("unknown profile %q", profile)
	}
	if len(e.failed) > 0 {
		t.Fatalf("%d gate(s) failed:\n  %s", len(e.failed), strings.Join(e.failed, "\n  "))
	}
}

func harnessDrainEnv(t *testing.T, dsn string, deadline time.Duration) (*harnessEnv, *[]string) {
	t.Helper()
	var fails []string
	e := &harnessEnv{t: t, dsn: dsn, outDir: t.TempDir(), label: "drain", drainDeadline: deadline,
		onFail: func(m string) { fails = append(fails, m) }}
	return e, &fails
}

func TestDrainReportsQueryErrorsAndEnforcesItsDeadline(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)

	// 1. No ingest schema: the failed query must fail the gate, not read as an empty queue.
	e, fails := harnessDrainEnv(t, dsn, 5*time.Second)
	e.drain()
	if len(*fails) == 0 || !strings.Contains((*fails)[0], "queue depth query failed") {
		t.Fatalf("a failed queue query was not reported: %v", *fails)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`CREATE SCHEMA magnetico_ingest`,
		`CREATE TABLE magnetico_ingest.queue (x int)`,
		`CREATE TABLE magnetico_ingest.quarantine (x int)`,
		`INSERT INTO magnetico_ingest.queue VALUES (1)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	// 2. A queue that never empties: the deadline ends the wait and fails the gate.
	e, fails = harnessDrainEnv(t, dsn, 1500*time.Millisecond)
	start := time.Now()
	e.drain()
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("drain overran its 1.5 s deadline: %s", took)
	}
	if len(*fails) == 0 || !strings.Contains((*fails)[0], "queue holds 1 rows") {
		t.Fatalf("a non-empty queue did not fail the gate: %v", *fails)
	}

	// 3. A statement blocked behind a lock: the context must cut it at the deadline,
	// and the unknown depth is reported as such, never as zero.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE magnetico_ingest.queue IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	e, fails = harnessDrainEnv(t, dsn, 1500*time.Millisecond)
	start = time.Now()
	e.drain()
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("a blocked statement outlived the deadline: drain took %s", took)
	}
	if len(*fails) == 0 || !strings.Contains((*fails)[0], "-1: unknown") {
		t.Fatalf("a blocked queue query was not reported as unknown depth: %v", *fails)
	}
}
