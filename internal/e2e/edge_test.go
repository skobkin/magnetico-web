package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
	"github.com/skobkin/magnetico-web/internal/ingest/server"
)

// Large-record qualification (Task 06, final corrections).
//
// Opt-in: runs only when MAGNETICO_E2E_EDGE_DIR names an output directory.
// Bounded data volumes, explicit per-stage timeouts, test-only wrappers
// (ingestd, crawlerd); the service code path is the production one.
//
//	MAGNETICO_E2E_EDGE_DIR=/home/herdr/qual/task06-edge/run1 \
//	MAGNETICO_E2E_EDGE_GOMEMLIMIT=256MiB \        # optional Go runtime target
//	MAGNETICO_E2E_EDGE_DOCKER_MEM=512m \          # optional hard container limit (cgroup v2)
//	go test -count=1 -p 1 -timeout 40m -run TestE2ELargeRecordQualification -v ./internal/e2e/
//
// With a container limit, ingestd runs in a `postgres:17` container (the only
// locally cached image; nothing is pulled) as a statically linked binary on
// the host network; OOM kills are read from the container's cgroup and are
// test failures.

const edgeStageTimeoutDefault = 300 // seconds

// jsonRecord builds the JSON of one valid record with exactly target bytes
// (target 0 = a small ordinary record). version lets tests build wrong-version
// records of the same size. All sizes are 1 so total_size equals the file
// count; paths are ~60 bytes so 16 MiB stays below the 250 000-file limit.
func jsonRecord(id, target, version int) []byte {
	head := func(n int) string {
		return fmt.Sprintf(`{"v":%d,"info_hash":"%040x","name":"edge %d","total_size":%d,"discovered_on":1700000000,"files":[`,
			version, id, id, n)
	}
	path := func(i int) string {
		return fmt.Sprintf("dir%04d/%s/file-%07d.bin", i%1000, strings.Repeat("p", 38), i)
	}
	entry := func(p string) string { return `{"path":"` + p + `","size":1}` }
	n := 1
	if target > 0 {
		base := len(head(0)) + 2
		per := len(entry(path(0))) + 1
		n = (target - base) / per
		for len(head(n))+2+n*per-1 > target {
			n--
		}
	}
	var b bytes.Buffer
	b.WriteString(head(n))
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		p := path(i)
		if i == n-1 && target > 0 {
			p += strings.Repeat("x", target-(b.Len()+len(entry(p))+2))
		}
		b.WriteString(entry(p))
	}
	b.WriteString("]}")
	if target > 0 && b.Len() != target {
		panic(fmt.Sprintf("jsonRecord: built %d bytes, wanted %d", b.Len(), target))
	}
	return b.Bytes()
}

// manyFilesRecord is a valid record with n tiny files (limit test on file count).
func manyFilesRecord(id, n int) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"v":1,"info_hash":"%040x","name":"edge files %d","total_size":%d,"discovered_on":1700000000,"files":[`, id, id, n)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"path":"f%06d","size":1}`, i)
	}
	b.WriteString("]}")
	return b.Bytes()
}

func envelope(recs ...[]byte) []byte {
	return append(append([]byte(`{"records":[`), bytes.Join(recs, []byte(","))...), `]}`...)
}

// postRaw posts a raw submit body and returns the status and ingest error code.
func postRaw(addr string, body []byte) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/ingest/v1/torrents", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out.Error.Code, nil
}

// --- ingest process: host or hard-limited container ------------------------

type edgeIngest struct {
	p         *proc
	container string // "" for a host process
	cgroup    string
	peak, oom int // last successful cgroup readings (container runs only)
	started   time.Time
	pprof     string
}

func buildStaticIngestd(t *testing.T) string {
	t.Helper()
	binaries(t)
	out := filepath.Join(binDir, "ingestd-static")
	if _, err := os.Stat(out); err == nil {
		return out
	}
	root, _ := filepath.Abs("../..")
	cmd := exec.Command("go", "build", "-o", out, "./internal/e2e/ingestd")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build static ingestd: %v\n%s", err, b)
	}
	return out
}

