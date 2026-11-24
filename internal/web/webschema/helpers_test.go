package webschema

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

func tctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return c
}

// torrentDB is a disposable database holding the 0.8.0 torrent schema.
func torrentDB(t *testing.T) (string, *pgtest.Cluster) {
	t.Helper()
	cl := pgtest.IntegrationCluster(t)
	dsn := cl.TestDatabase(t)
	if err := persistence.InitSchema(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	return dsn, cl
}

// webDB is torrentDB plus a successfully initialized magnetico_web schema.
func webDB(t *testing.T) string {
	t.Helper()
	dsn, _ := torrentDB(t)
	if _, err := Init(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func execSQL(t *testing.T, dsn string, stmts ...string) {
	t.Helper()
	conn, err := pgx.Connect(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	for _, s := range stmts {
		if _, err := conn.Exec(tctx(t), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// snapshot is a full, deterministic picture of the magnetico_web schema
// (catalog and row contents), so that a refused call can be proven to have
// changed nothing.
func snapshot(t *testing.T, dsn string) string {
	t.Helper()
	conn, err := pgx.Connect(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var b strings.Builder
	catalog := []string{
		`SELECT 'rel', c.relname, c.relkind::text, c.relrowsecurity::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='magnetico_web'`,
		`SELECT 'col', c.relname, a.attname, format_type(a.atttypid,a.atttypmod)||a.attnotnull::text||a.attidentity::text FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='magnetico_web' AND a.attnum>0 AND NOT a.attisdropped`,
		`SELECT 'con', c.relname, k.conname, pg_get_constraintdef(k.oid)||k.convalidated::text FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='magnetico_web'`,
		`SELECT 'idx', t.relname, i.relname, pg_get_indexdef(i.oid) FROM pg_index x JOIN pg_class i ON i.oid=x.indexrelid JOIN pg_class t ON t.oid=x.indrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname='magnetico_web'`,
		`SELECT 'trg', c.relname, t.tgname, t.tgenabled::text FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='magnetico_web' AND NOT t.tgisinternal`,
		`SELECT 'fn', p.proname, '', '' FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='magnetico_web'`,
	}
	for _, q := range catalog {
		rows, err := conn.Query(tctx(t), q+` ORDER BY 1,2,3`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var a, c, d, e string
			if err := rows.Scan(&a, &c, &d, &e); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s|%s|%s|%s\n", a, c, d, e)
		}
		rows.Close()
	}
	var exists bool
	_ = conn.QueryRow(tctx(t), `SELECT to_regnamespace('magnetico_web') IS NOT NULL`).Scan(&exists)
	if exists {
		rows, err := conn.Query(tctx(t), `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='magnetico_web' AND c.relkind='r' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		var tables []string
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			tables = append(tables, n)
		}
		rows.Close()
		for _, n := range tables {
			var cnt int
			var sum string
			if err := conn.QueryRow(tctx(t), fmt.Sprintf(
				`SELECT count(*), COALESCE(md5(string_agg(x::text, ',' ORDER BY x::text)), '') FROM magnetico_web.%s x`, n)).Scan(&cnt, &sum); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "rows|%s|%d|%s\n", n, cnt, sum)
		}
	}
	return b.String()
}

// roleDSN derives a DSN for a role on the same database (the harness uses
// trust authentication on a disposable cluster).
func roleDSN(t *testing.T, dsn, role string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.User(role)
	return u.String()
}

func isUnsupported(err error) bool { return errors.Is(err, ErrWebSchemaUnsupported) }

func torrentDBCluster(t *testing.T) (*pgtest.Cluster, struct{}) {
	return pgtest.IntegrationCluster(t), struct{}{}
}

// DocsGrants returns the GRANT block of docs/web-deployment.md (the
// documented minimum privileges), so that tests run exactly what operators
// are told to run.
func docsGrants(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../../docs/web-deployment.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	a := strings.Index(s, "<!-- grants:begin -->")
	b := strings.Index(s, "<!-- grants:end -->")
	if a < 0 || b < a {
		t.Fatal("grants block missing from docs/web-deployment.md")
	}
	block := s[a:b]
	i := strings.Index(block, "```sql")
	j := strings.LastIndex(block, "```")
	if i < 0 || j <= i {
		t.Fatal("no sql fence in the grants block")
	}
	return block[i+len("```sql") : j]
}

// RoleSetup creates uniquely named runtime and operator roles on the shared
// disposable cluster (cleaned up after the test) and applies the documented
// grants to them. It returns the role names.
func roleSetup(t *testing.T, cl *pgtest.Cluster, dsn string) (rt, op string) {
	t.Helper()
	suffix := pgtest.RandomSuffix()
	rt, op = "wrt_"+suffix, "wop_"+suffix
	admin, err := pgx.Connect(tctx(t), cl.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	for _, r := range []string{rt, op} {
		if _, err := admin.Exec(tctx(t), `CREATE ROLE `+pgtest.QuoteIdent(r)+` LOGIN`); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), cl.AdminDSN)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), `DROP DATABASE IF EXISTS `+pgtest.QuoteIdent(dbName(dsn))+` WITH (FORCE)`)
		for _, r := range []string{op, rt} {
			_, _ = c.Exec(context.Background(), `DROP ROLE IF EXISTS `+pgtest.QuoteIdent(r))
		}
	})
	grants := strings.NewReplacer("magnetico_web_rt", pgtest.QuoteIdent(rt), "magnetico_web_op", pgtest.QuoteIdent(op)).Replace(docsGrants(t))
	execSQL(t, dsn, grants)
	return rt, op
}

func dbName(dsn string) string {
	u, _ := url.Parse(dsn)
	return strings.TrimPrefix(u.Path, "/")
}
