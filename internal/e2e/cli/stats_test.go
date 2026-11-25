package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Real binaries, loopback only, synthetic data: the end-to-end qualification
// scenarios of the statistics subsystem. Every scenario ends with a graceful
// stop (the final statistics flush) unless it is about a hard kill.

func TestStatsMultiCrawlerCountersAndInvariants(t *testing.T) {
	s := newStatsWorld(t)
	for _, n := range []string{"alpha", "bravo", "charlie"} {
		s.crawler(n)
	}
	s.startIngest()
	// Overlapping ranges: [1,300] [151,450] [301,600] → 600 distinct torrents.
	res := s.runCrawlers(map[string][2]int{"alpha": {1, 300}, "bravo": {151, 300}, "charlie": {301, 300}})
	s.waitStored(600)
	s.stopIngest()

	received, ack, refused := s.sum("records_received"), s.sum("records_acknowledged"), s.sum("records_refused")
	enq, dup := s.sum("records_enqueued"), s.sum("admission_duplicates")
	// Conservation at quiescence: the load stopped and every handler returned.
	if received != ack+refused || refused != 0 {
		t.Errorf("received %d, acknowledged %d, refused %d: received must equal acknowledged + refused", received, ack, refused)
	}
	if ack != enq+dup {
		t.Errorf("acknowledged %d != enqueued %d + duplicates %d", ack, enq, dup)
	}
	settled := s.sum("settled_stored_new") + s.sum("settled_stored_existing") + s.sum("settled_quarantined")
	if settled != enq {
		t.Errorf("settled %d != enqueued %d with an empty queue", settled, enq)
	}
	if got := s.sum("torrents_created"); got != 600 || s.n(`SELECT count(*) FROM magneticod.torrents`) != 600 {
		t.Errorf("torrents_created %d, stored %d, want 600", got, s.n(`SELECT count(*) FROM magneticod.torrents`))
	}
	if s.sum("rejected_auth")+s.sum("rejected_unavailable_auth")+s.sum("auth_storage_failures")+s.sum("auth_busy") != 0 {
		t.Error("a request was rejected although every client had a valid credential")
	}
	// Steady-state authentication: at most one database lookup per credential per 60 s.
	if max := 3 * (int(time.Since(s.t0)/time.Minute) + 1); s.sum("auth_db_lookups") < 3 || s.sum("auth_db_lookups") > max {
		t.Errorf("auth_db_lookups %d, want 3..%d", s.sum("auth_db_lookups"), max)
	}
	// Per crawler: its own counters add up to the global ones.
	var sumRecv, sumEnq, sumDup int
	for name, r := range res {
		a := s.activity(name)
		if !a.found || !a.submitted || a.lastIP != "127.0.0.1" || a.bytes == 0 {
			t.Errorf("%s: activity %+v", name, a)
		}
		if a.received != r.acked || a.received != a.enqueued+a.duplicate {
			t.Errorf("%s: received %d, client-acknowledged %d, enqueued %d + duplicate %d", name, a.received, r.acked, a.enqueued, a.duplicate)
		}
		sumRecv, sumEnq, sumDup = sumRecv+a.received, sumEnq+a.enqueued, sumDup+a.duplicate
	}
	if sumRecv != received || sumEnq != enq || sumDup != dup {
		t.Errorf("per-crawler sums %d/%d/%d differ from the totals %d/%d/%d", sumRecv, sumEnq, sumDup, received, enq, dup)
	}
	if n := s.n(`SELECT count(*) FROM magnetico_stats.ingest_instances`); n != 1 {
		t.Errorf("%d instance rows, want 1", n)
	}
	// What the administrator sees.
	mkUser(t, &harnessWorld{t: t, bin: s.bin, dsn: s.dsn}, "bob", "admin")
	web := startWeb(t, s.bin, s.dsn, "http")
	bob := newBrowser(t, web.addr)
	if resp := bob.login("bob", adminPW); resp.StatusCode != 303 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if resp := bob.get("/admin/crawlers"); resp.StatusCode != 200 {
		t.Fatalf("crawlers page: %d", resp.StatusCode)
	}
	for _, want := range []string{"alpha", "bravo", "charlie", "127.0.0.1", "300"} {
		if !strings.Contains(bob.body, want) {
			t.Errorf("the crawlers page lacks %q", want)
		}
	}
	if resp := bob.get("/admin/api/overview"); resp.StatusCode != 200 || !strings.Contains(bob.body, "records") {
		t.Errorf("overview: %d %.200s", resp.StatusCode, bob.body)
	}
	if strings.Contains(s.ing.out.String(), "mgc_") {
		t.Error("a credential reached the ingest log")
	}
}

