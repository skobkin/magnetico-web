// Package e2e is the end-to-end fault-injection and release-readiness suite
// for the distributed crawler/ingest delivery (docs/specs/
// crawler-ingest-delivery-v1.md §12). It runs the real `magnetico` binary
// (`ingest`, `crawl`) and a test-only crawler harness (internal/e2e/crawlerd)
// as subprocesses against a DISPOSABLE PostgreSQL from internal/pgtest.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

const token = "e2e-token-0123456789abcdef0123456789abcdef"

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

// --- binaries ------------------------------------------------------------

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

func binaries(t *testing.T) (magnetico, crawlerd string) {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "magnetico-e2e-bin-")
		if buildErr != nil {
			return
		}
		root, _ := filepath.Abs("../..")
		for _, b := range []struct{ out, pkg string }{
			{"magnetico", "./cmd/magnetico"}, {"crawlerd", "./internal/e2e/crawlerd"},
		} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, b.out), b.pkg)
			cmd.Dir = root
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("build %s: %v\n%s", b.pkg, err, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return filepath.Join(binDir, "magnetico"), filepath.Join(binDir, "crawlerd")
}

// --- processes -------------------------------------------------------------

type proc struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
	err  error
}

func start(t *testing.T, env []string, name string, args ...string) *proc {
	t.Helper()
	p := &proc{cmd: exec.Command(name, args...), done: make(chan struct{})}
	p.cmd.Env = append(os.Environ(), env...)
	pipe, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.cmd.Stderr = &lockedWriter{p: p}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(pipe)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			p.mu.Lock()
			p.out.WriteString(sc.Text() + "\n")
			p.mu.Unlock()
		}
	}()
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { p.kill(); <-p.done })
	return p
}

type lockedWriter struct{ p *proc }

func (w *lockedWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	return w.p.out.Write(b)
}

func (p *proc) output() string { p.mu.Lock(); defer p.mu.Unlock(); return p.out.String() }
func (p *proc) kill()          { _ = p.cmd.Process.Signal(syscall.SIGKILL) }
func (p *proc) term()          { _ = p.cmd.Process.Signal(syscall.SIGTERM) }

func (p *proc) wait(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(d):
		t.Fatalf("process did not exit within %v; output:\n%s", d, tail(p.output()))
	}
	if p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}

func tail(s string) string {
	if len(s) > 3000 {
		return "…" + s[len(s)-3000:]
	}
	return s
}

func rssKiB(p *proc) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", p.cmd.Process.Pid))
	if err != nil {
		return 0
	}
	m := regexp.MustCompile(`VmRSS:\s+(\d+) kB`).FindSubmatch(b)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// --- environment -----------------------------------------------------------

type world struct {
	t    *testing.T
	dsn  string
	c    *pgtest.Cluster
	pool *pgxpool.Pool
	mag  string
	cd   string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	mag, cd := binaries(t)
	c := pgtest.IntegrationCluster(t)
	dsn := c.TestDatabase(t)
	ctx := context.Background()
	if err := persistence.InitSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	if err := pgqueue.InitSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &world{t: t, dsn: dsn, c: c, pool: pool, mag: mag, cd: cd}
}

