//go:build loadtest

package cli

// Qualification measurements of the statistics subsystem (Task 09, Task 11).
// Nothing here runs in the ordinary suite. Synthetic data, a disposable
// PostgreSQL, loopback only, the real binaries.
//
//	MAGNETICO_STATS_OUT=<dir>            JSON lines are appended to <dir>/<name>.jsonl (required)
//	MAGNETICO_STATS_QUALIFY_SCALE=<f>    multiplies every duration and run count (1 by default)
//
// go test -tags loadtest -run TestQualify<Name> -timeout 120m ./internal/e2e/cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func qualScale() float64 {
	if v, err := strconv.ParseFloat(os.Getenv("MAGNETICO_STATS_QUALIFY_SCALE"), 64); err == nil && v > 0 {
		return v
	}
	return 1
}

func scaled(d time.Duration) time.Duration { return time.Duration(float64(d) * qualScale()) }

// qualOut appends v as one JSON line to <MAGNETICO_STATS_OUT>/<name>.jsonl.
func qualOut(t *testing.T, name string, v any) {
	t.Helper()
	dir := os.Getenv("MAGNETICO_STATS_OUT")
	if dir == "" {
		t.Skip("MAGNETICO_STATS_OUT is not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	f, err := os.OpenFile(filepath.Join(dir, name+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func requireOut(t *testing.T) {
	t.Helper()
	if os.Getenv("MAGNETICO_STATS_OUT") == "" {
		t.Skip("MAGNETICO_STATS_OUT is not set")
	}
}

// ---- process measurements ------------------------------------------------------------

// cpuTicks is utime+stime of a process in clock ticks (100 per second).
func cpuTicks(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
	u, _ := strconv.ParseInt(f[11], 10, 64)
	k, _ := strconv.ParseInt(f[12], 10, 64)
	return u + k
}

func rssKiB(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			n, _ := strconv.Atoi(strings.Fields(v)[0])
			return n
		}
	}
	return 0
}

func median(v []float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)
	if len(s) == 0 {
		return 0
	}
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

// ---- closed-loop load generator with latency capture --------------------------------

type loadGen struct {
	addr, token string
	seq         atomic.Int64
	ok, bad     atomic.Int64
	mu          sync.Mutex
	lat         []float64 // ms
}

func (g *loadGen) run(workers, per int, d time.Duration) (recordsPerSec float64, lat []float64) {
	g.mu.Lock()
	g.lat = nil
	g.mu.Unlock()
	sb := &submitter{addr: g.addr, token: g.token}
	sb.seq.Store(g.seq.Load())
	end := time.Now().Add(d)
	begin := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var mine []float64
			for time.Now().Before(end) {
				t0 := time.Now()
				sb.batch(per)
				mine = append(mine, float64(time.Since(t0).Microseconds())/1000)
			}
			g.mu.Lock()
			g.lat = append(g.lat, mine...)
			g.mu.Unlock()
		}()
	}
	wg.Wait()
	g.seq.Store(sb.seq.Load())
	g.ok.Add(sb.ok.Swap(0))
	g.bad.Add(sb.unavailable.Swap(0) + sb.other.Swap(0) + sb.transport.Swap(0))
	slices.Sort(g.lat)
	return float64(g.ok.Load()) / time.Since(begin).Seconds(), g.lat
}

type runResult struct {
	Cond       string  `json:"cond"`
	Iter       int     `json:"iter"`
	RecPerSec  float64 `json:"records_per_s"`
	CPUUsPerRc float64 `json:"cpu_us_per_record"`
	P50        float64 `json:"p50_ms"`
	P95        float64 `json:"p95_ms"`
	P99        float64 `json:"p99_ms"`
	RSSKiB     int     `json:"rss_kib"`
	OK         int64   `json:"records_ok"`
	Bad        int64   `json:"records_bad"`
}

// measureRun starts a fresh world, warms the ingest up, applies the (optional)
// failure and measures one window.
func measureRun(t *testing.T, cond string, iter int, env []string, apply func(*testing.T, *statsWorld)) runResult {
	s := newStatsWorld(t)
	tok := s.crawler("load")
	s.startIngest(env...)
	g := &loadGen{addr: s.addr, token: tok}
	const workers, per = 8, 50
	g.run(workers, per, scaled(10*time.Second)) // warm-up: connections, caches, JIT-less but page cache
	g.ok.Store(0)
	g.bad.Store(0)
	if apply != nil {
		apply(t, s)
	}
	pid := s.ing.cmd.Process.Pid
	c0 := cpuTicks(pid)
	rate, lat := g.run(workers, per, scaled(60*time.Second))
	c1 := cpuTicks(pid)
	r := runResult{Cond: cond, Iter: iter, RecPerSec: rate, P50: percentile(lat, .5), P95: percentile(lat, .95), P99: percentile(lat, .99),
		RSSKiB: rssKiB(pid), OK: g.ok.Load(), Bad: g.bad.Load()}
	if g.ok.Load() > 0 {
		r.CPUUsPerRc = float64(c1-c0) * 10000 / float64(g.ok.Load()) // ticks (10 ms) → µs per record
	}
	s.stopIngest()
	qualOut(t, "overhead", r)
	t.Logf("%s #%d: %.0f rec/s, %.1f µs CPU/record, p95 %.1f ms, RSS %d MiB, bad %d", cond, iter, rate, r.CPUUsPerRc, r.P95, r.RSSKiB/1024, r.Bad)
	return r
}

