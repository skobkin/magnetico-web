//go:build loadtest

package cli

// Task 09A combined-service soak: profile "soak09a" of TestLoad (same guards: the
// confirmed, local, benchmark-marked database; see load_test.go).
//
//	MAGNETICO_BENCH_LARGE=1 MAGNETICO_BENCH_DSN=… MAGNETICO_BENCH_CONFIRM_DISPOSABLE_DB=bench \
//	  MAGNETICO_BENCH_OUT=<dir> MAGNETICO_BENCH_LABEL=soak09a MAGNETICO_BENCH_PROFILE=soak09a \
//	  MAGNETICO_BENCH_TAGS=diag MAGNETICO_BENCH_SOAK_SCALE=1 \
//	  go test -tags loadtest -run 'TestLoad$' -count=1 -v -timeout 8h ./internal/e2e/cli
//
// MAGNETICO_BENCH_SOAK_SCALE multiplies every duration (1 = about 4 h 50 min;
// 0.05 is a 15-minute smoke run).
//
// What runs together, for the whole soak, under the web and ingest cgroup limits of
// the load harness: five real crawler clients (internal/e2e/crawlerd: the
// production durable queue, batching sender and existence precheck, one
// credential each) delivering a steady stream with bursts every 30 minutes;
// interactive web users (search, paging, details, file browsing, API, RSS, magnet)
// with think time, plus a closed-loop phase; an administrator session reading the
// dashboard, its JSON and the listings; samplers of CPU, RSS, cgroup memory,
// goroutines, descriptors, heap, PostgreSQL connections, locks, dead tuples, queue,
// quota and statistics freshness.
//
// Timeline events (minutes at scale 1): cold web restart (20, 180); credential
// rotation (60) and disable/enable (90) during traffic, with the propagation delay
// measured; a graceful ingest restart (120); a statistics-only failure, the
// statistics tables locked for 10 minutes (150); a closed-loop web phase (240); then
// the crawlers stop, the queue drains and a final idle phase measures recovery.
//
// End state, written as "check" records and gated: received − acknowledged − refused
// is zero, acknowledged = enqueued + duplicates, every enqueued record is settled,
// torrents created equals the torrents stored by the crawlers, and for every counter
// the hourly, daily and monthly aggregates agree (the hour boundaries crossed during
// the run are exactly what this exercises).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/opstats"
)

const (
	soakCrawlers      = 5 // c1..c4 steady, c5 is the credential-event crawler
	soakBaselineRate  = 6.0
	soakBurstRate     = 30.0
	soakPrecheckRate  = 60.0
	soakWebUsers      = 40
	soakClosedLoop    = 16
	soakAdminEvery    = 3 * time.Second
	soakMonitorEvery  = 60 * time.Second
	soakHeapEvery     = 30 * time.Minute
	soakStatsLockHold = 10
)

// soakClock turns minutes of the nominal timeline into durations.
type soakClock struct {
	scale float64
	start time.Time
}

func (c soakClock) dur(min float64) time.Duration {
	return time.Duration(min * c.scale * float64(time.Minute))
}
func (c soakClock) elapsed() time.Duration { return time.Since(c.start) }
func (c soakClock) sleepUntil(min float64) {
	if d := c.dur(min) - c.elapsed(); d > 0 {
		time.Sleep(d)
	}
}

// phases builds the crawlerd phase list "rate:count,…" from offset to total:
// baseline with a burst of two minutes at the end of every half hour.
func (c soakClock) phases(offset, total time.Duration) string {
	var parts []string
	half := c.dur(30)
	burst := max(c.dur(2), 10*time.Second)
	for t := offset; t < total; {
		next := (t/half + 1) * half // the end of this half hour
		burstStart := next - burst
		var seg time.Duration
		rate := soakBaselineRate
		if t < burstStart {
			seg = min(burstStart, total) - t
		} else {
			seg, rate = min(next, total)-t, soakBurstRate
		}
		if seg <= 0 {
			break
		}
		if n := int(rate * seg.Seconds()); n > 0 {
			parts = append(parts, fmt.Sprintf("%g:%d", rate, n))
		}
		t += seg
	}
	return strings.Join(parts, ",")
}

type soakCrawler struct {
	name, dir, token string
	from             int
	proc             *harnessProc
	log              []string // the output of earlier processes of this crawler
}

