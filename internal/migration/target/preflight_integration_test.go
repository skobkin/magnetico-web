package target

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

// Suites ② (target part) and ③: identity distinctness, fresh-run state,
// resume authorization and reconciliation refusals.

func TestPreflightFreshRun(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)

	// A different-database identity: another harness database on the same
	// instance is a genuinely distinct target (the identity is checked for
	// distinctness against itself below, so any identity works).
	otherDB := pgtest.IntegrationCluster(t).TestDatabase(t)
	sourceIdentity := identityOf(t, otherDB)

	res, err := Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
	if err != nil {
		t.Fatalf("Preflight() error = %v", err)
	}
	defer res.Lock.Release(context.Background())
	if !res.Fresh {
		t.Fatal("Preflight() on an empty target must create fresh run state")
	}
	if res.Progress.Phase != state.PhaseImporting || res.Progress.LastProcessedID != 0 {
		t.Fatalf("fresh progress = %+v", res.Progress)
	}
	if res.Progress.ExpectedTotal != 2 || res.Progress.SourceMaxID != 2 {
		t.Fatalf("fresh progress source bounds = %+v", res.Progress)
	}
	if res.Progress.DeferIndexes {
		t.Fatal("defer_indexes must default to the requested false")
	}
}

func TestPreflightRefusesSameDatabase(t *testing.T) {
	t.Parallel()

	// Point the source at the target database itself: the identity check
	// (first, before any schema inspection) must refuse the same database
	// regardless of URL text.
	dsn := createTarget(t)
	sourceIdentity := identityOf(t, dsn)
	_, err := Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
	if !errors.Is(err, ErrSameDatabase) {
		t.Fatalf("Preflight() error = %v, want ErrSameDatabase", err)
	}
}

func TestPreflightRefusesNonemptyTargetWithoutRunState(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	otherDB := pgtest.IntegrationCluster(t).TestDatabase(t)
	sourceIdentity := identityOf(t, otherDB)
	seedImportedTorrent(t, dsn, 1, "1111111111111111111111111111111111111111", 1, false)

	_, err := Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
	if !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("Preflight() error = %v, want ErrTargetNotEmpty", err)
	}
}

func TestPreflightResumeAuthorized(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	otherDB := pgtest.IntegrationCluster(t).TestDatabase(t)
	sourceIdentity := identityOf(t, otherDB)

	first, err := Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
	if err != nil {
		t.Fatalf("first Preflight() error = %v", err)
	}
	runID := first.Progress.RunID
	first.Lock.Release(context.Background())

	// Simulate one committed batch: torrent 1 + blob imported, checkpoint 1.
	seedImportedTorrent(t, dsn, 1, "1111111111111111111111111111111111111111", 1, true)
	if !advanceRun(t, dsn, first.Progress, state.AdvanceInput{
		RunID: runID, ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseImporting,
		ExpectedLastID: 0, NewLastID: 1,
		ScannedDelta: 1, SuccessDelta: 1, BlobDelta: 1,
	}) {
		t.Fatal("checkpoint advance did not apply")
	}

	second, err := Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
	if err != nil {
		t.Fatalf("resume Preflight() error = %v", err)
	}
	defer second.Lock.Release(context.Background())
	if second.Fresh {
		t.Fatal("Preflight() must resume, not re-initialize")
	}
	if second.Progress.RunID != runID || second.Progress.LastProcessedID != 1 {
		t.Fatalf("resumed progress = %+v", second.Progress)
	}
}

func TestPreflightCompletedRunIsIdempotent(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	otherDB := pgtest.IntegrationCluster(t).TestDatabase(t)
	sourceIdentity := identityOf(t, otherDB)

	first, err := Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
	if err != nil {
		t.Fatalf("first Preflight() error = %v", err)
	}
	runID := first.Progress.RunID
	first.Lock.Release(context.Background())

	seedImportedTorrent(t, dsn, 1, "1111111111111111111111111111111111111111", 1, false)
	// Jump through the legal transitions to completion (0 errors).
	for _, in := range []state.AdvanceInput{
		{RunID: runID, ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseImporting, ExpectedLastID: 0, NewLastID: 1, ScannedDelta: 1, SuccessDelta: 1},
		{RunID: runID, ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseDataLoaded, ExpectedLastID: 1, NewLastID: 1},
		{RunID: runID, ExpectedPhase: state.PhaseDataLoaded, NewPhase: state.PhaseBuildingIndexes, ExpectedLastID: 1, NewLastID: 1},
		{RunID: runID, ExpectedPhase: state.PhaseBuildingIndexes, NewPhase: state.PhaseImportComplete, ExpectedLastID: 1, NewLastID: 1},
	} {
		if !advanceRun(t, dsn, first.Progress, in) {
			t.Fatalf("advance to %s did not apply", in.NewPhase)
		}
	}

	again, err := Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
	if err != nil {
		t.Fatalf("completed-run Preflight() error = %v", err)
	}
	defer again.Lock.Release(context.Background())
	if again.Progress.Phase != state.PhaseImportComplete {
		t.Fatalf("completed phase = %s", again.Progress.Phase)
	}
}

func TestPreflightResumeRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, dsn string, in *Input)
		wantErr error
	}{
		{
			name: "stale fingerprint",
			setup: func(t *testing.T, dsn string, in *Input) {
				in.SourceFingerprint.Torrents = 99 // source changed under us
			},
			wantErr: state.ErrRunStateCorrupt,
		},
		{
			name: "adapter mismatch",
			setup: func(t *testing.T, dsn string, in *Input) {
				in.SourceAdapter = "tgragnato-v1"
			},
			wantErr: state.ErrRunStateCorrupt,
		},
		{
			name: "contract version mismatch",
			setup: func(t *testing.T, dsn string, in *Input) {
				in.ContractVersion = 2
			},
			wantErr: state.ErrRunStateCorrupt,
		},
		{
			name: "defer_indexes immutable",
			setup: func(t *testing.T, dsn string, in *Input) {
				in.DeferIndexes = true
			},
			wantErr: state.ErrRunStateCorrupt,
		},
		{
			name: "torrent count mismatch",
			setup: func(t *testing.T, dsn string, in *Input) {
				// Two target rows, but the checkpoint/counters claim one.
				seedImportedTorrent(t, dsn, 1, "1111111111111111111111111111111111111111", 1, false)
				seedImportedTorrent(t, dsn, 2, "2222222222222222222222222222222222222222", 1, false)
				advanceRun(t, dsn, loadedProgress(t, dsn), state.AdvanceInput{
					RunID: storedRunID(t, dsn), ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseImporting,
					ExpectedLastID: 0, NewLastID: 1, ScannedDelta: 1, SuccessDelta: 1,
				})
			},
			wantErr: state.ErrRunStateCorrupt,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := createTarget(t)
			otherDB := pgtest.IntegrationCluster(t).TestDatabase(t)
			sourceIdentity := identityOf(t, otherDB)
			in := preflightInput(dsn, sourceIdentity)

			first, err := Preflight(context.Background(), in)
			if err != nil {
				t.Fatalf("first Preflight() error = %v", err)
			}
			first.Lock.Release(context.Background())

			tc.setup(t, dsn, &in)

			_, err = Preflight(context.Background(), in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Preflight() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// storedRunID reads the single progress row's run ID.
func storedRunID(t *testing.T, dsn string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	var runID string
	if err := conn.QueryRow(ctx,
		`SELECT run_id::text FROM magnetico_migration.progress`).Scan(&runID); err != nil {
		t.Fatalf("read run id: %v", err)
	}
	return runID
}

// pgxBeginFunc runs fn in one transaction on the target.
func pgxBeginFunc(t *testing.T, dsn string, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return pgx.BeginFunc(context.Background(), poolFor(t, dsn), fn)
}

// loadedProgress reads the stored progress row fresh.
func loadedProgress(t *testing.T, dsn string) *state.Progress {
	t.Helper()
	p, err := state.Load(context.Background(), poolFor(t, dsn))
	if err != nil {
		t.Fatalf("state.Load() error = %v", err)
	}
	return p
}

func TestPreflightRefusesPlantedInconsistencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		plant func(t *testing.T, dsn string)
	}{
		{
			name: "rows beyond checkpoint",
			plant: func(t *testing.T, dsn string) {
				seedImportedTorrent(t, dsn, 5, "5555555555555555555555555555555555555555", 1, false)
				// Checkpoint stays 0 while a row exists beyond it.
			},
		},
		{
			name: "blob count mismatch",
			plant: func(t *testing.T, dsn string) {
				seedImportedTorrent(t, dsn, 1, "1111111111111111111111111111111111111111", 1, true)
				advanceRun(t, dsn, loadedProgress(t, dsn), state.AdvanceInput{
					RunID: storedRunID(t, dsn), ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseImporting,
					ExpectedLastID: 0, NewLastID: 1, ScannedDelta: 1, SuccessDelta: 1,
				}) // blob_count stays 0 while a blob exists
			},
		},
		{
			name: "multi-file torrent without blob",
			plant: func(t *testing.T, dsn string) {
				seedImportedTorrent(t, dsn, 1, "1111111111111111111111111111111111111111", 3, false)
				advanceRun(t, dsn, loadedProgress(t, dsn), state.AdvanceInput{
					RunID: storedRunID(t, dsn), ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseImporting,
					ExpectedLastID: 0, NewLastID: 1, ScannedDelta: 1, SuccessDelta: 1, BlobDelta: 0,
				})
			},
		},
		{
			name: "error registry mismatch",
			plant: func(t *testing.T, dsn string) {
				p := loadedProgress(t, dsn)
				seedImportedTorrent(t, dsn, 1, "1111111111111111111111111111111111111111", 1, false)
				runID := storedRunID(t, dsn)
				err := pgxBeginFunc(t, dsn, func(tx pgx.Tx) error {
					return state.InsertErrorRecords(context.Background(), tx, runID, []state.ErrorRecord{{
						Scope: state.ScopeRecord, SourceID: ptrInt64(1),
						Stage: "validate", Category: state.CategoryIntegrity, Code: "invalid_metadata",
					}})
				})
				if err != nil {
					t.Fatalf("insert error record: %v", err)
				}
				advanceRun(t, dsn, p, state.AdvanceInput{
					RunID: runID, ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseImporting,
					ExpectedLastID: 0, NewLastID: 1, ScannedDelta: 1, ErrorDelta: 0, SuccessDelta: 1,
				}) // error_count stays 0 while a record error exists
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := createTarget(t)
			otherDB := pgtest.IntegrationCluster(t).TestDatabase(t)
			sourceIdentity := identityOf(t, otherDB)

			first, err := Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
			if err != nil {
				t.Fatalf("first Preflight() error = %v", err)
			}
			first.Lock.Release(context.Background())

			tc.plant(t, dsn)

			_, err = Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
			if !errors.Is(err, state.ErrRunStateCorrupt) {
				t.Fatalf("Preflight() error = %v, want ErrRunStateCorrupt", err)
			}
		})
	}
}

func TestPreflightRefusesPartialMigrationSchema(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	otherDB := pgtest.IntegrationCluster(t).TestDatabase(t)
	sourceIdentity := identityOf(t, otherDB)

	// Foreign bookkeeping: schema with only the errors table.
	execTarget(t, dsn, `CREATE SCHEMA magnetico_migration;
        CREATE TABLE magnetico_migration.errors (id int primary key);`)

	_, err := Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
	if !errors.Is(err, state.ErrRunStateCorrupt) {
		t.Fatalf("Preflight() error = %v, want ErrRunStateCorrupt", err)
	}
}

func TestPreflightLockExcludesConcurrentRuns(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	otherDB := pgtest.IntegrationCluster(t).TestDatabase(t)
	sourceIdentity := identityOf(t, otherDB)

	first, err := Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
	if err != nil {
		t.Fatalf("first Preflight() error = %v", err)
	}
	defer first.Lock.Release(context.Background())

	// A second migrator must be refused while the first holds the lock.
	_, err = Preflight(context.Background(), preflightInput(dsn, sourceIdentity))
	if !errors.Is(err, state.ErrLockBusy) {
		t.Fatalf("second Preflight() error = %v, want ErrLockBusy", err)
	}
}

func TestPreflightDeferredDropStateAuthorizedForMigratorOnly(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	otherDB := pgtest.IntegrationCluster(t).TestDatabase(t)
	sourceIdentity := identityOf(t, otherDB)

	first, err := Preflight(context.Background(), func() Input {
		in := preflightInput(dsn, sourceIdentity)
		in.DeferIndexes = true
		return in
	}())
	if err != nil {
		t.Fatalf("first Preflight() error = %v", err)
	}
	runID := first.Progress.RunID
	first.Lock.Release(context.Background())

	// Simulate the crash-after-drop state: exactly the three indexes gone,
	// the marker set — authorized only for the migrator.
	execTarget(t, dsn, `DROP INDEX magneticod.idx_torrents_total_size,
        magneticod.idx_torrents_discovered_on, magneticod.idx_torrents_name_gin_trgm`)
	execTarget(t, dsn,
		`UPDATE magnetico_migration.progress SET indexes_deferred = true WHERE run_id = $1::uuid`, runID)

	// The migrator's resume accepts it (phase importing).
	res, err := Preflight(context.Background(), func() Input {
		in := preflightInput(dsn, sourceIdentity)
		in.DeferIndexes = true
		return in
	}())
	if err != nil {
		t.Fatalf("deferred resume Preflight() error = %v", err)
	}
	res.Lock.Release(context.Background())

	// The normal application path must still refuse the same state.
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	_, err = persistence.MakeDatabase(ctx, dsn)
	if err == nil {
		t.Fatal("MakeDatabase accepted a deferred (index-less) state; strict verification is broken")
	}
}

func ptrInt64(v int64) *int64 { return &v }