// TestQualifyOverheadAndSabotage: instrumentation overhead (enabled vs
// MAGNETICO_INGEST_STATS_DISABLED=true), memory (RSS) and statistics-specific
// sabotage, as the median of alternating runs (5 by default).
func TestQualifyOverheadAndSabotage(t *testing.T) {
	requireOut(t)
	runs := max(1, int(5*qualScale()+0.5))
	if qualScale() >= 1 {
		runs = 5
	}
	res := map[string][]runResult{}
	conds := []struct {
		name string
		env  []string
		sab  *sabotage
	}{{"disabled", []string{"MAGNETICO_INGEST_STATS_DISABLED=true"}, nil}, {"enabled", nil, nil}}
	for _, sb := range sabotageCases() {
		conds = append(conds, struct {
			name string
			env  []string
			sab  *sabotage
		}{"sabotage: " + sb.name, nil, &sb})
	}
	for i := 1; i <= runs; i++ {
		// The order rotates with the iteration so that a position in the sequence
		// (a cold page cache, a vacuum from the previous run) favours no condition.
		for k := range conds {
			c := conds[(k+i-1)%len(conds)]
			t.Run(fmt.Sprintf("%s/%d", c.name, i), func(t *testing.T) {
				var apply func(*testing.T, *statsWorld)
				if c.sab != nil {
					apply = c.sab.apply
				}
				res[c.name] = append(res[c.name], measureRun(t, c.name, i, c.env, apply))
			})
		}
	}
	med := func(cond string, f func(runResult) float64) float64 {
		var v []float64
		for _, r := range res[cond] {
			v = append(v, f(r))
		}
		return median(v)
	}
	rate := func(r runResult) float64 { return r.RecPerSec }
	cpu := func(r runResult) float64 { return r.CPUUsPerRc }
	p95 := func(r runResult) float64 { return r.P95 }
	p99 := func(r runResult) float64 { return r.P99 }
	rss := func(r runResult) float64 { return float64(r.RSSKiB) }
	base := "disabled"
	summary := map[string]any{"runs": runs}
	for _, c := range conds[1:] {
		summary[c.name] = map[string]float64{
			"rate_ratio": med(c.name, rate) / med(base, rate), "cpu_ratio": med(c.name, cpu) / med(base, cpu),
			"p95_ratio": med(c.name, p95) / med(base, p95), "p99_ratio": med(c.name, p99) / med(base, p99),
			"rss_delta_mib": (med(c.name, rss) - med(base, rss)) / 1024,
		}
	}
	qualOut(t, "overhead_summary", summary)
	e := summary["enabled"].(map[string]float64)
	if e["rate_ratio"] < 0.97 {
		t.Errorf("throughput with statistics is %.1f%% of the baseline, want ≥ 97%%", e["rate_ratio"]*100)
	}
	if e["cpu_ratio"] > 1.05 {
		t.Errorf("CPU per record is +%.1f%%, want ≤ +5%%", (e["cpu_ratio"]-1)*100)
	}
	if e["p95_ratio"] > 1.10 || e["p99_ratio"] > 1.20 {
		t.Errorf("latency p95 %+.1f%%, p99 %+.1f%%, want ≤ +10%% / +20%%", (e["p95_ratio"]-1)*100, (e["p99_ratio"]-1)*100)
	}
	for _, c := range conds[2:] {
		m := summary[c.name].(map[string]float64)
		if m["rate_ratio"] < 0.9 {
			t.Errorf("%s: admission at %.1f%% of the baseline, want ≥ 90%%", c.name, m["rate_ratio"]*100)
		}
		for _, r := range res[c.name] {
			if r.Bad != 0 {
				t.Errorf("%s run %d: %d records refused or failed", c.name, r.Iter, r.Bad)
			}
		}
	}
}

// TestQualifySteadyStateMemory: the RSS of the ingest process at a production-like
// rate (200 records/s) with and without statistics, as the median of alternating
// runs: the statistics may add at most 16 MiB. (At the saturating rate of the
// overhead test the RSS varies by ±100 MiB with garbage-collection timing and
// says nothing about the statistics.)
func TestQualifySteadyStateMemory(t *testing.T) {
	requireOut(t)
	runs := 5
	if qualScale() < 1 {
		runs = 1
	}
	res := map[string][]float64{}
	for i := 1; i <= runs; i++ {
		for k := range 2 {
			cond := []string{"disabled", "enabled"}[(k+i-1)%2]
			t.Run(fmt.Sprintf("%s/%d", cond, i), func(t *testing.T) {
				s := newStatsWorld(t)
				tok := s.crawler("load")
				// The recommended production setting (docs/container.md): a soft heap target, the same for both conditions.
				env := []string{"GOMEMLIMIT=256MiB"}
				if cond == "disabled" {
					env = append(env, "MAGNETICO_INGEST_STATS_DISABLED=true")
				}
				s.startIngest(env...)
				sb := &submitter{addr: s.addr, token: tok}
				sb.paced(scaled(50*time.Second), 20, 100*time.Millisecond, nil)
				// The median of ten samples over the last 20 s: a single reading moves by tens of MiB with garbage-collection timing.
				var samples []float64
				for i := 0; i < 10; i++ {
					sb.paced(scaled(2*time.Second), 20, 100*time.Millisecond, nil)
					samples = append(samples, float64(rssKiB(s.ing.cmd.Process.Pid)))
				}
				rss := int(median(samples))
				res[cond] = append(res[cond], float64(rss)/1024)
				qualOut(t, "memory", map[string]any{"cond": cond, "iter": i, "rss_mib": float64(rss) / 1024, "records": sb.ok.Load()})
				t.Logf("%s #%d: RSS %.1f MiB after %d records", cond, i, float64(rss)/1024, sb.ok.Load())
				s.stopIngest()
			})
		}
	}
	d := median(res["enabled"]) - median(res["disabled"])
	qualOut(t, "memory_summary", map[string]any{"disabled_mib": median(res["disabled"]), "enabled_mib": median(res["enabled"]), "delta_mib": d})
	if d > 16 {
		t.Errorf("statistics add %.1f MiB of RSS, want ≤ 16 MiB", d)
	}
}

// ---- delivery batching ----------------------------------------------------------------

// countingProxy sits between a crawler client and the ingest service: it counts
// requests per path and connections, reads the creation stamps of the records in
// every submit request (crawlerd -stamp) and times them to the acknowledgement.
type countingProxy struct {
	addr   string
	target string
	srv    *http.Server
	ln     net.Listener
	client *http.Client
	down   atomic.Bool

	mu        sync.Mutex
	paths     map[string]int
	submitRec []int     // records per submit request
	submitAge []float64 // ms between the oldest stamp in a request and its arrival
	latency   []float64 // ms from a record's creation to its acknowledgement
	conns     int
	first     time.Time
	last      time.Time
	restored  time.Time // set by a scenario that takes the link down and up again
	firstUp   time.Time // the first successful submit after restored
}

var stampRe = regexp.MustCompile(`"name":"e2e torrent \d+ @(\d+)"`)

