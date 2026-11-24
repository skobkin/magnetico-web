package opstats

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

var bg = context.Background()

func newDB(t *testing.T, withStats bool) (string, *pgxpool.Pool) {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(bg, dsn); err != nil {
		t.Fatal(err)
	}
	if withStats {
		if err := InitSchema(bg, dsn); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := pgxpool.New(bg, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return dsn, pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(bg, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestInitSchemaIsIdempotentAndNonDestructive(t *testing.T) {
	dsn, pool := newDB(t, true)
	if err := Verify(bg, pool); err != nil {
		t.Fatalf("Verify after init: %v", err)
	}
	exec(t, pool, `INSERT INTO magnetico_stats.agg_hour (bucket_start, records_received) VALUES ('2026-01-01T10:00:00Z', 42)`)
	exec(t, pool, `INSERT INTO magnetico_stats.crawler_activity (crawler_id, first_seen_at, last_seen_at) VALUES (gen_random_uuid(), now(), now())`)
	for i := 0; i < 2; i++ {
		if err := InitSchema(bg, dsn); err != nil {
			t.Fatalf("InitSchema again: %v", err)
		}
	}
	var n int64
	if err := pool.QueryRow(bg, `SELECT records_received FROM magnetico_stats.agg_hour`).Scan(&n); err != nil || n != 42 {
		t.Fatalf("a repeated init changed data: %d %v", n, err)
	}
	var shared int
	if err := pool.QueryRow(bg, `SELECT count(*) FROM magnetico_stats.shared_state`).Scan(&shared); err != nil || shared != 1 {
		t.Fatalf("shared_state rows = %d (%v), want the single row", shared, err)
	}
}

func TestInitSchemaRequiresVerifiedMagneticodV1(t *testing.T) {
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := InitSchema(bg, dsn); err == nil {
		t.Fatal("InitSchema on a database without the 0.8.0 schema must fail")
	}
	conn, err := pgx.Connect(bg, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(bg)
	var exists bool
	if err := conn.QueryRow(bg, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname='magnetico_stats')`).Scan(&exists); err != nil || exists {
		t.Fatalf("a refused init created the schema (%v)", err)
	}
}

func TestInitSchemaTakesAdvisoryLock(t *testing.T) {
	dsn, _ := newDB(t, false)
	// Hold the advisory lock: a concurrent init must wait, then find the schema created by the first.
	holder, err := pgx.Connect(bg, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(bg)
	tx, err := holder.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(bg, "SELECT pg_advisory_xact_lock($1)", initAdvisoryKey); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- InitSchema(bg, dsn) }()
	select {
	case err := <-done:
		t.Fatalf("InitSchema ran while the advisory lock was held (%v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Rollback(bg); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("InitSchema after release: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("InitSchema did not proceed after the lock was released")
	}
	// Concurrent runs create the schema exactly once.
	dsn2, _ := newDB(t, false)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := InitSchema(bg, dsn2); err != nil {
				t.Errorf("concurrent InitSchema: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestVerifyDetectsExtraColumnMissingIndexForeignTrigger(t *testing.T) {
	cases := []struct{ name, sql, detail string }{
		{"missing column", `ALTER TABLE magnetico_stats.agg_hour DROP COLUMN records_received`, "records_received"},
		{"extra column", `ALTER TABLE magnetico_stats.snapshot ADD COLUMN note text`, "note"},
		{"wrong type", `ALTER TABLE magnetico_stats.agg_day ALTER COLUMN records_received TYPE integer`, "records_received"},
		{"nullability", `ALTER TABLE magnetico_stats.agg_hour ALTER COLUMN covered_mask DROP NOT NULL`, "covered_mask"},
		{"primary key", `ALTER TABLE magnetico_stats.crawler_activity DROP CONSTRAINT crawler_activity_pkey`, "crawler_activity"},
		{"missing table", `DROP TABLE magnetico_stats.storage_month`, "storage_month"},
		{"extra table", `CREATE TABLE magnetico_stats.leftover (x int)`, "leftover"},
		{"foreign key", `CREATE TABLE magnetico_ingest_stub (id uuid PRIMARY KEY);
			ALTER TABLE magnetico_stats.crawler_activity ADD FOREIGN KEY (crawler_id) REFERENCES magnetico_ingest_stub (id)`, "foreign keys"},
		{"trigger", `CREATE FUNCTION magnetico_stats.noop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
			CREATE TRIGGER t BEFORE INSERT ON magnetico_stats.agg_hour FOR EACH ROW EXECUTE FUNCTION magnetico_stats.noop()`, "triggers"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, pool := newDB(t, true)
			exec(t, pool, c.sql)
			err := Verify(bg, pool)
			if !errors.Is(err, ErrSchemaInvalid) {
				t.Fatalf("Verify: %v, want ErrSchemaInvalid", err)
			}
			if !strings.Contains(err.Error(), c.detail) {
				t.Errorf("error %q does not name %q", err, c.detail)
			}
		})
	}
}

func TestVerifyRefusesWrongVersionRow(t *testing.T) {
	dsn, pool := newDB(t, true)
	exec(t, pool, `UPDATE magnetico_stats.stats_version SET version = 2`)
	if err := Verify(bg, pool); !errors.Is(err, ErrSchemaMissing) {
		t.Fatalf("Verify over version 2: %v", err)
	}
	if err := InitSchema(bg, dsn); !errors.Is(err, ErrSchemaMissing) {
		t.Fatalf("InitSchema over version 2: %v", err)
	}
	exec(t, pool, `UPDATE magnetico_stats.stats_version SET version = 1`)
	exec(t, pool, `INSERT INTO magnetico_stats.stats_version VALUES (2)`)
	if err := Verify(bg, pool); !errors.Is(err, ErrSchemaMissing) {
		t.Fatalf("Verify with an extra version row: %v", err)
	}
	_, bare := newDB(t, false)
	if err := Verify(bg, bare); !errors.Is(err, ErrSchemaMissing) {
		t.Fatalf("Verify without the schema: %v", err)
	}
}

func TestSchemaDoesNotChangeOtherSchemasVerification(t *testing.T) {
	dsn, _ := newDB(t, true)
	// magneticod and magnetico_ingest verify and initialise with magnetico_stats present, in either order.
	s, err := persistence.MakeDatabase(bg, dsn)
	if err != nil {
		t.Fatalf("persistence.MakeDatabase with magnetico_stats present: %v", err)
	}
	_ = s.Close()
	if err := persistence.InitSchema(bg, dsn); err != nil {
		t.Fatalf("persistence.InitSchema: %v", err)
	}
	if err := pgqueue.InitSchema(bg, dsn); err != nil {
		t.Fatalf("pgqueue.InitSchema: %v", err)
	}
	q, err := pgqueue.Open(bg, dsn, pgqueue.Options{})
	if err != nil {
		t.Fatalf("pgqueue.Open: %v", err)
	}
	q.Close()
	if err := InitSchema(bg, dsn); err != nil {
		t.Fatalf("stats InitSchema after ingest: %v", err)
	}
}

func TestChecksRejectNegativeCountersAndMinGreaterThanMax(t *testing.T) {
	_, pool := newDB(t, true)
	for name, sql := range map[string]string{
		"negative counter":    `INSERT INTO magnetico_stats.agg_hour (bucket_start, records_received) VALUES ('2026-01-01T10:00:00Z', -1)`,
		"min above max":       `INSERT INTO magnetico_stats.agg_hour (bucket_start, queue_rows_min, queue_rows_max) VALUES ('2026-01-01T10:00:00Z', 5, 4)`,
		"negative gauge sum":  `INSERT INTO magnetico_stats.agg_day (bucket_start, queue_bytes_sum) VALUES ('2026-01-01T00:00:00Z', -5)`,
		"mask beyond 60 bits": `INSERT INTO magnetico_stats.agg_hour (bucket_start, covered_mask) VALUES ('2026-01-01T10:00:00Z', 1::bigint << 60)`,
		"day over 1440 min":   `INSERT INTO magnetico_stats.agg_day (bucket_start, covered_minutes) VALUES ('2026-01-01T00:00:00Z', 1441)`,
		"negative storage":    `INSERT INTO magnetico_stats.storage_hour (bucket_start, torrents_table_bytes_sum) VALUES ('2026-01-01T10:00:00Z', -1)`,
		"negative activity":   `INSERT INTO magnetico_stats.crawler_activity (crawler_id, first_seen_at, last_seen_at, records_received) VALUES (gen_random_uuid(), now(), now(), -1)`,
		"second shared row":   `INSERT INTO magnetico_stats.shared_state (id) VALUES (2)`,
	} {
		if _, err := pool.Exec(bg, sql); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	exec(t, pool, `INSERT INTO magnetico_stats.agg_hour (bucket_start, queue_rows_min, queue_rows_max, queue_rows_n, covered_mask)
VALUES ('2026-01-01T10:00:00Z', 4, 5, 2, (1::bigint << 60) - 1)`)
}

func TestBucketStartMustBeAligned(t *testing.T) {
	_, pool := newDB(t, true)
	bad := map[string][]string{
		"agg_hour":      {"2026-01-01T10:30:00Z", "2026-01-01T10:00:01Z"},
		"agg_day":       {"2026-01-01T01:00:00Z", "2026-01-01T00:00:01Z"},
		"agg_month":     {"2026-01-02T00:00:00Z", "2026-01-01T01:00:00Z"},
		"storage_hour":  {"2026-01-01T10:15:00Z"},
		"storage_day":   {"2026-01-01T10:00:00Z"},
		"storage_month": {"2026-01-15T00:00:00Z"},
	}
	for table, starts := range bad {
		for _, s := range starts {
			if _, err := pool.Exec(bg, `INSERT INTO magnetico_stats.`+table+` (bucket_start) VALUES ($1)`, s); err == nil {
				t.Errorf("%s accepted the unaligned start %s", table, s)
			}
		}
	}
	// Alignment is judged in UTC whatever the session time zone is.
	for _, tz := range []string{"UTC", "Asia/Kolkata", "America/St_Johns", "Pacific/Kiritimati"} {
		exec(t, pool, `SET TIME ZONE '`+tz+`'`)
		for table, s := range map[string]string{"agg_hour": "2026-01-01T10:00:00Z", "agg_day": "2026-01-01T00:00:00Z", "agg_month": "2026-01-01T00:00:00Z"} {
			if _, err := pool.Exec(bg, `INSERT INTO magnetico_stats.`+table+` (bucket_start) VALUES ($1) ON CONFLICT DO NOTHING`, s); err != nil {
				t.Errorf("%s rejected an aligned UTC start under time zone %s: %v", table, tz, err)
			}
		}
	}
}

func TestNoColumnCanHoldPayloadHashTokenOrCode(t *testing.T) {
	_, pool := newDB(t, true)
	rows, err := pool.Query(bg, `SELECT table_name, column_name, data_type FROM information_schema.columns WHERE table_schema = 'magnetico_stats'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var table, col, typ string
		if err := rows.Scan(&table, &col, &typ); err != nil {
			t.Fatal(err)
		}
		n++
		// Payloads, hashes and tokens are bytea; names, codes, details and
		// request bodies are text or json: the schema has no such column at all.
		switch typ {
		case "bytea", "text", "character varying", "character", "json", "jsonb", "xml", "bit varying":
			t.Errorf("%s.%s has type %s: a column that could hold a payload, hash, token, name or code", table, col, typ)
		}
		for _, bad := range []string{"payload", "token", "info_hash", "infohash", "password", "secret", "invite", "name", "email", "detail", "reason", "body_text"} {
			if strings.Contains(col, bad) {
				t.Errorf("%s.%s: the name suggests forbidden content (%s)", table, col, bad)
			}
		}
	}
	if n < 300 {
		t.Fatalf("only %d columns scanned", n)
	}
	for _, c := range ColumnNames() {
		if strings.Contains(c, "crawler_name") {
			t.Errorf("column %s", c)
		}
	}
}

func TestNoForeignKeyToIngestSchema(t *testing.T) {
	_, pool := newDB(t, true)
	var n int
	if err := pool.QueryRow(bg, `SELECT count(*) FROM pg_constraint k
JOIN pg_class own ON own.oid = k.conrelid JOIN pg_namespace ons ON ons.oid = own.relnamespace
JOIN pg_class ref ON ref.oid = k.confrelid JOIN pg_namespace rns ON rns.oid = ref.relnamespace
WHERE k.contype = 'f' AND (ons.nspname = 'magnetico_stats' OR rns.nspname = 'magnetico_stats')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("foreign keys out of magnetico_stats: %d (%v)", n, err)
	}
	// The registry can be dropped and re-created without touching statistics.
	if err := pgqueue.InitSchema(bg, mustDSN(t, pool)); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO magnetico_stats.crawler_activity (crawler_id, first_seen_at, last_seen_at) VALUES (gen_random_uuid(), now(), now())`)
	exec(t, pool, `DROP SCHEMA magnetico_ingest CASCADE`)
	var rows int
	if err := pool.QueryRow(bg, `SELECT count(*) FROM magnetico_stats.crawler_activity`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("statistics did not survive dropping the ingest schema: %d %v", rows, err)
	}
}

func mustDSN(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return pool.Config().ConnString()
}

func roleDB(t *testing.T, dsn string, pool *pgxpool.Pool, grants ...string) *pgxpool.Pool {
	t.Helper()
	cl := pgtest.IntegrationCluster(t)
	role := "stats_" + pgtest.RandomSuffix()
	exec(t, pool, `CREATE ROLE `+pgtest.QuoteIdent(role)+` LOGIN`)
	for _, g := range grants {
		exec(t, pool, strings.ReplaceAll(g, "$ROLE", pgtest.QuoteIdent(role)))
	}
	t.Cleanup(func() {
		if c, err := pgx.Connect(bg, dsn); err == nil {
			_, _ = c.Exec(bg, `DROP OWNED BY `+pgtest.QuoteIdent(role))
			c.Close(bg)
		}
		if c, err := pgx.Connect(bg, cl.AdminDSN); err == nil {
			_, _ = c.Exec(bg, `DROP ROLE IF EXISTS `+pgtest.QuoteIdent(role))
			c.Close(bg)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = role
	rp, err := pgxpool.NewWithConfig(bg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rp.Close)
	return rp
}

func TestIngestRoleWithoutStatsGrantsGetsNamedMissingPrivileges(t *testing.T) {
	dsn, pool := newDB(t, true)
	rp := roleDB(t, dsn, pool, `GRANT USAGE ON SCHEMA magnetico_stats TO $ROLE`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA magnetico_stats TO $ROLE`)
	missing, err := MissingPrivileges(bg, rp, RoleIngest)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(missing, "\n")
	for _, want := range []string{"magnetico_stats.agg_hour: INSERT", "magnetico_stats.agg_hour: UPDATE", "magnetico_stats.shared_state: DELETE", "magnetico_stats.crawler_activity: INSERT"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing list lacks %q:\n%s", want, joined)
		}
	}
	if m, _ := MissingPrivileges(bg, rp, RoleWeb); len(m) != 0 {
		t.Fatalf("a read-only role is complete for the web: %v", m)
	}
	// Full grants: nothing is missing.
	full := roleDB(t, dsn, pool, `GRANT USAGE ON SCHEMA magnetico_stats TO $ROLE`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA magnetico_stats TO $ROLE`)
	if m, err := MissingPrivileges(bg, full, RoleIngest); err != nil || len(m) != 0 {
		t.Fatalf("complete grants reported missing: %v %v", m, err)
	}
	// No USAGE: the schema privilege is the one finding.
	none := roleDB(t, dsn, pool)
	if m, err := MissingPrivileges(bg, none, RoleWeb); err != nil || len(m) != 1 || m[0] != "magnetico_stats: USAGE" {
		t.Fatalf("no usage: %v %v", m, err)
	}
	// An absent schema is Verify's finding, not a privilege list.
	_, bare := newDB(t, false)
	if m, err := MissingPrivileges(bg, bare, RoleIngest); err != nil || len(m) != 0 {
		t.Fatalf("absent schema: %v %v", m, err)
	}
}

func TestCatalogueIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for c := Counter(0); c < NumCounters; c++ {
		n := c.Name()
		if n == "" || seen[n] {
			t.Fatalf("counter %d has name %q (empty or duplicate)", c, n)
		}
		seen[n] = true
	}
	for g := Gauge(0); g < NumGauges; g++ {
		if g.Name() == "" {
			t.Fatalf("gauge %d unnamed", g)
		}
	}
	for m := Measure(0); m < NumMeasures; m++ {
		if m.Name() == "" {
			t.Fatalf("measure %d unnamed", m)
		}
	}
	if NumRejectClasses != 9 {
		t.Fatalf("%d reject classes, want exactly 9", NumRejectClasses)
	}
	if RejectInternal.Counter() != CRejectedInternal || RejectAuth.Counter() != CRejectedAuth || RejectBusy.Counter() != CRejectedBusy {
		t.Fatal("RejectClass.Counter does not follow the counter order")
	}
	for _, tb := range tables {
		for _, c := range tb.cols {
			if !strings.EqualFold(c.typ, strings.ToLower(c.typ)) {
				t.Fatalf("%s.%s type %q is not canonical", tb.name, c.name, c.typ)
			}
		}
	}
}

func TestBucketMath(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	for _, tc := range []struct {
		in               time.Time
		hour, day, month string
		bit              int64
	}{
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), "2026-12-31T23:00:00Z", "2026-12-31T00:00:00Z", "2026-12-01T00:00:00Z", 1 << 59},
		{time.Date(2028, 2, 29, 12, 30, 0, 5, time.UTC), "2028-02-29T12:00:00Z", "2028-02-29T00:00:00Z", "2028-02-01T00:00:00Z", 1 << 30},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), "2027-01-01T00:00:00Z", "2027-01-01T00:00:00Z", "2027-01-01T00:00:00Z", 1},
		// 2026-03-01 01:15 IST is 2026-02-28 19:45 UTC: the buckets follow UTC.
		{time.Date(2026, 3, 1, 1, 15, 0, 0, ist), "2026-02-28T19:00:00Z", "2026-02-28T00:00:00Z", "2026-02-01T00:00:00Z", 1 << 45},
	} {
		f := func(got time.Time, want string) {
			t.Helper()
			if got.Location() != time.UTC || got.Format(time.RFC3339) != want {
				t.Errorf("%v: got %v, want %s", tc.in, got, want)
			}
		}
		f(HourOf(tc.in), tc.hour)
		f(DayOf(tc.in), tc.day)
		f(MonthOf(tc.in), tc.month)
		if MinuteBit(tc.in) != tc.bit {
			t.Errorf("%v: MinuteBit = %b, want %b", tc.in, MinuteBit(tc.in), tc.bit)
		}
	}
	if BitCount(FullHourMask) != 60 || BitCount(0) != 0 || BitCount(0b1011) != 3 {
		t.Fatal("BitCount")
	}
	if AddSat(Saturation-1, 5) != Saturation || AddSat(3, 4) != 7 || AddSat(Saturation, Saturation) != Saturation {
		t.Fatal("AddSat does not saturate at 2^62")
	}
}