type soak struct {
	e     *harnessEnv
	clk   soakClock
	total time.Duration
	cd    string
	crs   []*soakCrawler

	events   []map[string]any
	evMu     sync.Mutex
	adminOK  atomic.Int64
	adminBad atomic.Int64
	adminHC  *http.Client
	adminCk  string

	webRestarting atomic.Bool
	ingLogs       []string // the output of ingest processes that were stopped
	webLogs       []string
}

func (s *soak) event(kind string, v map[string]any) {
	v["event"], v["t_s"] = kind, s.clk.elapsed().Seconds()
	s.evMu.Lock()
	s.events = append(s.events, v)
	s.evMu.Unlock()
	s.e.write("event", v)
}

func (s *soak) startCrawler(c *soakCrawler) {
	phases := s.clk.phases(s.clk.elapsed(), s.total)
	if phases == "" {
		return
	}
	// each start continues the id range, so a restart creates new records (and a few
	// duplicates through the prechecker), as a live crawler does
	c.from += 1_000_000
	args := []string{"-queue-dir", c.dir, "-url", "http://" + s.e.ingAddr, "-from", strconv.Itoa(c.from), "-phases", phases,
		"-precheck-rate", fmt.Sprint(soakPrecheckRate), "-precheck-count", strconv.Itoa(int(soakPrecheckRate*s.total.Seconds()) + 1), "-hold"}
	c.proc = harnessStart(s.e.t, s.cd, []string{"MAGNETICO_CRAWL_INGEST_TOKEN=" + c.token}, args...)
}

func (s *soak) stopCrawler(c *soakCrawler) int {
	if c.proc == nil {
		return 0
	}
	code := c.proc.terminate(s.e.t, 90*time.Second)
	c.log = append(c.log, c.proc.out.String())
	c.proc = nil
	return code
}