func newCountingProxy(t *testing.T, target string) *countingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &countingProxy{ln: ln, addr: ln.Addr().String(), target: target, paths: map[string]int{},
		client: &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{MaxIdleConnsPerHost: 8}}}
	p.srv = &http.Server{Handler: http.HandlerFunc(p.handle), ReadHeaderTimeout: 10 * time.Second,
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateNew {
				p.mu.Lock()
				p.conns++
				p.mu.Unlock()
			}
		}}
	go func() { _ = p.srv.Serve(ln) }()
	t.Cleanup(func() { _ = p.srv.Close() })
	return p
}

func (p *countingProxy) handle(w http.ResponseWriter, r *http.Request) {
	if p.down.Load() {
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				_ = c.Close() // a refused or dropped connection
				return
			}
		}
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	arrival := time.Now()
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	var stamps []int64
	if r.URL.Path == "/api/ingest/v1/torrents" {
		for _, m := range stampRe.FindAllSubmatch(body, -1) {
			ns, _ := strconv.ParseInt(string(m[1]), 10, 64)
			stamps = append(stamps, ns)
		}
	}
	req, _ := http.NewRequest(r.Method, "http://"+p.target+r.URL.Path, bytes.NewReader(body))
	req.Header = r.Header.Clone()
	resp, err := p.client.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	ackAt := time.Now()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paths[r.URL.Path]++
	if p.first.IsZero() {
		p.first = arrival
	}
	p.last = ackAt
	if r.URL.Path == "/api/ingest/v1/torrents" && resp.StatusCode/100 == 2 {
		if !p.restored.IsZero() && p.firstUp.IsZero() {
			p.firstUp = arrival
		}
		var n struct {
			Records []json.RawMessage `json:"records"`
		}
		_ = json.Unmarshal(body, &n)
		p.submitRec = append(p.submitRec, len(n.Records))
		oldest := int64(1<<62 - 1)
		for _, ns := range stamps {
			p.latency = append(p.latency, float64(ackAt.UnixNano()-ns)/1e6)
			oldest = min(oldest, ns)
		}
		if len(stamps) > 0 {
			p.submitAge = append(p.submitAge, float64(arrival.UnixNano()-oldest)/1e6)
		}
	}
}

type batchResult struct {
	Variant, Scenario              string
	SubmitRequests, ExistsRequests int
	EmptySubmits                   int
	MeanRecordsPerSubmit           float64
	SubmitPerSec, ExistsPerSec     float64
	LatencyP50, LatencyP95         float64
	FirstRequestAgeMs              float64
	Connections                    int
	ConnectionReuse                float64
	RecoverySeconds                float64 // link back → last acknowledgement (includes the sender's retry backoff wait)
	DrainSeconds                   float64 // first successful submit after the link came back → last acknowledgement
	Records                        int
	Seconds                        float64
}

func (p *countingProxy) result(variant, scenario string, wall time.Duration) batchResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := batchResult{Variant: variant, Scenario: scenario, SubmitRequests: p.paths["/api/ingest/v1/torrents"], ExistsRequests: p.paths["/api/ingest/v1/torrents/exists"],
		Connections: p.conns, Seconds: wall.Seconds()}
	total := 0
	for _, n := range p.submitRec {
		total += n
		if n == 0 {
			r.EmptySubmits++
		}
	}
	r.Records = total
	if len(p.submitRec) > 0 {
		r.MeanRecordsPerSubmit = float64(total) / float64(len(p.submitRec))
	}
	if sec := p.last.Sub(p.first).Seconds(); sec > 0 {
		r.SubmitPerSec, r.ExistsPerSec = float64(r.SubmitRequests)/sec, float64(r.ExistsRequests)/sec
	}
	lat := slices.Clone(p.latency)
	slices.Sort(lat)
	r.LatencyP50, r.LatencyP95 = percentile(lat, .5), percentile(lat, .95)
	if !p.restored.IsZero() {
		r.RecoverySeconds = p.last.Sub(p.restored).Seconds()
		if !p.firstUp.IsZero() {
			r.DrainSeconds = p.last.Sub(p.firstUp).Seconds()
		}
	}
	if len(p.submitAge) > 0 {
		r.FirstRequestAgeMs = p.submitAge[0]
	}
	if reqs := r.SubmitRequests + r.ExistsRequests; reqs > 0 {
		r.ConnectionReuse = 1 - float64(r.Connections)/float64(reqs)
	}
	return r
}

// crawlerBinaries is the crawler client under test (the current build) and, when
// MAGNETICO_STATS_BASELINE_CRAWLERD names one, the pre-batching client built from
// the commit before the delivery changes.
func crawlerBinaries(t *testing.T) map[string]string {
	t.Helper()
	m := map[string]string{"batching": buildCrawlerd(t)}
	if b := os.Getenv("MAGNETICO_STATS_BASELINE_CRAWLERD"); b != "" {
		m["baseline"] = b
	}
	return m
}

// batchRun runs one crawler client through the counting proxy against a fresh
// ingest and returns the proxy's view. during runs while the client works.
func batchRun(t *testing.T, bin, variant, scenario string, from int, args []string, during func(p *countingProxy)) (batchResult, string) {
	s := newStatsWorld(t)
	tok := s.crawler("batch")
	s.startIngest()
	p := newCountingProxy(t, s.addr)
	begin := time.Now()
	cr := harnessStart(t, bin, []string{"MAGNETICO_CRAWL_INGEST_TOKEN=" + tok},
		append([]string{"-queue-dir", t.TempDir(), "-url", "http://" + p.addr, "-from", strconv.Itoa(from), "-stamp"}, args...)...)
	if during != nil {
		go during(p)
	}
	select {
	case err := <-cr.exited:
		if err != nil {
			t.Fatalf("%s crawler: %v\n%s", variant, err, cr.out.String())
		}
	case <-time.After(15 * time.Minute):
		t.Fatalf("%s crawler timed out:\n%s", variant, cr.out.String())
	}
	wall := time.Since(begin)
	s.stopIngest()
	r := p.result(variant, scenario, wall)
	qualOut(t, "batching", r)
	t.Logf("%s/%s: %+v", scenario, variant, r)
	return r, cr.out.String()
}

