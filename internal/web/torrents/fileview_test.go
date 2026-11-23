package torrents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/filecache"
	"github.com/skobkin/magnetico-web/internal/web/fileview"
)

// ---- JSON documents (test-side view of the file-view contract) ----------------

type jname struct {
	Name       *string `json:"name"`
	NameBase64 string  `json:"name_base64"`
}

func (n jname) bytes(t *testing.T) string {
	t.Helper()
	if n.Name != nil {
		return *n.Name
	}
	b, err := base64.StdEncoding.DecodeString(n.NameBase64)
	if err != nil || n.NameBase64 == "" {
		t.Fatalf("name without a usable name_base64: %v", err)
	}
	return string(b)
}

type jentry struct {
	Type string `json:"type"`
	jname
	Path       string `json:"path"`
	FileCount  int    `json:"file_count"`
	Size       int64  `json:"size"`
	EntryCount int    `json:"entry_count"`
	FileIndex  int    `json:"file_index"`
	Duplicate  bool   `json:"duplicate"`
}

type jpage struct {
	Version   int    `json:"version"`
	Path      string `json:"path"`
	Ancestors []struct {
		jname
		Path string `json:"path"`
	} `json:"ancestors"`
	Dir struct {
		FileCount  int   `json:"file_count"`
		Size       int64 `json:"size"`
		EntryCount int   `json:"entry_count"`
	} `json:"dir"`
	Offset     int      `json:"offset"`
	Limit      int      `json:"limit"`
	NextOffset *int     `json:"next_offset"`
	Entries    []jentry `json:"entries"`
}

type jtreeDir struct {
	jname
	FileCount int         `json:"file_count"`
	Size      int64       `json:"size"`
	Dirs      []*jtreeDir `json:"dirs"`
	Files     []jentry    `json:"files"`
}

type jtree struct {
	Version   int      `json:"version"`
	FileCount int      `json:"file_count"`
	Size      int64    `json:"size"`
	Root      jtreeDir `json:"root"`
}

type jproblem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Alternate string `json:"alternate"`
}

func (c *client) dir(id int32, query string) (*jpage, int) {
	c.t.Helper()
	u := fmt.Sprintf("/torrents/%d/files", id)
	if query != "" {
		u += "?" + query
	}
	res := c.get(u)
	if res.StatusCode != 200 {
		return nil, res.StatusCode
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		c.t.Fatalf("content type %q", ct)
	}
	var p jpage
	if err := json.Unmarshal([]byte(c.body), &p); err != nil {
		c.t.Fatalf("%v\n%.300s", err, c.body)
	}
	return &p, 200
}

func (c *client) tree(id int32) (*jtree, int) {
	c.t.Helper()
	res := c.get(fmt.Sprintf("/torrents/%d/files/tree", id))
	if res.StatusCode != 200 {
		return nil, res.StatusCode
	}
	var d jtree
	if err := json.Unmarshal([]byte(c.body), &d); err != nil {
		c.t.Fatalf("%v", err)
	}
	return &d, 200
}

func (c *client) problem() jproblem {
	c.t.Helper()
	if ct := c.res.Header.Get("Content-Type"); ct != "application/problem+json" {
		c.t.Fatalf("content type %q, body %q", ct, c.body)
	}
	var p jproblem
	if err := json.Unmarshal([]byte(c.body), &p); err != nil {
		c.t.Fatal(err)
	}
	return p
}

// walkDirs visits every directory through positional paths only, returning
// the canonical path (joined with "/") of every file and the visited dirs.
func (c *client) walkDirs(id int32, limit int) (files map[int]string, dirs []string) {
	c.t.Helper()
	files = map[int]string{}
	var visit func(path, prefix string)
	visit = func(path, prefix string) {
		dirs = append(dirs, prefix)
		off := 0
		for {
			q := fmt.Sprintf("dir=%s&offset=%d&limit=%d", path, off, limit)
			p, code := c.dir(id, q)
			if code != 200 {
				c.t.Fatalf("dir %q: %d", path, code)
			}
			for _, e := range p.Entries {
				if e.Type == "dir" {
					visit(e.Path, prefix+e.jname.bytes(c.t)+"/")
				} else {
					files[e.FileIndex] = prefix + e.jname.bytes(c.t)
				}
			}
			if p.NextOffset == nil {
				return
			}
			off = *p.NextOffset
		}
	}
	visit("", "")
	return files, dirs
}

