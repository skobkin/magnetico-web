package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

const adminPW = "admin-section-password-1"

// mkUser creates an account through the operator CLI with the given role.
func mkUser(t *testing.T, w *harnessWorld, name, role string) {
	t.Helper()
	for _, c := range []struct {
		stdin string
		args  []string
	}{{"", []string{"user", "add", name}}, {adminPW + "\n", []string{"user", "set-password", name}}, {"", []string{"user", "set-role", name, role}}} {
		if out, errOut, code := runWebAdminBinary(t, w.bin, w.dsn, c.stdin, nil, c.args...); code != 0 {
			t.Fatalf("web %v: exit %d\n%s%s", c.args, code, out, errOut)
		}
	}
}

func setRole(t *testing.T, w *harnessWorld, name, role string) {
	t.Helper()
	if out, errOut, code := runWebAdminBinary(t, w.bin, w.dsn, "", nil, "user", "set-role", name, role); code != 0 {
		t.Fatalf("set-role: %d %s%s", code, out, errOut)
	}
}

func adminWorld(t *testing.T) *harnessWorld {
	t.Helper()
	w := newHarnessWorld(t)
	if out, code := runWebSchemaInit(t, w.bin, w.dsn, "stats-schema-init"); code != 0 {
		t.Fatalf("stats-schema-init: %d %s", code, out)
	}
	mkUser(t, w, "bob", "admin")
	return w
}

func (w *harnessWorld) browserFor(user string) *webBrowser {
	b := newBrowser(w.t, w.web.addr)
	if resp := b.login(user, adminPW); resp.StatusCode != 303 {
		w.t.Fatalf("login %s: %d\n%s", user, resp.StatusCode, b.body)
	}
	return b
}

var adminRoutes = []struct {
	path string
	json bool
}{
	{"/admin", false},
	{"/admin/api/overview", true},
	{"/admin/api/series?chart=throughput&g=day", true},
}

func isProblem(resp *http.Response) bool {
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json")
}

func TestAdminRoutesRequireAdminOnEveryRoute(t *testing.T) {
	w := adminWorld(t)
	anon := newBrowser(t, w.web.addr)
	bob := w.browserFor("bob")
	alice := w.alice // a plain user, signed in by the harness (its password is harnessPassword)
	mkUser(t, w, "carol", "admin")
	mkUser(t, w, "dave", "admin")
	carol, dave := w.browserFor("carol"), w.browserFor("dave")

	for _, r := range adminRoutes {
		// anonymous
		resp := anon.get(r.path)
		if r.json {
			if resp.StatusCode != 401 || !isProblem(resp) {
				t.Errorf("anonymous %s: %d %s", r.path, resp.StatusCode, resp.Header.Get("Content-Type"))
			}
		} else if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
			t.Errorf("anonymous %s: %d → %q", r.path, resp.StatusCode, resp.Header.Get("Location"))
		}
		// a signed-in non-administrator
		resp = alice.get(r.path)
		if resp.StatusCode != 403 || (r.json && !isProblem(resp)) {
			t.Errorf("user %s: %d", r.path, resp.StatusCode)
		}
		if strings.Contains(alice.body, "instances") && r.json {
			t.Errorf("the 403 of %s carries data: %s", r.path, alice.body)
		}
		// an administrator
		if resp = bob.get(r.path); resp.StatusCode != 200 {
			t.Errorf("admin %s: %d\n%s", r.path, resp.StatusCode, bob.body)
		}
	}
	// A disabled administrator is anonymous at once, on every route.
	if out, errOut, code := runWebAdminBinary(t, w.bin, w.dsn, "", nil, "user", "disable", "carol"); code != 0 {
		t.Fatalf("disable: %d %s%s", code, out, errOut)
	}
	for _, r := range adminRoutes {
		resp := carol.get(r.path)
		if (r.json && resp.StatusCode != 401) || (!r.json && resp.StatusCode != 303) {
			t.Errorf("disabled admin %s: %d", r.path, resp.StatusCode)
		}
	}
	// Demoted between two requests: 403 on the next one, no re-login needed.
	if resp := dave.get("/admin"); resp.StatusCode != 200 {
		t.Fatalf("dave before: %d", resp.StatusCode)
	}
	setRole(t, w, "dave", "user")
	for _, r := range adminRoutes {
		if resp := dave.get(r.path); resp.StatusCode != 403 {
			t.Errorf("demoted %s: %d", r.path, resp.StatusCode)
		}
	}
	// Promoted between two requests: 200 on the next one.
	setRole(t, w, "alice", "admin")
	for _, r := range adminRoutes {
		if resp := alice.get(r.path); resp.StatusCode != 200 {
			t.Errorf("promoted %s: %d", r.path, resp.StatusCode)
		}
	}
}

