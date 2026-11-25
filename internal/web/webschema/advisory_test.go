package webschema

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestInitDoesNotWaitForTheIngestConsumerLock pins the Task 09A fix: the
// ingest consumer holds a session advisory lock for as long as it runs, and
// `migrate web-schema-init` used to take the very same key, so it blocked
// until ingest stopped. The consumer's key is written out here on purpose:
// the test must fail if anything ever makes the two commands share it again.
func TestInitDoesNotWaitForTheIngestConsumerLock(t *testing.T) {
	const consumerKey int64 = 7881644658832750187 // pgqueue's session lock, held while ingest runs
	dsn, _ := torrentDB(t)

	holder, err := pgx.Connect(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(context.Background())
	if _, err := holder.Exec(tctx(t), `SELECT pg_advisory_lock($1)`, consumerKey); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Init(ctx, dsn); err != nil {
		t.Fatalf("Init while an ingest consumer holds its advisory lock: %v", err)
	}
}
