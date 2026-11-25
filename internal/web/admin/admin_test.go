package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/admin/access"
	"github.com/skobkin/magnetico-web/internal/web/admin/data"
	"github.com/skobkin/magnetico-web/internal/web/admin/stats"
	"github.com/skobkin/magnetico-web/internal/web/server"
)

type fakeBackend struct{}

func (fakeBackend) Verify(context.Context) error { return nil }
func (fakeBackend) Ping(context.Context) error   { return nil }
func (fakeBackend) Close()                       {}

func newServer(t *testing.T) *server.Server {
	t.Helper()
	pub, _ := url.Parse("https://magnet.example.org")
	srv, err := server.New(server.Config{
		PublicURL: pub, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RequestTimeout: 5 * time.Second, SearchTimeout: time.Minute, ShutdownTimeout: 5 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// passThrough is a guard that lets everything through WITHOUT a Principal: the
// handlers must still refuse (defence in depth).
func passThrough(next http.Handler) http.Handler { return next }

func TestAdminRoutesAreGetOnlyAndInTheAdminClasses(t *testing.T) {
	srv := newServer(t)
	srv.Router().SetGuard(server.Admin, passThrough)
	srv.Router().SetGuard(server.AdminJSON, passThrough)
	Install(srv, stats.NewService(nil), data.New(nil))
	var n int
	for _, r := range srv.Router().Routes() {
		if !strings.Contains(r.Pattern, " /admin") {
			continue
		}
		n++
		if !strings.HasPrefix(r.Pattern, "GET ") {
			t.Errorf("admin route %q is not GET-only", r.Pattern)
		}
		if r.Access != server.Admin && r.Access != server.AdminJSON {
			t.Errorf("admin route %q has access class %v", r.Pattern, r.Access)
		}
		if strings.Contains(r.Pattern, "/api/") && r.Access != server.AdminJSON {
			t.Errorf("JSON route %q is not AdminJSON", r.Pattern)
		}
	}
	if n < 3 {
		t.Fatalf("only %d admin routes registered", n)
	}
	// Registering a state-changing admin route is a programming error.
	for _, a := range []server.Access{server.Admin, server.AdminJSON} {
		for _, p := range []string{"POST /admin/x", "PUT /admin/x", "DELETE /admin/x", "PATCH /admin/x", "/admin/x"} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("%v %s did not panic", a, p)
					}
				}()
				srv.Router().Handle(p, server.KindRequest, a, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			}()
		}
	}
}

func TestMutatingMethodsAreRefusedAndRunNoHandler(t *testing.T) {
	srv := newServer(t)
	var guarded int
	count := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { guarded++; next.ServeHTTP(w, r) })
	}
	srv.Router().SetGuard(server.Admin, count)
	srv.Router().SetGuard(server.AdminJSON, count)
	Install(srv, stats.NewService(nil), data.New(nil))
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		for _, p := range []string{"/admin", "/admin/api/overview", "/admin/api/series"} {
			req := httptest.NewRequest(m, "https://magnet.example.org"+p, strings.NewReader("x=1"))
			req.RemoteAddr = "203.0.113.9:1"
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d, want 405", m, p, rec.Code)
			}
		}
	}
	if guarded != 0 {
		t.Fatalf("a mutating request reached a guard or handler %d times", guarded)
	}
}

func TestHandlersRefuseARequestThatDidNotPassTheGuard(t *testing.T) {
	srv := newServer(t)
	srv.Router().SetGuard(server.Admin, passThrough)
	srv.Router().SetGuard(server.AdminJSON, passThrough)
	Install(srv, stats.NewService(nil), data.New(nil))
	for _, p := range []string{"/admin", "/admin/api/overview", "/admin/api/series?chart=queue&g=day"} {
		req := httptest.NewRequest("GET", "https://magnet.example.org"+p, nil)
		req.RemoteAddr = "203.0.113.9:1"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without a Principal: %d, want 403", p, rec.Code)
		}
	}
}

