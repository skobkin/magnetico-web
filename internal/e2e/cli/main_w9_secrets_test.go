package cli

// W9 secret-leak regression. A secret is a unique value the test created. Each
// kind of secret has intentional delivery channels, which are NOT scanned
// (otherwise legitimate delivery would be a false positive):
//
//	user password                   its own form body (request) only
//	CSRF token                      the hidden field of a page delivered to its own client
//	session id                      Set-Cookie only
//	API / RSS token (new)           the response that creates it, once
//	legacy RSS key                  the migration, never a page
//	invitation code                 the owner's invitations page (and the link it is copied from)
//	password setup/reset/email link the CLI stdout that issued it, the email, the link's own page
//	database / SMTP password        none
//	sensitive email address         the owner's profile page
//
// Everything else is scanned: the service log, error and redirect responses,
// every response header except the session Set-Cookie, the pages of another
// user, and the diagnostics of failing CLI commands. Negative controls prove
// the scanner finds a planted secret in each kind of location.

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

type leakScan struct {
	t       *testing.T
	secrets []leakSecret
	found   []string
	checked int
}

type leakSecret struct{ name, value string }

func newLeakScan(t *testing.T) *leakScan { return &leakScan{t: t} }

// add registers a secret. Values shorter than 8 bytes would match by accident.
func (l *leakScan) add(name, value string) {
	l.t.Helper()
	if len(value) < 8 {
		l.t.Fatalf("secret %q is too short to scan for (%d bytes)", name, len(value))
	}
	l.secrets = append(l.secrets, leakSecret{name, value})
}

// forms returns the plain value and the encodings a server could echo.
func forms(v string) []string {
	return []string{v, url.QueryEscape(v), url.PathEscape(v), html.EscapeString(v)}
}

// check scans text found at location for every secret except those named in
// allowed (the secret's own intentional channel).
func (l *leakScan) check(location, text string, allowed ...string) {
	l.t.Helper()
	l.checked++
	for _, s := range l.secrets {
		skip := false
		for _, a := range allowed {
			if a == s.name {
				skip = true
			}
		}
		if skip {
			continue
		}
		for _, f := range forms(s.value) {
			if strings.Contains(text, f) {
				l.found = append(l.found, location+": "+s.name)
				break
			}
		}
	}
}

// checkResp scans a response: the body and every header (Set-Cookie too,
// except for the session id, whose channel it is).
func (l *leakScan) checkResp(location string, status int, h http.Header, body string, allowed ...string) {
	l.t.Helper()
	l.check(fmt.Sprintf("%s [%d] body", location, status), body, allowed...)
	for name, vals := range h {
		a := allowed
		if strings.EqualFold(name, "Set-Cookie") {
			a = append(append([]string(nil), allowed...), "session-alice", "session-bob", "csrf-pre-cookie")
		}
		l.check(fmt.Sprintf("%s [%d] header %s", location, status, name), strings.Join(vals, "\n"), a...)
	}
}

func (l *leakScan) report() {
	l.t.Helper()
	if len(l.found) > 0 {
		l.t.Errorf("secrets in unintended locations (%d locations scanned):\n  %s", l.checked, strings.Join(l.found, "\n  "))
	}
}

// The scanner itself: a planted secret is found in every kind of location, in
// plain and encoded form, and an allowed channel is not reported.
func TestW9LeakScannerDetectsPlantedSecrets(t *testing.T) {
	probe := func(plant func(l *leakScan, secret string)) bool {
		l := &leakScan{t: t}
		const secret = "planted-secret-value-0123456789"
		l.add("planted", secret)
		plant(l, secret)
		return len(l.found) > 0
	}
	for name, plant := range map[string]func(*leakScan, string){
		"log line":      func(l *leakScan, s string) { l.check("log", "level=INFO msg=x token="+s) },
		"response body": func(l *leakScan, s string) { l.checkResp("r", 400, http.Header{}, "<p>"+s+"</p>") },
		"response header": func(l *leakScan, s string) {
			l.checkResp("r", 200, http.Header{"Location": {"/x?t=" + s}}, "")
		},
		"query-escaped":     func(l *leakScan, s string) { l.check("p", url.QueryEscape(s)) },
		"html-escaped":      func(l *leakScan, s string) { l.check("p", html.EscapeString(s)) },
		"other cookie":      func(l *leakScan, s string) { l.checkResp("r", 200, http.Header{"Set-Cookie": {"other=" + s}}, "") },
		"CLI diagnostics":   func(l *leakScan, s string) { l.check("cli stderr", "error: bad value "+s) },
		"another user page": func(l *leakScan, s string) { l.check("bob account", "<code>"+s+"</code>") },
	} {
		if !probe(plant) {
			t.Errorf("a planted secret in a %s was not detected", name)
		}
	}
	// An intentional channel is not a finding.
	l := &leakScan{t: t}
	l.add("session-alice", "session-cookie-value-123456")
	l.checkResp("login", 303, http.Header{"Set-Cookie": {"magnetico_session=session-cookie-value-123456; HttpOnly"}}, "")
	if len(l.found) != 0 {
		t.Errorf("the session id in Set-Cookie was reported: %v", l.found)
	}
	l.checkResp("login", 303, http.Header{"Location": {"/x?s=session-cookie-value-123456"}}, "")
	if len(l.found) != 1 {
		t.Errorf("the session id in a Location header was not reported: %v", l.found)
	}
}

