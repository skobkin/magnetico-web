package cli

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

var (
	crawlerdOnce sync.Once
	crawlerdPath string
	crawlerdErr  error
)

// buildCrawlerd builds the test-only crawler harness once.
func buildCrawlerd(t *testing.T) string {
	t.Helper()
	crawlerdOnce.Do(func() {
		dir, err := os.MkdirTemp("", "magnetico-stats-crawlerd-")
		if err != nil {
			crawlerdErr = err
			return
		}
		root, _ := filepath.Abs("../../..")
		crawlerdPath = filepath.Join(dir, "crawlerd")
		cmd := exec.Command("go", "build", "-o", crawlerdPath, "./internal/e2e/crawlerd")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			crawlerdErr = fmt.Errorf("build crawlerd: %v\n%s", err, out)
		}
	})
	if crawlerdErr != nil {
		t.Fatal(crawlerdErr)
	}
	return crawlerdPath
}

// statsWorld is a database with every schema, real binaries and no processes
// started yet: the scenario starts what it needs.
type statsWorld struct {
	t      *testing.T
	bin    string
	cd     string
	dsn    string
	addr   string
	ing    *harnessProc
	tokens map[string]string
	t0     time.Time
}

func newStatsWorld(t *testing.T) *statsWorld {
	t.Helper()
	s := &statsWorld{t: t, bin: buildBinary(t), cd: buildCrawlerd(t), dsn: harnessAppAccount(t), tokens: map[string]string{}, t0: time.Now()}
	for _, cmd := range []string{"schema-init", "web-schema-init", "ingest-schema-init", "stats-schema-init"} {
		if out, code := runWebSchemaInit(t, s.bin, s.dsn, cmd); code != 0 {
			t.Fatalf("migrate %s: exit %d: %s", cmd, code, out)
		}
	}
	return s
}

func (s *statsWorld) crawler(name string) string {
	s.t.Helper()
	s.tokens[name] = registerCrawler(s.t, s.bin, s.dsn, name)
	return s.tokens[name]
}

// startIngest runs the real ingest binary and waits until it listens.
func (s *statsWorld) startIngest(extraEnv ...string) {
	s.t.Helper()
	if s.addr == "" {
		s.addr = freeLoopbackAddr(s.t)
	}
	s.ing = harnessStart(s.t, s.bin, append([]string{"MAGNETICO_INGEST_DATABASE=" + s.dsn, "MAGNETICO_INGEST_LISTEN=" + s.addr}, extraEnv...), "ingest")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-s.ing.exited:
			s.t.Fatalf("ingest exited early: %v\n%s", err, s.ing.out.String())
		default:
		}
		if c, err := net.DialTimeout("tcp", s.addr, time.Second); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("ingest did not listen:\n%s", s.ing.out.String())
}

// stopIngest sends SIGTERM and requires a clean exit (the final statistics
// flush runs during the shutdown).
func (s *statsWorld) stopIngest() {
	s.t.Helper()
	if code := s.ing.terminate(s.t, 45*time.Second); code != 0 {
		s.t.Fatalf("ingest exit %d on SIGTERM:\n%s", code, s.ing.out.String())
	}
}

// killIngest is SIGKILL: no final flush.
func (s *statsWorld) killIngest() {
	s.t.Helper()
	_ = s.ing.cmd.Process.Kill()
	select {
	case err := <-s.ing.exited:
		_ = err // killed: a signal exit is the expected outcome
	case <-time.After(15 * time.Second):
		s.t.Fatal("killed ingest did not exit")
	}
}

var crawlerdDone = regexp.MustCompile(`DONE queue_records=(\d+) quarantine_records=(\d+) appended=(\d+) duplicate=(\d+) acked=(\d+)`)

type crawlerResult struct{ queue, appended, duplicate, acked int }

// runCrawlers runs one real crawler client per range (own queue directory, own
// credential) against the ingest and waits for all of them.
func (s *statsWorld) runCrawlers(ranges map[string][2]int, extraArgs ...string) map[string]crawlerResult {
	s.t.Helper()
	procs := map[string]*harnessProc{}
	for name, r := range ranges {
		args := append([]string{"-queue-dir", s.t.TempDir(), "-url", "http://" + s.addr, "-from", strconv.Itoa(r[0]), "-count", strconv.Itoa(r[1])}, extraArgs...)
		procs[name] = harnessStart(s.t, s.cd, []string{"MAGNETICO_CRAWL_INGEST_TOKEN=" + s.tokens[name]}, args...)
	}
	out := map[string]crawlerResult{}
	for name, p := range procs {
		select {
		case err := <-p.exited:
			if err != nil {
				s.t.Fatalf("crawler %s: %v\n%s", name, err, p.out.String())
			}
		case <-time.After(5 * time.Minute):
			s.t.Fatalf("crawler %s timed out:\n%s", name, p.out.String())
		}
		m := crawlerdDone.FindStringSubmatch(p.out.String())
		if m == nil {
			s.t.Fatalf("crawler %s: no DONE line:\n%s", name, p.out.String())
		}
		v := func(i int) int { n, _ := strconv.Atoi(m[i]); return n }
		out[name] = crawlerResult{queue: v(1), appended: v(3), duplicate: v(4), acked: v(5)}
	}
	return out
}

func (s *statsWorld) n(q string, args ...any) int {
	s.t.Helper()
	return sqlCount(s.t, s.dsn, q, args...)
}

