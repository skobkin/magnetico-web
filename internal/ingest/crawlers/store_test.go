package crawlers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

type env struct {
	dsn  string
	pool *pgxpool.Pool
	st   *Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
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
	return &env{dsn: dsn, pool: pool, st: New(pool)}
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestAddGeneratesUUIDv4AndMgcToken(t *testing.T) {
	e := newEnv(t)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		c, err := e.st.Add(context.Background(), fmt.Sprintf("crawler-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if !uuidV4.MatchString(c.ID) {
			t.Fatalf("id %q is not a canonical UUIDv4", c.ID)
		}
		if !strings.HasPrefix(c.Token, TokenPrefix) || TokenPrefix != "mgc_" {
			t.Fatalf("token %q lacks the mgc_ prefix", c.Token)
		}
		// 32 random bytes in unpadded base64url = 43 characters.
		if n := len(strings.TrimPrefix(c.Token, TokenPrefix)); n < 43 {
			t.Fatalf("token body has %d characters, want at least 43 (32 bytes)", n)
		}
		if seen[c.ID] || seen[c.Token] {
			t.Fatal("duplicate id or token")
		}
		seen[c.ID], seen[c.Token] = true, true
	}
}

func TestOnlyTheHashIsStored(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, err := e.st.Add(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	// The plaintext appears in no column of any table of the database.
	rows, err := e.pool.Query(ctx, `SELECT table_schema, table_name FROM information_schema.tables
WHERE table_schema IN ('magnetico_ingest','magneticod') AND table_type='BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables [][2]string
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, [2]string{a, b})
	}
	rows.Close()
	for _, tb := range tables {
		var n int
		q := fmt.Sprintf(`SELECT count(*) FROM %s.%s x WHERE x::text LIKE '%%' || $1 || '%%'`,
			pgx.Identifier{tb[0]}.Sanitize(), pgx.Identifier{tb[1]}.Sanitize())
		if err := e.pool.QueryRow(ctx, q, c.Token).Scan(&n); err != nil {
			t.Fatalf("%v: %v", tb, err)
		}
		if n != 0 {
			t.Fatalf("the plaintext token is stored in %v", tb)
		}
	}
	digest := Digest(c.Token)
	var got []byte
	if err := e.pool.QueryRow(ctx, `SELECT token_hash FROM magnetico_ingest.crawlers`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(digest[:]) || len(got) != 32 {
		t.Fatalf("stored hash %x, want SHA-256 of the token %x", got, digest)
	}
}

func TestNamesAreUniqueCaseInsensitively(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.st.Add(ctx, "Alpha"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Alpha", "alpha", "ALPHA"} {
		if _, err := e.st.Add(ctx, n); !errors.Is(err, ErrNameTaken) {
			t.Fatalf("Add(%q): %v, want ErrNameTaken", n, err)
		}
	}
	if _, err := e.st.Add(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidNamesAreRejected(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"", " ", "-lead", ".lead", "has space", "has/slash", "unié", strings.Repeat("a", 65), "a\nb", "<script>"} {
		if _, err := e.st.Add(context.Background(), n); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Add(%q): %v, want ErrInvalidName", n, err)
		}
	}
	for _, n := range []string{"a", "A1", "node-1.eu_west", strings.Repeat("a", 64)} {
		if _, err := e.st.Add(context.Background(), n); err != nil {
			t.Errorf("Add(%q): %v", n, err)
		}
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM magnetico_ingest.crawlers`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("rows = %d (%v), want only the valid ones", n, err)
	}
}

func TestTokenHashIsUnique(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.st.Add(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	_, err := e.pool.Exec(ctx, `INSERT INTO magnetico_ingest.crawlers (crawler_id, name, token_hash)
SELECT gen_random_uuid(), 'b', token_hash FROM magnetico_ingest.crawlers WHERE name='a'`)
	if err == nil {
		t.Fatal("two crawlers may not share a token hash")
	}
}

func TestRotateKeepsIDAndInvalidatesTheOldToken(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, err := e.st.Add(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.st.RotateToken(ctx, "ALPHA")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != c.ID || r.Name != "alpha" || r.Token == c.Token || !strings.HasPrefix(r.Token, TokenPrefix) {
		t.Fatalf("rotate = %+v after %+v", r, c)
	}
	if _, err := e.st.LookupDigest(ctx, Digest(c.Token)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token still resolves: %v", err)
	}
	rec, err := e.st.LookupDigest(ctx, Digest(r.Token))
	if err != nil || rec.ID != c.ID || !rec.Enabled {
		t.Fatalf("new token lookup = %+v, %v", rec, err)
	}
	var rotated bool
	if err := e.pool.QueryRow(ctx, `SELECT token_rotated_at IS NOT NULL FROM magnetico_ingest.crawlers`).Scan(&rotated); err != nil || !rotated {
		t.Fatalf("token_rotated_at not set (%v)", err)
	}
	if _, err := e.st.RotateToken(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate unknown: %v", err)
	}
	// Rotating a disabled crawler does not enable it.
	if err := e.st.SetEnabled(ctx, "alpha", false); err != nil {
		t.Fatal(err)
	}
	r2, err := e.st.RotateToken(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	rec, err = e.st.LookupDigest(ctx, Digest(r2.Token))
	if err != nil || rec.Enabled {
		t.Fatalf("rotation of a disabled crawler enabled it: %+v %v", rec, err)
	}
}

func TestDisableEnableKeepIDAndHash(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, err := e.st.Add(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetEnabled(ctx, "alpha", false); err != nil {
		t.Fatal(err)
	}
	rec, err := e.st.LookupDigest(ctx, Digest(c.Token))
	if err != nil || rec.Enabled || rec.ID != c.ID {
		t.Fatalf("after disable: %+v %v", rec, err)
	}
	var disabledAt bool
	_ = e.pool.QueryRow(ctx, `SELECT disabled_at IS NOT NULL FROM magnetico_ingest.crawlers`).Scan(&disabledAt)
	if !disabledAt {
		t.Fatal("disabled_at not set")
	}
	if err := e.st.SetEnabled(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	rec, err = e.st.LookupDigest(ctx, Digest(c.Token))
	if err != nil || !rec.Enabled || rec.ID != c.ID {
		t.Fatalf("after enable: %+v %v", rec, err)
	}
	_ = e.pool.QueryRow(ctx, `SELECT disabled_at IS NOT NULL FROM magnetico_ingest.crawlers`).Scan(&disabledAt)
	if disabledAt {
		t.Fatal("disabled_at survived enabling")
	}
	if err := e.st.SetEnabled(ctx, "nope", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disable unknown: %v", err)
	}
}

func TestListNeverReturnsTokenOrHash(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, err := e.st.Add(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Add(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	list, err := e.st.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "beta" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	typ := reflect.TypeOf(Crawler{})
	for i := 0; i < typ.NumField(); i++ {
		n := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(n, "token") && n != "tokenrotatedat" || strings.Contains(n, "hash") || strings.Contains(n, "digest") {
			t.Errorf("Crawler has field %s", typ.Field(i).Name)
		}
	}
	if s := fmt.Sprintf("%+v", list); strings.Contains(s, c.Token) {
		t.Fatalf("listing leaks the token: %s", s)
	}
}

func TestWebRoleCannotSelectTokenHash(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.st.Add(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	role := "web_" + pgtest.RandomSuffix()
	for _, s := range []string{
		`CREATE ROLE ` + pgtest.QuoteIdent(role) + ` LOGIN`,
		`GRANT USAGE ON SCHEMA magnetico_ingest TO ` + pgtest.QuoteIdent(role),
		`GRANT SELECT (crawler_id, name, enabled, created_at, token_rotated_at, disabled_at) ON magnetico_ingest.crawlers TO ` + pgtest.QuoteIdent(role),
	} {
		if _, err := e.pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, e.dsn)
		if err == nil {
			defer c.Close(ctx)
			_, _ = c.Exec(ctx, `DROP OWNED BY `+pgtest.QuoteIdent(role))
			_, _ = c.Exec(ctx, `DROP ROLE `+pgtest.QuoteIdent(role))
		}
	})
	cfg, _ := pgx.ParseConfig(e.dsn)
	cfg.User = role
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var name string
	if err := conn.QueryRow(ctx, `SELECT name FROM magnetico_ingest.crawlers`).Scan(&name); err != nil || name != "alpha" {
		t.Fatalf("safe columns: %q %v", name, err)
	}
	for _, q := range []string{`SELECT token_hash FROM magnetico_ingest.crawlers`, `SELECT * FROM magnetico_ingest.crawlers`} {
		if _, err := conn.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("%s: %v, want permission denied", q, err)
		}
	}
}

func TestConcurrentAddsOfOneNameYieldOneRow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, taken := 0, 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.st.Add(ctx, "same")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrNameTaken):
				taken++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || taken != 15 {
		t.Fatalf("ok=%d taken=%d, want 1 and 15", ok, taken)
	}
}

func TestLookupUnknownDigest(t *testing.T) {
	e := newEnv(t)
	if _, err := e.st.LookupDigest(context.Background(), Digest("mgc_nothing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}