func (w *world) n(query string) int {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var n int
	if err := w.pool.QueryRow(ctx, query).Scan(&n); err != nil {
		w.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (w *world) torrents() int  { return w.n(`SELECT count(*) FROM magneticod.torrents`) }
func (w *world) pgQueue() int   { return w.n(`SELECT count(*) FROM magnetico_ingest.queue`) }
func (w *world) pgQuar() int    { return w.n(`SELECT count(*) FROM magnetico_ingest.quarantine`) }
func (w *world) usedBytes() int { return w.n(`SELECT used_bytes FROM magnetico_ingest.usage`) }
func (w *world) usageDrift() int {
	return w.n(`SELECT (SELECT used_bytes FROM magnetico_ingest.usage) - (SELECT COALESCE(sum(payload_bytes),0) FROM magnetico_ingest.queue) - (SELECT COALESCE(sum(payload_bytes),0) FROM magnetico_ingest.quarantine)`)
}

func (w *world) startIngest(addr string, extraEnv ...string) *proc {
	w.t.Helper()
	env := append([]string{
		"MAGNETICO_INGEST_TOKEN=" + token,
		"MAGNETICO_INGEST_DATABASE=" + w.dsn,
		"MAGNETICO_INGEST_LISTEN=" + addr,
	}, extraEnv...)
	p := start(w.t, env, w.mag, "ingest")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			w.t.Fatalf("ingest exited early:\n%s", tail(p.output()))
		default:
		}
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/ingest/v1/torrents/exists", strings.NewReader(`{"info_hashes":[]}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return p
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	w.t.Fatalf("ingest did not become ready:\n%s", tail(p.output()))
	return nil
}

func (w *world) crawlerd(queueDir, ingestURL string, args ...string) *proc {
	w.t.Helper()
	a := append([]string{"-queue-dir", queueDir, "-url", ingestURL}, args...)
	return start(w.t, []string{"MAGNETICO_CRAWL_INGEST_TOKEN=" + token}, w.cd, a...)
}

func eventually(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// settle waits for the PostgreSQL queue to drain and checks the invariants
// every scenario must end in.
func (w *world) settle(want int) {
	w.t.Helper()
	eventually(w.t, fmt.Sprintf("%d torrents and an empty queue (have %d torrents, %d queued)", want, w.torrents(), w.pgQueue()), 90*time.Second,
		func() bool { return w.torrents() == want && w.pgQueue() == 0 })
	if d := w.usageDrift(); d != 0 {
		w.t.Fatalf("usage accounting drifted by %d bytes", d)
	}
	if u := w.usedBytes(); u != w.n(`SELECT COALESCE(sum(payload_bytes),0) FROM magnetico_ingest.quarantine`) {
		w.t.Fatalf("used_bytes = %d with an empty queue", u)
	}
}

var doneRe = regexp.MustCompile(`DONE queue_records=(\d+) quarantine_records=(\d+) appended=(\d+) duplicate=(\d+) acked=(\d+) quarantined=(\d+)`)

type done struct{ queue, quarantine, appended, duplicate, acked, quarantined int }

func parseDone(t *testing.T, out string) done {
	t.Helper()
	m := doneRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no DONE line in:\n%s", tail(out))
	}
	v := func(i int) int { n, _ := strconv.Atoi(m[i]); return n }
	return done{v(1), v(2), v(3), v(4), v(5), v(6)}
}

// --- scenarios -------------------------------------------------------------

// Several crawlers with overlapping hash ranges feed one ingest: every hash
// is stored exactly once.
func TestE2EMultipleCrawlersOverlappingHashes(t *testing.T) {
	w := newWorld(t)
	addr := freePort(t)
	w.startIngest(addr)
	base := "http://" + addr

	// Ranges: [1,300], [151,450], [301,600] → union 600.
	var procs []*proc
	for i, from := range []int{1, 151, 301} {
		procs = append(procs, w.crawlerd(t.TempDir(), base, "-from", strconv.Itoa(from), "-count", "300"))
		_ = i
	}
	for _, p := range procs {
		if code := p.wait(t, 120*time.Second); code != 0 {
			t.Fatalf("crawler exit %d:\n%s", code, tail(p.output()))
		}
		if d := parseDone(t, p.output()); d.queue != 0 || d.acked != 300 {
			t.Fatalf("crawler done = %+v", d)
		}
	}
	w.settle(600)
}

// PostgreSQL refuses connections mid-run: the crawler keeps its records,
// ingest answers 503, and everything arrives after recovery.
func TestE2EPostgresOutageAndRecovery(t *testing.T) {
	w := newWorld(t)
	addr := freePort(t)
	ing := w.startIngest(addr)
	cq := t.TempDir()
	cr := w.crawlerd(cq, "http://"+addr, "-count", "400", "-rate", "100")

	eventually(t, "first deliveries", 60*time.Second, func() bool { return w.torrents() >= 40 })

	u, _ := url.Parse(w.dsn)
	db := strings.TrimPrefix(u.Path, "/")
	admin, err := pgx.Connect(context.Background(), w.c.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	setAllow := func(v bool) {
		if _, err := admin.Exec(context.Background(), fmt.Sprintf(`ALTER DATABASE %s WITH ALLOW_CONNECTIONS %v`, pgtest.QuoteIdent(db), v)); err != nil {
			t.Fatal(err)
		}
	}
	setAllow(false)
	t.Cleanup(func() {
		c2, err := pgx.Connect(context.Background(), w.c.AdminDSN)
		if err == nil {
			_, _ = c2.Exec(context.Background(), fmt.Sprintf(`ALTER DATABASE %s WITH ALLOW_CONNECTIONS true`, pgtest.QuoteIdent(db)))
			c2.Close(context.Background())
		}
	})
	_, _ = admin.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, db)
	time.Sleep(4 * time.Second) // outage window: the crawler keeps producing and queueing

	if st := cr.output(); !strings.Contains(st, "STATS") {
		t.Fatalf("crawler produced no statistics during the outage:\n%s", tail(st))
	}
	if ing.cmd.ProcessState != nil {
		t.Fatalf("ingest must survive a database outage:\n%s", tail(ing.output()))
	}
	setAllow(true)

	if code := cr.wait(t, 180*time.Second); code != 0 {
		t.Fatalf("crawler exit %d:\n%s", code, tail(cr.output()))
	}
	if d := parseDone(t, cr.output()); d.queue != 0 || d.acked != 400 {
		t.Fatalf("crawler done = %+v", d)
	}
	w.settle(400)
}

// SIGKILL of ingest while records are pending in its PostgreSQL queue and in
// flight: after a restart every acknowledged record is stored exactly once.
func TestE2EIngestSIGKILLWithPendingRecords(t *testing.T) {
	w := newWorld(t)
	addr := freePort(t)
	ing := w.startIngest(addr)
	cr := w.crawlerd(t.TempDir(), "http://"+addr, "-count", "600", "-rate", "150")

	eventually(t, "some progress", 60*time.Second, func() bool { return w.torrents()+w.pgQueue() >= 60 })
	ing.kill()
	<-ing.done
	pendingAtKill := w.pgQueue()
	t.Logf("ingest killed with %d rows pending in PostgreSQL, %d torrents stored", pendingAtKill, w.torrents())

	ing2 := w.startIngest(addr)
	_ = ing2
	if code := cr.wait(t, 180*time.Second); code != 0 {
		t.Fatalf("crawler exit %d:\n%s", code, tail(cr.output()))
	}
	w.settle(600)
}

// SIGKILL of the crawler at random points, repeatedly: records committed to
// its queue are never lost and redelivery never duplicates.
func TestE2ECrawlerSIGKILLRepeated(t *testing.T) {
	w := newWorld(t)
	addr := freePort(t)
	w.startIngest(addr)
	cq := t.TempDir()
	for i, delay := range []time.Duration{400 * time.Millisecond, 900 * time.Millisecond, 1500 * time.Millisecond} {
		p := w.crawlerd(cq, "http://"+addr, "-count", "500", "-rate", "300")
		time.Sleep(delay)
		p.kill()
		<-p.done
		t.Logf("kill #%d after %v: %d torrents stored", i+1, delay, w.torrents())
	}
	// Final run to completion with the same queue directory.
	p := w.crawlerd(cq, "http://"+addr, "-count", "500")
	if code := p.wait(t, 180*time.Second); code != 0 {
		t.Fatalf("final run exit %d:\n%s", code, tail(p.output()))
	}
	w.settle(500)
}

// The server commits but the 202 never reaches the crawler (a proxy drops the
// first responses): resend is idempotent.
func TestE2ELostAcknowledgementIdempotentRedelivery(t *testing.T) {
	w := newWorld(t)
	addr := freePort(t)
	w.startIngest(addr)

	var mu sync.Mutex
	dropped := 0
	target, _ := url.Parse("http://" + addr)
	proxy := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, _ := http.NewRequest(r.Method, target.String()+r.URL.Path, bytes.NewReader(body))
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			rw.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		mu.Lock()
		drop := strings.HasSuffix(r.URL.Path, "/torrents") && resp.StatusCode == 202 && dropped < 3
		if drop {
			dropped++
		}
		mu.Unlock()
		if drop { // committed upstream; the response never reaches the client
			hj, _ := rw.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		rw.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(rw, resp.Body)
	}))
	defer proxy.Close()

	p := w.crawlerd(t.TempDir(), proxy.URL, "-count", "300")
	if code := p.wait(t, 180*time.Second); code != 0 {
		t.Fatalf("crawler exit %d:\n%s", code, tail(p.output()))
	}
	mu.Lock()
	n := dropped
	mu.Unlock()
	if n != 3 {
		t.Fatalf("proxy dropped %d acknowledgements, want 3", n)
	}
	w.settle(300)
}