// probe sends one existence request with the credential and returns the status.
func (s *soak) probe(token string) int {
	req, _ := http.NewRequest("POST", "http://"+s.e.ingAddr+"/api/ingest/v1/torrents/exists", strings.NewReader(`{"info_hashes":[]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// waitStatus polls the probe until it returns want and reports how long it took.
func (s *soak) waitStatus(token string, want int, limit time.Duration) (time.Duration, bool) {
	start := time.Now()
	for time.Since(start) < limit {
		if s.probe(token) == want {
			return time.Since(start), true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return time.Since(start), false
}

// ---- admin client -----------------------------------------------------------------------

func (s *soak) setupAdmin() {
	e := s.e
	// the benchmark guard admits only the generated identities: the first of them is promoted
	const name, pw = "w9u00", harnessLoadPassword
	if out, errOut, code := runWebAdminBinary(e.t, e.bin, e.dsn, "", nil, "user", "set-role", name, "admin"); code != 0 {
		e.t.Fatalf("set-role: %d %s%s", code, out, errOut)
	}
	b := newBrowser(e.t, e.webAddr)
	b.get("/login", "X-Forwarded-For", "10.88.0.1")
	if resp := b.post("/login", map[string][]string{"username": {name}, "password": {pw}, "csrf_token": {b.token()}}, "X-Forwarded-For", "10.88.0.1"); resp.StatusCode != 303 {
		e.t.Fatalf("admin login: %d", resp.StatusCode)
	}
	s.adminCk = sessionCookieOf(b, e.webAddr)
	s.adminHC = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// adminGet fetches one admin URL and returns the status, the body and the latency.
func (s *soak) adminGet(path string) (int, []byte, time.Duration) {
	req, _ := http.NewRequest("GET", "http://"+s.e.webAddr+path, nil)
	req.Header.Set("Cookie", s.adminCk)
	req.Header.Set("X-Forwarded-For", "10.88.0.1")
	start := time.Now()
	resp, err := s.adminHC.Do(req)
	if err != nil {
		return -1, nil, time.Since(start)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, time.Since(start)
}

// adminLoop reads the dashboard, its JSON and the listings until stop is closed, and
// writes one condensed overview every minute.
func (s *soak) adminLoop(stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	paths := []string{"/admin", "/admin/api/overview", "/admin/api/series?chart=throughput&g=day", "/admin/api/series?chart=queue&g=day",
		"/admin/api/series?chart=storage&g=month", "/admin/api/series?chart=traffic&g=year", "/admin/crawlers", "/admin/quarantine", "/admin/users", "/admin/invitations", "/admin/trackers"}
	acc := newHarnessAcc()
	i, last := 0, time.Now()
	for {
		select {
		case <-stop:
			total, classes := acc.stats()
			s.e.write("admin_summary", map[string]any{"ok": s.adminOK.Load(), "bad": s.adminBad.Load(), "outcomes": total, "classes": classes})
			return
		case <-time.After(soakAdminEvery):
		}
		p := paths[i%len(paths)]
		i++
		code, _, d := s.adminGet(p)
		outcome := oOK
		if code != 200 {
			outcome = oUnexpect
			s.adminBad.Add(1)
		} else {
			s.adminOK.Add(1)
		}
		acc.add("admin "+strings.SplitN(p, "?", 2)[0], outcome, d)
		if time.Since(last) >= soakMonitorEvery {
			last = time.Now()
			if code, body, _ := s.adminGet("/admin/api/overview"); code == 200 {
				s.e.write("admin_overview", condenseOverview(body))
			}
		}
	}
}

func condenseOverview(body []byte) map[string]any {
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		return map[string]any{"unreadable": true}
	}
	out := map[string]any{"freshness": v["freshness"], "instances": v["instances"], "queue": v["queue"], "crawlers": v["crawlers"], "users": v["users"]}
	if rates, ok := v["rates"].([]any); ok && len(rates) > 0 {
		out["rate_60s"] = rates[0]
		if len(rates) > 2 {
			out["rate_60min"] = rates[2]
		}
	}
	return out
}

// ---- monitors ---------------------------------------------------------------------------

func (s *soak) diagReading(addr string) map[string]any {
	if addr == "" {
		return nil
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + addr + "/diag")
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	delete(v, "goroutine_groups") // the groups are recorded only at the checkpoints
	return v
}

func (s *soak) heapReading(addr, label string) {
	if addr == "" {
		return
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get("http://" + addr + "/heap")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var h map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&h)
	d, _ := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + addr + "/diag")
	var full map[string]any
	if d != nil {
		_ = json.NewDecoder(d.Body).Decode(&full)
		d.Body.Close()
	}
	s.e.write("heap_checkpoint", map[string]any{"label": label, "addr": addr, "heap": h, "goroutine_groups": full["goroutine_groups"], "goroutines": full["goroutines"]})
}

func (s *soak) monitorLoop(stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	e := s.e
	conn, err := pgx.Connect(context.Background(), e.dsn)
	if err != nil {
		e.t.Errorf("monitor: cannot connect: %v", err)
		return
	}
	defer conn.Close(context.Background())
	lastHeap := time.Now()
	for {
		select {
		case <-stop:
			return
		case <-time.After(soakMonitorEvery):
		}
		rec := map[string]any{"t_s": s.clk.elapsed().Seconds()}
		q := func(name, sql string, dest ...any) {
			ctx, cancel := context.WithTimeout(context.Background(), harnessQueryTimeout)
			defer cancel()
			if err := conn.QueryRow(ctx, sql).Scan(dest...); err != nil {
				rec[name+"_error"] = err.Error()
			}
		}
		var conns, waiting, ungranted int64
		q("pg", `SELECT (SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()),
(SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'),
(SELECT count(*) FROM pg_locks WHERE NOT granted)`, &conns, &waiting, &ungranted)
		rec["pg_connections"], rec["pg_lock_waiters"], rec["pg_ungranted_locks"] = conns, waiting, ungranted
		var oldestTx float64
		q("pg_tx", `SELECT COALESCE(max(EXTRACT(EPOCH FROM now() - xact_start)), 0) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND xact_start IS NOT NULL`, &oldestTx)
		rec["pg_oldest_tx_s"] = oldestTx
		var qRows, qBytes, maxID int64
		q("queue", `SELECT (SELECT count(*) FROM magnetico_ingest.queue), (SELECT used_bytes FROM magnetico_ingest.usage WHERE id = 1), (SELECT COALESCE(max(id), 0) FROM magneticod.torrents)`, &qRows, &qBytes, &maxID)
		rec["queue_rows"], rec["quota_used_bytes"], rec["torrents_max_id"] = qRows, qBytes, maxID
		var dead, statsBytes, ingestBytes int64
		q("dead", `SELECT COALESCE(sum(n_dead_tup) FILTER (WHERE schemaname = 'magnetico_stats'), 0), COALESCE(sum(pg_total_relation_size(relid)) FILTER (WHERE schemaname = 'magnetico_stats'), 0),
COALESCE(sum(n_dead_tup) FILTER (WHERE schemaname = 'magnetico_ingest'), 0) FROM pg_stat_user_tables`, &dead, &statsBytes, &ingestBytes)
		rec["dead_tuples_stats"], rec["stats_schema_bytes"], rec["dead_tuples_ingest"] = dead, statsBytes, ingestBytes
		var commits int64
		q("xact", `SELECT xact_commit FROM pg_stat_database WHERE datname = current_database()`, &commits)
		rec["xact_commit"] = commits
		rec["diag_web"], rec["diag_ingest"] = s.diagReading(e.diagAddr), s.diagReading(e.ingDiag)
		rec["web_events"], rec["ingest_events"] = cgroupEvents(e.web.cg), cgroupEvents(e.ingest.cg)
		e.write("monitor", rec)
		if time.Since(lastHeap) >= soakHeapEvery {
			lastHeap = time.Now()
			label := fmt.Sprintf("t=%.0fm", s.clk.elapsed().Minutes())
			s.heapReading(e.diagAddr, "web "+label)
			s.heapReading(e.ingDiag, "ingest "+label)
		}
	}
}

// ---- web users --------------------------------------------------------------------------

// webLoad runs interactive users (think time) in two-minute rounds until stop, or a
// closed loop of conc workers when think is 0, and records each round.
func (s *soak) webRounds(label string, conc int, think time.Duration, d time.Duration) {
	e := s.e
	end := time.Now().Add(d)
	for round := 0; time.Now().Before(end); round++ {
		acc := newHarnessAcc()
		load := newHarnessLoad(e.webAddr, e.pools)
		slice := min(2*time.Minute, time.Until(end))
		start := time.Now()
		load.run(conc, slice, think, acc)
		total, classes := acc.stats()
		n := 0
		for _, v := range total {
			n += v
		}
		e.write("web_round", map[string]any{"phase": label, "t_s": s.clk.elapsed().Seconds(), "concurrency": conc, "think_ms": think.Milliseconds(),
			"requests": n, "rps": float64(n) / time.Since(start).Seconds(), "outcomes": total, "classes": classes})
		// 429s and unexpected statuses are never acceptable; 5xx other than the designed
		// "busy" shedding likewise. Search timeouts are bounded by 0.1 %.
		e.gate(total[oOther5xx] == 0, "%s: %d other 5xx responses", label, total[oOther5xx])
		e.gate(total[oUnexpect] == 0, "%s: %d unexpected 4xx responses", label, total[oUnexpect])
		e.gate(total[o429] == 0, "%s: %d 429 responses", label, total[o429])
		e.gate(float64(total[oTimeout]) <= 0.001*float64(max(n, 1)), "%s: %d search timeouts exceed 0.1%%", label, total[oTimeout])
		e.gate(total[oTransport] == 0 || s.webRestarting.Load(), "%s: %d transport errors", label, total[oTransport])
	}
}

// ---- the run ------------------------------------------------------------------------------

func (e *harnessEnv) soak09a() {
	t := e.t
	scale, _ := strconv.ParseFloat(envOr("MAGNETICO_BENCH_SOAK_SCALE", "1"), 64)
	s := &soak{e: e, clk: soakClock{scale: scale, start: time.Now()}, cd: buildCrawlerd(t)}
	s.total = s.clk.dur(290)
	if e.diagAddr == "" {
		t.Log("the soak is most informative with MAGNETICO_BENCH_TAGS=diag (goroutine and live-heap readings)")
	}
	// A clean slate in the disposable benchmark database: the crawlers, statistics and test
	// torrents of an earlier run would falsify the end-state checks.
	for _, q := range []string{`DROP SCHEMA IF EXISTS magnetico_stats CASCADE`, `DELETE FROM magnetico_ingest.crawlers`,
		`DELETE FROM magneticod.torrent_file_blobs WHERE torrent_id IN (SELECT id FROM magneticod.torrents WHERE name LIKE 'e2e torrent%')`,
		`DELETE FROM magneticod.torrents WHERE name LIKE 'e2e torrent%'`} {
		sqlExec(t, e.dsn, q)
	}
	// the statistics are part of what is qualified: their schema must exist before ingest starts
	if out, code := runWebSchemaInit(t, e.bin, e.dsn, "stats-schema-init"); code != 0 {
		t.Fatalf("migrate stats-schema-init: %d %s", code, out)
	}
	e.begin()
	s.clk.start = time.Now()
	s.setupAdmin()
	for i := 1; i <= soakCrawlers; i++ {
		name := fmt.Sprintf("soak%d", i)
		c := &soakCrawler{name: name, dir: t.TempDir(), token: registerCrawler(t, e.bin, e.dsn, name), from: i * 100_000_000}
		s.crs = append(s.crs, c)
		s.startCrawler(c)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go s.adminLoop(stop, &wg)
	go s.monitorLoop(stop, &wg)
	e.poll("baseline-after-start")
	s.heapReading(e.diagAddr, "web baseline")
	s.heapReading(e.ingDiag, "ingest baseline")

	// the web workload: interactive users throughout, a closed loop 240-270
	webDone := make(chan struct{})
	go func() {
		defer close(webDone)
		s.webRounds("interactive-warm", soakWebUsers, time.Second, s.clk.dur(20)-s.clk.elapsed())
		s.restartWeb("cold-1")
		s.webRounds("interactive-cold", soakWebUsers, time.Second, s.clk.dur(35)-s.clk.elapsed())
		s.webRounds("interactive", soakWebUsers, time.Second, s.clk.dur(180)-s.clk.elapsed())
		s.restartWeb("cold-2")
		s.webRounds("interactive-cold", soakWebUsers, time.Second, s.clk.dur(195)-s.clk.elapsed())
		s.webRounds("interactive", soakWebUsers, time.Second, s.clk.dur(240)-s.clk.elapsed())
		s.webRounds("closed-loop", soakClosedLoop, 0, s.clk.dur(270)-s.clk.elapsed())
		s.webRounds("interactive", soakWebUsers, time.Second, s.clk.dur(290)-s.clk.elapsed())
	}()

	// the events
	s.clk.sleepUntil(60)
	s.rotateCredential(s.crs[4])
	s.clk.sleepUntil(90)
	s.disableCredential(s.crs[3], s.clk.dur(4))
	s.clk.sleepUntil(120)
	s.restartIngest()
	s.clk.sleepUntil(150)
	s.statsOutage(s.clk.dur(soakStatsLockHold))
	<-webDone

	// the end: the crawlers stop, the queue drains, then the idle phase
	e.poll("after-load")
	for _, c := range s.crs {
		if code := s.stopCrawler(c); code != 0 {
			e.gate(false, "crawler %s exit code %d on SIGTERM", c.name, code)
		}
	}
	close(stop)
	wg.Wait()
	e.drain()
	e.poll("idle-start")
	s.heapReading(e.diagAddr, "web idle-start")
	s.heapReading(e.ingDiag, "ingest idle-start")
	idle := s.clk.dur(20)
	time.Sleep(idle)
	e.poll("idle-end")
	s.heapReading(e.diagAddr, "web idle-end")
	s.heapReading(e.ingDiag, "ingest idle-end")
	s.finishSoak()
}

// restartWeb stops and starts the web service: the application caches (search plans,
// file index, sessions' derived state) start cold, the sessions and tokens persist.
func (s *soak) restartWeb(label string) {
	e := s.e
	s.webRestarting.Store(true)
	defer s.webRestarting.Store(false)
	start := time.Now()
	code := e.web.stop(60 * time.Second)
	s.webLogs = append(s.webLogs, e.web.out.String())
	e.gate(code == 0, "web exit code %d on SIGTERM (%s)", code, label)
	e.startWeb()
	e.samp.setProcs([]*harnessScoped{e.web, e.ingest})
	s.event("web_restart", map[string]any{"label": label, "exit_code": code, "down_s": time.Since(start).Seconds()})
}

func (s *soak) rotateCredential(c *soakCrawler) {
	e := s.e
	old := c.token
	out, errOut, code := runIngestCrawlerBinary(e.t, e.bin, e.dsn, nil, "rotate-token", c.name)
	if code != 0 {
		e.gate(false, "rotate-token: %d %s%s", code, out, errOut)
		return
	}
	var fresh string
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "token: "); ok {
			fresh = strings.TrimSpace(v)
		}
	}
	rotatedAt := time.Now()
	newOK := s.probe(fresh) == 200
	took, refused := s.waitStatus(old, 401, 90*time.Second)
	s.event("rotate_token", map[string]any{"crawler": c.name, "new_token_works_at_once": newOK, "old_token_refused": refused, "old_token_accepted_for_s": took.Seconds(), "since_s": time.Since(rotatedAt).Seconds()})
	e.gate(newOK, "rotated credential does not work at once")
	e.gate(refused && took <= 62*time.Second, "the old credential was still accepted %.1f s after rotation (limit 60 s)", took.Seconds())
	// the operator updates the crawler: restart it with the new credential
	s.stopCrawler(c)
	c.token = fresh
	s.startCrawler(c)
}

func (s *soak) disableCredential(c *soakCrawler, hold time.Duration) {
	e := s.e
	if out, errOut, code := runIngestCrawlerBinary(e.t, e.bin, e.dsn, nil, "disable", c.name); code != 0 {
		e.gate(false, "disable: %d %s%s", code, out, errOut)
		return
	}
	took, refused := s.waitStatus(c.token, 401, 90*time.Second)
	e.gate(refused && took <= 62*time.Second, "a disabled crawler was still accepted %.1f s after disable (limit 60 s)", took.Seconds())
	time.Sleep(hold)
	if out, errOut, code := runIngestCrawlerBinary(e.t, e.bin, e.dsn, nil, "enable", c.name); code != 0 {
		e.gate(false, "enable: %d %s%s", code, out, errOut)
		return
	}
	back, ok := s.waitStatus(c.token, 200, 2*time.Minute)
	s.event("disable_enable", map[string]any{"crawler": c.name, "refused_after_s": took.Seconds(), "disabled_for_s": hold.Seconds(), "accepted_again_after_s": back.Seconds(), "accepted_again": ok})
	e.gate(ok, "an enabled crawler was not accepted again within 2 minutes")
}

// restartIngest stops the ingest service gracefully (a drained shutdown with the final
// statistics flush) and starts it again on the same address; the crawlers keep their
// durable queues and retry.
func (s *soak) restartIngest() {
	e := s.e
	start := time.Now()
	code := e.ingest.stop(90 * time.Second)
	s.ingLogs = append(s.ingLogs, e.ingest.out.String())
	e.gate(code == 0, "ingest exit code %d on SIGTERM (restart)", code)
	down := time.Since(start)
	env := harnessLoadEnv("MAGNETICO_INGEST_DATABASE="+e.dsn, "MAGNETICO_INGEST_LISTEN="+e.ingAddr, "GOMAXPROCS=2", "GOMEMLIMIT=256MiB", "GODEBUG=gctrace=1")
	if e.ingDiag != "" {
		env = append(env, "MAGNETICO_DIAG_ADDR="+e.ingDiag)
	}
	e.ingest = startScoped(e.t, "ingest", "1G", 100, env, e.bin, "ingest")
	e.samp.setProcs([]*harnessScoped{e.web, e.ingest})
	up, ok := s.waitStatus(e.ingTok, 200, 60*time.Second)
	s.event("ingest_restart", map[string]any{"exit_code": code, "stop_s": down.Seconds(), "ready_after_s": up.Seconds(), "ready": ok})
	e.gate(ok, "ingest did not become ready after the restart")
}

// statsOutage locks the aggregate table the statistics writer needs: a failure of the
// statistics and only of the statistics. Admission and processing must go on.
func (s *soak) statsOutage(hold time.Duration) {
	e := s.e
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		e.gate(false, "stats outage: cannot connect: %v", err)
		return
	}
	defer conn.Close(ctx)
	idBefore := s.maxTorrentID(conn)
	if _, err := conn.Exec(ctx, `BEGIN`); err != nil {
		e.gate(false, "stats outage: %v", err)
		return
	}
	if _, err := conn.Exec(ctx, `LOCK TABLE magnetico_stats.agg_hour IN ACCESS EXCLUSIVE MODE`); err != nil {
		e.gate(false, "stats outage: %v", err)
		return
	}
	start := time.Now()
	s.event("stats_outage_begin", map[string]any{"hold_s": hold.Seconds()})
	time.Sleep(hold)
	idDuring := s.maxTorrentID(conn)
	if _, err := conn.Exec(ctx, `ROLLBACK`); err != nil {
		e.gate(false, "stats outage: release: %v", err)
	}
	// Recovery: the writer's next successful flush (its backlog applied in that one flush)
	// stamps ingest_instances.last_flush_at after the release; the dashboard is fresh again
	// is recorded too, but the heartbeat table was not locked, so it never went stale.
	var released time.Time
	_ = conn.QueryRow(ctx, `SELECT now()`).Scan(&released)
	recovered, ok := s.waitFlushAfter(conn, released, 5*time.Minute)
	fresh, freshOK := s.waitFresh(time.Minute)
	s.event("stats_outage_end", map[string]any{"held_s": time.Since(start).Seconds(), "torrents_added_during": idDuring - idBefore,
		"first_flush_after_release_s": recovered.Seconds(), "flushed": ok, "dashboard_fresh_after_s": fresh.Seconds(), "dashboard_fresh": freshOK})
	e.gate(idDuring-idBefore > 0, "no torrent was stored while the statistics were locked: processing stopped")
	e.gate(ok, "the statistics writer did not flush within 5 minutes after the lock was released")
}

// waitFlushAfter waits until some instance has flushed after the given database time.
func (s *soak) waitFlushAfter(conn *pgx.Conn, after time.Time, limit time.Duration) (time.Duration, bool) {
	start := time.Now()
	for time.Since(start) < limit {
		var done bool
		if err := conn.QueryRow(context.Background(), `SELECT COALESCE(max(last_flush_at) > $1, false) FROM magnetico_stats.ingest_instances`, after).Scan(&done); err == nil && done {
			return time.Since(start), true
		}
		time.Sleep(time.Second)
	}
	return time.Since(start), false
}

func (s *soak) maxTorrentID(conn *pgx.Conn) int64 {
	var n int64
	_ = conn.QueryRow(context.Background(), `SELECT COALESCE(max(id), 0) FROM magneticod.torrents`).Scan(&n)
	return n
}

// waitFresh waits until the overview reports the ingest instances as up.
func (s *soak) waitFresh(limit time.Duration) (time.Duration, bool) {
	start := time.Now()
	for time.Since(start) < limit {
		if code, body, _ := s.adminGet("/admin/api/overview"); code == 200 {
			var v struct {
				Freshness struct {
					Ingest string `json:"ingest"`
					Stale  bool   `json:"stale"`
				} `json:"freshness"`
			}
			if json.Unmarshal(body, &v) == nil && v.Freshness.Ingest == "up" && !v.Freshness.Stale {
				return time.Since(start), true
			}
		}
		time.Sleep(5 * time.Second)
	}
	return time.Since(start), false
}

// finishSoak stops the services (the ingest gracefully, so its final flush lands),
// checks the persisted statistics and writes the per-process reports.
func (s *soak) finishSoak() {
	e := s.e
	e.samp.finish()
	e.samp.mu.Lock()
	samples, queue := append([]harnessSample(nil), e.samp.samples...), append([]harnessPoint(nil), e.samp.queue...)
	e.samp.mu.Unlock()
	e.write("samples", map[string]any{"samples": thinS(samples, 3000), "ingest_queue": queue})
	for _, p := range []*harnessScoped{e.ingest, e.web} {
		rep := map[string]any{"proc": p.name}
		rep["cgroup_peak_mib"] = float64(readUint(p.cg+"/memory.peak")) / (1 << 20)
		ev := cgroupEvents(p.cg)
		rep["memory_events"] = ev
		e.gate(ev["oom_kill"] == 0 && ev["oom"] == 0, "the %s cgroup had OOM events %v", p.name, ev)
		code := p.stop(60 * time.Second)
		rep["exit_code"] = code
		e.gate(code == 0, "%s exit code %d on SIGTERM", p.name, code)
		logs := []string{p.out.String()}
		if p.name == "ingest" {
			logs = append(s.ingLogs, logs...)
		} else {
			logs = append(s.webLogs, logs...)
		}
		var live []float64
		var pauseTotal float64
		gcCount := 0
		warn := map[string]int{}
		for _, l := range logs {
			gcs, count, pause := parseGC(l)
			gcCount += count
			pauseTotal += pause
			for _, g := range gcs {
				live = append(live, g.LiveMB)
			}
			for _, line := range strings.Split(l, "\n") {
				if m := msgRe09a.FindStringSubmatch(line); m != nil {
					warn[m[1]+": "+m[2]]++
				}
			}
		}
		if len(live) > 0 {
			rep["heap_after_gc_mib_max"] = maxOf(live)
			rep["heap_after_gc_mib_series"] = thinFloat(live, 600)
		}
		rep["gc_count"], rep["gc_pause_ms_total"], rep["warn_error_lines"] = gcCount, pauseTotal, warn
		for k, n := range warn {
			if strings.HasPrefix(k, "ERROR") {
				e.gate(false, "%s: %d log line(s) at level %s", p.name, n, k)
			}
		}
		e.write("process", rep)
		if p == e.ingest {
			s.checkConservation() // after the graceful stop: the final flush has landed
		}
	}
	for _, c := range s.crs {
		for _, l := range c.log {
			if strings.Contains(l, "panic") {
				e.gate(false, "crawler %s logged a panic", c.name)
			}
		}
	}
}

func thinFloat(v []float64, n int) []float64 {
	if len(v) <= n {
		return v
	}
	step := len(v) / n
	var out []float64
	for i := 0; i < len(v); i += step {
		out = append(out, v[i])
	}
	return out
}

// checkConservation reads the persisted aggregates and gates the end state.
func (s *soak) checkConservation() {
	e := s.e
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		e.gate(false, "conservation: cannot connect: %v", err)
		return
	}
	defer conn.Close(ctx)
	sum := func(tbl, col string) int64 {
		var n int64
		if err := conn.QueryRow(ctx, `SELECT COALESCE(sum(`+col+`), 0)::bigint FROM magnetico_stats.`+tbl).Scan(&n); err != nil {
			e.gate(false, "conservation: sum(%s.%s): %v", tbl, col, err)
		}
		return n
	}
	h := func(c opstats.Counter) int64 { return sum("agg_hour", c.Name()) }
	rec, ack, ref, enq, dup := h(opstats.CRecordsReceived), h(opstats.CRecordsAcknowledged), h(opstats.CRecordsRefused), h(opstats.CRecordsEnqueued), h(opstats.CAdmissionDuplicates)
	settled := h(opstats.CSettledStoredNew) + h(opstats.CSettledStoredExisting) + h(opstats.CSettledQuarantined)
	created := h(opstats.CTorrentsCreated)
	var stored int64
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM magneticod.torrents WHERE name LIKE 'e2e torrent%'`).Scan(&stored)
	var hours int64
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM magnetico_stats.agg_hour`).Scan(&hours)
	var covered int64
	_ = conn.QueryRow(ctx, `SELECT COALESCE(sum(bit_count(covered_mask::bit(64))), 0) FROM magnetico_stats.agg_hour`).Scan(&covered)
	checks := map[string]any{"received": rec, "acknowledged": ack, "refused": ref, "enqueued": enq, "duplicates": dup, "settled": settled,
		"torrents_created": created, "torrents_stored_by_crawlers": stored, "agg_hour_rows": hours, "covered_minutes": covered,
		"run_minutes": s.clk.elapsed().Minutes()}
	e.gate(rec-ack-ref == 0, "conservation: received %d − acknowledged %d − refused %d = %d, want 0 at quiescence", rec, ack, ref, rec-ack-ref)
	e.gate(ack == enq+dup, "conservation: acknowledged %d ≠ enqueued %d + duplicates %d", ack, enq, dup)
	e.gate(settled == enq, "conservation: settled %d ≠ enqueued %d (the queue is empty)", settled, enq)
	e.gate(created == stored, "conservation: torrents created %d ≠ torrents stored by the crawlers %d", created, stored)
	// the three granularities agree for every counter
	mismatch := []string{}
	for c := opstats.Counter(0); c < opstats.NumCounters; c++ {
		a, b, m := sum("agg_hour", c.Name()), sum("agg_day", c.Name()), sum("agg_month", c.Name())
		if a != b || a != m {
			mismatch = append(mismatch, fmt.Sprintf("%s hour=%d day=%d month=%d", c.Name(), a, b, m))
		}
	}
	checks["rollup_mismatches"] = mismatch
	e.gate(len(mismatch) == 0, "the hourly, daily and monthly aggregates disagree: %v", mismatch)
	e.gate(hours >= max(1, int64(s.clk.elapsed().Hours())), "only %d hourly rows were persisted in a run of %.1f hours", hours, s.clk.elapsed().Hours())
	e.write("check", checks)
}

var msgRe09a = regexp.MustCompile(`level=(WARN|ERROR) msg="?([^"=]*[^" ])"?`)

// harness plumbing used above -----------------------------------------------------------

var _ = syscall.SIGTERM
var _ = os.Getenv
var _ = exec.Command
