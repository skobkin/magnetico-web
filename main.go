package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/skobkin/magnetico-web/dht"
	"github.com/skobkin/magnetico-web/dht/mainline"
	"github.com/skobkin/magnetico-web/internal/migration"
	"github.com/skobkin/magnetico-web/internal/migration/forensics"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/progress"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/metadata"
	"github.com/skobkin/magnetico-web/opflags"
	"github.com/skobkin/magnetico-web/persistence"
	"github.com/skobkin/magnetico-web/stats"
)

func main() {
	// opFlags is the "operational flags"
	opFlags := opflags.OpFlags{}
	command, err := opFlags.Parse()
	if err != nil {
		log.Fatalln(err.Error())
	}

	switch command {
	case "crawl":
		runCrawl(&opFlags.Crawl)
	case "web":
		notImplemented("web",
			"the authenticated web UI will be designed from the historical PHP application in a later phase")
	case "migrate":
		switch opFlags.Migrate.Subcommand {
		case "schema-init":
			runMigrateSchemaInit(&opFlags.Migrate.SchemaInit)
		case "torrents":
			runMigrateTorrents(&opFlags.Migrate.Torrents)
		case "retry-errors":
			runMigrateRetryErrors(&opFlags.Migrate.Torrents.RetryErrors)
		case "verify-decode":
			runMigrateVerifyDecode(&opFlags.Migrate.VerifyDecode)
		case "verify-content":
			runMigrateVerifyContent(&opFlags.Migrate.VerifyContent)
		case "export":
			runMigrateErrorsExport(&opFlags.Migrate.Errors.Export)
		case "cleanup":
			runMigrateCleanup(&opFlags.Migrate.Cleanup)
		default:
			// parseArgs guarantees a subcommand; this is defense in depth.
			log.Fatalln("migrate: expected a subcommand; expected: schema-init, torrents, verify-decode, verify-content, errors, cleanup (see --help)")
		}
	}
}

// notImplemented reports a reserved subcommand and exits non-zero.
func notImplemented(command, detail string) {
	fmt.Fprintf(os.Stderr, "magnetico %s: not implemented yet (%s)\n", command, detail)
	os.Exit(1)
}

// unparsableDatabaseURL is printed instead of any database URL that cannot be
// safely reduced to scheme://host/path. Raw URLs are never echoed: they
// routinely carry credentials in the userinfo or the query.
const unparsableDatabaseURL = "<unparsable database URL>"

// redactDatabaseURL renders a database URL without any credential material:
// only the scheme, host and path survive. Userinfo, query, fragment and opaque
// parts are dropped by construction (never url.URL.String, which re-emits
// them). Input that cannot be parsed into a scheme and a host maps to a fixed
// placeholder string.
func redactDatabaseURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return unparsableDatabaseURL
	}
	return fmt.Sprintf("%s://%s%s", parsed.Scheme, parsed.Host, parsed.Path)
}

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
	if !errors.As(err, &stateErr) {
		return nil
	}
	return boundedQuotedDetails(stateErr.Details)
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

// runMigrateSchemaInit implements `magnetico migrate schema-init`: it creates
// the 0.8.0 schema on a fresh database, strictly non-destructively (existing
// and legacy databases are refused, never modified).
func runMigrateSchemaInit(opFlags *opflags.SchemaInitOptions) {
	if err := persistence.InitSchema(context.Background(), opFlags.DatabaseURL); err != nil {
		log.Fatalln(schemaInitFailureMessage(opFlags.DatabaseURL, err))
	}
	// The redacted URL carries no credentials by construction.
	log.Printf("the database schema on %s is ready (created now or already up to date)",
		redactDatabaseURL(opFlags.DatabaseURL))
}

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
			Progress: func(p migration.VerifyProgress) {
				update(verifySnapshot("verify-decode", p))
			},
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
			Progress: func(p migration.VerifyProgress) {
				update(verifySnapshot("verify-content", p))
			},
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

