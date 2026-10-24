package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/metadata"
	"github.com/skobkin/magnetico-web/persistence"
	"github.com/skobkin/magnetico-web/stats"
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

// TestSchemaInitFailureMessage pins the credential rules on the
// `migrate schema-init` failure path: only the redacted URL and a controlled
// error detail may appear.
func TestSchemaInitFailureMessage(t *testing.T) {
	t.Parallel()

	t.Run("sentinel detail without error text", func(t *testing.T) {
		t.Parallel()

		raw := "postgres://user:hunter2@localhost:5432/db?sslmode=disable"
		wrapped := fmt.Errorf("connect failed with dsn %s: %w", raw, persistence.ErrLegacySchema)
		msg := schemaInitFailureMessage(raw, wrapped)

		if strings.Contains(msg, "hunter2") || strings.Contains(msg, raw) {
			t.Errorf("message leaks credentials: %s", msg)
		}
		if !strings.Contains(msg, persistence.ErrLegacySchema.Error()) {
			t.Errorf("message lost the sentinel's fixed detail: %s", msg)
		}
		if !strings.Contains(msg, "postgres://localhost:5432/db") {
			t.Errorf("message lost the redacted URL: %s", msg)
		}
	})

	t.Run("unrecognized error reduces to its type", func(t *testing.T) {
		t.Parallel()

		raw := "postgres://localhost/db"
		msg := schemaInitFailureMessage(raw, errors.New("pg: password=hunter2 rejected"))
		if strings.Contains(msg, "hunter2") || strings.Contains(msg, "pg:") {
			t.Errorf("message leaks the unrestricted error text: %s", msg)
		}
		if !strings.Contains(msg, "unexpected error (*errors.errorString)") {
			t.Errorf("message must name the error category: %s", msg)
		}
	})
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

// prometheusCounter reads a counter's current value through the metrics
// exposition.
func prometheusCounter(t *testing.T, name string) float64 {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	stats.MakePrometheusHandler()(rec, req)

	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, name+" ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("malformed metrics line %q", line)
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			t.Fatalf("counter %s value %q: %v", name, fields[1], err)
		}
		return v
	}
	return 0
}

// TestDropInvalidRecordMetrics pins the per-cause metric routing of dropped
// metadata records: the UTF-8/NUL bucket keeps its dedicated counter and
// every other invalid-metadata cause — including the uint64→int64
// conversion-boundary rejections — increments magnetico_invalid_metadata.
func TestDropInvalidRecordMetrics(t *testing.T) {
	t.Parallel()

	t.Run("conversion-boundary rejection increments invalid_metadata", func(t *testing.T) {
		before := prometheusCounter(t, "magnetico_invalid_metadata")
		dropInvalidRecord(fmt.Errorf("%w: total size exceeds the maximum supported value",
			persistence.ErrInvalidMetadata))
		if got := prometheusCounter(t, "magnetico_invalid_metadata"); got != before+1 {
			t.Errorf("magnetico_invalid_metadata = %v after one drop, want %v", got, before+1)
		}
	})

	t.Run("text-encoding rejection increments non_utf8 only", func(t *testing.T) {
		beforeInvalid := prometheusCounter(t, "magnetico_invalid_metadata")
		beforeUTF8 := prometheusCounter(t, "magnetico_non_utf8")
		// Production shape: the encoding cause travels dual-wrapped beneath
		// ErrInvalidMetadata.
		dropInvalidRecord(fmt.Errorf("%w: %w",
			persistence.ErrInvalidMetadata, persistence.ErrInvalidTextEncoding))
		if got := prometheusCounter(t, "magnetico_non_utf8"); got != beforeUTF8+1 {
			t.Errorf("magnetico_non_utf8 = %v after one drop, want %v", got, beforeUTF8+1)
		}
		if got := prometheusCounter(t, "magnetico_invalid_metadata"); got != beforeInvalid {
			t.Errorf("magnetico_invalid_metadata = %v, want it untouched at %v", got, beforeInvalid)
		}
	})
}

