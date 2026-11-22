package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Multi-crawler stress qualification (Task 06, final corrections).
//
// Opt-in: runs only when MAGNETICO_E2E_MULTI_DIR names an output directory.
// Three crawlerd processes, each with its own durable SQLite queue, deliver to
// one ingestd through a fault-injecting proxy:
//
//   - overlapping (crawler 0/1 and 1/2) and disjoint (0/2) infohash ranges;
//   - steady aggregate traffic (default 3 x 15 = 45 records/s), then a burst
//     (default 3 x 150 records/s for 3 x 1500 records);
//   - a brief ingest outage (graceful stop, restart on the same address);
//   - graceful restart of crawler 1 while it holds pending records;
//   - lost acknowledgements (the proxy drops 202s after the upstream commit).
//
// Knobs (all optional):
//
//	MAGNETICO_E2E_MULTI_STEADY_S      steady phase length per crawler (default 240)
//	MAGNETICO_E2E_MULTI_RATE_EACH     records/s per crawler in the steady phase (default 15)
//	MAGNETICO_E2E_MULTI_BURST_EACH    burst records per crawler (default 1500)
//	MAGNETICO_E2E_MULTI_BURST_RATE    burst rate per crawler (default 150)
//	MAGNETICO_E2E_MULTI_OUTAGE_S      ingest outage length (default 20)
//	MAGNETICO_E2E_MULTI_GOMEMLIMIT    ingest GOMEMLIMIT (default 256MiB)
//	MAGNETICO_E2E_MULTI_MAX_MIN       hard wall-clock stop (default 15)
//	MAGNETICO_E2E_MULTI_MIN_FREE_GIB  hard stop when the filesystem has less free space (default 12)
//	MAGNETICO_E2E_MULTI_DISK_MIB      hard stop when the run directory exceeds this (default 2048)
//
// Early stops (diagnostics preserved, test fails): wall-clock budget, free
// space, disk budget, ingest anonymous RSS > 1 GiB, ingest dead, a PostgreSQL
// backlog with no persistence progress for 120 s outside the outage, more than
// 200 ingest goroutines.

type atomicDrops struct{ n atomic.Int32 }

func dirBytes(dir string) int64 {
	var n int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}

