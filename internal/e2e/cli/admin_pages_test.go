package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

var pageRoutes = []string{"/admin/crawlers", "/admin/quarantine", "/admin/users", "/admin/invitations", "/admin/trackers"}

func pagesWorld(t *testing.T) (*harnessWorld, *webBrowser) {
	t.Helper()
	w := adminWorld(t)
	return w, w.browserFor("bob")
}

func TestCrawlersPageListsNeverConnectedAndDisabledCrawlers(t *testing.T) {
	w, bob := pagesWorld(t)
	tok := map[string]string{}
	for _, n := range []string{"alpha", "bravo", "charlie"} {
		tok[n] = registerCrawler(t, w.bin, w.dsn, n)
	}
	if out, errOut, code := runIngestCrawlerBinary(t, w.bin, w.dsn, nil, "disable", "charlie"); code != 0 {
		t.Fatalf("disable: %d %s%s", code, out, errOut)
	}
	sqlExec(t, w.dsn, `INSERT INTO magnetico_stats.crawler_activity (crawler_id, first_seen_at, last_seen_at, last_ip, last_submit_at, records_received, records_enqueued)
SELECT crawler_id, now() - interval '2 days', now(), '198.51.100.23', now(), 1234, 1200 FROM magnetico_ingest.crawlers WHERE name = 'alpha'`)
	hash := sqlString(t, w.dsn, `SELECT encode(token_hash, 'hex') FROM magnetico_ingest.crawlers WHERE name = 'alpha'`)
	if resp := bob.get("/admin/crawlers"); resp.StatusCode != 200 {
		t.Fatalf("crawlers page: %d", resp.StatusCode)
	}
	for _, want := range []string{"alpha", "bravo", "charlie", "198.51.100.23", "1,234", "never connected", "disabled", "enabled"} {
		if !strings.Contains(bob.body, want) {
			t.Errorf("the crawlers page does not show %q", want)
		}
	}
	// Neither a token nor its digest is ever rendered.
	for n, tk := range tok {
		if strings.Contains(bob.body, tk) || strings.Contains(bob.body, strings.TrimPrefix(tk, "mgc_")) {
			t.Errorf("the page shows the token of %s", n)
		}
	}
	if strings.Contains(bob.body, hash) || strings.Contains(bob.body, "mgc_") || strings.Contains(strings.ToLower(bob.body), "token_hash") {
		t.Error("the page shows a token digest or a token prefix")
	}
}

func TestInvitationsAndUsersNeverExposeCredentialsOrCodes(t *testing.T) {
	w, bob := pagesWorld(t)
	sqlExec(t, w.dsn, `UPDATE magnetico_web.users SET password_hash = '$argon2id$v=19$m=1$SECRETSALT$SECRETHASHVALUE' WHERE username = 'alice'`)
	sqlExec(t, w.dsn, `INSERT INTO magnetico_web.invites (owner_id, code, created_at) SELECT id, 'SECRETINVITATIONCODE' || repeat('z', 10), now() FROM magnetico_web.users WHERE username = 'bob'`)
	sqlExec(t, w.dsn, `INSERT INTO magnetico_web.invites (owner_id, code, used_by, used_at, created_at) SELECT b.id, 'USEDSECRETCODE' || repeat('y', 12), a.id, now(), now() FROM magnetico_web.users b, magnetico_web.users a WHERE b.username = 'bob' AND a.username = 'alice'`)
	for _, p := range []string{"/admin/users", "/admin/invitations"} {
		if resp := bob.get(p); resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		for _, secret := range []string{"SECRETINVITATIONCODE", "USEDSECRETCODE", "SECRETHASHVALUE", "SECRETSALT", "$argon2id"} {
			if strings.Contains(bob.body, secret) {
				t.Errorf("%s leaks %q", p, secret)
			}
		}
	}
	bob.get("/admin/invitations")
	for _, want := range []string{"unused", "used", "alice", "bob"} {
		if !strings.Contains(bob.body, want) {
			t.Errorf("invitations page lacks %q", want)
		}
	}
	bob.get("/admin/users")
	for _, want := range []string{"alice", "bob", "admin", "none"} {
		if !strings.Contains(bob.body, want) {
			t.Errorf("users page lacks %q", want)
		}
	}
}