func (w *world) startEdgeIngest(addr, goMemLimit, dockerMem string) *edgeIngest {
	w.t.Helper()
	t := w.t
	pprof := freePort(t)
	envKV := []string{
		"MAGNETICO_INGEST_TOKEN=" + token,
		"MAGNETICO_INGEST_DATABASE=" + w.dsn,
		"MAGNETICO_INGEST_LISTEN=" + addr,
		"INGESTD_PPROF=" + pprof,
		"INGESTD_SAMPLE_MS=1000",
	}
	if goMemLimit != "" {
		envKV = append(envKV, "GOMEMLIMIT="+goMemLimit)
	}
	e := &edgeIngest{pprof: pprof, started: time.Now()}
	if dockerMem == "" {
		e.p = start(t, envKV, buildIngestd(t))
	} else {
		bin := buildStaticIngestd(t)
		e.container = fmt.Sprintf("magnetico-edge-%d", time.Now().UnixNano())
		args := []string{"run", "--name", e.container, "--memory", dockerMem, "--memory-swap", dockerMem,
			"--network", "host", "-v", bin + ":/ingestd:ro"}
		for _, kv := range envKV {
			args = append(args, "-e", kv)
		}
		args = append(args, "postgres:17", "/ingestd")
		e.p = start(t, nil, "docker", args...)
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", e.container).Run() })
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-e.p.done:
			t.Fatalf("ingest exited early:\n%s", tail(e.p.output()))
		default:
		}
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/ingest/v1/torrents/exists", strings.NewReader(`{"info_hashes":[]}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				if e.container != "" {
					out, err := exec.Command("docker", "inspect", "-f", "{{.Id}} {{.State.Pid}}", e.container).Output()
					if err != nil {
						t.Fatalf("docker inspect: %v", err)
					}
					var id string
					var pid int
					if _, err := fmt.Sscan(string(out), &id, &pid); err != nil {
						t.Fatalf("docker inspect output %q: %v", out, err)
					}
					if e.cgroup, err = resolveCgroupDir(cgroupRoot, "/proc", pid, id); err != nil {
						t.Fatalf("a hard container memory limit was requested but the cgroup cannot be measured: %v", err)
					}
					if err := e.measure(); err != nil {
						t.Fatalf("a hard container memory limit was requested but %v", err)
					}
				}
				return e
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("ingest did not become ready:\n%s", tail(e.p.output()))
	return nil
}

func (e *edgeIngest) alive() bool {
	select {
	case <-e.p.done:
		return false
	default:
		return true
	}
}

// measure refreshes the container's cgroup peak and OOM counter. It is an
// error, never a zero, when they cannot be read; host processes have none.
func (e *edgeIngest) measure() error {
	if e.container == "" {
		return nil
	}
	peak, oom, err := readCgroup(e.cgroup)
	if err != nil {
		return fmt.Errorf("container memory measurement unavailable (%s): %w", e.cgroup, err)
	}
	e.peak, e.oom = peak, oom
	return nil
}

