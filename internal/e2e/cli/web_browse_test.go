package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/codec"
)

// seedBrowse inserts synthetic torrents (and blobs) as the schema owner.
func seedBrowse(t *testing.T, dsn string, name string, files []codec.File) int {
	t.Helper()
	h := sha256.Sum256([]byte(name))
	var total int64
	for _, f := range files {
		total += f.Size
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var id int
	if err := conn.QueryRow(ctx, `INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
	    VALUES ($1,$2,$3,1700000000,$4) RETURNING id`, h[:20], name, total, len(files)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	blob, _, err := codec.Encode(files, codec.ProfileCompact, codec.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO magneticod.torrent_file_blobs (torrent_id, data) VALUES ($1,$2)`, id, blob); err != nil {
		t.Fatal(err)
	}
	return id
}

func webBinaryFixture(t *testing.T, extraEnv ...string) (*runningWeb, *webBrowser, string) {
	t.Helper()
	bin := buildBinary(t)
	dsn := webTarget(t)
	if out, code := runWebSchemaInit(t, bin, dsn, "web-schema-init"); code != 0 {
		t.Fatalf("web-schema-init: exit %d: %s", code, out)
	}
	rtDSN, opDSN := webAuthRoles(t, dsn)
	const pw = "binary-browse-password-1"
	for _, c := range []struct {
		stdin string
		args  []string
	}{{"", []string{"user", "add", "alice"}}, {pw + "\n", []string{"user", "set-password", "alice"}}} {
		if out, errOut, code := runWebAdminBinary(t, bin, opDSN, c.stdin, nil, c.args...); code != 0 {
			t.Fatalf("%v: %d %s%s", c.args, code, out, errOut)
		}
	}
	w := startWeb(t, bin, rtDSN, "http", extraEnv...)
	b := newBrowser(t, w.addr)
	if resp := b.login("alice", pw); resp.StatusCode != 303 {
		t.Fatalf("login: %d\n%s", resp.StatusCode, b.body)
	}
	return w, b, dsn
}

func TestWebBinaryBrowseSearchDetailsFiles(t *testing.T) {
	w, b, dsn := webBinaryFixture(t)
	files := []codec.File{
		{Path: "BINSECRETPATH/a.txt", Size: 10}, {Path: "BINSECRETPATH/sub/b%2Fc", Size: 20},
		{Path: "bad\xff/x", Size: 5}, {Path: "z", Size: 1}, {Path: "z", Size: 1},
	}
	id := seedBrowse(t, dsn, "BINSECRETNAME release", files)
	for i := 0; i < 30; i++ {
		seedBrowse(t, dsn, fmt.Sprintf("filler item %02d", i), []codec.File{{Path: "f", Size: int64(i + 1)}})
	}

	if resp := b.get("/"); resp.StatusCode != 303 || resp.Header.Get("Location") != "/torrents" {
		t.Fatalf("/: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := b.get("/torrents?q=BINSECRETNAME"); resp.StatusCode != 200 || !strings.Contains(b.body, "BINSECRETNAME release") {
		t.Fatalf("search: %d", resp.StatusCode)
	}
	detail := fmt.Sprintf("/torrents/%d", id)
	if resp := b.get(detail); resp.StatusCode != 200 || !strings.Contains(b.body, "magnet:?xt=urn:btih:") {
		t.Fatalf("details: %d", resp.StatusCode)
	}
	if !strings.Contains(b.body, `data-mode="tree"`) || !strings.Contains(b.body, "data-tree-url=") {
		t.Fatalf("details lacks the file browser mode:\n%s", b.body)
	}
	base := detail + "/files"
	if resp := b.get(base + "/tree"); resp.StatusCode != 200 || !json.Valid([]byte(b.body)) {
		t.Fatalf("tree: %d", resp.StatusCode)
	}
	var tree struct {
		FileCount int `json:"file_count"`
		Root      struct {
			Dirs []struct {
				Name  string `json:"name"`
				Files []struct {
					Name      string `json:"name"`
					Duplicate bool   `json:"duplicate"`
				} `json:"files"`
			} `json:"dirs"`
			Files []struct {
				Name      string `json:"name"`
				Duplicate bool   `json:"duplicate"`
			} `json:"files"`
		} `json:"root"`
	}
	if err := json.Unmarshal([]byte(b.body), &tree); err != nil {
		t.Fatal(err)
	}
	if tree.FileCount != 5 || len(tree.Root.Files) != 2 || !tree.Root.Files[1].Duplicate {
		t.Fatalf("tree content: %+v", tree)
	}
	if resp := b.get(base); resp.StatusCode != 200 || !strings.Contains(b.body, `"name":"BINSECRETPATH"`) ||
		!strings.Contains(b.body, `"name_base64":"YmFk/w=="`) {
		t.Fatalf("directory page: %d\n%s", resp.StatusCode, b.body)
	}
	if resp := b.get(base + "?dir=0_0"); resp.StatusCode != 200 || !strings.Contains(b.body, `"name":"b%2Fc"`) {
		t.Fatalf("nested: %d\n%s", resp.StatusCode, b.body)
	}
	for q, want := range map[string]int{"?dir=0_9": 404, "?dir=x": 400, "?limit=0": 400, "?offset=-1": 400} {
		if resp := b.get(base + q); resp.StatusCode != want {
			t.Fatalf("%s: %d, want %d", q, resp.StatusCode, want)
		}
	}
	anon := newBrowser(t, w.addr)
	for _, p := range []string{base, base + "/tree"} {
		if resp := anon.get(p); resp.StatusCode != 401 || resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Fatalf("anonymous %s: %d %q", p, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	if resp := b.get("/torrents/424242"); resp.StatusCode != 404 {
		t.Fatalf("unknown torrent: %d", resp.StatusCode)
	}
	if resp := b.get("/torrents?q=ub&cursor=zzz"); resp.StatusCode != 400 {
		t.Fatalf("bad cursor: %d", resp.StatusCode)
	}

	w.stop(t) // graceful: exit status 0
	out := w.out.String()
	for _, secret := range []string{"BINSECRET", "binary-browse-password-1", "filler item"} {
		if strings.Contains(out, secret) {
			t.Errorf("process output leaks %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "stopped cleanly") {
		t.Errorf("no clean stop record:\n%s", out)
	}
}

// A search blocked on a table lock still lets login and details answer, and a
// SIGTERM whose grace period expires cancels it and exits with status 3.
func TestWebBinaryForcedShutdownAndResponsiveness(t *testing.T) {
	w, b, dsn := webBinaryFixture(t,
		"MAGNETICO_WEB_SHUTDOWN_TIMEOUT=1s", "MAGNETICO_WEB_SEARCH_TIMEOUT=20s")
	id := seedBrowse(t, dsn, "locked torrent", []codec.File{{Path: "a", Size: 1}, {Path: "b", Size: 2}})

	ctx := context.Background()
	lock, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close(ctx)
	tx, err := lock.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE magneticod.torrents IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("GET", b.base+"/torrents?q=locked", nil)
		for _, c := range b.c.Jar.Cookies(req.URL) {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- -1
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	obs, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer obs.Close(ctx)
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int
		_ = obs.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query ILIKE '%magneticod.torrents%'`).Scan(&n)
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the search never reached the table lock")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Login (another browser) stays responsive while the search is stuck.
	other := newBrowser(t, w.addr)
	start := time.Now()
	if resp := other.login("alice", "binary-browse-password-1"); resp.StatusCode != 303 || time.Since(start) > 3*time.Second {
		t.Fatalf("login during a blocked search: %d in %v", resp.StatusCode, time.Since(start))
	}

	if err := w.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-w.exited:
		var ee *exec.ExitError
		if !asExit(err, &ee) || ee.ExitCode() != 3 {
			t.Fatalf("exit after forced shutdown: %v\n%s", err, w.out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("no exit:\n%s", w.out.String())
	}
	if !strings.Contains(w.out.String(), "grace period expired") {
		t.Errorf("forced-shutdown record missing:\n%s", w.out.String())
	}
	<-done
	_ = id
}

func asExit(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}
