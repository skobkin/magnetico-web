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
	"sync"
	"syscall"
	"time"

	"github.com/skobkin/magnetico-web/dht"
	"github.com/skobkin/magnetico-web/dht/mainline"
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
		notImplemented("migrate",
			"the offline legacy database migrator follows phase 3, see docs/specs/postgresql-persistence-v2.md")
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
	}
	return fmt.Sprintf("unexpected error (%T)", err)
}

// databaseOpenFailureMessage composes the database-open failure log line from
// controlled content only: the redacted URL and the controlled error detail.
func databaseOpenFailureMessage(rawURL string, err error) string {
	return fmt.Sprintf(
		"Could not open the database %s. %s",
		redactDatabaseURL(rawURL), controlledErrorDetail(err),
	)
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
				warnInvalidMetadata(err)
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
				// bad input — is dropped with a throttled warning; it is
				// never a database failure.
				warnInvalidMetadata(err)
			default:
				go stats.GetInstance().IncDBError(true)
			}
		}
	}
}
