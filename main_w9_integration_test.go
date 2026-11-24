package main

// W9 cross-service integration (task 07J, stage S3): the real `magnetico`
// binaries on one disposable PostgreSQL database, driven only through their
// public interfaces. Synthetic data only; loopback only; no DHT, no BitTorrent,
// no tracker traffic.
//
// Every service and every CLI command in this file connects with ONE
// PostgreSQL account, the way the default deployment does (docs/web-deployment.md
// §4): a non-superuser that owns the schemas it created.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/devseed"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
	"github.com/skobkin/magnetico-web/internal/oastest"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/web/apidocs"
	"github.com/skobkin/magnetico-web/persistence"
)

const w9Password = "w9-integration-password-1"

// w9AppAccount creates a database and a non-superuser role that may create
// schemas in it, and returns the role's DSN.
func w9AppAccount(t *testing.T) string {
	t.Helper()
	cl := pgtest.IntegrationCluster(t)
	dsn := cl.TestDatabase(t)
	u, _ := url.Parse(dsn)
	dbName := strings.TrimPrefix(u.Path, "/")
	role := "w9app_" + pgtest.RandomSuffix()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, cl.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgtest.QuoteIdent(role)+" LOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), cl.AdminDSN)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgtest.QuoteIdent(dbName)+" WITH (FORCE)")
		_, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgtest.QuoteIdent(role))
	})
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	for _, q := range []string{
		"GRANT CONNECT, CREATE ON DATABASE " + pgtest.QuoteIdent(dbName) + " TO " + pgtest.QuoteIdent(role),
		"GRANT USAGE, CREATE ON SCHEMA public TO " + pgtest.QuoteIdent(role),
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ru := *u
	ru.User = url.UserPassword(role, "w9-role-secret-pw")
	return ru.String()
}

// w9Proc is a running binary with captured output.
type w9Proc struct {
	cmd    *exec.Cmd
	out    *bytes.Buffer
	exited chan error
}

