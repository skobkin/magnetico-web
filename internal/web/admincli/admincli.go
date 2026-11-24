// Package admincli implements the operator commands `magnetico web user …`
// and `magnetico web tracker …` (docs/web spec §4.10). Each command is a
// function over an Env, so it can be tested against a disposable database
// with captured output. Nothing secret is printed except what the operator
// asked for (a setup/reset/invite link); passwords never come from argv.
package admincli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/password"
	"golang.org/x/term"
)

// PasswordSource reads the new password for `set-password`.
type PasswordSource interface {
	ReadNewPassword() (string, error)
}

// Env is everything a command needs.
type Env struct {
	Store       *accounts.Store
	Trackers    *TrackerStore
	Out         io.Writer // stdout: results and the requested links
	PublicBase  *url.URL  // set for commands that print links
	InviteQuota int       // default invitations for `user add`
	Passwords   PasswordSource
	Hasher      *password.Service // nil means password.Default
	Now         func() time.Time
}

func (e *Env) hasher() *password.Service {
	if e.Hasher != nil {
		return e.Hasher
	}
	return password.Default
}

func (e *Env) link(path string) string {
	return strings.TrimRight(e.PublicBase.String(), "/") + path
}

func (e *Env) printf(format string, args ...any) { fmt.Fprintf(e.Out, format, args...) }

// StdinPasswords reads the password from a terminal with echo off and asks
// twice, or, when stdin is not a terminal, from one line of standard input
// without a confirmation (automation). Prompts go to stderr.
type StdinPasswords struct {
	In     *os.File
	Prompt io.Writer
}

func (s StdinPasswords) ReadNewPassword() (string, error) {
	fd := int(s.In.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(s.Prompt, "New password: ")
		first, err := term.ReadPassword(fd)
		fmt.Fprintln(s.Prompt)
		if err != nil {
			return "", err
		}
		fmt.Fprint(s.Prompt, "Repeat password: ")
		second, err := term.ReadPassword(fd)
		fmt.Fprintln(s.Prompt)
		if err != nil {
			return "", err
		}
		if string(first) != string(second) {
			return "", cmdErr("the two entries do not match")
		}
		return string(first), nil
	}
	return ReadPasswordLine(s.In)
}

// ReadPasswordLine reads one line (at most 4,097 bytes plus the newline)
// from r and strips the line ending.
func ReadPasswordLine(r io.Reader) (string, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, password.MaxLoginLength+3), 8192)
	line, err := br.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", cmdErr("no password on standard input")
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	if len(line) > password.MaxNewLength {
		return "", password.ErrTooLong
	}
	return line, nil
}

// CommandError is a failure whose message is controlled text meant for the
// operator (it never carries credentials, URLs or driver output).
type CommandError struct{ msg string }

func (e *CommandError) Error() string { return e.msg }

func cmdErr(format string, args ...any) error {
	return &CommandError{msg: fmt.Sprintf(format, args...)}
}