func TestQualifyBatching(t *testing.T) {
	requireOut(t)
	bins := crawlerBinaries(t)
	order := []string{"batching", "baseline"}
	res := map[string]map[string]batchResult{}
	for _, sc := range []string{"sparse", "does-not-fit", "sustained", "recovery"} {
		res[sc] = map[string]batchResult{}
	}
	for _, v := range order {
		bin, ok := bins[v]
		if !ok {
			continue
		}
		t.Run("sparse/"+v, func(t *testing.T) { // ≈ 1 record per 3 s
			res["sparse"][v], _ = batchRun(t, bin, v, "sparse", 1, []string{"-count", "12", "-rate", "0.3333"}, nil)
		})
		t.Run("does-not-fit/"+v, func(t *testing.T) { // 5 MB records: the next one never fits behind the previous
			res["does-not-fit"][v], _ = batchRun(t, bin, v, "does-not-fit", 100, []string{"-count", "2", "-rate", "0", "-big", "5000000:1,5000000:1"}, nil)
		})
		t.Run("sustained/"+v, func(t *testing.T) {
			// 150 s: at one request per second a 60 s window has too few requests for
			// "99 % reuse" to be reachable with even a single connection (59/60).
			n := max(int(30000*qualScale()), 600)
			args := []string{"-count", strconv.Itoa(n), "-rate", "200"}
			if v == "batching" { // the exists endpoint: the real prechecker fed at 200 discoveries/s
				args = append(args, "-precheck-rate", "200", "-precheck-count", strconv.Itoa(n))
			}
			res["sustained"][v], _ = batchRun(t, bin, v, "sustained", 1000, args, nil)
		})
		t.Run("recovery/"+v, func(t *testing.T) { // a 60 s outage; production ends when the link returns, so what follows is pure backlog
			outage := scaled(60 * time.Second)
			count := int(200 * (10 + outage.Seconds()))
			var reps []batchResult
			for i := 0; i < 3; i++ { // the retry backoff is jittered: its phase at the moment the link returns is luck
				r, _ := batchRun(t, bin, v, "recovery", 100000+i*100000, []string{"-count", strconv.Itoa(count), "-rate", "200"}, func(p *countingProxy) {
					time.Sleep(10 * time.Second)
					p.down.Store(true)
					time.Sleep(outage)
					p.down.Store(false)
					p.mu.Lock()
					p.restored = time.Now()
					p.mu.Unlock()
				})
				reps = append(reps, r)
			}
			slices.SortFunc(reps, func(a, b batchResult) int { return int(a.DrainSeconds*1000) - int(b.DrainSeconds*1000) })
			res["recovery"][v] = reps[1] // the median drain
		})
	}
	n, ok := res["sustained"]["batching"]
	if !ok {
		return
	}
	if sp := res["sparse"]["batching"]; sp.EmptySubmits != 0 || sp.SubmitRequests > 12 || sp.MeanRecordsPerSubmit > 1.01 {
		t.Errorf("sparse traffic: %+v, want one request per record and no empty request", sp)
	}
	if nf := res["does-not-fit"]["batching"]; nf.FirstRequestAgeMs > 700 {
		t.Errorf("a next record that does not fit: the first request left %.0f ms after its oldest record, want an immediate dispatch (< 700 ms, the linger is 1,000 ms)", nf.FirstRequestAgeMs)
	}
	if n.SubmitPerSec < 0.8 || n.SubmitPerSec > 1.2 || n.ExistsPerSec < 0.8 || n.ExistsPerSec > 1.2 || n.MeanRecordsPerSubmit < 100 {
		t.Errorf("sustained 200 records/s: %.2f submit/s, %.2f exists/s, %.0f records/request; want 0.8–1.2 requests/s and ≥ 100 records/request", n.SubmitPerSec, n.ExistsPerSec, n.MeanRecordsPerSubmit)
	}
	if n.LatencyP95 > 2500 {
		t.Errorf("enqueue→ack latency p95 %.0f ms, want ≤ 2,500 ms", n.LatencyP95)
	}
	if n.ConnectionReuse < 0.99 {
		t.Errorf("connection reuse %.1f%%, want ≥ 99%%", n.ConnectionReuse*100)
	}
	if b, ok := res["recovery"]["baseline"]; ok && res["recovery"]["batching"].DrainSeconds > b.DrainSeconds*1.05+1 {
		t.Errorf("draining the backlog took %.1f s, the baseline %.1f s", res["recovery"]["batching"].DrainSeconds, b.DrainSeconds)
	}
}

// ---- statistics write frequency ---------------------------------------------------------

// tupleWrites is inserts+updates per statistics table (pg_stat_user_tables).
func (s *statsWorld) tupleWrites() map[string]int64 {
	s.t.Helper()
	time.Sleep(3 * time.Second) // backends report their counters within about a second of going idle
	c, err := pgx.Connect(context.Background(), s.dsn)
	if err != nil {
		s.t.Fatal(err)
	}
	defer c.Close(context.Background())
	rows, err := c.Query(context.Background(), `SELECT relname, n_tup_ins + n_tup_upd FROM pg_stat_user_tables WHERE schemaname = 'magnetico_stats'`)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var n string
		var v int64
		if err := rows.Scan(&n, &v); err != nil {
			s.t.Fatal(err)
		}
		out[n] = v
	}
	return out
}

func (s *statsWorld) clientConnections() int {
	s.t.Helper()
	u, _ := url.Parse(s.dsn)
	return s.n(`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND backend_type = 'client backend' AND pid <> pg_backend_pid()`, strings.TrimPrefix(u.Path, "/"))
}