func flattenTree(t *testing.T, d *jtreeDir, prefix string, out map[int]string) {
	for _, c := range d.Dirs {
		flattenTree(t, c, prefix+c.jname.bytes(t)+"/", out)
	}
	for _, f := range d.Files {
		out[f.FileIndex] = prefix + f.jname.bytes(t)
	}
}

// ---- tests ---------------------------------------------------------------------

func TestDetailsCarriesModeAndNeverDecodes(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Some Name & more", 1536, 1_700_000_000, []codec.File{{Path: "a/b", Size: 1000}, {Path: "c", Size: 536}})
	c := e.signedIn()
	if res := c.get(fmt.Sprintf("/torrents/%d", id)); res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	for _, want := range []string{"Some Name &amp; more", "1.5 KiB (1536 bytes)", "2023-11-14", "magnet:?xt=urn:btih:", "dn=Some",
		`data-mode="tree"`, fmt.Sprintf(`data-tree-url="/torrents/%d/files/tree"`, id), fmt.Sprintf(`data-dir-url="/torrents/%d/files"`, id), "<noscript>"} {
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
		for _, suffix := range []string{"/files", "/files/tree"} {
			if res := c.get(p + suffix); res.StatusCode != 404 || c.problem().Type != "urn:magnetico:problem:not-found" {
				t.Errorf("GET %s%s: %d, want 404 problem", p, suffix, res.StatusCode)
			}
		}
	}
}

func TestDetailsModeIsDirsForLargeTorrentsAndSurvivesPeekFailure(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.view.MaxTreeFiles = 3 })
	big := e.seed("big", 0, 1, []codec.File{{Path: "a", Size: 1}, {Path: "b", Size: 1}, {Path: "c", Size: 1}, {Path: "d", Size: 1}})
	small := e.seed("small", 0, 1, []codec.File{{Path: "a", Size: 1}, {Path: "b", Size: 1}})
	nopeek := e.seed("nopeek", 3, 1, nil)
	e.exec(`UPDATE magneticod.torrents SET file_count=3 WHERE id=$1`, nopeek) // multi-file without blob: the peek fails
	c := e.signedIn()
	for id, want := range map[int32]string{big: "dirs", small: "tree", nopeek: "dirs"} {
		if res := c.get(fmt.Sprintf("/torrents/%d", id)); res.StatusCode != 200 || !strings.Contains(c.body, `data-mode="`+want+`"`) {
			t.Errorf("torrent %d: %d, want mode %s", id, res.StatusCode, want)
		}
	}
	if strings.Contains(e.log.String(), "SECRET") || !strings.Contains(e.log.String(), "file view decision unavailable") {
		t.Errorf("the failed decision should be logged by category:\n%s", e.log.String())
	}
}