// TestSchemaStateDetailsSurfaced pins the bounded, escaped catalog-object
// list in the schema-refusal failure messages — and that no other error
// category contributes details.
func TestSchemaStateDetailsSurfaced(t *testing.T) {
	t.Parallel()

	t.Run("bounded list with overflow marker", func(t *testing.T) {
		t.Parallel()

		details := make([]string, 12)
		for i := range details {
			details[i] = fmt.Sprintf("magneticod.table_%02d", i)
		}
		err := &persistence.SchemaStateError{Err: persistence.ErrUnsupportedSchema, Details: details}
		msg := schemaInitFailureMessage("postgres://user:hunter2@localhost:5432/db", err)

		if strings.Contains(msg, "hunter2") {
			t.Errorf("message leaks credentials: %s", msg)
		}
		if !strings.Contains(msg, strconv.Quote("magneticod.table_07")) {
			t.Errorf("message lacks the eighth detail: %s", msg)
		}
		if strings.Contains(msg, "table_08") {
			t.Errorf("message carries more than the bounded detail count: %s", msg)
		}
		if !strings.Contains(msg, "(+4 more)") {
			t.Errorf("message lacks the overflow marker: %s", msg)
		}
	})

	t.Run("long and hostile details are truncated and escaped", func(t *testing.T) {
		t.Parallel()

		// The specials sit inside the kept prefix; the marker sits past the
		// truncation point.
		hostile := "e\"q\n" + strings.Repeat("x", 123) + "TAILMARKER"
		err := &persistence.SchemaStateError{
			Err:     persistence.ErrUnsupportedSchema,
			Details: []string{hostile},
		}
		msg := databaseOpenFailureMessage("postgres://localhost/db", err)

		if strings.Contains(msg, "\n") {
			t.Errorf("message must stay single-line: %q", msg)
		}
		if !strings.Contains(msg, `e\"q`) || !strings.Contains(msg, `\n`) {
			t.Errorf("message lacks the escaped detail: %s", msg)
		}
		if strings.Contains(msg, "TAILMARKER") {
			t.Errorf("detail not truncated: %s", msg)
		}
	})

	t.Run("wrapped schema-state errors surface details too", func(t *testing.T) {
		t.Parallel()

		// errors.As unwraps: a SchemaStateError wrapped by an upper layer
		// still contributes its bounded detail list.
		stateErr := &persistence.SchemaStateError{
			Err:     persistence.ErrUnsupportedSchema,
			Details: []string{"unexpected table magneticod.stray"},
		}
		wrapped := fmt.Errorf("startup verification failed: %w", stateErr)
		msg := databaseOpenFailureMessage("postgres://user:hunter2@localhost/db", wrapped)

		if strings.Contains(msg, "hunter2") {
			t.Errorf("message leaks credentials: %s", msg)
		}
		if !strings.Contains(msg, strconv.Quote("unexpected table magneticod.stray")) {
			t.Errorf("the wrapped error's details did not surface: %s", msg)
		}
	})

	t.Run("schema-init composer keeps its exact non-schema shape", func(t *testing.T) {
		t.Parallel()

		wrapped := fmt.Errorf("connect failed: %w", persistence.ErrStorageIO)
		raw := "postgres://localhost/db"
		msg := schemaInitFailureMessage(raw, wrapped)
		want := fmt.Sprintf("Could not initialize the database schema on %s. %s",
			redactDatabaseURL(raw), controlledErrorDetail(wrapped))
		if msg != want {
			t.Errorf("message = %q, want %q", msg, want)
		}
	})

	t.Run("non schema-state errors carry no details", func(t *testing.T) {
		t.Parallel()

		wrapped := fmt.Errorf("connect failed: %w", persistence.ErrStorageNotImplemented)
		msg := databaseOpenFailureMessage("postgres://localhost/db", wrapped)
		want := fmt.Sprintf("Could not open the database %s. %s",
			redactDatabaseURL("postgres://localhost/db"), controlledErrorDetail(wrapped))
		if msg != want {
			t.Errorf("message = %q, want %q", msg, want)
		}
	})
}
