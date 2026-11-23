package mail

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testSender(t *testing.T, f *fakeSMTP, ca *testCA, security string, mut func(*Config)) *Sender {
	t.Helper()
	cfg := Config{
		Host: "localhost", Port: f.port(), Security: security,
		Username: "mailer", Password: "s3cret-pass", From: "Magnetico <noreply@example.org>",
		DialTimeout: 2 * time.Second, SendTimeout: 5 * time.Second, rootCAs: ca.pool,
	}
	if mut != nil {
		mut(&cfg)
	}
	s, err := NewSender(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var hello = Message{To: "alice@example.com", Subject: "Hello ✓", Body: "line one\nline two\n.dot line\nlink: https://example.org/x/abcdef\n"}

func TestSTARTTLSDeliversAfterTLSAndOnlyThen(t *testing.T) {
	ca := newTestCA(t)
	f := startFake(t, "starttls", ca.serverCert(t, "localhost"))
	s := testSender(t, f, ca, StartTLS, nil)
	if err := s.Send(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	ev, msgs, creds := f.snapshot()
	want := []string{"EHLO", "STARTTLS", "EHLO", "AUTH:tls=true", "MAIL:tls=true", "RCPT", "DATA:tls=true"}
	if strings.Join(ev, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", ev, want)
	}
	if len(creds) != 1 || creds[0] != "mailer:s3cret-pass" {
		t.Fatalf("creds = %v", creds)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d", len(msgs))
	}
	parsed, err := mail.ReadMessage(strings.NewReader(msgs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Header.Get("To"); got != "alice@example.com" {
		t.Errorf("To = %q", got)
	}
	subj, _ := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if subj != "Hello ✓" {
		t.Errorf("Subject = %q", subj)
	}
	if from := parsed.Header.Get("From"); !strings.Contains(from, "noreply@example.org") {
		t.Errorf("From = %q", from)
	}
	if parsed.Header.Get("Message-ID") == "" || parsed.Header.Get("Date") == "" {
		t.Error("missing Message-ID or Date")
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(parsed.Body))
	if string(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))) != hello.Body {
		t.Errorf("body round trip = %q", body)
	}
}

func TestImplicitTLS(t *testing.T) {
	ca := newTestCA(t)
	f := startFake(t, "tls", ca.serverCert(t, "localhost"))
	s := testSender(t, f, ca, ImplicitTLS, nil)
	if err := s.Send(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	ev, _, creds := f.snapshot()
	for _, e := range ev {
		if strings.Contains(e, "tls=false") {
			t.Fatalf("something happened outside TLS: %v", ev)
		}
	}
	if len(creds) != 1 {
		t.Fatalf("creds = %v", creds)
	}
}

func TestAuthLoginWhenOnlyLoginIsOffered(t *testing.T) {
	ca := newTestCA(t)
	f := startFake(t, "starttls", ca.serverCert(t, "localhost"))
	f.authMechs = "LOGIN"
	s := testSender(t, f, ca, StartTLS, nil)
	if err := s.Send(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	if _, _, creds := f.snapshot(); len(creds) != 1 || creds[0] != "mailer:s3cret-pass" {
		t.Fatalf("creds = %v", creds)
	}
}

func TestNoAuthWhenNoCredentialsConfigured(t *testing.T) {
	ca := newTestCA(t)
	f := startFake(t, "starttls", ca.serverCert(t, "localhost"))
	s := testSender(t, f, ca, StartTLS, func(c *Config) { c.Username, c.Password = "", "" })
	if err := s.Send(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	if f.has("AUTH") {
		t.Fatal("AUTH issued without credentials")
	}
}

// A server that does not offer STARTTLS, on loopback, with AUTH advertised: the
// send fails, and neither credentials, sender, recipient nor content arrive.
func TestMissingSTARTTLSSendsNothingEvenOnLoopback(t *testing.T) {
	ca := newTestCA(t)
	f := startFake(t, "plain", ca.serverCert(t, "localhost"))
	s := testSender(t, f, ca, StartTLS, nil)
	err := s.Send(context.Background(), hello)
	if Category(err) != CatSTARTTLSAbsent {
		t.Fatalf("err = %v", err)
	}
	ev, msgs, creds := f.snapshot()
	for _, e := range ev {
		if e != "EHLO" {
			t.Fatalf("beyond EHLO the client sent %v", ev)
		}
	}
	if len(msgs) != 0 || len(creds) != 0 {
		t.Fatalf("messages=%d creds=%v", len(msgs), creds)
	}
}

func TestImplicitTLSAgainstPlaintextServerFails(t *testing.T) {
	ca := newTestCA(t)
	f := startFake(t, "plain", ca.serverCert(t, "localhost"))
	s := testSender(t, f, ca, ImplicitTLS, nil)
	err := s.Send(context.Background(), hello)
	if c := Category(err); c != CatTLS && c != CatTimeout {
		t.Fatalf("err = %v", err)
	}
	if f.has("AUTH") || f.has("MAIL") {
		t.Fatal("protocol commands reached a server that never completed TLS")
	}
}

func TestCertificateFailuresSendNothing(t *testing.T) {
	ca := newTestCA(t)
	other := newTestCA(t)
	cases := map[string]struct {
		cert  func() *fakeSMTP
		sec   string
		trust *testCA
	}{
		"wrong hostname starttls": {func() *fakeSMTP { return startFake(t, "starttls", ca.serverCert(t, "other.test")) }, StartTLS, ca},
		"untrusted CA starttls":   {func() *fakeSMTP { return startFake(t, "starttls", other.serverCert(t, "localhost")) }, StartTLS, ca},
		"wrong hostname tls":      {func() *fakeSMTP { return startFake(t, "tls", ca.serverCert(t, "other.test")) }, ImplicitTLS, ca},
		"untrusted CA tls":        {func() *fakeSMTP { return startFake(t, "tls", other.serverCert(t, "localhost")) }, ImplicitTLS, ca},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := tc.cert()
			s := testSender(t, f, tc.trust, tc.sec, nil)
			err := s.Send(context.Background(), hello)
			if Category(err) != CatTLS {
				t.Fatalf("err = %v", err)
			}
			_, msgs, creds := f.snapshot()
			if f.has("AUTH") || f.has("MAIL") || f.has("RCPT") || f.has("DATA") || len(msgs) != 0 || len(creds) != 0 {
				ev, _, _ := f.snapshot()
				t.Fatalf("data flowed despite the certificate failure: %v", ev)
			}
		})
	}
}

func TestSystemRootsAreUsedByDefault(t *testing.T) {
	// With no test roots the server's private CA is not trusted.
	ca := newTestCA(t)
	f := startFake(t, "starttls", ca.serverCert(t, "localhost"))
	s := testSender(t, f, ca, StartTLS, func(c *Config) { c.rootCAs = nil })
	if err := s.Send(context.Background(), hello); Category(err) != CatTLS {
		t.Fatalf("err = %v", err)
	}
	if f.has("AUTH") {
		t.Fatal("AUTH sent")
	}
}

func TestSSLCertFileIsHonoured(t *testing.T) {
	// The binary tests rely on SSL_CERT_FILE to trust their test CA.
	ca := newTestCA(t)
	f := startFake(t, "starttls", ca.serverCert(t, "localhost"))
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", path)
	s := testSender(t, f, ca, StartTLS, func(c *Config) { c.rootCAs = nil })
	if err := s.Send(context.Background(), hello); err != nil {
		t.Skipf("system roots were already initialised in this process: %v", err)
	}
}

func TestHeaderInjectionIsRefusedBeforeDialing(t *testing.T) {
	ca := newTestCA(t)
	f := startFake(t, "starttls", ca.serverCert(t, "localhost"))
	s := testSender(t, f, ca, StartTLS, nil)
	bad := []Message{
		{To: "a@example.com\r\nBcc: evil@example.net", Subject: "x", Body: "b"},
		{To: "a@example.com\nBcc: evil@example.net", Subject: "x", Body: "b"},
		Message{To: "a@example.com", Body: "b"}.withSubject("hi\r\nBcc: evil@example.net"),
		{To: "a@example.com", Subject: "x\x00y", Body: "b"},
		{To: "Alice <a@example.com>", Subject: "x", Body: "b"},
		{To: "a@example.com, b@example.com", Subject: "x", Body: "b"},
		{To: "", Subject: "x", Body: "b"},
		{To: "a@example.com", Subject: "", Body: "b"},
		{To: "not an address", Subject: "x", Body: "b"},
	}
	for i, m := range bad {
		if err := s.Send(context.Background(), m); Category(err) != CatMessage {
			t.Errorf("case %d: err = %v", i, err)
		}
	}
	ev, _, _ := f.snapshot()
	if len(ev) != 0 {
		t.Fatalf("the server was contacted for an invalid message: %v", ev)
	}
}

func (m Message) withSubject(s string) Message { m.Subject = s; return m }

func TestNewSenderValidation(t *testing.T) {
	good := Config{Host: "mail.example.org", Port: 587, Security: StartTLS, From: "n@example.org"}
	if _, err := NewSender(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"no host":        func(c *Config) { c.Host = "" },
		"bad port":       func(c *Config) { c.Port = 0 },
		"plaintext mode": func(c *Config) { c.Security = "plain" },
		"empty mode":     func(c *Config) { c.Security = "" },
		"user only":      func(c *Config) { c.Username = "u" },
		"pass only":      func(c *Config) { c.Password = "p" },
		"bad from":       func(c *Config) { c.From = "nope" },
		"from injection": func(c *Config) { c.From = "n@example.org\r\nBcc: x@y.z" },
		"host with path": func(c *Config) { c.Host = "mail.example.org/x" },
	} {
		c := good
		mut(&c)
		if _, err := NewSender(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestStalledServerIsBoundedByTheSendTimeout(t *testing.T) {
	ca := newTestCA(t)
	f := startFake(t, "silent", ca.serverCert(t, "localhost"))
	s := testSender(t, f, ca, StartTLS, func(c *Config) { c.SendTimeout = 300 * time.Millisecond })
	start := time.Now()
	err := s.Send(context.Background(), hello)
	if Category(err) != CatTimeout && Category(err) != CatProtocol {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestDialFailureAndErrorsCarryCategoryOnly(t *testing.T) {
	ca := newTestCA(t)
	f := startFake(t, "starttls", ca.serverCert(t, "localhost"))
	port := f.port()
	f.ln.Close()
	s := testSender(t, f, ca, StartTLS, func(c *Config) { c.Port = port })
	err := s.Send(context.Background(), hello)
	if Category(err) != CatDial {
		t.Fatalf("err = %v", err)
	}
	for _, secret := range []string{"alice@example.com", "s3cret-pass", "abcdef", "localhost"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error text leaks %q: %v", secret, err)
		}
	}
}

// --- queue ---

type fakeMailer struct {
	mu    sync.Mutex
	sent  []Message
	err   error
	delay time.Duration
	block chan struct{}
}

func (m *fakeMailer) Send(ctx context.Context, msg Message) error {
	if m.block != nil {
		select {
		case <-m.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	time.Sleep(m.delay)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, msg)
	return nil
}

func (m *fakeMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func logTo(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func runQueue(t *testing.T, q *Queue) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestQueueDeliversAndCountsAndFreesSlots(t *testing.T) {
	m := &fakeMailer{}
	var logs bytes.Buffer
	q := NewQueue(m, logTo(&logs), Options{Capacity: 4, Workers: 2})
	runQueue(t, q)
	for i := 0; i < 20; i++ {
		var slot *Slot
		waitFor(t, "slot", func() bool { var err error; slot, err = q.Reserve(); return err == nil })
		slot.Submit(func(ctx context.Context, send func(Message) error) {
			send(Message{To: "a@example.com", Subject: "s", Body: "b"})
		})
	}
	waitFor(t, "all sent", func() bool { return m.count() == 20 })
	waitFor(t, "slots free", func() bool { return q.Pending() == 0 })
	if st := q.Stats(); st.Sent != 20 || st.Failed != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestReserveFailsWhenFullWithoutBlockingAndReleaseFrees(t *testing.T) {
	q := NewQueue(&fakeMailer{}, logTo(&bytes.Buffer{}), Options{Capacity: 2})
	a, err := q.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := q.Reserve()
	start := time.Now()
	if _, err := q.Reserve(); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("Reserve blocked")
	}
	a.Release()
	a.Release() // idempotent
	if q.Pending() != 1 {
		t.Fatalf("pending = %d", q.Pending())
	}
	c, err := q.Reserve()
	if err != nil {
		t.Fatalf("released slot not reusable: %v", err)
	}
	b.Release()
	c.Release()
	if q.Stats().Rejected != 1 {
		t.Fatalf("rejected = %d", q.Stats().Rejected)
	}
}

func TestQueueClosedRejectsButSubmittedJobsFinishOnDrain(t *testing.T) {
	m := &fakeMailer{delay: 20 * time.Millisecond}
	q := NewQueue(m, logTo(&bytes.Buffer{}), Options{Capacity: 8, Workers: 2})
	runQueue(t, q)
	for i := 0; i < 6; i++ {
		s, _ := q.Reserve()
		s.Submit(func(ctx context.Context, send func(Message) error) { send(Message{To: "a@example.com", Subject: "s"}) })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n := q.Drain(ctx); n != 0 {
		t.Fatalf("abandoned %d", n)
	}
	if m.count() != 6 {
		t.Fatalf("sent %d of 6 before Drain returned", m.count())
	}
	if _, err := q.Reserve(); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("err = %v", err)
	}
}

func TestDrainGivesUpAtTheDeadlineAndReportsAbandonedJobs(t *testing.T) {
	m := &fakeMailer{block: make(chan struct{})}
	var logs bytes.Buffer
	q := NewQueue(m, logTo(&logs), Options{Capacity: 4, Workers: 1})
	runQueue(t, q)
	for i := 0; i < 3; i++ {
		s, _ := q.Reserve()
		s.Submit(func(ctx context.Context, send func(Message) error) { send(Message{To: "a@example.com", Subject: "s"}) })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if n := q.Drain(ctx); n != 3 {
		t.Fatalf("abandoned = %d, want 3", n)
	}
	if !strings.Contains(logs.String(), "abandoned") {
		t.Fatalf("no log: %s", logs.String())
	}
	close(m.block)
}

func TestOldJobsAreDroppedNotSent(t *testing.T) {
	m := &fakeMailer{}
	now := time.Now()
	var mu sync.Mutex
	clock := now
	q := NewQueue(m, logTo(&bytes.Buffer{}), Options{Capacity: 4, Workers: 1, MaxAge: time.Minute,
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }})
	s, _ := q.Reserve()
	s.Submit(func(ctx context.Context, send func(Message) error) { send(Message{To: "a@example.com", Subject: "s"}) })
	mu.Lock()
	clock = now.Add(2 * time.Minute)
	mu.Unlock()
	runQueue(t, q)
	waitFor(t, "expiry", func() bool { return q.Stats().Expired == 1 })
	if m.count() != 0 {
		t.Fatal("an expired job was sent")
	}
	waitFor(t, "slot freed", func() bool { return q.Pending() == 0 })
}

func TestFailuresAndPanicsDoNotKillWorkersOrLeakDetails(t *testing.T) {
	m := &fakeMailer{err: &SendError{Category: CatTLS}}
	var logs bytes.Buffer
	q := NewQueue(m, logTo(&logs), Options{Capacity: 4, Workers: 1})
	runQueue(t, q)
	var ran atomic.Int32
	s, _ := q.Reserve()
	s.Submit(func(ctx context.Context, send func(Message) error) {
		send(Message{To: "victim@example.com", Subject: "s", Body: "https://example.org/reset/SECRETTOKEN"})
		ran.Add(1)
	})
	s, _ = q.Reserve()
	s.Submit(func(ctx context.Context, send func(Message) error) { panic("boom victim@example.com") })
	s, _ = q.Reserve()
	s.Submit(func(ctx context.Context, send func(Message) error) { ran.Add(1) })
	waitFor(t, "jobs done", func() bool { return ran.Load() == 2 && q.Pending() == 0 })
	st := q.Stats()
	if st.Failed != 1 || st.Panicked != 1 {
		t.Fatalf("stats = %+v", st)
	}
	for _, secret := range []string{"victim@example.com", "SECRETTOKEN", "boom"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log leaks %q:\n%s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "category=tls") {
		t.Fatalf("category missing from the log:\n%s", logs.String())
	}
}

func TestTemplatesCarryBothConsequencesAndTheLink(t *testing.T) {
	m := PasswordLinkMessage("a@example.com", "https://example.org/password/reset/TOK", false, time.Hour)
	for _, want := range []string{"https://example.org/password/reset/TOK", "ALL of your existing API tokens", "ALL of your existing RSS tokens", "RSS feed URLs", "1 hour", "update your"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("reset body lacks %q:\n%s", want, m.Body)
		}
	}
	if !strings.Contains(PasswordLinkMessage("a@example.com", "l", true, 72*time.Hour).Body, "72 hours") {
		t.Error("setup ttl")
	}
	v := VerifyMessage("a@example.com", "https://example.org/email/verify/T", 24*time.Hour)
	if !strings.Contains(v.Body, "https://example.org/email/verify/T") || !strings.Contains(v.Body, "24 hours") {
		t.Errorf("verify body: %s", v.Body)
	}
	// Every template builds a valid message.
	from, _ := mail.ParseAddress("n@example.org")
	for _, msg := range []Message{m, v, EmailChangedNotice("a@example.com")} {
		if _, _, err := buildMessage(from, msg, time.Now()); err != nil {
			t.Errorf("%q: %v", msg.Subject, err)
		}
	}
}

// The two ways to weaken the transport must not exist anywhere in the web
// service's non-test sources.
func TestNoPlaintextFallbackOrVerificationBypassInSource(t *testing.T) {
	root := filepath.Join("..", "..")
	forbidden := []string{"Insecure" + "SkipVerify", "smtp." + "SendMail"}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, f := range forbidden {
			if bytes.Contains(b, []byte(f)) {
				t.Errorf("%s contains %s", path, f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
