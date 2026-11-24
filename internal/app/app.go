package app

import (
	"fmt"
	"log"
	"os"

	"github.com/skobkin/magnetico-web/internal/buildinfo"
	"github.com/skobkin/magnetico-web/internal/legacywarn"
	"github.com/skobkin/magnetico-web/opflags"
)

// Main is the entry point of the magnetico executable: it parses the command
// line and runs one of the commands. Commands report failures through their
// documented exit codes (see the migrate and service commands) and never return
// an error to the caller.
func Main() {
	// `magnetico --version` is answered before anything else is read: no
	// configuration file, no environment validation, no database, no network.
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(buildinfo.Info())
		os.Exit(0)
	}

	// TEMPORARY (planned removal around 0.10.0): explain the cause before any
	// configuration failure when the PHP deployment's environment is present.
	// Help and the no-command usage stay quiet.
	if !helpOrNoCommand(os.Args[1:]) {
		legacywarn.Warn(os.Stderr, os.Environ())
	}

	// opFlags is the "operational flags"
	opFlags := opflags.OpFlags{}
	command, err := opFlags.Parse()
	if err != nil {
		log.Fatalln(err.Error())
	}

	switch command {
	case "crawl":
		runCrawl(&opFlags.Crawl)
	case "ingest":
		runIngest(&opFlags.Ingest)
	case "web":
		if opFlags.Web.Subcommand != "" {
			runWebAdmin(&opFlags.Web)
		} else {
			runWeb(&opFlags.Web)
		}
	case "dev":
		runDevSeedTorrents(&opFlags.Dev.SeedTorrents)
	case "migrate":
		switch opFlags.Migrate.Subcommand {
		case "schema-init":
			runMigrateSchemaInit(&opFlags.Migrate.SchemaInit)
		case "ingest-schema-init":
			runMigrateIngestSchemaInit(&opFlags.Migrate.IngestSchemaInit)
		case "web-schema-init":
			runMigrateWebSchemaInit(&opFlags.Migrate.WebSchemaInit)
		case "torrents":
			runMigrateTorrents(&opFlags.Migrate.Torrents)
		case "retry-errors":
			runMigrateRetryErrors(&opFlags.Migrate.Torrents.RetryErrors)
		case "users":
			runMigrateUsers(&opFlags.Migrate.Users)
		case "users-verify":
			runMigrateUsersVerify(&opFlags.Migrate.Users.Verify)
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
			log.Fatalln("migrate: expected a subcommand; expected: schema-init, ingest-schema-init, web-schema-init, torrents, users, verify-decode, verify-content, errors, cleanup (see --help)")
		}
	}
}

// helpOrNoCommand reports whether args ask for help or name no command at all.
func helpOrNoCommand(args []string) bool {
	if len(args) == 0 {
		return true
	}
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}