// Principals are granted by exactly one non-test file: the guard.
func TestOnlyTheGuardGrantsAPrincipal(t *testing.T) {
	var offenders []string
	_ = filepath.Walk("../..", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "access.Grant(") && filepath.ToSlash(path) != "../../web/auth/admin.go" {
			offenders = append(offenders, path)
		}
		return nil
	})
	if len(offenders) > 0 {
		t.Fatalf("access.Grant is called outside the guard: %v", offenders)
	}
	if access.Grant(0, "x").Valid() || (access.Principal{}).Valid() {
		t.Fatal("a Principal without a positive user id or the zero value is valid")
	}
}

func TestFormattingNeverTurnsUnavailableIntoZero(t *testing.T) {
	if fmtInt(stats.Measured[int64]{}) != dash || fmtBytes(stats.Measured[int64]{}) != dash ||
		fmtFloat(stats.Measured[float64]{}, "/s") != dash || fmtPercent(stats.Measured[float64]{}) != dash || fmtAge(stats.Measured[time.Duration]{}) != dash {
		t.Fatal("an unavailable value is not a dash")
	}
	if got := fmtInt(stats.Measured[int64]{Value: 0, Valid: true}); got != "0" {
		t.Fatalf("a measured zero renders as %q", got)
	}
	if got := fmtInt(stats.Measured[int64]{Value: 1234567, Valid: true, Approx: true}); got != "≈ 1,234,567" {
		t.Fatalf("approximate count: %q", got)
	}
	if got := bytesIEC(1536); got != "1.5 KiB" {
		t.Fatalf("bytesIEC: %q", got)
	}
}

func TestCursorsAreStrictlyParsed(t *testing.T) {
	for _, id := range []int64{1, 50, 123456789, 1<<62 - 1} {
		got, ok := decodeID(encodeID(id))
		if !ok || got != id {
			t.Errorf("id %d does not round-trip: %d %v", id, got, ok)
		}
	}
	for _, bad := range []string{
		"", "0", "-1", "abc", "MQ==", "MQ=", strings.Repeat("A", 101), "MDE" /* "01": leading zero */, "MS4w" /* "1.0" */, "MSAy", /* "1 2" */
		"bnVsbA" /* "null" */, "OTIyMzM3MjAzNjg1NDc3NTgwOA" /* 2^63 */, "NDYxMTY4NjAxODQyNzM4NzkwNg" /* 2^62 */, "%00", "../x",
	} {
		if n, ok := decodeID(bad); ok {
			t.Errorf("decodeID(%q) = %d: accepted", bad, n)
		}
	}
	for _, name := range []string{"alpha", "a", "node-1.eu_west", strings.Repeat("a", 64)} {
		got, ok := decodeName(encodeName(name))
		if !ok || got != name {
			t.Errorf("name %q does not round-trip: %q %v", name, got, ok)
		}
	}
	for _, bad := range []string{"", "Alpha" /* upper case */, "-lead", ".dot", "has space", strings.Repeat("a", 65), "a\x00b", "a/b", "é"} {
		if _, ok := decodeName(encodeName(bad)); ok && bad != "" {
			t.Errorf("decodeName accepted %q", bad)
		}
		if bad != "" {
			continue
		}
		if _, ok := decodeName(""); ok {
			t.Error("the empty cursor was accepted")
		}
	}
	if _, ok := decodeName("not base64!"); ok {
		t.Error("garbage accepted")
	}
}