func TestPositionalNavigationEveryDirectory(t *testing.T) {
	e := newEnv(t)
	files := []codec.File{
		{Path: "docs/a.txt", Size: 10}, {Path: "docs/sub/deep/b.txt", Size: 20}, {Path: "docs/sub/c.txt", Size: 30}, {Path: "z.bin", Size: 40},
	}
	id := e.seed("flow", 0, 7, files)
	c := e.signedIn()

	root, code := c.dir(id, "")
	if code != 200 || root.Version != 1 || root.Path != "" || root.Dir.FileCount != 4 || root.Dir.Size != 100 {
		t.Fatalf("root: %+v %d", root, code)
	}
	if len(root.Entries) != 2 || root.Entries[0].Type != "dir" || root.Entries[0].Path != "0" || root.Entries[0].FileCount != 3 ||
		root.Entries[0].Size != 60 || root.Entries[1].Type != "file" || root.Entries[1].FileIndex != 3 {
		t.Fatalf("root entries (directories first, positional paths, aggregates): %+v", root.Entries)
	}
	sub, _ := c.dir(id, "dir=0_0")
	if sub.Path != "0_0" || len(sub.Ancestors) != 2 || sub.Ancestors[0].jname.bytes(t) != "docs" || sub.Ancestors[0].Path != "0" ||
		sub.Ancestors[1].jname.bytes(t) != "sub" || sub.Ancestors[1].Path != "0_0" {
		t.Fatalf("ancestors: %+v", sub)
	}
	if sub.Entries[0].Type != "dir" || sub.Entries[0].Path != "0_0_0" || sub.Entries[1].jname.bytes(t) != "c.txt" {
		t.Fatalf("sub entries: %+v", sub.Entries)
	}
	got, dirs := c.walkDirs(id, 2)
	if len(got) != 4 || got[0] != "docs/a.txt" || got[1] != "docs/sub/c.txt" || got[2] != "docs/sub/deep/b.txt" || got[3] != "z.bin" {
		t.Fatalf("navigation: %v", got)
	}
	if len(dirs) != 4 { // root, docs, docs/sub, docs/sub/deep
		t.Fatalf("directories: %v", dirs)
	}
	// Directory and file positions are separate namespaces: the root's file
	// z.bin has file_index 3, while path "3" is out of range for directories.
	if _, code := c.dir(id, "dir=3"); code != 404 {
		t.Fatalf("a file position must not address a directory: %d", code)
	}
	if _, code := c.dir(id, "dir=0_0_0_0"); code != 404 {
		t.Fatalf("descending below a leaf directory: %d", code)
	}
	// Sign out still works from the details page chrome.
	c.get(fmt.Sprintf("/torrents/%d", id))
	m := tokenRe.FindStringSubmatch(c.body)
	if m == nil {
		t.Fatal("no logout token in the page chrome")
	}
	if res := c.do("POST", "/logout", url.Values{"csrf_token": {m[1]}}); res.StatusCode != 303 {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	if res := c.get(fmt.Sprintf("/torrents/%d/files", id)); res.StatusCode != 401 {
		t.Fatalf("after logout: %d", res.StatusCode)
	}
}

// Both endpoints preserve unusual path bytes, duplicates and order exactly.
func TestUnusualPathsAndDuplicatesByteExact(t *testing.T) {
	e := newEnv(t)
	files := []codec.File{
		{Path: "100%/x", Size: 1}, {Path: "a%2Fb/x", Size: 2}, {Path: "a+b&c=d#e?f/x", Size: 3},
		{Path: "../up/x", Size: 4}, {Path: "./here/x", Size: 5}, {Path: "/lead/x", Size: 6},
		{Path: "mid//dle/x", Size: 7}, {Path: "bad\xff\xfe/x", Size: 8}, {Path: "ctl\x01\u202e/x", Size: 9},
		{Path: "dup/same", Size: 10}, {Path: "dup/same", Size: 10}, {Path: "dup/same", Size: 11},
		{Path: "тест/файл", Size: 12}, {Path: "end/", Size: 13}, {Path: "q\"uo\\te</>&\u2028", Size: 14},
		{Path: "nul\x00byte/y", Size: 15}, {Path: "real\uFFFDchar/z", Size: 16},
	}
	id := e.seed("odd", 0, 7, files)
	c := e.signedIn()

	canon := slices.Clone(files)
	slices.SortStableFunc(canon, func(a, b codec.File) int {
		if x := strings.Compare(a.Path, b.Path); x != 0 {
			return x
		}
		return int(a.Size - b.Size)
	})

	fromDirs, dirs := c.walkDirs(id, 3)
	tr, code := c.tree(id)
	if code != 200 {
		t.Fatalf("tree: %d", code)
	}
	fromTree := map[int]string{}
	flattenTree(t, &tr.Root, "", fromTree)
	for i, f := range canon {
		if fromDirs[i] != f.Path {
			t.Errorf("paged: file_index %d = %q, want %q", i, fromDirs[i], f.Path)
		}
		if fromTree[i] != f.Path {
			t.Errorf("tree: file_index %d = %q, want %q", i, fromTree[i], f.Path)
		}
	}
	if len(fromDirs) != len(files) || len(fromTree) != len(files) || tr.FileCount != len(files) {
		t.Fatalf("counts: paged %d tree %d", len(fromDirs), len(fromTree))
	}
	want := map[string]bool{}
	for _, f := range files {
		for i := 0; i < len(f.Path); i++ {
			if f.Path[i] == '/' {
				want[f.Path[:i+1]] = true
			}
		}
	}
	want[""] = true
	for _, d := range dirs {
		delete(want, d)
	}
	if len(want) != 0 {
		t.Errorf("directories unreachable by positional paths: %v", want)
	}
	// Invalid bytes travel as base64, never as U+FFFD.
	root, _ := c.dir(id, "limit=1000")
	foundBad := false
	for _, en := range root.Entries {
		if en.Type == "dir" && en.Name == nil {
			foundBad = foundBad || en.jname.bytes(t) == "bad\xff\xfe"
		}
	}
	if !foundBad {
		t.Error("the invalid-UTF-8 directory must be sent as name_base64")
	}
	// Duplicates are distinct, flagged entries.
	var dupDir string
	for _, en := range root.Entries {
		if en.Type == "dir" && en.Name != nil && *en.Name == "dup" {
			dupDir = en.Path
		}
	}
	d, _ := c.dir(id, "dir="+dupDir)
	if len(d.Entries) != 3 || d.Entries[0].Duplicate || !d.Entries[1].Duplicate || !d.Entries[2].Duplicate ||
		d.Entries[0].FileIndex == d.Entries[1].FileIndex {
		t.Fatalf("duplicates: %+v", d.Entries)
	}
}

func TestPaginationLimitOffsetAndMalformedValues(t *testing.T) {
	e := newEnv(t)
	var files []codec.File
	for i := 0; i < 1203; i++ {
		files = append(files, codec.File{Path: fmt.Sprintf("big/f%05d", i), Size: 1})
	}
	id := e.seed("many", 0, 1, files)
	c := e.signedIn()
	counts := []int{}
	off := 0
	for {
		p, code := c.dir(id, fmt.Sprintf("dir=0&offset=%d&limit=500", off))
		if code != 200 {
			t.Fatal(code)
		}
		counts = append(counts, len(p.Entries))
		if p.NextOffset == nil {
			break
		}
		off = *p.NextOffset
	}
	if !slices.Equal(counts, []int{500, 500, 203}) {
		t.Fatalf("page sizes %v", counts)
	}
	p, _ := c.dir(id, "dir=0&offset=500&limit=2")
	if p.Entries[0].jname.bytes(t) != "f00500" || p.Offset != 500 || p.Limit != 2 || *p.NextOffset != 502 {
		t.Fatalf("positional page: %+v", p)
	}
	// Defaults: the UI page size.
	p, _ = c.dir(id, "dir=0")
	if len(p.Entries) != fileview.DefaultLimit || p.Limit != fileview.DefaultLimit {
		t.Fatalf("default limit: %d/%d", len(p.Entries), p.Limit)
	}
	// An offset beyond the end is an empty, valid page.
	p, code := c.dir(id, "dir=0&offset=5000")
	if code != 200 || len(p.Entries) != 0 || p.NextOffset != nil {
		t.Fatalf("beyond the end: %+v %d", p, code)
	}
	for _, bad := range []string{
		"dir=x", "dir=-1", "dir=01", "dir=1__2", "dir=0_", "dir=_0", "dir=0%20", "dir=1000000", "dir=0x1",
		"offset=-1", "offset=abc", "offset=0500", "offset=99999999999999999999", "limit=0", "limit=1001", "limit=-1", "limit=1.5",
		"dir=0&dir=1", "offset=1&offset=2", "limit=5&limit=6", "dir=%zz", "dir=" + strings.TrimSuffix(strings.Repeat("0_", 257), "_"),
	} {
		res := c.get(fmt.Sprintf("/torrents/%d/files?%s", id, bad))
		wantStatus := 400
		if bad == "dir=1000000" {
			wantStatus = 400 // seven digits: not a valid component
		}
		if res.StatusCode != wantStatus || c.problem().Status != wantStatus {
			t.Errorf("%s: %d, want %d", bad, res.StatusCode, wantStatus)
		}
	}
	if res := c.get(fmt.Sprintf("/torrents/%d/files?dir=999999", id)); res.StatusCode != 404 {
		t.Errorf("out-of-range six-digit component: %d, want 404", res.StatusCode)
	}
	if s := e.cache.Stats(); s.Builds != 1 {
		t.Fatalf("one decode for all pages, got %+v", s)
	}
}

func TestJSONResponseHeaders(t *testing.T) {
	e := newEnv(t)
	id := e.seed("h", 0, 1, []codec.File{{Path: "a/b", Size: 1}, {Path: "c", Size: 2}})
	c := e.signedIn()
	for _, p := range []string{"/files", "/files/tree", "/files?dir=9", "/files?limit=0"} {
		res := c.get(fmt.Sprintf("/torrents/%d%s", id, p))
		if cc := res.Header.Get("Cache-Control"); cc != "private, no-store" {
			t.Errorf("%s: Cache-Control %q", p, cc)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("X-Robots-Tag") == "" {
			t.Errorf("%s: security headers missing: %v", p, res.Header)
		}
		if res.StatusCode == 200 && res.Header.Get("Content-Length") == "" {
			t.Errorf("%s: no Content-Length", p)
		}
	}
	if res := c.do("POST", fmt.Sprintf("/torrents/%d/files", id), url.Values{}); res.StatusCode == 200 {
		t.Errorf("POST must not be served: %d", res.StatusCode)
	}
}

func TestTreeRefusedForLargeTorrentButPaginationWorks(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.view.MaxTreeFiles = 50 })
	var files []codec.File
	for i := 0; i < 120; i++ {
		files = append(files, codec.File{Path: fmt.Sprintf("d%d/f%03d", i%4, i), Size: 1})
	}
	id := e.seed("large", 0, 1, files)
	c := e.signedIn()
	if res := c.get(fmt.Sprintf("/torrents/%d", id)); !strings.Contains(c.body, `data-mode="dirs"`) || res.StatusCode != 200 {
		t.Fatal("a large torrent must advertise directory mode")
	}
	builds := e.cache.Stats().Builds
	res := c.get(fmt.Sprintf("/torrents/%d/files/tree", id))
	p := c.problem()
	if res.StatusCode != 422 || p.Type != "urn:magnetico:problem:full-tree-unavailable" || p.Alternate != fmt.Sprintf("/torrents/%d/files", id) {
		t.Fatalf("%d %+v", res.StatusCode, p)
	}
	if e.cache.Stats().Builds != builds {
		t.Fatal("a refused tree must not decode the list")
	}
	got, _ := c.walkDirs(id, 20)
	if len(got) != 120 {
		t.Fatalf("pagination reached %d of 120 files", len(got))
	}
}

