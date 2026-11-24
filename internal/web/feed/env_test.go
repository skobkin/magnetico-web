package feed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/trackers"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

func tctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return c
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
func (s *syncBuf) reset()                      { s.mu.Lock(); defer s.mu.Unlock(); s.b.Reset() }

type fakeBackend struct{}

func (fakeBackend) Verify(context.Context) error { return nil }
func (fakeBackend) Ping(context.Context) error   { return nil }
func (fakeBackend) Close()                       {}

// spyRows counts and can block or fail the page reads.
type spyRows struct {
	inner Rows
	calls atomic.Int64
	gate  chan struct{}
	err   atomic.Pointer[error]
}

func (s *spyRows) LatestTorrents(ctx context.Context, offset, limit int) ([]persistence.Torrent, error) {
	s.calls.Add(1)
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e := s.err.Load(); e != nil {
		return nil, *e
	}
	return s.inner.LatestTorrents(ctx, offset, limit)
}

type fakeSource struct{ urls atomic.Pointer[[]string] }

func (f *fakeSource) URLs(context.Context) ([]string, error) { return *f.urls.Load(), nil }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type envOpts struct {
	failLim, useLim *ratelimit.Limiter
	noTrackers      bool
}

type env struct {
	t        *testing.T
	h        http.Handler
	pool     *pgxpool.Pool
	store    *accounts.Store
	rows     *spyRows
	trackers *trackers.Cache
	source   *fakeSource
	clock    *clock
	log      *syncBuf
	user     accounts.User
	srv      *server.Server
}

const publicBase = "https://magnet.example.org"

func newEnv(t *testing.T, mutate ...func(*envOpts)) *env {
	t.Helper()
	var o envOpts
	for _, m := range mutate {
		m(&o)
	}
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	if _, err := webschema.Init(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.MaxConns = 24
	pool, err := pgxpool.NewWithConfig(tctx(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	e := &env{t: t, pool: pool, log: &syncBuf{}, clock: &clock{t: time.Unix(1_800_000_000, 0)}}
	logger := slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pu, _ := url.Parse(publicBase)
	srv, err := server.New(server.Config{
		PublicURL: pu, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RequestTimeout: 10 * time.Second, SearchTimeout: time.Minute, ShutdownTimeout: 5 * time.Second, Logger: logger,
	}, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	e.store = accounts.New(pool)
	e.rows = &spyRows{inner: persistence.NewReader(pool, codec.DefaultLimits())}
	urls := []string{"udp://tracker.one:1337/announce", "http://tracker.two:80/announce"}
	e.source = &fakeSource{}
	e.source.urls.Store(&urls)
	e.trackers = trackers.New(e.source, time.Hour, logger)
	if err := e.trackers.Load(tctx(t)); err != nil {
		t.Fatal(err)
	}
	big := func() *ratelimit.Limiter {
		l, _ := ratelimit.New(ratelimit.Config{Burst: 1000000, Interval: time.Microsecond, MaxKeys: 1000})
		return l
	}
	c := Config{Auth: e.store, Rows: e.rows, PublicBase: pu, Now: e.clock.Now, CacheTTL: time.Minute, FailLimiter: big(), UseLimiter: big()}
	if !o.noTrackers {
		c.Trackers = e.trackers
	}
	if o.failLim != nil {
		c.FailLimiter = o.failLim
	}
	if o.useLim != nil {
		c.UseLimiter = o.useLim
	}
	if _, err := Install(srv, c); err != nil {
		t.Fatal(err)
	}
	e.h = srv.Handler()

	res, err := e.store.CreateUser(tctx(t), accounts.CreateUserParams{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	e.user, _ = e.store.GetUserByID(tctx(t), res.ID)
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(tctx(e.t), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// seedFixed inserts a torrent with an exact info hash (40 hex).
func (e *env) seedFixed(hashHex, name string, total, disc int64) int32 {
	e.t.Helper()
	var id int32
	if err := e.pool.QueryRow(tctx(e.t), `INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
	    VALUES (decode($1,'hex'),$2,$3,$4,1) RETURNING id`, hashHex, name, total, disc).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) rssToken() accounts.CreatedToken {
	e.t.Helper()
	tok, err := e.store.CreateRSSToken(tctx(e.t), e.user.ID, "feed", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *env) apiToken() accounts.CreatedToken {
	e.t.Helper()
	tok, err := e.store.CreateAPIToken(tctx(e.t), e.user.ID, "api", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

// resp is one HTTP exchange.
type resp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (e *env) do(path string, hdr map[string]string, cookies ...*http.Cookie) resp {
	e.t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "203.0.113.9:4000"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Header: res.Header, Body: b}
}

// ---- structural comparison ---------------------------------------------------

// canon renders an XML document as a canonical, comparable string: elements in
// document order, attributes sorted, whitespace-only text dropped, and the
// text of the channel dates replaced by a placeholder.
func canon(t *testing.T, doc []byte) string {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(doc))
	var b strings.Builder
	var stack []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("invalid XML: %v\n%s", err, doc)
		}
		switch v := tok.(type) {
		case xml.StartElement:
			var attrs []string
			for _, a := range v.Attr {
				attrs = append(attrs, a.Name.Space+":"+a.Name.Local+"="+a.Value)
			}
			sort.Strings(attrs)
			name := v.Name.Space + ":" + v.Name.Local
			stack = append(stack, v.Name.Local)
			fmt.Fprintf(&b, "<%s %s>", name, strings.Join(attrs, " "))
		case xml.EndElement:
			stack = stack[:len(stack)-1]
			b.WriteString("</>")
		case xml.CharData:
			s := strings.TrimSpace(string(v))
			if s == "" {
				continue
			}
			if n := len(stack); n >= 2 && stack[n-2] == "channel" && (stack[n-1] == "pubDate" || stack[n-1] == "lastBuildDate") {
				s = "{DATE}"
			}
			b.WriteString(s)
		}
	}
	return b.String()
}

func hashFor(t *testing.T) (string, error) {
	t.Helper()
	svc := newPasswordService()
	return svc.Hash(context.Background(), "correct horse battery")
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func (e *env) srvPublicBase() *url.URL { return mustURL(publicBase) }

func syntheticRows(n int) []persistence.Torrent {
	rows := make([]persistence.Torrent, n)
	for i := range rows {
		h := sha256.Sum256([]byte(fmt.Sprint(i)))
		rows[i] = persistence.Torrent{ID: int32(i + 1), InfoHash: h[:20], Name: fmt.Sprintf("Synthetic torrent number %d with a moderately long descriptive name", i),
			TotalSize: int64(i) * 1000, DiscoveredOn: 1_700_000_000 + int64(i), FileCount: 3}
	}
	return rows
}
