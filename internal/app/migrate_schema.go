package app

import (
	"context"
	"log"

	"github.com/skobkin/magnetico-web/opflags"
	"github.com/skobkin/magnetico-web/persistence"
)

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
