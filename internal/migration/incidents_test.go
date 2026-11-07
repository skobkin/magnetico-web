package migration

// Unit tests for diagnostic sanitization and the stored profile name.

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/skobkin/magnetico-web/codec"
)

func TestSanitizeDetailRedactsCredentials(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, mustNotContain string }{
		{"connect failed: password=hunter2 host=db", "hunter2"},
		{"failed: postgres://user:hunter2@db.example/magneticod", "hunter2"},
		{"failed: postgresql://user:hunter2@db.example/magneticod", "hunter2"},
		{"sslpassword=sekret", "sekret"},
		{"passfile=/home/me/.pgpass", "/home/me/.pgpass"},
	}
	for _, tc := range cases {
		if got := sanitizeDetail(tc.in); strings.Contains(got, tc.mustNotContain) {
			t.Fatalf("sanitizeDetail(%q) = %q, still contains %q", tc.in, got, tc.mustNotContain)
		}
	}

	// Harmless text survives.
	plain := "batch commit failed at checkpoint 42"
	if got := sanitizeDetail(plain); got != plain {
		t.Fatalf("sanitizeDetail(%q) = %q", plain, got)
	}

	// Truncation and invalid UTF-8 are handled.
	long := strings.Repeat("x", maxDetailLen+100)
	if got := sanitizeDetail(long); len(got) > maxDetailLen {
		t.Fatalf("sanitizeDetail returned %d bytes, cap is %d", len(got), maxDetailLen)
	}
	if got := sanitizeDetail("bad\xff\xffutf8"); !utf8.ValidString(got) {
		t.Fatalf("sanitizeDetail output invalid UTF-8: %q", got)
	}
}

func TestProfileName(t *testing.T) {
	t.Parallel()

	if got := profileName(codec.DefaultProfile()); got != "compact" {
		t.Fatalf("profileName(default) = %q, want compact", got)
	}
	if got := profileName(codec.Profile(99)); got != "profile-99" {
		t.Fatalf("profileName(99) = %q", got)
	}
}