// TestCrawlersPageShowsWhenItWasReadAndWarnsWhenItIsCached (Task 09A): the
// crawlers page is cached, and after a failed refresh it shows the last known
// good page. It must say when the data was read, and that a refresh failed,
// instead of looking current.
func TestCrawlersPageShowsWhenItWasReadAndWarnsWhenItIsCached(t *testing.T) {
	srv := newServer(t)
	read := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	page := stats.CrawlerPage{Ingest: stats.IngestStatus{State: stats.IngestOK}, StatsAvailable: true,
		Items: []stats.CrawlerRow{{ID: "11111111-1111-4111-8111-111111111111", Name: "alpha", Enabled: true, CreatedAt: read}}}
	render := func(stale bool) string {
		pg := buildCrawlersPage(chrome{Tab: "crawlers", IsAdmin: true, Tabs: tabs("crawlers")}, page, stats.Freshness{FetchedAt: read, Stale: stale})
		rec := httptest.NewRecorder()
		if err := srv.Renderer().Render(rec, http.StatusOK, "admin_crawlers", pg); err != nil {
			t.Fatal(err)
		}
		return rec.Body.String()
	}
	fresh := render(false)
	if !strings.Contains(fresh, "Read at 2026-11-25 10:00:00 UTC") {
		t.Error("the page does not say when it was read")
	}
	if strings.Contains(fresh, "last known") {
		t.Error("a fresh read must not claim to be the last known state")
	}
	stale := render(true)
	if !strings.Contains(stale, "last known state from 2026-11-25 10:00:00 UTC") {
		t.Error("a cached page after a failed refresh must say so, with the time it was read")
	}
}

func overviewForPage() stats.Overview {
	var o stats.Overview
	o.Schema = stats.StateUp
	o.Crawlers = stats.CrawlerCounts{Registered: stats.Measured[int64]{Value: 7, Valid: true}, Enabled: stats.Measured[int64]{Value: 5, Valid: true}, Active: stats.Measured[int64]{Value: 3, Valid: true}}
	o.Users = stats.Users{Total: stats.Measured[int64]{Value: 12, Valid: true}, Active: stats.Measured[int64]{Value: 9, Valid: true},
		Disabled: stats.Measured[int64]{Value: 3, Valid: true}, Admins: stats.Measured[int64]{Value: 2, Valid: true}}
	rate := func(name string, v float64) {
		o.Rates.Windows[0].Values[opstatsIndex(name)] = stats.Measured[float64]{Value: v, Valid: true}
	}
	o.Rates.Windows[0].Seconds, o.Rates.Windows[0].Unit, o.Rates.Windows[0].Included = 60, "per second", 1
	rate("submit_requests", 1.25)
	rate("exists_requests", 2.5)
	rate("torrents_created", 0.75)
	rate("auth_db_lookups", 0.5)
	rate("auth_cache_hits", 90)
	rate("auth_cache_misses", 10)
	rate("auth_busy", 0.125)
	rate("auth_storage_failures", 0.0625)
	return o
}

func opstatsIndex(name string) int {
	for i, n := range stats.MetricNames {
		if n == name {
			return i
		}
	}
	panic("no metric " + name)
}

func renderOverview(t *testing.T, o stats.Overview) string {
	t.Helper()
	srv := newServer(t)
	rec := httptest.NewRecorder()
	pg := buildOverview(chrome{Tab: "overview", IsAdmin: true, Tabs: tabs("overview")}, o, stats.Freshness{FetchedAt: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)})
	if err := srv.Renderer().Render(rec, http.StatusOK, "admin_overview", pg); err != nil {
		t.Fatal(err)
	}
	return rec.Body.String()
}

// TestOverviewShowsTheAgreedCrawlerRateAndAuthenticationFigures (Task 09A): the
// approved dashboard has registered, enabled and recently active crawlers, the
// submit and existence request rates, newly created torrents, the authentication
// lookups, cache efficiency, busy refusals and storage failures, and the disabled
// user count.
func TestOverviewShowsTheAgreedCrawlerRateAndAuthenticationFigures(t *testing.T) {
	body := renderOverview(t, overviewForPage())
	for _, want := range []string{
		"Registered crawlers", ">7<", "Crawlers enabled", ">5<", "Recently active", ">3<",
		"Submit requests", "1.25", "Existence requests", "2.5", "New torrents", "0.75",
		"Authentication database lookups", "0.5", "90 %", "Authentication busy refusals", "0.125", "Authentication storage failures", "0.0625",
		"Users disabled", ">3<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the overview page lacks %q", want)
		}
	}
	if !strings.Contains(body, "15 minutes") {
		t.Error("the page must state the definition of recently active (15 minutes)")
	}
}