func TestStatsCredentialLifecycleIsolatedAndBoundedBy60Seconds(t *testing.T) {
	t.Parallel()
	s := newStatsWorld(t)
	for _, n := range []string{"a", "b", "c"} {
		s.crawler(n)
	}
	s.startIngest()
	oldB, oldC := s.tokens["b"], s.tokens["c"]
	calls := map[string]int{} // successful (200) existence requests per crawler name
	unauthorized := 0         // 401 answers seen
	check := func(name, tok string, want int) int {
		st := existsStatus(s.addr, tok)
		if st == 200 {
			calls[name]++
		}
		if st == http.StatusUnauthorized {
			unauthorized++
		}
		if want != 0 && st != want {
			t.Errorf("%s: status %d, want %d", name, st, want)
		}
		return st
	}
	for _, n := range []string{"a", "b", "c"} {
		check(n, s.tokens[n], 200)
	}
	// Rotate b and disable c at the same moment.
	out, errOut, code := runIngestCrawlerBinary(t, s.bin, s.dsn, nil, "rotate-token", "b")
	if code != 0 {
		t.Fatalf("rotate: %d %s%s", code, out, errOut)
	}
	newB := ""
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "token: "); ok {
			newB = strings.TrimSpace(v)
		}
	}
	if newB == "" || newB == oldB {
		t.Fatalf("no new token: %q", out)
	}
	if _, errOut, code = runIngestCrawlerBinary(t, s.bin, s.dsn, nil, "disable", "c"); code != 0 {
		t.Fatalf("disable: %d %s", code, errOut)
	}
	begin := time.Now()
	check("b", newB, 200) // the new credential works at once (a cache miss, one lookup)
	rejectedB, rejectedC := time.Duration(0), time.Duration(0)
	for rejectedB == 0 || rejectedC == 0 {
		if time.Since(begin) > 70*time.Second {
			t.Fatalf("after 70 s: the old b token rejected after %v, the disabled c after %v", rejectedB, rejectedC)
		}
		check("a", s.tokens["a"], 200) // unaffected throughout
		if rejectedB == 0 && check("b", oldB, 0) == http.StatusUnauthorized {
			rejectedB = time.Since(begin)
		}
		if rejectedC == 0 && check("c", oldC, 0) == http.StatusUnauthorized {
			rejectedC = time.Since(begin)
		}
		time.Sleep(time.Second)
	}
	t.Logf("propagation: old token of b rejected after %v, disabled c after %v", rejectedB.Round(time.Second), rejectedC.Round(time.Second))
	if rejectedB > 62*time.Second || rejectedC > 62*time.Second {
		t.Errorf("a revocation took longer than the 60 s cache bound: %v / %v", rejectedB, rejectedC)
	}
	check("b", newB, 200)
	// Enable c again: its old token works again within the same bound.
	if _, errOut, code = runIngestCrawlerBinary(t, s.bin, s.dsn, nil, "enable", "c"); code != 0 {
		t.Fatalf("enable: %d %s", code, errOut)
	}
	begin = time.Now()
	for check("c", oldC, 0) != 200 {
		if time.Since(begin) > 70*time.Second {
			t.Fatal("c is still refused 70 s after enabling")
		}
		time.Sleep(time.Second)
	}
	t.Logf("c accepted again after %v", time.Since(begin).Round(time.Second))
	check("a", s.tokens["a"], 200)
	s.stopIngest()

	// The statistics of every crawler stayed its own, and every request is accounted for exactly.
	for _, n := range []string{"a", "b", "c"} {
		if a := s.activity(n); !a.found || a.existsReq != calls[n] {
			t.Errorf("%s: %d existence requests persisted, %d answered 200", n, a.existsReq, calls[n])
		}
	}
	if a := s.activity("a"); a.rejected != 0 {
		t.Errorf("crawler a, which was never revoked, has %d rejected requests", a.rejected)
	}
	if got := s.sum("rejected_auth"); got != unauthorized {
		t.Errorf("rejected_auth %d, 401 answers seen %d", got, unauthorized)
	}
	if s.sum("rejected_unavailable_auth")+s.sum("auth_storage_failures") != 0 {
		t.Error("an authentication was refused for a storage reason although the database was healthy")
	}
	if log := s.ing.out.String(); strings.Contains(log, "mgc_") {
		t.Error("a credential reached the ingest log")
	}
}

