package cli

// Security regression: forged Host and forwarding headers, canonical public
// URLs, and the trusted-proxy boundary.

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/codec"
)

// Every absolute URL the service emits comes from PUBLIC_URL, never from the
// Host header or any forwarding header: password-recovery email links, invitation
// links on the account page, the RSS self, item and comment links. A forged
// Host that matches a forged Origin is still a cross-origin request.
func TestForgedHostAndForwardedHeadersDoNotChangeLinks(t *testing.T) {
	bin := buildBinary(t)
	dsn := harnessAppAccount(t)
	for _, c := range []string{"schema-init", "web-schema-init"} {
		if out, code := runWebSchemaInit(t, bin, dsn, c); code != 0 {
			t.Fatalf("%s: %d %s", c, code, out)
		}
	}
	sink, caFile := startSMTPSink(t)
	const pw = "w9-forged-host-password-1"
	for _, c := range []struct {
		stdin string
		args  []string
	}{{"", []string{"user", "add", "alice"}}, {pw + "\n", []string{"user", "set-password", "alice"}}, {"", []string{"user", "invite", "add", "alice"}}} {
		if out, errOut, code := runWebAdminBinary(t, bin, dsn, c.stdin, nil, c.args...); code != 0 {
			t.Fatalf("web %v: %d %s%s", c.args, code, out, errOut)
		}
	}
	sqlExec(t, dsn, `UPDATE magnetico_web.users SET email = 'alice@example.org', email_verified_at = now() WHERE username = 'alice'`)
	seedBrowse(t, dsn, "forged host torrent", []codec.File{{Path: "a/b", Size: 1}, {Path: "c", Size: 2}})

	w := startWeb(t, bin, dsn, "http",
		"MAGNETICO_WEB_SMTP_HOST=localhost", fmt.Sprintf("MAGNETICO_WEB_SMTP_PORT=%d", sink.port()), "MAGNETICO_WEB_SMTP_SECURITY=starttls",
		"MAGNETICO_WEB_SMTP_USERNAME=mailer", "MAGNETICO_WEB_SMTP_PASSWORD=smtp-secret-pw-88",
		"MAGNETICO_WEB_SMTP_FROM=Magnetico <noreply@example.org>", "SSL_CERT_FILE="+caFile)
	public := "http://" + w.addr
	forged := []string{
		"X-Forwarded-Host", "evil.example", "X-Forwarded-Proto", "https", "Forwarded", "host=evil.example;proto=https",
		"X-Forwarded-For", "6.6.6.6", "X-Real-IP", "6.6.6.6", "X-Original-Host", "evil.example", "X-Forwarded-Server", "evil.example",
		"X-Forwarded-Port", "8443", "X-Original-URL", "http://evil.example/", "X-Rewrite-URL", "http://evil.example/",
	}
	newForged := func() *webBrowser {
		b := newBrowser(t, w.addr)
		b.host = "evil.example"
		return b
	}
	noEvil := func(what, s string) {
		t.Helper()
		if strings.Contains(s, "evil.example") {
			t.Errorf("%s contains the forged host", what)
		}
	}

	// 1. A recovery email requested through a forged Host carries the public URL.
	anon := newForged()
	anon.get("/password/forgot", forged...)
	if resp := anon.post("/password/forgot", url.Values{"email": {"alice@example.org"}, "csrf_token": {anon.token()}}, forged...); resp.StatusCode != 200 {
		t.Fatalf("forgot: %d\n%s", resp.StatusCode, anon.body)
	}
	mails := sink.wait(t, 1)
	link := regexp.MustCompile(`https?://[^/\s]+/password/(?:reset|setup)/[A-Za-z0-9_-]+`).FindString(mails[0].Body)
	if link == "" {
		t.Fatalf("no recovery link in the mail:\n%s", mails[0].Body)
	}
	if !strings.HasPrefix(link, public+"/password/") {
		t.Errorf("recovery link %q does not start with the public URL %s", link, public)
	}
	noEvil("the recovery mail", mails[0].Body)

	// 2. Pages and feeds served through a forged Host.
	b := newForged()
	b.get("/login", forged...)
	resp := b.post("/login", url.Values{"username": {"alice"}, "password": {pw}, "csrf_token": {b.token()}}, forged...)
	if resp.StatusCode != 303 {
		t.Fatalf("login through a forged Host: %d", resp.StatusCode)
	}
	for _, h := range resp.Header["Set-Cookie"] {
		if strings.Contains(h, "Secure") || strings.Contains(h, "__Host-") || strings.Contains(h, "evil.example") {
			t.Errorf("a forwarding header changed the cookie policy: %s", h)
		}
	}
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS was sent because of X-Forwarded-Proto")
	}
	b.get("/account?tab=invitations", forged...)
	noEvil("the invitations page", b.body)
	if !strings.Contains(b.body, public+"/register/") {
		t.Errorf("the invitation link is not built from the public URL:\n%s", b.body)
	}
	rss := b.mint("rss")
	for _, p := range []string{"/api/v2/feeds/latest.rss?feed_token=" + rss, "/api/v1/feed/rss/last?api-token=" + rss} {
		b2 := newForged()
		b2.get(p, forged...)
		noEvil("the feed "+strings.SplitN(p, "?", 2)[0], b2.body)
		if !strings.Contains(b2.body, public+"/torrents/") {
			t.Errorf("%s: the item comments links are not built from the public URL", strings.SplitN(p, "?", 2)[0])
		}
	}
	b.get("/torrents?q=forged", forged...)
	noEvil("the search page", b.body)
	for _, bad := range []string{"https://" + w.addr, "http://evil"} {
		if strings.Contains(b.body, bad) {
			t.Errorf("the search page contains %q", bad)
		}
	}

	// 3. A forged Host with a matching forged Origin: Go's cross-origin
	// protection compares Origin with the request's own Host, so such a request
	// counts as same-origin (a rewriting reverse proxy relies on this: the
	// Host seen by the service may differ from the public one, and the
	// service must not insist on it). The protection that remains is the one
	// that matters and is asserted here: the CSRF token, which only the page of
	// the real host carries, and the host-bound session cookie, which a browser
	// never sends to another host. Recorded as finding F2 (docs/wip/07J report).
	forgedBoth := append([]string{"Origin", "http://evil.example"}, forged...)
	fresh := newForged() // no pre-session cookie, no session cookie
	if resp := fresh.post("/login", url.Values{"username": {"alice"}, "password": {pw}, "csrf_token": {"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}, forgedBoth...); resp.StatusCode != 403 {
		t.Errorf("POST /login with Host and Origin evil.example and no valid pre-session token: %d, want 403", resp.StatusCode)
	}
	// With the real session cookie (an attacker cannot make a browser send it)
	// but a token the attacker cannot know, a state change is still refused.
	b.get("/account", forged...)
	good := b.token()
	if resp := b.post("/account/api-tokens", url.Values{"label": {"x"}, "csrf_token": {"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}, forgedBoth...); resp.StatusCode != 403 {
		t.Errorf("state change with Host and Origin evil.example and a guessed token: %d, want 403", resp.StatusCode)
	}
	_ = good

	// 4. Nothing sensitive reached the service output: not the SMTP password, the
	// token of the mailed recovery link, the account's address, its password or
	// its session.
	out := w.out.String()
	for what, secret := range map[string]string{
		"SMTP password": "smtp-secret-pw-88", "recovery link token": link[strings.LastIndex(link, "/")+1:],
		"email address": "alice@example.org", "user password": pw, "RSS token": rss, "database password": "w9-role-secret-pw",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("the service output contains the %s", what)
		}
	}
	if len(out) < 200 {
		t.Errorf("the service output is suspiciously small (%d bytes)", len(out))
	}
}

// The trusted-proxy boundary: with the test client trusted as a proxy, the
// client identity of the limiters is the first untrusted address from the right
// of X-Forwarded-For; a client cannot pick its identity by prepending
// addresses, a malformed chain falls back to the peer, and identities are
// independent of each other.
func TestTrustedProxyBoundaryDecidesTheRateLimitIdentity(t *testing.T) {
	s := newHarnessSec(t, trustLoopbackProxy)
	n := 0
	attempt := func(xff string) int {
		t.Helper()
		n++
		b := newBrowser(t, s.web.addr)
		var hdr []string
		if xff != "" {
			hdr = []string{"X-Forwarded-For", xff}
		}
		b.get("/login", hdr...)
		// A different username every time: only the per-address limiter is under test.
		resp := b.post("/login", url.Values{"username": {fmt.Sprintf("nobody%d", n)}, "password": {"wrong-password-x"}, "csrf_token": {b.token()}}, hdr...)
		return resp.StatusCode
	}
	exhaust := func(xff string) {
		t.Helper()
		for i := 1; i <= 20; i++ {
			if st := attempt(xff); st == 429 {
				t.Fatalf("%q: limited after %d attempts, the burst is 20", xff, i-1)
			}
		}
		if st := attempt(xff); st != 429 {
			t.Fatalf("%q: attempt 21: %d, want 429", xff, st)
		}
	}

	exhaust("9.9.9.9")
	// Prepending addresses does not change the identity: the rightmost
	// untrusted hop is still 9.9.9.9.
	if st := attempt("1.2.3.4, 9.9.9.9"); st != 429 {
		t.Errorf("a prepended address changed the identity: %d, want 429", st)
	}
	// A trusted hop on the right is skipped (127.0.0.1 is the proxy itself).
	if st := attempt("9.9.9.9, 127.0.0.1"); st != 429 {
		t.Errorf("a trailing trusted hop changed the identity: %d, want 429", st)
	}
	// Another client is unaffected.
	if st := attempt("8.8.8.8"); st == 429 {
		t.Error("a different client address shared the exhausted bucket")
	}
	// A malformed chain falls back to the peer address, and so do a request with
	// no header and one with an over-long chain: they all share the peer's bucket.
	exhaust("not-an-ip, ;;")
	if st := attempt(""); st != 429 {
		t.Errorf("no header after the peer bucket was exhausted: %d, want 429", st)
	}
	if st := attempt(strings.Repeat("7.7.7.7, ", 200) + "7.7.7.7"); st != 429 {
		t.Errorf("an over-long chain did not fall back to the peer: %d, want 429", st)
	}
}
