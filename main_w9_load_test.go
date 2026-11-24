package main

// W9 load steps and soak (task 07J, stage S6). Gated like the other heavy
// harnesses: nothing here runs in the ordinary suite.
//
//	MAGNETICO_BENCH_LARGE=1
//	MAGNETICO_W9_DSN       owner DSN of the seeded dataset database (TestW9SeedLargeDataset)
//	MAGNETICO_W9_OUT       output directory for the JSONL results (created)
//	MAGNETICO_W9_PROFILE   "steps" | "soak" | "memfloor"   (what to run)
//
// Optional: MAGNETICO_W9_LABEL, MAGNETICO_W9_WEB_MEM (cgroup MemoryMax, default
// 1G), MAGNETICO_W9_WEB_GOMEMLIMIT (default 768MiB), MAGNETICO_W9_STEP_SECONDS
// (default 180), MAGNETICO_W9_SOAK_SCALE (multiplies the 30/90/20 minute soak
// phases; 1 by default), MAGNETICO_W9_TAGS (build tags, "w9diag" for the
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
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
	"github.com/skobkin/magnetico-web/internal/web/benchkit"
	"github.com/skobkin/magnetico-web/persistence"
)

const (
	w9LoadUsers      = 32
	w9TokensPerUser  = 10
	w9LoadPassword   = "w9-load-password-0001"
	w9ClientTimeout  = 90 * time.Second
	w9IngestRate     = 20 // records per second during the steady phase
	w9DrainDeadline  = 10 * time.Minute
	w9SampleInterval = 10 * time.Second
)

// outcome classes, reported separately and never merged.
const (
	oOK        = "ok"
	o429       = "limited429"
	oBusy      = "busy503"
	oTimeout   = "timeout503"
	oOther5xx  = "other5xx"
	oUnexpect  = "unexpected4xx"
	oTransport = "transport"
)

var w9Outcomes = []string{oOK, o429, oBusy, oTimeout, oOther5xx, oUnexpect, oTransport}

// ---- processes in their own scopes --------------------------------------------------

type w9Scoped struct {
	name   string
	cmd    *exec.Cmd
	out    *syncBuf
	exited chan error
	pid    int
	cg     string
	start  time.Time
}

var w9ScopeSeq atomic.Int64

