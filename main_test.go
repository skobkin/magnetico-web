package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/persistence"
)

func TestRedactDatabaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "userinfo password dropped",
			raw:  "postgres://magnetico:hunter2@localhost:5432/db?sslmode=disable",
			want: "postgres://localhost:5432/db",
		},
		{
			name: "query secret dropped",
			raw:  "postgres://localhost:5432/db?password=q2w1e4",
			want: "postgres://localhost:5432/db",
		},
		{
			name: "ipv6 host preserved",
			raw:  "postgres://u:p@[::1]:5432/db",
			want: "postgres://[::1]:5432/db",
		},
		{
			name: "path preserved",
			raw:  "postgres://u:p@example.com:5432/magnetico",
			want: "postgres://example.com:5432/magnetico",
		},
		{
			name: "empty input",
			raw:  "",
			want: unparsableDatabaseURL,
		},
		{
			name: "unparsable input",
			raw:  "://nope",
			want: unparsableDatabaseURL,
		},
		{
			name: "no host",
			raw:  "sqlite:///tmp/db.sqlite3",
			want: unparsableDatabaseURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := redactDatabaseURL(tt.raw); got != tt.want {
				t.Errorf("redactDatabaseURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRedactDatabaseURL_NoCredentialMaterial(t *testing.T) {
	t.Parallel()

	raws := []string{
		"postgres://user:p%40ss@localhost:5432/db",
		"postgres://user:hunter2@localhost/db?sslcert=/tmp/c&passfile=/tmp/pf",
		"postgresql://u:p@[2001:db8::1]:5432/db?sslkey=/tmp/k",
	}
	secrets := []string{"hunter2", "p%40ss", "p@ss", "passfile", "sslkey", "sslcert", "user:", "u:p"}

	for _, raw := range raws {
		got := redactDatabaseURL(raw)
		for _, secret := range secrets {
			if strings.Contains(got, secret) {
				t.Errorf("redactDatabaseURL(%q) = %q, leaks %q", raw, got, secret)
			}
		}
	}
}

func TestDatabaseOpenFailureMessage_SentinelError(t *testing.T) {
	t.Parallel()

	raw := "postgres://user:hunter2@localhost/db?password=x"
	_, err := persistence.MakeDatabase(raw)
	msg := databaseOpenFailureMessage(raw, err)

	for _, leak := range []string{"hunter2", "user:hunter2@", "password=x"} {
		if strings.Contains(msg, leak) {
			t.Errorf("message leaks %q: %s", leak, msg)
		}
	}
	if !strings.Contains(msg, "postgres://localhost/db") {
		t.Errorf("message lost the redacted URL: %s", msg)
	}
	if !strings.Contains(msg, persistence.ErrStorageNotImplemented.Error()) {
		t.Errorf("message lost the sentinel's fixed detail: %s", msg)
	}
}

func TestDatabaseOpenFailureMessage_WrappedSentinelCarryingCredentials(t *testing.T) {
	t.Parallel()

	raw := "postgres://user:hunter2@host/db"
	wrapped := fmt.Errorf("password=secret for %s: %w", raw, persistence.ErrStorageNotImplemented)
	msg := databaseOpenFailureMessage(raw, wrapped)

	for _, leak := range []string{"secret", "hunter2", "password=", raw} {
		if strings.Contains(msg, leak) {
			t.Errorf("message leaks %q: %s", leak, msg)
		}
	}
	// Only the sentinel's own fixed message may surface, never the wrapper's.
	if !strings.Contains(msg, persistence.ErrStorageNotImplemented.Error()) {
		t.Errorf("message must contain the sentinel's fixed detail: %s", msg)
	}
}

func TestDatabaseOpenFailureMessage_UnrestrictedErrorTextNeverLogged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		rawURL string
		err    error
	}{
		{
			name:   "standalone query-string credentials without the full DSN",
			rawURL: "postgres://localhost/db",
			err:    errors.New("connect failed: password=hunter2 sslkey=/tmp/k"),
		},
		{
			name:   "error embedding the full raw DSN",
			rawURL: "postgres://user:hunter2@localhost:5432/db?sslmode=disable",
			err:    errors.New(`could not connect to "postgres://user:hunter2@localhost:5432/db?sslmode=disable"`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := databaseOpenFailureMessage(tt.rawURL, tt.err)
			if strings.Contains(msg, tt.err.Error()) {
				t.Errorf("message contains the unrestricted error text: %s", msg)
			}
			for _, leak := range []string{"hunter2", "password=", "sslkey"} {
				if strings.Contains(msg, leak) {
					t.Errorf("message leaks %q: %s", leak, msg)
				}
			}
			if !strings.Contains(msg, "unexpected error (*errors.errorString)") {
				t.Errorf("message must name the error category, not its text: %s", msg)
			}
		})
	}
}
