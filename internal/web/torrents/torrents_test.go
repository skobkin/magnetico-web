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

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/filecache"
	"github.com/skobkin/magnetico-web/internal/web/pathfmt"
)

func TestAnonymousAndRoot(t *testing.T) {
	e := newEnv(t)
	id := e.seed("x", 5, 1, nil)
	c := e.client()
	for _, p := range []string{"/torrents", fmt.Sprintf("/torrents/%d", id), fmt.Sprintf("/torrents/%d/files", id)} {
		res := c.get(p)
		if res.StatusCode != 303 || !strings.HasPrefix(res.Header.Get("Location"), "/login?next=") {
			t.Errorf("anonymous GET %s: %d %q", p, res.StatusCode, res.Header.Get("Location"))
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

func TestDetails(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Some Name & more", 1536, 1_700_000_000, []codec.File{{Path: "a/b", Size: 1000}, {Path: "c", Size: 536}})
	c := e.signedIn()
	if res := c.get(fmt.Sprintf("/torrents/%d", id)); res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	for _, want := range []string{"Some Name &amp; more", "1.5 KiB (1536 bytes)", "2023-11-14", "magnet:?xt=urn:btih:", "dn=Some", "/files"} {
		if !strings.Contains(c.body, want) {
			t.Errorf("details page lacks %q", want)
		}
	}
	if strings.Contains(c.body, "&tr=") {
		t.Error("the interim magnet link must be tracker-less")
	}
	if s := e.cache.Stats(); s.Builds != 0 || s.Misses != 0 {
		t.Fatalf("details must not decode the file blob: %+v", s)
	}
	for _, p := range []string{"/torrents/0", "/torrents/-1", "/torrents/007", "/torrents/1e3", "/torrents/99999999999", "/torrents/abc", "/torrents/424242", "/torrents/%2B1"} {
		if res := c.get(p); res.StatusCode != 404 {
			t.Errorf("GET %s: %d, want 404", p, res.StatusCode)
		}
		if res := c.get(p + "/files"); res.StatusCode != 404 {
			t.Errorf("GET %s/files: %d, want 404", p, res.StatusCode)
		}
	}
}

func TestFullFlowNestedNavigation(t *testing.T) {
	e := newEnv(t)
	files := []codec.File{
		{Path: "docs/a.txt", Size: 10}, {Path: "docs/sub/deep/b.txt", Size: 20}, {Path: "docs/sub/c.txt", Size: 30}, {Path: "z.bin", Size: 40},
	}
	id := e.seed("flow torrent", 0, 7, files)
	c := e.signedIn()
	c.get("/torrents?q=flow")
	href := ""
	for _, l := range c.links() {
		if strings.HasPrefix(l, "/torrents/") {
			href = l
		}
	}
	if href != fmt.Sprintf("/torrents/%d", id) {
		t.Fatalf("list link %q", href)
	}
	c.get(href)
	files0 := href + "/files"
	if !slicesContains(c.links(), files0) {
		t.Fatal("details must link to the file browser")
	}
	c.get(files0)
	// Root: directories before files, with aggregates.
	iDocs, iZ := strings.Index(c.body, "docs/"), strings.Index(c.body, "z.bin")
	if iDocs < 0 || iZ < 0 || iDocs > iZ {
		t.Fatal("directories must precede files")
	}
	if !strings.Contains(c.body, "<td class=\"num\">60 B</td><td class=\"num\">3</td>") {
		t.Fatalf("directory aggregate missing:\n%s", c.body)
	}
	c.get(files0 + "?dir=docs%2F")
	c.get(files0 + "?dir=docs%2Fsub%2F")
	if !strings.Contains(c.body, "deep/") || !strings.Contains(c.body, "c.txt") {
		t.Fatal("nested listing")
	}
	c.get(files0 + "?dir=docs%2Fsub%2Fdeep%2F")
	if !strings.Contains(c.body, "b.txt") || !strings.Contains(c.body, "20 B") {
		t.Fatal("leaf directory")
	}
	if res := c.get(files0 + "?dir=docs%2Fnope%2F"); res.StatusCode != 404 {
		t.Fatalf("unknown dir: %d", res.StatusCode)
	}
	if res := c.get(files0 + "?dir=docs"); res.StatusCode != 404 { // no trailing slash
		t.Fatalf("dir without a trailing separator: %d", res.StatusCode)
	}
	// Sign out works from a files page (CSRF token present).
	c.get(files0)
	m := tokenRe.FindStringSubmatch(c.body)
	if m == nil {
		t.Fatal("no logout token in the page chrome")
	}
	if res := c.do("POST", "/logout", url.Values{"csrf_token": {m[1]}}); res.StatusCode != 303 {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	if res := c.get(files0); res.StatusCode != 303 {
		t.Fatalf("after logout: %d", res.StatusCode)
	}
}

func slicesContains(s []string, v string) bool { return slices.Contains(s, v) }

// The unusual-path matrix: every directory link of every listing must resolve
// to exactly the directory it names, and the displayed names must carry
// markers instead of replacement characters.
func TestUnusualPathsRoundTrip(t *testing.T) {
	e := newEnv(t)
	files := []codec.File{
		{Path: "100%/x", Size: 1}, {Path: "a%2Fb/x", Size: 2}, {Path: "a+b&c=d#e?f/x", Size: 3},
		{Path: "../up/x", Size: 4}, {Path: "./here/x", Size: 5}, {Path: "/lead/x", Size: 6},
		{Path: "mid//dle/x", Size: 7}, {Path: "bad\xff\xfe/x", Size: 8}, {Path: "ctl\x01\u202e/x", Size: 9},
		{Path: "dup/same", Size: 10}, {Path: "dup/same", Size: 10}, {Path: "dup/same", Size: 11},
		{Path: "тест/файл", Size: 12}, {Path: "end/", Size: 13},
	}
	id := e.seed("odd", 0, 7, files)
	c := e.signedIn()
	base := fmt.Sprintf("/torrents/%d/files", id)

	// Reference: the set of directory prefixes derived independently.
	want := map[string]bool{}
	for _, f := range files {
		p := f.Path
		for i := 0; i < len(p); i++ {
			if p[i] == '/' {
				want[p[:i+1]] = true
			}
		}
	}
	visited := map[string]bool{}
	var walk func(dir string)
	walk = func(dir string) {
		if visited[dir] {
			return
		}
		visited[dir] = true
		link := base
		if dir != "" {
			link += "?" + pathfmt.DirQuery(dir)
		}
		if res := c.get(link); res.StatusCode != 200 {
			t.Fatalf("dir %q: %d", dir, res.StatusCode)
		}
		for _, l := range c.links() {
			u, err := url.Parse(l)
			if err != nil || !strings.HasPrefix(u.Path, base) || u.RawQuery == "" || strings.Contains(u.RawQuery, "from=") {
				continue
			}
			sub := u.Query().Get("dir") // exactly one decoding
			if !strings.HasPrefix(sub, dir) || strings.Count(sub[len(dir):], "/") != 1 || !strings.HasSuffix(sub, "/") {
				// breadcrumb or a parent link: must be a known directory
				if !want[sub] && sub != "" {
					t.Fatalf("link to unknown directory %q (from %q)", sub, dir)
				}
				continue
			}
			if !want[sub] {
				t.Fatalf("link to a directory that does not exist: %q", sub)
			}
			walk(sub)
		}
	}
	walk("")
	for d := range want {
		if !visited[d] {
			t.Errorf("directory %q unreachable by links", d)
		}
	}

	// Markers, no U+FFFD substitution, no normalisation.
	c.get(base + "?" + pathfmt.DirQuery("bad\xff\xfe/"))
	if !strings.Contains(c.body, `\xff`) && !strings.Contains(c.body, `\xff`) {
		t.Error("invalid bytes must be shown as \\xNN markers")
	}
	c.get(base)
	if strings.ContainsRune(c.body, '\uFFFD') {
		t.Error("U+FFFD must never be substituted")
	}
	for _, m := range []string{`\xff</span>`, `\xfe</span>`, `\u{0001}</span>`, `\u{202E}</span>`, "(empty)"} {
		if !strings.Contains(c.body, m) {
			t.Errorf("marker %q missing", m)
		}
	}
	if false {
		t.Errorf("markers missing:\n%s", c.body)
	}
	if !strings.Contains(c.body, "a%2Fb") && !strings.Contains(c.body, "a%2Fb") {
		t.Error("a literal %2F in a name must be shown literally")
	}
	// Duplicates are kept and marked.
	c.get(base + "?dir=dup%2F")
	if strings.Count(c.body, "same") != 3 || strings.Count(c.body, "(duplicate path)") != 2 {
		t.Errorf("duplicates:\n%s", c.body)
	}
	// "a%2Fb/" is a real directory name; its link decodes exactly once to it.
	if res := c.get(base + "?dir=a%252Fb%2F"); res.StatusCode != 200 {
		t.Errorf("a%%2Fb/ must resolve: %d", res.StatusCode)
	}
	// Decoding twice would turn the next value into a%2Fb/ (it must not).
	if res := c.get(base + "?dir=a%25252Fb%2F"); res.StatusCode != 404 {
		t.Errorf("a%%252Fb/ does not exist and must not alias a%%2Fb/: %d", res.StatusCode)
	}
	if res := c.get(base + "?dir=a%2Fb%2F"); res.StatusCode != 404 {
		t.Errorf("a/b/ must not alias a%%2Fb/: %d", res.StatusCode)
	}
}

func TestFilePagination500AndPositions(t *testing.T) {
	e := newEnv(t)
	var files []codec.File
	for i := 0; i < 1203; i++ {
		files = append(files, codec.File{Path: fmt.Sprintf("big/f%05d", i), Size: 1})
	}
	id := e.seed("many", 0, 1, files)
	c := e.signedIn()
	base := fmt.Sprintf("/torrents/%d/files?dir=big%%2F", id)
	counts := []int{}
	next := base
	for next != "" {
		c.get(next)
		counts = append(counts, strings.Count(c.body, `<td class="name">`))
		next = c.link("next")
	}
	if !slices.Equal(counts, []int{500, 500, 203}) {
		t.Fatalf("page sizes %v", counts)
	}
	c.get(base + "&from=500")
	if !strings.Contains(c.body, "f00500") || strings.Contains(c.body, "f00499") || !strings.Contains(c.body, "501–1000 of 1203") {
		t.Fatal("positional page")
	}
	if p := c.link("prev"); !strings.Contains(p, "from=0") && strings.Contains(p, "from=") {
		t.Fatalf("prev link %q", p)
	}
	for _, bad := range []string{"&from=-1", "&from=abc", "&from=0500", "&from=1203", "&from=99999999999999999999"} {
		if res := c.get(base + bad); res.StatusCode != 400 {
			t.Errorf("%s: %d, want 400", bad, res.StatusCode)
		}
	}
	if s := e.cache.Stats(); s.Builds != 1 {
		t.Fatalf("one decode for all pages, got %+v", s)
	}
}

func TestHundredNavigationsOneDecodeAndReplacement(t *testing.T) {
	e := newEnv(t)
	files := []codec.File{{Path: "d/a", Size: 1}, {Path: "d/b", Size: 2}, {Path: "e", Size: 3}}
	id := e.seed("nav", 0, 1, files)
	c := e.signedIn()
	for i := 0; i < 100; i++ {
		dir := []string{"", "?dir=d%2F"}[i%2]
		if res := c.get(fmt.Sprintf("/torrents/%d/files%s", id, dir)); res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
	}
	if s := e.cache.Stats(); s.Builds != 1 || s.Hits != 99 {
		t.Fatalf("stats %+v", s)
	}
	// Replace the blob: the next request must show the new list.
	nb, _, _ := codec.Encode([]codec.File{{Path: "newfile", Size: 6}}, codec.ProfileCompact, codec.DefaultLimits())
	e.exec(`UPDATE magneticod.torrent_file_blobs SET data=$2 WHERE torrent_id=$1`, id, nb)
	e.exec(`UPDATE magneticod.torrents SET file_count=1, total_size=6 WHERE id=$1`, id)
	c.get(fmt.Sprintf("/torrents/%d/files", id))
	if !strings.Contains(c.body, "newfile") || strings.Contains(c.body, "d/") {
		t.Fatalf("stale index served:\n%s", c.body)
	}
	if s := e.cache.Stats(); s.Builds != 2 || s.Replaced != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestBlobFailuresAreTyped500sWithSafeLogs(t *testing.T) {
	e := newEnv(t)
	good := []codec.File{{Path: "SECRETPATH/x", Size: 1}, {Path: "y", Size: 2}}
	missing := e.seed("SECRETNAME-missing", 3, 1, nil)
	e.exec(`UPDATE magneticod.torrents SET file_count=3 WHERE id=$1`, missing) // multi-file, no blob
	garbage := e.seed("SECRETNAME-garbage", 0, 1, good)
	e.exec(`UPDATE magneticod.torrent_file_blobs SET data='\x0102030405' WHERE torrent_id=$1`, garbage)
	mismatch := e.seed("SECRETNAME-mismatch", 0, 1, good)
	e.exec(`UPDATE magneticod.torrents SET file_count=5 WHERE id=$1`, mismatch)
	oversized := e.seed("SECRETNAME-oversized", 0, 1, good)
	e.exec(`UPDATE magneticod.torrent_file_blobs SET data = '\x0100'::bytea || repeat('\x00'::text,1)::bytea WHERE torrent_id=$1`, oversized)
	e.exec(`UPDATE magneticod.torrent_file_blobs SET data = decode(repeat('00', 33*1024*1024), 'hex') WHERE torrent_id=$1`, oversized)

	c := e.signedIn()
	for name, id := range map[string]int32{"missing": missing, "garbage": garbage, "mismatch": mismatch, "oversized": oversized} {
		res := c.get(fmt.Sprintf("/torrents/%d/files", id))
		if res.StatusCode != 500 {
			t.Errorf("%s: %d, want 500", name, res.StatusCode)
		}
		if strings.Contains(c.body, "<table") {
			t.Errorf("%s: a partial or empty list was rendered", name)
		}
	}
	// Details of those torrents still work (no blob decode).
	if res := c.get(fmt.Sprintf("/torrents/%d", garbage)); res.StatusCode != 200 {
		t.Fatalf("details of a torrent with a corrupt blob: %d", res.StatusCode)
	}
	logs := e.log.String()
	for _, forbidden := range []string{"SECRETNAME", "SECRETPATH"} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("logs leak %q", forbidden)
		}
	}
	for _, want := range []string{"category=corrupt_blob", "category=missing_blob", "limit=stored_bytes"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
	if s := e.cache.Stats(); s.Entries != 0 || s.Retries != 0 {
		t.Fatalf("errors must neither be cached nor retried: %+v", s)
	}
}

func TestLogsCarryNoSearchTextNamesOrPaths(t *testing.T) {
	e := newEnv(t)
	id := e.seed("PRIVATENAME torrent", 0, 1, []codec.File{{Path: "PRIVATEPATH/f", Size: 1}})
	c := e.signedIn()
	c.get("/torrents?q=PRIVATETERM+PRIVATENAME")
	c.get(fmt.Sprintf("/torrents/%d", id))
	c.get(fmt.Sprintf("/torrents/%d/files?dir=PRIVATEPATH%%2F", id))
	c.get(fmt.Sprintf("/torrents/%d/files?dir=PRIVATEMISSING%%2F", id))
	c.get("/torrents?q=PRIVATETERM&cursor=PRIVATECURSOR")
	logs := e.log.String()
	for _, f := range []string{"PRIVATE", "private", "correct horse"} {
		if strings.Contains(logs, f) {
			t.Errorf("logs contain %q:\n%s", f, logs)
		}
	}
	if !strings.Contains(logs, "msg=search") {
		t.Error("the structured search record is missing")
	}
}

func TestDecodeSaturationIsBusyAndRecovers(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.cache.QueueWait = 100 * time.Millisecond; o.cache.MaxWaiters = 1 })
	id := e.seed("sat", 0, 1, []codec.File{{Path: "a/b", Size: 1}, {Path: "c", Size: 1}})
	c := e.signedIn()
	g := e.cache.Gate()
	hold, err := g.Acquire(context.Background(), g.Capacity()-1)
	if err != nil {
		t.Fatal(err)
	}
	res := c.get(fmt.Sprintf("/torrents/%d/files", id))
	if res.StatusCode != 503 || res.Header.Get("Retry-After") != "5" || !strings.Contains(c.body, "busy") {
		t.Fatalf("saturated decode: %d ra=%q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	// Details and search stay available while decode capacity is exhausted.
	if r := c.get(fmt.Sprintf("/torrents/%d", id)); r.StatusCode != 200 {
		t.Fatalf("details during decode saturation: %d", r.StatusCode)
	}
	hold()
	if r := c.get(fmt.Sprintf("/torrents/%d/files", id)); r.StatusCode != 200 {
		t.Fatalf("after release: %d", r.StatusCode)
	}
	_ = filecache.DecodeCapacity
}

// A directory is addressed by its exact path in the `dir` query parameter, and
// the HTTP server bounds the request line (see server.TestRequestLineLimitBoundary).
// Paths up to codec.MaxPathBytes are valid, and percent-escaping can triple
// their length, so a codec-valid directory can have a link no server will
// accept. The link must still be generated losslessly (never truncated or
// altered), and a moderately long directory must round-trip.
func TestLongDirectoryLinkLimitation(t *testing.T) {
	e := newEnv(t)
	moderate := strings.Repeat("d", 3000) // escaped link far below the 16 KiB bound
	huge := strings.Repeat("é", 20000)    // 40 000 bytes (codec-valid, < 64 KiB); 120 000 escaped
	id := e.seed("long", 0, 1, []codec.File{
		{Path: moderate + "/f", Size: 1},
		{Path: huge + "/f", Size: 1},
	})
	c := e.signedIn()

	c.get(fmt.Sprintf("/torrents/%d/files", id))
	var gotHuge string
	for _, l := range c.links() {
		if strings.Contains(l, "/files?dir=") && len(l) > 100000 {
			gotHuge = l
		}
	}
	if want := filesHref(id, huge+"/", 0); gotHuge != want {
		t.Fatalf("link to the huge directory was altered or missing (got %d bytes, want %d)", len(gotHuge), len(want))
	}
	const serverLimit = 16 << 10 // server.maxHeaderBytes
	if len(gotHuge) <= serverLimit {
		t.Fatalf("test premise: link %d bytes should exceed the %d-byte server bound", len(gotHuge), serverLimit)
	}

	link := filesHref(id, moderate+"/", 0)
	if len(link) >= serverLimit {
		t.Fatalf("test premise: moderate link is %d bytes", len(link))
	}
	if res := c.get(link); res.StatusCode != 200 || !strings.Contains(c.body, ">f<") {
		t.Fatalf("moderate directory: %d", res.StatusCode)
	}
}