func TestQuarantinePageShowsReasonAgeAttemptsSizeAndNeverPayloadOrHash(t *testing.T) {
	w, bob := pagesWorld(t)
	sqlExec(t, w.dsn, `INSERT INTO magnetico_ingest.quarantine (info_hash, format, payload, payload_bytes, reason_code, detail, attempts, received_at, quarantined_at)
VALUES (decode('00112233445566778899aabbccddeeff00112233', 'hex'), 1, convert_to('{"name":"SECRETPAYLOADTEXT"}', 'UTF8'), 4096, 'max_attempts', 'attempt limit reached', 10, now() - interval '3 hours', now() - interval '2 hours')`)
	if resp := bob.get("/admin/quarantine"); resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	for _, want := range []string{"max_attempts", "attempt limit reached", "10", "4.0 KiB", "2 h 0 min"} {
		if !strings.Contains(bob.body, want) {
			t.Errorf("the quarantine page lacks %q:\n%s", want, bob.body)
		}
	}
	for _, secret := range []string{"SECRETPAYLOADTEXT", "00112233445566778899aabbccddeeff00112233", "eyJuYW1l"} {
		if strings.Contains(bob.body, secret) {
			t.Errorf("the quarantine page leaks %q", secret)
		}
	}
}

func TestPaginationOfThePagesIsKeysetBoundedAndStable(t *testing.T) {
	w, bob := pagesWorld(t)
	sqlExec(t, w.dsn, `INSERT INTO magnetico_web.users (username) SELECT 'bulk' || lpad(g::text, 4, '0') FROM generate_series(1, 120) g`)
	nextRe := regexp.MustCompile(`href="(/admin/users\?[^"]*after=[^"]+)"`)
	names := map[string]bool{}
	path, pages := "/admin/users", 0
	for path != "" && pages < 10 {
		if resp := bob.get(path); resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		pages++
		for _, m := range regexp.MustCompile(`<td>(bulk\d{4})</td>`).FindAllStringSubmatch(bob.body, -1) {
			if names[m[1]] {
				t.Fatalf("%s listed twice", m[1])
			}
			names[m[1]] = true
		}
		if n := strings.Count(bob.body, "<tr>") - 1; n > 50 {
			t.Fatalf("a page of %d rows", n)
		}
		path = ""
		if m := nextRe.FindStringSubmatch(bob.body); m != nil {
			path = strings.ReplaceAll(m[1], "&amp;", "&")
		}
	}
	if len(names) != 120 || pages != 3 {
		t.Fatalf("%d users over %d pages", len(names), pages)
	}
}

func TestCursorAndQueryAbuseIsRejectedWithoutDetail(t *testing.T) {
	_, bob := pagesWorld(t)
	for name, p := range map[string]string{
		"garbage cursor":      "/admin/users?after=%00%00",
		"negative id":         "/admin/users?after=LTE",
		"huge cursor":         "/admin/users?after=" + strings.Repeat("A", 5000),
		"duplicate cursor":    "/admin/users?after=MQ&after=Mg",
		"sql in cursor":       "/admin/invitations?after=" + url.QueryEscape("1' OR '1'='1"),
		"crawler bad cursor":  "/admin/crawlers?after=" + url.QueryEscape("../../etc/passwd"),
		"quarantine garbage":  "/admin/quarantine?after=not-base64!",
		"search too long":     "/admin/users?q=" + strings.Repeat("a", 65),
		"search duplicate":    "/admin/users?q=a&q=b",
		"search invalid utf8": "/admin/users?q=%ff%fe",
	} {
		resp := bob.get(p)
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
		lower := strings.ToLower(bob.body)
		for _, leak := range []string{"pgx", "syntax error", "sql", "postgres", "' or '", "passwd"} {
			if strings.Contains(lower, leak) {
				t.Errorf("%s: the error page echoes %q", name, leak)
			}
		}
	}
}