// Counters persist across a graceful restart and a SIGKILL: an event is counted
// once (a kill loses at most what was not flushed yet, never double-counts), and
// a restarted process continues in the same bucket.
func TestStatsSurviveKillAndGracefulRestarts(t *testing.T) {
	s := newStatsWorld(t)
	s.crawler("alpha")
	s.startIngest()
	s.runCrawlers(map[string][2]int{"alpha": {1, 100}})
	s.waitStored(100)
	s.stopIngest()
	n1 := s.sum("records_received")
	if n1 != 100 || s.sum("records_acknowledged") != 100 {
		t.Fatalf("after the graceful run: received %d, acknowledged %d, want 100", n1, s.sum("records_acknowledged"))
	}

	s.startIngest()
	s.runCrawlers(map[string][2]int{"alpha": {1000, 100}})
	s.waitStored(200)
	s.killIngest()
	r2 := s.sum("records_received")
	if r2 < n1 || r2 > n1+100 {
		t.Fatalf("after SIGKILL: received %d, want %d..%d (a kill may lose the unflushed events, never add)", r2, n1, n1+100)
	}

	s.startIngest()
	s.runCrawlers(map[string][2]int{"alpha": {2000, 100}})
	s.waitStored(300)
	s.stopIngest()
	if got := s.sum("records_received"); got != r2+100 {
		t.Errorf("after the third run: received %d, want exactly %d: the killed run's events are counted once or lost, never later", got, r2+100)
	}
	if got, ack := s.sum("records_received"), s.sum("records_acknowledged")+s.sum("records_refused"); got != ack {
		t.Errorf("received %d != acknowledged + refused %d at quiescence", got, ack)
	}
	if n := s.n(`SELECT count(*) FROM magnetico_stats.ingest_instances`); n != 3 {
		t.Errorf("%d instance rows, want 3 (one per process)", n)
	}
	if n := s.n(`SELECT count(*) FROM magnetico_stats.agg_hour WHERE bucket_start > now()`); n != 0 {
		t.Errorf("%d buckets in the future", n)
	}
	if got := s.sum("torrents_created"); got < 200 || got > 300 {
		t.Errorf("torrents_created %d", got)
	}
}

// ---- sabotage: statistics fail, PostgreSQL stays healthy ---------------------------------------

// paced posts batches of per records every tick for d from one goroutine.
func (sb *submitter) paced(d time.Duration, per int, tick time.Duration, maxLatency *atomic.Int64) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		begin := time.Now()
		sb.batch(per)
		if l := time.Since(begin).Milliseconds(); maxLatency != nil {
			for {
				cur := maxLatency.Load()
				if l <= cur || maxLatency.CompareAndSwap(cur, l) {
					break
				}
			}
		}
		time.Sleep(max(0, tick-time.Since(begin)))
	}
}

