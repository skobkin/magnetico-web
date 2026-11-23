package persistence

// Explicitly invoked S1 measurements (plan §3.3 and §3.1). They are skipped
// unless MAGNETICO_MEASURE=1; output goes to the test log and, when
// MAGNETICO_MEASURE_OUT is set, to that file. Synthetic, seeded data in a
// disposable database only.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"math/rand"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type report struct{ b strings.Builder }

func (r *report) f(format string, a ...any) { fmt.Fprintf(&r.b, format+"\n", a...) }

func measureEnv(t *testing.T) (*readerEnv, *report) {
	if os.Getenv("MAGNETICO_MEASURE") != "1" {
		t.Skip("set MAGNETICO_MEASURE=1 to run the S1 measurements")
	}
	e := newReaderEnv(t)
	r := &report{}
	t.Cleanup(func() {
		t.Log("\n" + r.b.String())
		if out := os.Getenv("MAGNETICO_MEASURE_OUT"); out != "" {
			f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err == nil {
				_, _ = f.WriteString(r.b.String() + "\n")
				_ = f.Close()
			}
		}
	})
	return e, r
}

func (e *readerEnv) setting(ctx context.Context, q string) string {
	var s string
	_ = e.pool.QueryRow(ctx, q).Scan(&s)
	return s
}

func syntheticWord(rank int) string {
	const cons, vow = "bcdfghklmnprstvz", "aeiou"
	n := rank + 1
	var b strings.Builder
	for i := 0; i < 3; i++ {
		b.WriteByte(cons[n%len(cons)])
		n /= len(cons)
		b.WriteByte(vow[n%len(vow)])
		n /= len(vow)
	}
	return b.String() + fmt.Sprintf("%d", rank%10)
}

