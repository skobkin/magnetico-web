package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/ingest/crawlercli"
	"github.com/skobkin/magnetico-web/internal/ingest/crawlers"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/opflags"
)

// crawlerErrorDetail maps a command failure to controlled text: the store's
// sentinels carry fixed messages; a missing privilege and an invalid schema
// are named; everything else goes through the database-error reduction.
func crawlerErrorDetail(err error) string {
	for _, known := range []error{crawlers.ErrNameTaken, crawlers.ErrNotFound, crawlers.ErrInvalidName, pgqueue.ErrSchemaMissing} {
		if errors.Is(err, known) {
			return err.Error()
		}
	}
	var invalid *pgqueue.SchemaInvalidError
	if errors.As(err, &invalid) {
		return invalid.Error()
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "42501" {
		return "the database role used by this command lacks a privilege on magnetico_ingest.crawlers " +
			"(it needs SELECT, INSERT and UPDATE; see docs/container.md)"
	}
	if errors.Is(err, context.Canceled) {
		return "interrupted"
	}
	return controlledErrorDetail(err)
}

// runIngestCrawler implements `magnetico ingest crawler …`. The result and the
// one-time token go to stdout; failures go to stderr with controlled text, and
// no database URL, driver text or digest is ever printed.
func runIngestCrawler(o *opflags.IngestOptions) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := o.QueueDatabaseURL
	if dsn == "" {
		dsn = o.DatabaseURL
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("Could not run the crawler command: database %s. %s",
			redactDatabaseURL(dsn), controlledErrorDetail(err))
	}
	defer pool.Close()

	if err := pgqueue.VerifySchema(ctx, pool); err != nil {
		pool.Close()
		log.Fatalln(appendSchemaStateDetails(fmt.Sprintf("The crawler command cannot run: database %s. %s",
			redactDatabaseURL(dsn), crawlerErrorDetail(err)), err))
	}
	env := &crawlercli.Env{Store: crawlers.New(pool), Out: os.Stdout}
	if err := dispatchIngestCrawler(ctx, env, o); err != nil {
		pool.Close()
		log.Fatalln("The crawler command failed: " + crawlerErrorDetail(err))
	}
}

func dispatchIngestCrawler(ctx context.Context, e *crawlercli.Env, o *opflags.IngestOptions) error {
	c := &o.Crawler
	switch o.Subcommand {
	case "crawler add":
		return crawlercli.Add(ctx, e, c.Add.Args.Name)
	case "crawler list":
		return crawlercli.List(ctx, e)
	case "crawler disable":
		return crawlercli.SetEnabled(ctx, e, c.Disable.Args.Name, false)
	case "crawler enable":
		return crawlercli.SetEnabled(ctx, e, c.Enable.Args.Name, true)
	case "crawler rotate-token":
		return crawlercli.RotateToken(ctx, e, c.RotateToken.Args.Name)
	}
	return fmt.Errorf("unknown crawler command %q", o.Subcommand)
}
