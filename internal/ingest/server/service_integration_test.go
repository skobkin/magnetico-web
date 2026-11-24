package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/ingest/crawlers"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

func prepareDB(t *testing.T, withIngest bool) string {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	if withIngest {
		if err := pgqueue.InitSchema(context.Background(), dsn); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

// registerCrawler adds a crawler to the database and returns its token.
func registerCrawler(t *testing.T, dsn, name string) string {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	c, err := crawlers.New(pool).Add(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return c.Token
}

type running struct {
	token  string
	base   string
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	err    error
}

// wait returns Run's result; safe to call repeatedly.
func (r *running) wait(d time.Duration) (error, bool) {
	timedOut := false
	r.once.Do(func() {
		select {
		case r.err = <-r.done:
		case <-time.After(d):
			timedOut = true
		}
	})
	return r.err, !timedOut
}

func startService(t *testing.T, dsn string, mutate func(*Options)) *running {
	t.Helper()
	return startServiceWithToken(t, dsn, registerCrawler(t, dsn, "integration-"+pgtest.RandomSuffix()), mutate)
}

func startServiceWithToken(t *testing.T, dsn, token string, mutate func(*Options)) *running {
	t.Helper()
	o := Options{
		Listen: "127.0.0.1:0", DatabaseURL: dsn,
		QueueMaxBytes: 1 << 30, CacheMaxBytes: 1 << 20, Logger: nil,
	}
	if mutate != nil {
		mutate(&o)
	}
	ready := make(chan net.Addr, 1)
	o.Ready = func(a net.Addr) { ready <- a }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o) }()
	select {
	case a := <-ready:
		r := &running{token: token, base: "http://" + a.String() + APIPrefix, cancel: cancel, done: done}
		t.Cleanup(func() { cancel(); _, _ = r.wait(60 * time.Second) }) // best-effort drain at cleanup
		return r
	case err := <-done:
		cancel()
		t.Fatalf("Run returned before ready: %v", err)
	case <-time.After(60 * time.Second):
		cancel()
		t.Fatal("service did not start")
	}
	return nil
}

func (r *running) post(path, body string) (int, []byte, error) {
	req, _ := http.NewRequest(http.MethodPost, r.base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func countRows(t *testing.T, dsn, query string) int {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestServiceEndToEnd(t *testing.T) {
	dsn := prepareDB(t, true)
	r := startService(t, dsn, nil)

	var recs []string
	for i := 1; i <= 20; i++ {
		recs = append(recs, recJSON(t, i))
	}
	for i := 0; i < 20; i += 5 {
		st, b, err := r.post("/torrents", records(recs[i:i+5]...))
		if err != nil || st != 202 || strings.TrimSpace(string(b)) != `{"accepted":5}` {
			t.Fatalf("submit: %v %d %s", err, st, b)
		}
	}
	// Redelivery is acknowledged and creates nothing new.
	if st, _, err := r.post("/torrents", records(recs[:5]...)); err != nil || st != 202 {
		t.Fatalf("redelivery: %v %d", err, st)
	}
	eventually(t, "consumer drain", func() bool {
		return countRows(t, dsn, `SELECT count(*) FROM magneticod.torrents`) == 20 &&
			countRows(t, dsn, `SELECT count(*) FROM magnetico_ingest.queue`) == 0
	})
	// Stored hashes are reported as existing; an unseen one as missing.
	st, b, err := r.post("/torrents/exists", fmt.Sprintf(`{"info_hashes":["%s","%s"]}`, hx(1), hx(999)))
	var out existsResponse
	if err != nil || st != 200 || json.Unmarshal(b, &out) != nil ||
		len(out.Exists) != 1 || out.Exists[0] != hx(1) || len(out.Missing) != 1 || out.Missing[0] != hx(999) {
		t.Fatalf("exists: %v %d %s", err, st, b)
	}
	if n := countRows(t, dsn, `SELECT used_bytes FROM magnetico_ingest.usage`); n != 0 {
		t.Fatalf("used_bytes = %d after drain", n)
	}
}

func TestServiceRefusesAMissingOrIncompatibleIngestSchema(t *testing.T) {
	dsn := prepareDB(t, false)
	err := Run(context.Background(), Options{Listen: "127.0.0.1:0", DatabaseURL: dsn})
	if !errors.Is(err, pgqueue.ErrSchemaMissing) {
		t.Fatalf("Run without ingest schema: %v, want ErrSchemaMissing", err)
	}
	// An older development schema without the crawler registry is refused, not repaired.
	dsn = prepareDB(t, true)
	pool, perr := pgxpool.New(context.Background(), dsn)
	if perr != nil {
		t.Fatal(perr)
	}
	if _, perr = pool.Exec(context.Background(), `DROP TABLE magnetico_ingest.crawlers`); perr != nil {
		t.Fatal(perr)
	}
	pool.Close()
	err = Run(context.Background(), Options{Listen: "127.0.0.1:0", DatabaseURL: dsn})
	if !errors.Is(err, pgqueue.ErrSchemaInvalid) {
		t.Fatalf("Run over a schema without crawlers: %v, want ErrSchemaInvalid", err)
	}
	if countRows(t, dsn, `SELECT count(*) FROM pg_class WHERE relname = 'crawlers'`) != 0 {
		t.Fatal("the refused start recreated the crawlers table")
	}
}

func TestNoCrawlerRegisteredWarningAtStart(t *testing.T) {
	dsn := prepareDB(t, true)
	var logs strings.Builder
	r := startServiceWithToken(t, dsn, "mgc_"+strings.Repeat("a", 43), func(o *Options) {
		o.Logger = log.New(&syncWriter{w: &logs}, "", 0)
	})
	if st, _, err := r.post("/torrents", records(recJSON(t, 1))); err != nil || st != http.StatusUnauthorized {
		t.Fatalf("with no crawler registered: %v %d, want 401", err, st)
	}
	if !strings.Contains(logs.String(), "no enabled crawler is registered") {
		t.Fatalf("no warning logged:\n%s", logs.String())
	}
	// A crawler added later works without a restart: its digest is not cached.
	r.token = registerCrawler(t, dsn, "late")
	if st, b, err := r.post("/torrents", records(recJSON(t, 1))); err != nil || st != http.StatusAccepted {
		t.Fatalf("after adding a crawler: %v %d %s", err, st, b)
	}
}

func TestPersistenceAcrossIngestRestart(t *testing.T) {
	dsn := prepareDB(t, true)
	token := registerCrawler(t, dsn, "durable")
	for i := 0; i < 2; i++ {
		r := startServiceWithToken(t, dsn, token, nil)
		if st, b, err := r.post("/torrents", records(recJSON(t, i+1))); err != nil || st != http.StatusAccepted {
			t.Fatalf("run %d: %v %d %s", i, err, st, b)
		}
		r.cancel()
		if err, done := r.wait(60 * time.Second); !done || err != nil {
			t.Fatalf("run %d did not stop cleanly: %v %v", i, err, done)
		}
	}
}

func TestServiceRefusesToStartWithoutSelectOnTheRegistry(t *testing.T) {
	c := pgtest.IntegrationCluster(t)
	dsn := prepareDB(t, true)
	role := "ing_" + pgtest.RandomSuffix()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, q := range []string{
		`CREATE ROLE ` + pgtest.QuoteIdent(role) + ` LOGIN`,
		`GRANT USAGE ON SCHEMA magnetico_ingest TO ` + pgtest.QuoteIdent(role),
		`GRANT USAGE ON SCHEMA magneticod TO ` + pgtest.QuoteIdent(role),
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA magnetico_ingest TO ` + pgtest.QuoteIdent(role),
		`REVOKE ALL ON magnetico_ingest.crawlers FROM ` + pgtest.QuoteIdent(role),
		`GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA magneticod TO ` + pgtest.QuoteIdent(role),
	} {
		if _, err := pool.Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if conn, err := pgx.Connect(ctx, dsn); err == nil {
			_, _ = conn.Exec(ctx, `DROP OWNED BY `+pgtest.QuoteIdent(role))
			conn.Close(ctx)
		}
		if conn, err := pgx.Connect(ctx, c.AdminDSN); err == nil {
			_, _ = conn.Exec(ctx, `DROP ROLE IF EXISTS `+pgtest.QuoteIdent(role))
			conn.Close(ctx)
		}
	})
	u, _ := url.Parse(dsn)
	u.User = url.User(role)
	err = Run(context.Background(), Options{Listen: "127.0.0.1:0", DatabaseURL: dsn, QueueDatabaseURL: u.String(), AllowNonDurable: true})
	if !errors.Is(err, ErrCrawlerRegistry) {
		t.Fatalf("Run without SELECT on crawlers: %v, want ErrCrawlerRegistry", err)
	}
}

// TestServiceStorageOutageAndRecovery makes the database refuse connections
// (and terminates the existing ones): admissions and lookups must answer 503
// storage_unavailable — never 202, never "missing" — and succeed again after
// the database is reopened.
func TestServiceStorageOutageAndRecovery(t *testing.T) {
	c := pgtest.IntegrationCluster(t)
	dsn := prepareDB(t, true)
	r := startService(t, dsn, nil)
	if st, b, err := r.post("/torrents", records(recJSON(t, 1))); err != nil || st != 202 {
		t.Fatalf("baseline: %v %d %s", err, st, b)
	}
	eventually(t, "baseline drain", func() bool { return countRows(t, dsn, `SELECT count(*) FROM magneticod.torrents`) == 1 })

	u, _ := url.Parse(dsn)
	dbName := strings.TrimPrefix(u.Path, "/")
	admin, err := pgx.Connect(context.Background(), c.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	setAllow := func(allow bool) {
		if _, err := admin.Exec(context.Background(),
			fmt.Sprintf(`ALTER DATABASE %s WITH ALLOW_CONNECTIONS %v`, pgtest.QuoteIdent(dbName), allow)); err != nil {
			t.Fatal(err)
		}
	}
	setAllow(false)
	// The deferred admin.Close runs before t.Cleanup callbacks, so use a
	// fresh connection to leave the database connectable for the drop.
	t.Cleanup(func() {
		c2, err := pgx.Connect(context.Background(), c.AdminDSN)
		if err != nil {
			return
		}
		defer c2.Close(context.Background())
		_, _ = c2.Exec(context.Background(),
			fmt.Sprintf(`ALTER DATABASE %s WITH ALLOW_CONNECTIONS true`, pgtest.QuoteIdent(dbName)))
	})
	if _, err := admin.Exec(context.Background(),
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName); err != nil {
		t.Fatal(err)
	}

	eventually(t, "503 storage_unavailable on submit", func() bool {
		st, b, err := r.post("/torrents", records(recJSON(t, 2)))
		if err == nil && st == 202 {
			t.Fatalf("202 while the database refuses connections: %s", b)
		}
		return err == nil && st == 503 && errCode(t, b) == "storage_unavailable"
	})
	eventually(t, "503 on exists", func() bool {
		st, b, err := r.post("/torrents/exists", fmt.Sprintf(`{"info_hashes":["%s"]}`, hx(77)))
		if err == nil && st == 200 && strings.Contains(string(b), hx(77)) && !strings.Contains(string(b), `"unknown":["`) {
			t.Fatalf("lookup answered during the outage: %s", b)
		}
		return err == nil && st == 503
	})

	setAllow(true)
	eventually(t, "recovery", func() bool {
		st, _, err := r.post("/torrents", records(recJSON(t, 2)))
		return err == nil && st == 202
	})
	eventually(t, "post-recovery drain", func() bool { return countRows(t, dsn, `SELECT count(*) FROM magneticod.torrents`) == 2 })
}

// TestServiceShutdownWithInFlightRequests cancels the service while many
// submissions are in flight. Run must return promptly and cleanly, and every
// record that received a 202 must be durably present (queue or torrents).
func TestServiceShutdownWithInFlightRequests(t *testing.T) {
	dsn := prepareDB(t, true)
	r := startService(t, dsn, nil)

	var mu sync.Mutex
	acked := map[int]bool{}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				n := 1000 + w*1000 + i
				st, _, err := r.post("/torrents", records(recJSON(t, n)))
				if err == nil && st == 202 {
					mu.Lock()
					acked[n] = true
					mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}(w)
	}
	eventually(t, "some acknowledgements", func() bool { mu.Lock(); defer mu.Unlock(); return len(acked) >= 30 })
	r.cancel()
	if err, ok := r.wait(40 * time.Second); !ok {
		t.Fatal("shutdown did not complete")
	} else if err != nil {
		t.Fatalf("Run returned %v on shutdown", err)
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	present := countRows(t, dsn, `SELECT count(*) FROM magneticod.torrents`) +
		countRows(t, dsn, `SELECT count(*) FROM magnetico_ingest.queue`)
	if present < len(acked) {
		t.Fatalf("%d records acknowledged but only %d present (lost acknowledged records)", len(acked), present)
	}
}
