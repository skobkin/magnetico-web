package torrents

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAnonymousAndRoot(t *testing.T) {
	e := newEnv(t)
	id := e.seed("x", 5, 1, nil)
	c := e.client()
	for _, p := range []string{"/torrents", fmt.Sprintf("/torrents/%d", id)} {
		res := c.get(p)
		if res.StatusCode != 303 || !strings.HasPrefix(res.Header.Get("Location"), "/login?next=") {
			t.Errorf("anonymous GET %s: %d %q", p, res.StatusCode, res.Header.Get("Location"))
		}
	}
	// The JSON file-view endpoints never redirect: 401 problem+json.
	for _, p := range []string{fmt.Sprintf("/torrents/%d/files", id), fmt.Sprintf("/torrents/%d/files/tree", id),
		fmt.Sprintf("/torrents/%d/files?dir=0_1&offset=0&limit=5", id), "/torrents/424242/files/tree"} {
		res := c.get(p)
		if res.StatusCode != 401 || res.Header.Get("Location") != "" || res.Header.Get("Content-Type") != "application/problem+json" {
			t.Errorf("anonymous GET %s: %d %q %q", p, res.StatusCode, res.Header.Get("Location"), res.Header.Get("Content-Type"))
		}
		if strings.Contains(c.body, "tree") && strings.Contains(c.body, `"dirs"`) {
			t.Errorf("anonymous GET %s leaked file data", p)
		}
	}
	c.signIn()
	if res := c.get("/"); res.StatusCode != 303 || res.Header.Get("Location") != "/torrents" {
		t.Errorf("GET /: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res := c.do("POST", "/torrents", url.Values{}); res.StatusCode != 405 && res.StatusCode != 403 {
		t.Errorf("POST /torrents: %d", res.StatusCode)
	}
	if res := c.get("/torrents/nope/x"); res.StatusCode != 404 {
		t.Errorf("unknown path: %d", res.StatusCode)
	}
}

func TestSearchPaginationBothDirections(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 55; i++ {
		e.seed(fmt.Sprintf("item %03d", i), int64(1000+i%7), int64(10_000+i), nil) // size ties
	}
	e.seed("unrelated", 1, 1, nil)
	c := e.signedIn()

	for _, tc := range []struct{ sort, order string }{{"discovered", "desc"}, {"discovered", "asc"}, {"size", "desc"}, {"name", "asc"}, {"name", "desc"}} {
		t.Run(tc.sort+"-"+tc.order, func(t *testing.T) {
			base := "/torrents?q=item&sort=" + tc.sort + "&order=" + tc.order + "&size=20"
			var fwd []int32
			next := base
			var pages [][]int32
			var prevLink string
			for next != "" {
				if res := c.get(next); res.StatusCode != 200 {
					t.Fatalf("%s: %d", next, res.StatusCode)
				}
				ids := c.rowIDs()
				pages = append(pages, ids)
				fwd = append(fwd, ids...)
				prevLink = c.link("prev")
				next = c.link("next")
				for _, l := range []string{next, prevLink} {
					if l != "" && (!strings.Contains(l, "q=item") || !strings.Contains(l, "sort="+tc.sort) ||
						!strings.Contains(l, "order="+tc.order) || !strings.Contains(l, "size=20")) {
						t.Fatalf("link loses parameters: %s", l)
					}
				}
			}
			if len(fwd) != 55 || len(pages) != 3 {
				t.Fatalf("got %d rows in %d pages", len(fwd), len(pages))
			}
			seen := map[int32]bool{}
			for _, id := range fwd {
				if seen[id] {
					t.Fatalf("duplicate %d", id)
				}
				seen[id] = true
			}
			// Backward from the last page reproduces the earlier pages exactly.
			for i := len(pages) - 1; i > 0; i-- {
				if prevLink == "" {
					t.Fatalf("no previous link on page %d", i+1)
				}
				c.get(prevLink)
				if got := c.rowIDs(); !slices.Equal(got, pages[i-1]) {
					t.Fatalf("backward page %d = %v, want %v", i, got, pages[i-1])
				}
				prevLink = c.link("prev")
			}
			if prevLink != "" {
				t.Fatal("first page must have no previous link")
			}
		})
	}
}

func TestSearchStatesAndErrors(t *testing.T) {
	e := newEnv(t)
	e.seed("ubuntu desktop", 5, 5, nil)
	c := e.signedIn()

	c.get("/torrents?q=nomatchatall")
	if c.res.StatusCode != 200 || !strings.Contains(c.body, "No torrents match") {
		t.Fatalf("empty result: %d", c.res.StatusCode)
	}
	c.get("/torrents?q=ab+ubuntu")
	if !strings.Contains(c.body, "shorter than 3") || !strings.Contains(c.body, "ubuntu") {
		t.Fatal("short words must be reported as ignored")
	}
	for name, q := range map[string]string{"nul": "a%00bcd", "invalid-utf8": "%ff%fe%fdabc", "too-long": strings.Repeat("a", 600)} {
		if res := c.get("/torrents?q=" + q); res.StatusCode != 400 || !strings.Contains(c.body, "not valid") {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	if res := c.get("/torrents?q=ubuntu&cursor=garbage"); res.StatusCode != 400 || !strings.Contains(c.body, "no longer valid") {
		t.Errorf("bad cursor: %d", res.StatusCode)
	}
	// Info-hash lookup.
	var hash string
	if err := e.pool.QueryRow(tctx(t), `SELECT encode(info_hash,'hex') FROM magneticod.torrents LIMIT 1`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	c.get("/torrents?q=" + strings.ToUpper(hash))
	if len(c.rowIDs()) != 1 {
		t.Fatal("info-hash lookup must find exactly one row")
	}
	c.get("/torrents?q=" + strings.Repeat("0", 40))
	if !strings.Contains(c.body, "No torrent has that info hash") {
		t.Fatal("missing-hash empty state")
	}
	// Escaping: a hostile name is escaped, not interpreted.
	e.seed(`<script>alert(1)</script> hostile`, 1, 9, nil)
	c.get("/torrents?q=hostile")
	if strings.Contains(c.body, "<script>alert") || !strings.Contains(c.body, "&lt;script&gt;") {
		t.Fatal("name not escaped")
	}
	c.get(`/torrents?q="><script>x</script>`)
	if strings.Contains(c.body, "<script>x") {
		t.Fatal("query echo not escaped")
	}
}

func TestSearchTimeoutBusyAndLoginResponsiveness(t *testing.T) {
	e := newEnv(t, func(o *envOpts) {
		o.searchConc = 1
		o.searchWait = 150 * time.Millisecond
		o.searchTO = 1500 * time.Millisecond
	})
	e.seed("ubuntu", 5, 5, nil)
	c := e.signedIn()

	// Block the torrents table so a real search statement waits on the lock.
	tx, err := e.pool.Begin(tctx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(tctx(t), `LOCK TABLE magneticod.torrents IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	type out struct {
		code int
		body string
		ra   string
	}
	results := make(chan out, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cc := e.client()
			cc.jar = c.jar
			cc.get("/torrents?q=ubuntu")
			results <- out{cc.res.StatusCode, cc.body, cc.res.Header.Get("Retry-After")}
		}()
	}
	// While searches are stuck, login and other pages stay responsive.
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	e.client().signIn()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("login took %v during blocked searches", d)
	}
	wg.Wait()
	close(results)
	var busy, timeout int
	for r := range results {
		switch {
		case r.code == 503 && strings.Contains(r.body, "busy"):
			busy++
			if r.ra != "5" {
				t.Errorf("busy Retry-After %q", r.ra)
			}
		case r.code == 503 && strings.Contains(r.body, "did not finish"):
			timeout++
			if r.ra != "30" || strings.Contains(r.body, "not valid") {
				t.Errorf("timeout page: ra=%q", r.ra)
			}
		default:
			t.Errorf("unexpected %d: %.200s", r.code, r.body)
		}
	}
	if busy != 2 || timeout != 1 {
		t.Fatalf("busy=%d timeout=%d, want 2 and 1", busy, timeout)
	}
	tx.Rollback(context.Background())
	if res := c.get("/torrents?q=ubuntu"); res.StatusCode != 200 {
		t.Fatalf("search after the lock is released: %d", res.StatusCode)
	}
}