// waitStored waits for want distinct torrents and an empty ingest queue.
func (s *statsWorld) waitStored(want int) {
	s.t.Helper()
	waitFor(s.t, fmt.Sprintf("%d stored torrents and an empty queue", want), func() bool {
		return s.n(`SELECT count(*) FROM magneticod.torrents`) >= want && s.n(`SELECT count(*) FROM magnetico_ingest.queue`) == 0
	})
}

// sum is the total of a counter over every hour bucket.
func (s *statsWorld) sum(col string) int {
	s.t.Helper()
	return s.n(`SELECT COALESCE(sum(` + col + `), 0)::bigint FROM magnetico_stats.agg_hour`)
}

type activityRow struct {
	received, enqueued, duplicate, bytes, existsReq, existsHashes, rejected int
	lastIP                                                                  string
	submitted, found                                                        bool
}

func (s *statsWorld) activity(name string) activityRow {
	s.t.Helper()
	c, err := pgx.Connect(context.Background(), s.dsn)
	if err != nil {
		s.t.Fatal(err)
	}
	defer c.Close(context.Background())
	var a activityRow
	err = c.QueryRow(context.Background(), `SELECT a.records_received, a.records_enqueued, a.records_duplicate, a.bytes_body, a.exists_requests, a.exists_hashes, a.rejected_requests,
       COALESCE(host(a.last_ip), ''), a.last_submit_at IS NOT NULL
FROM magnetico_stats.crawler_activity a JOIN magnetico_ingest.crawlers c USING (crawler_id) WHERE c.name = $1`, name).
		Scan(&a.received, &a.enqueued, &a.duplicate, &a.bytes, &a.existsReq, &a.existsHashes, &a.rejected, &a.lastIP, &a.submitted)
	if err == nil {
		a.found = true
	} else if err != pgx.ErrNoRows {
		s.t.Fatal(err)
	}
	return a
}

// ---- direct submitters ----------------------------------------------------------------

var submitClient = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}

func synthRecord(seq int) json.RawMessage {
	h := sha1.Sum([]byte("stats-e2e/" + strconv.Itoa(seq)))
	files := []persistence.File{{Path: "d/a.bin", Size: int64(seq%977 + 1)}, {Path: "d/b.bin", Size: int64(seq%131 + 1)}}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	b, err := record.Encode(record.Record{InfoHash: h, Name: fmt.Sprintf("stats e2e %08d", seq), TotalSize: total, DiscoveredOn: 1_800_000_000 + int64(seq), Files: files}, codec.DefaultLimits())
	if err != nil {
		panic(err)
	}
	return b
}

// submitter posts batches of distinct synthetic records and classifies the
// outcome per record; every counter is read after the workers stopped.
type submitter struct {
	addr, token string
	seq         atomic.Int64
	ok          atomic.Int64 // records in 2xx (accepted) responses
	unavailable atomic.Int64 // records in 503 responses
	other       atomic.Int64 // records in any other status
	transport   atomic.Int64 // records in requests that failed without a response
	requests    atomic.Int64
	unauth      atomic.Int64 // 401 responses
}

func (sb *submitter) batch(n int) {
	start := int(sb.seq.Add(int64(n))) - n
	raws := make([]json.RawMessage, n)
	for i := range raws {
		raws[i] = synthRecord(start + i + 1)
	}
	body, _ := json.Marshal(map[string]any{"records": raws})
	req, _ := http.NewRequest("POST", "http://"+sb.addr+"/api/ingest/v1/torrents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sb.token)
	req.Header.Set("Content-Type", "application/json")
	sb.requests.Add(1)
	resp, err := submitClient.Do(req)
	if err != nil {
		sb.transport.Add(int64(n))
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	switch resp.StatusCode {
	case 200, 202:
		sb.ok.Add(int64(n))
	case 503:
		sb.unavailable.Add(int64(n))
	case 401:
		sb.unauth.Add(1)
		sb.other.Add(int64(n))
	default:
		sb.other.Add(int64(n))
	}
}

// existsStatus posts an existence request with an empty hash list.
func existsStatus(addr, token string) int {
	req, _ := http.NewRequest("POST", "http://"+addr+"/api/ingest/v1/torrents/exists", strings.NewReader(`{"info_hashes":[]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := submitClient.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// setAllowConnections toggles new connections to the world's database and
// terminates the existing ones when off: a real PostgreSQL outage.
func (s *statsWorld) setAllowConnections(allow bool) {
	s.t.Helper()
	u, _ := url.Parse(s.dsn)
	db := strings.TrimPrefix(u.Path, "/")
	admin, err := pgx.Connect(context.Background(), pgtest.IntegrationCluster(s.t).AdminDSN)
	if err != nil {
		s.t.Fatal(err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(context.Background(), fmt.Sprintf(`ALTER DATABASE %s WITH ALLOW_CONNECTIONS %v`, pgtest.QuoteIdent(db), allow)); err != nil {
		s.t.Fatal(err)
	}
	if !allow {
		_, _ = admin.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, db)
	}
}

// appRole is the database role of the world's DSN.
func (s *statsWorld) appRole() string {
	u, _ := url.Parse(s.dsn)
	return u.User.Username()
}

// sabotage breaks the statistics (and only them) while PostgreSQL stays healthy.
type sabotage struct {
	name  string
	apply func(t *testing.T, s *statsWorld)
}

func sabotageCases() []sabotage {
	return []sabotage{
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
}
