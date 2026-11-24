package cli

// Regression tests for the W9 load client itself (not gated: they need no
// database and no binary). The load numbers are only as honest as the client's
// classification, so these pin it: a response that cannot be read to the end is
// a transport failure, a 422 counts as expected only when it is the documented
// full-tree answer, and the search workload really follows keyset continuation
// links.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

type errAfter struct {
	data string
	err  error
	done bool
}

func (r *errAfter) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), nil
}

func TestW9ReadBodyReportsReadErrors(t *testing.T) {
	boom := errors.New("connection reset")
	if _, err := readBody(&errAfter{data: "partial", err: boom}); !errors.Is(err, boom) {
		t.Fatalf("readBody swallowed the read error: %v", err)
	}
	b, err := readBody(strings.NewReader(strings.Repeat("x", w9BodyKeep+10)))
	if err != nil || len(b) != w9BodyKeep {
		t.Fatalf("an oversized body keeps %d bytes and no error, got %d / %v", w9BodyKeep, len(b), err)
	}
}

func w9TestLoad(addr string, timeout time.Duration) (*w9Load, *w9Ident) {
	hc := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &w9Load{addr: addr, hc: hc}, &w9Ident{xff: "10.0.0.1"}
}

func TestW9TruncatedAndStalledBodiesAreTransportFailures(t *testing.T) {
	// Truncated: the promised length is never delivered.
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 1000\r\n\r\nshort")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	defer truncated.Close()
	l, id := w9TestLoad(strings.TrimPrefix(truncated.URL, "http://"), 5*time.Second)
	out, _, _ := l.fetch(w9Req{class: "details", path: "/x", expect: []int{200}}, id)
	if out != oTransport {
		t.Fatalf("a truncated 200 response was classified %q, want %q", out, oTransport)
	}

	// Stalled: status line and headers arrive, the body stops past the client timeout.
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("short"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer stalled.Close()
	defer close(release)
	l, id = w9TestLoad(strings.TrimPrefix(stalled.URL, "http://"), 300*time.Millisecond)
	out, _, _ = l.fetch(w9Req{class: "details", path: "/x", expect: []int{200}}, id)
	if out != oTransport {
		t.Fatalf("a body that timed out mid-transfer was classified %q, want %q", out, oTransport)
	}

	// A complete body is still ok.
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "fine") }))
	defer okSrv.Close()
	l, id = w9TestLoad(strings.TrimPrefix(okSrv.URL, "http://"), 5*time.Second)
	if out, body, _ := l.fetch(w9Req{class: "details", path: "/x", expect: []int{200}}, id); out != oOK || string(body) != "fine" {
		t.Fatalf("complete response: %q %q", out, body)
	}
}

func TestW9ConnectionRefusedIsATransportFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	l, id := w9TestLoad(addr, time.Second)
	if out, _, _ := l.fetch(w9Req{class: "details", path: "/x", expect: []int{200}}, id); out != oTransport {
		t.Fatalf("refused connection classified %q", out)
	}
}

func TestW9ClassifyTreeUnprocessable(t *testing.T) {
	expect := []int{200, 422}
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"complete tree", 200, "{}", oOK},
		{"documented full-tree problem", 422, `{"type":"https://example.test/problems/full-tree-unavailable"}`, oOK},
		{"file index failure", 422, `{"type":"https://example.test/problems/index-too-complex"}`, oUnexpect},
		{"empty 422", 422, ``, oUnexpect},
		{"busy", 503, `{"title":"The server is busy"}`, oBusy},
		{"timeout", 503, `{"title":"The request timed out"}`, oTimeout},
		{"limiter", 429, ``, o429},
		{"other", 404, ``, oUnexpect},
	} {
		if got := classify(c.status, []byte(c.body), expect); got != c.want {
			t.Errorf("%s: classify(%d) = %q, want %q", c.name, c.status, got, c.want)
		}
	}
	// A 422 is never acceptable for a request that did not expect it.
	if got := classify(422, []byte("full-tree-unavailable"), []int{200}); got != oUnexpect {
		t.Errorf("unexpected 422 classified %q", got)
	}
}

