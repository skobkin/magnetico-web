// Package mail is the optional outgoing-email subsystem of the web service
// (docs/specs/web-v1.md §4.9): a strict-TLS SMTP submission client and a
// bounded in-process delivery queue.
//
// Transport rules (§4.9.10): the connection is encrypted and the server
// certificate is verified (system roots, the configured host as ServerName,
// TLS 1.2 or newer) BEFORE any credential or message byte is sent. Two modes
// exist, implicit TLS and STARTTLS; there is no plaintext mode and no
// opportunistic fallback, loopback included. The standard library's one-call
// sender is not used because it silently continues without STARTTLS, and
// there is no way to disable certificate verification.
//
// Errors carry a category only. The text of an SMTP failure can quote an
// address or a link, so nothing from the server or the message reaches an
// error string, and nothing here logs credentials or links.
package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Transport security modes.
const (
	StartTLS    = "starttls"
	ImplicitTLS = "tls"
)

// Defaults for the bounded connection and send times.
const (
	DefaultDialTimeout = 10 * time.Second
	DefaultSendTimeout = 30 * time.Second
)

// Config configures a Sender. Host, Port, Security and From are required.
type Config struct {
	Host     string
	Port     int
	Security string // StartTLS or ImplicitTLS
	Username string // optional; Password must then be set too
	Password string
	From     string // an address, optionally with a display name

	DialTimeout time.Duration // 0 → DefaultDialTimeout
	SendTimeout time.Duration // 0 → DefaultSendTimeout; covers the whole exchange

	// rootCAs replaces the system roots; set only by tests. There is
	// deliberately no exported way to change certificate verification.
	rootCAs *x509.CertPool
}

// Failure categories (the only text an error carries).
const (
	CatMessage        = "message"
	CatDial           = "dial"
	CatTLS            = "tls"
	CatSTARTTLSAbsent = "starttls-missing"
	CatAuth           = "auth"
	CatRejected       = "rejected"
	CatTimeout        = "timeout"
	CatProtocol       = "protocol"
)

// SendError is a failed delivery attempt.
type SendError struct{ Category string }

func (e *SendError) Error() string { return "mail: send failed: " + e.Category }

func fail(cat string) error { return &SendError{Category: cat} }

// Category returns the failure category of err, or "" if it is not a SendError.
func Category(err error) string {
	var se *SendError
	if errors.As(err, &se) {
		return se.Category
	}
	return ""
}

// Sender delivers messages over one configured SMTP submission server.
type Sender struct {
	cfg  Config
	from *mail.Address
}

// NewSender validates cfg. It makes no network connection.
func NewSender(cfg Config) (*Sender, error) {
	if cfg.Host == "" || cfg.Port < 1 || cfg.Port > 65535 {
		return nil, errors.New("mail: host and port are required")
	}
	if strings.ContainsAny(cfg.Host, " \t\r\n/") {
		return nil, errors.New("mail: invalid host")
	}
	if cfg.Security != StartTLS && cfg.Security != ImplicitTLS {
		return nil, errors.New("mail: security must be starttls or tls")
	}
	if (cfg.Username == "") != (cfg.Password == "") {
		return nil, errors.New("mail: username and password must be given together")
	}
	if hasControl(cfg.From) || hasControl(cfg.Username) {
		return nil, errors.New("mail: invalid sender or username")
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil || from.Address == "" {
		return nil, errors.New("mail: invalid sender address")
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = DefaultSendTimeout
	}
	return &Sender{cfg: cfg, from: from}, nil
}

// Send delivers m. The message is fully validated before any connection is
// made. It is bounded by the dial timeout and by SendTimeout for the whole
// exchange (and by ctx). Nothing sensitive is transmitted unless the TLS
// handshake with certificate verification has completed.
func (s *Sender) Send(ctx context.Context, m Message) error {
	to, raw, err := buildMessage(s.from, m, time.Now())
	if err != nil {
		return fail(CatMessage)
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.SendTimeout)
	defer cancel()

	host := s.cfg.Host
	dialer := net.Dialer{Timeout: s.cfg.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(s.cfg.Port)))
	if err != nil {
		return classify(ctx, CatDial)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	// A stuck handshake or read ends when the context does.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	tlsCfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: s.cfg.rootCAs}

	var c *smtp.Client
	switch s.cfg.Security {
	case ImplicitTLS:
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return classify(ctx, CatTLS)
		}
		if c, err = smtp.NewClient(tc, host); err != nil {
			return classify(ctx, CatProtocol)
		}
	default:
		if c, err = smtp.NewClient(conn, host); err != nil {
			return classify(ctx, CatProtocol)
		}
		if ok, _ := c.Extension("STARTTLS"); !ok {
			// No AUTH, no MAIL: nothing has been sent but EHLO.
			return fail(CatSTARTTLSAbsent)
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return classify(ctx, CatTLS)
		}
	}
	defer c.Close()

	// Belt and braces: whatever happened above, nothing proceeds unless the
	// connection reports a completed, verified handshake.
	if st, ok := c.TLSConnectionState(); !ok || !st.HandshakeComplete {
		return fail(CatTLS)
	}

	if s.cfg.Username != "" {
		auth, ok := s.chooseAuth(c)
		if !ok {
			return fail(CatAuth)
		}
		if err := c.Auth(auth); err != nil {
			return classify(ctx, CatAuth)
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return classify(ctx, CatRejected)
	}
	if err := c.Rcpt(to); err != nil {
		return classify(ctx, CatRejected)
	}
	w, err := c.Data()
	if err != nil {
		return classify(ctx, CatRejected)
	}
	if _, err := w.Write(raw); err != nil {
		return classify(ctx, CatProtocol)
	}
	if err := w.Close(); err != nil {
		return classify(ctx, CatRejected)
	}
	// The message is accepted; a failing QUIT does not undo that.
	c.Quit()
	return nil
}

// classify turns an I/O failure into a timeout when the exchange deadline
// passed, else into the given category.
func classify(ctx context.Context, cat string) error {
	if ctx.Err() != nil {
		return fail(CatTimeout)
	}
	return fail(cat)
}

// chooseAuth picks PLAIN, or LOGIN when only that is offered. It is called
// only after TLS is established, and net/smtp's PlainAuth additionally refuses
// to send a password over an unencrypted connection.
func (s *Sender) chooseAuth(c *smtp.Client) (smtp.Auth, bool) {
	ok, params := c.Extension("AUTH")
	if !ok {
		return nil, false
	}
	var plain, login bool
	for _, m := range strings.Fields(strings.ToUpper(params)) {
		switch m {
		case "PLAIN":
			plain = true
		case "LOGIN":
			login = true
		}
	}
	switch {
	case plain:
		return smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host), true
	case login:
		return &loginAuth{user: s.cfg.Username, pass: s.cfg.Password}, true
	}
	return nil, false
}

// loginAuth is the AUTH LOGIN mechanism for servers that offer nothing else.
// It is used only on a TLS connection (see Send).
type loginAuth struct{ user, pass string }

func (a *loginAuth) Start(si *smtp.ServerInfo) (string, []byte, error) {
	if !si.TLS {
		return "", nil, errors.New("mail: refusing AUTH LOGIN without TLS")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(challenge))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("mail: unexpected AUTH LOGIN challenge")
}