func (e *edgeIngest) containerState() string {
	if e.container == "" {
		return "host process"
	}
	out, err := exec.Command("docker", "inspect", "-f", "oom_killed={{.State.OOMKilled}} status={{.State.Status}} exit={{.State.ExitCode}}", e.container).Output()
	if err != nil {
		return "docker inspect failed: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

// stop terminates ingest gracefully and returns its exit code.
func (e *edgeIngest) stop(t *testing.T) int {
	t.Helper()
	if e.container != "" {
		_ = exec.Command("docker", "stop", "-t", "40", e.container).Run()
	} else {
		e.p.term()
	}
	return e.p.wait(t, 60*time.Second)
}

// --- per-stage memory statistics ---------------------------------------------

type stageMem struct {
	maxRSS, maxAnon, maxHWM, maxLive, maxInuse, maxGoal int // KiB
	gcs, pauseMs, cpuMs, gcCPUMs                        int
	samples                                             int
}

func (s stageMem) String() string {
	return fmt.Sprintf("rss_max=%dMiB anon_max=%dMiB hwm=%dMiB heap_live_max=%dMiB heap_inuse_max=%dMiB gc=%d pause=%dms cpu=%dms gc_cpu=%dms",
		s.maxRSS>>10, s.maxAnon>>10, s.maxHWM>>10, s.maxLive>>10, s.maxInuse>>10, s.gcs, s.pauseMs, s.cpuMs, s.gcCPUMs)
}

// memBetween aggregates the ingest MEM lines whose t lies in [from, to].
func memBetween(out string, from, to int) stageMem {
	var s stageMem
	var firstGC, firstPause, firstCPU, firstGCCPU = -1, -1, -1, -1
	var lastGC, lastPause, lastCPU, lastGCCPU int
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "MEM ") {
			continue
		}
		m := memRe.FindStringSubmatch(line)
		c := cpuRe.FindStringSubmatch(line)
		if m == nil || c == nil {
			continue
		}
		v := func(i int) int { n, _ := strconv.Atoi(m[i]); return n }
		if ts := v(1); ts < from || ts > to {
			continue
		}
		s.samples++
		s.maxRSS = max(s.maxRSS, v(2))
		s.maxAnon = max(s.maxAnon, v(3))
		s.maxHWM = max(s.maxHWM, v(5))
		s.maxInuse = max(s.maxInuse, v(7)>>10)
		s.maxLive = max(s.maxLive, v(13)>>10)
		s.maxGoal = max(s.maxGoal, v(14)>>10)
		cv := func(i int) int { n, _ := strconv.Atoi(c[i]); return n }
		cpu := cv(1) + cv(2)
		if firstGC < 0 {
			firstGC, firstPause, firstCPU, firstGCCPU = v(17), v(18), cpu, cv(3)
		}
		lastGC, lastPause, lastCPU, lastGCCPU = v(17), v(18), cpu, cv(3)
	}
	if firstGC >= 0 {
		s.gcs, s.pauseMs, s.cpuMs, s.gcCPUMs = lastGC-firstGC, lastPause-firstPause, lastCPU-firstCPU, lastGCCPU-firstGCCPU
	}
	return s
}

// --- the test ------------------------------------------------------------------

type edgeStage struct {
	name     string
	records  int
	jsonMiB  float64
	dur      time.Duration
	mem      stageMem
	cgroupHi int // container memory.peak (bytes) after the stage
	note     string
}