// startScoped runs bin inside its own memory- and CPU-limited systemd user scope.
func startScoped(t *testing.T, name, mem string, cpuPercent int, env []string, bin string, args ...string) *w9Scoped {
	t.Helper()
	unit := fmt.Sprintf("magnetico-w9-%s-%d-%d", name, os.Getpid(), w9ScopeSeq.Add(1))
	sd := []string{"systemd-run", "--user", "--scope", "--quiet", "--collect", "--unit=" + unit,
		"-p", "MemoryMax=" + mem, "-p", "MemorySwapMax=0", "-p", "CPUQuota=" + strconv.Itoa(cpuPercent) + "%", bin}
	cmd := exec.Command(sd[0], append(sd[1:], args...)...)
	cmd.Env = env
	out := &syncBuf{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &w9Scoped{name: name, cmd: cmd, out: out, exited: make(chan error, 1), pid: cmd.Process.Pid, start: time.Now()}
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
func (s *w9Scoped) stop(within time.Duration) int {
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

type w9Sample struct {
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

type w9Sampler struct {
	start   time.Time
	mu      sync.Mutex
	samples []w9Sample
	queue   []w9Point // ingest queue depth
	stop    chan struct{}
	wg      sync.WaitGroup
}

type w9Point struct {
	T float64 `json:"t"`
	V float64 `json:"v"`
}

func startSampler(procs []*w9Scoped, dsn string) *w9Sampler {
	s := &w9Sampler{start: time.Now(), stop: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var conn *pgx.Conn
		defer func() {
			if conn != nil {
				conn.Close(context.Background())
			}
		}()
		tk := time.NewTicker(w9SampleInterval)
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
				s.samples = append(s.samples, w9Sample{T: now, Proc: p.name, RSSMiB: float64(rss) / (1 << 20),
					CgroupMiB: float64(readUint(p.cg+"/memory.current")) / (1 << 20), CPUSec: cgCPUSeconds(p.cg), Threads: th, FDs: countFDs(p.pid)})
			}
			s.mu.Unlock()
			if conn == nil {
				conn, _ = pgx.Connect(context.Background(), dsn)
			}
			if conn != nil {
				var n int
				// The queue table is bounded by the admission quota: a cheap count.
				if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM magnetico_ingest.queue`).Scan(&n); err == nil {
					s.mu.Lock()
					s.queue = append(s.queue, w9Point{now, float64(n)})
					s.mu.Unlock()
				} else {
					conn.Close(context.Background())
					conn = nil
				}
			}
		}
	}()
	return s
}

func (s *w9Sampler) finish() {
	close(s.stop)
	s.wg.Wait()
}

// ---- identities and requests --------------------------------------------------------

type w9Ident struct {
	user    string
	xff     string
	session string // "name=value"
	api     []string
	rss     []string
}

type w9Pools struct {
	small, medium, large, huge []int
	idents                     []*w9Ident
	apiSeq, rssSeq             atomic.Int64
}

func (p *w9Pools) nextAPI() string {
	n := p.apiSeq.Add(1)
	id := p.idents[int(n)%len(p.idents)]
	return id.api[int(n/int64(len(p.idents)))%len(id.api)]
}

func (p *w9Pools) nextRSS() string {
	n := p.rssSeq.Add(1)
	id := p.idents[int(n)%len(p.idents)]
	return id.rss[int(n/int64(len(p.idents)))%len(id.rss)]
}

type w9Req struct {
	class  string
	path   string
	hdr    map[string]string
	expect []int
}

var (
	w9Common = []string{"1080p", "720p", "x264", "x265", "bluray", "webrip", "complete", "season", "ubuntu", "linux", "multi", "remux"}
	w9Mixes  = []struct {
		class  string
		weight int
	}{{"search", 45}, {"details", 20}, {"dirs", 12}, {"tree", 5}, {"api", 10}, {"rss", 3}, {"magnet", 3}, {"apifiles", 2}}
)

func w9PickClass(rng *rand.Rand) string {
	n := rng.IntN(100)
	for _, m := range w9Mixes {
		if n < m.weight {
			return m.class
		}
		n -= m.weight
	}
	return "search"
}

func w9Term(rng *rand.Rand) string {
	switch n := rng.IntN(100); {
	case n < 50:
		return w9Common[rng.IntN(len(w9Common))]
	case n < 60:
		return w9Common[rng.IntN(len(w9Common))] + " " + w9Common[rng.IntN(len(w9Common))]
	case n < 80:
		return "m" + strconv.Itoa(int(math.Pow(rng.Float64(), 2)*400))
	case n < 92:
		return "w" + strconv.Itoa(rng.IntN(200000))
	case n < 94:
		return "zzqxj"
	case n < 97:
		return []string{"日本語", "Кириллица"}[rng.IntN(2)]
	}
	return ""
}

func (p *w9Pools) pick(rng *rand.Rand, id *w9Ident) w9Req {
	ses := map[string]string{"Cookie": id.session}
	multi := func() int {
		switch n := rng.IntN(100); {
		case n < 40:
			return p.small[rng.IntN(len(p.small))]
		case n < 75:
			return p.medium[rng.IntN(len(p.medium))]
		case n < 95:
			return p.large[rng.IntN(len(p.large))]
		}
		return p.huge[rng.IntN(len(p.huge))]
	}
	cls := w9PickClass(rng)
	switch cls {
	case "search":
		q := url.Values{}
		if t := w9Term(rng); t != "" {
			q.Set("q", t)
		}
		q.Set("sort", []string{"discovered", "discovered", "discovered", "size", "name"}[rng.IntN(5)])
		q.Set("order", []string{"asc", "desc"}[rng.IntN(2)])
		q.Set("size", []string{"20", "50", "100"}[rng.IntN(3)])
		return w9Req{cls, "/torrents?" + q.Encode(), ses, []int{200}}
	case "details":
		return w9Req{cls, "/torrents/" + strconv.Itoa(multi()), ses, []int{200}}
	case "dirs":
		return w9Req{cls, fmt.Sprintf("/torrents/%d/files?dir=&limit=160", multi()), ses, []int{200}}
	case "tree":
		// Torrents the backend serves as a complete tree (up to 4,000 files); the
		// rest answer 422 and the client falls back to pages, both are expected.
		return w9Req{cls, fmt.Sprintf("/torrents/%d/files/tree", multi()), ses, []int{200, 422}}
	case "api":
		q := url.Values{"limit": {[]string{"20", "50", "100"}[rng.IntN(3)]}}
		if t := w9Term(rng); t != "" {
			q.Set("q", t)
		}
		return w9Req{cls, "/api/v2/torrents?" + q.Encode(), map[string]string{"Authorization": "Bearer " + p.nextAPI()}, []int{200}}
	case "apifiles":
		return w9Req{cls, fmt.Sprintf("/api/v2/torrents/%d/files?limit=200", multi()), map[string]string{"Authorization": "Bearer " + p.nextAPI()}, []int{200}}
	case "rss":
		if rng.IntN(2) == 0 {
			return w9Req{cls, "/api/v2/feeds/latest.rss?feed_token=" + p.nextRSS() + "&page=" + strconv.Itoa(1+rng.IntN(3)), nil, []int{200}}
		}
		return w9Req{cls, "/api/v1/feed/rss/last?api-token=" + p.nextRSS(), nil, []int{200}}
	}
	return w9Req{"magnet", "/magnet/" + hex.EncodeToString(sha1Sum(strconv.Itoa(rng.IntN(1000000)))), nil, []int{307}}
}

func sha1Sum(s string) []byte { h := sha1.Sum([]byte(s)); return h[:] }

// ---- accumulators -------------------------------------------------------------------

type w9Acc struct {
	mu   sync.Mutex
	n    map[string]int            // outcome -> count
	durs map[string][]float32      // class -> ms (ok only)
	cls  map[string]map[string]int // class -> outcome -> count
}

func newW9Acc() *w9Acc {
	return &w9Acc{n: map[string]int{}, durs: map[string][]float32{}, cls: map[string]map[string]int{}}
}

func (a *w9Acc) add(class, outcome string, d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n[outcome]++
	if a.cls[class] == nil {
		a.cls[class] = map[string]int{}
	}
	a.cls[class][outcome]++
	if outcome == oOK {
		a.durs[class] = append(a.durs[class], float32(d.Seconds()*1000))
	}
}

func pct(v []float32, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float32(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return float64(c[min(len(c)-1, int(math.Ceil(p/100*float64(len(c))))-1)])
}

type w9ClassStat struct {
	N        int            `json:"n"`
	Outcomes map[string]int `json:"outcomes"`
	P50      float64        `json:"p50_ms"`
	P95      float64        `json:"p95_ms"`
	P99      float64        `json:"p99_ms"`
	Max      float64        `json:"max_ms"`
}

func (a *w9Acc) stats() (total map[string]int, classes map[string]w9ClassStat) {
	a.mu.Lock()
	defer a.mu.Unlock()
	total = map[string]int{}
	for k, v := range a.n {
		total[k] = v
	}
	classes = map[string]w9ClassStat{}
	for c, o := range a.cls {
		st := w9ClassStat{Outcomes: map[string]int{}}
		for k, v := range o {
			st.Outcomes[k] = v
			st.N += v
		}
		d := a.durs[c]
		st.P50, st.P95, st.P99 = pct(d, 50), pct(d, 95), pct(d, 99)
		for _, x := range d {
			st.Max = max(st.Max, float64(x))
		}
		classes[c] = st
	}
	return
}

// ---- load generation ----------------------------------------------------------------

type w9Load struct {
	addr  string
	pools *w9Pools
	hc    *http.Client
}

func newW9Load(addr string, pools *w9Pools) *w9Load {
	tr := &http.Transport{MaxIdleConnsPerHost: 256, MaxConnsPerHost: 0, IdleConnTimeout: 90 * time.Second}
	return &w9Load{addr: addr, pools: pools, hc: &http.Client{Transport: tr, Timeout: w9ClientTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func classify(status int, body []byte, expect []int) string {
	for _, e := range expect {
		if status == e {
			return oOK
		}
	}
	switch {
	case status == 429:
		return o429
	case status == 503:
		b := string(body)
		switch {
		case strings.Contains(b, "timed out"):
			return oTimeout
		case strings.Contains(b, "busy"):
			return oBusy
		}
		return oOther5xx
	case status >= 500:
		return oOther5xx
	}
	return oUnexpect
}

// run drives conc workers for d and returns what they saw. A worker stops
// starting requests at the deadline and finishes the one in flight.
func (l *w9Load) run(conc int, d time.Duration, think time.Duration, acc *w9Acc) {
	stop := time.Now().Add(d)
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(i)+1, uint64(time.Now().UnixNano())))
			id := l.pools.idents[i%len(l.pools.idents)]
			for time.Now().Before(stop) {
				rq := l.pools.pick(rng, id)
				req, _ := http.NewRequest("GET", "http://"+l.addr+rq.path, nil)
				req.Header.Set("X-Forwarded-For", id.xff)
				for k, v := range rq.hdr {
					req.Header.Set(k, v)
				}
				start := time.Now()
				resp, err := l.hc.Do(req)
				if err != nil {
					acc.add(rq.class, oTransport, time.Since(start))
					continue
				}
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				acc.add(rq.class, classify(resp.StatusCode, body, rq.expect), time.Since(start))
				if think > 0 {
					// An interactive user: an exponentially distributed pause (mean = think).
					time.Sleep(time.Duration(rng.ExpFloat64() * float64(think)))
				}
			}
		}(i)
	}
	wg.Wait()
}

// ---- ingest producer ----------------------------------------------------------------

type w9Producer struct {
	addr, token string
	hc          *http.Client
	sent, acked atomic.Int64
	refused     map[int]int
	mu          sync.Mutex
	hashes      [][20]byte
	stop        chan struct{}
	wg          sync.WaitGroup
}

func startProducer(addr, token string, rate int) *w9Producer {
	p := &w9Producer{addr: addr, token: token, hc: &http.Client{Timeout: 30 * time.Second}, refused: map[int]int{}, stop: make(chan struct{})}
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
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				code = resp.StatusCode
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

func (p *w9Producer) halt() { close(p.stop); p.wg.Wait() }

// ---- gc trace -----------------------------------------------------------------------

var w9GCRe = regexp.MustCompile(`gc (\d+) @([\d.]+)s (\d+)%: ([\d.]+)\+([\d.]+)\+([\d.]+) ms clock, .*?, (\d+)->(\d+)->(\d+) MB`)

type w9GC struct {
	T, LiveMB, HeapMB float64
}

func parseGC(out string) (pts []w9GC, count int, pauseMs float64) {
	for _, l := range strings.Split(out, "\n") {
		if m := w9GCRe.FindStringSubmatch(l); m != nil {
			t, _ := strconv.ParseFloat(m[2], 64)
			a, _ := strconv.ParseFloat(m[4], 64)
			c, _ := strconv.ParseFloat(m[6], 64)
			h, _ := strconv.ParseFloat(m[7], 64)
			live, _ := strconv.ParseFloat(m[9], 64)
			pts = append(pts, w9GC{t, live, h})
			count++
			pauseMs += a + c
		}
	}
	return
}

// slope is the least-squares slope of v over t (units per second).
func slope(ts, vs []float64) float64 {
	n := float64(len(ts))
	if n < 2 {
		return 0
	}
	var st, sv, stt, stv float64
	for i := range ts {
		st += ts[i]
		sv += vs[i]
		stt += ts[i] * ts[i]
		stv += ts[i] * vs[i]
	}
	den := n*stt - st*st
	if den == 0 {
		return 0
	}
	return (n*stv - st*sv) / den
}

// ---- setup --------------------------------------------------------------------------

func w9LoadEnv(extra ...string) []string { return webEnv(extra...) }

// w9Prepare makes the dataset database ready: web and ingest schemas, the load
// users, their sessions and tokens (through the real interfaces), and the id
// pools. webAddr must already serve the database.
func w9Accounts(t *testing.T, bin, dsn string) {
	t.Helper()
	for _, cmd := range []string{"web-schema-init", "ingest-schema-init"} {
		if out, code := runWebSchemaInit(t, bin, dsn, cmd); code != 0 {
			t.Fatalf("migrate %s: %d %s", cmd, code, out)
		}
	}
	have := sqlCount(t, dsn, `SELECT count(*) FROM magnetico_web.users WHERE username LIKE 'w9u%'`)
	for i := have; i < w9LoadUsers; i++ {
		name := fmt.Sprintf("w9u%02d", i)
		if out, errOut, code := runWebAdminBinary(t, bin, dsn, "", nil, "user", "add", name); code != 0 {
			t.Fatalf("user add: %d %s%s", code, out, errOut)
		}
		if out, errOut, code := runWebAdminBinary(t, bin, dsn, w9LoadPassword+"\n", nil, "user", "set-password", name); code != 0 {
			t.Fatalf("set-password: %d %s%s", code, out, errOut)
		}
	}
}

func w9Identities(t *testing.T, addr, dsn string) *w9Pools {
	t.Helper()
	p := &w9Pools{}
	for i := 0; i < w9LoadUsers; i++ {
		name := fmt.Sprintf("w9u%02d", i)
		xff := fmt.Sprintf("10.77.%d.%d", i, 1)
		b := newBrowser(t, addr)
		b.get("/login", "X-Forwarded-For", xff)
		if resp := b.post("/login", url.Values{"username": {name}, "password": {w9LoadPassword}, "csrf_token": {b.token()}}, "X-Forwarded-For", xff); resp.StatusCode != 303 {
			t.Fatalf("login %s: %d", name, resp.StatusCode)
		}
		id := &w9Ident{user: name, xff: xff, session: sessionCookieOf(b, addr)}
		for _, kind := range []string{"api", "rss"} {
			for k := 0; k < w9TokensPerUser; k++ {
				tab, path := "api", "/account/api-tokens"
				if kind == "rss" {
					tab, path = "rss", "/account/rss-tokens"
				}
				b.get("/account?tab="+tab, "X-Forwarded-For", xff)
				if resp := b.post(path, url.Values{"label": {"w9"}, "csrf_token": {b.token()}}, "X-Forwarded-For", xff); resp.StatusCode != 200 {
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

type w9Env struct {
	t         *testing.T
	bin       string
	dsn       string
	outDir    string
	label     string
	webMem    string
	goMem     string
	web       *w9Scoped
	webAddr   string
	ingest    *w9Scoped
	ingAddr   string
	ingTok    string
	pools     *w9Pools
	samp      *w9Sampler
	failed    []string
	producers []*w9Producer
}

func (e *w9Env) startWeb() {
	e.t.Helper()
	e.webAddr = freeLoopbackAddr(e.t)
	env := w9LoadEnv("MAGNETICO_WEB_DATABASE="+e.dsn, "MAGNETICO_WEB_LISTEN="+e.webAddr, "MAGNETICO_WEB_PUBLIC_URL=http://"+e.webAddr,
		"MAGNETICO_WEB_TRUSTED_PROXIES=127.0.0.1/32", "MAGNETICO_WEB_POOL_MAX_CONNS=20", "GOMAXPROCS=2", "GODEBUG=gctrace=1", "GOMEMLIMIT="+e.goMem)
	e.web = startScoped(e.t, "web", e.webMem, 200, env, e.bin, "web")
	e.waitReady("http://" + e.webAddr + "/readyz")
}

func (e *w9Env) startIngest() {
	e.t.Helper()
	e.ingAddr = freeLoopbackAddr(e.t)
	tok := make([]byte, 24)
	for i := range tok {
		tok[i] = byte(rand.IntN(256))
	}
	e.ingTok = hex.EncodeToString(tok)
	env := w9LoadEnv("MAGNETICO_INGEST_TOKEN="+e.ingTok, "MAGNETICO_INGEST_DATABASE="+e.dsn, "MAGNETICO_INGEST_LISTEN="+e.ingAddr, "GOMAXPROCS=2", "GOMEMLIMIT=256MiB")
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

func (e *w9Env) waitReady(u string) {
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

func (e *w9Env) write(kind string, v any) {
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

func (e *w9Env) gate(cond bool, format string, args ...any) {
	if !cond {
		msg := fmt.Sprintf(format, args...)
		e.failed = append(e.failed, msg)
		e.t.Errorf("GATE FAILED: %s", msg)
	}
}

// segment runs one load segment and records and gates what it saw.
func (e *w9Env) segment(name string, conc int, d time.Duration, ingest bool, gates segGates) {
	e.t.Helper()
	acc := newW9Acc()
	load := newW9Load(e.webAddr, e.pools)
	var prod *w9Producer
	if ingest {
		prod = startProducer(e.ingAddr, e.ingTok, w9IngestRate)
	}
	start := time.Now()
	if conc > 0 {
		load.run(conc, d, gates.think, acc)
	} else {
		time.Sleep(d)
	}
	wall := time.Since(start)
	rec := map[string]any{"segment": name, "concurrency": conc, "seconds": wall.Seconds(), "ingest": ingest}
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

// drain stops nothing: it waits for the ingest queue to empty within the deadline
// and then verifies that every acknowledged record is stored.
func (e *w9Env) drain() {
	e.t.Helper()
	start := time.Now()
	conn, err := pgx.Connect(context.Background(), e.dsn)
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var depth int
	for time.Since(start) < w9DrainDeadline {
		_ = conn.QueryRow(context.Background(), `SELECT count(*) FROM magnetico_ingest.queue`).Scan(&depth)
		if depth == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	took := time.Since(start)
	e.gate(depth == 0, "drain: the ingest queue still holds %d rows after %s (deadline %s)", depth, took.Round(time.Second), w9DrainDeadline)
	var quarantined int
	_ = conn.QueryRow(context.Background(), `SELECT count(*) FROM magnetico_ingest.quarantine`).Scan(&quarantined)
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
	missing := 0
	for i := 0; i < len(all); i += 1000 {
		chunk := all[i:min(i+1000, len(all))]
		var found int
		_ = conn.QueryRow(context.Background(), `SELECT count(*) FROM magneticod.torrents WHERE info_hash = ANY($1)`, chunk).Scan(&found)
		missing += len(chunk) - found
	}
	e.gate(missing == 0, "drain: %d of %d acknowledged records are not stored", missing, len(all))
	e.write("drain", map[string]any{"drain_seconds": took.Seconds(), "queue_left": depth, "acked": len(all), "missing": missing, "quarantined": quarantined})
}

// finish stops the services, collects gc and cache information and writes the
// per-process report.
func (e *w9Env) finish(prefix string) {
	e.t.Helper()
	e.samp.finish()
	e.samp.mu.Lock()
	samples, queue := append([]w9Sample(nil), e.samp.samples...), append([]w9Point(nil), e.samp.queue...)
	e.samp.mu.Unlock()
	for _, p := range []*w9Scoped{e.web, e.ingest} {
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

func thin(g []w9GC, n int) []w9GC {
	if len(g) <= n {
		return g
	}
	step := len(g) / n
	var out []w9GC
	for i := 0; i < len(g); i += step {
		out = append(out, g[i])
	}
	return out
}

func thinS(s []w9Sample, n int) []w9Sample {
	if len(s) <= n {
		return s
	}
	step := len(s) / n
	var out []w9Sample
	for i := 0; i < len(s); i += step {
		out = append(out, s[i])
	}
	return out
}

func newW9Env(t *testing.T) *w9Env {
	t.Helper()
	if !benchkit.Enabled() {
		t.Skip("set MAGNETICO_BENCH_LARGE=1 to run the W9 load profiles")
	}
	dsn, out := os.Getenv("MAGNETICO_W9_DSN"), os.Getenv("MAGNETICO_W9_OUT")
	if dsn == "" || out == "" {
		t.Fatal("MAGNETICO_W9_DSN and MAGNETICO_W9_OUT are required")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"build"}
	if tags := os.Getenv("MAGNETICO_W9_TAGS"); tags != "" {
		args = append(args, "-tags", tags)
	}
	bin := filepath.Join(t.TempDir(), "magnetico")
	if b, err := exec.Command("go", append(args, "-o", bin, ".")...).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	e := &w9Env{t: t, bin: bin, dsn: dsn, outDir: out, label: envOr("MAGNETICO_W9_LABEL", "w9"),
		webMem: envOr("MAGNETICO_W9_WEB_MEM", "1G"), goMem: envOr("MAGNETICO_W9_WEB_GOMEMLIMIT", "768MiB")}
	w9Accounts(t, bin, dsn)
	return e
}

// begin starts (or restarts) the services, creates the identities and starts
// sampling.
func (e *w9Env) begin() {
	e.t.Helper()
	e.startIngest()
	e.startWeb()
	e.pools = w9Identities(e.t, e.webAddr, e.dsn)
	e.samp = startSampler([]*w9Scoped{e.web, e.ingest}, e.dsn)
	e.write("start", map[string]any{
		"machine": benchkit.Describe().String(), "web_mem": e.webMem, "web_gomemlimit": e.goMem, "git": gitSHA(),
		"pools": map[string]int{"small": len(e.pools.small), "medium": len(e.pools.medium), "large": len(e.pools.large), "huge": len(e.pools.huge)},
	})
}

func gitSHA() string {
	b, _ := exec.Command("git", "rev-parse", "--short=12", "HEAD").Output()
	return strings.TrimSpace(string(b))
}

func TestW9Load(t *testing.T) {
	e := newW9Env(t)
	profile := envOr("MAGNETICO_W9_PROFILE", "steps")
	stepSec, _ := strconv.Atoi(envOr("MAGNETICO_W9_STEP_SECONDS", "180"))
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
	case "memfloor":
		e.begin()
		e.segment("memfloor-c32", 32, 3*step, false, segGates{noTimeouts: true})
		e.finish("memfloor")
	case "soak":
		scale, _ := strconv.ParseFloat(envOr("MAGNETICO_W9_SOAK_SCALE", "1"), 64)
		dur := func(min float64) time.Duration { return time.Duration(min * scale * float64(time.Minute)) }
		e.begin()
		e.segment("warmup", 16, dur(30), false, segGates{noTimeouts: true, noBusy: true})
		e.segment("steady", 16, dur(90), true, segGates{noTimeouts: true, noBusy: true})
		e.drain()
		e.segment("idle", 0, dur(20), false, segGates{})
		e.finish("soak")
	default:
		t.Fatalf("unknown profile %q", profile)
	}
	if len(e.failed) > 0 {
		t.Fatalf("%d gate(s) failed:\n  %s", len(e.failed), strings.Join(e.failed, "\n  "))
	}
}
