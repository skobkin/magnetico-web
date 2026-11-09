package migration

// Parity test (04C slice 5): forensics.DecodeFinding must classify codec
// decode errors EXACTLY like this package's decodeErrorIssueCode — the
// forensic decode verdicts and the verification issue codes must never
// drift apart. The duplication is deliberate (zero churn to green 04B
// code); this test is the drift alarm.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/forensics"
)

func TestDecodeFindingParityWithForensics(t *testing.T) {
	t.Parallel()
	limitErr := &codec.LimitError{Limit: codec.LimitStoredBytes, Actual: 10, Allowed: 5}
	wrap := func(err error) error { return fmt.Errorf("while decoding record: %w", err) }
	cases := []struct {
		name string
		err  error
	}{
		{"codec limit", wrap(limitErr)},
		{"wire version", wrap(codec.ErrUnsupportedVersion)},
		{"wire serializer", wrap(codec.ErrUnsupportedSerializer)},
		{"wire compressor", wrap(codec.ErrUnsupportedCompressor)},
		{"truncated", wrap(codec.ErrTruncated)},
		{"trailing bytes", wrap(codec.ErrTrailingBytes)},
		{"noncanonical order", wrap(codec.ErrNonCanonicalOrder)},
		{"corrupt compressed", wrap(codec.ErrCorruptCompressed)},
		{"overflow", wrap(codec.ErrOverflow)},
		{"negative size", wrap(codec.ErrNegativeSize)},
		{"invalid count", wrap(codec.ErrInvalidCount)},
		{"nonmaximal prefix", wrap(codec.ErrNonMaximalPrefix)},
		{"prefix out of range", wrap(codec.ErrPrefixOutOfRange)},
		{"duplicate dir name", wrap(codec.ErrDuplicateDirName)},
		{"empty path component", wrap(codec.ErrEmptyPathComponent)},
		{"path component separator", wrap(codec.ErrPathComponentSeparator)},
		{"unknown error", wrap(errors.New("something entirely unexpected"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			migCode, migDetails := decodeErrorIssueCode(tc.err)
			fCode, fDetails := forensics.DecodeFinding(tc.err)
			if migCode != fCode {
				t.Fatalf("codes diverge: migration=%q forensics=%q", migCode, fCode)
			}
			// Limit diagnostics must carry the same bounded fields.
			var le *codec.LimitError
			if errors.As(tc.err, &le) {
				for _, k := range []string{"limit", "actual", "allowed"} {
					if _, ok := migDetails[k]; !ok {
						t.Fatalf("migration details missing %q: %v", k, migDetails)
					}
					if _, ok := fDetails[k]; !ok {
						t.Fatalf("forensics details missing %q: %v", k, fDetails)
					}
				}
				if migDetails["limit"] != fDetails["limit"] ||
					migDetails["actual"] != fDetails["actual"] ||
					migDetails["allowed"] != fDetails["allowed"] {
					t.Fatalf("limit details diverge: migration=%v forensics=%v", migDetails, fDetails)
				}
			}
		})
	}
}
