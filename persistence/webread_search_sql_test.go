package persistence

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/codec"
)

// sqlRecorder is a pgx query tracer that keeps every statement sent on the
// pool's connections.
type sqlRecorder struct {
	mu   sync.Mutex
	sqls []string
	args [][]any
}

func (r *sqlRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqls = append(r.sqls, d.SQL)
	r.args = append(r.args, d.Args)
	return ctx
}
func (r *sqlRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *sqlRecorder) reset() { r.mu.Lock(); r.sqls, r.args = nil, nil; r.mu.Unlock() }

// payload returns the statements of the call that are not transaction control
// (BEGIN, COMMIT, ROLLBACK, SET LOCAL statement_timeout): the statements that
// read data.
func (r *sqlRecorder) payload() (sqls []string, args [][]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.sqls {
		u := strings.ToUpper(strings.TrimSpace(s))
		if u == "BEGIN" || strings.HasPrefix(u, "BEGIN ") || u == "COMMIT" || u == "ROLLBACK" || strings.HasPrefix(u, "SET LOCAL STATEMENT_TIMEOUT") {
			continue
		}
		sqls = append(sqls, s)
		args = append(args, r.args[i])
	}
	return
}

// whereClause is the text between WHERE and ORDER BY of a SELECT.
func whereClause(sql string) string {
	i := strings.Index(sql, " WHERE ")
	if i < 0 {
		return ""
	}
	rest := sql[i+len(" WHERE "):]
	if j := strings.Index(rest, " ORDER BY "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// An ordinary name search is one data statement: no extra info-hash lookup, no
// info-hash predicate (the column appears only in the select list), no COUNT, no
// OFFSET, and LIMIT n+1 to learn whether another page exists. The statements of
// the surrounding read-only transaction are excluded by construction, and so are
// the authentication and session statements, which run on other code paths and
// never inside the persistence search operation.
func TestNameSearchSQLHasNoInfoHashPredicate(t *testing.T) {
	e := newReaderEnv(t)
	for i := 0; i < 30; i++ {
		e.seed(t, fmt.Sprintf("Ubuntu release %02d", i), int64(i+1), int64(1000+i), 1)
	}
	rec := &sqlRecorder{}
	cfg, err := pgxpool.ParseConfig(e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.Tracer = rec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r := NewReader(pool, codec.DefaultLimits())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Warm the connection, so no connect-time statement is counted.
	if _, err := r.SearchTorrents(ctx, SearchSpec{Terms: []string{"ubuntu"}, Sort: SortDiscovered, Desc: true, Limit: 5}); err != nil {
		t.Fatal(err)
	}

	type call struct {
		name string
		spec SearchSpec
		want int // data statements
	}
	cases := []call{
		{"one word", SearchSpec{Terms: []string{"ubuntu"}, Sort: SortDiscovered, Desc: true, Limit: 7}, 1},
		{"several words", SearchSpec{Terms: []string{"ubuntu", "release", "12"}, Sort: SortSize, Desc: false, Limit: 20}, 1},
		{"name sort", SearchSpec{Terms: []string{"ubuntu"}, Sort: SortName, Desc: false, Limit: 10}, 1},
		{"empty query", SearchSpec{Sort: SortDiscovered, Desc: true, Limit: 20}, 1},
		{"a word that looks like a hash prefix", SearchSpec{Terms: []string{"0123456789abcdef"}, Sort: SortDiscovered, Desc: true, Limit: 20}, 1},
		{"keyset page", SearchSpec{Terms: []string{"ubuntu"}, Sort: SortDiscovered, Desc: true, Limit: 7, After: &Anchor{ID: 20, Int: 1019}}, 1},
		{"keyset page, backward", SearchSpec{Terms: []string{"ubuntu"}, Sort: SortName, Limit: 7, Backward: true, After: &Anchor{ID: 20, Name: "Ubuntu release 19"}}, 1},
		// An anchor-only cursor first resolves the row's sort value with one
		// primary-key lookup: the documented second statement.
		{"anchor-only cursor", SearchSpec{Terms: []string{"ubuntu"}, Sort: SortSize, Limit: 7, After: &Anchor{ID: 20, ByID: true}}, 2},
	}
	for _, tc := range cases {
		rec.reset()
		if _, err := r.SearchTorrents(ctx, tc.spec); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		sqls, args := rec.payload()
		if len(sqls) != tc.want {
			t.Fatalf("%s: %d data statements, want %d:\n%s", tc.name, len(sqls), tc.want, strings.Join(sqls, "\n---\n"))
		}
		search := sqls[len(sqls)-1]
		if !strings.Contains(search, "FROM magneticod.torrents") {
			t.Errorf("%s: unexpected statement %s", tc.name, search)
		}
		// The info hash appears only in the select list.
		if w := whereClause(search); strings.Contains(strings.ToLower(w), "info_hash") {
			t.Errorf("%s: an info-hash predicate in the WHERE clause: %s", tc.name, w)
		}
		if strings.Contains(strings.ToLower(search), "where") && strings.Contains(strings.ToLower(whereClause(search)), "decode(") {
			t.Errorf("%s: a hash decoding in the WHERE clause", tc.name)
		}
		for _, bad := range []string{"count(", "offset", "union", "or info_hash", "info_hash ="} {
			if strings.Contains(strings.ToLower(search), bad) {
				t.Errorf("%s: the statement contains %q:\n%s", tc.name, bad, search)
			}
		}
		// LIMIT n+1: the last argument is the page size plus one.
		if last := args[len(args)-1]; len(last) == 0 || fmt.Sprint(last[len(last)-1]) != fmt.Sprint(tc.spec.Limit+1) {
			t.Errorf("%s: LIMIT argument %v, want %d", tc.name, last, tc.spec.Limit+1)
		}
		// Every term is one literal, case-insensitive substring predicate.
		if n := strings.Count(whereClause(search), "name ILIKE"); n != len(tc.spec.Terms) {
			t.Errorf("%s: %d name predicates for %d terms:\n%s", tc.name, n, len(tc.spec.Terms), whereClause(search))
		}
	}
}
