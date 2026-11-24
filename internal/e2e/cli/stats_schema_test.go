package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// `migrate stats-schema-init` through the real binary: creates, repeats, refuses
// a structurally different schema and leaves it untouched, never echoing the
// database password.
func TestMigrateStatsSchemaInitBinary(t *testing.T) {
	bin := buildBinary(t)
	secret := "stats-secret-pw-for-test"
	admin := webTarget(t)
	dsn := withPassword(t, admin, secret)

	out, code := runWebSchemaInit(t, bin, dsn, "stats-schema-init")
	if code != 0 || !strings.Contains(out, "magnetico_stats schema") || strings.Contains(out, secret) {
		t.Fatalf("first run: exit %d: %s", code, out)
	}
	if out, code = runWebSchemaInit(t, bin, dsn, "stats-schema-init"); code != 0 || strings.Contains(out, secret) {
		t.Fatalf("repeat: exit %d: %s", code, out)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `ALTER TABLE magnetico_stats.snapshot ADD COLUMN note text`); err != nil {
		t.Fatal(err)
	}
	out, code = runWebSchemaInit(t, bin, dsn, "stats-schema-init")
	if code != 1 || !strings.Contains(out, "structurally incompatible") || !strings.Contains(out, "snapshot has unexpected column note") ||
		strings.Contains(out, secret) {
		t.Fatalf("incompatible schema: exit %d: %s", code, out)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='magnetico_stats' AND column_name='note'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the refused init altered the schema (%d, %v)", n, err)
	}
	// Without a verified 0.8.0 schema nothing is created.
	bare := withPassword(t, pgtest.IntegrationCluster(t).TestDatabase(t), secret)
	if out, code = runWebSchemaInit(t, bin, bare, "stats-schema-init"); code == 0 {
		t.Fatalf("init without the 0.8.0 schema succeeded: %s", out)
	}
}
