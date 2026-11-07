package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/skobkin/magnetico-web/dht"
	"github.com/skobkin/magnetico-web/dht/mainline"
	"github.com/skobkin/magnetico-web/internal/migration"
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
		default:
			// parseArgs guarantees a subcommand; this is defense in depth.
			log.Fatalln("migrate: expected a subcommand; expected: schema-init, torrents (see --help)")
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
	case errors.Is(err, migration.ErrSourceIDRange):
		return migration.ErrSourceIDRange.Error()
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
	return appendSchemaStateDetails("migration refused: "+controlledErrorDetail(err), err)
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