func TestE2EMultiCrawlerStress(t *testing.T) {
	dir := os.Getenv("MAGNETICO_E2E_MULTI_DIR")
	if dir == "" {
		t.Skip("set MAGNETICO_E2E_MULTI_DIR to run the multi-crawler stress qualification")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	steadyS := envIntDefault("MAGNETICO_E2E_MULTI_STEADY_S", 240)
	rateEach := envIntDefault("MAGNETICO_E2E_MULTI_RATE_EACH", 15)
	burstEach := envIntDefault("MAGNETICO_E2E_MULTI_BURST_EACH", 1500)
	burstRate := envIntDefault("MAGNETICO_E2E_MULTI_BURST_RATE", 150)
	outage := time.Duration(envIntDefault("MAGNETICO_E2E_MULTI_OUTAGE_S", 20)) * time.Second
	maxWall := time.Duration(envIntDefault("MAGNETICO_E2E_MULTI_MAX_MIN", 15)) * time.Minute
	minFree := envIntDefault("MAGNETICO_E2E_MULTI_MIN_FREE_GIB", 12)
	diskBudget := int64(envIntDefault("MAGNETICO_E2E_MULTI_DISK_MIB", 2048)) << 20
	goMemLimit := os.Getenv("MAGNETICO_E2E_MULTI_GOMEMLIMIT")
	if goMemLimit == "" {
		goMemLimit = "256MiB"
	}

	steadyEach := rateEach * steadyS
	totalEach := steadyEach + burstEach
	// Crawler k covers ids [from_k, from_k+totalEach): neighbours overlap by a
	// third, crawlers 0 and 2 are disjoint.
	const idBase = 1_000_000
	froms := []int{idBase, idBase + totalEach*2/3, idBase + totalEach*4/3}
	union := map[int]bool{}
	for _, f := range froms {
		for i := 0; i < totalEach; i++ {
			union[f+i] = true
		}
	}
	want := len(union)
	t.Logf("plan: %d crawlers x %d records (steady %d @ %d/s, burst %d @ %d/s), union %d distinct torrents, GOMEMLIMIT=%s",
		len(froms), totalEach, steadyEach, rateEach, burstEach, burstRate, want, goMemLimit)

	w := newWorld(t)
	addr := freePort(t)
	var ingests []*edgeIngest
	curIng := w.startEdgeIngest(addr, goMemLimit, "")
	ingests = append(ingests, curIng)

	// Fault-injecting proxy: forwards to ingest; drops the next N 202 responses
	// after the upstream commit; answers 502 while ingest is down.
	var drops atomicDrops
	var dropped atomic.Int32
	target, _ := url.Parse("http://" + addr)
	proxy := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, _ := http.NewRequest(r.Method, target.String()+r.URL.Path, bytes.NewReader(body))
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			rw.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if strings.HasSuffix(r.URL.Path, "/torrents") && resp.StatusCode == 202 && drops.n.Add(-1) >= 0 {
			dropped.Add(1)
			hj, _ := rw.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		if drops.n.Load() < 0 {
			drops.n.Store(0)
		}
		rw.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(rw, resp.Body)
	}))
	defer proxy.Close()

	// Crawlers.
	phasesFor := func(doneOrdinary int) string {
		if doneOrdinary < steadyEach {
			return fmt.Sprintf("%d:%d,%d:%d", rateEach, steadyEach-doneOrdinary, burstRate, burstEach)
		}
		return fmt.Sprintf("%d:%d", burstRate, totalEach-doneOrdinary)
	}
	qdirs := make([]string, len(froms))
	crs := make([]*proc, len(froms))
	for k := range froms {
		qdirs[k] = filepath.Join(dir, fmt.Sprintf("queue-%d", k))
		crs[k] = w.crawlerd(qdirs[k], proxy.URL, "-from", strconv.Itoa(froms[k]), "-phases", phasesFor(0))
	}

	csv, err := os.Create(filepath.Join(dir, "timeseries.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer csv.Close()
	fmt.Fprintln(csv, "t_s,event,torrents,pg_queue,used_bytes,ing_rss_kib,ing_anon_kib,ing_live_kib,ing_goroutines,ing_cpu_ms,ing_gc_cpu_ms,"+
		"cr0_rss_kib,cr1_rss_kib,cr2_rss_kib,cr_cpu_ms_sum,cr_queue_sum,cr_queue_bytes_sum,dir_mib,free_gib,dropped_acks")

	t0 := time.Now()
	var (
		peakRSS, peakAnon, peakLive, peakCrawlerRSS int
		lastProgress                                = time.Now()
		lastTorrents                                = -1
		inOutage                                    bool
		recoveredAt                                 time.Duration
		restartedIngestAt                           time.Duration
		lastEvent                                   string
		steadyCPU0, steadyCPUWall                   int
		peakQueueSum                                int
		peakPG                                      int
	)
	summaryFail := ""
	stopRun := func(format string, a ...any) {
		t.Helper()
		summaryFail = fmt.Sprintf(format, a...)
		_ = os.WriteFile(filepath.Join(dir, "EARLY-STOP.txt"), []byte(summaryFail+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "ingest-current.log"), []byte(curIng.p.output()), 0o644)
		for k, c := range crs {
			_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("crawler-%d-current.log", k)), []byte(c.output()), 0o644)
		}
		if curIng.alive() {
			fetchTo(t, "http://"+curIng.pprof+"/debug/pprof/heap", filepath.Join(dir, "stop-heap.pb.gz"))
			fetchTo(t, "http://"+curIng.pprof+"/debug/pprof/goroutine?debug=2", filepath.Join(dir, "stop-goroutines.txt"))
		}
		t.Fatalf("EARLY STOP: %s", summaryFail)
	}

	// ingestCPU sums cumulative CPU over every ingest instance so far.
	ingestCPU := func() (cpu, gc int) {
		for _, ing := range ingests {
			all := cpuRe.FindAllStringSubmatch(ing.p.output(), -1)
			if len(all) == 0 {
				continue
			}
			m := all[len(all)-1]
			u, _ := strconv.Atoi(m[1])
			s, _ := strconv.Atoi(m[2])
			g, _ := strconv.Atoi(m[3])
			cpu += u + s
			gc += g
		}
		return
	}

	sample := func() {
		t.Helper()
		el := time.Since(t0)
		torrents, pgq, used := w.torrents(), w.pgQueue(), w.usedBytes()
		peakPG = max(peakPG, pgq)
		var ms memSample
		if curIng.alive() {
			ms, _ = lastMem(curIng.p.output())
		}
		peakRSS, peakAnon, peakLive = max(peakRSS, ms.rss), max(peakAnon, ms.anon), max(peakLive, ms.live)
		cpu, gc := ingestCPU()
		var crRSS [3]int
		crCPU, qSum, qBytes := 0, 0, 0
		for k, c := range crs {
			select {
			case <-c.done:
			default:
				crRSS[k] = rssKiB(c)
				crCPU += procCPUms(c.cmd.Process.Pid)
			}
			peakCrawlerRSS = max(peakCrawlerRSS, crRSS[k])
			if m := statsRe.FindAllStringSubmatch(c.output(), -1); len(m) > 0 {
				q, _ := strconv.Atoi(m[len(m)-1][1])
				b, _ := strconv.Atoi(m[len(m)-1][2])
				qSum += q
				qBytes += b
			}
		}
		peakQueueSum = max(peakQueueSum, qSum)
		free := freeGiB(dir)
		dsz := dirBytes(dir)
		fmt.Fprintf(csv, "%d,%s,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d\n", int(el.Seconds()), lastEvent, torrents, pgq, used,
			ms.rss, ms.anon, ms.live, ms.goroutines, cpu, gc, crRSS[0], crRSS[1], crRSS[2], crCPU, qSum, qBytes, dsz>>20, free, dropped.Load())
		lastEvent = ""
		if restartedIngestAt > 0 && recoveredAt == 0 && qSum <= 30 {
			recoveredAt = el
		}

		// early-stop conditions
		switch {
		case el > maxWall:
			stopRun("wall-clock budget %v exceeded", maxWall)
		case free >= 0 && free < minFree:
			stopRun("only %d GiB free (limit %d GiB)", free, minFree)
		case dsz > diskBudget:
			stopRun("run directory is %d MiB, budget %d MiB", dsz>>20, diskBudget>>20)
		case ms.anon > 1<<20:
			stopRun("ingest anonymous RSS %d MiB exceeds 1 GiB", ms.anon>>10)
		case ms.goroutines > goroutineStop:
			stopRun("ingest has %d goroutines (limit %d)", ms.goroutines, goroutineStop)
		case !inOutage && !curIng.alive():
			stopRun("ingest exited unexpectedly:\n%s", tail(curIng.p.output()))
		}
		if torrents != lastTorrents || pgq == 0 {
			lastProgress, lastTorrents = time.Now(), torrents
		}
		if !inOutage && pgq > 0 && time.Since(lastProgress) > 120*time.Second {
			stopRun("%d rows queued in PostgreSQL with no persistence progress for 120 s", pgq)
		}
		if d := w.usageDrift(); d != 0 {
			stopRun("usage accounting drifted by %d bytes", d)
		}
		if q := w.pgQuar(); q != 0 {
			stopRun("%d rows in the PostgreSQL quarantine", q)
		}
	}
	sleepSampling := func(d time.Duration) {
		t.Helper()
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			time.Sleep(min(5*time.Second, time.Until(end)))
			sample()
		}
	}
	allDone := func() bool {
		for _, c := range crs {
			select {
			case <-c.done:
			default:
				return false
			}
		}
		return true
	}

	// Timeline.
	outageAt := time.Duration(steadyS) * time.Second * 3 / 10
	ackAt := time.Duration(steadyS) * time.Second * 55 / 100
	var outageStart, outageEnd time.Duration
	var restartInfo string

	for time.Since(t0) < outageAt {
		sleepSampling(5 * time.Second)
	}
	// Ingest outage, graceful stop.
	lastEvent = "ingest-stop"
	inOutage = true
	outageStart = time.Since(t0)
	if code := curIng.stop(t); code != 0 {
		stopRun("ingest exit code %d on graceful stop", code)
	}
	t.Logf("ingest stopped at t=%v with %d rows pending in PostgreSQL", outageStart.Round(time.Second), w.pgQueue())
	sleepSampling(5 * time.Second)

	// Graceful restart of crawler 1 with pending records.
	lastEvent = "crawler1-sigterm"
	crs[1].term()
	if code := crs[1].wait(t, 90*time.Second); code != 0 {
		stopRun("crawler 1 exit code %d on SIGTERM:\n%s", code, tail(crs[1].output()))
	}
	dn := parseDone(t, crs[1].output())
	if dn.queue == 0 {
		stopRun("crawler 1 had no pending records at the restart (done %+v): the scenario did not apply", dn)
	}
	restartInfo = fmt.Sprintf("crawler 1 stopped with %d pending, %d appended, %d acked", dn.queue, dn.appended, dn.acked)
	t.Log(restartInfo)
	lastEvent = "crawler1-restart"
	crs[1] = w.crawlerd(qdirs[1], proxy.URL, "-from", strconv.Itoa(froms[1]+dn.appended), "-phases", phasesFor(dn.appended))

	for time.Since(t0) < outageStart+outage {
		sleepSampling(2 * time.Second)
	}
	lastEvent = "ingest-start"
	curIng = w.startEdgeIngest(addr, goMemLimit, "")
	ingests = append(ingests, curIng)
	inOutage = false
	outageEnd = time.Since(t0)
	restartedIngestAt = outageEnd
	lastProgress = time.Now()
	steadyCPU0, _ = ingestCPU()
	steadyCPUWall = int(outageEnd.Milliseconds())

	for time.Since(t0) < ackAt {
		sleepSampling(5 * time.Second)
	}
	lastEvent = "drop-acks"
	drops.n.Store(8)
	for !allDone() {
		sleepSampling(5 * time.Second)
	}
	crawlersDoneAt := time.Since(t0)
	lastEvent = "crawlers-done"
	for w.torrents() != want || w.pgQueue() != 0 {
		sleepSampling(2 * time.Second)
	}
	pgDrainedAt := time.Since(t0)
	sample()

	// Final verification.
	var crawlerDone []done
	for k, c := range crs {
		if code := c.cmd.ProcessState.ExitCode(); code != 0 {
			t.Fatalf("crawler %d exit %d:\n%s", k, code, tail(c.output()))
		}
		d := parseDone(t, c.output())
		crawlerDone = append(crawlerDone, d)
		if d.queue != 0 || d.quarantine != 0 {
			t.Fatalf("crawler %d left queue=%d quarantine=%d", k, d.queue, d.quarantine)
		}
	}
	if dropped.Load() < 8 {
		t.Fatalf("the proxy dropped %d acknowledgements, want 8", dropped.Load())
	}
	w.settle(want)
	if dup := w.n(`SELECT count(*) - count(DISTINCT info_hash) FROM magneticod.torrents`); dup != 0 {
		t.Fatalf("%d duplicate torrents", dup)
	}
	// Every expected hash is present: count of the expected id range matches.
	if got := w.n(`SELECT count(*) FROM magneticod.torrents WHERE name LIKE 'e2e torrent %'`); got != want {
		t.Fatalf("%d synthetic torrents stored, want %d", got, want)
	}
	for _, ing := range ingests {
		if ing.alive() {
			if code := ing.stop(t); code != 0 {
				t.Fatalf("ingest exit code %d on graceful stop", code)
			}
		}
	}

	// Summary.
	var b strings.Builder
	fmt.Fprintf(&b, "plan: 3 crawlers x %d records (steady %d @ %d/s each = %d/s aggregate; burst %d @ %d/s each = %d/s aggregate), union %d distinct torrents, ingest GOMEMLIMIT=%s\n",
		totalEach, steadyEach, rateEach, 3*rateEach, burstEach, burstRate, 3*burstRate, want, goMemLimit)
	fmt.Fprintf(&b, "result: %d torrents stored (exactly the union), 0 duplicates, PostgreSQL queue 0, quarantine 0, usage drift 0, used_bytes 0\n", w.torrents())
	for k, d := range crawlerDone {
		fmt.Fprintf(&b, "crawler %d final (last process): %+v\n", k, d)
	}
	fmt.Fprintf(&b, "events: ingest stopped t=%ds, restarted t=%ds (outage %ds); %s; proxy dropped %d acknowledgements after commit\n",
		int(outageStart.Seconds()), int(outageEnd.Seconds()), int((outageEnd - outageStart).Seconds()), restartInfo, dropped.Load())
	fmt.Fprintf(&b, "recovery: crawler queues back to <= 30 records %ds after ingest restart; peak crawler queue sum %d records\n",
		int((recoveredAt - restartedIngestAt).Seconds()), peakQueueSum)
	fmt.Fprintf(&b, "timing: crawlers finished t=%ds, PostgreSQL drained t=%ds (drain after last crawler exit %ds); wall %ds\n",
		int(crawlersDoneAt.Seconds()), int(pgDrainedAt.Seconds()), int((pgDrainedAt - crawlersDoneAt).Seconds()), int(time.Since(t0).Seconds()))
	cpu, gc := ingestCPU()
	fmt.Fprintf(&b, "ingest: peak rss=%dMiB anon=%dMiB heap_live=%dMiB, peak PostgreSQL queue %d rows; CPU total %dms (gc %dms) over %ds wall = %.1f%% of one core; since restart %.1f%%\n",
		peakRSS>>10, peakAnon>>10, peakLive>>10, peakPG, cpu, gc, int(time.Since(t0).Seconds()),
		100*float64(cpu)/float64(time.Since(t0).Milliseconds()),
		100*float64(cpu-steadyCPU0)/float64(time.Since(t0).Milliseconds()-int64(steadyCPUWall)))
	fmt.Fprintf(&b, "crawlers: peak RSS %d MiB each\n", peakCrawlerRSS>>10)
	for i, ing := range ingests {
		hwm := memBetween(ing.p.output(), 0, 1<<30)
		fmt.Fprintf(&b, "ingest instance %d: %s\n", i, hwm)
	}
	_ = os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(b.String()), 0o644)
	for i, ing := range ingests {
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("ingest-%d.log", i)), []byte(ing.p.output()), 0o644)
	}
	for k, c := range crs {
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("crawler-%d.log", k)), []byte(c.output()), 0o644)
	}
	t.Log("\n" + b.String())
}
