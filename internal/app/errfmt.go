package app

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/skobkin/magnetico-web/internal/migration"
	"github.com/skobkin/magnetico-web/internal/migration/forensics"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/persistence"
)

// controlledErrorDetail is the only error text allowed in database log lines.
// It never returns err.Error(), not even for a recognized sentinel: driver
// errors and wrapped sentinels can carry credentials anywhere in their text.
// A recognized sentinel contributes its own fixed message; anything else
// contributes only the error's Go type, which carries no runtime content.
func controlledErrorDetail(err error) string {
	switch {
	case errors.Is(err, persistence.ErrStorageNotImplemented):
		return persistence.ErrStorageNotImplemented.Error()
	case errors.Is(err, persistence.ErrMissingExtension):
		return persistence.ErrMissingExtension.Error()
	case errors.Is(err, persistence.ErrLegacySchema):
		return persistence.ErrLegacySchema.Error()
	case errors.Is(err, persistence.ErrUnsupportedSchema):
		return persistence.ErrUnsupportedSchema.Error()
	case errors.Is(err, persistence.ErrInvalidMetadata):
		return persistence.ErrInvalidMetadata.Error()
	case errors.Is(err, persistence.ErrStorageIO):
		return persistence.ErrStorageIO.Error()
	case errors.Is(err, target.ErrSourceRequired):
		return target.ErrSourceRequired.Error()
	case errors.Is(err, target.ErrSameDatabase):
		return target.ErrSameDatabase.Error()
	case errors.Is(err, target.ErrTargetIdentityUnknown):
		return target.ErrTargetIdentityUnknown.Error()
	case errors.Is(err, target.ErrTargetNotEmpty):
		return target.ErrTargetNotEmpty.Error()
	case errors.Is(err, target.ErrIdentityExhausted):
		return target.ErrIdentityExhausted.Error()
	case errors.Is(err, state.ErrLockBusy):
		return state.ErrLockBusy.Error()
	case errors.Is(err, state.ErrNoRunState):
		return state.ErrNoRunState.Error()
	case errors.Is(err, state.ErrRunStateCorrupt):
		return state.ErrRunStateCorrupt.Error()
	case errors.Is(err, target.ErrRunNotComplete):
		return target.ErrRunNotComplete.Error()
	case errors.Is(err, target.ErrRetryConflict):
		return target.ErrRetryConflict.Error()
	case errors.Is(err, migration.ErrSourceIDRange):
		return migration.ErrSourceIDRange.Error()
	case errors.Is(err, migration.ErrSourceDrift):
		return migration.ErrSourceDrift.Error()
	case errors.Is(err, migration.ErrRetryRemaining):
		return migration.ErrRetryRemaining.Error()
	case errors.Is(err, migration.ErrVerificationFindings):
		return migration.ErrVerificationFindings.Error()
	case errors.Is(err, forensics.ErrNoMigrationHistory):
		return forensics.ErrNoMigrationHistory.Error()
	case errors.Is(err, forensics.ErrOutputExists):
		return forensics.ErrOutputExists.Error()
	case errors.Is(err, forensics.ErrExportCapExceeded):
		return forensics.ErrExportCapExceeded.Error()
	case errors.Is(err, forensics.ErrRecordsRequireSource):
		return forensics.ErrRecordsRequireSource.Error()
	case errors.Is(err, forensics.ErrSourceUnavailable):
		return forensics.ErrSourceUnavailable.Error()
	case errors.Is(err, forensics.ErrEvidenceRejected):
		return forensics.ErrEvidenceRejected.Error()
	case errors.Is(err, forensics.ErrCleanupRefused):
		return forensics.ErrCleanupRefused.Error()
	case errors.Is(err, forensics.ErrCleanupAmbiguous):
		return forensics.ErrCleanupAmbiguous.Error()
	case errors.Is(err, forensics.ErrLockLost):
		return forensics.ErrLockLost.Error()
	}
	return fmt.Sprintf("unexpected error (%T)", err)
}

// schemaStateDetailLimits bound the catalog-object list surfaced to the
// operator: at most this many entries, each at most this many bytes before
// quoting.
const (
	schemaStateDetailMaxItems = 8
	schemaStateDetailMaxBytes = 128
)

