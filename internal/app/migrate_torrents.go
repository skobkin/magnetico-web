package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/skobkin/magnetico-web/internal/migration"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/progress"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/opflags"
)

// runMigrateTorrents implements `magnetico migrate torrents`: the offline
// import of an original magnetico v0.12.0 database. Exit codes: 0 only for
// a completed migration (import_complete or import_complete_with_errors —
// neither means the data is verified), 2 for an interrupted run (the settled
// prefix is committed; a re-run resumes), 1 for any fatal failure.
func runMigrateTorrents(opFlags *opflags.TorrentsOptions) {
	// SIGINT/SIGTERM interrupt the run gracefully: in-flight records settle,
	// the last committed checkpoint stands, and the next run resumes.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Operator progress: a rolling single line on a TTY, periodic log lines
	// otherwise (every 30 s, plus immediately on phase changes and index
	// steps). Update never blocks the import pipeline; the renderer writes
	// on its own goroutine to stderr, leaving stdout untouched.
	renderer := progress.New(progress.IsTTY(os.Stderr), os.Stderr, nil)
	go renderer.Run()
	defer renderer.Stop()

	var lastProgress migration.Progress
	err := migration.ImportTorrents(ctx, migration.TorrentsInput{
		SourceDSN:        opFlags.SourceDatabaseURL,
		TargetDSN:        opFlags.DatabaseURL,
		DeferIndexes:     opFlags.DeferSecondaryIndexes,
		Workers:          int(opFlags.Workers),
		BatchSize:        int(opFlags.BatchSize),
		MaxInflightBytes: opFlags.MaxInflightBytes,
		Progress: func(p migration.Progress) {
			lastProgress = p
			renderer.Update(progress.Snapshot{
				Phase:         string(p.Phase),
				Checkpoint:    p.Checkpoint,
				Scanned:       p.Scanned,
				Successes:     p.Successes,
				Errors:        p.Errors,
				Blobs:         p.Blobs,
				RunErrors:     p.RunErrors,
				ExpectedTotal: p.ExpectedTotal,
				IndexStep:     p.IndexStep,
				IndexTotal:    p.IndexTotal,
				IndexName:     p.IndexName,
			})
		},
	})
	// The renderer's final line lands before the outcome. Stop is
	// idempotent (the deferred call above stays as panic insurance), but an
	// explicit call here is required: the exit paths below — os.Exit and
	// log.Fatalln — bypass deferred calls and would leave the run without
	// its final progress line (on a TTY, without the line-terminating
	// newline).
	renderer.Stop()
	switch {
	case err == nil:
		// The redacted URL carries no credentials by construction.
		log.Printf("torrent migration on %s finished in phase %s "+
			"(%d scanned, %d migrated, %d record errors, %d unresolved integrity incidents); "+
			"neither completion phase means the data is verified — run verification before cutover",
			redactDatabaseURL(opFlags.DatabaseURL), lastProgress.Phase,
			lastProgress.Scanned, lastProgress.Successes, lastProgress.Errors, lastProgress.RunErrors)
	case errors.Is(err, migration.ErrInterrupted):
		log.Printf("torrent migration interrupted before completion; " +
			"the settled prefix is committed — re-run to resume from the checkpoint")
		os.Exit(2)
	default:
		log.Fatalln(migrationFailureMessage(err)) // exit 1
	}
}

