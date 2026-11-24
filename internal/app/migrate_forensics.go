package app

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/skobkin/magnetico-web/internal/migration"
	"github.com/skobkin/magnetico-web/internal/migration/forensics"
	"github.com/skobkin/magnetico-web/internal/migration/progress"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/opflags"
)

// runMigrateErrorsExport implements `magnetico migrate errors export`: the
// forensic, fail-open JSONL export of the migration operational state.
// Read-only unless --include-records (which contacts the source). Exit
// codes: 0 for a published export (stdout mode is a stream, not a
// publishable artifact), 1 for any fatal failure (refusals, cap breaches,
// output collisions), 2 for an interrupted export (no output file is
// published).
func runMigrateErrorsExport(opFlags *opflags.ErrorsExportOptions) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Operator progress: same renderer discipline as every migration
	// command (non-blocking updates on their own goroutine; explicit Stop
	// before every exit path — os.Exit and log.Fatalln bypass deferred
	// calls).
	renderer := progress.New(progress.IsTTY(os.Stderr), os.Stderr, nil)
	go renderer.Run()
	defer renderer.Stop()

	res, err := migration.ExportErrors(ctx, forensics.ExportInput{
		TargetDSN: opFlags.DatabaseURL,
		Options: forensics.ExportOptions{
			OutputPath:        opFlags.Output,
			WriteManifest:     opFlags.Manifest,
			OnlyUnresolved:    opFlags.OnlyUnresolved,
			VerificationID:    opFlags.VerificationID,
			IncludeRecords:    opFlags.IncludeRecords,
			IncludeBlobs:      opFlags.IncludeBlobs,
			SourceDatabaseURL: opFlags.SourceDatabaseURL,
			Limits: forensics.Limits{
				MaxEvents:      opFlags.MaxEvents,
				MaxRecords:     opFlags.MaxRecords,
				MaxOutputBytes: opFlags.MaxOutputBytes,
				MaxBlobBytes:   opFlags.MaxBlobBytes,
			},
		},
		Progress: func(p forensics.ExportProgress) {
			renderer.Update(progress.Snapshot{
				Title:     "errors export",
				Phase:     p.Phase,
				Scanned:   p.Events,
				Successes: p.Records,
			})
		},
	})
	// The renderer's final line lands before the outcome; the deferred Stop
	// above stays as panic insurance.
	renderer.Stop()
	switch {
	case err == nil:
		log.Printf("%s", exportOutcomeMessage(opFlags.DatabaseURL, res))
	case errors.Is(err, migration.ErrInterrupted):
		log.Printf("errors export on %s interrupted before completion; "+
			"no output file was published — re-run to produce the export",
			redactDatabaseURL(opFlags.DatabaseURL))
		os.Exit(2)
	default:
		log.Printf("errors export on %s failed", redactDatabaseURL(opFlags.DatabaseURL))
		log.Fatalln(exportFailureMessage(err)) // exit 1
	}
}

// runMigrateCleanup implements `magnetico migrate cleanup`: the gated,
// fail-closed teardown of the magnetico_migration schema. Without --execute
// it is a non-destructive gate-by-gate preview (exit 0; blockers are printed
// findings). With --execute: 0 only on a definite COMMIT acknowledgment (or
// the already-cleaned no-op), 1 for a gate refusal or an ambiguous COMMIT
// (never auto-retried), 2 for an interrupt before the drop committed
// (rollback; nothing half-dropped).
func runMigrateCleanup(opFlags *opflags.CleanupOptions) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	renderer := progress.New(progress.IsTTY(os.Stderr), os.Stderr, nil)
	go renderer.Run()
	defer renderer.Stop()
	renderer.Update(progress.Snapshot{Title: "cleanup", Phase: "preview"})

	res, err := migration.Cleanup(ctx, forensics.CleanupInput{
		TargetDSN: opFlags.DatabaseURL,
		Options: forensics.CleanupOptions{
			DryRun:                     opFlags.DryRun,
			Execute:                    opFlags.Execute,
			ConfirmBackup:              opFlags.ConfirmBackup,
			ConfirmExport:              opFlags.ConfirmExport,
			EvidenceManifest:           opFlags.EvidenceManifest,
			ForceUnverified:            opFlags.ForceUnverified,
			Reason:                     opFlags.Reason,
			AllowMissingExportEvidence: opFlags.AllowMissingExportEvidence,
			ConfirmHistoryLoss:         opFlags.ConfirmHistoryLoss,
		},
	})
	// The renderer's final line lands before the outcome; the deferred Stop
	// above stays as panic insurance.
	renderer.Stop()
	switch {
	case err == nil:
		// The preview (and, on --execute, the record of what the gates saw)
		// is the operator-facing report.
		for _, line := range res.Preview {
			fmt.Println(line)
		}
		switch {
		case res.NoOp:
			log.Printf("cleanup on %s: nothing to clean up; a past cleanup cannot be "+
				"established from absence alone",
				redactDatabaseURL(opFlags.DatabaseURL))
		case res.Dropped:
			log.Printf("cleanup on %s dropped the magnetico_migration schema "+
				"(run %s, phase %s, data revision %d); the production data was not "+
				"touched — keep the backup and the export evidence",
				redactDatabaseURL(opFlags.DatabaseURL), res.RunID, res.Phase, res.DataRevision)
		case len(res.Blockers) > 0:
			log.Printf("cleanup preview on %s: %d gate(s) block execution — address them, "+
				"or re-run with --execute once the gates pass",
				redactDatabaseURL(opFlags.DatabaseURL), len(res.Blockers))
		default:
			log.Printf("cleanup preview on %s: every gate passes — re-run with --execute "+
				"and the required attestations to drop",
				redactDatabaseURL(opFlags.DatabaseURL))
		}
	case errors.Is(err, migration.ErrInterrupted):
		log.Printf("cleanup on %s interrupted before the drop transaction committed; "+
			"the transaction rolled back and nothing was dropped",
			redactDatabaseURL(opFlags.DatabaseURL))
		os.Exit(2)
	default:
		log.Printf("cleanup on %s failed", redactDatabaseURL(opFlags.DatabaseURL))
		log.Fatalln(cleanupFailureMessage(err)) // exit 1 (refusals and ambiguous outcomes alike)
	}
}

