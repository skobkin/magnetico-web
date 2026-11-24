package main

// W9 security regression: credential separation, account-state lifecycle and
// session expiry, against the in-process production wiring (see
// main_w9_security_test.go for the model).

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/opflags"
)

func sqlString(t *testing.T, dsn, q string, args ...any) string {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var s string
	if err := c.QueryRow(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

var tokenInPage = regexp.MustCompile(`<code>(m[gt][tf]_[A-Za-z0-9_-]+)</code>`)

// mint creates an API ("api") or RSS ("rss") token through the account page.
func (b *webBrowser) mint(kind string) string {
	b.t.Helper()
	tab, path := "api", "/account/api-tokens"
	if kind == "rss" {
		tab, path = "rss", "/account/rss-tokens"
	}
	b.get("/account?tab=" + tab)
	if resp := b.post(path, url.Values{"label": {"w9"}, "csrf_token": {b.token()}}); resp.StatusCode != 200 {
		b.t.Fatalf("mint %s: %d\n%s", kind, resp.StatusCode, b.body)
	}
	m := tokenInPage.FindStringSubmatch(b.body)
	if m == nil {
		b.t.Fatalf("no %s token revealed", kind)
	}
	return m[1]
}

// legacyRSSToken plants what the user migration produces: a legacy RSS token,
// stored as its SHA-256 only, owned by the user.
func legacyRSSToken(t *testing.T, dsn, user, key string) {
	t.Helper()
	sqlExec(t, dsn, `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy)
	    SELECT $1, id, true FROM magnetico_web.users WHERE username = $2`, accounts.TokenDigest(key), user)
}

// --- credential separation ---

func TestW9CredentialSeparationMatrix(t *testing.T) {
	s := newW9Sec(t, trustLoopbackProxy)
	s.uniqueClients = true
	b := s.login("alice", secUserPW)
	api, rss := b.mint("api"), b.mint("rss")
	const legacy = "0123456789abcdef0123456789abcdef"
	legacyRSSToken(t, s.dsn, "alice", legacy)
	session := ""
	for _, c := range b.c.Jar.Cookies(&url.URL{Scheme: "http", Host: s.web.addr}) {
		if strings.Contains(c.Name, "session") {
			session = c.Name + "=" + c.Value
		}
	}
	if session == "" {
		t.Fatal("no session cookie")
	}

	status := func(path string, hdr map[string]string) int { return s.raw("GET", path, hdr, "").status }
	want := func(what string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s: %d, want %d", what, got, want)
		}
	}

	// API v2 (every JSON route): the API token as Bearer only.
	for _, p := range []string{"/api/v2/torrents", "/api/v2/torrents/424242", "/api/v2/torrents/424242/files", "/api/v2/torrents/424242/files/tree"} {
		okStatus := 200
		if p != "/api/v2/torrents" {
			okStatus = 404 // authenticated: the torrent does not exist
		}
		want("API token as Bearer on "+p, status(p, bearerHdr(api)), okStatus)
		for name, hdr := range map[string]map[string]string{
			"RSS token as Bearer":            bearerHdr(rss),
			"legacy token as Bearer":         bearerHdr(legacy),
			"session cookie":                 {"Cookie": session},
			"API token in api-token header":  {"api-token": api},
			"API token in api-token cookie":  {"Cookie": "api-token=" + api},
			"no credential":                  nil,
			"Basic scheme":                   {"Authorization": "Basic " + api},
			"token without a scheme":         {"Authorization": api},
			"Bearer with two spaces":         {"Authorization": "Bearer  " + api},
			"two Authorization-like headers": {"Authorization": "Bearer " + api + ", Bearer " + api},
		} {
			want(name+" on "+p, status(p, hdr), 401)
		}
		for _, q := range []string{"api-token", "token", "access_token", "feed_token", "api_key"} {
			want("API token in ?"+q+"= on "+p, status(p+"?"+q+"="+api, nil), 401)
		}
	}

	// The v2 feed: the RSS token in feed_token (exactly once), nothing else.
	feed := "/api/v2/feeds/latest.rss"
	want("new RSS token on the v2 feed", status(feed+"?feed_token="+rss, nil), 200)
	want("legacy RSS token on the v2 feed", status(feed+"?feed_token="+legacy, nil), 200)
	want("two feed_token parameters", status(feed+"?feed_token="+rss+"&feed_token="+rss, nil), 401)
	want("API token as feed_token", status(feed+"?feed_token="+api, nil), 401)
	want("RSS token as Bearer on the v2 feed", status(feed, bearerHdr(rss)), 401)
	want("API token as Bearer on the v2 feed", status(feed, bearerHdr(api)), 401)
	want("RSS token as api-token query on the v2 feed", status(feed+"?api-token="+rss, nil), 401)
	want("RSS token in the api-token header on the v2 feed", status(feed, map[string]string{"api-token": rss}), 401)
	want("session cookie on the v2 feed", status(feed, map[string]string{"Cookie": session}), 401)

	// The v1 feed (deprecated): api-token by header, cookie or query; both RSS
	// token kinds; never an API token or another parameter.
	v1 := "/api/v1/feed/rss/last"
	for _, tok := range []struct{ name, val string }{{"new RSS token", rss}, {"legacy token", legacy}} {
		want(tok.name+" in the query", status(v1+"?api-token="+tok.val, nil), 200)
		want(tok.name+" in the header", status(v1, map[string]string{"api-token": tok.val}), 200)
		want(tok.name+" in the cookie", status(v1, map[string]string{"Cookie": "api-token=" + tok.val}), 200)
		want(tok.name+" as Bearer", status(v1, bearerHdr(tok.val)), 401)
		want(tok.name+" as feed_token", status(v1+"?feed_token="+tok.val, nil), 401)
	}
	for name, hdr := range map[string]map[string]string{
		"API token in the header": {"api-token": api}, "API token in the cookie": {"Cookie": "api-token=" + api},
		"API token as Bearer": bearerHdr(api), "session cookie": {"Cookie": session}, "no credential": nil,
	} {
		want(name+" on the v1 feed", status(v1, hdr), 401)
	}
	want("API token in the query of the v1 feed", status(v1+"?api-token="+api, nil), 401)
	// Precedence: header, then cookie, then query; the first present wins and is
	// not retried with the next.
	want("bad header + good query", status(v1+"?api-token="+rss, map[string]string{"api-token": "mgf_" + strings.Repeat("Z", 43)}), 401)
	want("good header + bad query", status(v1+"?api-token=mgf_"+strings.Repeat("Z", 43), map[string]string{"api-token": rss}), 200)
	want("bad cookie + good query", status(v1+"?api-token="+rss, map[string]string{"Cookie": "api-token=mgf_" + strings.Repeat("Z", 43)}), 401)
	want("good cookie + bad query", status(v1+"?api-token=mgf_"+strings.Repeat("Z", 43), map[string]string{"Cookie": "api-token=" + rss}), 200)
	want("bad header + good cookie", status(v1, map[string]string{"api-token": "mgf_" + strings.Repeat("Z", 43), "Cookie": "api-token=" + rss}), 401)

	// The credential is never reflected in a feed, whatever carried it.
	for name, req := range map[string]struct {
		path string
		hdr  map[string]string
		tok  string
	}{
		"v2 query":  {feed + "?feed_token=" + rss, nil, rss},
		"v1 query":  {v1 + "?api-token=" + rss, nil, rss},
		"v1 header": {v1, map[string]string{"api-token": rss}, rss},
		"v1 cookie": {v1, map[string]string{"Cookie": "api-token=" + rss}, rss},
		"legacy v1": {v1 + "?api-token=" + legacy, nil, legacy},
	} {
		r := s.raw("GET", req.path, req.hdr, "")
		if r.status != 200 || strings.Contains(r.body, req.tok) {
			t.Errorf("%s: status %d, credential reflected: %v", name, r.status, strings.Contains(r.body, req.tok))
		}
		for h, v := range r.hdr {
			if strings.Contains(strings.Join(v, ","), req.tok) {
				t.Errorf("%s: the credential appears in the response header %s", name, h)
			}
		}
		if r.hdr.Get("Referrer-Policy") != "no-referrer" || r.hdr.Get("Cache-Control") != "private, no-store" {
			t.Errorf("%s: Referrer-Policy %q, Cache-Control %q", name, r.hdr.Get("Referrer-Policy"), r.hdr.Get("Cache-Control"))
		}
	}

	// Session-only JSON endpoints and pages ignore bearer and RSS credentials.
	for _, p := range []string{"/torrents/424242/files", "/torrents/424242/files/tree"} {
		for name, hdr := range map[string]map[string]string{"API token as Bearer": bearerHdr(api), "RSS token as Bearer": bearerHdr(rss), "api-token header": {"api-token": rss}} {
			want(name+" on "+p, status(p, hdr), 401)
		}
		want("session cookie on "+p, status(p, map[string]string{"Cookie": session}), 404)
	}
	for _, p := range []string{"/account", "/torrents"} {
		r := s.raw("GET", p, bearerHdr(api), "")
		if r.status != 303 || !strings.HasPrefix(r.hdr.Get("Location"), "/login") {
			t.Errorf("API token as Bearer on %s: %d %q, want the sign-in redirect", p, r.status, r.hdr.Get("Location"))
		}
	}
}

// --- account state and credential lifecycle ---

func TestW9AccountStateAndCredentialLifecycle(t *testing.T) {
	s := newW9Sec(t, nil)
	cli := func(args ...string) string {
		t.Helper()
		out, errOut, code := runWebAdminBinary(t, s.bin, s.dsn, "", nil, append([]string{"user"}, args...)...)
		if code != 0 {
			t.Fatalf("web user %v: %d %s%s", args, code, out, errOut)
		}
		return out
	}
	b := s.login("alice", secUserPW)
	api, rss := b.mint("api"), b.mint("rss")
	legacy := "fedcba9876543210fedcba9876543210"
	legacyRSSToken(t, s.dsn, "alice", legacy)
	cli("invite", "add", "alice")
	code := sqlString(t, s.dsn, `SELECT code FROM magnetico_web.invites i JOIN magnetico_web.users u ON u.id = i.owner_id WHERE u.username = 'alice' ORDER BY i.id LIMIT 1`)

	feed := "/api/v2/feeds/latest.rss?feed_token="
	v1 := "/api/v1/feed/rss/last?api-token="
	probe := func() (session, apiT, rssT, legacyT int, invite string) {
		t.Helper()
		session = b.get("/account").StatusCode
		apiT = s.raw("GET", "/api/v2/torrents", bearerHdr(api), "").status
		rssT = s.raw("GET", feed+rss, nil, "").status
		legacyT = s.raw("GET", v1+legacy, nil, "").status
		reg := newBrowser(t, s.web.addr)
		reg.get("/register/" + code)
		switch {
		case strings.Contains(reg.body, "This invitation is invalid or no longer available."):
			invite = "invalid"
		case strings.Contains(reg.body, `name="username"`):
			invite = "valid"
		default:
			invite = "unknown"
		}
		return
	}
	expect := func(step string, wantSession, wantAPI, wantRSS, wantLegacy int, wantInvite string) {
		t.Helper()
		ss, a, r, l, inv := probe()
		if ss != wantSession || a != wantAPI || r != wantRSS || l != wantLegacy || inv != wantInvite {
			t.Errorf("%s: session %d api %d rss %d legacy %d invite %s; want %d %d %d %d %s", step, ss, a, r, l, inv, wantSession, wantAPI, wantRSS, wantLegacy, wantInvite)
		}
	}
	expect("baseline", 200, 200, 200, 200, "valid")

	// Disable: the session is gone for good, the credentials and the invitation
	// are suspended, and signing in is refused with the generic failure.
	cli("disable", "alice")
	expect("disabled", 303, 401, 401, 401, "invalid")
	attempt := newBrowser(t, s.web.addr)
	if resp := attempt.login("alice", secUserPW); resp.StatusCode == 303 {
		t.Fatal("a disabled account signed in")
	}
	// Enable: tokens and the invitation return, the session does NOT.
	cli("enable", "alice")
	expect("re-enabled", 303, 200, 200, 200, "valid")
	b = s.login("alice", secUserPW)
	expect("signed in again", 200, 200, 200, 200, "valid")

	// An explicitly revoked token stays revoked across disable and enable.
	b.get("/account?tab=api")
	apiID := regexp.MustCompile(`/account/api-tokens/(\d+)/revoke`).FindStringSubmatch(b.body)
	if apiID == nil {
		t.Fatal("no API token to revoke on the page")
	}
	if resp := b.post("/account/api-tokens/"+apiID[1]+"/revoke", url.Values{"csrf_token": {b.token()}}); resp.StatusCode != 303 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	expect("API token revoked", 200, 401, 200, 200, "valid")
	cli("disable", "alice")
	cli("enable", "alice")
	b = s.login("alice", secUserPW)
	expect("revoked token after disable/enable", 200, 401, 200, 200, "valid")

	// revoke-tokens revokes every API and RSS token (legacy included) for good
	// and leaves sessions and invitations alone.
	cli("revoke-tokens", "alice")
	expect("revoke-tokens", 200, 401, 401, 401, "valid")
	cli("disable", "alice")
	cli("enable", "alice")
	b = s.login("alice", secUserPW)
	expect("revoked tokens after disable/enable", 200, 401, 401, 401, "valid")

	// A password reset revokes sessions and both token kinds, legacy included.
	api, rss = b.mint("api"), b.mint("rss")
	legacy2 := "00112233445566778899aabbccddeeff"
	legacyRSSToken(t, s.dsn, "alice", legacy2)
	legacy = legacy2
	expect("fresh credentials", 200, 200, 200, 200, "valid")
	out := cli("reset-password", "alice")
	m := regexp.MustCompile(`/password/(reset|setup)/([A-Za-z0-9_-]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no reset link in the CLI output")
	}
	link := "/password/" + m[1] + "/" + m[2]
	fresh := newBrowser(t, s.web.addr)
	if resp := fresh.get(link); resp.StatusCode != 200 {
		t.Fatalf("reset page: %d", resp.StatusCode)
	}
	const newPW = "w9-new-security-password-3"
	if resp := fresh.post(link, url.Values{"password": {newPW}, "password2": {newPW}, "new_password": {newPW}, "new_password2": {newPW}, "csrf_token": {fresh.token()}}); resp.StatusCode != 303 {
		t.Fatalf("complete reset: %d\n%s", resp.StatusCode, fresh.body)
	}
	expect("after the reset", 303, 401, 401, 401, "valid")
	// The link cannot be replayed, and the old password no longer works.
	replay := newBrowser(t, s.web.addr)
	replay.get(link)
	if resp := replay.post(link, url.Values{"password": {"another-password-4-xx"}, "password2": {"another-password-4-xx"}, "new_password": {"another-password-4-xx"}, "new_password2": {"another-password-4-xx"}, "csrf_token": {replay.token()}}); resp.StatusCode == 303 {
		t.Fatal("a used reset link completed a second time")
	}
	if resp := newBrowser(t, s.web.addr).login("alice", secUserPW); resp.StatusCode == 303 {
		t.Fatal("the old password still signs in")
	}
	if resp := newBrowser(t, s.web.addr).login("alice", newPW); resp.StatusCode != 303 {
		t.Fatalf("the new password does not sign in: %d", resp.StatusCode)
	}
}

// --- session expiry, idle timeout, remember-me, cookie attributes ---

func TestW9SessionExpiryIdleAndRemember(t *testing.T) {
	// The shortest permitted idle timeout (5 minutes), so a moved timestamp is
	// decisive; the other lifetimes are short but ordered (idle <= absolute <=
	// remember).
	s := newW9Sec(t, func(o *opflags.WebOptions) {
		o.SessionIdleTimeout, o.SessionAbsoluteLifetime, o.SessionRememberLifetime = 5*time.Minute, 24*time.Hour, 48*time.Hour
	})
	signIn := func(remember bool) (*webBrowser, *http.Response) {
		t.Helper()
		b := newBrowser(t, s.web.addr)
		b.get("/login")
		f := url.Values{"username": {"alice"}, "password": {secUserPW}, "csrf_token": {b.token()}}
		if remember {
			f.Set("remember", "1")
		}
		resp := b.post("/login", f)
		if resp.StatusCode != 303 {
			t.Fatalf("login: %d\n%s", resp.StatusCode, b.body)
		}
		return b, resp
	}
	valid := func(b *webBrowser) bool { return b.get("/account").StatusCode == 200 }
	moveAll := func(set string) {
		t.Helper()
		sqlExec(t, s.dsn, `UPDATE magnetico_web.sessions SET `+set)
	}

	// Cookie attributes in development mode (production mode is covered by
	// TestWebBinaryProductionCookieAttributes): HttpOnly, SameSite=Lax, Path=/,
	// no Domain, no Secure, no __Host- prefix; a session cookie without an
	// expiry for an ordinary session and with one for "remember me".
	cookieOf := func(resp *http.Response) string {
		for _, c := range resp.Header["Set-Cookie"] {
			if strings.HasPrefix(c, "magnetico_session=") || strings.Contains(c, "_session=") {
				return c
			}
		}
		t.Fatalf("no session cookie in %v", resp.Header["Set-Cookie"])
		return ""
	}
	ordinary, resp := signIn(false)
	c := cookieOf(resp)
	for _, want := range []string{"HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(c, want) {
			t.Errorf("ordinary cookie lacks %s: %s", want, c)
		}
	}
	for _, bad := range []string{"Domain=", "Secure", "__Host-", "Expires=", "Max-Age="} {
		if strings.Contains(c, bad) {
			t.Errorf("ordinary development cookie contains %s: %s", bad, c)
		}
	}
	remember, rresp := signIn(true)
	rc := cookieOf(rresp)
	if !strings.Contains(rc, "Expires=") && !strings.Contains(rc, "Max-Age=") {
		t.Errorf("remember-me cookie has no expiry: %s", rc)
	}
	for _, bad := range []string{"Domain=", "Secure", "__Host-"} {
		if strings.Contains(rc, bad) {
			t.Errorf("remember-me development cookie contains %s: %s", bad, rc)
		}
	}
	if n := sqlCount(t, s.dsn, `SELECT count(*) FROM magnetico_web.sessions WHERE expires_at IS NULL`); n != 0 {
		t.Fatalf("%d sessions without an absolute expiry", n)
	}
	if n := sqlCount(t, s.dsn, `SELECT count(*) FROM magnetico_web.sessions WHERE remember`); n != 1 {
		t.Fatalf("remember sessions: %d", n)
	}

	// Idle timeout: an ordinary session unused for less than the window is
	// valid, for longer is not. A remember-me session is not subject to it.
	moveAll(`last_seen_at = now() - interval '4 minutes'`)
	if !valid(ordinary) {
		t.Fatal("an ordinary session idle for 4 of 5 minutes was refused")
	}
	moveAll(`last_seen_at = now() - interval '6 minutes'`)
	if valid(ordinary) {
		t.Fatal("an ordinary session idle for 6 of 5 minutes was accepted")
	}
	moveAll(`last_seen_at = now() - interval '30 days'`)
	if !valid(remember) {
		t.Fatal("a remember-me session was refused for idleness (its lifetime is absolute)")
	}

	// Absolute expiry: an ordinary session is capped even when in constant use,
	// and a remember-me session ends at its absolute lifetime, not extended by
	// activity.
	ordinary, _ = signIn(false)
	moveAll(`created_at = now() - interval '2 days', last_seen_at = now(), expires_at = now() - interval '1 minute' WHERE NOT remember`)
	if valid(ordinary) {
		t.Fatal("an ordinary session past its absolute lifetime was accepted")
	}
	if hours := sqlString(t, s.dsn, `SELECT round(extract(epoch FROM expires_at - created_at) / 3600)::text FROM magnetico_web.sessions WHERE remember`); hours != "48" {
		t.Errorf("remember-me lifetime = %s h, want 48 h", hours)
	}
	if !valid(remember) {
		t.Fatal("the remember-me session ended early")
	}
	moveAll(`created_at = now() - interval '3 days', expires_at = now() - interval '1 minute' WHERE remember`)
	if valid(remember) {
		t.Fatal("a remember-me session past its absolute lifetime was accepted")
	}
	// Expired rows are not usable even before the purger runs, and never revive.
	if valid(remember) || valid(ordinary) {
		t.Fatal("an expired session came back")
	}
}

// --- authentication failures are indistinguishable ---

// An unknown name, a wrong password, an account that has no password yet, one
// whose stored hash format is unsupported ("needs reset") and a disabled one
// all fail with the same status and the same page, and none of them gets a
// session; the failures also share one rate-limit bucket per address.
func TestW9AuthenticationFailuresAreIndistinguishable(t *testing.T) {
	s := newW9Sec(t, trustLoopbackProxy)
	s.uniqueClients = true
	for _, name := range []string{"nopass", "oldhash", "gone"} {
		if out, errOut, code := runWebAdminBinary(t, s.bin, s.dsn, "", nil, "user", "add", name); code != 0 {
			t.Fatalf("user add %s: %d %s%s", name, code, out, errOut)
		}
	}
	// An unsupported legacy format: a hash family the verifier does not support
	// (recognising a prefix is not support).
	sqlExec(t, s.dsn, `UPDATE magnetico_web.users SET password_hash = '$1$w9salt$abcdefghijklmnopqrstuv' WHERE username = 'oldhash'`)
	s.addUser("gone2", "w9-gone-password-123", "")
	if out, errOut, code := runWebAdminBinary(t, s.bin, s.dsn, "", nil, "user", "disable", "gone2"); code != 0 {
		t.Fatalf("disable: %d %s%s", code, out, errOut)
	}
	// `web user list` tells the operator which accounts need a reset.
	if list, _, _ := runWebAdminBinary(t, s.bin, s.dsn, "", nil, "user", "list"); !strings.Contains(list, "oldhash") {
		t.Fatalf("user list:\n%s", list)
	}

	type shot struct {
		status int
		body   string
		hdr    string
	}
	csrfValue := regexp.MustCompile(`name="csrf_token" value="[^"]*"`)
	try := func(user, pw string) shot {
		b := newBrowser(t, s.web.addr)
		b.get("/login", "X-Forwarded-For", fmt.Sprintf("10.9.%d.1", len(user)))
		resp := b.post("/login", url.Values{"username": {user}, "password": {pw}, "csrf_token": {b.token()}}, "X-Forwarded-For", fmt.Sprintf("10.9.%d.1", len(user)))
		if cookies := resp.Header["Set-Cookie"]; len(cookies) > 0 {
			for _, c := range cookies {
				if strings.Contains(c, "session") && !strings.Contains(c, "Max-Age=0") && !strings.Contains(c, "01 Jan 1970") {
					t.Errorf("%s: a failed login set a session cookie: %s", user, c)
				}
			}
		}
		return shot{resp.StatusCode, csrfValue.ReplaceAllString(b.body, ""), resp.Header.Get("Cache-Control")}
	}
	base := try("no-such-user-xyz", "whatever-password-1")
	if base.status == 303 || base.status >= 500 {
		t.Fatalf("baseline failure: %d", base.status)
	}
	for name, got := range map[string]shot{
		"wrong password":      try("alice", "not-the-password-xx"),
		"no password yet":     try("nopass", "any-password-here-1"),
		"unsupported hash":    try("oldhash", "any-password-here-2"),
		"disabled (correct)":  try("gone2", "w9-gone-password-123"),
		"empty password":      try("alice", ""),
		"unknown, long name":  try(strings.Repeat("n", 24), "x"),
		"case-different name": try("ALICE", secUserPW), // names are case sensitive
	} {
		// The page may echo the submitted username (a form re-render); compare the
		// shape with that value removed.
		want := base
		if got.status != want.status || got.hdr != want.hdr {
			t.Errorf("%s: status %d / Cache-Control %q, want %d / %q", name, got.status, got.hdr, want.status, want.hdr)
		}
		norm := func(s string) string {
			return regexp.MustCompile(`value="[^"]*"`).ReplaceAllString(s, `value=""`)
		}
		if norm(got.body) != norm(want.body) {
			t.Errorf("%s: the failure page differs from the unknown-user page", name)
		}
	}
	// The correct password of a disabled account never signs in, even after the
	// answer above; enabling it does.
	if resp := newBrowser(t, s.web.addr).login("gone2", "w9-gone-password-123"); resp.StatusCode == 303 {
		t.Fatal("a disabled account signed in")
	}
	if out, errOut, code := runWebAdminBinary(t, s.bin, s.dsn, "", nil, "user", "enable", "gone2"); code != 0 {
		t.Fatalf("enable: %d %s%s", code, out, errOut)
	}
	if resp := newBrowser(t, s.web.addr).login("gone2", "w9-gone-password-123"); resp.StatusCode != 303 {
		t.Fatalf("re-enabled account: %d", resp.StatusCode)
	}
}
