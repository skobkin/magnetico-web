package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/migration"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/opflags"
)

// Source-schema refusals must reach the operator as their fixed sentinel
// plus a bounded, quoted catalog-detail list — never as an opaque
// "unexpected error (*original.SourceStateError)" that hides whether the
// source is a v1 fork or has an unexpected layout. (Review finding:
// Codex pass 1, #6.)
func TestMigrationFailureMessageRendersSourceStateRefusals(t *testing.T) {
	v1Fork := &original.SourceStateError{
		Err:     original.ErrSourceV1Fork,
		Details: []string{`relation "magneticod.files_p0"`, `migrations schema_version 1`},
	}
	got := migrationFailureMessage(v1Fork)
	if !strings.Contains(got, original.ErrSourceV1Fork.Error()) ||
		!strings.Contains(got, "magneticod.files_p0") {
		t.Fatalf("migrationFailureMessage() = %q, want the v1-fork sentinel and the catalog details", got)
	}
	if strings.Contains(got, "unexpected error") {
		t.Fatalf("migrationFailureMessage() = %q, want the typed refusal, not %T", got, v1Fork)
	}

	// Detail bounding: many/long catalog names are quoted and cut at the
	// bounded list size, never echoed raw and unbounded.
	var longDetails []string
	for i := 0; i < 20; i++ {
		longDetails = append(longDetails, strings.Repeat("n", 300)+fmt.Sprint(i))
	}
	overlong := &original.SourceStateError{Err: original.ErrSourceUnsupported, Details: longDetails}
	got = migrationFailureMessage(overlong)
	if len(got) > 4096 {
		t.Fatalf("failure message is %d bytes, want a bounded diagnostic", len(got))
	}
	if strings.Contains(got, strings.Repeat("n", 300)) {
		t.Fatalf("failure message echoes an unbounded detail verbatim: %q", got)
	}
}

// verifyResult builds a VerificationResult with the shared bookkeeping the
// message builders rely on.
func verifyResult() *migration.VerificationResult {
	expected := int64(3)
	return &migration.VerificationResult{
		Kind:                   migration.VerifyKindDecode,
		VerificationID:         "00000000-0000-0000-0000-000000000001",
		Seq:                    1,
		Pass:                   true,
		StartDataRevision:      7,
		ExpectedTargets:        &expected,
		ObservedTargets:        3,
		SourceObserved:         3,
		Verified:               2,
		AssumedSingle:          1,
		IssuesBySeverity:       map[string]int64{"info": 1},
		UnresolvedRecordErrors: 4,
		UnresolvedIntegrity:    2,
	}
}

// The summary lines must scope what a scan actually proved: verify-decode is
// explicitly a target decode pass only, verify-content is the full equality
// scan, the assumed-single blind spot and the separately-counted import
// registry are always surfaced, and a passing verdict states its data
// revision (state staleness).
func TestVerificationOutcomeMessageScopesAndStaleness(t *testing.T) {
	res := verifyResult()

	decodePass := verificationOutcomeMessage("verify-decode", "postgres://u:p@localhost/t", res, true)
	if !strings.Contains(decodePass, "target decode pass only") {
		t.Fatalf("decode pass message = %q, want the target-only scope phrase", decodePass)
	}
	if strings.Contains(decodePass, "postgres://u:p@localhost/t") {
		t.Fatalf("decode pass message echoes the raw URL: %q", decodePass)
	}

	contentPass := verificationOutcomeMessage("verify-content", "postgres://u:p@localhost/t", res, true)
	if !strings.Contains(contentPass, "full source↔target equality") ||
		!strings.Contains(contentPass, "3 source rows classified") {
		t.Fatalf("content pass message = %q, want the equality scope and the source count", contentPass)
	}

	if !strings.Contains(decodePass, "3/3 target rows observed") {
		t.Fatalf("decode pass message = %q, want the observed count", decodePass)
	}

	for _, msg := range []string{decodePass, contentPass} {
		for _, want := range []string{
			"1 assumed single-file", "1 issue(s)",
			"4 unresolved import record error(s)", "2 unresolved integrity incident(s)",
			"data revision 7", "postgres://localhost/t",
		} {
			if !strings.Contains(msg, want) {
				t.Fatalf("message %q missing %q", msg, want)
			}
		}
	}

	fail := verificationOutcomeMessage("verify-decode", "postgres://u:p@localhost/t", res, false)
	if !strings.Contains(fail, "FAIL") || !strings.Contains(fail, "verification_issues") {
		t.Fatalf("fail message = %q, want the FAIL verdict and the issues-table pointer", fail)
	}
	if strings.Contains(fail, "data revision 7") {
		t.Fatalf("fail message claims a current verdict revision: %q", fail)
	}
}

