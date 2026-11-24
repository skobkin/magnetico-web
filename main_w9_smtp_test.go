package main

// W9 security regression: SMTP failures. Mail is delivered over strictly
// verified TLS or not at all; a failing mail server never changes what a client
// sees, never receives a credential or a message in the clear, and never gets
// its failure text, an address or a link into the service log.

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a plain-TCP SMTP server with a deliberately broken behaviour. It
// records every command a client sends after the greeting.
type fakeSMTP struct {
	ln   net.Listener
	mode string
	mu   sync.Mutex
	cmds []string
	wg   sync.WaitGroup
}

func startFakeSMTP(t *testing.T, mode string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, mode: mode}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Add(1)
			go func() { defer f.wg.Done(); defer c.Close(); f.serve(c) }()
		}
	}()
	t.Cleanup(func() { ln.Close(); f.wg.Wait() })
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if f.mode == "drop" {
		return
	}
	r := bufio.NewReader(c)
	fmt.Fprintf(c, "220 fake ESMTP\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.mu.Lock()
		f.cmds = append(f.cmds, line)
		f.mu.Unlock()
		switch up := strings.ToUpper(line); {
		case strings.HasPrefix(up, "EHLO"):
			// Offers AUTH but never STARTTLS: a client that falls back to plaintext
			// would authenticate and send here.
			fmt.Fprintf(c, "250-fake\r\n250 AUTH PLAIN LOGIN\r\n")
		case up == "QUIT":
			fmt.Fprintf(c, "221 bye\r\n")
			return
		default:
			fmt.Fprintf(c, "502 not here\r\n")
		}
	}
}

func (f *fakeSMTP) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

var linkTokenInLog = regexp.MustCompile(`/(reset|setup|verify|register)/[A-Za-z0-9_-]{16,}`)

func TestW9SMTPFailuresAreContainedAndLeakNothing(t *testing.T) {
	bin := buildBinary(t)
	dsn := w9AppAccount(t)
	for _, c := range []string{"schema-init", "web-schema-init"} {
		if out, code := runWebSchemaInit(t, bin, dsn, c); code != 0 {
			t.Fatalf("%s: %d %s", c, code, out)
		}
	}
	const pw, smtpPW = "w9-smtp-failure-password-1", "smtp-secret-pw-99"
	for _, c := range []struct {
		stdin string
		args  []string
	}{{"", []string{"user", "add", "alice"}}, {pw + "\n", []string{"user", "set-password", "alice"}}} {
		if out, errOut, code := runWebAdminBinary(t, bin, dsn, c.stdin, nil, c.args...); code != 0 {
			t.Fatalf("web %v: %d %s%s", c.args, code, out, errOut)
		}
	}
	sqlExec(t, dsn, `UPDATE magnetico_web.users SET email = 'alice-smtp@example.org', email_verified_at = now() WHERE username = 'alice'`)

	closed := func() int { // a port nothing listens on
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		p := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		return p
	}
	// "untrusted certificate": the standard sink (STARTTLS with a throwaway CA)
	// started without SSL_CERT_FILE, so the certificate cannot verify.
	untrusted, _ := startSMTPSink(t)
	noTLS := startFakeSMTP(t, "no-starttls")
	drop := startFakeSMTP(t, "drop")

	for _, sc := range []struct {
		name string
		port int
		// check runs after the failures were logged.
		check func(t *testing.T)
	}{
		{"connection refused", closed(), nil},
		{"server without STARTTLS", noTLS.port(), func(t *testing.T) {
			for _, c := range noTLS.commands() {
				u := strings.ToUpper(c)
				if strings.HasPrefix(u, "AUTH") || strings.HasPrefix(u, "MAIL") || strings.HasPrefix(u, "RCPT") || u == "DATA" {
					t.Errorf("the client sent %q to a server that offers no STARTTLS (plaintext fallback)", c)
				}
			}
		}},
		{"untrusted certificate", untrusted.port(), func(t *testing.T) {
			untrusted.mu.Lock()
			defer untrusted.mu.Unlock()
			if len(untrusted.auth) != 0 || len(untrusted.msgs) != 0 {
				t.Errorf("credentials or mail reached a server whose certificate cannot verify: auth=%v msgs=%d", untrusted.auth, len(untrusted.msgs))
			}
		}},
		{"connection dropped", drop.port(), nil},
	} {
		t.Run(sc.name, func(t *testing.T) {
			w := startWeb(t, bin, dsn, "http",
				"MAGNETICO_WEB_SMTP_HOST=localhost", fmt.Sprintf("MAGNETICO_WEB_SMTP_PORT=%d", sc.port), "MAGNETICO_WEB_SMTP_SECURITY=starttls",
				"MAGNETICO_WEB_SMTP_USERNAME=mailer", "MAGNETICO_WEB_SMTP_PASSWORD="+smtpPW, "MAGNETICO_WEB_SMTP_FROM=Magnetico <noreply@example.org>")
			forgot := func(addr string) (int, string) {
				b := newBrowser(t, w.addr)
				b.get("/password/forgot")
				resp := b.post("/password/forgot", url.Values{"email": {addr}, "csrf_token": {b.token()}})
				return resp.StatusCode, regexp.MustCompile(`name="csrf_token" value="[^"]*"`).ReplaceAllString(b.body, "")
			}
			// A verified address and an unknown one get the same answer while mail fails.
			s1, b1 := forgot("alice-smtp@example.org")
			s2, b2 := forgot("nobody-here@example.org")
			if s1 != 200 || s1 != s2 || b1 != b2 {
				t.Errorf("the forgot-password answer depends on the account while mail fails: %d/%d, equal bodies %v", s1, s2, b1 == b2)
			}
			// An authenticated address change is queued and answered normally.
			a := newBrowser(t, w.addr)
			if resp := a.login("alice", pw); resp.StatusCode != 303 {
				t.Fatalf("login: %d", resp.StatusCode)
			}
			a.get("/account")
			resp := a.post("/account/email", url.Values{"email": {"alice-new@example.org"}, "current_password": {pw}, "csrf_token": {a.token()}})
			if resp.StatusCode != 303 {
				t.Errorf("address change while mail fails: %d\n%s", resp.StatusCode, a.body)
			}
			// The failures are logged, by category only.
			deadline := time.Now().Add(15 * time.Second)
			for strings.Count(w.out.String(), "mail delivery failed") < 2 && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			out := w.out.String()
			if n := strings.Count(out, "mail delivery failed"); n < 2 {
				t.Fatalf("expected two logged delivery failures, saw %d:\n%s", n, out)
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "mail delivery failed") && !regexp.MustCompile(`category=[a-z-]+\s*$`).MatchString(strings.TrimSpace(line)) {
					t.Errorf("a delivery failure is logged with more than its category: %s", line)
				}
			}
			for what, secret := range map[string]string{
				"SMTP password": smtpPW, "SMTP user": "mailer", "verified address": "alice-smtp@example.org", "new address": "alice-new@example.org",
				"unknown address": "nobody-here@example.org", "user password": pw, "database password": "w9-role-secret-pw",
			} {
				if strings.Contains(out, secret) {
					t.Errorf("the service log contains the %s", what)
				}
			}
			if linkTokenInLog.MatchString(out) {
				t.Errorf("the service log contains a link token: %s", linkTokenInLog.FindString(out))
			}
			if sc.check != nil {
				sc.check(t)
			}
			// Mail failing does not stop the service from shutting down cleanly.
			w.stop(t)
		})
	}
}