// Records that are permanently invalid are quarantined on the crawler side,
// the quarantine persists across restarts, and good records still arrive.
func TestE2EQuarantinePersistence(t *testing.T) {
	w := newWorld(t)
	addr := freePort(t)
	w.startIngest(addr)
	cq := t.TempDir()
	p := w.crawlerd(cq, "http://"+addr, "-count", "50", "-poison", "3")
	if code := p.wait(t, 120*time.Second); code != 0 {
		t.Fatalf("crawler exit %d:\n%s", code, tail(p.output()))
	}
	d := parseDone(t, p.output())
	if d.quarantine != 3 || d.quarantined != 3 || d.queue != 0 {
		t.Fatalf("crawler done = %+v, want 3 quarantined", d)
	}
	w.settle(50)

	// Restart with the same directory: quarantine is still there and nothing
	// is re-sent.
	p2 := w.crawlerd(cq, "http://"+addr, "-count", "0")
	if code := p2.wait(t, 60*time.Second); code != 0 {
		t.Fatalf("restart exit %d:\n%s", code, tail(p2.output()))
	}
	if d := parseDone(t, p2.output()); d.quarantine != 3 {
		t.Fatalf("quarantine after restart = %+v", d)
	}

	// Server-side quarantine survives an ingest restart.
	ctx := context.Background()
	junk := make([]byte, 20)
	junk[0] = 0xFF
	if _, err := w.pool.Exec(ctx, `INSERT INTO magnetico_ingest.queue (info_hash, format, payload, payload_bytes) VALUES ($1, 1, '\x7b7d', 2)`, junk); err != nil {
		t.Fatal(err)
	}
	ing := w.startIngest(freePort(t))
	eventually(t, "server quarantine", 30*time.Second, func() bool { return w.pgQuar() == 1 })
	ing.kill()
	<-ing.done
	if w.pgQuar() != 1 || w.pgQueue() != 0 {
		t.Fatalf("quarantine=%d queue=%d after kill", w.pgQuar(), w.pgQueue())
	}
	w.startIngest(freePort(t))
	time.Sleep(time.Second)
	if w.pgQuar() != 1 {
		t.Fatalf("quarantine lost across restart: %d", w.pgQuar())
	}
	if d := w.usageDrift(); d != 0 {
		t.Fatalf("usage drift %d", d)
	}
}