func TestAPITokenAndRSSTokenNeverReachAdmin(t *testing.T) {
	w := adminWorld(t)
	bob := w.browserFor("bob")
	api, rss := bob.mint("api"), bob.mint("rss")
	anon := newBrowser(t, w.web.addr)
	for _, r := range adminRoutes {
		for name, hdr := range map[string][]string{
			"api token as bearer": {"Authorization", "Bearer " + api},
			"rss token as bearer": {"Authorization", "Bearer " + rss},
			"basic":               {"Authorization", "Basic Ym9iOnB3"},
		} {
			if resp := anon.get(r.path, hdr...); resp.StatusCode == 200 {
				t.Errorf("%s reached %s", name, r.path)
			}
		}
		for _, q := range []string{"token=" + api, "key=" + rss, "api_key=" + api} {
			sep := "?"
			if strings.Contains(r.path, "?") {
				sep = "&"
			}
			if resp := anon.get(r.path + sep + q); resp.StatusCode == 200 {
				t.Errorf("a token in the query (%s) reached %s", q[:6], r.path)
			}
		}
	}
}

func TestAdminLinkVisibleOnlyToAdmins(t *testing.T) {
	w := adminWorld(t)
	bob := w.browserFor("bob")
	for _, page := range []string{"/torrents", "/account", "/api", "/admin"} {
		bob.get(page)
		if !strings.Contains(bob.body, `href="/admin"`) {
			t.Errorf("the admin link is missing on %s for an administrator", page)
		}
		w.alice.get(page)
		if page == "/admin" {
			continue
		}
		if strings.Contains(w.alice.body, `href="/admin"`) {
			t.Errorf("a plain user sees the admin link on %s", page)
		}
	}
	anon := newBrowser(t, w.web.addr)
	anon.get("/login")
	if strings.Contains(anon.body, `href="/admin"`) {
		t.Error("an anonymous visitor sees the admin link")
	}
}

func TestNoAdminDataInHealthzReadyzOrErrors(t *testing.T) {
	w := adminWorld(t)
	sqlExec(t, w.dsn, `UPDATE magnetico_stats.shared_state SET queue_sampled_at = now(), queue_rows = 424242, queue_bytes = 1, quota_used_bytes = 1, quota_max_bytes = 2`)
	anon := newBrowser(t, w.web.addr)
	for _, p := range []string{"/healthz", "/readyz", "/admin/nope", "/admin/api/nope", "/nope"} {
		resp := anon.get(p)
		if strings.Contains(anon.body, "424242") || strings.Contains(strings.ToLower(anon.body), "ingest instance") {
			t.Errorf("%s leaks administrator data (%d)", p, resp.StatusCode)
		}
	}
	if resp := anon.get("/admin/nope"); resp.StatusCode != 404 {
		t.Errorf("an unknown admin path is %d for an anonymous visitor, want 404", resp.StatusCode)
	}
}