func TestW9NoSecretsInUnintendedLocations(t *testing.T) {
	s := newW9Sec(t, nil)
	s.addUser("bob", "w9-bob-security-password-9", "")
	l := newLeakScan(t)

	const dbPassword = "w9-role-secret-pw"
	l.add("db-password", dbPassword)
	l.add("pw-alice", secUserPW)
	l.add("pw-bob", "w9-bob-security-password-9")
	sqlExec(t, s.dsn, `UPDATE magnetico_web.users SET email = 'alice-sensitive@example.org' WHERE username = 'alice'`)
	l.add("email-alice", "alice-sensitive@example.org")

	// Sign in; collect the secrets of both users.
	anon := newBrowser(t, s.web.addr)
	anon.get("/login")
	l.add("csrf-pre", anon.token())
	alice := s.login("alice", secUserPW)
	alice.get("/account")
	l.add("csrf-alice", alice.token())
	bob := s.login("bob", "w9-bob-security-password-9")
	bob.get("/account")
	l.add("csrf-bob", bob.token())
	for user, b := range map[string]*webBrowser{"alice": alice, "bob": bob} {
		for _, c := range b.c.Jar.Cookies(&url.URL{Scheme: "http", Host: s.web.addr}) {
			if strings.Contains(c.Name, "session") {
				l.add("session-"+user, c.Value)
			}
		}
	}
	api, rss := alice.mint("api"), alice.mint("rss")
	l.add("api-token", api)
	l.add("rss-token", rss)
	const legacy = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"
	legacyRSSToken(t, s.dsn, "alice", legacy)
	l.add("legacy-rss", legacy)
	if _, errOut, code := runWebAdminBinary(t, s.bin, s.dsn, "", nil, "user", "invite", "add", "alice"); code != 0 {
		t.Fatalf("invite add: %d %s", code, errOut)
	}
	invite := sqlString(t, s.dsn, `SELECT code FROM magnetico_web.invites i JOIN magnetico_web.users u ON u.id = i.owner_id WHERE u.username = 'alice' ORDER BY i.id LIMIT 1`)
	l.add("invite-code", invite)
	reset, _, code := runWebAdminBinary(t, s.bin, s.dsn, "", nil, "user", "reset-password", "alice")
	if code != 0 {
		t.Fatalf("reset-password: %d", code)
	}
	resetTok := tokenInURL.FindString(reset)
	if resetTok == "" {
		t.Fatalf("no link token in %q", reset)
	}
	l.add("reset-link", resetTok)
	// Wrong passwords that were sent in request bodies.
	const wrong1, wrong2 = "wrong-password-w9-aaaa", "wrong-password-w9-bbbb"
	l.add("pw-wrong1", wrong1)
	l.add("pw-wrong2", wrong2)

	obs := func(loc string, r secResp, allowed ...string) { l.checkResp(loc, r.status, r.hdr, r.body, allowed...) }

	// Failed and odd requests that carry secrets. Their responses are scanned.
	for i, pw := range []string{wrong1, wrong2} {
		b := newBrowser(t, s.web.addr)
		b.get("/login")
		resp := b.post("/login", url.Values{"username": {"alice"}, "password": {pw}, "csrf_token": {b.token()}})
		l.checkResp(fmt.Sprintf("failed login %d", i), resp.StatusCode, resp.Header, b.body)
		// The failure page is a form again: its own fresh CSRF token is its channel.
	}
	// A missing CSRF token with a password in the body: the refusal page.
	{
		b := newBrowser(t, s.web.addr)
		resp := b.post("/login", url.Values{"username": {"alice"}, "password": {secUserPW}})
		l.checkResp("login without token", resp.StatusCode, resp.Header, b.body)
	}
	// Credentials in every transport against every credentialed surface.
	for _, tok := range []string{api, rss, legacy} {
		for _, p := range []string{"/api/v2/torrents?token=" + tok, "/api/v2/torrents?api-token=" + tok, "/api/v2/feeds/latest.rss?api-token=" + tok, "/api/v1/feed/rss/last?feed_token=" + tok, "/api/v2/torrents/1/files?access_token=" + tok} {
			obs("GET "+strings.SplitN(p, "?", 2)[0], s.raw("GET", p, nil, ""))
		}
		obs("bearer on the feed", s.raw("GET", "/api/v2/feeds/latest.rss", bearerHdr(tok), ""))
		obs("api-token header on v2", s.raw("GET", "/api/v2/torrents", map[string]string{"api-token": tok}, ""))
		obs("api-token cookie on v2", s.raw("GET", "/api/v2/torrents", map[string]string{"Cookie": "api-token=" + tok}, ""))
	}
	// The valid feed requests: the document and the headers must not echo the credential.
	obs("v2 feed", s.raw("GET", "/api/v2/feeds/latest.rss?feed_token="+rss, nil, ""))
	obs("v1 feed header", s.raw("GET", "/api/v1/feed/rss/last", map[string]string{"api-token": rss}, ""))
	obs("v1 feed cookie", s.raw("GET", "/api/v1/feed/rss/last", map[string]string{"Cookie": "api-token=" + rss}, ""))
	obs("v1 feed legacy", s.raw("GET", "/api/v1/feed/rss/last?api-token="+legacy, nil, ""))
	obs("api list", s.raw("GET", "/api/v2/torrents", bearerHdr(api), ""))
	// Secrets in URL paths of pages that must not echo them back.
	// (The public /magnet/<hash> redirect intentionally carries the hash it was
	// asked for, so it is not among them.)
	for _, p := range []string{"/register/" + strings.Repeat("A", 22), "/password/reset/" + strings.Repeat("b", 43), "/password/setup/" + strings.Repeat("c", 43), "/email/verify/" + strings.Repeat("d", 43), "/magnet/" + strings.Repeat("f", 39)} {
		l.add("path:"+p, p[strings.LastIndex(p, "/")+1:])
		obs("GET "+p, s.raw("GET", p, nil, "")) // no echo of the path segment in a body or header
	}
	// Another user's pages: none of alice's secrets (nor her address, invitation
	// code, tokens or CSRF token) appear, on any session page.
	bobOwn := []string{"pw-bob", "csrf-bob", "session-bob"} // bob's own secrets on his own pages
	for _, p := range []string{"/account", "/account?tab=invitations", "/account?tab=rss", "/account?tab=api", "/torrents", "/api", "/api/v2"} {
		resp := bob.get(p)
		l.checkResp("bob "+p, resp.StatusCode, resp.Header, bob.body, bobOwn...)
	}
	// And the reverse for the pre-session token and bob's secrets on alice's pages.
	for _, p := range []string{"/account", "/account?tab=rss", "/torrents"} {
		resp := alice.get(p)
		l.checkResp("alice "+p, resp.StatusCode, resp.Header, alice.body,
			"pw-alice", "csrf-alice", "session-alice", "email-alice", "invite-code", "api-token", "rss-token") // her own page may show her own address and invitation link
	}
	// Failing operator commands: stderr (and the stdout of commands that issue no
	// secret) must not repeat what was piped to stdin or any other secret.
	for _, c := range []struct {
		stdin string
		args  []string
	}{
		{wrong1 + "\n", []string{"user", "set-password", "nobody-exists"}},
		{"short\n", []string{"user", "set-password", "alice"}},
		{"", []string{"user", "disable", "nobody-exists"}},
		{"", []string{"user", "reset-password", "nobody-exists"}},
		{"", []string{"user", "invite", "revoke", "999999"}},
		{"", []string{"user", "list"}},
	} {
		out, errOut, _ := runWebAdminBinary(t, s.bin, s.dsn, c.stdin, nil, c.args...)
		l.check("CLI stdout "+strings.Join(c.args, " "), out, "email-alice") // `user list` shows the operator the address
		l.check("CLI stderr "+strings.Join(c.args, " "), errOut)
	}
	// Reset-link completion: the log must not hold the link token, the new
	// password, or the CSRF token of the form.
	fresh := newBrowser(t, s.web.addr)
	link := "/password/reset/" + resetTok
	fresh.get(link)
	l.add("csrf-reset-form", fresh.token())
	const newPW = "w9-reset-new-password-5"
	l.add("pw-new", newPW)
	resp := fresh.post(link, url.Values{"password": {newPW}, "password2": {newPW}, "new_password": {newPW}, "new_password2": {newPW}, "csrf_token": {fresh.token()}})
	l.checkResp("reset completion", resp.StatusCode, resp.Header, fresh.body, "reset-link", "csrf-reset-form", "csrf-pre-cookie")

	// The service log, scanned last: every request above passed through it.
	logs := s.web.logs.String()
	if !strings.Contains(logs, "msg=") || len(logs) < 1000 {
		t.Fatalf("the captured log is suspiciously small (%d bytes)", len(logs))
	}
	l.check("service log", logs)
	l.report()

	// Negative control on the real log: the scan finds a planted secret in it.
	control := &leakScan{t: t}
	control.add("planted", "planted-log-secret-424242")
	control.check("service log", logs+"\nlevel=INFO msg=x token=planted-log-secret-424242")
	if len(control.found) != 1 {
		t.Error("the scanner missed a planted secret in the service log")
	}
}

var tokenInURL = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)
