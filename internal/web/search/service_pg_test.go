package search

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

type env struct {
	pool *pgxpool.Pool
	r    *persistence.Reader
	dsn  string
	n    int
}

func newEnv(t *testing.T, maxConns int32) *env {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := persistence.InitSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &env{pool: pool, r: persistence.NewReader(pool, codec.DefaultLimits()), dsn: dsn}
}

func (e *env) seed(t *testing.T, name string, total, disc int64) int32 {
	t.Helper()
	e.n++
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", name, e.n)))
	var id int32
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
	    VALUES ($1,$2,$3,$4,1) RETURNING id`, h[:20], name, total, disc).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) service(t *testing.T, conc int, wait time.Duration) *Service {
	t.Helper()
	s, err := New(e.r, Config{Concurrency: conc, QueueWait: wait})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ids(ts []persistence.Torrent) []int32 {
	out := make([]int32, len(ts))
	for i, x := range ts {
		out[i] = x.ID
	}
	return out
}

func (e *env) reference(t *testing.T, p Params) []int32 {
	t.Helper()
	col := map[Sort]string{SortDiscovered: "discovered_on", SortSize: "total_size", SortName: "name"}[p.Sort]
	dir := "ASC"
	if p.Desc {
		dir = "DESC"
	}
	rows, err := e.pool.Query(context.Background(), `SELECT id FROM magneticod.torrents WHERE name ILIKE '%item%' ORDER BY `+col+` `+dir+`, id `+dir)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int32
	for rows.Next() {
		var id int32
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

// Walks every page through the service's own cursors: forward to the end,
// backward to the head, and the return trip page 1 → 2 → 1 → 2. Includes a
// name longer than the cursor limit, which lands on a page boundary so the
// anchor-only cursors are exercised in both directions.
func TestServiceWalksAndReturnTripThroughCursors(t *testing.T) {
	e := newEnv(t, 4)
	var longIDs []int32
	for i := 0; i < 38; i++ {
		name := fmt.Sprintf("Item %c%d", 'a'+rune(i%4), i%3)
		if i == 9 || i == 25 {
			name = fmt.Sprintf("Item long %d %s", i, strings.Repeat("x", maxCursorName+500))
		}
		id := e.seed(t, name, int64(i%5+1), int64(100+i%6))
		if i == 9 || i == 25 {
			longIDs = append(longIDs, id)
		}
	}
	s := e.service(t, 2, time.Second)
	ctx := context.Background()

	for _, sort := range []string{"discovered", "size", "name"} {
		for _, order := range []string{"asc", "desc"} {
			t.Run(sort+"/"+order, func(t *testing.T) {
				p, _ := ParseParams("item", sort, order, "", "")
				ref := e.reference(t, p)
				// Size the page so the first oversized name is the LAST row of page 1
				// (a boundary row whose cursor must be anchor-only), and the rest
				// continue from there.
				p.PageSize = 6
				if sort == "name" {
					for i, id := range ref {
						if slices.Contains(longIDs, id) {
							p.PageSize = i + 1
							break
						}
					}
				}

				var walked []int32
				var firstPage, secondPage Result
				cur := ""
				anchorKinds := 0
				for n := 0; ; n++ {
					pp := p
					pp.Cursor = cur
					res, err := s.Search(ctx, pp)
					if err != nil {
						t.Fatalf("page %d: %v", n, err)
					}
					if n == 0 {
						firstPage = res
						if res.Prev != "" {
							t.Fatal("first page has a Previous link")
						}
					} else if res.Prev == "" {
						t.Fatalf("page %d has no Previous link", n)
					}
					if n == 1 {
						secondPage = res
					}
					walked = append(walked, ids(res.Items)...)
					if res.Next != "" {
						if c, _ := DecodeCursor(p, res.Next); c.ByID {
							anchorKinds++
						}
					}
					if res.Next == "" {
						break
					}
					if n > 100 {
						t.Fatal("no end")
					}
					cur = res.Next
				}
				if !slices.Equal(walked, ref) {
					t.Fatalf("forward walk differs from the reference")
				}
				if sort == "name" && anchorKinds == 0 {
					t.Fatal("no page boundary fell on an oversized name: anchor cursors were not exercised")
				}

				// Return trip: page 2 → Previous == page 1 (no Previous) → Next == page 2.
				pb := p
				pb.Cursor = secondPage.Prev
				back, err := s.Search(ctx, pb)
				if err != nil || !slices.Equal(ids(back.Items), ids(firstPage.Items)) || back.Prev != "" || back.Next == "" {
					t.Fatalf("previous from page 2: %v prev=%q next=%q", err, back.Prev, back.Next)
				}
				pn := p
				pn.Cursor = back.Next
				again, err := s.Search(ctx, pn)
				if err != nil || !slices.Equal(ids(again.Items), ids(secondPage.Items)) {
					t.Fatalf("next after the return trip differs from page 2: %v", err)
				}

				// Backward to the head from the last page, through Previous links only.
				var rev []int32
				cur = ""
				var last Result
				for pp := p; ; {
					pp.Cursor = cur
					res, err := s.Search(ctx, pp)
					if err != nil {
						t.Fatal(err)
					}
					last, cur = res, res.Next
					if cur == "" {
						break
					}
				}
				for res := last; ; {
					rev = append(slices.Clone(ids(res.Items)), rev...)
					if res.Prev == "" {
						break
					}
					pp := p
					pp.Cursor = res.Prev
					var err error
					if res, err = s.Search(ctx, pp); err != nil {
						t.Fatal(err)
					}
				}
				if !slices.Equal(rev, ref) {
					t.Fatalf("backward walk differs from the reference")
				}
			})
		}
	}
}

func TestServiceOversizedAnchorCursorsBothDirectionsAndDeletedAnchor(t *testing.T) {
	e := newEnv(t, 4)
	var longID int32
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("Item %02d", i)
		if i == 5 {
			name = "Item 05 " + strings.Repeat("y", maxCursorName+10)
		}
		id := e.seed(t, name, 1, 1)
		if i == 5 {
			longID = id
		}
	}
	s := e.service(t, 2, time.Second)
	ctx := context.Background()
	p, _ := ParseParams("item", "name", "asc", "", "")
	p.PageSize = 6 // rows 0..5 on page 1: the long name is the boundary row

	res, err := s.Search(ctx, p)
	if err != nil || res.Items[len(res.Items)-1].ID != longID {
		t.Fatalf("setup: %v", err)
	}
	fwd, _ := DecodeCursor(p, res.Next)
	if !fwd.ByID || fwd.Backward {
		t.Fatalf("next cursor for an oversized name must be a forward anchor cursor: %+v", fwd)
	}
	pn := p
	pn.Cursor = res.Next
	page2, err := s.Search(ctx, pn)
	if err != nil || len(page2.Items) != 6 {
		t.Fatalf("page 2: %v", err)
	}
	// Backward from a cursor whose anchor is the oversized row: the first row of
	// page 2 is row 6, so make the oversized row the FIRST row of a page.
	p.PageSize = 6
	pp := p
	pp.Cursor = EncodeCursor(p, Cursor{ID: longID, Name: strings.Repeat("y", maxCursorName+10)}) // forward anchor
	after, err := s.Search(ctx, pp)
	if err != nil || ids(after.Items)[0] == longID {
		t.Fatalf("forward anchor cursor: %v", err)
	}
	pb := p
	pb.Cursor = EncodeCursor(p, Cursor{ID: longID, Name: strings.Repeat("y", maxCursorName+10), Backward: true})
	before, err := s.Search(ctx, pb)
	if err != nil || len(before.Items) != 5 || slices.Contains(ids(before.Items), longID) {
		t.Fatalf("backward anchor cursor: %v items=%v", err, ids(before.Items))
	}

	// Deleted anchor, both directions: expired, never an empty page.
	if _, err := e.pool.Exec(ctx, `DELETE FROM magneticod.torrents WHERE id = $1`, longID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{pp.Cursor, pb.Cursor} {
		q := p
		q.Cursor = c
		if _, err := s.Search(ctx, q); !errors.Is(err, ErrCursorExpired) || Outcome(err) != "invalid_cursor" {
			t.Errorf("deleted anchor: %v", err)
		}
	}
}

func TestServiceSearchSemanticsThroughTheParser(t *testing.T) {
	e := newEnv(t, 4)
	for i, n := range []string{"Ubuntu 24.04 desktop", "ubuntu_server", "Ubuntuxserver", "100% legit", "100 percent", `back\slash`, "日本語 アニメ", "日本 short"} {
		e.seed(t, n, int64(i+1), int64(i+1))
	}
	s := e.service(t, 2, time.Second)
	find := func(q string) []string {
		p, err := ParseParams(q, "", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Search(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range res.Items {
			out = append(out, it.Name)
		}
		slices.Sort(out)
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("got %q want %q", got, want)
		}
	}
	eq(find("UBUNTU desktop"), "Ubuntu 24.04 desktop")
	eq(find("tu_s"), "ubuntu_server")
	eq(find("100%"), "100% legit")
	eq(find(`k\s`), `back\slash`)
	eq(find("日本語"), "日本語 アニメ")
	eq(find("short"), "日本 short")
	// "日本" has only two code points: ignored, so the query selects the newest
	// torrents (all rows), not "no match".
	if got := find("日本"); len(got) != 8 {
		t.Errorf("an all-ignored query must list the newest torrents, got %d rows", len(got))
	}
}

func TestServiceTimeoutAgainstALockedTable(t *testing.T) {
	e := newEnv(t, 4)
	e.seed(t, "item x", 1, 1)
	release := lock(t, e.dsn)
	defer release()
	s := e.service(t, 2, time.Second)
	p, _ := ParseParams("item", "", "", "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Search(ctx, p)
	if !errors.Is(err, ErrTimeout) || errors.Is(err, ErrInvalidQuery) || errors.Is(err, ErrBusy) || Outcome(err) != "timeout" {
		t.Fatalf("got %v, want ErrTimeout only", err)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("took %v", time.Since(start))
	}
	if s.gate.InUse() != 0 {
		t.Fatal("permit leaked")
	}
	release()
	if _, err := s.Search(context.Background(), p); err != nil {
		t.Fatalf("after the lock was released: %v", err)
	}
}

// Slow searches cannot starve other work: with a 4-connection pool and two
// search slots, two blocked searches and a refused third leave connections for
// everything else.
func TestSlowSearchesDoNotStarveOtherPoolUsers(t *testing.T) {
	e := newEnv(t, 4)
	e.seed(t, "item x", 1, 1)
	release := lock(t, e.dsn)
	defer release()
	s := e.service(t, 2, 200*time.Millisecond)
	p, _ := ParseParams("item", "", "", "", "")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.Search(ctx, p) }()
	}
	for s.gate.InUse() != 2 || blockedBackends(t, e.dsn) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	// Third search: refused after the queue wait, without a connection.
	if _, err := s.Search(ctx, p); !errors.Is(err, ErrBusy) || errors.Is(err, ErrTimeout) {
		t.Fatalf("third search: %v, want ErrBusy", err)
	}
	// Stand-ins for a session lookup and a login: they use the same pool.
	start := time.Now()
	for i := 0; i < 5; i++ {
		var one int
		qctx, qcancel := context.WithTimeout(ctx, 2*time.Second)
		err := e.pool.QueryRow(qctx, `SELECT count(*) FROM magneticod.schema_version`).Scan(&one)
		qcancel()
		if err != nil {
			t.Fatalf("other pool user starved: %v", err)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("other pool users were slow: %v", d)
	}
	cancel()
	wg.Wait()
}

func TestServiceCancellationEndsTheBackendAndPoolRecovers(t *testing.T) {
	e := newEnv(t, 4)
	e.seed(t, "item x", 1, 1)
	release := lock(t, e.dsn)
	defer release()
	s := e.service(t, 2, time.Second)
	p, _ := ParseParams("item", "", "", "", "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	errc := make(chan error, 1)
	go func() { _, err := s.Search(ctx, p); errc <- err }()
	for blockedBackends(t, e.dsn) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	err := <-errc
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) || Outcome(err) != "canceled" {
		t.Fatalf("got %v", err)
	}
	for start := time.Now(); blockedBackends(t, e.dsn) != 0; {
		if time.Since(start) > 10*time.Second {
			t.Fatal("cancelled search still waiting on the server")
		}
		time.Sleep(50 * time.Millisecond)
	}
	release()
	if _, err := s.Search(context.Background(), p); err != nil {
		t.Fatalf("pool did not recover: %v", err)
	}
	if s.gate.InUse() != 0 {
		t.Fatal("permit leaked")
	}
}

func lock(t *testing.T, dsn string) func() {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`BEGIN`, `LOCK TABLE magneticod.torrents IN ACCESS EXCLUSIVE MODE`} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	done := false
	rel := func() {
		if !done {
			done = true
			_, _ = c.Exec(ctx, `ROLLBACK`)
			_ = c.Close(ctx)
		}
	}
	t.Cleanup(rel)
	return rel
}

func blockedBackends(t *testing.T, dsn string) int {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
	    WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE 'SELECT id, info_hash, name%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