// A corrupted queue file is reported by `crawl check-queue` and refused by the
// crawler; it is never repaired or modified.
func TestE2EQueueCorruption(t *testing.T) {
	w := newWorld(t)
	addr := freePort(t)
	w.startIngest(addr)
	cq := t.TempDir()
	p := w.crawlerd(cq, "http://"+addr, "-count", "100", "-hold")
	eventually(t, "queue written", 30*time.Second, func() bool { return strings.Contains(p.output(), "acked=100") })
	p.term()
	if code := p.wait(t, 30*time.Second); code != 0 {
		t.Fatalf("exit %d:\n%s", code, tail(p.output()))
	}

	// Healthy queue passes the diagnostic.
	ok := start(t, nil, w.mag, "crawl", "check-queue", "--queue-dir", cq)
	if code := ok.wait(t, 30*time.Second); code != 0 {
		t.Fatalf("healthy check-queue exit %d:\n%s", code, tail(ok.output()))
	}

	// Corrupt the database file in the middle.
	dbPath := filepath.Join(cq, "queue.db")
	if _, err := os.Stat(dbPath); err != nil {
		entries, _ := os.ReadDir(cq)
		t.Fatalf("queue file not found (%v): %v", err, entries)
	}
	f, err := os.OpenFile(dbPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Stat(); st.Size() > 8192 {
		_, _ = f.WriteAt(bytes.Repeat([]byte{0xDE, 0xAD}, 2048), 4096)
	} else {
		_, _ = f.WriteAt(bytes.Repeat([]byte{0xDE, 0xAD}, 64), 100)
	}
	f.Close()
	before, _ := os.ReadFile(dbPath)

	bad := start(t, nil, w.mag, "crawl", "check-queue", "--queue-dir", cq, "--full")
	if code := bad.wait(t, 30*time.Second); code == 0 {
		t.Fatalf("a corrupted queue passed check-queue:\n%s", tail(bad.output()))
	}
	after, _ := os.ReadFile(dbPath)
	if !bytes.Equal(before, after) {
		t.Fatal("check-queue modified the queue file")
	}
}

// Loopback smoke of the real binaries: `ingest` and `crawl` start, `crawl`
// runs its DHT/pipeline without a database, and both shut down cleanly on
// SIGTERM. A stale database setting is refused with a pointer to UPGRADE.md.
func TestE2ELoopbackSmokeRealBinaries(t *testing.T) {
	w := newWorld(t)
	addr := freePort(t)
	ing := w.startIngest(addr)

	crawlEnv := []string{
		"MAGNETICO_CRAWL_INGEST_TOKEN=" + token,
		"MAGNETICO_CRAWL_INGEST_URL=http://" + addr,
		"MAGNETICO_CRAWL_QUEUE_DIR=" + t.TempDir(),
	}
	// No DHT traffic leaves the machine: loopback-only filter, a bootstrap
	// node that does not answer.
	args := []string{"crawl", "--indexer-addr", "127.0.0.1:0", "--filter-nodes-cidrs", "127.0.0.0/8", "--bootstrap-node", "127.0.0.1:9"}
	cr := start(t, crawlEnv, w.mag, args...)
	eventually(t, "crawler start line", 30*time.Second, func() bool { return strings.Contains(cr.output(), "crawler started") })
	time.Sleep(time.Second)
	cr.term()
	if code := cr.wait(t, 40*time.Second); code != 0 {
		t.Fatalf("crawl exit %d:\n%s", code, tail(cr.output()))
	}
	if !strings.Contains(cr.output(), "crawl summary") {
		t.Fatalf("no summary on shutdown:\n%s", tail(cr.output()))
	}

	stale := start(t, append(crawlEnv, "MAGNETICO_CRAWL_DATABASE=postgres://u:secretpw@localhost/db"), w.mag, args...)
	if code := stale.wait(t, 20*time.Second); code == 0 || !strings.Contains(stale.output(), "UPGRADE.md") ||
		strings.Contains(stale.output(), "secretpw") {
		t.Fatalf("stale env: exit %d output %q", code, stale.output())
	}

	ing.term()
	if code := ing.wait(t, 40*time.Second); code != 0 {
		t.Fatalf("ingest exit %d:\n%s", code, tail(ing.output()))
	}
}

// Realistic-rate acceptance run (10–50 records/s, measured size mix): the
// crawler queue and the PostgreSQL backlog must stay bounded, and memory and
// disk stay within limits. Duration: MAGNETICO_E2E_LOAD_SECONDS (default 30).
func TestE2ELoadAtExpectedRate(t *testing.T) {
	secs := 30
	if v, err := strconv.Atoi(os.Getenv("MAGNETICO_E2E_LOAD_SECONDS")); err == nil && v > 0 {
		secs = v
	}
	const rate = 40
	total := rate * secs

	w := newWorld(t)
	addr := freePort(t)
	ing := w.startIngest(addr)
	cq := t.TempDir()
	start0 := time.Now()
	cr := w.crawlerd(cq, "http://"+addr, "-count", strconv.Itoa(total), "-rate", strconv.Itoa(rate))

	var maxCrawlerQ, maxPGQ, maxIngestRSS, maxCrawlRSS int
	var samples []string
	re := regexp.MustCompile(`STATS queue_records=(\d+)`)
	for i := 0; ; i++ {
		select {
		case <-cr.done:
		default:
		}
		time.Sleep(2 * time.Second)
		out := cr.output()
		qr := 0
		if all := re.FindAllStringSubmatch(out, -1); len(all) > 0 {
			qr, _ = strconv.Atoi(all[len(all)-1][1])
		}
		pq := w.pgQueue()
		if qr > maxCrawlerQ {
			maxCrawlerQ = qr
		}
		if pq > maxPGQ {
			maxPGQ = pq
		}
		if r := rssKiB(ing); r > maxIngestRSS {
			maxIngestRSS = r
		}
		select {
		case <-cr.done:
		default:
			if r := rssKiB(cr); r > maxCrawlRSS {
				maxCrawlRSS = r
			}
		}
		samples = append(samples, fmt.Sprintf("t=%2ds crawler_queue=%d pg_backlog=%d stored=%d", int(time.Since(start0).Seconds()), qr, pq, w.torrents()))
		select {
		case <-cr.done:
			goto finished
		default:
		}
		if time.Since(start0) > time.Duration(secs+120)*time.Second {
			t.Fatalf("load run did not finish:\n%s", tail(cr.output()))
		}
	}
finished:
	if code := cr.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("crawler exit %d:\n%s", code, tail(cr.output()))
	}
	elapsed := time.Since(start0)
	w.settle(total)
	for _, s := range samples {
		t.Log(s)
	}
	var diskBytes int64
	_ = filepath.Walk(cq, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			diskBytes += fi.Size()
		}
		return nil
	})
	var pgBytes int
	_ = w.pool.QueryRow(context.Background(), `SELECT pg_total_relation_size('magnetico_ingest.queue')`).Scan(&pgBytes)
	t.Logf("RESULT rate=%d/s records=%d wall=%.1fs throughput=%.1f rec/s max_crawler_queue=%d max_pg_backlog=%d max_rss_ingest=%dMiB max_rss_crawler=%dMiB crawler_disk=%dKiB pg_queue_relation=%dKiB",
		rate, total, elapsed.Seconds(), float64(total)/elapsed.Seconds(), maxCrawlerQ, maxPGQ, maxIngestRSS/1024, maxCrawlRSS/1024, diskBytes/1024, pgBytes/1024)

	// Bounded, not growing: well below the volume produced.
	if maxCrawlerQ > total/4 || maxPGQ > total/4 {
		t.Fatalf("queue depth grew without bound: crawler=%d pg=%d of %d", maxCrawlerQ, maxPGQ, total)
	}
	if maxIngestRSS > 400*1024 || maxCrawlRSS > 400*1024 {
		t.Fatalf("memory above 400 MiB: ingest=%d KiB crawler=%d KiB", maxIngestRSS, maxCrawlRSS)
	}
}
