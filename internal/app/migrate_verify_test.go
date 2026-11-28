package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/migration"
)

// A resumed verification run announces itself exactly once, before its first
// snapshot reaches the renderer; a fresh run never does.
func TestAnnounceResumeLogsOnceBeforeFirstSnapshot(t *testing.T) {
	t.Parallel()

	var events []string
	logf := func(format string, args ...any) {
		events = append(events, "log: "+fmt.Sprintf(format, args...))
	}
	forward := func(p migration.VerifyProgress) {
		events = append(events, fmt.Sprintf("snapshot %d", p.Checkpoint))
	}

	cb := announceResume("verify-decode", "postgres://u:secret@db/target", logf, forward)
	for _, cp := range []int64{100, 200, 300} {
		cb(migration.VerifyProgress{
			VerificationID: "run-1", Phase: "scanning", Resumed: true, Checkpoint: cp,
			ExpectedTargets: 1000, Observed: cp,
		})
	}
	if len(events) != 4 {
		t.Fatalf("events = %q, want one notice and three snapshots", events)
	}
	if !strings.HasPrefix(events[0], "log: verify-decode on ") ||
		!strings.Contains(events[0], "run-1") || !strings.Contains(events[0], "target checkpoint 100") {
		t.Fatalf("first event = %q, want the resume notice for run-1 at checkpoint 100", events[0])
	}
	if strings.Contains(events[0], "secret") {
		t.Fatalf("resume notice = %q leaks the database password", events[0])
	}
	for i, want := range []string{"snapshot 100", "snapshot 200", "snapshot 300"} {
		if events[i+1] != want {
			t.Fatalf("event %d = %q, want %q", i+1, events[i+1], want)
		}
	}

	events = nil
	fresh := announceResume("verify-decode", "postgres://db/target", logf, forward)
	fresh(migration.VerifyProgress{VerificationID: "run-2", Phase: "scanning"})
	fresh(migration.VerifyProgress{VerificationID: "run-2", Phase: "scanning", Checkpoint: 5})
	if len(events) != 2 || strings.HasPrefix(events[0], "log:") {
		t.Fatalf("fresh run events = %q, want snapshots only", events)
	}
}

func TestVerificationResumeMessage(t *testing.T) {
	t.Parallel()

	src := int64(4567)
	p := migration.VerifyProgress{
		VerificationID: "8a1f", Phase: "scanning", Resumed: true,
		Checkpoint: 1234, SourceCheckpoint: &src,
		ExpectedTargets: 5000, Observed: 1200, SourceObserved: 1210,
		Verified: 1100, Mismatched: 3, Unverifiable: 2, AssumedSingle: 95, Issues: 5,
	}
	content := verificationResumeMessage("verify-content", "postgres://db/target", p)
	for _, want := range []string{
		"verify-content on ", "resuming interrupted verification run 8a1f (phase scanning)",
		"last committed target checkpoint 1234, source checkpoint 4567",
		"cumulative progress 1200/5000 target rows observed, 1210 source rows classified",
		"1100 verified, 3 mismatched, 2 unverifiable, 95 assumed single-file; 5 issue(s) recorded",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("content notice = %q, want it to contain %q", content, want)
		}
	}

	p.SourceCheckpoint = nil
	if got := verificationResumeMessage("verify-content", "postgres://db/target", p); !strings.Contains(got, "source checkpoint none") {
		t.Fatalf("content notice without a source commit = %q, want \"source checkpoint none\"", got)
	}

	decode := verificationResumeMessage("verify-decode", "postgres://db/target", p)
	if strings.Contains(decode, "source") {
		t.Fatalf("decode notice = %q, want no source-side fields", decode)
	}
	if !strings.Contains(decode, "last committed target checkpoint 1234; cumulative progress 1200/5000 target rows observed") {
		t.Fatalf("decode notice = %q, want the target checkpoint and progress", decode)
	}

	// No target commit yet: the target side reads "none", never checkpoint 0.
	p.Checkpoint = 0
	if got := verificationResumeMessage("verify-content", "postgres://db/target", p); !strings.Contains(got, "last committed target checkpoint none, source checkpoint none") {
		t.Fatalf("content notice without any commit = %q, want \"target checkpoint none, source checkpoint none\"", got)
	}
}
