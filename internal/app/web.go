package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/skobkin/magnetico-web/internal/web/assemble"
	webauth "github.com/skobkin/magnetico-web/internal/web/auth"
	webserver "github.com/skobkin/magnetico-web/internal/web/server"
	webdb "github.com/skobkin/magnetico-web/internal/web/webdb"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/opflags"
)

// runWeb implements `magnetico web` (the HTTP foundation). It
// verifies the database read-only, serves until SIGINT/SIGTERM and then shuts
// down within the configured grace period. Error text is reduced to
// controlled content: database URLs and driver messages never reach the log.
func runWeb(o *opflags.WebOptions) {
	logger, err := webserver.NewLogger(os.Stderr, o.LogLevel, o.LogFormat)
	if err != nil {
		log.Fatalln(err.Error()) // unreachable: validated by opflags
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	backend, err := webdb.Open(ctx, o.DatabaseURL, o.PoolMaxConns)
	if err != nil {
		log.Fatalln(fmt.Sprintf("The web service could not start: database %s. %s",
			redactDatabaseURL(o.DatabaseURL), controlledErrorDetail(err)))
	}
	srv, err := assemble.Web(o, backend, logger, diagHook)
	if err != nil {
		backend.Close()
		log.Fatalln("The web service could not start: " + err.Error())
	}

	err = srv.Run(ctx)
	if errors.Is(err, webserver.ErrShutdownIncomplete) {
		// Same convention as `crawl` and `ingest`.
		log.Println(webserver.ErrShutdownIncomplete)
		os.Exit(3)
	}
	if err != nil {
		log.Fatalln(appendSchemaStateDetails(fmt.Sprintf(
			"The web service stopped: database %s. %s", redactDatabaseURL(o.DatabaseURL), webErrorDetail(err)), err))
	}
}

// webErrorDetail is controlledErrorDetail plus the web-schema sentinels
// (fixed messages only; the catalog-object details are appended separately by
// appendSchemaStateDetails).
func webErrorDetail(err error) string {
	switch {
	case errors.Is(err, webschema.ErrWebSchemaMissing):
		return webschema.ErrWebSchemaMissing.Error()
	case errors.Is(err, webschema.ErrWebSchemaUnsupported):
		return webschema.ErrWebSchemaUnsupported.Error()
	case errors.Is(err, webschema.ErrWebPrivileges):
		return webschema.ErrWebPrivileges.Error()
	case errors.Is(err, webauth.ErrPrepare):
		return webauth.ErrPrepare.Error()
	}
	return controlledErrorDetail(err)
}

// runMigrateWebSchemaInit implements `magnetico migrate web-schema-init`: it
// creates the magnetico_web schema next to a verified 0.8.0 schema and seeds
// the approved tracker list once. It is non-destructive and idempotent; an
// existing schema that is not exactly the supported version is refused and
// left untouched.
func runMigrateWebSchemaInit(opFlags *opflags.SchemaInitOptions) {
	report, err := webschema.Init(context.Background(), opFlags.DatabaseURL)
	if err != nil {
		log.Fatalln(appendSchemaStateDetails(fmt.Sprintf(
			"Could not initialize the web schema on %s. %s",
			redactDatabaseURL(opFlags.DatabaseURL), webErrorDetail(err)), err))
	}
	state := "already up to date"
	if report.Created {
		state = "created now"
	}
	seed := string(report.Seed)
	if report.Seed == webschema.SeedApplied {
		seed = fmt.Sprintf("seeded %d", report.SeededCount)
	}
	log.Printf("the magnetico_web schema on %s is ready (%s); trackers: %s",
		redactDatabaseURL(opFlags.DatabaseURL), state, seed)
}