func TestExactSizeCapGives422AndNoPartialBody(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.view.MaxTreeBytes = 8 << 10; o.view.MaxPageBytes = 4 << 10 })
	var files []codec.File
	for i := 0; i < 400; i++ { // small count, eligible by the estimate, but ~40 KiB of JSON
		files = append(files, codec.File{Path: fmt.Sprintf("dir/file-number-%04d.bin", i), Size: int64(i)})
	}
	id := e.seed("cap", 0, 1, files)
	c := e.signedIn()
	res := c.get(fmt.Sprintf("/torrents/%d/files/tree", id))
	if res.StatusCode != 422 || c.problem().Type != "urn:magnetico:problem:full-tree-unavailable" || strings.Contains(c.body, `"dirs"`) {
		t.Fatalf("%d %.200s", res.StatusCode, c.body)
	}
	if _, code := c.dir(id, "dir=0&limit=50"); code != 200 {
		t.Fatalf("pagination: %d", code)
	}
}

func TestIndexComplexityEverywhereButDetails(t *testing.T) {
	e := newEnv(t)
	deep := e.seed("deep", 0, 1, []codec.File{{Path: strings.Repeat("d/", 300) + "f", Size: 1}, {Path: "other", Size: 2}})
	ok := e.seed("ok256", 0, 1, []codec.File{{Path: strings.Repeat("d/", 256) + "f", Size: 1}, {Path: "other", Size: 2}})
	c := e.signedIn()
	for _, p := range []string{"/files/tree", "/files", "/files?dir=0_0"} {
		res := c.get(fmt.Sprintf("/torrents/%d%s", deep, p))
		if res.StatusCode != 422 || c.problem().Type != "urn:magnetico:problem:index-too-complex" {
			t.Errorf("%s: %d %s", p, res.StatusCode, c.body)
		}
	}
	if res := c.get(fmt.Sprintf("/torrents/%d", deep)); res.StatusCode != 200 {
		t.Fatalf("details must still render: %d", res.StatusCode)
	}
	if res := c.get("/torrents?q=deep"); res.StatusCode != 200 || !strings.Contains(c.body, "deep") {
		t.Fatalf("search: %d", res.StatusCode)
	}
	// The deepest allowed hierarchy navigates with a short positional path.
	path := strings.TrimSuffix(strings.Repeat("0_", 256), "_")
	if _, code := c.dir(ok, "dir="+path); code != 200 {
		t.Fatalf("256-deep directory: %d", code)
	}
	if len(path) > 600 {
		t.Fatalf("positional path is %d bytes", len(path))
	}
	if strings.Contains(e.log.String(), "level=ERROR") {
		t.Errorf("a complexity rejection must not log as an error:\n%s", e.log.String())
	}
}