// schemaStateDetails returns a bounded, quoted copy of the catalog-object
// list carried by a SchemaStateError, or nil for any other error.
func schemaStateDetails(err error) []string {
	var stateErr *persistence.SchemaStateError
	if errors.As(err, &stateErr) {
		return boundedQuotedDetails(stateErr.Details)
	}
	var webErr *webschema.StateError
	if errors.As(err, &webErr) {
		return boundedQuotedDetails(webErr.Details)
	}
	return nil
}

// boundedQuotedDetails quotes and bounds a catalog-object detail list —
// schema-qualified names built from catalog data, never driver error text
// and never credentials — so the log line stays single-line and printable
// whatever an object is named.
func boundedQuotedDetails(details []string) []string {
	out := make([]string, 0, min(len(details), schemaStateDetailMaxItems+1))
	for i, d := range details {
		if i == schemaStateDetailMaxItems {
			out = append(out, fmt.Sprintf("(+%d more)", len(details)-i))
			break
		}
		if len(d) > schemaStateDetailMaxBytes {
			d = d[:schemaStateDetailMaxBytes]
		}
		out = append(out, strconv.Quote(d))
	}
	return out
}

// appendBoundedDetails appends a bounded, quoted detail list to a failure
// message; an empty list contributes nothing.
func appendBoundedDetails(msg string, details []string) string {
	quoted := boundedQuotedDetails(details)
	if len(quoted) == 0 {
		return msg
	}
	return msg + " " + strings.Join(quoted, " ")
}

// appendSchemaStateDetails appends the bounded catalog-object list of a
// SchemaStateError to a failure message; other errors contribute nothing.
func appendSchemaStateDetails(msg string, err error) string {
	details := schemaStateDetails(err)
	if len(details) == 0 {
		return msg
	}
	return msg + " " + strings.Join(details, " ")
}

// databaseOpenFailureMessage composes the database-open failure log line from
// controlled content only: the redacted URL, the controlled error detail and,
// for a schema-state refusal, the bounded escaped catalog-object list.
func databaseOpenFailureMessage(rawURL string, err error) string {
	return appendSchemaStateDetails(fmt.Sprintf(
		"Could not open the database %s. %s",
		redactDatabaseURL(rawURL), controlledErrorDetail(err),
	), err)
}

// schemaInitFailureMessage composes the `migrate schema-init` failure log
// line from controlled content only, with the same schema-refusal details.
func schemaInitFailureMessage(rawURL string, err error) string {
	return appendSchemaStateDetails(fmt.Sprintf(
		"Could not initialize the database schema on %s. %s",
		redactDatabaseURL(rawURL), controlledErrorDetail(err),
	), err)
}

// boundTo truncates a controlled detail line to n bytes (the catalog
// detail lists use the tighter boundDetail; the cleanup gate details are
// longer composite lines).
func boundTo(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// sentinelDetail extracts the bounded detail suffix of a wrapped sentinel
// error, accepting only the two wrap shapes this codebase composes
// (`<sentinel>: <detail>` and `<sentinel> (<detail>)`). The detail is
// controlled content by construction — numbers, database names, fixed
// phrases — and the strict prefix check guarantees no other error can leak
// its text through here.
func sentinelDetail(err error, sentinel error) string {
	text := err.Error()
	if rest, ok := strings.CutPrefix(text, sentinel.Error()+": "); ok {
		return boundDetail(rest)
	}
	if rest, ok := strings.CutPrefix(text, sentinel.Error()+" ("); ok && strings.HasSuffix(rest, ")") {
		return boundDetail(strings.TrimSuffix(rest, ")"))
	}
	return ""
}

// boundDetail truncates a controlled detail line to the same per-item bound
// as the catalog detail lists.
func boundDetail(s string) string {
	if len(s) > schemaStateDetailMaxBytes {
		return s[:schemaStateDetailMaxBytes]
	}
	return s
}

// appendSentinelDetail appends the bounded wrap-suffix detail of a
// recognized sentinel error to a failure message; an empty detail
// contributes nothing.
func appendSentinelDetail(msg string, err error, sentinel error) string {
	if d := sentinelDetail(err, sentinel); d != "" {
		return appendBoundedDetails(msg, []string{d})
	}
	return msg
}