// exportOutcomeMessage composes the errors-export summary line from
// controlled content only: the redacted URL, the run's counters, the
// artifact checksum and basenames (never full paths — the operator chose
// the directory; the log lines may be captured anywhere).
func exportOutcomeMessage(rawTargetURL string, res *forensics.ExportResult) string {
	artifact := "stdout (a stream, not a publishable artifact — redirect to a file)"
	if res.OutputPath != forensics.StdoutPath {
		artifact = filepath.Base(res.OutputPath)
	}
	msg := fmt.Sprintf("errors export on %s finished: %d event(s), %d bytes, "+
		"sha256 %s, coverage %s → %s",
		redactDatabaseURL(rawTargetURL), res.Events, res.Bytes,
		hex.EncodeToString(res.SHA256[:]), res.Coverage, artifact)
	if res.ManifestPath != "" {
		msg += fmt.Sprintf(" (manifest %s)", filepath.Base(res.ManifestPath))
	}
	return msg
}

// exportFailureMessage composes the errors-export failure log line from
// controlled content only: engine-fixed option text; the fixed sentinel
// texts with their bounded engine-composed wrap details (cap names, values
// and remediation; basenames). Driver-derived text (the sanitized source
// failure detail) never reaches the log — the fixed sentinel carries the
// outcome.
func exportFailureMessage(err error) string {
	var optsErr *forensics.OptionsError
	if errors.As(err, &optsErr) {
		return "errors export: " + optsErr.Error()
	}
	switch {
	case errors.Is(err, forensics.ErrExportCapExceeded):
		return appendSentinelDetail("errors export refused: "+forensics.ErrExportCapExceeded.Error(),
			err, forensics.ErrExportCapExceeded)
	case errors.Is(err, forensics.ErrOutputExists):
		return appendSentinelDetail("errors export refused: "+forensics.ErrOutputExists.Error(),
			err, forensics.ErrOutputExists)
	case errors.Is(err, forensics.ErrSourceUnavailable):
		return "errors export refused: " + forensics.ErrSourceUnavailable.Error()
	case errors.Is(err, forensics.ErrRecordsRequireSource):
		return "errors export refused: " + forensics.ErrRecordsRequireSource.Error()
	case errors.Is(err, forensics.ErrNoMigrationHistory):
		return appendSentinelDetail("errors export stopped: "+forensics.ErrNoMigrationHistory.Error(),
			err, forensics.ErrNoMigrationHistory)
	}
	return migrationFailureMessage(err)
}

// cleanupDetailMaxBytes bounds the engine-composed refusal/ambiguous detail
// echoed by the cleanup failure lines (gate names, statuses, fixed phrases,
// counts — and basenames quoted by the manifest validator).
const cleanupDetailMaxBytes = 512

// cleanupFailureMessage composes the `migrate cleanup` failure line from
// controlled content only: engine-fixed option text; corrupt-state details
// as a bounded quoted list; the fixed refusal/ambiguous sentinel texts with
// their bounded, quoted engine-composed wrap details (quoting keeps even a
// hostile evidence basename single-line and printable); the generic
// migration rendering otherwise.
func cleanupFailureMessage(err error) string {
	var optsErr *forensics.OptionsError
	if errors.As(err, &optsErr) {
		return "cleanup: " + optsErr.Error()
	}
	var stateErr *state.StateError
	if errors.As(err, &stateErr) {
		return appendBoundedDetails("cleanup refused: "+stateErr.Err.Error(), stateErr.Details)
	}
	if errors.Is(err, forensics.ErrCleanupAmbiguous) {
		msg := "cleanup stopped: " + forensics.ErrCleanupAmbiguous.Error()
		if d := sentinelDetail(err, forensics.ErrCleanupAmbiguous); d != "" {
			return msg + " " + strconv.Quote(boundTo(d, cleanupDetailMaxBytes))
		}
		return msg
	}
	if errors.Is(err, forensics.ErrCleanupRefused) {
		msg := "cleanup refused: " + forensics.ErrCleanupRefused.Error()
		if d := sentinelDetail(err, forensics.ErrCleanupRefused); d != "" {
			return msg + " " + strconv.Quote(boundTo(d, cleanupDetailMaxBytes))
		}
		return msg
	}
	return migrationFailureMessage(err)
}