// The retry summary must state the remaining-record advice and the cutover
// rule, and record an authorized repair as provenance.
func TestRetryOutcomeMessageAdviceAndProvenance(t *testing.T) {
	clean := retryOutcomeMessage("postgres://u:p@localhost/legacy", "postgres://u:p@localhost/t", &migration.RetryResult{
		Attempted: 3, Fixed: 2, StillInvalid: 1, Missing: 0,
		UnresolvedAtStart: 3, UnresolvedRemaining: 0, PhaseFlipped: true, AcceptedSourceRepair: true,
	})
	for _, want := range []string{
		"3 attempted", "2 fixed", "1 still invalid", "0 unresolved record error(s) remain",
		"import_complete", "authorized source repair", "run verify-content before cutover",
		"postgres://localhost/t", "postgres://localhost/legacy",
	} {
		if !strings.Contains(clean, want) {
			t.Fatalf("clean retry message %q missing %q", clean, want)
		}
	}
	if strings.Contains(clean, "postgres://u:p@localhost") {
		t.Fatalf("retry message echoes a raw URL: %q", clean)
	}

	remaining := retryOutcomeMessage("postgres://u:p@localhost/legacy", "postgres://u:p@localhost/t", &migration.RetryResult{
		Attempted: 3, Fixed: 0, StillInvalid: 3, Missing: 0,
		UnresolvedAtStart: 3, UnresolvedRemaining: 3,
	})
	if !strings.Contains(remaining, "re-run retry-errors") || !strings.Contains(remaining, "3 unresolved record error(s) remain") {
		t.Fatalf("remaining retry message = %q", remaining)
	}
}

// sentinelDetail must accept only the two wrap shapes this codebase
// composes, and only after a full sentinel-prefix match — a foreign error
// whose text merely begins with similar words must never leak its text.
func TestSentinelDetailExtraction(t *testing.T) {
	sentinel := migration.ErrSourceDrift

	colon := fmt.Errorf("%w: error row for source 42 vanished", target.ErrRetryConflict)
	if got := sentinelDetail(colon, target.ErrRetryConflict); got != "error row for source 42 vanished" {
		t.Fatalf("colon detail = %q", got)
	}

	paren := fmt.Errorf("%w (source torrents 3→4, id range [1,3]→[1,4]; repair the source or pass --accept-source-repairs)", sentinel)
	got := sentinelDetail(paren, sentinel)
	if !strings.Contains(got, "--accept-source-repairs") || strings.HasPrefix(got, "(") {
		t.Fatalf("parenthesized detail = %q", got)
	}

	// A full-sentinel prefix followed by free text (neither wrap shape)
	// contributes nothing — the detail only surfaces through the two
	// composed shapes.
	if d := sentinelDetail(errors.New("source fingerprint differs from the recorded import fingerprint and something else entirely"), sentinel); d != "" {
		t.Fatalf("non-wrap-shaped suffix leaked a detail: %q", d)
	}

	// A prefix-mismatched error contributes nothing.
	if d := sentinelDetail(errors.New("totally unrelated"), sentinel); d != "" {
		t.Fatalf("foreign error leaked a detail: %q", d)
	}

	// Overlong details are bounded.
	long := fmt.Errorf("%w (%s)", sentinel, strings.Repeat("x", 500))
	if d := sentinelDetail(long, sentinel); len(d) != 128 {
		t.Fatalf("bounded detail length = %d, want 128", len(d))
	}
}

// The 04B refusals must render as their typed sentinels with the actionable
// wrap detail — never as "unexpected error".
func TestMigrationFailureMessageRenders04BRefusals(t *testing.T) {
	drift := fmt.Errorf("%w (source torrents 3→4, id range [1,3]→[1,4]; repair the source or pass --accept-source-repairs)", migration.ErrSourceDrift)
	got := migrationFailureMessage(drift)
	if !strings.Contains(got, migration.ErrSourceDrift.Error()) ||
		!strings.Contains(got, "--accept-source-repairs") ||
		strings.Contains(got, "unexpected error") {
		t.Fatalf("drift refusal = %q", got)
	}

	conflict := fmt.Errorf("%w: target already has torrent id 42", target.ErrRetryConflict)
	got = migrationFailureMessage(conflict)
	if !strings.Contains(got, target.ErrRetryConflict.Error()) || !strings.Contains(got, "torrent id 42") {
		t.Fatalf("conflict refusal = %q", got)
	}

	notComplete := fmt.Errorf("%w (phase %q — finish `migrate torrents` first)", target.ErrRunNotComplete, state.PhaseImporting)
	got = migrationFailureMessage(notComplete)
	if !strings.Contains(got, target.ErrRunNotComplete.Error()) || !strings.Contains(got, "migrate torrents") {
		t.Fatalf("not-complete refusal = %q", got)
	}

	opFlags := &opflags.VerifyOptions{}
	if err := (errors.Join(target.ErrSourceRequired)); err != nil {
		got = migrationFailureMessage(err)
		if !strings.Contains(got, target.ErrSourceRequired.Error()) {
			t.Fatalf("source-required refusal = %q", got)
		}
	}
	_ = opFlags
}