// runMigrateRetryErrors implements `magnetico migrate torrents retry-errors`:
// the transactional repair pass over the unresolved record errors of a
// completed import. Exit codes: 0 for a clean pass (no unresolved records
// remain), 3 for a completed pass with still-invalid or missing records
// (the summary is printed), 2 for an interrupted pass, 1 for any fatal
// failure (drift refusal, precondition conflict, corrupt state).
func runMigrateRetryErrors(opFlags *opflags.RetryErrorsOptions) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	renderer := progress.New(progress.IsTTY(os.Stderr), os.Stderr, nil)
	go renderer.Run()
	defer renderer.Stop()

	res, err := migration.RetryErrors(ctx, migration.RetryErrorsInput{
		SourceDSN:           opFlags.SourceDatabaseURL,
		TargetDSN:           opFlags.DatabaseURL,
		Workers:             int(opFlags.Workers),
		BatchSize:           int(opFlags.BatchSize),
		MaxInflightBytes:    opFlags.MaxInflightBytes,
		AcceptSourceRepairs: opFlags.AcceptSourceRepairs,
		Progress: func(p migration.RetryProgress) {
			renderer.Update(progress.Snapshot{
				Title:         "retry-errors",
				Phase:         p.Phase,
				Checkpoint:    p.Cursor,
				Scanned:       p.Attempted,
				Successes:     p.Fixed,
				Errors:        p.StillInvalid + p.Missing,
				ExpectedTotal: p.UnresolvedAtStart,
			})
		},
	})
	// The renderer's final line lands before the outcome; the deferred Stop
	// above stays as panic insurance.
	renderer.Stop()
	switch {
	case err == nil:
		log.Printf("%s", retryOutcomeMessage(opFlags.SourceDatabaseURL, opFlags.DatabaseURL, res))
	case errors.Is(err, migration.ErrRetryRemaining):
		if res == nil {
			log.Fatalln(migrationFailureMessage(err)) // unreachable per the runners' contract
		}
		log.Printf("%s", retryOutcomeMessage(opFlags.SourceDatabaseURL, opFlags.DatabaseURL, res))
		os.Exit(3)
	case errors.Is(err, migration.ErrInterrupted):
		log.Printf("retry-errors on %s interrupted before completion; "+
			"the settled attempts are committed — re-run to resume",
			redactDatabaseURL(opFlags.DatabaseURL))
		os.Exit(2)
	default:
		log.Printf("retry-errors on %s failed", redactDatabaseURL(opFlags.DatabaseURL))
		log.Fatalln(migrationFailureMessage(err)) // exit 1
	}
}

// retryOutcomeMessage composes the summary line of a completed retry pass
// from controlled content only: the redacted URLs, the pass's counters and
// fixed phrases.
func retryOutcomeMessage(rawSourceURL, rawTargetURL string, res *migration.RetryResult) string {
	msg := fmt.Sprintf("retry-errors on %s (source %s) finished: %d attempted, %d fixed, %d still invalid, %d missing source; %d unresolved record error(s) remain",
		redactDatabaseURL(rawTargetURL), redactDatabaseURL(rawSourceURL),
		res.Attempted, res.Fixed, res.StillInvalid, res.Missing, res.UnresolvedRemaining)
	if res.PhaseFlipped {
		msg += "; import phase advanced to import_complete"
	}
	if res.AcceptedSourceRepair {
		msg += " (authorized source repair — provenance recorded in retry_runs)"
	}
	if res.UnresolvedRemaining == 0 {
		msg += "; the target data revision advanced — run verify-content before cutover"
	} else {
		msg += "; correct the source records and re-run retry-errors; verify-content stays required before cutover"
	}
	return msg
}

// migrationFailureMessage composes the `migrate torrents` failure log line
// from controlled content only: a FatalError's text is sanitized at
// construction (stage, attempted ID range, checkpoint, SQLSTATE — credential
// material redacted); migration config errors carry static text; typed
// migration/state refusals contribute their fixed sentinel text with
// bounded catalog details; anything else contributes only its Go type.
func migrationFailureMessage(err error) string {
	var fatal *migration.FatalError
	if errors.As(err, &fatal) {
		return fatal.Error()
	}
	var configErr *migration.ConfigError
	if errors.As(err, &configErr) {
		return configErr.Error()
	}
	var stateErr *state.StateError
	if errors.As(err, &stateErr) {
		return appendBoundedDetails("migration refused: "+stateErr.Err.Error(), stateErr.Details)
	}
	var srcStateErr *original.SourceStateError
	if errors.As(err, &srcStateErr) {
		// Source-schema refusals name the detected catalog objects (same
		// trust class as target SchemaStateError details: schema-qualified
		// identifiers, never driver text). Render the fixed sentinel plus a
		// bounded, quoted detail list instead of the raw Error(), whose
		// detail length is unbounded.
		return appendBoundedDetails("migration refused: "+srcStateErr.Err.Error(), srcStateErr.Details)
	}
	// 04B refusals whose wrap suffix is composed by this codebase from
	// controlled content (counts, database names, ID bounds, fixed advice):
	// surface it through the bounded, prefix-verified extraction so the
	// operator sees the actionable detail, not just the sentinel.
	switch {
	case errors.Is(err, migration.ErrSourceDrift):
		return appendSentinelDetail("migration refused: "+migration.ErrSourceDrift.Error(), err, migration.ErrSourceDrift)
	case errors.Is(err, target.ErrRetryConflict):
		return appendSentinelDetail("migration refused: "+target.ErrRetryConflict.Error(), err, target.ErrRetryConflict)
	case errors.Is(err, target.ErrRunNotComplete):
		return appendSentinelDetail("migration refused: "+target.ErrRunNotComplete.Error(), err, target.ErrRunNotComplete)
	}
	return appendSchemaStateDetails("migration refused: "+controlledErrorDetail(err), err)
}
