package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	ingestserver "github.com/skobkin/magnetico-web/internal/ingest/server"
	"github.com/skobkin/magnetico-web/opflags"
)

// runIngest implements `magnetico ingest`: the authenticated HTTP ingest
// service plus the single queue consumer. It runs until SIGINT/SIGTERM and
// then shuts down gracefully (in-flight requests, current record, pools).
// Error text is reduced to controlled content: database URLs and driver
// messages never reach the log.
func runIngest(opFlags *opflags.IngestOptions) {
	if opFlags.Subcommand != "" {
		runIngestCrawler(opFlags)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if diagHook != nil { // only the diag build sets it: a loopback listener with goroutine and heap readings
		diagHook(diagSources{})
	}
	err := ingestserver.Run(ctx, ingestserver.Options{
		Listen:           opFlags.Listen,
		DatabaseURL:      opFlags.DatabaseURL,
		QueueDatabaseURL: opFlags.QueueDatabaseURL,
		QueueMaxBytes:    opFlags.QueueMaxBytes,
		CacheMaxBytes:    opFlags.CacheMaxBytes,
		CacheDisabled:    opFlags.CacheDisabled,
		AllowNonDurable:  opFlags.AllowNonDurable,
		TrustedProxies:   opFlags.TrustedProxyPrefixes,
		StatsDisabled:    opFlags.StatsDisabled,
	})
	if errors.Is(err, ingestserver.ErrShutdownIncomplete) {
		// Same convention as `crawl`: a stuck goroutine was abandoned within
		// the shutdown budget; interrupted work is redone after the next start.
		log.Println(ingestserver.ErrShutdownIncomplete)
		os.Exit(3)
	}
	if err != nil {
		log.Fatalln(appendSchemaStateDetails(fmt.Sprintf(
			"The ingest service stopped: database %s. %s",
			redactDatabaseURL(opFlags.DatabaseURL), ingestErrorDetail(err)), err))
	}
	log.Println("the ingest service stopped cleanly")
}

// ingestErrorDetail is controlledErrorDetail plus the ingest-specific
// sentinels (fixed messages only).
func ingestErrorDetail(err error) string {
	switch {
	case errors.Is(err, pgqueue.ErrSchemaMissing):
		return pgqueue.ErrSchemaMissing.Error()
	case errors.Is(err, pgqueue.ErrSchemaInvalid):
		return err.Error()
	case errors.Is(err, ingestserver.ErrCrawlerRegistry):
		return ingestserver.ErrCrawlerRegistry.Error()
	case errors.Is(err, pgqueue.ErrImportIncomplete):
		return pgqueue.ErrImportIncomplete.Error()
	case errors.Is(err, pgqueue.ErrNonDurableDatabase):
		return pgqueue.ErrNonDurableDatabase.Error()
	}
	return controlledErrorDetail(err)
}

// runMigrateIngestSchemaInit implements `magnetico migrate ingest-schema-init`:
// it creates the magnetico_ingest queue schema next to a verified 0.8.0
// schema (non-destructive, idempotent, advisory-locked).
func runMigrateIngestSchemaInit(opFlags *opflags.SchemaInitOptions) {
	if err := pgqueue.InitSchema(context.Background(), opFlags.DatabaseURL); err != nil {
		switch {
		case errors.Is(err, pgqueue.ErrSchemaMissing), errors.Is(err, pgqueue.ErrSchemaInvalid):
			log.Fatalln(appendSchemaStateDetails(fmt.Sprintf(
				"Could not initialize the ingest schema on %s. %s",
				redactDatabaseURL(opFlags.DatabaseURL), err.Error()), err))
		default:
			log.Fatalln(appendSchemaStateDetails(fmt.Sprintf(
				"Could not initialize the ingest schema on %s. %s",
				redactDatabaseURL(opFlags.DatabaseURL), controlledErrorDetail(err)), err))
		}
	}
	log.Printf("the magnetico_ingest schema on %s is ready (created now or already up to date)",
		redactDatabaseURL(opFlags.DatabaseURL))
}
