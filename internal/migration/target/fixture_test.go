package target

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

// targetFixture: a fresh, schema-initialized target database and its DSN.
func createTarget(t *testing.T) string {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	if err := persistence.InitSchema(ctx, dsn); err != nil {
		t.Fatalf("InitSchema() error = %v", err)
	}
	return dsn
}

// identityOf reads the server identity of any database the harness created.
func identityOf(t *testing.T, dsn string) pgidentity.Identity {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("identity connect: %v", err)
	}
	defer conn.Close(context.Background())
	identity, err := pgidentity.Read(ctx, conn)
	if err != nil {
		t.Fatalf("identity read: %v", err)
	}
	return identity
}

// fakeSourceFingerprint fabricates a plausible source fingerprint for
// preflight tests (the target path needs no real source database).
func fakeSourceFingerprint(identity pgidentity.Identity) original.Fingerprint {
	return original.Fingerprint{
		PostmasterStart:   identity.PostmasterStart,
		Database:          "legacy_source",
		Torrents:          2,
		MinID:             1,
		MaxID:             2,
		AnchorMinInfoHash: "aaaa",
		AnchorMaxInfoHash: "bbbb",
		FileRows:          3,
		NullOwnerFiles:    0,
		OrphanFiles:       0,
	}
}

// preflightInput assembles a default Input for one target DSN.
func preflightInput(dsn string, identity pgidentity.Identity) Input {
	return Input{
		DSN:               dsn,
		SourceIdentity:    identity,
		SourceFingerprint: fakeSourceFingerprint(identity),
		DeferIndexes:      false,
		SourceAdapter:     "original-v0.12.0",
		ContractVersion:   1,
		CodecProfile:      "compact",
		Options:           state.Options{Workers: 4, BatchSize: 256, MaxInflightBytes: 64 << 20},
	}
}

// execTarget runs one statement on a target (fixture manipulation only).
func execTarget(t *testing.T, dsn, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("target connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("target exec %q: %v", sql, err)
	}
}

// seedImportedTorrent plants one imported torrent (explicit ID) in the
// target, as the writer would.
func seedImportedTorrent(t *testing.T, dsn string, id int32, infoHashHex string, fileCount int32, withBlob bool) {
	t.Helper()
	execTarget(t, dsn, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on, file_count)
        VALUES ($1, decode($2,'hex'), 'seeded', 10, 1700000000, $3)`, id, infoHashHex, fileCount)
	if withBlob {
		execTarget(t, dsn, `INSERT INTO magneticod.torrent_file_blobs (torrent_id, data)
            VALUES ($1, decode('78da','hex'))`, id)
	}
}

// advanceRun applies one checkpoint advance through the real CAS primitive,
// in its own transaction — what a committed batch would have done.
func advanceRun(t *testing.T, dsn string, p *state.Progress, in state.AdvanceInput) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	var advanced bool
	err := pgx.BeginFunc(ctx, poolFor(t, dsn), func(tx pgx.Tx) error {
		var err error
		advanced, err = state.AdvanceCheckpoint(ctx, tx, in)
		return err
	})
	if err != nil {
		t.Fatalf("AdvanceCheckpoint() error = %v", err)
	}
	// Keep the caller's copy roughly current (phase/checkpoint).
	p.LastProcessedID = in.NewLastID
	p.Phase = in.NewPhase
	p.ScannedCount += in.ScannedDelta
	p.SuccessCount += in.SuccessDelta
	p.ErrorCount += in.ErrorDelta
	p.BlobCount += in.BlobDelta
	return advanced
}

func poolFor(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool parse: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