func TestUserSearchThroughThePageEscapesWildcards(t *testing.T) {
	w, bob := pagesWorld(t)
	sqlExec(t, w.dsn, `INSERT INTO magnetico_web.users (username) VALUES ('100%sure'), ('plainname')`)
	bob.get("/admin/users?q=" + url.QueryEscape("%"))
	if !strings.Contains(bob.body, "100%sure") || strings.Contains(bob.body, "plainname") {
		t.Errorf("a percent sign matched as a wildcard:\n%s", bob.body)
	}
	bob.get("/admin/users?q=" + url.QueryEscape("_"))
	if strings.Contains(bob.body, "plainname") {
		t.Error("an underscore matched as a wildcard")
	}
	bob.get("/admin/users?q=PLAINNAME")
	if !strings.Contains(bob.body, "plainname") {
		t.Error("the search is not case-insensitive")
	}
}

func TestAllAdminOutputEscapesUntrustedStrings(t *testing.T) {
	w, bob := pagesWorld(t)
	hostile := []string{`<script>alert(1)</script>`, `"><img src=x onerror=alert(2)>`, "\u202eevil\u202c", `'><svg onload=alert(3)>`}
	sqlExec(t, w.dsn, `INSERT INTO magnetico_web.users (username, email) VALUES ($1, $2)`, hostile[0][:25], hostile[1]+"@example.org")
	sqlExec(t, w.dsn, `INSERT INTO magnetico_ingest.quarantine (info_hash, format, payload, payload_bytes, reason_code, detail, attempts, received_at)
VALUES ('\x01', 1, '\x00', 1, 'x', $1, 1, now())`, strings.Join(hostile, " "))
	for _, p := range pageRoutes {
		if resp := bob.get(p); resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		for _, raw := range []string{"<script>alert", "<img src=x onerror", "<svg onload"} {
			if strings.Contains(bob.body, raw) {
				t.Errorf("%s renders markup from untrusted data: %q", p, raw)
			}
		}
	}
	bob.get("/admin/quarantine")
	if !strings.Contains(bob.body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the quarantine detail is not shown escaped")
	}
}

func TestAdminRoutesHaveNoMutationControls(t *testing.T) {
	w, bob := pagesWorld(t)
	registerCrawler(t, w.bin, w.dsn, "alpha")
	for _, p := range append([]string{"/admin"}, pageRoutes...) {
		bob.get(p)
		body := strings.ToLower(bob.body)
		forms := regexp.MustCompile(`<form[^>]*>`).FindAllString(body, -1)
		for _, f := range forms {
			switch {
			case strings.Contains(f, `action="/logout"`): // the site header
			case strings.Contains(f, `method="get"`) && strings.Contains(f, `action="/admin/users"`): // the user search
			default:
				t.Errorf("%s has a form that is neither sign-out nor the search: %s", p, f)
			}
		}
		for _, bad := range []string{`name="csrf_token"`} {
			if strings.Count(body, bad) != 1 { // only the sign-out form's token
				t.Errorf("%s: %d CSRF tokens, want exactly the sign-out form's", p, strings.Count(body, bad))
			}
		}
		if regexp.MustCompile(`(?i)(rotate|disable|enable|revoke|delete|remove|reset|create|add)[^<]{0,12}(token|crawler|invit|user|tracker)`).MatchString(strings.ReplaceAll(body, "magnetico ingest crawler add", "")) {
			if !strings.Contains(p, "/admin/crawlers") && !strings.Contains(p, "/admin/trackers") {
				t.Errorf("%s mentions a mutation", p)
			}
		}
	}
	// No admin route accepts any state-changing method (405, and no data changed).
	before := sqlCount(t, w.dsn, `SELECT count(*) FROM magnetico_web.users`)
	for _, p := range pageRoutes {
		for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
			req, _ := newRequest(m, bob.base+p, "x=1")
			resp := bob.do(req)
			if resp.StatusCode != 405 {
				t.Errorf("%s %s: %d, want 405", m, p, resp.StatusCode)
			}
		}
	}
	if sqlCount(t, w.dsn, `SELECT count(*) FROM magnetico_web.users`) != before {
		t.Error("data changed")
	}
}