func TestE2ELargeRecordQualification(t *testing.T) {
	dir := os.Getenv("MAGNETICO_E2E_EDGE_DIR")
	if dir == "" {
		t.Skip("set MAGNETICO_E2E_EDGE_DIR to run the large-record qualification")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	goMemLimit := os.Getenv("MAGNETICO_E2E_EDGE_GOMEMLIMIT")
	dockerMem := os.Getenv("MAGNETICO_E2E_EDGE_DOCKER_MEM")
	stageTimeout := time.Duration(envIntDefault("MAGNETICO_E2E_EDGE_STAGE_TIMEOUT_S", edgeStageTimeoutDefault)) * time.Second
	minFree := envIntDefault("MAGNETICO_E2E_EDGE_MIN_FREE_GIB", 12)

	w := newWorld(t)
	addr := freePort(t)
	ing := w.startEdgeIngest(addr, goMemLimit, dockerMem)
	base := "http://" + addr
	t.Logf("ingest: GOMEMLIMIT=%q container limit=%q", goMemLimit, dockerMem)

	var stages []edgeStage
	var failed string
	writeSummary := func() {
		var b strings.Builder
		fmt.Fprintf(&b, "GOMEMLIMIT=%q container_limit=%q state=%s oom_kills=%d container_memory_peak=%dMiB\n",
			goMemLimit, dockerMem, ing.containerState(), ing.oom, ing.peak>>20)
		if failed != "" {
			fmt.Fprintf(&b, "FAILED: %s\n", failed)
		}
		for _, s := range stages {
			fmt.Fprintf(&b, "STAGE %-26s records=%-5d json=%7.1fMiB dur=%6.1fs  %s  container_peak=%dMiB %s\n",
				s.name, s.records, s.jsonMiB, s.dur.Seconds(), s.mem, s.cgroupHi>>20, s.note)
		}
		_ = os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(b.String()), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "ingest.log"), []byte(ing.p.output()), 0o644)
		t.Log("\n" + b.String())
	}
	t.Cleanup(writeSummary)

	// fail records a stage failure with ingest diagnostics preserved.
	fail := func(format string, a ...any) {
		t.Helper()
		failed = fmt.Sprintf(format, a...)
		fetchTo2 := func(path, name string) {
			if resp, err := (&http.Client{Timeout: 20 * time.Second}).Get("http://" + ing.pprof + path); err == nil {
				defer resp.Body.Close()
				if f, err := os.Create(filepath.Join(dir, name)); err == nil {
					_, _ = io.Copy(f, resp.Body)
					f.Close()
				}
			}
		}
		if ing.alive() {
			fetchTo2("/debug/pprof/heap", "fail-heap.pb.gz")
			fetchTo2("/debug/pprof/goroutine?debug=2", "fail-goroutines.txt")
		}
		_ = ing.measure() // best effort here; the failure is already being reported
		t.Fatalf("%s\ncontainer: %s oom_kills=%d\ningest tail:\n%s", failed, ing.containerState(), ing.oom, tail(ing.p.output()))
	}

	// runStage times a stage, checks the process is still alive and records
	// per-stage memory/CPU from the ingest MEM lines.
	runStage := func(name string, records int, jsonBytes int, note string, body func()) {
		t.Helper()
		if f := freeGiB(dir); f >= 0 && f < minFree {
			fail("stage %s: only %d GiB free under %s (limit %d GiB)", name, f, dir, minFree)
		}
		t0 := time.Since(ing.started)
		st := time.Now()
		body()
		if !ing.alive() {
			fail("stage %s: ingest exited during the stage (%s)", name, ing.containerState())
		}
		time.Sleep(2500 * time.Millisecond) // let one more MEM sample land
		if err := ing.measure(); err != nil {
			fail("stage %s: %v", name, err)
		}
		stages = append(stages, edgeStage{name: name, records: records, jsonMiB: float64(jsonBytes) / (1 << 20),
			dur: time.Since(st), mem: memBetween(ing.p.output(), int(t0.Seconds()), int((time.Since(ing.started)).Seconds())),
			cgroupHi: ing.peak, note: note})
		fetchTo(t, "http://"+ing.pprof+"/debug/pprof/heap", filepath.Join(dir, "heap-after-"+regexp.MustCompile(`[^A-Za-z0-9+-]+`).ReplaceAllString(name, "_")+".pb.gz"))
		t.Logf("stage %s done: %s", name, stages[len(stages)-1].mem)
	}

	// settleStage waits until the PostgreSQL queue is empty and the stored
	// total is base+delta, then checks the invariants every stage ends in.
	settleStage := func(name string, before, delta int) {
		t.Helper()
		deadline := time.Now().Add(stageTimeout)
		for time.Now().Before(deadline) {
			if !ing.alive() {
				fail("stage %s: ingest died while draining (%s)", name, ing.containerState())
			}
			if w.torrents() == before+delta && w.pgQueue() == 0 {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if got := w.torrents(); got != before+delta || w.pgQueue() != 0 {
			fail("stage %s: stored %d torrents (want %d), %d rows still queued", name, got, before+delta, w.pgQueue())
		}
		if q := w.pgQuar(); q != 0 {
			fail("stage %s: %d rows in the PostgreSQL quarantine, want 0", name, q)
		}
		if d := w.usageDrift(); d != 0 {
			fail("stage %s: usage accounting drifted by %d bytes", name, d)
		}
		if u := w.usedBytes(); u != 0 {
			fail("stage %s: used_bytes = %d with an empty queue and no quarantine", name, u)
		}
		if dup := w.n(`SELECT count(*) - count(DISTINCT info_hash) FROM magneticod.torrents`); dup != 0 {
			fail("stage %s: %d duplicate torrents", name, dup)
		}
	}

	runCrawler := func(name string, p *proc) done {
		t.Helper()
		select {
		case <-p.done:
		case <-time.After(stageTimeout):
			fail("stage %s: crawler did not finish within %v:\n%s", name, stageTimeout, tail(p.output()))
		}
		if code := p.cmd.ProcessState.ExitCode(); code != 0 {
			fail("stage %s: crawler exit %d:\n%s", name, code, tail(p.output()))
		}
		return parseDone(t, p.output())
	}

	stageSel := os.Getenv("MAGNETICO_E2E_EDGE_STAGES") // optional comma list; empty = all
	stageOn := func(name string) bool {
		return stageSel == "" || strings.Contains(","+stageSel+",", ","+name+",")
	}

	near := record.MaxRecordBytes - 1024 // ≈ 16 MiB serialized JSON
	const mib = 1 << 20

	// 1. Mixed workload: ordinary records at the expected rate with 1 MiB,
	// 8 MiB and near-limit records interleaved (bounded: 20 + 6 + 3 large).
	if stageOn("mixed") {
		before := w.torrents()
		var dn done
		runStage("mixed ordinary+1/8/16MiB", 300+29, 20*mib+6*8*mib+3*near, "crawler delivery at 40 rec/s", func() {
			cr := w.crawlerd(t.TempDir(), base, "-count", "300", "-rate", "40",
				"-big", fmt.Sprintf("%d:20,%d:6,%d:3", mib, 8*mib, near))
			dn = runCrawler("mixed", cr)
			settleStage("mixed", before, 329)
		})
		if dn.appended != 329 || dn.acked != 329 || dn.queue != 0 || dn.quarantine != 0 {
			fail("mixed: crawler done = %+v, want 329 appended and acked, nothing queued or quarantined", dn)
		}
		verifyBigTorrents(t, w, 29)
	}

	// 2. Lost acknowledgements with large records: the proxy commits upstream
	// and then drops the 202, so the crawler resends 8 MiB and 15 MiB bodies.
	if stageOn("lost-ack") {
		before := w.torrents()
		var mu sync.Mutex
		dropped := 0
		target, _ := url.Parse(base)
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
			drop := strings.HasSuffix(r.URL.Path, "/torrents") && resp.StatusCode == 202 && len(body) > 4*mib && dropped < 3
			if drop {
				dropped++
			}
			mu.Unlock()
			if drop {
				hj, _ := rw.(http.Hijacker)
				c, _, _ := hj.Hijack()
				c.Close()
				return
			}
			rw.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(rw, resp.Body)
		}))
		defer proxy.Close()
		runStage("lost-ack large", 20+4, 2*8*mib+2*15*mib, "first 3 large 202s dropped after commit", func() {
			cr := w.crawlerd(t.TempDir(), proxy.URL, "-count", "20", "-from", "7050000", "-big-from", "7100000",
				"-big", fmt.Sprintf("%d:2,%d:2", 8*mib, 15*mib))
			dn := runCrawler("lost-ack", cr)
			if dn.appended != 24 || dn.acked != 24 || dn.queue != 0 {
				fail("lost-ack: crawler done = %+v", dn)
			}
			settleStage("lost-ack", before, 24)
		})
		mu.Lock()
		n := dropped
		mu.Unlock()
		if n != 3 {
			fail("lost-ack: proxy dropped %d acknowledgements, want 3", n)
		}
	}

	// 3. Protocol and codec boundaries through raw HTTP.
	if stageOn("boundaries") {
		before := w.torrents()
		accepted := 0
		type tc struct {
			name       string
			body       []byte
			status     int
			code       string
			storedRecs int
		}
		small := func(id int) []byte { return jsonRecord(id, 0, 1) }
		cases := []tc{
			{"single record exactly MaxRecordBytes", envelope(jsonRecord(8_000_001, record.MaxRecordBytes, 1)), 202, "", 1},
			{"single record MaxRecordBytes+1", envelope(jsonRecord(8_000_002, record.MaxRecordBytes+1, 1)), 413, "record_too_large", 0},
			{"batch with one oversized record", envelope(small(8_000_003), jsonRecord(8_000_004, record.MaxRecordBytes+1, 1)), 413, "payload_too_large", 0},
			{"oversized record with unsupported version (precedence)", envelope(jsonRecord(8_000_005, record.MaxRecordBytes+1, 2)), 400, "unsupported_version", 0},
			{"two 8 MiB records in one request", envelope(jsonRecord(8_000_006, 8*mib, 1), jsonRecord(8_000_007, 8*mib, 1)), 202, "", 2},
			{"256 records (the maximum)", func() []byte {
				var rs [][]byte
				for i := 0; i < server.MaxSubmitRecords; i++ {
					rs = append(rs, small(8_100_000+i))
				}
				return envelope(rs...)
			}(), 202, "", 256},
			{"257 records", func() []byte {
				var rs [][]byte
				for i := 0; i <= server.MaxSubmitRecords; i++ {
					rs = append(rs, small(8_200_000+i))
				}
				return envelope(rs...)
			}(), 400, "invalid_request", 0},
			{"body exactly MaxSubmitBodyBytes", padBody(envelope(small(8_000_008)), server.MaxSubmitBodyBytes), 202, "", 1},
			{"body MaxSubmitBodyBytes+1", padBody(envelope(small(8_000_009)), server.MaxSubmitBodyBytes+1), 413, "payload_too_large", 0},
			{"unsupported version", envelope(jsonRecord(8_000_010, 0, 2)), 400, "unsupported_version", 0},
			{"invalid record (missing field)", envelope([]byte(`{"v":1,"info_hash":"` + fmt.Sprintf("%040x", 8_000_011) + `"}`)), 400, "invalid_record", 0},
			{"invalid UTF-8 in name", envelope(bytes.Replace(small(8_000_012), []byte("edge 8000012"), []byte("edge \xff\xfe"), 1)), 400, "invalid_record", 0},
			{"batch with one invalid record", envelope(small(8_000_013), []byte(`{"v":1}`)), 400, "invalid_batch", 0},
			{"250 000 files (MaxFiles)", envelope(manyFilesRecord(8_000_014, 250_000)), 202, "", 1},
			{"250 001 files", envelope(manyFilesRecord(8_000_015, 250_001)), 400, "invalid_record", 0},
		}
		var jsonBytes int
		runStage("boundaries", 0, 0, "raw HTTP protocol/codec limits", func() {
			for _, c := range cases {
				status, code, err := postRaw(addr, c.body)
				if err != nil {
					fail("boundary %q: %v", c.name, err)
				}
				if status != c.status || code != c.code {
					fail("boundary %q: got HTTP %d %q, want %d %q", c.name, status, code, c.status, c.code)
				}
				if status == 202 {
					accepted += c.storedRecs
					jsonBytes += len(c.body)
				}
				t.Logf("boundary ok: %-58s %d %s (%.1f MiB body)", c.name, status, code, float64(len(c.body))/mib)
			}
			settleStage("boundaries", before, accepted)
		})
		stages[len(stages)-1].records, stages[len(stages)-1].jsonMiB = accepted, float64(jsonBytes)/mib
	}

	// 4. Crawler-side oversize and server-side 413 quarantine.
	if stageOn("oversize-quarantine") {
		before := w.torrents()
		runStage("oversize quarantine", 50, 0, "2 enqueue-too-large + 2 server-413 quarantines", func() {
			cq := t.TempDir()
			cr := w.crawlerd(cq, base, "-count", "50", "-from", "9100000", "-poison-large", "2",
				"-big-from", "9200000", "-big", fmt.Sprintf("%d:2", record.MaxRecordBytes+4096))
			dn := runCrawler("oversize", cr)
			if dn.appended != 50 || dn.acked != 50 || dn.queue != 0 || dn.quarantine != 4 || dn.quarantined != 2 {
				fail("oversize: crawler done = %+v, want 50 appended/acked, 4 local quarantine rows (2 sender), empty queue", dn)
			}
			if !strings.Contains(cr.output(), "too_large=2") {
				fail("oversize: the crawler did not count 2 enqueue-too-large records:\n%s", tail(cr.output()))
			}
			settleStage("oversize", before, 50)
		})
	}

	// 5. Three crawlers deliver large records at the same time.
	if stageOn("concurrent-crawlers") {
		before := w.torrents()
		runStage("3 crawlers x large", 3*(30+4), 3*(2*near+2*8*mib), "3 simultaneous near-limit senders", func() {
			var crs []*proc
			for k := 0; k < 3; k++ {
				crs = append(crs, w.crawlerd(t.TempDir(), base, "-count", "30", "-from", strconv.Itoa(9300000+k*1000),
					"-big-from", strconv.Itoa(9400000+k*100), "-big", fmt.Sprintf("%d:2,%d:2", near, 8*mib)))
			}
			for k, cr := range crs {
				if dn := runCrawler("concurrent", cr); dn.appended != 34 || dn.acked != 34 || dn.queue != 0 {
					fail("concurrent: crawler %d done = %+v", k, dn)
				}
			}
			settleStage("concurrent", before, 3*34)
		})
	}

	// 6. Worst case the spec admits: all 8 submit slots busy with ~16 MiB bodies.
	if stageOn("eight-concurrent-submits") {
		before := w.torrents()
		runStage("8 concurrent 16MiB submits", 8, 8*near, "server submit-slot maximum (spec 5.5)", func() {
			var wg sync.WaitGroup
			errs := make(chan string, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					status, code, err := postRaw(addr, envelope(jsonRecord(8_300_000+i, near, 1)))
					if err != nil || status != 202 {
						errs <- fmt.Sprintf("submit %d: HTTP %d %q err=%v", i, status, code, err)
					}
				}(i)
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				fail("eight-concurrent: %s", e)
			}
			settleStage("eight-concurrent", before, 8)
		})
	}

	if err := ing.measure(); err != nil { // before stop: the cgroup disappears with the container
		fail("final measurement: %v", err)
	}
	if code := ing.stop(t); code != 0 {
		fail("ingest exit code %d on graceful stop (%s)", code, ing.containerState())
	}
	if ing.oom != 0 {
		fail("container recorded %d OOM kills", ing.oom)
	}
	if ing.container != "" && strings.Contains(ing.containerState(), "oom_killed=true") {
		fail("Docker reports the container as OOM-killed (%s)", ing.containerState())
	}
}