// TestQualifyWriteFrequency counts what the statistics write per minute at the
// production-like rate and at ten times that rate: the count must not follow the
// load.
func TestQualifyWriteFrequency(t *testing.T) {
	requireOut(t)
	window := scaled(180 * time.Second)
	type perMin struct{ Flush, InstanceRow, Snapshot, Shared, Hour, Day, Month, Activity, Storage float64 }
	got := map[string]perMin{}
	for _, load := range []struct {
		name string
		per  int
		tick time.Duration
	}{{"1x", 20, 100 * time.Millisecond}, {"10x", 100, 50 * time.Millisecond}} {
		t.Run(load.name, func(t *testing.T) {
			s := newStatsWorld(t)
			tok := s.crawler("load")
			s.startIngest()
			sb := &submitter{addr: s.addr, token: tok}
			sb.paced(scaled(20*time.Second), load.per, load.tick, nil)
			before := s.tupleWrites()
			sb.paced(window, load.per, load.tick, nil)
			after := s.tupleWrites()
			d := func(tbl string) float64 { return float64(after[tbl]-before[tbl]) / window.Minutes() }
			// One flush transaction upserts one hour row (two at an hour boundary); the instance row also carries the heartbeat time.
			p := perMin{Flush: d("agg_hour"), InstanceRow: d("ingest_instances"), Snapshot: d("snapshot"), Shared: d("shared_state"), Hour: d("agg_hour"), Day: d("agg_day"), Month: d("agg_month"),
				Activity: d("crawler_activity"), Storage: d("storage_hour")}
			got[load.name] = p
			conns := s.clientConnections()
			qualOut(t, "write_frequency", map[string]any{"load": load.name, "records_per_s": float64(sb.ok.Load()) / (window.Seconds() + scaled(20*time.Second).Seconds()),
				"per_minute": p, "client_connections": conns, "window_s": window.Seconds()})
			t.Logf("%s: per minute %+v, %d client connections", load.name, p, conns)
			s.stopIngest()
		})
	}
	one, ten := got["1x"], got["10x"]
	slack := 1.0/window.Minutes() + 0.01 // one write more or less per window
	if one.Flush > 1+slack || one.Snapshot > 6+slack*2 || one.Shared > 6+slack*2 || one.Flush+one.Snapshot+one.Shared > 14+slack*3 {
		t.Errorf("write frequency at 1x: %+v, want ≤ 1 flush, ≤ 6 snapshot upserts, ≤ 6 shared_state upserts (≤ 14 transactions) per minute", one)
	}
	for _, c := range [][2]float64{{one.Flush, ten.Flush}, {one.Snapshot, ten.Snapshot}, {one.Shared, ten.Shared}, {one.Hour, ten.Hour}, {one.Activity, ten.Activity}} {
		if c[1] > c[0]+slack*2 {
			t.Errorf("the write count follows the load: %+v at 1x, %+v at 10x", one, ten)
			break
		}
	}
	// Connections: three more than with the statistics disabled, at most.
	var conns [2]int
	for i, env := range [][]string{nil, {"MAGNETICO_INGEST_STATS_DISABLED=true"}} {
		t.Run(fmt.Sprintf("connections/%d", i), func(t *testing.T) {
			s := newStatsWorld(t)
			tok := s.crawler("load")
			s.startIngest(env...)
			sb := &submitter{addr: s.addr, token: tok}
			sb.paced(scaled(15*time.Second), 20, 50*time.Millisecond, nil)
			conns[i] = s.clientConnections()
			s.stopIngest()
		})
	}
	qualOut(t, "write_frequency", map[string]any{"connections_enabled": conns[0], "connections_disabled": conns[1]})
	if conns[0]-conns[1] > 3 {
		t.Errorf("%d connections with the statistics, %d without: more than 3 added", conns[0], conns[1])
	}
}

// ---- authentication under a flood of invalid credentials -------------------------------------

func randomToken() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	b := make([]byte, 43)
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return "mgc_" + string(b)
}