// boundTo truncates a controlled detail line to n bytes (the catalog
// detail lists use the tighter boundDetail; the cleanup gate details are
// longer composite lines).
func boundTo(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
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

// torrentFromMetadata converts a drained metadata record into a persistence
// Torrent. The uint64→int64 boundary check happens HERE — in the crawler
// wiring, before any conversion or Torrent construction — so an oversized
// total size can never wrap into a negative value and reach persistence.
// AddTorrent independently validates its int64 input.
func torrentFromMetadata(md *metadata.Metadata) (persistence.Torrent, error) {
	if md.TotalSize > math.MaxInt64 {
		return persistence.Torrent{}, fmt.Errorf(
			"%w: total size %d exceeds the maximum supported value",
			persistence.ErrInvalidMetadata, md.TotalSize)
	}
	return persistence.Torrent{
		InfoHash:     md.InfoHash,
		Name:         md.Name,
		TotalSize:    int64(md.TotalSize),
		DiscoveredOn: md.DiscoveredOn,
	}, nil
}

// invalidMetadataWarnInterval spaces out the dropped-record warnings: a
// hostile swarm can produce invalid records far faster than they are worth
// logging.
const invalidMetadataWarnInterval = time.Hour

var invalidMetadataWarn struct {
	mu      sync.Mutex
	lastSec int64
}

// warnInvalidMetadata logs a throttled warning for a dropped record. The
// detail is controlled text only; no file paths, infohashes or error chains.
func warnInvalidMetadata(err error) {
	invalidMetadataWarn.mu.Lock()
	defer invalidMetadataWarn.mu.Unlock()
	now := time.Now().Unix()
	if invalidMetadataWarn.lastSec != 0 &&
		now-invalidMetadataWarn.lastSec < int64(invalidMetadataWarnInterval/time.Second) {
		return
	}
	invalidMetadataWarn.lastSec = now
	log.Printf("dropping an invalid metadata record: %s", controlledErrorDetail(err))
}

// dropInvalidRecord handles a rejected metadata record: the throttled
// warning plus the per-cause metric (the UTF-8/NUL bucket keeps its
// dedicated counter). Used for every ErrInvalidMetadata drop, whether the
// record was rejected at the conversion boundary or by AddTorrent; it is
// never a database failure.
func dropInvalidRecord(err error) {
	warnInvalidMetadata(err)
	if errors.Is(err, persistence.ErrInvalidTextEncoding) {
		stats.GetInstance().IncNonUTF8()
	} else {
		stats.GetInstance().IncInvalidMetadata()
	}
}

func runCrawl(opFlags *opflags.CrawlOptions) {
	// Handle Ctrl-C and SIGTERM (the timeout-based smoke harness sends
	// SIGTERM) through a context: on cancellation the event loop stops and
	// the DHT manager is terminated. Known phase-2 limitation, kept honest:
	// in-flight leech goroutines are not drained and the metadata sink's
	// Terminate is not called, so shutdown is prompt but not graceful for
	// buffered work; this task documents that limitation instead of claiming
	// a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Open the store with read-only startup verification: it fails fast on
	// missing pg_trgm, unsupported or legacy schema states. It never creates
	// or migrates anything.
	store, err := persistence.MakeDatabase(ctx, opFlags.DatabaseURL)
	if err != nil {
		// The raw URL and the error text may both carry credentials; log the
		// redacted URL and a controlled error detail instead.
		log.Fatalln(databaseOpenFailureMessage(opFlags.DatabaseURL, err))
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("Could not close database! %s\n", controlledErrorDetail(err))
		}
	}()

	if opFlags.PyroscopeURL != "" {
		profiler, err := stats.InitPyroscope(opFlags.PyroscopeURL)
		if err != nil {
			log.Fatalf("could not start pyroscope %s\n", err.Error())
		}
		defer func() {
			if err := profiler.Stop(); err != nil {
				log.Printf("Could not stop pyroscope! %s\n", err.Error())
			}
		}()
	}

	mainline.DefaultThrottleRate = int(opFlags.MaxRPS)
	trawlingManager := dht.NewManager(
		opFlags.IndexerAddrs,
		opFlags.IndexerMaxNeighbors,
		opFlags.BootstrappingNodes,
		opFlags.FilterNodesIpNets,
	)
	metadataSink := metadata.NewSink(
		time.Duration(opFlags.LeechDeadline)*time.Second,
		int(opFlags.LeechMaxN),
		opFlags.FilterNodesIpNets,
	)

	// The Event Loop
	for stopped := false; !stopped; {
		select {
		case <-ctx.Done():
			trawlingManager.Terminate()
			stopped = true

		case result, ok := <-trawlingManager.Output():
			if !ok {
				continue
			}
			infoHash := result.InfoHash()

			exists, err := store.DoesTorrentExist(ctx, infoHash[:])
			if err != nil {
				go stats.GetInstance().IncDBError(false)
			} else if !exists {
				metadataSink.Sink(result)
			}

		case md := <-metadataSink.Drain():
			torrent, err := torrentFromMetadata(&md)
			if err != nil {
				dropInvalidRecord(err)
				continue
			}
			created, _, err := store.AddTorrent(ctx, torrent, md.Files)
			switch {
			case err == nil && !created:
				// Already present: silently dropped (spec §4.5).
			case err == nil:
				// Inserted.
			case errors.Is(err, persistence.ErrInvalidMetadata):
				// Invalid metadata — including codec encode failures from
				// bad input — is dropped with a throttled warning and a
				// per-cause metric; it is never a database failure.
				dropInvalidRecord(err)
			default:
				go stats.GetInstance().IncDBError(true)
			}
		}
	}
}
