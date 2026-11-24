package app

import (
	"log"

	"github.com/skobkin/magnetico-web/opflags"
)

func Main() {
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