func w9Start(t *testing.T, bin string, env []string, args ...string) *w9Proc {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = webEnv(env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &w9Proc{cmd: cmd, out: &out, exited: make(chan error, 1)}
	go func() { p.exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return p
}

// terminate sends SIGTERM and returns the exit code (-1 on timeout).
func (p *w9Proc) terminate(t *testing.T, within time.Duration) int {
	t.Helper()
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-p.exited:
		if err == nil {
			return 0
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return -2
	case <-time.After(within):
		return -1
	}
}

// w9World is a database with all three schemas, an ingest service and a web
// service, all connected with the same account.
type w9World struct {
	t           *testing.T
	bin         string
	dsn         string
	ingestAddr  string
	ingestToken string
	ingest      *w9Proc
	web         *runningWeb
	alice       *webBrowser
}

func newW9World(t *testing.T, webEnvExtra ...string) *w9World {
	t.Helper()
	return newW9WorldAfterSchema(t, nil, webEnvExtra...)
}

// newW9WorldAfterSchema is newW9World with a hook that runs after the torrent
// schema exists and before the web and ingest schemas are created: the point at
// which `migrate torrents` runs in a real cutover.
func newW9WorldAfterSchema(t *testing.T, afterSchema func(w *w9World), webEnvExtra ...string) *w9World {
	t.Helper()
	w := &w9World{t: t, bin: buildBinary(t), dsn: w9AppAccount(t)}
	for _, cmd := range []string{"schema-init", "web-schema-init", "ingest-schema-init"} {
		if cmd == "web-schema-init" && afterSchema != nil {
			afterSchema(w)
		}
		if out, code := runWebSchemaInit(t, w.bin, w.dsn, cmd); code != 0 {
			t.Fatalf("migrate %s: exit %d: %s", cmd, code, out)
		}
	}
	for _, c := range []struct {
		stdin string
		args  []string
	}{{"", []string{"user", "add", "alice"}}, {w9Password + "\n", []string{"user", "set-password", "alice"}}} {
		if out, errOut, code := runWebAdminBinary(t, w.bin, w.dsn, c.stdin, nil, c.args...); code != 0 {
			t.Fatalf("web %v: %d %s%s", c.args, code, out, errOut)
		}
	}
	tok := make([]byte, 24)
	_, _ = rand.Read(tok)
	w.ingestToken = hex.EncodeToString(tok)
	w.ingestAddr = freeLoopbackAddr(t)
	w.ingest = w9Start(t, w.bin, []string{
		"MAGNETICO_INGEST_TOKEN=" + w.ingestToken, "MAGNETICO_INGEST_DATABASE=" + w.dsn, "MAGNETICO_INGEST_LISTEN=" + w.ingestAddr,
	}, "ingest")
	w.waitIngest()
	w.web = startWeb(t, w.bin, w.dsn, "http", webEnvExtra...)
	w.alice = newBrowser(t, w.web.addr)
	if resp := w.alice.login("alice", w9Password); resp.StatusCode != 303 {
		t.Fatalf("login: %d\n%s", resp.StatusCode, w.alice.body)
	}
	return w
}

func (w *w9World) waitIngest() {
	w.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-w.ingest.exited:
			w.t.Fatalf("ingest exited early: %v\n%s", err, w.ingest.out.String())
		default:
		}
		req, _ := http.NewRequest("POST", "http://"+w.ingestAddr+"/api/ingest/v1/torrents/exists", strings.NewReader(`{"info_hashes":[]}`))
		req.Header.Set("Authorization", "Bearer "+w.ingestToken)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	w.t.Fatalf("ingest not ready:\n%s", w.ingest.out.String())
}

// submit posts the records through the real ingest HTTP interface and returns
// the status code.
func (w *w9World) submit(recs ...record.Record) int {
	w.t.Helper()
	var raws []json.RawMessage
	for _, r := range recs {
		b, err := record.Encode(r, codec.DefaultLimits())
		if err != nil {
			w.t.Fatal(err)
		}
		raws = append(raws, b)
	}
	body, _ := json.Marshal(map[string]any{"records": raws})
	req, _ := http.NewRequest("POST", "http://"+w.ingestAddr+"/api/ingest/v1/torrents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+w.ingestToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// waitStored waits until n torrents are durable in magneticod.torrents and the
// ingest queue is empty.
func (w *w9World) waitStored(n int) {
	w.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if sqlCount(w.t, w.dsn, `SELECT count(*) FROM magneticod.torrents`) >= n &&
			sqlCount(w.t, w.dsn, `SELECT count(*) FROM magnetico_ingest.queue`) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	w.t.Fatalf("not stored in time: torrents=%d queue=%d\ningest:\n%s",
		sqlCount(w.t, w.dsn, `SELECT count(*) FROM magneticod.torrents`),
		sqlCount(w.t, w.dsn, `SELECT count(*) FROM magnetico_ingest.queue`), w.ingest.out.String())
}

// mintToken creates an API or RSS token through the account page (the only way
// a user gets one) and returns its plaintext.
func (w *w9World) mintToken(kind string) string {
	w.t.Helper()
	tab, path := "api", "/account/api-tokens"
	if kind == "rss" {
		tab, path = "rss", "/account/rss-tokens"
	}
	w.alice.get("/account?tab=" + tab)
	resp := w.alice.post(path, url.Values{"label": {"w9"}, "csrf_token": {w.alice.token()}})
	if resp.StatusCode != 200 {
		w.t.Fatalf("mint %s: %d\n%s", kind, resp.StatusCode, w.alice.body)
	}
	m := regexp.MustCompile(`<code>(m[gt][tf]_[A-Za-z0-9_-]+)</code>`).FindStringSubmatch(w.alice.body)
	if m == nil {
		w.t.Fatalf("no %s token revealed:\n%s", kind, w.alice.body)
	}
	return m[1]
}

// rotatingAPI mints n API tokens and returns a getter that cycles through them,
// so a scenario with hundreds of requests stays below the per-token limit (120
// burst, 2 per second) the way a real client population does. Token creation
// itself is limited to a burst of 10 per account.
func (w *w9World) rotatingAPI(n int) func(string) (int, []byte) {
	w.t.Helper()
	toks := make([]string, n)
	for i := range toks {
		toks[i] = w.mintToken("api")
	}
	i := 0
	return func(p string) (int, []byte) {
		// A well-behaved client: a 429 is answered by waiting Retry-After.
		for attempt := 0; ; attempt++ {
			tok := toks[i%n]
			i++
			st, hdr, b := w.bearer(tok, "/api/v2"+p)
			if st != http.StatusTooManyRequests || attempt >= 30 {
				return st, b
			}
			wait := time.Second
			if sec, err := strconv.Atoi(hdr.Get("Retry-After")); err == nil && sec > 0 && sec <= 10 {
				wait = time.Duration(sec) * time.Second
			}
			time.Sleep(wait)
		}
	}
}

// bearer GETs a path with an API token and returns status, headers and body.
func (w *w9World) bearer(token, path string) (int, http.Header, []byte) {
	w.t.Helper()
	req, _ := http.NewRequest("GET", "http://"+w.web.addr+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// --- synthetic records ---

func w9Hash(name string) [20]byte { return sha1.Sum([]byte("w9/" + name)) }

func w9Record(name string, files []persistence.File) record.Record {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	return record.Record{InfoHash: w9Hash(name), Name: name, TotalSize: total, DiscoveredOn: 1700000000 + int64(len(name)), Files: files}
}

// The three shapes the scenario submits. Every name carries "w9needle".
func w9Records() (single, hostile, large record.Record) {
	sname := "w9needle single <img src=x onerror=alert(1)> \u202e.exe"
	single = w9Record(sname, []persistence.File{{Path: sname, Size: 4242}})

	hname := "w9needle \"><script>window.__pwn=1</script> multi & 'q' \u202ebidi"
	hostile = w9Record(hname, []persistence.File{
		{Path: "a/<b>.txt", Size: 1}, {Path: "a/b/../c", Size: 2}, {Path: "./x", Size: 3}, {Path: "../y", Size: 4},
		{Path: "dup", Size: 5}, {Path: "dup", Size: 5}, {Path: "dup", Size: 6},
		{Path: "dir/file", Size: 7}, {Path: "dir", Size: 8},
		{Path: "é/日本語.txt", Size: 9}, {Path: "\u202eevil/cod.exe", Size: 10},
		{Path: "long" + strings.Repeat("x", 200) + "/f", Size: 11},
		{Path: "/leading", Size: 12}, {Path: "trail/", Size: 13}, {Path: "dbl//slash", Size: 14},
		{Path: "q?x=1&y=2#frag/%2e%2e/z", Size: 15}, {Path: "t\tab/nl", Size: 16},
	})

	lname := "w9needle large archive"
	var lf []persistence.File
	for d := 0; d < 41; d++ {
		for f := 0; f < 100; f++ {
			lf = append(lf, persistence.File{Path: fmt.Sprintf("d%02d/f%03d.bin", d, f), Size: int64(d*100 + f + 1)})
		}
	}
	large = w9Record(lname, lf)
	return
}

func w9Key(f persistence.File) string { return fmt.Sprintf("%d|%s", f.Size, f.Path) }

func w9Keys(files []persistence.File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = w9Key(f)
	}
	sort.Strings(out)
	return out
}

// --- JSON file views ---

type w9Entry struct {
	Type      string  `json:"type"`
	Name      *string `json:"name"`
	NameB64   string  `json:"name_base64"`
	Path      string  `json:"path"`
	Size      int64   `json:"size"`
	FileIndex int     `json:"file_index"`
}

type w9DirPage struct {
	Entries    []w9Entry `json:"entries"`
	NextOffset *int      `json:"next_offset"`
}

type w9TreeNode struct {
	Dirs []struct {
		Name *string `json:"name"`
		w9TreeNode
	} `json:"dirs"`
	Files []struct {
		Name *string `json:"name"`
		Size int64   `json:"size"`
	} `json:"files"`
}

func (n w9TreeNode) flatten(prefix string, out *[]string) {
	for _, f := range n.Files {
		*out = append(*out, fmt.Sprintf("%d|%s%s", f.Size, prefix, *f.Name))
	}
	for _, d := range n.Dirs {
		d.w9TreeNode.flatten(prefix+*d.Name+"/", out)
	}
}

// treeKeys fetches the complete tree through get and flattens it.
func w9TreeKeys(t *testing.T, get func(string) (int, []byte), id int) ([]string, []byte) {
	t.Helper()
	status, body := get(fmt.Sprintf("/torrents/%d/files/tree", id))
	if status != 200 {
		t.Fatalf("tree of %d: %d %s", id, status, body)
	}
	var doc struct {
		Root w9TreeNode `json:"root"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var keys []string
	doc.Root.flatten("", &keys)
	sort.Strings(keys)
	return keys, body
}

// dirKeys walks every directory page of a torrent.
func w9DirKeys(t *testing.T, get func(string) (int, []byte), id int) ([]string, [][]byte) {
	t.Helper()
	var keys []string
	var pages [][]byte
	var walk func(dir, prefix string)
	walk = func(dir, prefix string) {
		offset := 0
		for {
			status, body := get(fmt.Sprintf("/torrents/%d/files?dir=%s&offset=%d&limit=1000", id, dir, offset))
			if status != 200 {
				t.Fatalf("dir %q of %d: %d %s", dir, id, status, body)
			}
			pages = append(pages, body)
			var pg w9DirPage
			if err := json.Unmarshal(body, &pg); err != nil {
				t.Fatal(err)
			}
			for _, e := range pg.Entries {
				if e.Name == nil {
					t.Fatalf("entry without a valid name in %q", dir)
				}
				if e.Type == "dir" {
					walk(e.Path, prefix+*e.Name+"/")
				} else {
					keys = append(keys, fmt.Sprintf("%d|%s%s", e.Size, prefix, *e.Name))
				}
			}
			if pg.NextOffset == nil {
				return
			}
			offset = *pg.NextOffset
		}
	}
	walk("", "")
	sort.Strings(keys)
	return keys, pages
}

// torrentIDByHash reads the stored id.
func (w *w9World) torrentID(h [20]byte) int {
	w.t.Helper()
	return sqlCount(w.t, w.dsn, `SELECT id FROM magneticod.torrents WHERE info_hash = $1`, h[:])
}

type w9RSS struct {
	Channel struct {
		Items []struct {
			GUID  string `xml:"guid"`
			Title string `xml:"title"`
			Link  string `xml:"link"`
		} `xml:"item"`
	} `xml:"channel"`
}

func w9Equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d entries, want %d\n got: %q\nwant: %q", what, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: entry %d = %q, want %q", what, i, got[i], want[i])
		}
	}
}

// Records submitted through the real ingest HTTP interface become visible,
// byte-faithful and safely rendered, through the web UI, API v2 and both RSS
// feeds, all served by services that share one non-superuser account.
func TestW9IngestToWebAndAPI(t *testing.T) {
	w := newW9World(t)
	single, hostile, large := w9Records()

	// Existence check before: all three unknown to the store.
	if code := w.submit(single, hostile, large); code != 202 {
		t.Fatalf("submit: %d\ningest:\n%s", code, w.ingest.out.String())
	}
	w.waitStored(3)
	hsID, hoID, laID := w.torrentID(single.InfoHash), w.torrentID(hostile.InfoHash), w.torrentID(large.InfoHash)

	// --- UI: search and details, escaped in HTML.
	if resp := w.alice.get("/torrents?q=w9needle"); resp.StatusCode != 200 {
		t.Fatalf("search: %d", resp.StatusCode)
	}
	for _, want := range []string{"&lt;script&gt;window.__pwn=1&lt;/script&gt;", "&lt;img src=x onerror=alert(1)&gt;"} {
		if !strings.Contains(w.alice.body, want) {
			t.Errorf("search page lacks the escaped form %q", want)
		}
	}
	for _, bad := range []string{"<script>window.__pwn=1", "<img src=x onerror"} {
		if strings.Contains(w.alice.body, bad) {
			t.Fatalf("search page contains raw %q", bad)
		}
	}
	for _, id := range []int{hsID, hoID, laID} {
		if resp := w.alice.get(fmt.Sprintf("/torrents/%d", id)); resp.StatusCode != 200 {
			t.Fatalf("details %d: %d", id, resp.StatusCode)
		}
		if strings.Contains(w.alice.body, "<script>window.__pwn=1") || strings.Contains(w.alice.body, "<img src=x onerror") {
			t.Fatalf("details %d contains raw markup", id)
		}
	}
	// The two ordinary torrents use the complete tree, the 4,100-file one the
	// paginated mode (the backend decides, the page carries it).
	for id, mode := range map[int]string{hoID: "tree", laID: "dirs"} {
		w.alice.get(fmt.Sprintf("/torrents/%d", id))
		if !strings.Contains(w.alice.body, `data-mode="`+mode+`"`) {
			t.Errorf("torrent %d: expected data-mode=%q", id, mode)
		}
	}

	// --- file lists equal what was submitted, through the UI JSON endpoints.
	uiGet := func(p string) (int, []byte) {
		resp := w.alice.get(p)
		return resp.StatusCode, []byte(w.alice.body)
	}
	wantHostile := w9Keys(hostile.Files)
	uiTree, uiTreeBody := w9TreeKeys(t, uiGet, hoID)
	w9Equal(t, "UI tree of the hostile torrent", uiTree, wantHostile)
	uiDirs, uiPages := w9DirKeys(t, uiGet, hoID)
	w9Equal(t, "UI directory pages of the hostile torrent", uiDirs, wantHostile)
	uiLarge, uiLargePages := w9DirKeys(t, uiGet, laID)
	w9Equal(t, "UI directory pages of the large torrent", uiLarge, w9Keys(large.Files))
	if status, _ := uiGet(fmt.Sprintf("/torrents/%d/files/tree", laID)); status != 422 {
		t.Fatalf("tree of the large torrent: %d, want 422", status)
	}

	// --- API v2 with a bearer token: same data, same bytes as the UI.
	api := w.mintToken("api")
	apiGet := func(p string) (int, []byte) { st, _, b := w.bearer(api, "/api/v2"+p); return st, b }
	apiTree, apiTreeBody := w9TreeKeys(t, apiGet, hoID)
	w9Equal(t, "API tree", apiTree, wantHostile)
	if !bytes.Equal(uiTreeBody, apiTreeBody) {
		t.Fatal("UI and API tree documents differ")
	}
	apiDirs, apiPages := w9DirKeys(t, apiGet, hoID)
	w9Equal(t, "API directory pages", apiDirs, wantHostile)
	if len(apiPages) != len(uiPages) {
		t.Fatalf("page counts differ: %d vs %d", len(apiPages), len(uiPages))
	}
	for i := range apiPages {
		if !bytes.Equal(apiPages[i], uiPages[i]) {
			t.Fatalf("UI and API directory page %d differ", i)
		}
	}
	apiLarge, apiLargePages := w9DirKeys(t, apiGet, laID)
	w9Equal(t, "API large torrent", apiLarge, w9Keys(large.Files))
	for i := range apiLargePages {
		if !bytes.Equal(apiLargePages[i], uiLargePages[i]) {
			t.Fatalf("UI and API directory page %d of the large torrent differ", i)
		}
	}
	if st, body := apiGet(fmt.Sprintf("/torrents/%d/files/tree", laID)); st != 422 || !bytes.Contains(body, []byte("full-tree-unavailable")) {
		t.Fatalf("API tree of the large torrent: %d %s", st, body)
	}
	// Details and search.
	for id, wantTree := range map[int]bool{hsID: true, hoID: true, laID: false} {
		st, body := apiGet(fmt.Sprintf("/torrents/%d", id))
		var d struct {
			FullTreeAvailable bool   `json:"full_tree_available"`
			MagnetURI         string `json:"magnet_uri"`
			FileCount         int    `json:"file_count"`
		}
		if st != 200 || json.Unmarshal(body, &d) != nil || d.FullTreeAvailable != wantTree || !strings.HasPrefix(d.MagnetURI, "magnet:?xt=urn:btih:") {
			t.Fatalf("API details %d: %d %s", id, st, body)
		}
	}
	st, body := apiGet("/torrents?q=w9needle&limit=100")
	var list struct {
		Items []struct {
			Name     string `json:"name"`
			InfoHash string `json:"info_hash"`
		} `json:"items"`
	}
	if st != 200 || json.Unmarshal(body, &list) != nil || len(list.Items) != 3 {
		t.Fatalf("API search: %d %s", st, body)
	}
	names := map[string]bool{}
	for _, it := range list.Items {
		names[it.Name] = true
	}
	for _, r := range []record.Record{single, hostile, large} {
		if !names[r.Name] {
			t.Errorf("API search lacks %q", r.Name)
		}
	}
	if st, body := apiGet("/torrents?info_hash=" + hex.EncodeToString(hostile.InfoHash[:])); st != 200 || !bytes.Contains(body, []byte(`"items":[{`)) {
		t.Fatalf("API info-hash lookup: %d %s", st, body)
	}

	// --- RSS v2 (feed_token) and v1 (api-token) list all three, with GUID = hash.
	rssTok := w.mintToken("rss")
	for _, feed := range []string{"/api/v2/feeds/latest.rss?feed_token=" + rssTok, "/api/v1/feed/rss/last?api-token=" + rssTok} {
		st, hdr, body := w.bearer("", feed)
		if st != 200 || !strings.Contains(hdr.Get("Content-Type"), "rss") {
			t.Fatalf("%s: %d %q", strings.SplitN(feed, "?", 2)[0], st, hdr.Get("Content-Type"))
		}
		var doc w9RSS
		if err := xml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("%s is not valid XML: %v", feed, err)
		}
		guids := map[string]bool{}
		for _, it := range doc.Channel.Items {
			guids[it.GUID] = true
		}
		for _, r := range []record.Record{single, hostile, large} {
			if !guids[hex.EncodeToString(r.InfoHash[:])] {
				t.Errorf("%s lacks %x", strings.SplitN(feed, "?", 2)[0], r.InfoHash)
			}
		}
		if bytes.Contains(body, []byte(rssTok)) {
			t.Errorf("%s reflects the credential", strings.SplitN(feed, "?", 2)[0])
		}
	}
	// The magnet short link is public and redirects without a lookup.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get("http://" + w.web.addr + "/magnet/" + hex.EncodeToString(hostile.InfoHash[:]))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 307 || !strings.HasPrefix(resp.Header.Get("Location"), "magnet:?xt=urn:btih:"+hex.EncodeToString(hostile.InfoHash[:])) {
		t.Fatalf("magnet: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// --- A resubmission is idempotent and the existence check now reports them.
	if code := w.submit(single); code != 202 {
		t.Fatalf("resubmit: %d", code)
	}
	w.waitStored(3)
	if n := sqlCount(t, w.dsn, `SELECT count(*) FROM magneticod.torrents`); n != 3 {
		t.Fatalf("torrents after resubmission: %d", n)
	}
	if sqlCount(t, w.dsn, `SELECT count(*) FROM magnetico_ingest.quarantine`) != 0 {
		t.Fatal("quarantine is not empty")
	}

	// --- Both services stop cleanly on SIGTERM.
	w.web.stop(t)
	if code := w.ingest.terminate(t, 30*time.Second); code != 0 {
		t.Fatalf("ingest exit after SIGTERM: %d\n%s", code, w.ingest.out.String())
	}

	// Nothing sensitive reached the output of either service: not the ingest
	// bearer token, the database password, user credentials, or the tokens that
	// were used against the web service.
	for name, out := range map[string]string{"web": w.web.out.String(), "ingest": w.ingest.out.String()} {
		for what, secret := range map[string]string{
			"ingest token": w.ingestToken, "database password": "w9-role-secret-pw", "user password": w9Password,
			"API token": api, "RSS token": rssTok,
		} {
			if strings.Contains(out, secret) {
				t.Errorf("the %s output contains the %s", name, what)
			}
		}
		if len(out) < 200 {
			t.Errorf("the %s output is suspiciously small (%d bytes): the scan would be vacuous", name, len(out))
		}
	}
}

// A legacy v0.12.0 source is imported with `migrate torrents`, verified with
// both verifications, and then served by the web service and API, with the
// legacy ids and every file list (duplicates included) intact. Ingest then adds
// a torrent next to the migrated ones, on the same account.
func TestW9TorrentMigrationToWeb(t *testing.T) {
	src := pgtest.IntegrationCluster(t).TestDatabase(t)
	execSmokeSQL(t, src, smokeSourceDDL)
	execSmokeSQL(t, src, `
        INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on) VALUES
          (11, decode(lpad(to_hex(11), 40, '0'), 'hex'), 'legacy single', 100, 1600000001),
          (12, decode(lpad(to_hex(12), 40, '0'), 'hex'), 'legacy multi <b>x</b>', 61, 1600000002),
          (17, decode(lpad(to_hex(17), 40, '0'), 'hex'), 'legacy gap id', 7, 1600000003);
        INSERT INTO magneticod.files (torrent_id, path, size) VALUES
          (11, 'legacy single', 100),
          (12, 'a/b.txt', 10), (12, 'a/b.txt', 10), (12, 'a/b.txt', 11), (12, 'c', 20), (12, '../d', 10),
          (17, 'only/one.bin', 7);`)
	wantFiles := map[int][]persistence.File{
		12: {{Path: "a/b.txt", Size: 10}, {Path: "a/b.txt", Size: 10}, {Path: "a/b.txt", Size: 11}, {Path: "c", Size: 20}, {Path: "../d", Size: 10}},
		17: {{Path: "only/one.bin", Size: 7}},
	}

	w := newW9WorldAfterSchema(t, func(w *w9World) {
		t.Helper()
		migrate := func(args ...string) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, w.bin, append([]string{"migrate"}, args...)...)
			cmd.Env = webEnv("MAGNETICO_MIGRATION_SOURCE_DSN="+src, "MAGNETICO_MIGRATE_DATABASE="+w.dsn)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("migrate %v: %v\n%s", args, err, out)
			}
		}
		migrate("torrents", "--batch-size", "10")
		migrate("verify-decode")
		migrate("verify-content")
	})

	if n := sqlCount(t, w.dsn, `SELECT count(*) FROM magneticod.torrents`); n != 3 {
		t.Fatalf("migrated torrents: %d", n)
	}
	api := w.mintToken("api")
	apiGet := func(p string) (int, []byte) { st, _, b := w.bearer(api, "/api/v2"+p); return st, b }
	// The legacy ids are kept, gaps included.
	for _, id := range []int{11, 12, 17} {
		if st, body := apiGet(fmt.Sprintf("/torrents/%d", id)); st != 200 {
			t.Fatalf("migrated torrent %d: %d %s", id, st, body)
		}
	}
	if st, _ := apiGet("/torrents/13"); st != 404 {
		t.Fatalf("an id that never existed: %d", st)
	}
	for id, files := range wantFiles {
		tree, _ := w9TreeKeys(t, apiGet, id)
		w9Equal(t, fmt.Sprintf("migrated tree %d", id), tree, w9Keys(files))
		dirs, _ := w9DirKeys(t, apiGet, id)
		w9Equal(t, fmt.Sprintf("migrated directory pages %d", id), dirs, w9Keys(files))
	}
	// Search sees them, and the markup in a legacy name is escaped.
	w.alice.get("/torrents?q=legacy")
	if !strings.Contains(w.alice.body, "legacy multi &lt;b&gt;x&lt;/b&gt;") || strings.Contains(w.alice.body, "<b>x</b>") {
		t.Fatalf("migrated name not escaped:\n%s", w.alice.body)
	}

	// Ingest adds a torrent next to the migrated ones; the imported ids stay,
	// and the new row gets a larger id.
	single, _, _ := w9Records()
	if code := w.submit(single); code != 202 {
		t.Fatalf("submit: %d", code)
	}
	w.waitStored(4)
	if id := w.torrentID(single.InfoHash); id <= 17 {
		t.Fatalf("the ingested torrent got id %d, want above the migrated ids", id)
	}
	if st, body := apiGet("/torrents?q=legacy&limit=100"); st != 200 || strings.Count(string(body), `"info_hash"`) != 3 {
		t.Fatalf("search after ingest: %d %s", st, body)
	}
}

// `dev seed-torrents` followed by the web service: all 101 fixtures are
// listed, every file list is returned exactly (hierarchy, duplicates, odd
// components), and the backend selects the documented browsing mode for each.
func TestW9DevFixturesToWeb(t *testing.T) {
	w := newW9WorldAfterSchema(t, func(w *w9World) {
		cmd := exec.Command(w.bin, "dev", "seed-torrents", "--confirm-development-database")
		cmd.Env = webEnv("MAGNETICO_DEV_DATABASE=" + w.dsn)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("dev seed-torrents: %v\n%s", err, out)
		}
	})
	fixtures := devseed.Dataset()
	if len(fixtures) != 101 {
		t.Fatalf("dataset has %d fixtures", len(fixtures))
	}
	apiGet := w.rotatingAPI(10)

	// Listing: every fixture, through the keyset cursor.
	type item struct {
		ID        int    `json:"id"`
		InfoHash  string `json:"info_hash"`
		Name      string `json:"name"`
		FileCount int    `json:"file_count"`
	}
	listed := map[string]item{}
	cursor := ""
	for page := 0; page < 10; page++ {
		path := "/torrents?limit=100&sort=discovered&order=desc"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		st, body := apiGet(path)
		var doc struct {
			Items      []item  `json:"items"`
			NextCursor *string `json:"next_cursor"`
		}
		if st != 200 || json.Unmarshal(body, &doc) != nil {
			t.Fatalf("list page %d: %d %s", page, st, body)
		}
		for _, it := range doc.Items {
			if _, dup := listed[it.InfoHash]; dup {
				t.Fatalf("%s listed twice", it.InfoHash)
			}
			listed[it.InfoHash] = it
		}
		if doc.NextCursor == nil {
			break
		}
		cursor = *doc.NextCursor
	}
	if len(listed) != 101 {
		t.Fatalf("listed %d torrents, want 101", len(listed))
	}

	var tree, dirs, singles int
	for _, f := range fixtures {
		it, ok := listed[hex.EncodeToString(f.Torrent.InfoHash)]
		if !ok {
			t.Fatalf("fixture %s is not listed", f.Key)
		}
		if it.Name != f.Torrent.Name || it.FileCount != len(f.Files) {
			t.Fatalf("fixture %s: listed as %q with %d files", f.Key, it.Name, it.FileCount)
		}
		st, body := apiGet(fmt.Sprintf("/torrents/%d", it.ID))
		var d struct {
			FullTreeAvailable bool `json:"full_tree_available"`
		}
		if st != 200 || json.Unmarshal(body, &d) != nil {
			t.Fatalf("details %s: %d", f.Key, st)
		}
		want := w9Keys(f.Files)
		switch {
		case len(f.Files) == 1:
			singles++
		case d.FullTreeAvailable:
			tree++
			got, _ := w9TreeKeys(t, apiGet, it.ID)
			w9Equal(t, "fixture tree "+f.Key, got, want)
		default:
			dirs++
		}
		if len(f.Files) > 1 {
			got, _ := w9DirKeys(t, apiGet, it.ID)
			w9Equal(t, "fixture directory pages "+f.Key, got, want)
		}
	}
	// The documented split of docs/development-fixtures.md.
	if singles != 40 || tree != 60 || dirs != 1 {
		t.Fatalf("mode split: %d single-file, %d tree, %d paginated; documented 40 / 60 / 1", singles, tree, dirs)
	}
}

// Each service refuses to start against a database that lacks its schema, with
// a controlled message, no listener and no credential in the output.
func TestW9ServicesFailClosedWithoutTheirSchemas(t *testing.T) {
	bin := buildBinary(t)
	dsn := w9AppAccount(t)
	if out, code := runWebSchemaInit(t, bin, dsn, "schema-init"); code != 0 {
		t.Fatalf("schema-init: %d %s", code, out)
	}
	const secret = "w9-role-secret-pw"
	run := func(env []string, args ...string) (string, int) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = webEnv(env...)
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	addr := freeLoopbackAddr(t)
	out, code := run([]string{"MAGNETICO_INGEST_TOKEN=" + strings.Repeat("t", 40), "MAGNETICO_INGEST_DATABASE=" + dsn, "MAGNETICO_INGEST_LISTEN=" + addr}, "ingest")
	if code != 1 || strings.Contains(out, secret) {
		t.Fatalf("ingest without its schema: exit %d\n%s", code, out)
	}
	if conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("ingest is listening although its schema is missing")
	}
	addr = freeLoopbackAddr(t)
	out, code = run([]string{"MAGNETICO_WEB_DATABASE=" + dsn, "MAGNETICO_WEB_LISTEN=" + addr, "MAGNETICO_WEB_PUBLIC_URL=http://" + addr}, "web")
	if code != 1 || strings.Contains(out, secret) {
		t.Fatalf("web without its schema: exit %d\n%s", code, out)
	}
	if conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("web is listening although its schema is missing")
	}
}

// Both services are stopped with SIGTERM while clients keep sending requests
// and records: both exit 0 within their bounds, neither logs a panic, and the
// ingest accounting stays exact.
func TestW9GracefulShutdownUnderLoad(t *testing.T) {
	w := newW9World(t)
	single, hostile, large := w9Records()
	if code := w.submit(single, hostile, large); code != 202 {
		t.Fatalf("submit: %d", code)
	}
	w.waitStored(3)
	api := w.mintToken("api")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	hammer := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				f(i)
			}
		}()
	}
	quiet := &http.Client{Timeout: 5 * time.Second}
	get := func(path, token string) {
		req, _ := http.NewRequest("GET", "http://"+w.web.addr+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if resp, err := quiet.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	hammer(func(int) { get("/healthz", "") })
	hammer(func(int) { get("/api/v2/torrents?q=w9needle", api) })
	hammer(func(i int) {
		r := w9Record(fmt.Sprintf("w9needle shutdown %d", i), []persistence.File{{Path: "a/b", Size: int64(i + 1)}})
		b, _ := record.Encode(r, codec.DefaultLimits())
		body, _ := json.Marshal(map[string]any{"records": []json.RawMessage{b}})
		req, _ := http.NewRequest("POST", "http://"+w.ingestAddr+"/api/ingest/v1/torrents", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+w.ingestToken)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := quiet.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		time.Sleep(5 * time.Millisecond)
	})
	time.Sleep(500 * time.Millisecond)

	// SIGTERM to both at once.
	webDone := make(chan int, 1)
	go func() {
		p := &w9Proc{cmd: w.web.cmd, out: w.web.out, exited: w.web.exited}
		webDone <- p.terminate(t, 40*time.Second)
	}()
	ingestCode := w.ingest.terminate(t, 40*time.Second)
	webCode := <-webDone
	close(stop)
	wg.Wait()
	if webCode != 0 || ingestCode != 0 {
		t.Fatalf("exit codes: web %d, ingest %d\nweb:\n%s\ningest:\n%s", webCode, ingestCode, w.web.out.String(), w.ingest.out.String())
	}
	for name, out := range map[string]string{"web": w.web.out.String(), "ingest": w.ingest.out.String()} {
		if strings.Contains(out, "panic") || strings.Contains(out, "goroutine ") {
			t.Errorf("%s output shows a crash:\n%s", name, out)
		}
	}
	// The quota accounting matches what is queued or quarantined.
	if drift := sqlCount(t, w.dsn, `SELECT (SELECT used_bytes FROM magnetico_ingest.usage)
	  - (SELECT COALESCE(sum(payload_bytes),0) FROM magnetico_ingest.queue)
	  - (SELECT COALESCE(sum(payload_bytes),0) FROM magnetico_ingest.quarantine)`); drift != 0 {
		t.Fatalf("ingest usage drift after the shutdown: %d", drift)
	}
	if sqlCount(t, w.dsn, `SELECT count(*) FROM magnetico_ingest.quarantine`) != 0 {
		t.Fatal("records were quarantined by the shutdown")
	}
}

// Real responses received over TCP from the built binary match the embedded
// OpenAPI documents: every documented operation is exercised, and every
// response (success and problem) uses a documented status, media type and
// schema.
func TestW9RealBinaryResponsesMatchOpenAPI(t *testing.T) {
	w := newW9World(t)
	single, hostile, large := w9Records()
	if code := w.submit(single, hostile, large); code != 202 {
		t.Fatalf("submit: %d", code)
	}
	w.waitStored(3)
	hoID, laID := w.torrentID(hostile.InfoHash), w.torrentID(large.InfoHash)
	apiTok, rssTok := w.mintToken("api"), w.mintToken("rss")

	v2 := oastest.Load(t, apidocs.V2())
	v1 := oastest.Load(t, apidocs.V1())
	do := func(spec *oastest.Spec, prefix, tmpl, path, token string, want int) []byte {
		t.Helper()
		st, hdr, body := w.bearer(token, prefix+path)
		if st != want {
			t.Fatalf("GET %s: %d, want %d\n%.300s", path, st, want, body)
		}
		spec.Check(t, "GET", tmpl, st, hdr, body)
		return body
	}
	v2do := func(tmpl, path, token string, want int) []byte {
		t.Helper()
		return do(v2, "/api/v2", tmpl, path, token, want)
	}
	id := func(n int) string { return strconv.Itoa(n) }

	// /torrents
	v2do("/torrents", "/torrents?limit=2", apiTok, 200)
	v2do("/torrents", "/torrents?q=w9needle&sort=name&order=asc", apiTok, 200)
	v2do("/torrents", "/torrents?q=nomatchatallxyz", apiTok, 200)
	v2do("/torrents", "/torrents?info_hash="+hex.EncodeToString(hostile.InfoHash[:]), apiTok, 200)
	v2do("/torrents", "/torrents?limit=0", apiTok, 400)
	v2do("/torrents", "/torrents?cursor=not-a-cursor-at-all", apiTok, 400)
	v2do("/torrents", "/torrents", "", 401)
	v2do("/torrents", "/torrents", "mgt_"+strings.Repeat("A", 43), 401)
	// /torrents/{id}
	for _, n := range []int{w.torrentID(single.InfoHash), hoID, laID} {
		v2do("/torrents/{id}", "/torrents/"+id(n), apiTok, 200)
	}
	v2do("/torrents/{id}", "/torrents/424242", apiTok, 404)
	v2do("/torrents/{id}", "/torrents/abc", apiTok, 404) // a malformed id is "no such torrent"
	v2do("/torrents/{id}", "/torrents/"+id(hoID), "", 401)
	// /torrents/{id}/files/tree
	v2do("/torrents/{id}/files/tree", "/torrents/"+id(hoID)+"/files/tree", apiTok, 200)
	v2do("/torrents/{id}/files/tree", "/torrents/"+id(laID)+"/files/tree", apiTok, 422)
	v2do("/torrents/{id}/files/tree", "/torrents/424242/files/tree", apiTok, 404)
	v2do("/torrents/{id}/files/tree", "/torrents/"+id(hoID)+"/files/tree", "", 401)
	// /torrents/{id}/files
	v2do("/torrents/{id}/files", "/torrents/"+id(laID)+"/files", apiTok, 200)
	v2do("/torrents/{id}/files", "/torrents/"+id(laID)+"/files?dir=0&limit=10&offset=5", apiTok, 200)
	v2do("/torrents/{id}/files", "/torrents/"+id(laID)+"/files?dir=x", apiTok, 400)
	v2do("/torrents/{id}/files", "/torrents/"+id(laID)+"/files?dir=0_999", apiTok, 404)
	v2do("/torrents/{id}/files", "/torrents/424242/files", apiTok, 404)
	v2do("/torrents/{id}/files", "/torrents/"+id(laID)+"/files", "", 401)
	// /feeds/latest.rss (RSS token in the query, nothing else)
	if body := v2do("/feeds/latest.rss", "/feeds/latest.rss?feed_token="+rssTok, "", 200); !strings.Contains(string(body), "<rss") {
		t.Fatal("not an RSS document")
	}
	v2do("/feeds/latest.rss", "/feeds/latest.rss?feed_token=mgf_"+strings.Repeat("A", 43), "", 401)
	v2do("/feeds/latest.rss", "/feeds/latest.rss", "", 401)
	v2do("/feeds/latest.rss", "/feeds/latest.rss?feed_token="+apiTok, "", 401) // an API token is not an RSS token

	// v1 feed (deprecated): token by query, header and cookie; the legacy 401.
	do(v1, "/api/v1", "/feed/rss/last", "/feed/rss/last?api-token="+rssTok, "", 200)
	do(v1, "/api/v1", "/feed/rss/last", "/feed/rss/last", "", 401)
	do(v1, "/api/v1", "/feed/rss/last", "/feed/rss/last?api-token="+apiTok, "", 401)

	// 429: the failed-credential limiter of each surface answers with the
	// documented problem (v2) or legacy envelope (v1) and a Retry-After.
	limited := func(spec *oastest.Spec, prefix, tmpl, path, token string) {
		t.Helper()
		for i := 0; i < 60; i++ {
			st, hdr, body := w.bearer(token, prefix+path)
			if st == http.StatusTooManyRequests {
				spec.Check(t, "GET", tmpl, st, hdr, body)
				return
			}
		}
		t.Fatalf("%s never answered 429", path)
	}
	limited(v1, "/api/v1", "/feed/rss/last", "/feed/rss/last?api-token=mgf_"+strings.Repeat("B", 43), "")
	limited(v2, "/api/v2", "/feeds/latest.rss", "/feeds/latest.rss?feed_token=mgf_"+strings.Repeat("C", 43), "")
	limited(v2, "/api/v2", "/torrents", "/torrents", "mgt_"+strings.Repeat("D", 43))

	// Every documented operation of both documents was exercised.
	for _, c := range []struct {
		name string
		spec *oastest.Spec
	}{{"v2", v2}, {"v1", v1}} {
		for _, op := range c.spec.Operations() {
			parts := strings.SplitN(op, " ", 2)
			if !c.spec.Covered(parts[0], parts[1]) {
				t.Errorf("%s operation %s has no real-binary exercise", c.name, op)
			}
		}
	}
}

// The shipped binary links neither the test-only OpenAPI validator nor its
// dependencies, and carries no profiling endpoint.
func TestW9BinaryDoesNotLinkTestSupport(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 50 {
		t.Fatalf("suspiciously small dependency list (%d)", len(deps))
	}
	for _, dep := range deps {
		for _, bad := range []string{
			"github.com/skobkin/magnetico-web/internal/oastest",
			"github.com/santhosh-tekuri/jsonschema",
			"github.com/dlclark/regexp2",
			"net/http/pprof",
		} {
			if dep == bad || strings.HasPrefix(dep, bad+"/") {
				t.Errorf("the binary links %s", dep)
			}
		}
	}
}