func TestW9SearchContinuationFollowsTheNextLink(t *testing.T) {
	page := []byte(`<a class="btn small" href="/torrents?q=1080p&amp;sort=discovered&amp;cursor=AbC_-9&amp;size=20" rel="next">Next</a>`)
	next, ok := nextPage(page, map[string]string{"Cookie": "s=1"})
	if !ok || next.path != "/torrents?q=1080p&sort=discovered&cursor=AbC_-9&size=20" || next.class != "searchpage" || next.hdr["Cookie"] != "s=1" {
		t.Fatalf("continuation = %+v ok=%v", next, ok)
	}
	for _, bad := range []string{
		`<span class="btn small" aria-disabled="true">Next</span>`,             // last page
		`<a href="https://evil.example/torrents?cursor=x" rel="next">Next</a>`, // other host
		`<a href="/account?cursor=x" rel="next">Next</a>`,                      // other route
		`<a href="/torrents?q=x" rel="next">Next</a>`,                          // no cursor
	} {
		if _, ok := nextPage([]byte(bad), nil); ok {
			t.Errorf("followed %q", bad)
		}
	}

	// Search page, then its continuation, as the worker loop does: the server sees
	// the cursor requests.
	var seen, cont atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		if r.URL.Query().Get("cursor") != "" {
			cont.Add(1)
		}
		_, _ = io.WriteString(w, `<a href="/torrents?cursor=zz&amp;q=a" rel="next">Next</a>`)
	}))
	defer srv.Close()
	l, id := w9TestLoad(strings.TrimPrefix(srv.URL, "http://"), 5*time.Second)
	for i := 0; i < 20; i++ {
		out, body, _ := l.fetch(w9Req{class: "search", path: "/torrents?q=a", expect: []int{200}}, id)
		if out != oOK {
			t.Fatal(out)
		}
		if n, ok := nextPage(body, nil); !ok {
			t.Fatal("no continuation")
		} else if out, _, _ := l.fetch(n, id); out != oOK {
			t.Fatal(out)
		}
	}
	if cont.Load() != 20 || seen.Load() != 40 {
		t.Fatalf("server saw %d requests, %d of them continuations", seen.Load(), cont.Load())
	}
}

// ---- the ingest drain ----------------------------------------------------------------

func w9DrainEnv(t *testing.T, dsn string, deadline time.Duration) (*w9Env, *[]string) {
	t.Helper()
	var fails []string
	e := &w9Env{t: t, dsn: dsn, outDir: t.TempDir(), label: "drain", drainDeadline: deadline,
		onFail: func(m string) { fails = append(fails, m) }}
	return e, &fails
}

func TestW9DrainReportsQueryErrorsAndEnforcesItsDeadline(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)

	// 1. No ingest schema: the failed query must fail the gate, not read as an empty queue.
	e, fails := w9DrainEnv(t, dsn, 5*time.Second)
	e.drain()
	if len(*fails) == 0 || !strings.Contains((*fails)[0], "queue depth query failed") {
		t.Fatalf("a failed queue query was not reported: %v", *fails)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`CREATE SCHEMA magnetico_ingest`,
		`CREATE TABLE magnetico_ingest.queue (x int)`,
		`CREATE TABLE magnetico_ingest.quarantine (x int)`,
		`INSERT INTO magnetico_ingest.queue VALUES (1)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	// 2. A queue that never empties: the deadline ends the wait and fails the gate.
	e, fails = w9DrainEnv(t, dsn, 1500*time.Millisecond)
	start := time.Now()
	e.drain()
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("drain overran its 1.5 s deadline: %s", took)
	}
	if len(*fails) == 0 || !strings.Contains((*fails)[0], "queue holds 1 rows") {
		t.Fatalf("a non-empty queue did not fail the gate: %v", *fails)
	}

	// 3. A statement blocked behind a lock: the context must cut it at the deadline,
	// and the unknown depth is reported as such, never as zero.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE magnetico_ingest.queue IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	e, fails = w9DrainEnv(t, dsn, 1500*time.Millisecond)
	start = time.Now()
	e.drain()
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("a blocked statement outlived the deadline: drain took %s", took)
	}
	if len(*fails) == 0 || !strings.Contains((*fails)[0], "-1: unknown") {
		t.Fatalf("a blocked queue query was not reported as unknown depth: %v", *fails)
	}
}