func TestQualifyAuthFlood(t *testing.T) {
	requireOut(t)
	s := newStatsWorld(t)
	good := s.crawler("good")
	later := s.crawler("later")
	s.startIngest()
	if st := existsStatus(s.addr, good); st != 200 {
		t.Fatalf("warm-up: %d", st)
	}
	u, _ := url.Parse(s.dsn)
	db := strings.TrimPrefix(u.Path, "/")
	dur := scaled(30 * time.Second)
	stop := make(chan struct{})
	time.AfterFunc(dur, func() { close(stop) })
	var flood, floodOK, flood401, flood503, floodOther atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 256; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				flood.Add(1)
				switch st := existsStatus(s.addr, randomToken()); st {
				case 200:
					floodOK.Add(1)
				case 401:
					flood401.Add(1)
				case 503:
					flood503.Add(1)
				default:
					floodOther.Add(1)
				}
			}
		}()
	}
	var maxLookups atomic.Int64
	wg.Add(1)
	go func() { // concurrent credential lookups, as the database sees them
		defer wg.Done()
		c, err := pgx.Connect(context.Background(), s.dsn)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			var n int64
			if err := c.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND state = 'active' AND query LIKE '%magnetico_ingest.crawlers%' AND query NOT LIKE '%pg_stat_activity%'`, db).Scan(&n); err == nil && n > maxLookups.Load() {
				maxLookups.Store(n)
			}
		}
	}()
	var probeBad atomic.Int64
	var probeLat []float64
	wg.Add(1)
	go func() { // a legitimate, cached credential keeps working throughout
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			t0 := time.Now()
			if existsStatus(s.addr, good) != 200 {
				probeBad.Add(1)
			}
			probeLat = append(probeLat, float64(time.Since(t0).Microseconds())/1000)
		}
	}()
	wg.Wait()
	time.Sleep(2 * time.Second)
	// The state is empty afterwards: a credential never seen before authenticates at once.
	t0 := time.Now()
	afterStatus := existsStatus(s.addr, later)
	afterLat := time.Since(t0)
	slices.Sort(probeLat)
	s.stopIngest()
	res := map[string]any{"flood_requests": flood.Load(), "flood_401": flood401.Load(), "flood_503": flood503.Load(), "flood_200": floodOK.Load(), "flood_other": floodOther.Load(),
		"max_concurrent_lookups": maxLookups.Load(), "probe_failures": probeBad.Load(), "probe_p95_ms": percentile(probeLat, .95), "probe_max_ms": percentile(probeLat, 1),
		"after_flood_status": afterStatus, "after_flood_ms": afterLat.Milliseconds(),
		"persisted_rejected_auth": s.sum("rejected_auth"), "persisted_auth_busy": s.sum("auth_busy"), "persisted_auth_db_lookups": s.sum("auth_db_lookups"),
		"persisted_auth_storage_failures": s.sum("auth_storage_failures"), "persisted_rejected_unavailable_auth": s.sum("rejected_unavailable_auth"), "duration_s": dur.Seconds()}
	qualOut(t, "auth_flood", res)
	t.Logf("%+v", res)
	if maxLookups.Load() > 8 {
		t.Errorf("%d concurrent credential lookups, want ≤ 8", maxLookups.Load())
	}
	if floodOK.Load() != 0 || floodOther.Load() != 0 {
		t.Errorf("a forged credential was accepted (%d) or got an unexpected status (%d)", floodOK.Load(), floodOther.Load())
	}
	if probeBad.Load() != 0 {
		t.Errorf("%d requests with a legitimate cached credential failed during the flood", probeBad.Load())
	}
	if afterStatus != 200 || afterLat > 500*time.Millisecond {
		t.Errorf("after the flood a new credential got %d in %v: the coordination state was not released", afterStatus, afterLat)
	}
	if s.sum("auth_storage_failures") != 0 || s.sum("rejected_unavailable_auth") != s.sum("auth_busy") {
		t.Errorf("the flood made a healthy database look unavailable: %d storage failures; %d unavailable refusals of which %d were load shedding",
			s.sum("auth_storage_failures"), s.sum("rejected_unavailable_auth"), s.sum("auth_busy"))
	}
}

// ---- admission while the queue is large -------------------------------------------------------

// TestQualifyQueueSamplingAdmissionInflation: admission p95 with the statistics
// (the queue sampler counting 1 M queued rows every 10 s) against the statistics
// disabled, on the same 1 M-row queue.
func TestQualifyQueueSamplingAdmissionInflation(t *testing.T) {
	requireOut(t)
	rows := max(int(1_000_000*qualScale()), 50_000)
	pairs := 3
	if qualScale() < 1 {
		pairs = 1
	}
	p95 := map[string][]float64{}
	rate := map[string][]float64{}
	for i := 1; i <= pairs; i++ {
		for k := 0; k < 2; k++ {
			cond := []string{"disabled", "enabled"}[(k+i-1)%2]
			t.Run(fmt.Sprintf("%s/%d", cond, i), func(t *testing.T) {
				s := newStatsWorld(t)
				tok := s.crawler("load")
				// Rows the consumer leaves alone (due in 30 days): a large, static backlog.
				sqlExec(t, s.dsn, fmt.Sprintf(`INSERT INTO magnetico_ingest.queue (info_hash, format, payload, payload_bytes, next_attempt_at)
SELECT decode(lpad(to_hex(g + 100000000), 40, '0'), 'hex'), 1, decode(repeat('ab', 300), 'hex'), 300, now() + interval '30 days' FROM generate_series(1, %d) g`, rows))
				sqlExec(t, s.dsn, `VACUUM ANALYZE magnetico_ingest.queue`)
				var env []string
				if cond == "disabled" {
					env = []string{"MAGNETICO_INGEST_STATS_DISABLED=true"}
				}
				s.startIngest(env...)
				g := &loadGen{addr: s.addr, token: tok}
				g.run(8, 50, scaled(10*time.Second))
				g.ok.Store(0)
				r, lat := g.run(8, 50, scaled(90*time.Second))
				p95[cond] = append(p95[cond], percentile(lat, .95))
				rate[cond] = append(rate[cond], r)
				qualOut(t, "queue_admission", map[string]any{"cond": cond, "iter": i, "rows": rows, "records_per_s": r, "p50_ms": percentile(lat, .5), "p95_ms": percentile(lat, .95), "p99_ms": percentile(lat, .99), "bad": g.bad.Load()})
				t.Logf("%s #%d: %.0f rec/s, p95 %.1f ms with %d queued rows", cond, i, r, percentile(lat, .95), rows)
				s.stopIngest()
			})
		}
	}
	ratio := median(p95["enabled"]) / median(p95["disabled"])
	qualOut(t, "queue_admission_summary", map[string]any{"rows": rows, "p95_ratio": ratio, "rate_ratio": median(rate["enabled"]) / median(rate["disabled"])})
	if ratio > 1.10 {
		t.Errorf("admission p95 is +%.1f%% with the sampler at %d queued rows, want ≤ +10%%", (ratio-1)*100, rows)
	}
}

// ---- storage growth ---------------------------------------------------------------------------

// genHistory inserts history rows for [lo, hi) (UTC) into every aggregate table.
func genHistory(t *testing.T, dsn string, lo, hi time.Time) {
	t.Helper()
	l, h := lo.UTC().Format("2006-01-02 15:04:05"), hi.UTC().Format("2006-01-02 15:04:05")
	for _, q := range []string{
		`INSERT INTO magnetico_stats.agg_hour (bucket_start, submit_requests, records_received, records_enqueued, records_acknowledged, bytes_body, bytes_accepted, exists_requests, exists_hashes,
   settled_stored_new, torrents_created, processing_attempts, processing_ms_sum, processing_ms_count, queue_rows_sum, queue_rows_n, queue_rows_min, queue_rows_max, queue_rows_last, quota_used_bytes_sum, quota_used_bytes_n, covered_mask)
 SELECT g AT TIME ZONE 'UTC', 3600, 720000 + (extract(epoch from g)::bigint %% 997), 700000, 719000, 150000000, 140000000, 3600, 3600000, 700000, 650000, 700000, 9000000, 700000,
   5000 * 360, 360, 100, 9000, 4000, 2000000000, 360, (1::bigint << 60) - 1
 FROM generate_series(date_trunc('hour', $1::timestamp), $2::timestamp - interval '1 hour', interval '1 hour') g`,
		`INSERT INTO magnetico_stats.agg_day (bucket_start, submit_requests, records_received, records_enqueued, records_acknowledged, bytes_body, bytes_accepted, exists_requests, exists_hashes,
   settled_stored_new, torrents_created, processing_attempts, processing_ms_sum, processing_ms_count, queue_rows_sum, queue_rows_n, queue_rows_min, queue_rows_max, queue_rows_last, quota_used_bytes_sum, quota_used_bytes_n, covered_minutes)
 SELECT g AT TIME ZONE 'UTC', 86400, 17280000 + (extract(epoch from g)::bigint %% 997), 16800000, 17256000, 3600000000, 3360000000, 86400, 86400000, 16800000, 15600000, 16800000, 216000000, 16800000,
   5000 * 8640, 8640, 100, 9000, 4000, 48000000000, 8640, 1440
 FROM generate_series(date_trunc('day', $1::timestamp), $2::timestamp - interval '1 day', interval '1 day') g`,
		`INSERT INTO magnetico_stats.agg_month (bucket_start, submit_requests, records_received, records_enqueued, records_acknowledged, bytes_body, bytes_accepted, exists_requests, exists_hashes,
   settled_stored_new, torrents_created, processing_attempts, processing_ms_sum, processing_ms_count, queue_rows_sum, queue_rows_n, queue_rows_min, queue_rows_max, queue_rows_last, quota_used_bytes_sum, quota_used_bytes_n, covered_minutes)
 SELECT g AT TIME ZONE 'UTC', 2592000, 518400000, 504000000, 517680000, 108000000000, 100800000000, 2592000, 2592000000, 504000000, 468000000, 504000000, 6480000000, 504000000,
   5000 * 259200, 259200, 100, 9000, 4000, 1440000000000, 259200, 43200
 FROM generate_series(date_trunc('month', $1::timestamp), $2::timestamp - interval '1 month', interval '1 month') g`,
	} {
		sqlExec(t, dsn, strings.ReplaceAll(q, "%%", "%"), l, h)
	}
	for _, unit := range []string{"hour", "day", "month"} {
		agg := map[string]string{"hour": "1 hour", "day": "1 day", "month": "1 month"}[unit]
		sqlExec(t, dsn, `INSERT INTO magnetico_stats.storage_`+unit+` (bucket_start, torrents_table_bytes_last, torrents_table_bytes_max, torrents_table_bytes_sum, torrents_table_bytes_n,
   blobs_table_bytes_last, blobs_table_bytes_max, indexes_bytes_last, indexes_bytes_max, database_total_bytes_last, database_total_bytes_max, samples, last_sample_at)
 SELECT g AT TIME ZONE 'UTC', 9000000000, 9000000000, 9000000000 * 12, 12, 40000000000, 40000000000, 20000000000, 20000000000, 80000000000, 80000000000, 12, g AT TIME ZONE 'UTC'
 FROM generate_series(date_trunc('`+unit+`', $1::timestamp), $2::timestamp - interval '`+agg+`', interval '`+agg+`') g`, l, h)
	}
}

type relSizes struct {
	Table, Indexes, Total int64
	Rows                  int64
}

func (s *statsWorld) sizes() map[string]relSizes {
	s.t.Helper()
	sqlExec(s.t, s.dsn, `VACUUM (ANALYZE) magnetico_stats.agg_hour, magnetico_stats.agg_day, magnetico_stats.agg_month, magnetico_stats.storage_hour, magnetico_stats.storage_day, magnetico_stats.storage_month`)
	c, err := pgx.Connect(context.Background(), s.dsn)
	if err != nil {
		s.t.Fatal(err)
	}
	defer c.Close(context.Background())
	rows, err := c.Query(context.Background(), `SELECT c.relname, pg_relation_size(c.oid), pg_indexes_size(c.oid), pg_total_relation_size(c.oid), c.reltuples::bigint
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'magnetico_stats' AND c.relkind = 'r' ORDER BY 1`)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]relSizes{}
	for rows.Next() {
		var n string
		var r relSizes
		if err := rows.Scan(&n, &r.Table, &r.Indexes, &r.Total, &r.Rows); err != nil {
			s.t.Fatal(err)
		}
		out[n] = r
	}
	return out
}

// TestQualifyStorageGrowth generates 1, 5 and 10 years of aggregates, measures
// every statistics relation with the size functions and validates the linear
// extrapolation from one year against the measured five and ten.
func TestQualifyStorageGrowth(t *testing.T) {
	requireOut(t)
	s := newStatsWorld(t)
	now := time.Now().UTC().Truncate(time.Hour)
	totals := map[int]int64{}
	perTable := map[int]map[string]relSizes{}
	prev := 0
	for _, years := range []int{1, 5, 10} {
		for y := prev + 1; y <= years; y++ {
			genHistory(t, s.dsn, now.AddDate(-y, 0, 0), now.AddDate(-(y-1), 0, 0))
		}
		prev = years
		sz := s.sizes()
		perTable[years] = sz
		var total int64
		for _, r := range sz {
			total += r.Total
		}
		totals[years] = total
		qualOut(t, "storage_growth", map[string]any{"years": years, "total_bytes": total, "tables": sz,
			"measured_total_database_bytes": s.n(`SELECT pg_database_size(current_database())`)})
		t.Logf("%d years: %.1f MiB", years, float64(total)/(1<<20))
	}
	// Linear extrapolation from the first year, validated against the measurement.
	for _, years := range []int{5, 10} {
		est := float64(totals[1]) * float64(years)
		errPct := (est - float64(totals[years])) / float64(totals[years]) * 100
		qualOut(t, "storage_growth", map[string]any{"years": years, "extrapolated_from_1y_bytes": est, "measured_bytes": totals[years], "error_percent": errPct})
		if errPct > 10 || errPct < -10 {
			t.Errorf("the extrapolation from one year to %d years is off by %.1f%%", years, errPct)
		}
	}
	// Per-crawler activity: 10,000 registered crawlers.
	sqlExec(t, s.dsn, `INSERT INTO magnetico_stats.crawler_activity (crawler_id, first_seen_at, last_seen_at, last_ip, last_submit_at, records_received, records_enqueued, records_duplicate, bytes_body, exists_requests, exists_hashes, rejected_requests)
 SELECT gen_random_uuid(), now(), now(), '198.51.100.7', now(), 123456789, 123000000, 456789, 9876543210, 1234567, 123456789, 12 FROM generate_series(1, 10000)`)
	sqlExec(t, s.dsn, `VACUUM (ANALYZE) magnetico_stats.crawler_activity`)
	ca := s.sizes()["crawler_activity"]
	qualOut(t, "storage_growth", map[string]any{"crawler_activity_10000_rows": ca})
	// Update churn: one month of flushes (43,200 upserts of the newest month row), each in its own
	// transaction as a flush is, with autovacuum left to do what it does.
	before := s.sizes()
	c, err := pgx.Connect(context.Background(), s.dsn)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 43200; i++ {
		if _, err := c.Exec(context.Background(), `UPDATE magnetico_stats.agg_month SET records_received = records_received + 1 WHERE bucket_start = (SELECT max(bucket_start) FROM magnetico_stats.agg_month)`); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Close(context.Background())
	after := s.sizes()
	qualOut(t, "storage_growth", map[string]any{"month_row_43200_separate_updates_table_bytes_before": before["agg_month"].Table, "after": after["agg_month"].Table})
	if after["agg_month"].Table > before["agg_month"].Table+1<<20 {
		t.Errorf("one month of upserts grew agg_month from %d to %d bytes", before["agg_month"].Table, after["agg_month"].Table)
	}
}

// ---- dashboard latency ---------------------------------------------------------------------------

func TestQualifyDashboardLatency(t *testing.T) {
	requireOut(t)
	s := newStatsWorld(t)
	tok := s.crawler("load")
	s.startIngest()
	sb := &submitter{addr: s.addr, token: tok}
	sb.paced(8*time.Second, 20, 100*time.Millisecond, nil)
	s.stopIngest() // a real snapshot, shared state and a first bucket
	genHistory(t, s.dsn, time.Now().UTC().Truncate(time.Hour).AddDate(-10, 0, 0), time.Now().UTC().Truncate(time.Hour).Add(-2*time.Hour))
	mkUser(t, &harnessWorld{t: t, bin: s.bin, dsn: s.dsn}, "bob", "admin")
	web := startWeb(t, s.bin, s.dsn, "http")
	bob := newBrowser(t, web.addr)
	if resp := bob.login("bob", adminPW); resp.StatusCode != 303 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	timeGet := func(path string) (float64, int) {
		t0 := time.Now()
		resp := bob.get(path)
		return float64(time.Since(t0).Microseconds()) / 1000, resp.StatusCode
	}
	stat := func(v []float64) map[string]float64 {
		slices.Sort(v)
		return map[string]float64{"n": float64(len(v)), "p50_ms": percentile(v, .5), "p95_ms": percentile(v, .95), "max_ms": percentile(v, 1)}
	}
	var cold, cached, coldSeries, cachedSeries []float64
	// Cold overview: each request after the 10 s cache lifetime.
	n := max(int(40*qualScale()), 5)
	for i := 0; i < n; i++ {
		time.Sleep(10500 * time.Millisecond)
		d, st := timeGet("/admin/api/overview")
		if st != 200 {
			t.Fatalf("overview: %d", st)
		}
		cold = append(cold, d)
	}
	for i := 0; i < 100; i++ {
		d, _ := timeGet("/admin/api/overview")
		cached = append(cached, d)
	}
	// Cold series: a different day each time is a different cache key.
	charts := []string{"throughput", "outcomes", "queue", "traffic", "storage"}
	for i := 0; i < 100; i++ {
		day := time.Now().UTC().AddDate(0, 0, -(10 + i)).Format("2006-01-02")
		d, st := timeGet("/admin/api/series?chart=" + charts[i%len(charts)] + "&g=day&period=" + day)
		if st != 200 {
			t.Fatalf("series %s: %d", day, st)
		}
		coldSeries = append(coldSeries, d)
	}
	for i := 0; i < 100; i++ {
		day := time.Now().UTC().AddDate(0, 0, -(10 + i%5)).Format("2006-01-02")
		d, _ := timeGet("/admin/api/series?chart=" + charts[i%len(charts)] + "&g=day&period=" + day)
		cachedSeries = append(cachedSeries, d)
	}
	for _, g := range []string{"month", "year"} {
		period := map[string]string{"month": time.Now().UTC().AddDate(0, -3, 0).Format("2006-01"), "year": time.Now().UTC().AddDate(-3, 0, 0).Format("2006")}[g]
		d, st := timeGet("/admin/api/series?chart=throughput&g=" + g + "&period=" + period)
		qualOut(t, "dashboard", map[string]any{"cold_series_" + g + "_ms": d, "status": st})
	}
	res := map[string]any{"overview_cold": stat(cold), "overview_cached": stat(cached), "series_day_cold": stat(coldSeries), "series_day_cached": stat(cachedSeries), "history_years": 10}
	qualOut(t, "dashboard", res)
	t.Logf("%+v", res)
	if p95 := stat(cached)["p95_ms"]; p95 > 50 {
		t.Errorf("cached overview p95 %.1f ms, want ≤ 50 ms", p95)
	}
	if p95 := stat(cold)["p95_ms"]; p95 > 1000 {
		t.Errorf("cold overview p95 %.1f ms, want ≤ 1,000 ms", p95)
	}
	if p95 := stat(coldSeries)["p95_ms"]; p95 > 1000 {
		t.Errorf("cold series p95 %.1f ms, want ≤ 1,000 ms", p95)
	}
}

// TestQualifySabotageControl separates the cost of the statistics code from the
// cost of the sabotage method: the "tables locked" sabotage holds a transaction
// open, and an open transaction pins the vacuum horizon of the whole database
// while the saturating load churns the queue. The control holds the same kind of
// transaction with the statistics DISABLED and no statistics table involved.
func TestQualifySabotageControl(t *testing.T) {
	requireOut(t)
	runs := 3
	if qualScale() < 1 {
		runs = 1
	}
	idleTx := func(t *testing.T, s *statsWorld) { // an open transaction on an unrelated table: nothing of the statistics
		c, err := pgx.Connect(context.Background(), s.dsn)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{`BEGIN`, `SELECT txid_current()`, `LOCK TABLE magnetico_web.settings IN ACCESS EXCLUSIVE MODE`} {
			if _, err := c.Exec(context.Background(), q); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _ = c.Close(context.Background()) })
	}
	locked := sabotageCases()[2]
	conds := []struct {
		name  string
		env   []string
		apply func(*testing.T, *statsWorld)
	}{
		{"control: disabled", []string{"MAGNETICO_INGEST_STATS_DISABLED=true"}, nil},
		{"control: disabled, idle transaction on an unrelated table", []string{"MAGNETICO_INGEST_STATS_DISABLED=true"}, idleTx},
		{"enabled, statistics tables locked", nil, locked.apply},
	}
	res := map[string][]float64{}
	for i := 1; i <= runs; i++ {
		for k := range conds {
			c := conds[(k+i-1)%len(conds)]
			t.Run(fmt.Sprintf("%s/%d", c.name, i), func(t *testing.T) {
				r := measureRun(t, c.name, 100+i, c.env, c.apply)
				res[c.name] = append(res[c.name], r.RecPerSec)
			})
		}
	}
	base := median(res[conds[0].name])
	summary := map[string]float64{}
	for _, c := range conds {
		summary[c.name] = median(res[c.name]) / base
	}
	qualOut(t, "sabotage_control_summary", summary)
	t.Logf("rate relative to the disabled baseline: %+v", summary)
}