func TestStatsFailuresNeverAffectAdmissionOrAuthentication(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(t *testing.T, s *statsWorld)
	}{
		{"schema dropped", func(t *testing.T, s *statsWorld) { sqlExec(t, s.dsn, `DROP SCHEMA magnetico_stats CASCADE`) }},
		{"grants revoked", func(t *testing.T, s *statsWorld) {
			sqlExec(t, s.dsn, `REVOKE ALL ON ALL TABLES IN SCHEMA magnetico_stats FROM `+s.appRole())
			sqlExec(t, s.dsn, `REVOKE USAGE ON SCHEMA magnetico_stats FROM `+s.appRole())
		}},
		{"tables locked", func(t *testing.T, s *statsWorld) {
			c, err := pgx.Connect(context.Background(), s.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Exec(context.Background(), `BEGIN`); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Exec(context.Background(), `LOCK TABLE magnetico_stats.agg_hour, magnetico_stats.ingest_instances, magnetico_stats.snapshot, magnetico_stats.crawler_activity, magnetico_stats.shared_state IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close(context.Background()) })
		}},
		{"statements exceed their timeout", func(t *testing.T, s *statsWorld) {
			sqlExec(t, s.dsn, `CREATE FUNCTION magnetico_stats.slow() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(30); RETURN NEW; END $$`)
			for _, tbl := range []string{"agg_hour", "ingest_instances", "snapshot", "crawler_activity", "shared_state"} {
				sqlExec(t, s.dsn, `CREATE TRIGGER slow BEFORE INSERT OR UPDATE ON magnetico_stats.`+tbl+` FOR EACH ROW EXECUTE FUNCTION magnetico_stats.slow()`)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStatsWorld(t)
			tok := s.crawler("alpha")
			s.startIngest()
			sb := &submitter{addr: s.addr, token: tok}
			var maxHealthy, maxBroken atomic.Int64
			sb.paced(6*time.Second, 20, 100*time.Millisecond, &maxHealthy)
			c.break_(t, s)
			// Long enough for heartbeats (10 s) and queue samples (10 s) to hit the failure.
			sb.paced(30*time.Second, 20, 100*time.Millisecond, &maxBroken)
			if bad := sb.unavailable.Load() + sb.other.Load() + sb.transport.Load() + sb.unauth.Load(); bad != 0 {
				t.Errorf("%d records or requests were refused or failed while only the statistics were broken", bad)
			}
			t.Logf("slowest request: %d ms healthy, %d ms with the failure", maxHealthy.Load(), maxBroken.Load())
			if maxBroken.Load() > 3000 {
				t.Errorf("a request took %d ms while the statistics were failing", maxBroken.Load())
			}
			s.waitStored(int(sb.ok.Load()))
			s.stopIngest() // the final flush is bounded and its failure is not an error exit
			log := s.ing.out.String()
			if strings.Contains(log, "panic") {
				t.Errorf("a panic in the ingest log:\n%s", log)
			}
			for _, topic := range []string{"statistics: persisting failed", "statistics: publishing the heartbeat failed", "statistics: sampling the queue failed"} {
				if n := strings.Count(log, topic); n > 1 {
					t.Errorf("%q logged %d times, want at most once per state change", topic, n)
				}
			}
			if strings.Contains(log, "mgc_") {
				t.Error("a credential reached the log")
			}
		})
	}
}

// ---- outage and flaky link -----------------------------------------------------------------------

// conserved checks the persisted counters against what the clients observed:
// received = acknowledged + refused exactly (quiescence), nothing acknowledged
// that no client was told about except the replies lost on the wire, nothing
// double-counted.
func conserved(t *testing.T, s *statsWorld, sb *submitter) {
	t.Helper()
	received, ack, refused := s.sum("records_received"), s.sum("records_acknowledged"), s.sum("records_refused")
	t.Logf("clients: ok %d, 503 %d, transport errors %d, other %d; persisted: received %d, acknowledged %d, refused %d",
		sb.ok.Load(), sb.unavailable.Load(), sb.transport.Load(), sb.other.Load(), received, ack, refused)
	if received != ack+refused {
		t.Errorf("received %d != acknowledged %d + refused %d", received, ack, refused)
	}
	if int64(ack) < sb.ok.Load() || int64(ack) > sb.ok.Load()+sb.transport.Load() {
		t.Errorf("acknowledged %d, clients saw %d acknowledged (+ at most %d replies lost on the wire)", ack, sb.ok.Load(), sb.transport.Load())
	}
	if stored := s.n(`SELECT count(*) FROM magneticod.torrents`); stored > ack {
		t.Errorf("%d torrents stored but only %d acknowledged", stored, ack)
	}
}

func TestStatsConservedAcrossAFullPostgresOutageSpanningAFlush(t *testing.T) {
	t.Parallel()
	s := newStatsWorld(t)
	tok := s.crawler("alpha")
	s.startIngest()
	sb := &submitter{addr: s.addr, token: tok}
	t.Cleanup(func() { s.setAllowConnections(true) })
	sb.paced(15*time.Second, 20, 100*time.Millisecond, nil)
	s.setAllowConnections(false)
	// The outage covers the flush at 60 s after the start and the expiry of the 60 s authentication cache.
	sb.paced(55*time.Second, 20, 100*time.Millisecond, nil)
	if s.ing.cmd.ProcessState != nil {
		t.Fatalf("ingest died in the outage:\n%s", s.ing.out.String())
	}
	if sb.unavailable.Load() == 0 {
		t.Error("no 503 during the outage: admission cannot work without the database")
	}
	s.setAllowConnections(true)
	sb.paced(15*time.Second, 20, 100*time.Millisecond, nil)
	if sb.ok.Load() < 1000 {
		t.Fatalf("admission did not recover: %d records admitted", sb.ok.Load())
	}
	s.waitStored(int(sb.ok.Load()))
	s.stopIngest()
	conserved(t, s, sb)
	if s.sum("rejected_storage_unavailable")+s.sum("rejected_unavailable_auth") == 0 {
		t.Error("the outage left no rejected_* counter")
	}
	if strings.Count(s.ing.out.String(), "statistics: persisting failed") > 2 {
		t.Errorf("the flush failure was logged repeatedly")
	}
}

// cutter is a TCP proxy to PostgreSQL that can add latency and cut every
// connection: a network that delays and drops around COMMIT.
type cutter struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
	delay  atomic.Int64 // ns added to every chunk server → client
}

func newCutter(t *testing.T, dsn string) *cutter {
	t.Helper()
	u, _ := url.Parse(dsn)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &cutter{ln: ln, target: u.Host}
	t.Cleanup(func() { _ = ln.Close(); c.cutAll() })
	go func() {
		for {
			cl, err := ln.Accept()
			if err != nil {
				return
			}
			sv, err := net.Dial("tcp", c.target)
			if err != nil {
				_ = cl.Close()
				continue
			}
			c.mu.Lock()
			c.conns = append(c.conns, cl, sv)
			c.mu.Unlock()
			go func() { _, _ = io.Copy(sv, cl); _ = sv.Close() }()
			go func() {
				buf := make([]byte, 32<<10)
				for {
					n, err := sv.Read(buf)
					if n > 0 {
						if d := c.delay.Load(); d > 0 {
							time.Sleep(time.Duration(d))
						}
						if _, werr := cl.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						_ = cl.Close()
						return
					}
				}
			}()
		}
	}()
	return c
}

func (c *cutter) cutAll() {
	c.mu.Lock()
	cs := c.conns
	c.conns = nil
	c.mu.Unlock()
	for _, x := range cs {
		_ = x.Close()
	}
}

func TestStatsConservedWhenTheLinkDelaysAndDropsAroundTheFlush(t *testing.T) {
	t.Parallel()
	s := newStatsWorld(t)
	tok := s.crawler("alpha")
	px := newCutter(t, s.dsn)
	u, _ := url.Parse(s.dsn)
	u.Host = px.ln.Addr().String()
	direct := s.dsn
	s.dsn = u.String()
	s.startIngest() // the ingest talks through the proxy
	s.dsn = direct  // the test inspects the database directly
	sb := &submitter{addr: s.addr, token: tok}
	sb.paced(52*time.Second, 20, 100*time.Millisecond, nil)
	// 52 s .. 80 s: every connection is cut every 700 ms and every reply is 250 ms late, around the flush at 60 s.
	px.delay.Store(int64(250 * time.Millisecond))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(700 * time.Millisecond):
				px.cutAll()
			}
		}
	}()
	sb.paced(28*time.Second, 20, 100*time.Millisecond, nil)
	close(stop)
	wg.Wait()
	px.delay.Store(0)
	sb.paced(10*time.Second, 20, 100*time.Millisecond, nil)
	if sb.ok.Load() < 1000 {
		t.Fatalf("only %d records admitted", sb.ok.Load())
	}
	s.waitStored(int(sb.ok.Load()))
	s.stopIngest()
	conserved(t, s, sb)
}

// The documented reinitialisation of an older development ingest schema
// (UPGRADE.md) through the real binary: only magnetico_ingest is dropped and
// recreated; the torrents, the web accounts and the statistics are untouched,
// and orphaned per-crawler activity of the old registry is not shown.
func TestDocumentedReinitialisationLeavesTorrentsWebAndStatsUntouched(t *testing.T) {
	s := newStatsWorld(t)
	s.crawler("old")
	s.startIngest()
	s.runCrawlers(map[string][2]int{"old": {1, 50}})
	s.waitStored(50)
	s.stopIngest()
	mkUser(t, &harnessWorld{t: t, bin: s.bin, dsn: s.dsn}, "bob", "admin")
	digest := func() string {
		return sqlString(t, s.dsn, `SELECT md5(coalesce((SELECT string_agg(x::text, ',' ORDER BY info_hash) FROM magneticod.torrents x), ''))
 || md5(coalesce((SELECT string_agg(u.username || u.role, ',' ORDER BY id) FROM magnetico_web.users u), ''))
 || md5(coalesce((SELECT string_agg(a::text, ',' ORDER BY bucket_start) FROM magnetico_stats.agg_hour a), ''))`)
	}
	before := digest()
	// An older development schema: no crawlers table, so the service refuses it.
	sqlExec(t, s.dsn, `DROP TABLE magnetico_ingest.crawlers`)
	// The documented steps.
	sqlExec(t, s.dsn, `DROP SCHEMA magnetico_ingest CASCADE`)
	if out, code := runWebSchemaInit(t, s.bin, s.dsn, "ingest-schema-init"); code != 0 {
		t.Fatalf("ingest-schema-init: %d %s", code, out)
	}
	s.crawler("fresh")
	if after := digest(); after != before {
		t.Fatalf("the reinitialisation changed torrents, accounts or statistics")
	}
	s.startIngest() // starts cleanly on the new schema
	s.runCrawlers(map[string][2]int{"fresh": {5000, 20}})
	s.waitStored(70)
	s.stopIngest()
	web := startWeb(t, s.bin, s.dsn, "http")
	bob := newBrowser(t, web.addr)
	if resp := bob.login("bob", adminPW); resp.StatusCode != 303 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	bob.get("/admin/crawlers")
	if !strings.Contains(bob.body, "fresh") || strings.Contains(bob.body, ">old<") {
		t.Errorf("the crawlers page after the reinitialisation:\n%s", bob.body)
	}
}