func TestHundredNavigationsOneDecodeAndReplacement(t *testing.T) {
	e := newEnv(t)
	files := []codec.File{{Path: "d/a", Size: 1}, {Path: "d/b", Size: 2}, {Path: "e", Size: 3}}
	id := e.seed("nav", 0, 1, files)
	c := e.signedIn()
	for i := 0; i < 100; i++ {
		q := []string{"", "dir=0"}[i%2]
		if _, code := c.dir(id, q); code != 200 {
			t.Fatal(code)
		}
	}
	if s := e.cache.Stats(); s.Builds != 1 || s.Hits != 99 {
		t.Fatalf("stats %+v", s)
	}
	nb, _, _ := codec.Encode([]codec.File{{Path: "newfile", Size: 6}}, codec.ProfileCompact, codec.DefaultLimits())
	e.exec(`UPDATE magneticod.torrent_file_blobs SET data=$2 WHERE torrent_id=$1`, id, nb)
	e.exec(`UPDATE magneticod.torrents SET file_count=1, total_size=6 WHERE id=$1`, id)
	p, _ := c.dir(id, "")
	if len(p.Entries) != 1 || p.Entries[0].jname.bytes(t) != "newfile" {
		t.Fatalf("stale index served: %+v", p)
	}
	if s := e.cache.Stats(); s.Builds != 2 || s.Replaced != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestBlobFailuresAreTypedProblemsWithSafeLogs(t *testing.T) {
	e := newEnv(t)
	good := []codec.File{{Path: "SECRETPATH/x", Size: 1}, {Path: "y", Size: 2}}
	missing := e.seed("SECRETNAME-missing", 3, 1, nil)
	e.exec(`UPDATE magneticod.torrents SET file_count=3 WHERE id=$1`, missing)
	garbage := e.seed("SECRETNAME-garbage", 0, 1, good)
	e.exec(`UPDATE magneticod.torrent_file_blobs SET data='\x0102030405' WHERE torrent_id=$1`, garbage)
	mismatch := e.seed("SECRETNAME-mismatch", 0, 1, good)
	e.exec(`UPDATE magneticod.torrents SET file_count=5 WHERE id=$1`, mismatch)
	oversized := e.seed("SECRETNAME-oversized", 0, 1, good)
	e.exec(`UPDATE magneticod.torrent_file_blobs SET data = decode(repeat('00', 33*1024*1024), 'hex') WHERE torrent_id=$1`, oversized)

	c := e.signedIn()
	for name, id := range map[string]int32{"missing": missing, "garbage": garbage, "mismatch": mismatch, "oversized": oversized} {
		for _, p := range []string{"/files", "/files/tree"} {
			res := c.get(fmt.Sprintf("/torrents/%d%s", id, p))
			if res.StatusCode != 500 && !(p == "/files/tree" && res.StatusCode == 422) {
				t.Errorf("%s%s: %d, want 500", name, p, res.StatusCode)
			}
			if strings.Contains(c.body, `"entries"`) || strings.Contains(c.body, `"dirs"`) {
				t.Errorf("%s%s: a partial list was sent", name, p)
			}
			if pr := c.problem(); strings.Contains(pr.Detail+pr.Title, "SECRET") {
				t.Errorf("%s%s: problem leaks names", name, p)
			}
		}
	}
	if res := c.get(fmt.Sprintf("/torrents/%d", garbage)); res.StatusCode != 200 {
		t.Fatalf("details of a torrent with a corrupt blob: %d", res.StatusCode)
	}
	logs := e.log.String()
	for _, forbidden := range []string{"SECRETNAME", "SECRETPATH"} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("logs leak %q", forbidden)
		}
	}
	for _, want := range []string{"category=corrupt_blob", "category=missing_blob"} {
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
	c.get(fmt.Sprintf("/torrents/%d/files/tree", id))
	c.get(fmt.Sprintf("/torrents/%d/files?dir=0", id))
	c.get(fmt.Sprintf("/torrents/%d/files?dir=PRIVATEMISSING", id))
	c.get("/torrents?q=PRIVATETERM&cursor=PRIVATECURSOR")
	logs := e.log.String()
	for _, f := range []string{"PRIVATE", "private", "correct horse"} {
		if strings.Contains(logs, f) {
			t.Errorf("logs contain %q:\n%s", f, logs)
		}
	}
	for _, want := range []string{"msg=search", "msg=\"file view\""} {
		if !strings.Contains(logs, want) {
			t.Errorf("structured record %s is missing", want)
		}
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
	for _, p := range []string{"/files", "/files/tree"} {
		res := c.get(fmt.Sprintf("/torrents/%d%s", id, p))
		if res.StatusCode != 503 || res.Header.Get("Retry-After") != "5" || c.problem().Type != "urn:magnetico:problem:temporarily-unavailable" {
			t.Fatalf("saturated decode %s: %d ra=%q", p, res.StatusCode, res.Header.Get("Retry-After"))
		}
	}
	if r := c.get(fmt.Sprintf("/torrents/%d", id)); r.StatusCode != 200 {
		t.Fatalf("details during decode saturation: %d", r.StatusCode)
	}
	hold()
	if _, code := c.dir(id, ""); code != 200 {
		t.Fatalf("after release: %d", code)
	}
	_ = filecache.DecodeCapacity
}

func TestEncodeSaturationIsBusyAndRecovers(t *testing.T) {
	e := newEnv(t, func(o *envOpts) {
		o.view.EncodeSlots = 1
		o.view.MaxWaiters = 1
		o.view.QueueWait = 50 * time.Millisecond
	})
	id := e.seed("enc", 0, 1, []codec.File{{Path: "a/b", Size: 1}, {Path: "c", Size: 1}})
	c := e.signedIn()
	tor, err := e.reader.TorrentByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := e.view.Tree(context.Background(), tor) // occupies the only slot until released
	if err != nil {
		t.Fatal(err)
	}
	res := c.get(fmt.Sprintf("/torrents/%d/files/tree", id))
	if res.StatusCode != 503 || res.Header.Get("Retry-After") != "5" {
		t.Fatalf("saturated encoder: %d", res.StatusCode)
	}
	hold.Release()
	if _, code := c.tree(id); code != 200 {
		t.Fatalf("after release: %d", code)
	}
}

func TestNoPublicAPIv2RoutesYet(t *testing.T) {
	e := newEnv(t)
	for _, r := range e.srv.Router().Routes() {
		if strings.Contains(r.Pattern, "/api/") {
			t.Errorf("API route %s must not exist before W7's bearer authentication", r.Pattern)
		}
	}
	c := e.signedIn()
	id := e.seed("x", 5, 1, nil)
	for _, p := range []string{fmt.Sprintf("/api/v2/torrents/%d", id), fmt.Sprintf("/api/v2/torrents/%d/files", id), fmt.Sprintf("/api/v2/torrents/%d/files/tree", id)} {
		if res := c.get(p); res.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", p, res.StatusCode)
		}
	}
}