func TestUnavailableCrawlerCountsAreDashesNotZero(t *testing.T) {
	o := overviewForPage()
	o.Crawlers = stats.CrawlerCounts{}
	body := renderOverview(t, o)
	i := strings.Index(body, "Registered crawlers")
	if i < 0 || !strings.Contains(body[i:i+200], "—") {
		t.Fatalf("an unavailable count must read as a dash:\n%s", body[max(i, 0):min(i+200, len(body))])
	}
}

func TestCrawlersPageDistinguishesDisabledInactiveNeverConnectedAndUnavailable(t *testing.T) {
	srv := newServer(t)
	read := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	mk := func(name string, enabled bool, a *stats.Activity) stats.CrawlerRow {
		return stats.CrawlerRow{ID: "11111111-1111-4111-8111-111111111111", Name: name, Enabled: enabled, CreatedAt: read, Activity: a}
	}
	act := func(ago time.Duration) *stats.Activity {
		return &stats.Activity{FirstSeen: read.Add(-48 * time.Hour), LastSeen: read.Add(-ago), SeenAgo: ago, ExistsHashes: 4242, Received: 7}
	}
	page := stats.CrawlerPage{Ingest: stats.IngestStatus{State: stats.IngestOK}, StatsAvailable: true, Items: []stats.CrawlerRow{
		mk("on", true, act(time.Minute)), mk("idle", true, act(3*time.Hour)), mk("new", true, nil), mk("off", false, act(time.Minute)),
	}}
	render := func(p stats.CrawlerPage) string {
		rec := httptest.NewRecorder()
		if err := srv.Renderer().Render(rec, http.StatusOK, "admin_crawlers", buildCrawlersPage(chrome{Tab: "crawlers", IsAdmin: true, Tabs: tabs("crawlers")}, p, stats.Freshness{FetchedAt: read})); err != nil {
			t.Fatal(err)
		}
		return rec.Body.String()
	}
	body := render(page)
	for _, want := range []string{">active<", ">inactive<", ">never connected<", ">disabled<", "First seen", "2026-11-23 10:00", "Existence hashes", "4,242"} {
		if !strings.Contains(body, want) {
			t.Errorf("the crawlers page lacks %q", want)
		}
	}
	page.StatsAvailable = false
	page.Items = page.Items[:3]
	for i := range page.Items {
		page.Items[i].Activity = nil
	}
	body = render(page)
	if !strings.Contains(body, ">activity unavailable<") || strings.Contains(body, ">never connected<") {
		t.Error("with the statistics unreadable an enabled crawler's activity is unavailable, not 'never connected'")
	}
}

func TestSeriesJSONCarriesNotesAndFreshness(t *testing.T) {
	b := []time.Time{time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC)}
	s := stats.Series{Query: stats.Query{Chart: "queue", Granularity: "day", Period: "2026-11-24"}, Buckets: b, Partial: []bool{false, true},
		Notes: [][]string{nil, {stats.NoteApproximateCounts}}}
	j := buildSeriesJSON(s, stats.Freshness{FetchedAt: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC), Stale: true})
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Notes     [][]string `json:"notes"`
		Freshness struct {
			FetchedAt string `json:"fetched_at"`
			Stale     bool   `json:"stale"`
		} `json:"freshness"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Notes) != 2 || back.Notes[0] != nil || len(back.Notes[1]) != 1 || back.Notes[1][0] != "approximate_counts" {
		t.Fatalf("notes = %v in %s", back.Notes, raw)
	}
	if !back.Freshness.Stale || back.Freshness.FetchedAt != "2026-11-25T10:00:00Z" {
		t.Fatalf("freshness = %+v", back.Freshness)
	}
}
