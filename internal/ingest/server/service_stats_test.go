package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/ingest/stats"
	"github.com/skobkin/magnetico-web/internal/opstats"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

func prepareDBWithStats(t *testing.T) string {
	t.Helper()
	dsn := prepareDB(t, true)
	if err := opstats.InitSchema(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func fastStats(o *stats.WriterOptions) {
	o.FlushInterval, o.HeartbeatInterval, o.QueueInterval, o.StorageInterval = 150*time.Millisecond, 80*time.Millisecond, 80*time.Millisecond, 150*time.Millisecond
}

func submitN(t *testing.T, r *running, from, to int) {
	t.Helper()
	for i := from; i <= to; i += 5 {
		var recs []string
		for j := i; j < i+5 && j <= to; j++ {
			recs = append(recs, recJSON(t, j))
		}
		st, b, err := r.post("/torrents", records(recs...))
		if err != nil || st != http.StatusAccepted {
			t.Fatalf("submit %d: %v %d %s", i, err, st, b)
		}
	}
}

func TestStatsEndToEndThroughTheService(t *testing.T) {
	dsn := prepareDBWithStats(t)
	r := startService(t, dsn, func(o *Options) { o.statsWriter = fastStats })
	submitN(t, r, 1, 20)
	eventually(t, "the statistics to reach the database", func() bool {
		return countRows(t, dsn, `SELECT COALESCE(sum(settled_stored_new), 0) FROM magnetico_stats.agg_hour`) == 20 &&
			countRows(t, dsn, `SELECT count(*) FROM magnetico_stats.snapshot WHERE total_settled_stored_new = 20`) == 1 &&
			countRows(t, dsn, `SELECT count(*) FROM magnetico_stats.shared_state WHERE queue_sampled_at IS NOT NULL AND storage_sampled_at IS NOT NULL`) == 1
	})
	for col, want := range map[string]int{
		"submit_requests": 4, "records_received": 20, "records_enqueued": 20, "records_acknowledged": 20, "records_refused": 0,
		"torrents_created": 20, "settled_stored_new": 20, "auth_db_lookups": 1, "auth_cache_hits": 3,
	} {
		if got := countRows(t, dsn, `SELECT COALESCE(sum(`+col+`), 0) FROM magnetico_stats.agg_hour`); got != want {
			t.Errorf("%s = %d, want %d", col, got, want)
		}
	}
	if countRows(t, dsn, `SELECT bytes_body FROM magnetico_stats.agg_day`) <= 0 || countRows(t, dsn, `SELECT bytes_accepted FROM magnetico_stats.agg_day`) <= 0 {
		t.Fatal("byte counters are empty")
	}
	if got := countRows(t, dsn, `SELECT count(*) FROM magnetico_stats.crawler_activity WHERE records_enqueued = 20 AND host(last_ip) = '127.0.0.1' AND last_submit_at IS NOT NULL`); got != 1 {
		t.Fatalf("crawler activity rows = %d", got)
	}
	if countRows(t, dsn, `SELECT count(*) FROM magnetico_stats.agg_hour WHERE queue_rows_n > 0 AND covered_mask > 0`) < 1 {
		t.Fatal("no queue gauge samples or coverage persisted")
	}
	if countRows(t, dsn, `SELECT count(*) FROM magnetico_stats.storage_hour WHERE samples > 0`) < 1 {
		t.Fatal("no storage samples persisted")
	}
	// At quiescence the conservation identity holds exactly.
	if countRows(t, dsn, `SELECT COALESCE(sum(records_received) - sum(records_acknowledged) - sum(records_refused), 0) FROM magnetico_stats.agg_hour`) != 0 {
		t.Fatal("received != acknowledged + refused at quiescence")
	}
}

// Statistics failures must leave admission, authentication and the consumer alone.
func TestStatsFailureNeverBlocksAdmissionOrConsumer(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, dsn string) (opts func(*Options), cleanup func()){
		"the statistics schema does not exist": func(t *testing.T, dsn string) (func(*Options), func()) {
			pool := mustPool(t, dsn)
			defer pool.Close()
			if _, err := pool.Exec(context.Background(), `DROP SCHEMA magnetico_stats CASCADE`); err != nil {
				t.Fatal(err)
			}
			return func(o *Options) { o.statsWriter = fastStats }, func() {}
		},
		"a table is locked ACCESS EXCLUSIVE": func(t *testing.T, dsn string) (func(*Options), func()) {
			conn, err := pgx.Connect(context.Background(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := conn.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, tbl := range []string{"agg_hour", "ingest_instances", "snapshot", "shared_state"} {
				if _, err := tx.Exec(context.Background(), `LOCK TABLE magnetico_stats.`+tbl+` IN ACCESS EXCLUSIVE MODE`); err != nil {
					t.Fatal(err)
				}
			}
			return func(o *Options) { o.statsWriter = fastStats }, func() {
				_ = tx.Rollback(context.Background())
				conn.Close(context.Background())
			}
		},
		"the ingest role has no statistics grants": func(t *testing.T, dsn string) (func(*Options), func()) {
			c := pgtest.IntegrationCluster(t)
			role := "nostats_" + pgtest.RandomSuffix()
			pool := mustPool(t, dsn)
			defer pool.Close()
			for _, q := range []string{
				`CREATE ROLE ` + pgtest.QuoteIdent(role) + ` LOGIN`,
				`GRANT USAGE ON SCHEMA magnetico_ingest, magneticod TO ` + pgtest.QuoteIdent(role),
				`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA magnetico_ingest TO ` + pgtest.QuoteIdent(role),
				`GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA magneticod TO ` + pgtest.QuoteIdent(role),
				`GRANT USAGE ON ALL SEQUENCES IN SCHEMA magneticod TO ` + pgtest.QuoteIdent(role),
				`GRANT USAGE ON SCHEMA magnetico_stats TO ` + pgtest.QuoteIdent(role),
				`GRANT SELECT ON ALL TABLES IN SCHEMA magnetico_stats TO ` + pgtest.QuoteIdent(role),
			} {
				if _, err := pool.Exec(context.Background(), q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			u, _ := url.Parse(dsn)
			u.User = url.User(role)
			return func(o *Options) { o.QueueDatabaseURL = u.String(); o.statsWriter = fastStats }, func() {
				ctx := context.Background()
				if conn, err := pgx.Connect(ctx, dsn); err == nil {
					_, _ = conn.Exec(ctx, `DROP OWNED BY `+pgtest.QuoteIdent(role))
					conn.Close(ctx)
				}
				if conn, err := pgx.Connect(ctx, c.AdminDSN); err == nil {
					_, _ = conn.Exec(ctx, `DROP ROLE IF EXISTS `+pgtest.QuoteIdent(role))
					conn.Close(ctx)
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dsn := prepareDBWithStats(t)
			token := registerCrawler(t, dsn, "sabotage")
			opts, cleanup := setup(t, dsn)
			defer cleanup()
			var logs strings.Builder
			var mu sync.Mutex
			r := startServiceWithToken(t, dsn, token, func(o *Options) {
				opts(o)
				o.Logger = log.New(&lockedWriter{mu: &mu, b: &logs}, "", 0)
				o.AllowNonDurable = true
			})
			start := time.Now()
			submitN(t, r, 1, 40) // every request authenticates; none may be refused because of statistics
			if st, _, err := r.post("/torrents/exists", fmt.Sprintf(`{"info_hashes":["%s"]}`, hx(1))); err != nil || st != 200 {
				t.Fatalf("exists: %v %d", err, st)
			}
			time.Sleep(600 * time.Millisecond) // several failing flush attempts
			eventually(t, "the consumer to store everything", func() bool {
				return countRows(t, dsn, `SELECT count(*) FROM magneticod.torrents`) == 40
			})
			if d := time.Since(start); d > 30*time.Second {
				t.Fatalf("took %v: statistics failures slowed ingestion down", d)
			}
			mu.Lock()
			l := logs.String()
			mu.Unlock()
			if strings.Contains(l, "mgc_") {
				t.Fatal("a token reached the log")
			}
		})
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func mustPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSchemaMissingDisablesWriterWithOneWarning(t *testing.T) {
	dsn := prepareDB(t, true) // no magnetico_stats
	var logs strings.Builder
	var mu sync.Mutex
	r := startService(t, dsn, func(o *Options) {
		o.statsWriter = func(w *stats.WriterOptions) {
			w.FlushInterval, w.HeartbeatInterval = 40*time.Millisecond, 40*time.Millisecond
		}
		o.Logger = log.New(&lockedWriter{mu: &mu, b: &logs}, "", 0)
	})
	submitN(t, r, 1, 5)
	time.Sleep(1500 * time.Millisecond) // dozens of retry ticks
	mu.Lock()
	l := logs.String()
	mu.Unlock()
	if n := strings.Count(l, "statistics: persisting failed"); n != 1 {
		t.Fatalf("%d warnings about the missing schema, want exactly one:\n%s", n, l)
	}
	if !strings.Contains(l, "magnetico migrate stats-schema-init") {
		t.Fatalf("the warning does not name the remedy:\n%s", l)
	}
	if countRows(t, dsn, `SELECT count(*) FROM pg_namespace WHERE nspname = 'magnetico_stats'`) != 0 {
		t.Fatal("the service created the statistics schema on its own")
	}
	// Creating the schema later activates the writer without a restart.
	if err := opstats.InitSchema(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			mu.Lock()
			t.Logf("LOG:\n%s\nrows: enqueued=%d instances=%d", logs.String(),
				countRows(t, dsn, `SELECT COALESCE(sum(records_enqueued), 0) FROM magnetico_stats.agg_hour`),
				countRows(t, dsn, `SELECT count(*) FROM magnetico_stats.ingest_instances`))
			mu.Unlock()
		}
	}()
	eventually(t, "the writer to pick up the new schema", func() bool {
		return countRows(t, dsn, `SELECT COALESCE(sum(records_enqueued), 0) FROM magnetico_stats.agg_hour`) == 5
	})
}

func TestKillSwitchStartsNoWriterAndNoRecorder(t *testing.T) {
	dsn := prepareDBWithStats(t)
	r := startService(t, dsn, func(o *Options) { o.StatsDisabled = true; o.statsWriter = fastStats })
	submitN(t, r, 1, 10)
	eventually(t, "the consumer", func() bool { return countRows(t, dsn, `SELECT count(*) FROM magneticod.torrents`) == 10 })
	time.Sleep(500 * time.Millisecond)
	if n := countRows(t, dsn, `SELECT count(*) FROM pg_stat_activity WHERE application_name LIKE 'magnetico-stats-%' AND datname = current_database()`); n != 0 {
		t.Fatalf("%d statistics connections with the kill switch on", n)
	}
	for _, tbl := range []string{"ingest_instances", "snapshot", "agg_hour", "crawler_activity"} {
		if n := countRows(t, dsn, `SELECT count(*) FROM magnetico_stats.`+tbl); n != 0 {
			t.Errorf("%s has %d rows with the kill switch on", tbl, n)
		}
	}
}

func TestGracefulShutdownFlushesWithinBudget(t *testing.T) {
	dsn := prepareDBWithStats(t)
	r := startService(t, dsn, func(o *Options) {
		o.statsWriter = func(w *stats.WriterOptions) { w.FlushInterval = time.Hour }
	})
	submitN(t, r, 1, 15)
	eventually(t, "the consumer", func() bool { return countRows(t, dsn, `SELECT count(*) FROM magneticod.torrents`) == 15 })
	if n := countRows(t, dsn, `SELECT COALESCE(sum(records_enqueued), 0) FROM magnetico_stats.agg_hour`); n != 0 {
		t.Fatalf("%d events were persisted before the (hour-long) flush interval", n)
	}
	start := time.Now()
	r.cancel()
	if err, done := r.wait(60 * time.Second); !done || err != nil {
		t.Fatalf("Run: %v %v", err, done)
	}
	if d := time.Since(start); d > 12*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	if got := countRows(t, dsn, `SELECT COALESCE(sum(records_enqueued), 0) FROM magnetico_stats.agg_hour`); got != 15 {
		t.Fatalf("the final flush persisted %d of 15 events", got)
	}
	if got := countRows(t, dsn, `SELECT COALESCE(sum(settled_stored_new), 0) FROM magnetico_stats.agg_hour`); got != 15 {
		t.Fatalf("the final flush persisted %d of 15 settlements", got)
	}
}
