package cli

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
)

// smtpSink is a local SMTP server (STARTTLS with a test CA) that records the
// messages it receives. Nothing leaves the machine.
type smtpSink struct {
	ln   net.Listener
	cert tls.Certificate
	mu   sync.Mutex
	msgs []sunkMail
	auth []string
	wg   sync.WaitGroup
}

type sunkMail struct {
	To, Subject, Body string
}

func startSMTPSink(t *testing.T) (*smtpSink, string) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "07g test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpSink{ln: ln, cert: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); defer c.Close(); s.serve(c) }()
		}
	}()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s, caFile
}

func (s *smtpSink) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *smtpSink) serve(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReader(c)
	say := func(l string) { fmt.Fprintf(c, "%s\r\n", l) }
	say("220 sink ESMTP")
	secure := false
	var to string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			say("250-sink")
			if !secure {
				say("250-STARTTLS")
			}
			say("250 AUTH PLAIN")
		case up == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{s.cert}, MinVersion: tls.VersionTLS12})
			if tc.Handshake() != nil {
				return
			}
			c, r, secure = tc, bufio.NewReader(tc), true
			say = func(l string) { fmt.Fprintf(c, "%s\r\n", l) }
		case strings.HasPrefix(up, "AUTH PLAIN"):
			s.mu.Lock()
			s.auth = append(s.auth, fmt.Sprintf("secure=%v", secure))
			s.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM"):
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO"):
			to = strings.Trim(line[len("RCPT TO:"):], "<> ")
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			m, err := mail.ReadMessage(strings.NewReader(b.String()))
			if err == nil {
				body, _ := io.ReadAll(quotedprintable.NewReader(m.Body))
				s.mu.Lock()
				s.msgs = append(s.msgs, sunkMail{To: to, Subject: m.Header.Get("Subject"), Body: string(body)})
				s.mu.Unlock()
			}
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (s *smtpSink) wait(t *testing.T, n int) []sunkMail {
	t.Helper()
	for i := 0; i < 600; i++ {
		s.mu.Lock()
		m := append([]sunkMail(nil), s.msgs...)
		s.mu.Unlock()
		if len(m) >= n {
			return m
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected %d mails", n)
	return nil
}

// ---- helpers ----

var (
	linkPathRe = regexp.MustCompile(`https?://[^/\s]+(/[^\s]+)`)
	secretRe   = regexp.MustCompile(`<code>(m[gt][tf]_[A-Za-z0-9_-]+)</code>`)
	rssURLRe   = regexp.MustCompile(`<code>(http://[^<]*/api/v2/feeds/latest\.rss\?feed_token=[^<]+)</code>`)
)

func linkPath(t *testing.T, s string) string {
	t.Helper()
	m := linkPathRe.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no link in %q", s)
	}
	return m[1]
}

// plain performs a request without cookies or session.
func plain(t *testing.T, base, path string, hdr map[string]string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(b)
}

func infoHashOf(name string) string {
	h := sha256.Sum256([]byte(name))
	return hex.EncodeToString(h[:20])
}

// TestWebBinaryAccountAPIFeedsDocsAndEmail drives the real binary against a
// disposable PostgreSQL (restricted runtime role with the documented grants)
// and a local SMTP sink: onboarding, credentials, API v2, both feeds, the
// magnet short link, the documentation, lifecycle (disable/enable/reset),
// registration, recovery and email confirmation, and a clean shutdown.
func TestWebBinaryAccountAPIFeedsDocsAndEmail(t *testing.T) {
	bin := buildBinary(t)
	dsn := webTarget(t)
	if out, code := runWebSchemaInit(t, bin, dsn, "web-schema-init"); code != 0 {
		t.Fatalf("web-schema-init: exit %d: %s", code, out)
	}
	rtDSN, opDSN := webAuthRoles(t, dsn)
	sink, caFile := startSMTPSink(t)
	const (
		pw1     = "binary-first-password-1"
		pw2     = "binary-second-password-2"
		smtpPW  = "smtp-secret-pw-77"
		bobPW   = "bob-password-value-3"
		newPW   = "bob-recovered-password-4"
		bobMail = "bob@example.org"
	)
	admin := func(stdin string, args ...string) string {
		t.Helper()
		out, errOut, code := runWebAdminBinary(t, bin, opDSN, stdin, []string{"MAGNETICO_WEB_INVITE_QUOTA=2"}, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
		return out
	}

	// Seed synthetic torrents as the schema owner (no network, no DHT).
	files := []codec.File{{Path: "season/ep1.mkv", Size: 100}, {Path: "season/ep2.mkv", Size: 200}, {Path: "readme.txt", Size: 5}}
	id := seedBrowse(t, dsn, "07G binary torrent", files)
	for i := 0; i < 5; i++ {
		seedBrowse(t, dsn, fmt.Sprintf("07G filler %d", i), []codec.File{{Path: "f", Size: int64(i + 1)}})
	}

	// 1. Operator creates the account and a setup link; the user completes it on the public page.
	setup := linkPath(t, admin("", "user", "add", "--invites", "2", "alice"))
	w := startWeb(t, bin, rtDSN, "http",
		"MAGNETICO_WEB_SMTP_HOST=localhost", fmt.Sprintf("MAGNETICO_WEB_SMTP_PORT=%d", sink.port()), "MAGNETICO_WEB_SMTP_SECURITY=starttls",
		"MAGNETICO_WEB_SMTP_USERNAME=mailer", "MAGNETICO_WEB_SMTP_PASSWORD="+smtpPW, "MAGNETICO_WEB_SMTP_FROM=Magnetico <noreply@example.org>",
		"MAGNETICO_WEB_INVITE_QUOTA=2", "SSL_CERT_FILE="+caFile)
	base := "http://" + w.addr
	b := newBrowser(t, w.addr)

	if resp := b.get(setup); resp.StatusCode != 200 || !strings.Contains(b.body, "all of your existing API tokens") {
		t.Fatalf("setup page: %d\n%s", resp.StatusCode, b.body)
	}
	if resp := b.post(setup, url.Values{"csrf_token": {b.token()}, "password": {pw1}, "password2": {pw1}}); resp.StatusCode != 303 || resp.Header.Get("Location") != "/login?notice=password-set" {
		t.Fatalf("setup POST: %d %s\n%s", resp.StatusCode, resp.Header.Get("Location"), b.body)
	}
	if resp := b.login("alice", pw1); resp.StatusCode != 303 {
		t.Fatalf("login: %d\n%s", resp.StatusCode, b.body)
	}

	// 2. Credentials on the account page: shown once, listed without the secret.
	b.get("/account?tab=rss")
	if resp := b.post("/account/rss-tokens", url.Values{"csrf_token": {b.token()}, "label": {"qBittorrent"}}); resp.StatusCode != 200 {
		t.Fatalf("create rss token: %d\n%s", resp.StatusCode, b.body)
	}
	rssTok := secretRe.FindStringSubmatch(b.body)[1]
	rssURL := strings.ReplaceAll(rssURLRe.FindStringSubmatch(b.body)[1], "&amp;", "&")
	if !strings.HasPrefix(rssTok, "mgf_") || !strings.HasPrefix(rssURL, base+"/api/v2/feeds/latest.rss?feed_token=") {
		t.Fatalf("rss secret %q url %q", rssTok, rssURL)
	}
	b.get("/account?tab=api")
	if resp := b.post("/account/api-tokens", url.Values{"csrf_token": {b.token()}, "label": {"automation"}}); resp.StatusCode != 200 {
		t.Fatalf("create api token: %d", resp.StatusCode)
	}
	apiTok := secretRe.FindStringSubmatch(b.body)[1]
	if !strings.HasPrefix(apiTok, "mgt_") {
		t.Fatalf("api token %q", apiTok)
	}
	b.get("/account?tab=api")
	if strings.Contains(b.body, apiTok) || !strings.Contains(b.body, "automation") {
		t.Fatalf("api token list:\n%s", b.body)
	}
	if n := sqlCount(t, dsn, `SELECT count(*) FROM magnetico_web.api_tokens WHERE token_hash = sha256($1::text::bytea)`, apiTok); n != 1 {
		t.Fatalf("api token hash not stored (%d)", n)
	}
	if n := sqlCount(t, dsn, `SELECT count(*) FROM magnetico_web.api_tokens a, magnetico_web.rss_tokens r WHERE position($1 in a::text) > 0 OR position($2 in r::text) > 0`, strings.TrimPrefix(apiTok, "mgt_"), strings.TrimPrefix(rssTok, "mgf_")); n != 0 {
		t.Fatal("a token plaintext is stored")
	}

	// 3. API v2 with the Bearer token only.
	bearer := map[string]string{"Authorization": "Bearer " + apiTok}
	code, hdr, body := plain(t, base, "/api/v2/torrents?q=07G+binary", bearer)
	var list struct {
		Items []struct {
			ID       int    `json:"id"`
			InfoHash string `json:"info_hash"`
		} `json:"items"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil || len(list.Items) != 1 || list.Items[0].ID != id || hdr.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("api list: %d %s", code, body)
	}
	hash := infoHashOf("07G binary torrent")
	if list.Items[0].InfoHash != hash {
		t.Fatalf("hash %s want %s", list.Items[0].InfoHash, hash)
	}
	code, _, body = plain(t, base, fmt.Sprintf("/api/v2/torrents/%d", id), bearer)
	var det struct {
		Magnet string `json:"magnet_uri"`
		Full   bool   `json:"full_tree_available"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &det) != nil || !det.Full || !strings.Contains(det.Magnet, "&dn=07G+binary+torrent&tr=udp%3A%2F%2Ftracker.opentrackr.org") {
		t.Fatalf("api details: %d %s", code, body)
	}
	if code, _, body = plain(t, base, fmt.Sprintf("/api/v2/torrents/%d/files/tree", id), bearer); code != 200 || !json.Valid([]byte(body)) || !strings.Contains(body, `"file_count":3`) {
		t.Fatalf("api tree: %d %s", code, body)
	}
	if code, _, body = plain(t, base, fmt.Sprintf("/api/v2/torrents/%d/files?dir=0", id), bearer); code != 200 || !strings.Contains(body, `"name":"ep1.mkv"`) {
		t.Fatalf("api dir: %d %s", code, body)
	}
	// Credential separation over real HTTP.
	for name, c := range map[string]struct {
		path string
		hdr  map[string]string
	}{
		"no credential":        {"/api/v2/torrents", nil},
		"rss token as bearer":  {"/api/v2/torrents", map[string]string{"Authorization": "Bearer " + rssTok}},
		"api token in query":   {"/api/v2/torrents?access_token=" + apiTok, nil},
		"rss token in feed_tk": {"/api/v2/torrents?feed_token=" + rssTok, nil},
	} {
		if code, _, _ := plain(t, base, c.path, c.hdr); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}

	// 3b. Both feeds with the RSS token; the API token fails on both.
	feedPath := strings.TrimPrefix(rssURL, base)
	if code, hdr, body = plain(t, base, feedPath, nil); code != 200 || !strings.Contains(hdr.Get("Content-Type"), "application/rss+xml") ||
		!strings.Contains(body, "<guid isPermaLink=\"false\">"+hash+"</guid>") || hdr.Get("Referrer-Policy") != "no-referrer" || hdr.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("v2 feed: %d %v\n%.300s", code, hdr, body)
	}
	if strings.Contains(body, rssTok) {
		t.Fatal("the feed echoes the token")
	}
	for name, c := range map[string]struct {
		path string
		hdr  map[string]string
		want int
	}{
		"v1 query":         {"/api/v1/feed/rss/last?api-token=" + rssTok, nil, 200},
		"v1 header":        {"/api/v1/feed/rss/last", map[string]string{"api-token": rssTok}, 200},
		"v1 cookie":        {"/api/v1/feed/rss/last", map[string]string{"Cookie": "api-token=" + rssTok}, 200},
		"v1 api token":     {"/api/v1/feed/rss/last?api-token=" + apiTok, nil, 401},
		"v2 api token":     {"/api/v2/feeds/latest.rss?feed_token=" + apiTok, nil, 401},
		"v2 legacy header": {"/api/v2/feeds/latest.rss", map[string]string{"api-token": rssTok}, 401},
	} {
		code, _, body := plain(t, base, c.path, c.hdr)
		if code != c.want {
			t.Errorf("%s: %d", name, code)
		}
		if c.want == 401 && strings.HasPrefix(c.path, "/api/v1") && body != `{"code":401,"status":"error","message":"Invalid credentials.","data":null}` {
			t.Errorf("%s: envelope %q", name, body)
		}
	}

	// 4. The public magnet short link: no session, no lookup, trackers in stored order, no dn.
	code, hdr, _ = plain(t, base, "/magnet/"+strings.ToUpper(hash), nil)
	loc := hdr.Get("Location")
	if code != 307 || !strings.HasPrefix(loc, "magnet:?xt=urn:btih:"+hash+"&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337%2Fannounce&tr=") || strings.Contains(loc, "dn=") || strings.Count(loc, "&tr=") != 12 {
		t.Fatalf("magnet: %d %q", code, loc)
	}
	unknown := strings.Repeat("ab", 20)
	if code, hdr, _ = plain(t, base, "/magnet/"+unknown, nil); code != 307 || !strings.HasPrefix(hdr.Get("Location"), "magnet:?xt=urn:btih:"+unknown) {
		t.Fatalf("an unknown hash must redirect all the same: %d", code)
	}
	b.get(fmt.Sprintf("/torrents/%d", id))
	if !strings.Contains(b.body, base+"/magnet/"+hash) || !strings.Contains(b.body, "Share") {
		t.Fatalf("details lacks the short link:\n%s", b.body)
	}

	// 5. Documentation: anonymous requests go to the sign-in page; a signed-in
	// session gets the pages.
	docs := map[string]string{"/api": "API documentation", "/api/v1": "swagger-ui", "/api/v2": "swagger-ui", "/api/v2/openapi.yaml": "openapi: 3.2.0", "/api/v1/openapi.yaml": "deprecated"}
	for path := range docs {
		if code, hdr, _ := plain(t, base, path, nil); code != 303 || !strings.HasPrefix(hdr.Get("Location"), "/login?next=") {
			t.Errorf("%s anonymous: %d %q", path, code, hdr.Get("Location"))
		}
	}
	for path, want := range docs {
		res := b.get(path)
		if res.StatusCode != 200 || !strings.Contains(b.body, want) || res.Header.Get("X-Robots-Tag") != "noindex, nofollow" {
			t.Errorf("%s: %d %q", path, res.StatusCode, res.Header.Get("X-Robots-Tag"))
		}
	}
	res := b.get("/api/v2")
	body = b.body
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "img-src 'self' data:") {
		t.Errorf("docs CSP = %q", res.Header.Get("Content-Security-Policy"))
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="(/static/[^"]+)"`).FindAllStringSubmatch(body, -1) {
		if code, _, _ := plain(t, base, m[1], nil); code != 200 {
			t.Errorf("asset %s: %d", m[1], code)
		}
	}

	// 6. Disable suspends, enable restores; the user must sign in again.
	admin("", "user", "disable", "alice")
	for name, c := range map[string]struct {
		path string
		hdr  map[string]string
	}{"api": {"/api/v2/torrents", bearer}, "feed v2": {feedPath, nil}, "feed v1": {"/api/v1/feed/rss/last?api-token=" + rssTok, nil}} {
		if code, _, _ := plain(t, base, c.path, c.hdr); code != 401 {
			t.Errorf("%s works while disabled: %d", name, code)
		}
	}
	if resp := b.get("/account"); resp.StatusCode != 303 {
		t.Fatalf("a disabled account's session still works: %d", resp.StatusCode)
	}
	admin("", "user", "enable", "alice")
	for name, c := range map[string]struct {
		path string
		hdr  map[string]string
	}{"api": {"/api/v2/torrents", bearer}, "feed v2": {feedPath, nil}, "feed v1": {"/api/v1/feed/rss/last?api-token=" + rssTok, nil}} {
		if code, _, _ := plain(t, base, c.path, c.hdr); code != 200 {
			t.Errorf("%s does not work after enable: %d", name, code)
		}
	}
	if resp := b.login("alice", pw1); resp.StatusCode != 303 {
		t.Fatalf("login after enable: %d", resp.StatusCode)
	}

	// 7. Invitations: alice has two; she revokes one, the other registers bob (email verification mail).
	b.get("/account?tab=invitations")
	regLinks := regexp.MustCompile(`class="invitation-url mono">([^<]+)<`).FindAllStringSubmatch(b.body, -1)
	if len(regLinks) != 2 {
		t.Fatalf("invitation links = %v\n%s", regLinks, b.body)
	}
	inviteRe := regexp.MustCompile(`action="/account/invites/(\d+)/revoke"`)
	ids := inviteRe.FindAllStringSubmatch(b.body, -1)
	if resp := b.post("/account/invites/"+ids[0][1]+"/revoke", url.Values{"csrf_token": {b.token()}}); resp.StatusCode != 303 {
		t.Fatalf("revoke invite: %d", resp.StatusCode)
	}
	regPath := linkPath(t, regLinks[1][1])
	bobBrowser := newBrowser(t, w.addr)
	bobBrowser.get(regPath)
	if !strings.Contains(bobBrowser.body, "Create your account") {
		t.Fatalf("registration page (links %v):\n%s", regLinks, bobBrowser.body)
	}
	if resp := bobBrowser.post(regPath, url.Values{"csrf_token": {bobBrowser.token()}, "username": {"bob"}, "password": {bobPW}, "password2": {bobPW}, "email": {bobMail}}); resp.StatusCode != 303 {
		t.Fatalf("register: %d\n%s", resp.StatusCode, bobBrowser.body)
	}
	if n := sqlCount(t, dsn, `SELECT count(*) FROM magnetico_web.invites WHERE code = $1 AND used_by IS NOT NULL`, strings.TrimPrefix(regPath, "/register/")); n != 1 {
		t.Fatal("invitation not consumed")
	}
	if sqlCount(t, dsn, `SELECT count(*) FROM magnetico_web.invites WHERE owner_id = (SELECT id FROM magnetico_web.users WHERE username='bob') AND used_by IS NULL`) != 2 {
		t.Fatal("bob did not receive the configured invitation quota")
	}
	verify := sink.wait(t, 1)[0]
	if verify.To != bobMail || !strings.Contains(verify.Body, "/email/verify/") {
		t.Fatalf("verification mail: %+v", verify)
	}
	verifyPath := linkPath(t, verify.Body)
	anon := newBrowser(t, w.addr)
	anon.get(verifyPath)
	if resp := anon.post(verifyPath, url.Values{"csrf_token": {anon.token()}}); resp.StatusCode != 200 || !strings.Contains(anon.body, "Email address confirmed") {
		t.Fatalf("confirm email: %d\n%s", resp.StatusCode, anon.body)
	}

	// 8. Forgot password: identical answers, one mail for the verified address only.
	for _, addr := range []string{"nobody@example.org", bobMail} {
		f := newBrowser(t, w.addr)
		f.get("/password/forgot")
		if resp := f.post("/password/forgot", url.Values{"csrf_token": {f.token()}, "email": {addr}}); resp.StatusCode != 200 || !strings.Contains(f.body, "If the address you entered is registered and verified") {
			t.Fatalf("forgot %s: %d", addr, resp.StatusCode)
		}
	}
	reset := sink.wait(t, 2)[1]
	time.Sleep(300 * time.Millisecond)
	if got := sink.wait(t, 2); len(got) != 2 {
		t.Fatalf("%d mails; an unknown address must not get one", len(got))
	}
	if reset.To != bobMail || !strings.Contains(reset.Body, "ALL of your existing API tokens") || !strings.Contains(reset.Body, "ALL of your existing RSS tokens") {
		t.Fatalf("reset mail: %+v", reset)
	}
	resetPath := linkPath(t, reset.Body)
	rb := newBrowser(t, w.addr)
	rb.get(resetPath)
	if resp := rb.post(resetPath, url.Values{"csrf_token": {rb.token()}, "password": {newPW}, "password2": {newPW}}); resp.StatusCode != 303 {
		t.Fatalf("reset: %d\n%s", resp.StatusCode, rb.body)
	}
	bob := newBrowser(t, w.addr)
	if resp := bob.login("bob", newPW); resp.StatusCode != 303 {
		t.Fatalf("bob after reset: %d", resp.StatusCode)
	}

	// 9. Email change: current password required, old address stays until confirmed, notice to the old one.
	bob.get("/account?tab=profile")
	if resp := bob.post("/account/email", url.Values{"csrf_token": {bob.token()}, "email": {"bob2@example.org"}, "current_password": {"wrong-password-xx"}}); resp.StatusCode != 400 {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	bob.get("/account?tab=profile")
	if resp := bob.post("/account/email", url.Values{"csrf_token": {bob.token()}, "email": {"bob2@example.org"}, "current_password": {newPW}}); resp.StatusCode != 303 {
		t.Fatalf("email change: %d\n%s", resp.StatusCode, bob.body)
	}
	if sqlCount(t, dsn, `SELECT count(*) FROM magnetico_web.users WHERE username='bob' AND email = $1 AND email_verified_at IS NOT NULL`, bobMail) != 1 {
		t.Fatal("the old verified address was replaced before confirmation")
	}
	change := sink.wait(t, 3)[2]
	if change.To != "bob2@example.org" {
		t.Fatalf("change mail: %+v", change)
	}
	cp := linkPath(t, change.Body)
	anon2 := newBrowser(t, w.addr)
	anon2.get(cp)
	if resp := anon2.post(cp, url.Values{"csrf_token": {anon2.token()}}); resp.StatusCode != 200 {
		t.Fatalf("confirm change: %d", resp.StatusCode)
	}
	if notice := sink.wait(t, 4)[3]; notice.To != bobMail || !strings.Contains(notice.Subject, "changed") {
		t.Fatalf("notice to the old address: %+v", notice)
	}

	// 10. Operator reset kills alice's credentials: the emailed/CLI link path.
	resetLink := linkPath(t, admin("", "user", "reset-password", "alice"))
	ab := newBrowser(t, w.addr)
	ab.get(resetLink)
	if resp := ab.post(resetLink, url.Values{"csrf_token": {ab.token()}, "password": {pw2}, "password2": {pw2}}); resp.StatusCode != 303 {
		t.Fatalf("operator reset: %d\n%s", resp.StatusCode, ab.body)
	}
	for name, c := range map[string]struct {
		path string
		hdr  map[string]string
	}{"api": {"/api/v2/torrents", bearer}, "feed v2": {feedPath, nil}, "feed v1": {"/api/v1/feed/rss/last?api-token=" + rssTok, nil}} {
		if code, _, _ := plain(t, base, c.path, c.hdr); code != 401 {
			t.Errorf("%s survives a password reset: %d", name, code)
		}
	}
	if resp := b.get("/account"); resp.StatusCode != 303 {
		t.Fatalf("alice's old session survives the reset: %d", resp.StatusCode)
	}

	// 11. Clean shutdown (the mail queue drains) and a log without secrets.
	w.stop(t)
	logs := w.out.String()
	if !strings.Contains(logs, "mail queue stopped") {
		t.Errorf("no mail queue shutdown line:\n%s", logs)
	}
	for _, secret := range []string{
		pw1, pw2, bobPW, newPW, smtpPW, apiTok, strings.TrimPrefix(apiTok, "mgt_"), rssTok, strings.TrimPrefix(rssTok, "mgf_"),
		bobMail, "bob2@example.org", strings.TrimPrefix(regPath, "/register/"), strings.TrimPrefix(verifyPath, "/email/verify/"),
		strings.TrimPrefix(resetPath, "/password/reset/"), strings.TrimPrefix(resetLink, "/password/reset/"), "07G binary torrent",
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, a := range sink.auth {
		if a != "secure=true" {
			t.Errorf("AUTH outside TLS: %v", sink.auth)
		}
	}
	if len(sink.auth) == 0 {
		t.Error("the mail worker never authenticated")
	}
}

// Without SMTP configured the same binary serves everything except recovery.
func TestWebBinaryWithoutSMTPDisablesOnlyEmailFeatures(t *testing.T) {
	w, b, _ := webBinaryFixture(t)
	if resp := b.get("/account?tab=profile"); resp.StatusCode != 200 || !strings.Contains(b.body, "cannot send email") || strings.Contains(b.body, `action="/account/email"`) {
		t.Fatalf("profile: %d\n%s", resp.StatusCode, b.body)
	}
	anon := newBrowser(t, w.addr)
	anon.get("/login")
	if strings.Contains(anon.body, "Forgot password?") {
		t.Fatal("login offers recovery")
	}
	anon.get("/password/forgot")
	if !strings.Contains(anon.body, "not available on this server") {
		t.Fatalf("forgot page:\n%s", anon.body)
	}
	if resp := b.post("/account/api-tokens", url.Values{"csrf_token": {func() string { b.get("/account?tab=api"); return b.token() }()}, "label": {"x"}}); resp.StatusCode != 200 {
		t.Fatalf("tokens still work without SMTP: %d", resp.StatusCode)
	}
	w.stop(t)
}
