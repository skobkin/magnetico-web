package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/skobkin/magnetico-web/internal/migration"
	"github.com/skobkin/magnetico-web/internal/migration/progress"
	"github.com/skobkin/magnetico-web/opflags"
)

// runMigrateVerifyDecode implements `magnetico migrate verify-decode`: the
// full target-only decode scan of an imported database (the source is never
// contacted). Exit codes: 0 for a completed passing scan, 3 for a completed
// scan with findings (the summary is printed, not a fatal), 2 for an
// interrupted scan (the settled pages are committed; a re-run resumes), 1
// for any fatal failure.
func runMigrateVerifyDecode(opFlags *opflags.VerifyOptions) {
	res, err := runVerificationCommand("verify-decode", opFlags.DatabaseURL, func(ctx context.Context, update func(progress.Snapshot)) (*migration.VerificationResult, error) {
		return migration.VerifyDecode(ctx, migration.VerifyDecodeInput{
			TargetDSN:        opFlags.DatabaseURL,
			Workers:          int(opFlags.Workers),
			BatchSize:        int(opFlags.BatchSize),
			MaxInflightBytes: opFlags.MaxInflightBytes,
			Progress: announceResume("verify-decode", opFlags.DatabaseURL, log.Printf, func(p migration.VerifyProgress) {
				update(verifySnapshot("verify-decode", p))
			}),
		})
	})
	switch {
	case err == nil:
		log.Printf("%s", verificationOutcomeMessage("verify-decode", opFlags.DatabaseURL, res, true))
	case errors.Is(err, migration.ErrVerificationFindings):
		log.Printf("%s", verificationOutcomeMessage("verify-decode", opFlags.DatabaseURL, res, false))
		os.Exit(3)
	default:
		finishVerificationCommand("verify-decode", opFlags.DatabaseURL, err)
	}
}

// runMigrateVerifyContent implements `magnetico migrate verify-content`:
// the full source↔target equality scan. Exit codes: 0 pass, 3 completed
// with findings, 2 interrupted, 1 fatal — as with verify-decode.
func runMigrateVerifyContent(opFlags *opflags.VerifyOptions) {
	res, err := runVerificationCommand("verify-content", opFlags.DatabaseURL, func(ctx context.Context, update func(progress.Snapshot)) (*migration.VerificationResult, error) {
		return migration.VerifyContent(ctx, migration.VerifyContentInput{
			SourceDSN:        opFlags.SourceDatabaseURL,
			TargetDSN:        opFlags.DatabaseURL,
			Workers:          int(opFlags.Workers),
			BatchSize:        int(opFlags.BatchSize),
			MaxInflightBytes: opFlags.MaxInflightBytes,
			Progress: announceResume("verify-content", opFlags.DatabaseURL, log.Printf, func(p migration.VerifyProgress) {
				update(verifySnapshot("verify-content", p))
			}),
		})
	})
	switch {
	case err == nil:
		log.Printf("%s", verificationOutcomeMessage("verify-content", opFlags.DatabaseURL, res, true))
	case errors.Is(err, migration.ErrVerificationFindings):
		log.Printf("%s", verificationOutcomeMessage("verify-content", opFlags.DatabaseURL, res, false))
		os.Exit(3)
	default:
		finishVerificationCommand("verify-content", opFlags.DatabaseURL, err)
	}
}