func TestTrackersPageIsReadOnlyAndMatchesTheList(t *testing.T) {
	w, bob := pagesWorld(t)
	n := sqlCount(t, w.dsn, `SELECT count(*) FROM magnetico_web.trackers`)
	if resp := bob.get("/admin/trackers"); resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if got := strings.Count(bob.body, "<tr>") - 1; got != n || n < 10 {
		t.Fatalf("%d tracker rows shown, %d stored", got, n)
	}
	if !strings.Contains(bob.body, "udp://") && !strings.Contains(bob.body, "http") {
		t.Error("no tracker URL shown")
	}
	_ = fmt.Sprint
}

func TestCrawlersAndQuarantinePagesExplainAnUnusableIngestSchema(t *testing.T) {
	bin := buildBinary(t)
	dsn := harnessAppAccount(t)
	for _, cmd := range []string{"schema-init", "web-schema-init"} { // no ingest schema at all
		if out, code := runWebSchemaInit(t, bin, dsn, cmd); code != 0 {
			t.Fatalf("%s: %d %s", cmd, code, out)
		}
	}
	w := &harnessWorld{t: t, bin: bin, dsn: dsn}
	mkUser(t, w, "bob", "admin")
	w.web = startWeb(t, bin, dsn, "http")
	bob := newBrowser(t, w.web.addr)
	if resp := bob.login("bob", adminPW); resp.StatusCode != 303 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	check := func(wants ...string) {
		t.Helper()
		for _, p := range []string{"/admin/crawlers", "/admin/quarantine"} {
			resp := bob.get(p)
			if resp.StatusCode != 200 {
				t.Fatalf("%s: %d", p, resp.StatusCode)
			}
			for _, want := range wants {
				if !strings.Contains(bob.body, want) {
					t.Errorf("%s does not say %q:\n%s", p, want, bob.body)
				}
			}
			for _, leak := range []string{"postgres://", "pgx", "SQLSTATE", "password"} {
				if strings.Contains(bob.body, leak) {
					t.Errorf("%s leaks %q", p, leak)
				}
			}
		}
	}
	// 1. missing
	check("does not exist", "ingest-schema-init")
	// The other administrator pages keep working, and so does the public site.
	for _, p := range []string{"/admin", "/admin/users", "/admin/invitations", "/admin/trackers", "/torrents"} {
		if resp := bob.get(p); resp.StatusCode != 200 {
			t.Errorf("%s: %d while the ingest schema is missing", p, resp.StatusCode)
		}
	}
	// 2. incompatible: a development schema without the crawler registry (the probe is cached for 60 s).
	if out, code := runWebSchemaInit(t, bin, dsn, "ingest-schema-init"); code != 0 {
		t.Fatalf("ingest-schema-init: %d %s", code, out)
	}
	sqlExec(t, dsn, `DROP TABLE magnetico_ingest.crawlers`)
	waitFor(t, "the probe to see the incompatible schema", func() bool {
		bob.get("/admin/crawlers")
		return strings.Contains(bob.body, "not compatible")
	})
	check("not compatible", "crawlers", "UPGRADE.md")
}

func newRequest(method, u, body string) (*http.Request, error) {
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return req, err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Second)
	}
}