func TestSearchPageUiMarkupSortingAndMagnets(t *testing.T) {
	e := newEnv(t)
	small := e.seed("alpha <b>release</b>", 100, 3, nil)
	big := e.seed("alpha huge release", 5000, 2, nil)
	e.seed("alpha middle release", 900, 1, nil)
	c := e.signedIn()

	c.get("/torrents?q=alpha")
	for _, want := range []string{
		`aria-sort="descending"`, `class="table-sort" href="/torrents?order=asc&amp;q=alpha&amp;size=50&amp;sort=discovered"`,
		`href="/torrents?order=desc&amp;q=alpha&amp;size=50&amp;sort=size"`, `href="/torrents?order=asc&amp;q=alpha&amp;size=50&amp;sort=name"`,
		`class="results-mobile"`, `class="table-shell desktop-table"`, `<caption class="sr-only">`, `name="by"`, `value="size:desc"`,
		"alpha &lt;b&gt;release&lt;/b&gt;", `href="magnet:?xt=urn:btih:`, `class="skip-link"`, "<noscript>",
	} {
		if !strings.Contains(c.body, want) {
			t.Errorf("search page lacks %q", want)
		}
	}
	if strings.Contains(c.body, "<b>release") || strings.Contains(c.body, "&tr=") {
		t.Error("names must be escaped and magnets tracker-less")
	}
	if ids := c.rowIDs(); len(ids) != 3 || ids[0] != small {
		t.Fatalf("default order (newest first): %v", ids)
	}
	// The mobile "by" selector is a plain GET parameter.
	c.get("/torrents?q=alpha&by=size:desc")
	if ids := c.rowIDs(); ids[0] != big || !strings.Contains(c.body, `aria-sort="descending"`) {
		t.Fatalf("by=size:desc: %v", ids)
	}
	c.get("/torrents?q=alpha&by=size:asc")
	if ids := c.rowIDs(); ids[0] != small || !strings.Contains(c.body, `aria-sort="ascending"`) {
		t.Fatalf("by=size:asc: %v", ids)
	}
	// Name sorting needs a query; without one the header is plain text.
	c.get("/torrents")
	if strings.Contains(c.body, "sort=name") {
		t.Error("name sort must not be offered without a query")
	}
	// An info-hash lookup offers no sorting at all.
	c.get("/torrents?q=" + strings.Repeat("a", 40))
	if strings.Contains(c.body, `class="table-sort"`) {
		t.Error("a lookup must not offer sorting")
	}
	if res := c.get("/torrents?by=garbage"); res.StatusCode != 200 {
		t.Errorf("malformed by must be ignored: %d", res.StatusCode)
	}
}
