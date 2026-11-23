package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// Message is one plain-text email. Every field is validated by buildMessage
// before any network connection is made.
type Message struct {
	To      string // a bare address, no display name
	Subject string
	Body    string
}

var errMessage = errors.New("invalid message")

// hasControl reports a C0 control character (CR, LF, NUL and tabs included) or
// DEL: none of them may appear in a header value or an envelope address.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// parseBareAddress accepts exactly one plain address ("user@host"): no display
// name, no group, no comment, no control characters.
func parseBareAddress(s string) (string, error) {
	if s == "" || len(s) > 254 || hasControl(s) {
		return "", errMessage
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s {
		return "", errMessage
	}
	return a.Address, nil
}

// buildMessage renders the RFC 5322 message: headers built only from validated
// values, a quoted-printable UTF-8 text body, CRLF line ends.
func buildMessage(from *mail.Address, m Message, now time.Time) (to string, raw []byte, err error) {
	if to, err = parseBareAddress(m.To); err != nil {
		return "", nil, err
	}
	if hasControl(m.Subject) || m.Subject == "" || len(m.Subject) > 200 {
		return "", nil, errMessage
	}
	at := strings.LastIndexByte(from.Address, '@')
	if at < 0 {
		return "", nil, errMessage
	}
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", nil, err
	}

	var b bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	hdr("From", from.String())
	hdr("To", to)
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	hdr("Date", now.UTC().Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id[:])+"@"+from.Address[at+1:]+">")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", `text/plain; charset="utf-8"`)
	hdr("Content-Transfer-Encoding", "quoted-printable")
	hdr("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	body := strings.ReplaceAll(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n", "\r\n")
	if _, err := qp.Write([]byte(body)); err != nil {
		return "", nil, err
	}
	if err := qp.Close(); err != nil {
		return "", nil, err
	}
	return to, b.Bytes(), nil
}
