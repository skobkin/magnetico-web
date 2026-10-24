package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/metadata"
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

	// A non-postgres scheme fails closed without any network activity.
	raw := "sqlite3:///tmp/failclosed.db"
	_, err := persistence.MakeDatabase(context.Background(), raw)
	msg := databaseOpenFailureMessage(raw, err)

	for _, leak := range []string{"/tmp/failclosed.db"} {
		if strings.Contains(msg, leak) {
			t.Errorf("message leaks %q: %s", leak, msg)
		}
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

func TestControlledErrorDetail_NewSentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "missing extension",
			err:  persistence.ErrMissingExtension,
			want: persistence.ErrMissingExtension.Error(),
		},
		{
			name: "unsupported schema",
			err:  persistence.ErrUnsupportedSchema,
			want: persistence.ErrUnsupportedSchema.Error(),
		},
		{
			name: "legacy schema via SchemaStateError",
			err: &persistence.SchemaStateError{
				Err:     persistence.ErrLegacySchema,
				Details: []string{"public.files", "magneticod.torrents without file_count column"},
			},
			want: persistence.ErrLegacySchema.Error(),
		},
		{
			name: "invalid metadata",
			err:  persistence.ErrInvalidMetadata,
			want: persistence.ErrInvalidMetadata.Error(),
		},
		{
			name: "storage I/O",
			err:  persistence.ErrStorageIO,
			want: persistence.ErrStorageIO.Error(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := controlledErrorDetail(tt.err); got != tt.want {
				t.Errorf("controlledErrorDetail() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestControlledErrorDetail_WrappedCredentialsNeverSurface pins the rule on
// the new sentinels: a wrapper that embeds credentials around a recognized
// sentinel contributes only the sentinel's fixed message.
func TestControlledErrorDetail_WrappedCredentialsNeverSurface(t *testing.T) {
	t.Parallel()

	dsn := "postgres://user:hunter2@host/db?password=secret"
	wrapped := fmt.Errorf("%w: failed connecting to %s", persistence.ErrStorageIO, dsn)
	got := controlledErrorDetail(wrapped)

	if got != persistence.ErrStorageIO.Error() {
		t.Errorf("controlledErrorDetail() = %q, want the fixed sentinel text", got)
	}
	for _, leak := range []string{"hunter2", "secret", dsn, "failed connecting"} {
		if strings.Contains(got, leak) {
			t.Errorf("controlledErrorDetail() leaks %q: %s", leak, got)
		}
	}
}

// TestTorrentFromMetadata_OversizedTotalSizeRejected is the uint64→int64
// conversion-boundary regression: an oversized uint64 total size must be
// rejected before any conversion or Torrent construction, so it can never
// wrap into a negative value and reach persistence.
func TestTorrentFromMetadata_OversizedTotalSizeRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		totalSize uint64
		wantErr   bool
		wantTotal int64
	}{
		{name: "max int64 is representable", totalSize: math.MaxInt64, wantTotal: math.MaxInt64},
		{name: "max int64 + 1 wraps — must be rejected", totalSize: math.MaxInt64 + 1, wantErr: true},
		{name: "1<<63 + 1 wraps — must be rejected", totalSize: 1<<63 + 1, wantErr: true},
		{name: "max uint64 must be rejected", totalSize: math.MaxUint64, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			md := metadata.Metadata{
				InfoHash:     []byte("01234567890123456789"),
				Name:         "release",
				TotalSize:    tt.totalSize,
				DiscoveredOn: 1700000000,
			}
			got, err := torrentFromMetadata(&md)
			if tt.wantErr {
				if !errors.Is(err, persistence.ErrInvalidMetadata) {
					t.Fatalf("torrentFromMetadata() error = %v, want ErrInvalidMetadata", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("torrentFromMetadata() error = %v", err)
			}
			if got.TotalSize != tt.wantTotal {
				t.Errorf("Torrent.TotalSize = %d, want %d", got.TotalSize, tt.wantTotal)
			}
			if got.TotalSize < 0 {
				t.Errorf("Torrent.TotalSize = %d wrapped negative", got.TotalSize)
			}
		})
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