// padBody pads a JSON body with trailing spaces to exactly n bytes.
func padBody(b []byte, n int) []byte {
	if len(b) > n {
		panic("padBody: body already larger")
	}
	return append(b, bytes.Repeat([]byte(" "), n-len(b))...)
}

// verifyBigTorrents checks that every stored "e2e big" torrent round-trips: its
// stored blob decodes to exactly the file list the crawler generated.
func verifyBigTorrents(t *testing.T, w *world, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rows, err := w.pool.Query(ctx, `SELECT t.name, t.total_size, t.file_count, b.data
		FROM magneticod.torrents t JOIN magneticod.torrent_file_blobs b ON b.torrent_id = t.id
		WHERE t.name LIKE 'e2e big %'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	re := regexp.MustCompile(`^media/set-(\d{6})/season-(\d{2})/episode-(\d{6})\.mkv(x*)$`)
	n := 0
	for rows.Next() {
		var name string
		var total int64
		var count int
		var blob []byte
		if err := rows.Scan(&name, &total, &count, &blob); err != nil {
			t.Fatal(err)
		}
		files, err := codec.Decode(blob, codec.DefaultLimits())
		if err != nil {
			t.Fatalf("%s: blob does not decode: %v", name, err)
		}
		if len(files) != count {
			t.Fatalf("%s: blob has %d files, row says %d", name, len(files), count)
		}
		seen := make([]bool, count)
		var sum int64
		for _, f := range files {
			m := re.FindStringSubmatch(f.Path)
			if m == nil {
				t.Fatalf("%s: unexpected stored path %q", name, f.Path)
			}
			i, _ := strconv.Atoi(m[3])
			set, _ := strconv.Atoi(m[1])
			season, _ := strconv.Atoi(m[2])
			if i >= count || seen[i] || set != i/50 || season != i%50 || f.Size != 1000+int64(i) {
				t.Fatalf("%s: stored file %q size %d does not match the generated list", name, f.Path, f.Size)
			}
			seen[i] = true
			sum += f.Size
		}
		if sum != total {
			t.Fatalf("%s: file sizes sum to %d, row total_size is %d", name, sum, total)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("verified %d large torrents, want %d", n, want)
	}
	t.Logf("verified %d large torrents: stored blobs decode to the generated file lists", n)
}