func TestAdminResponsesAreNoStoreAndKeepSecurityHeaders(t *testing.T) {
	w := adminWorld(t)
	bob := w.browserFor("bob")
	ref := bob.get("/torrents").Header
	for _, r := range adminRoutes {
		resp := bob.get(r.path)
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") || !strings.Contains(cc, "private") {
			t.Errorf("%s Cache-Control = %q", r.path, cc)
		}
		for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options", "Strict-Transport-Security", "Permissions-Policy", "Cross-Origin-Opener-Policy"} {
			if got, want := resp.Header.Get(h), ref.Get(h); got != want {
				t.Errorf("%s header %s = %q, the site's other pages send %q", r.path, h, got, want)
			}
		}
	}
	if csp := ref.Get("Content-Security-Policy"); csp == "" || strings.Contains(csp, "unsafe") {
		t.Fatalf("site CSP %q", csp)
	}
}

func TestPeriodAndChartParametersAreStrictlyValidated(t *testing.T) {
	w := adminWorld(t)
	bob := w.browserFor("bob")
	for name, q := range map[string]string{
		"missing chart":         "g=day",
		"missing granularity":   "chart=queue",
		"unknown chart":         "chart=nope&g=day",
		"unknown granularity":   "chart=queue&g=week",
		"malformed day":         "chart=queue&g=day&period=2026-13-45",
		"huge period":           "chart=queue&g=day&period=" + strings.Repeat("9", 5000),
		"duplicate chart":       "chart=queue&chart=traffic&g=day",
		"negative-looking year": "chart=queue&g=year&period=-2026",
		"sql in period":         "chart=queue&g=day&period=" + url.QueryEscape("2026-01-01'; DROP TABLE x;--"),
		"null byte":             "chart=queue&g=day&period=2026-01-0%001",
	} {
		resp := bob.get("/admin/api/series?" + q)
		if resp.StatusCode != 400 || !isProblem(resp) {
			t.Errorf("%s: %d %s", name, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if strings.Contains(bob.body, "DROP TABLE") || strings.Contains(strings.ToLower(bob.body), "pgx") || strings.Contains(strings.ToLower(bob.body), "sql") {
			t.Errorf("%s: the error echoes input or internals: %s", name, bob.body)
		}
	}
	if resp := bob.get("/admin/api/series?chart=queue&g=year&period=2001"); resp.StatusCode != 404 {
		t.Errorf("a year before any data: %d", resp.StatusCode)
	}
	for _, g := range []string{"day", "month", "year"} {
		if resp := bob.get("/admin/api/series?chart=queue&g=" + g); resp.StatusCode != 200 {
			t.Errorf("default period for %s: %d", g, resp.StatusCode)
		}
	}
}

func TestBoundedResponseSizesAndTheDashboardShowsWhatIsStored(t *testing.T) {
	w := adminWorld(t)
	// 50 instance rows (the bound) and a shared state, seeded directly.
	sqlExec(t, w.dsn, `INSERT INTO magnetico_stats.snapshot (instance_id, started_at, published_at, window_end, owns_consumer,
 w60_covered_s, w60_records_received, w900_covered_s, w900_records_received, w3600_covered_s, w3600_records_received,
 total_records_received, total_records_acknowledged, total_settled_stored_new)
SELECT gen_random_uuid(), now() - interval '1 hour', now() - (g || ' seconds')::interval, now(), g = 1, 60, 120, 900, 9000, 3600, 36000, 1000, 990, 980
FROM generate_series(1, 60) g`)
	sqlExec(t, w.dsn, `UPDATE magnetico_stats.shared_state SET queue_sampled_at = now(), queue_rows = 1234, queue_bytes = 5678000, quota_used_bytes = 5678000,
 quota_max_bytes = 1000000000, quarantine_rows = 3, quarantine_bytes = 300, oldest_age_s = 75, storage_sampled_at = now(),
 storage_torrents_table_bytes = 1048576, storage_magneticod_total_bytes = 4194304, storage_indexed_torrents_estimate = 1000`)
	bob := w.browserFor("bob")
	resp := bob.get("/admin/api/overview")
	if resp.StatusCode != 200 || len(bob.body) > 64<<10 {
		t.Fatalf("overview JSON: %d, %d bytes", resp.StatusCode, len(bob.body))
	}
	var j struct {
		Schema    string `json:"schema"`
		Instances []struct {
			State string `json:"state"`
		} `json:"instances"`
		Queue struct {
			Rows struct {
				Value *int64 `json:"value"`
				Valid bool   `json:"valid"`
			} `json:"rows"`
			Utilisation struct {
				Value *float64 `json:"value"`
			} `json:"quota_utilisation"`
			Quarantine struct {
				Value *int64 `json:"value"`
			} `json:"quarantine_rows"`
		} `json:"queue"`
		Storage struct {
			PerTorrent struct {
				Value *float64 `json:"value"`
				Valid bool     `json:"valid"`
			} `json:"bytes_per_torrent"`
		} `json:"storage"`
		Rates []struct {
			Values map[string]struct {
				Valid bool `json:"valid"`
			} `json:"values"`
		} `json:"rates"`
	}
	if err := json.Unmarshal([]byte(bob.body), &j); err != nil {
		t.Fatal(err)
	}
	if j.Schema != "up" || len(j.Instances) != 50 || j.Queue.Rows.Value == nil || *j.Queue.Rows.Value != 1234 ||
		j.Queue.Utilisation.Value == nil || *j.Queue.Utilisation.Value < 0.0056 || *j.Queue.Utilisation.Value > 0.0057 ||
		j.Queue.Quarantine.Value == nil || *j.Queue.Quarantine.Value != 3 || !j.Storage.PerTorrent.Valid || *j.Storage.PerTorrent.Value != 4194.304 {
		t.Fatalf("overview JSON: %s", bob.body)
	}
	if !j.Rates[0].Values["received"].Valid {
		t.Fatalf("no rate with 50 fresh instances: %s", bob.body)
	}
	resp = bob.get("/admin")
	for _, want := range []string{"1,234", "5.4 MiB", "75 s", "Not included", "owner", "Administration"} {
		if !strings.Contains(bob.body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
	if resp.StatusCode != 200 || len(bob.body) > 256<<10 {
		t.Fatalf("overview page: %d, %d bytes", resp.StatusCode, len(bob.body))
	}
	resp = bob.get("/admin/api/series?chart=throughput&g=year")
	if resp.StatusCode != 200 || len(bob.body) > 100<<10 {
		t.Fatalf("series: %d, %d bytes", resp.StatusCode, len(bob.body))
	}
}

func TestWebStartsWithoutTheStatsSchemaAndSaysSo(t *testing.T) {
	bin := buildBinary(t)
	dsn := harnessAppAccount(t)
	for _, cmd := range []string{"schema-init", "web-schema-init"} { // no ingest schema, no statistics schema
		if out, code := runWebSchemaInit(t, bin, dsn, cmd); code != 0 {
			t.Fatalf("%s: %d %s", cmd, code, out)
		}
	}
	w := &harnessWorld{t: t, bin: bin, dsn: dsn}
	mkUser(t, w, "bob", "admin")
	w.web = startWeb(t, bin, dsn, "http")
	b := newBrowser(t, w.web.addr)
	if resp := b.login("bob", adminPW); resp.StatusCode != 303 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	// The public site works.
	if resp := b.get("/torrents"); resp.StatusCode != 200 {
		t.Fatalf("/torrents: %d", resp.StatusCode)
	}
	// The dashboard says what is missing instead of failing or showing zeros.
	resp := b.get("/admin")
	if resp.StatusCode != 200 || !strings.Contains(b.body, "has not been created") || !strings.Contains(b.body, "stats-schema-init") {
		t.Fatalf("/admin without the statistics schema: %d\n%s", resp.StatusCode, b.body)
	}
	resp = b.get("/admin/api/overview")
	if resp.StatusCode != 200 || !strings.Contains(b.body, `"schema":"uninitialised"`) {
		t.Fatalf("overview JSON: %d %s", resp.StatusCode, b.body)
	}
	// An incompatible schema is reported, not repaired and not shown as data.
	if out, code := runWebSchemaInit(t, bin, dsn, "stats-schema-init"); code != 0 {
		t.Fatalf("stats-schema-init: %d %s", code, out)
	}
	sqlExec(t, dsn, `ALTER TABLE magnetico_stats.snapshot ADD COLUMN junk text`)
	// The dashboard answer is cached for 10 s: wait for the state change to show.
	deadline := time.Now().Add(25 * time.Second)
	for {
		b.get("/admin")
		if strings.Contains(b.body, "not compatible") && strings.Contains(b.body, "stats-schema-init") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("incompatible schema not explained:\n%s", b.body)
		}
		time.Sleep(500 * time.Millisecond)
	}
	// The same page keeps working for the public site, and the process never exited.
	if resp := b.get("/torrents"); resp.StatusCode != 200 {
		t.Fatalf("/torrents after: %d", resp.StatusCode)
	}
	select {
	case err := <-w.web.exited:
		t.Fatalf("web exited: %v", err)
	default:
	}
	_ = io.Discard
}

// The Content-Security-Policy of the administrator pages is byte-identical to the site's
// and to the value this milestone started from: the charts needed no relaxation.
func TestCSPHeaderIsUnchanged(t *testing.T) {
	w := adminWorld(t)
	bob := w.browserFor("bob")
	const want = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"
	for _, p := range []string{"/admin", "/admin/api/overview", "/torrents"} {
		if got := bob.get(p).Header.Get("Content-Security-Policy"); got != want {
			t.Errorf("%s: CSP %q, want %q", p, got, want)
		}
	}
}

var assetRef = regexp.MustCompile(`(?:src|href)="(/static/[^"]+)"`)

func TestAdminPagesLoadOnlySameOriginAssets(t *testing.T) {
	w := adminWorld(t)
	bob := w.browserFor("bob")
	bob.get("/admin")
	body := bob.body
	if regexp.MustCompile(`(?i)<script[^>]*>[^<]|\son[a-z]+\s*=|\sstyle\s*=|<style`).MatchString(body) {
		t.Error("the rendered page has inline script, inline style or an event handler (the CSP would block them)")
	}
	for _, m := range regexp.MustCompile(`(?:src|href|action)="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		u := m[1]
		if strings.HasPrefix(u, "http") || strings.HasPrefix(u, "//") {
			if !strings.HasPrefix(u, "https://git.skobk.in/") && !strings.HasPrefix(u, "https://github.com/skobkin/") { // the footer's source links
				t.Errorf("the page references another origin: %s", u)
			}
		}
	}
	refs := assetRef.FindAllStringSubmatch(body, -1)
	seen := map[string]bool{}
	for _, m := range refs {
		seen[m[1][strings.LastIndex(m[1], "/")+1:]] = true
		resp := bob.get(m[1])
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d", m[1], resp.StatusCode)
		}
		if !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s is not served immutable", m[1])
		}
	}
	var js, css int
	for name := range seen {
		switch {
		case strings.HasPrefix(name, "uPlot.iife.min.") && strings.HasSuffix(name, ".js"), strings.HasPrefix(name, "admin.") && strings.HasSuffix(name, ".js"):
			js++
		case strings.HasPrefix(name, "uPlot.min.") && strings.HasSuffix(name, ".css"), strings.HasPrefix(name, "admin.") && strings.HasSuffix(name, ".css"):
			css++
		}
	}
	if js != 2 || css != 2 {
		t.Errorf("the page references %d chart scripts and %d chart stylesheets (assets: %v), want 2 and 2", js, css, seen)
	}
	// Five chart cards with a text alternative each; the data is fetched, never embedded.
	if n := strings.Count(body, `data-chart="`); n != 5 {
		t.Errorf("%d chart cards, want 5", n)
	}
	if n := strings.Count(body, `<details class="chart-summary">`); n != 5 {
		t.Errorf("%d text summaries, want one per chart", n)
	}
	if strings.Contains(body, `"buckets"`) {
		t.Error("chart data is embedded in the page")
	}
}
