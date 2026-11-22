package target

import (
	"context"
	"testing"

	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// The magnetico_ingest queue schema must not change migrate preflight
// (docs/specs/crawler-ingest-delivery-v1.md §6.3).
func TestPreflightUnaffectedByIngestSchema(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	if err := pgqueue.InitSchema(context.Background(), dsn); err != nil {
		t.Fatalf("ingest-schema-init: %v", err)
	}
	sourceIdentity := identityOf(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	res, err := Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
	if err != nil {
		t.Fatalf("Preflight() with magnetico_ingest present: %v", err)
	}
	defer res.Lock.Release(context.Background())
	if !res.Fresh {
		t.Fatal("Preflight() must still create fresh run state")
	}
}