func TestMeasureSearchPlans(t *testing.T) {
	e, rep := measureEnv(t)
	ctx := context.Background()
	const rows = 300_000
	rng := rand.New(rand.NewSource(20261123))
	zipf := rand.NewZipf(rng, 1.15, 8, 1999)

	data := make([][]any, 0, rows)
	for i := 0; i < rows; i++ {
		nw := 3 + rng.Intn(4)
		words := make([]string, nw)
		for j := range words {
			words[j] = syntheticWord(int(zipf.Uint64()))
		}
		h := sha256.Sum256([]byte(fmt.Sprintf("m/%d", i)))
		size := int64(math.Exp(rng.NormFloat64()*2+18)) + 1
		disc := int64(1_600_000_000+i*30) + int64(rng.Intn(6000)) - 3000
		data = append(data, []any{h[:20], strings.Join(words, " "), size, disc, int32(1 + rng.Intn(40))})
	}
	start := time.Now()
	if _, err := e.pool.CopyFrom(ctx, pgx.Identifier{"magneticod", "torrents"},
		[]string{"info_hash", "name", "total_size", "discovered_on", "file_count"}, pgx.CopyFromRows(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `ANALYZE magneticod.torrents`); err != nil {
		t.Fatal(err)
	}
	rep.f("## S1 search plans — %d synthetic rows (seed 20261123, Zipf vocabulary of 2000 words), load+index+analyze %v", rows, time.Since(start).Round(time.Millisecond))
	rep.f("version: %s", e.setting(ctx, `SELECT version()`))
	for _, s := range []string{"shared_buffers", "work_mem", "random_page_cost", "effective_cache_size", "max_parallel_workers_per_gather", "plan_cache_mode"} {
		rep.f("%s = %s", s, e.setting(ctx, `SHOW `+s))
	}
	rep.f("datcollate/datctype = %s", e.setting(ctx, `SELECT datcollate||' / '||datctype FROM pg_database WHERE datname = current_database()`))

	// Term frequencies actually present.
	countLike := func(term string) int {
		var n int
		_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM magneticod.torrents WHERE name ILIKE $1`, "%"+term+"%").Scan(&n)
		return n
	}
	common, medium, rare := syntheticWord(0), syntheticWord(100), syntheticWord(1500)
	rep.f("terms: common=%q (%d rows) medium=%q (%d) rare=%q (%d)", common, countLike(common), medium, countLike(medium), rare, countLike(rare))

	// Anchors for deep-page cases: the row at 50% of the newest-first order.
	var mid Torrent
	_ = e.pool.QueryRow(ctx, `SELECT `+torrentColumns+` FROM magneticod.torrents ORDER BY discovered_on DESC, id DESC OFFSET $1 LIMIT 1`, rows/2).
		Scan(&mid.ID, &mid.InfoHash, &mid.Name, &mid.TotalSize, &mid.DiscoveredOn, &mid.FileCount)

	type c struct {
		name string
		spec SearchSpec
	}
	base := func(terms []string, sort SortKey, desc bool) SearchSpec {
		return SearchSpec{Terms: terms, Sort: sort, Desc: desc, Limit: 50}
	}
	deep := base(nil, SortDiscovered, true)
	deep.After = &Anchor{ID: mid.ID, Int: mid.DiscoveredOn}
	back := deep
	back.Backward = true
	cases := []c{
		{"a newest first page", base(nil, SortDiscovered, true)},
		{"b deep cursor page (50% position)", deep},
		{"c backward page from the same cursor", back},
		{"d rare term, newest", base([]string{rare}, SortDiscovered, true)},
		{"e medium term, newest", base([]string{medium}, SortDiscovered, true)},
		{"f common term, newest", base([]string{common}, SortDiscovered, true)},
		{"g common + rare", base([]string{common, rare}, SortDiscovered, true)},
		{"h three common terms", base([]string{common, syntheticWord(1), syntheticWord(2)}, SortDiscovered, true)},
		{"i common term, size desc", base([]string{common}, SortSize, true)},
		{"j common term, name asc", base([]string{common}, SortName, false)},
		{"k rare term, name asc", base([]string{rare}, SortName, false)},
		{"l rare term, size desc", base([]string{rare}, SortSize, true)},
	}
	explain := func(sql string, args []any) string {
		rows, err := e.pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args...)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			b.WriteString("    " + line + "\n")
		}
		return b.String()
	}
	for _, cs := range cases {
		for _, expanded := range []bool{false, true} {
			if cs.spec.After == nil && expanded {
				continue
			}
			sql, args, err := buildSearchSQL(cs.spec, expanded)
			if err != nil {
				t.Fatal(err)
			}
			form := "row-comparison"
			if expanded {
				form = "OR-expanded"
			}
			if cs.spec.After == nil {
				form = "no cursor"
			}
			explain(sql, args) // warm
			rep.f("\n### %s [%s]\n  %s", cs.name, form, sql)
			rep.f("%s", explain(sql, args))
		}
	}

	// Info-hash lookup.
	var ih []byte
	_ = e.pool.QueryRow(ctx, `SELECT info_hash FROM magneticod.torrents WHERE id = $1`, mid.ID).Scan(&ih)
	rep.f("\n### info-hash lookup\n%s", explain(`SELECT `+torrentColumns+` FROM magneticod.torrents WHERE info_hash = $1`, []any{ih}))

	// Generic-plan risk: pgx caches prepared statements, PostgreSQL may switch a
	// prepared statement to a generic plan after five executions.
	conn, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, cs := range []c{cases[1], cases[5]} {
		sql, args, _ := buildSearchSQL(cs.spec, false)
		name := "gp_" + strings.Fields(cs.name)[0]
		if _, err := conn.Prepare(ctx, name, sql); err != nil {
			t.Fatal(err)
		}
		var times []time.Duration
		for i := 0; i < 9; i++ {
			s := time.Now()
			rs, err := conn.Query(ctx, name, args...)
			if err != nil {
				t.Fatal(err)
			}
			for rs.Next() {
			}
			rs.Close()
			times = append(times, time.Since(s))
		}
		rep.f("\n### generic-plan check: %s — prepared statement, 9 executions (ms): %v", cs.name, durMS(times))
		// Plan used by the 10th execution, via EXPLAIN EXECUTE on the same session.
		lits := make([]string, len(args))
		for i, a := range args {
			lits[i] = fmt.Sprintf("%v", a)
			if s, ok := a.(string); ok {
				lits[i] = "'" + strings.ReplaceAll(s, "'", "''") + "'"
			}
		}
		_, _ = conn.Exec(ctx, "DEALLOCATE "+name)
		_, _ = conn.Exec(ctx, "PREPARE sp AS "+sql)
		for i := 0; i < 7; i++ {
			rs, _ := conn.Query(ctx, "EXECUTE sp("+strings.Join(lits, ",")+")", pgx.QueryExecModeSimpleProtocol)
			for rs.Next() {
			}
			rs.Close()
		}
		rs, _ := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) EXECUTE sp("+strings.Join(lits, ",")+")", pgx.QueryExecModeSimpleProtocol)
		for rs.Next() {
			var l string
			_ = rs.Scan(&l)
			rep.f("    %s", l)
		}
		rs.Close()
		_, _ = conn.Exec(ctx, "DEALLOCATE sp")
	}
}

func durMS(ds []time.Duration) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = fmt.Sprintf("%.1f", float64(d.Microseconds())/1000)
	}
	return out
}

func percentile(ds []time.Duration, p float64) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[int(float64(len(s)-1)*p)]
}

func TestMeasureTOASTIdentityAndHeader(t *testing.T) {
	e, rep := measureEnv(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))

	type blobCase struct {
		name string
		data []byte
	}
	random := make([]byte, 24<<20)
	rng.Read(random)
	compressible := []byte(strings.Repeat("magnetico synthetic compressible payload line\n", (24<<20)/45))
	cases := []blobCase{{"incompressible 24 MiB (stored external)", random}, {"compressible 24 MiB (pglz)", compressible}, {"small 2 KiB (inline)", random[:2048]}}

	rep.f("## S1 TOAST measurement — identity and header queries against large bytea values (warm, repeated)")
	rep.f("version: %s; shared_buffers = %s", e.setting(ctx, `SELECT version()`), e.setting(ctx, `SHOW shared_buffers`))

	// Every measured operation runs on one dedicated connection whose pending
	// statistics are flushed explicitly (pg_stat_force_next_flush), so the I/O
	// counters cannot be attributed to the wrong operation by flush lag. (An
	// earlier version relied on sleeping and mis-attributed the pending
	// statistics of the 24 MiB INSERT to the first measured query.)
	cfg, _ := pgxpool.ParseConfig(e.dsn)
	cfg.MaxConns = 1
	solo, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer solo.Close()
	soloR := NewReader(solo, e.r.Limits())
	flush := func(p *pgxpool.Pool) {
		_, _ = p.Exec(ctx, `SELECT pg_stat_force_next_flush()`)
		_, _ = p.Exec(ctx, `SELECT 1`)
	}
	toastBlks := func() (hit, read int64) {
		flush(solo)
		flush(e.pool)
		time.Sleep(200 * time.Millisecond)
		c, _ := pgx.Connect(ctx, e.dsn)
		defer c.Close(ctx)
		_, _ = c.Exec(ctx, `SELECT pg_stat_clear_snapshot()`)
		_ = c.QueryRow(ctx, `SELECT coalesce(toast_blks_hit,0), coalesce(toast_blks_read,0) FROM pg_statio_user_tables WHERE schemaname='magneticod' AND relname='torrent_file_blobs'`).Scan(&hit, &read)
		return
	}
	for _, bc := range cases {
		id := e.seed(t, bc.name, int64(len(bc.data)), 1, 2)
		if _, err := e.pool.Exec(ctx, `INSERT INTO magneticod.torrent_file_blobs (torrent_id, data) VALUES ($1,$2)`, id, bc.data); err != nil {
			t.Fatal(err)
		}
		var toastBytes int64
		_ = e.pool.QueryRow(ctx, `SELECT pg_column_size(data) FROM magneticod.torrent_file_blobs WHERE torrent_id=$1`, id).Scan(&toastBytes)
		var strat string
		_ = e.pool.QueryRow(ctx, `SELECT pg_column_compression(data)::text FROM magneticod.torrent_file_blobs WHERE torrent_id=$1`, id).Scan(&strat)
		rep.f("\n### %s — stored size %d bytes, compression=%q", bc.name, toastBytes, strat)
		fi, err := e.r.FileIdentity(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		type op struct {
			name string
			run  func() error
		}
		ops := []op{
			{"FileIdentity (as implemented)", func() error { _, err := soloR.FileIdentity(ctx, id); return err }},
			{"identity WITH octet_length (rejected)", func() error {
				return soloR.readTx(ctx, func(tx pgx.Tx) error {
					var tt Torrent
					var has bool
					var n *int32
					var v *string
					return tx.QueryRow(ctx, `SELECT t.id, t.info_hash, t.name, t.total_size, t.discovered_on, t.file_count, b.torrent_id IS NOT NULL, octet_length(b.data), b.xmin::text
					    FROM magneticod.torrents t LEFT JOIN magneticod.torrent_file_blobs b ON b.torrent_id = t.id WHERE t.id = $1`, id).
						Scan(&tt.ID, &tt.InfoHash, &tt.Name, &tt.TotalSize, &tt.DiscoveredOn, &tt.FileCount, &has, &n, &v)
				})
			}},
			{"FileHeader (substring + octet_length)", func() error { _, _, err := soloR.FileHeader(ctx, id); return err }},
			{"FileBlob (full transfer)", func() error { _, err := soloR.FileBlob(ctx, id, fi.Version); return err }},
		}
		for _, o := range ops {
			n := 200
			if strings.HasPrefix(o.name, "FileBlob") {
				n = 15
			}
			_ = o.run() // warm
			h0, r0 := toastBlks()
			var ds []time.Duration
			for i := 0; i < n; i++ {
				s := time.Now()
				if err := o.run(); err != nil {
					t.Fatalf("%s: %v", o.name, err)
				}
				ds = append(ds, time.Since(s))
			}
			h1, r1 := toastBlks()
			rep.f("  %-36s n=%-3d p50=%8.3f ms  p95=%8.3f ms  max=%8.3f ms   toast_blks_hit +%d, toast_blks_read +%d (per call: %.1f hit)",
				o.name, n, ms(percentile(ds, .5)), ms(percentile(ds, .95)), ms(percentile(ds, 1)), h1-h0, r1-r0, float64(h1-h0)/float64(n))
		}
		for _, q := range []struct{ name, sql string }{
			{"identity with octet_length", `SELECT octet_length(b.data), b.xmin::text FROM magneticod.torrent_file_blobs b WHERE b.torrent_id = $1`},
			{"identity without octet_length", `SELECT b.xmin::text FROM magneticod.torrent_file_blobs b WHERE b.torrent_id = $1`},
			{"header substring", `SELECT substring(data from 1 for 12) FROM magneticod.torrent_file_blobs WHERE torrent_id = $1`},
		} {
			rs, _ := e.pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+q.sql, id)
			rep.f("  EXPLAIN %s:", q.name)
			for rs.Next() {
				var l string
				_ = rs.Scan(&l)
				rep.f("      %s", l)
			}
			rs.Close()
		}
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
