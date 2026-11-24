package app

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/skobkin/magnetico-web/internal/opstats"
	"github.com/skobkin/magnetico-web/opflags"
)

// runMigrateStatsSchemaInit implements `magnetico migrate stats-schema-init`:
// it creates the optional magnetico_stats schema next to a verified 0.8.0
// schema (non-destructive, idempotent, advisory-locked). An existing schema
// that does not verify is refused and left untouched.
func runMigrateStatsSchemaInit(o *opflags.SchemaInitOptions) {
	if err := opstats.InitSchema(context.Background(), o.DatabaseURL); err != nil {
		detail := controlledErrorDetail(err)
		if errors.Is(err, opstats.ErrSchemaMissing) || errors.Is(err, opstats.ErrSchemaInvalid) {
			detail = err.Error()
		}
		log.Fatalln(appendSchemaStateDetails(fmt.Sprintf("Could not initialize the statistics schema on %s. %s",
			redactDatabaseURL(o.DatabaseURL), detail), err))
	}
	log.Printf("the magnetico_stats schema on %s is ready (created now or already up to date)", redactDatabaseURL(o.DatabaseURL))
}