// runVerificationCommand carries the plumbing shared by the two verification
// commands: the interrupt context, the progress renderer with an explicit
// Stop before every exit path, and the interrupted/fatal classification.
// The run callback receives a snapshot updater bound to the renderer.
func runVerificationCommand(command, rawTargetURL string, run func(ctx context.Context, update func(progress.Snapshot)) (*migration.VerificationResult, error)) (*migration.VerificationResult, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Operator progress: same renderer discipline as the import (non-blocking
	// updates on their own goroutine; explicit Stop before every exit path —
	// os.Exit and log.Fatalln bypass deferred calls).
	renderer := progress.New(progress.IsTTY(os.Stderr), os.Stderr, nil)
	go renderer.Run()
	defer renderer.Stop()
	update := func(s progress.Snapshot) { renderer.Update(s) }

	res, err := run(ctx, update)
	// The renderer's final line lands before the outcome; the deferred Stop
	// above stays as panic insurance.
	renderer.Stop()
	switch {
	case err == nil, errors.Is(err, migration.ErrVerificationFindings):
		// Both completed outcomes print the summary in the caller (res is
		// non-nil for both, per the runners' contract).
		return res, err
	case errors.Is(err, migration.ErrInterrupted):
		log.Printf("%s on %s interrupted before completion; "+
			"the settled pages are committed — re-run to resume",
			command, redactDatabaseURL(rawTargetURL))
		os.Exit(2)
	default:
		log.Fatalln(migrationFailureMessage(err)) // exit 1
	}
	return nil, err // unreachable
}

// finishVerificationCommand is the fatal exit path of the verification
// commands (kept separate so every caller's switch stays explicit about the
// completed/interrupted/fatal split).
func finishVerificationCommand(command, rawTargetURL string, err error) {
	log.Printf("%s on %s failed", command, redactDatabaseURL(rawTargetURL))
	log.Fatalln(migrationFailureMessage(err))
}

// verifySnapshot maps a verify progress snapshot onto the renderer's
// cumulative fields: observed→Scanned, verified→Successes, mismatched→
// Errors, unverifiable→RunErrors (the renderer's vocabulary is the import's;
// the command's own summary line carries the authoritative numbers).
func verifySnapshot(title string, p migration.VerifyProgress) progress.Snapshot {
	return progress.Snapshot{
		Title:         title,
		Phase:         p.Phase,
		Checkpoint:    p.Checkpoint,
		Scanned:       p.Observed,
		Successes:     p.Verified,
		Errors:        p.Mismatched,
		RunErrors:     p.Unverifiable,
		ExpectedTotal: p.ExpectedTargets,
	}
}

// announceResume wraps a verification progress callback so that the first
// snapshot of an adopted (resumed) run logs one resume notice before it is
// forwarded. The runners report once right after adopting the run and before
// reading the first page, and the renderer draws nothing before its first
// snapshot, so the notice precedes every scanning progress line; later
// snapshots are forwarded untouched.
func announceResume(command, rawTargetURL string, logf func(string, ...any), next func(migration.VerifyProgress)) func(migration.VerifyProgress) {
	var once sync.Once
	return func(p migration.VerifyProgress) {
		if p.Resumed {
			once.Do(func() { logf("%s", verificationResumeMessage(command, rawTargetURL, p)) })
		}
		next(p)
	}
}

// verificationResumeMessage composes the resume notice from controlled
// content only: the redacted URL, the run ID, its committed checkpoints and
// cumulative counters. A target checkpoint of 0 means no target commit yet
// (the keyset starts after 0, so a committed boundary is always a torrent ID
// above it) and prints as "none", like an absent source checkpoint.
func verificationResumeMessage(command, rawTargetURL string, p migration.VerifyProgress) string {
	target := "none"
	if p.Checkpoint != 0 {
		target = fmt.Sprint(p.Checkpoint)
	}
	checkpoints := "last committed target checkpoint " + target
	observed := fmt.Sprintf("%d/%d target rows observed", p.Observed, p.ExpectedTargets)
	if command == "verify-content" {
		source := "none"
		if p.SourceCheckpoint != nil {
			source = fmt.Sprint(*p.SourceCheckpoint)
		}
		checkpoints += ", source checkpoint " + source
		observed += fmt.Sprintf(", %d source rows classified", p.SourceObserved)
	}
	return fmt.Sprintf("%s on %s resuming interrupted verification run %s (phase %s): %s; cumulative progress %s — %d verified, %d mismatched, %d unverifiable, %d assumed single-file; %d issue(s) recorded",
		command, redactDatabaseURL(rawTargetURL), p.VerificationID, p.Phase, checkpoints, observed,
		p.Verified, p.Mismatched, p.Unverifiable, p.AssumedSingle, p.Issues)
}

// verificationOutcomeMessage composes the summary line of a completed
// verification scan from controlled content only: the redacted URL, the
// run's counters and fixed phrases. pass selects the PASS/FAIL wording and
// the cutover advice. Issue details are never echoed into the log — they
// live in magnetico_migration.verification_issues and are reported through
// the report path.
func verificationOutcomeMessage(command, rawTargetURL string, res *migration.VerificationResult, pass bool) string {
	expected := int64(0)
	if res.ExpectedTargets != nil {
		expected = *res.ExpectedTargets
	}
	issues := int64(0)
	for _, n := range res.IssuesBySeverity {
		issues += n
	}
	verdict := "FAIL — the discrepancies are recorded in magnetico_migration.verification_issues; resolve them and re-run the scan"
	if pass {
		verdict = "PASS"
	}
	scope := "target decode pass only — source comparison requires verify-content"
	if command == "verify-content" {
		scope = "full source↔target equality scan"
	}
	observed := fmt.Sprintf("%d/%d target rows observed", res.ObservedTargets, expected)
	if command == "verify-content" {
		observed = fmt.Sprintf("%d/%d target rows, %d source rows classified", res.ObservedTargets, expected, res.SourceObserved)
	}

	msg := fmt.Sprintf("%s on %s finished: %s (%s) — %s: %d verified, %d mismatched, %d unverifiable, %d assumed single-file (historical blind spot); %d issue(s) recorded",
		command, redactDatabaseURL(rawTargetURL), verdict, scope, observed,
		res.Verified, res.Mismatched, res.Unverifiable, res.AssumedSingle, issues)
	if res.OrphanTargetBlobs > 0 {
		msg += fmt.Sprintf("; %d orphan blob(s)", res.OrphanTargetBlobs)
	}
	if res.UnresolvedRecordErrors > 0 || res.UnresolvedIntegrity > 0 {
		msg += fmt.Sprintf("; %d unresolved import record error(s) and %d unresolved integrity incident(s) counted separately (not verification findings)",
			res.UnresolvedRecordErrors, res.UnresolvedIntegrity)
	}
	if pass {
		msg += fmt.Sprintf("; verdict applies to target data revision %d — re-run the scan after any further target change", res.StartDataRevision)
	}
	return msg
}
