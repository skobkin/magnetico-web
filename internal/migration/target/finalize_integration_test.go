package target

// Suite ⑦ (index lifecycle, target level): the atomic drop+mark, the
// crash-boundary rebuild (zero/one/two/three indexes created), definition
// verification refusals, identity reseeding and the completion-phase gating
// on error_count AND run_error_count.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func makeLock(t *testing.T, dsn string) *state.RunLock {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	lock, err := state.AcquireRunLock(ctx, dsn)
	if err != nil {
		t.Fatalf("AcquireRunLock() error = %v", err)
	}
	t.Cleanup(func() { lock.Release(context.Background()) })
	return lock
}

func dropAllThree(t *testing.T, dsn string) {
	t.Helper()
	execTarget(t, dsn, `DROP INDEX magneticod.idx_torrents_total_size,
        magneticod.idx_torrents_discovered_on, magneticod.idx_torrents_name_gin_trgm`)
}

// indexDef reads one secondary index's definition; present excludes
// constraint-backing indexes.
func indexDef(t *testing.T, dsn, name string) (def string, present bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	err := poolFor(t, dsn).QueryRow(ctx, `SELECT pg_get_indexdef(ix.indexrelid)
        FROM pg_index ix JOIN pg_class i ON i.oid = ix.indexrelid
        WHERE i.relname = $1
          AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = i.oid)`, name).Scan(&def)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read index %s: %v", name, err)
	}
	return def, true
}

func wantIndexAbsent(t *testing.T, dsn, name string) {
	t.Helper()
	if _, present := indexDef(t, dsn, name); present {
		t.Fatalf("index magneticod.%s must be absent", name)
	}
}

func indexesDeferredFlag(t *testing.T, dsn string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	var deferred bool
	if err := poolFor(t, dsn).QueryRow(ctx,
		`SELECT indexes_deferred FROM magnetico_migration.progress`).Scan(&deferred); err != nil {
		t.Fatalf("read indexes_deferred: %v", err)
	}
	return deferred
}

// dataConstraintCount counts PK/UNIQUE/FK constraints the drop must never
// touch: torrents PK + UNIQUE(info_hash), blobs PK + FK, schema_version PK.
func dataConstraintCount(t *testing.T, dsn string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	var n int64
	if err := poolFor(t, dsn).QueryRow(ctx, `SELECT COUNT(*) FROM pg_constraint con
        JOIN pg_class c ON c.oid = con.conrelid
        JOIN pg_namespace ns ON ns.oid = con.connamespace
        WHERE ns.nspname = 'magneticod' AND con.contype IN ('p','u','f')`).Scan(&n); err != nil {
		t.Fatalf("constraint count: %v", err)
	}
	return n
}

// --- atomic drop + mark -----------------------------------------------------

func TestDropSecondaryIndexesAtomicAndMarked(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := initRunState(t, dsn)
	lock := makeLock(t, dsn)

	if err := DropSecondaryIndexes(context.Background(), lock, runID); err != nil {
		t.Fatalf("DropSecondaryIndexes() error = %v", err)
	}
	wantIndexAbsent(t, dsn, "idx_torrents_total_size")
	wantIndexAbsent(t, dsn, "idx_torrents_discovered_on")
	wantIndexAbsent(t, dsn, "idx_torrents_name_gin_trgm")
	if n := dataConstraintCount(t, dsn); n != 5 {
		t.Fatalf("integrity constraints = %d, want 5 (PK/UNIQUE/FK untouched)", n)
	}
	if !indexesDeferredFlag(t, dsn) {
		t.Fatal("the drop must set indexes_deferred")
	}

	// A second drop refuses: the authorized state must never be re-dropped.
	if err := DropSecondaryIndexes(context.Background(), lock, runID); err == nil {
		t.Fatal("second DropSecondaryIndexes() must refuse the index-less state")
	}
	if !indexesDeferredFlag(t, dsn) {
		t.Fatal("refused drop must not clear the marker")
	}
}

func TestDropSecondaryIndexesRefusesIncompleteState(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	initRunState(t, dsn)
	// One index already missing: the preflight verification refuses before
	// any DROP — the remaining two stay, the marker stays false.
	execTarget(t, dsn, `DROP INDEX magneticod.idx_torrents_discovered_on`)
	lock := makeLock(t, dsn)

	if err := DropSecondaryIndexes(context.Background(), lock, storedRunID(t, dsn)); err == nil {
		t.Fatal("DropSecondaryIndexes() must refuse a target missing one of the three indexes")
	}
	if def, present := indexDef(t, dsn, "idx_torrents_total_size"); !present || !strings.Contains(def, "USING btree (total_size)") {
		t.Fatalf("idx_torrents_total_size = (%q, %v), want present and untouched", def, present)
	}
	if def, present := indexDef(t, dsn, "idx_torrents_name_gin_trgm"); !present || !strings.Contains(def, "USING gin (name gin_trgm_ops)") {
		t.Fatalf("idx_torrents_name_gin_trgm = (%q, %v), want present and untouched", def, present)
	}
	if indexesDeferredFlag(t, dsn) {
		t.Fatal("refused drop must not set the marker")
	}
}

// --- crash-boundary rebuild ---------------------------------------------------

func TestBuildSecondaryIndexesCrashBoundaries(t *testing.T) {
	t.Parallel()

	createdSets := map[string][]string{
		"zero":       {},
		"one":        {"idx_torrents_total_size"},
		"two":        {"idx_torrents_total_size", "idx_torrents_discovered_on"},
		"three-noop": {"idx_torrents_total_size", "idx_torrents_discovered_on", "idx_torrents_name_gin_trgm"},
	}
	for name, pre := range createdSets {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dsn := createTarget(t)
			dropAllThree(t, dsn)
			for _, idx := range pre {
				execTarget(t, dsn, secondaryIndexDDL[idx])
			}
			lock := makeLock(t, dsn)
			if err := BuildSecondaryIndexes(context.Background(), lock); err != nil {
				t.Fatalf("BuildSecondaryIndexes() error = %v", err)
			}
			for idx, fragment := range secondaryIndexDefinitions {
				def, present := indexDef(t, dsn, idx)
				if !present || !strings.Contains(def, fragment) {
					t.Fatalf("index %s = (%q, %v), want present with definition %q", idx, def, present, fragment)
				}
			}
			if n := dataConstraintCount(t, dsn); n != 5 {
				t.Fatalf("integrity constraints = %d, want 5", n)
			}
		})
	}
}

func TestBuildSecondaryIndexesRefusesWrongDefinition(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	dropAllThree(t, dsn)
	// Same name, wrong definition: refuse, never silently drop-and-recreate.
	execTarget(t, dsn, `CREATE INDEX idx_torrents_total_size ON magneticod.torrents (discovered_on)`)
	lock := makeLock(t, dsn)

	err := BuildSecondaryIndexes(context.Background(), lock)
	var sse *persistence.SchemaStateError
	if !errors.As(err, &sse) {
		t.Fatalf("BuildSecondaryIndexes() error = %v (%T), want SchemaStateError", err, err)
	}
	// The wrong index is untouched; the build stopped before creating more.
	if def, present := indexDef(t, dsn, "idx_torrents_total_size"); !present || !strings.Contains(def, "USING btree (discovered_on)") {
		t.Fatalf("wrong-definition index = (%q, %v), want preserved as planted", def, present)
	}
	wantIndexAbsent(t, dsn, "idx_torrents_discovered_on")
	wantIndexAbsent(t, dsn, "idx_torrents_name_gin_trgm")
}

// --- identity reseed ----------------------------------------------------------

func TestReseedIdentity(t *testing.T) {
	t.Parallel()

	t.Run("past imported max", func(t *testing.T) {
		t.Parallel()
		dsn := createTarget(t)
		seedImportedTorrent(t, dsn, 1, "1111111111111111111111111111111111111111", 1, false)
		seedImportedTorrent(t, dsn, 3, "3333333333333333333333333333333333333333", 1, false)
		seedImportedTorrent(t, dsn, 5, "5555555555555555555555555555555555555555", 1, false)

		reseeded, err := ReseedIdentity(context.Background(), poolFor(t, dsn))
		if err != nil || !reseeded {
			t.Fatalf("ReseedIdentity() = (%v, %v), want true, nil", reseeded, err)
		}
		// The next application-generated ID is exactly max+1 (no collision).
		var nextID int32
		if err := pgx.BeginFunc(context.Background(), poolFor(t, dsn), func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(),
				`INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
             VALUES (decode('9999999999999999999999999999999999999999','hex'), 'app', 1, 1, 1)
             RETURNING id`).Scan(&nextID)
		}); err != nil {
			t.Fatalf("insert without id: %v", err)
		}
		if nextID != 6 {
			t.Fatalf("generated id = %d, want 6", nextID)
		}
	})

	t.Run("empty import leaves initial state", func(t *testing.T) {
		t.Parallel()
		dsn := createTarget(t)
		reseeded, err := ReseedIdentity(context.Background(), poolFor(t, dsn))
		if err != nil || reseeded {
			t.Fatalf("ReseedIdentity() on empty target = (%v, %v), want false, nil", reseeded, err)
		}
		var nextID int32
		if err := pgx.BeginFunc(context.Background(), poolFor(t, dsn), func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(),
				`INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
             VALUES (decode('1111111111111111111111111111111111111111','hex'), 'first', 1, 1, 1)
             RETURNING id`).Scan(&nextID)
		}); err != nil {
			t.Fatalf("insert without id: %v", err)
		}
		if nextID != 1 {
			t.Fatalf("generated id = %d, want 1 (sequence untouched)", nextID)
		}
	})

	t.Run("max id refused", func(t *testing.T) {
		t.Parallel()
		dsn := createTarget(t)
		seedImportedTorrent(t, dsn, 2147483647, "7fffffffffffffffffffffffffffffffffffffff", 1, false)
		_, err := ReseedIdentity(context.Background(), poolFor(t, dsn))
		if !errors.Is(err, ErrIdentityExhausted) {
			t.Fatalf("ReseedIdentity() error = %v, want ErrIdentityExhausted", err)
		}
	})
}

// --- completion gating --------------------------------------------------------

// runAtBuildingIndexes walks a fresh run state to building_indexes through
// the real CAS transitions (checkpoint stays at the initial 0).
func runAtBuildingIndexes(t *testing.T, dsn string) string {
	t.Helper()
	runID := initRunState(t, dsn)
	p := loadedProgress(t, dsn)
	if !advanceRun(t, dsn, p, state.AdvanceInput{
		RunID: runID, ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseDataLoaded,
		ExpectedLastID: 0, NewLastID: 0,
	}) {
		t.Fatal("data_loaded transition did not apply")
	}
	if !advanceRun(t, dsn, p, state.AdvanceInput{
		RunID: runID, ExpectedPhase: state.PhaseDataLoaded, NewPhase: state.PhaseBuildingIndexes,
		ExpectedLastID: 0, NewLastID: 0,
	}) {
		t.Fatal("building_indexes transition did not apply")
	}
	return runID
}

func storedPhase(t *testing.T, dsn string) state.Phase {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	var phase string
	if err := poolFor(t, dsn).QueryRow(ctx,
		`SELECT phase FROM magnetico_migration.progress`).Scan(&phase); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	return state.Phase(phase)
}

func TestCompleteImportGating(t *testing.T) {
	t.Parallel()

	t.Run("clean run completes plainly", func(t *testing.T) {
		t.Parallel()
		dsn := createTarget(t)
		runID := runAtBuildingIndexes(t, dsn)
		phase, err := CompleteImport(context.Background(), poolFor(t, dsn), runID, 0)
		if err != nil {
			t.Fatalf("CompleteImport() error = %v", err)
		}
		if phase != state.PhaseImportComplete || storedPhase(t, dsn) != state.PhaseImportComplete {
			t.Fatalf("phase = %s/%s, want import_complete", phase, storedPhase(t, dsn))
		}
	})

	t.Run("record errors force with_errors", func(t *testing.T) {
		t.Parallel()
		dsn := createTarget(t)
		runID := runAtBuildingIndexes(t, dsn)
		if err := pgx.BeginFunc(context.Background(), poolFor(t, dsn), func(tx pgx.Tx) error {
			return state.InsertErrorRecords(context.Background(), tx, runID, []state.ErrorRecord{{
				Scope: state.ScopeRecord, SourceID: ptrInt64(1),
				Stage: "validate", Category: state.CategoryIntegrity, Code: "invalid_metadata",
			}})
		}); err != nil {
			t.Fatalf("insert record error: %v", err)
		}
		advanceRun(t, dsn, loadedProgress(t, dsn), state.AdvanceInput{
			RunID: runID, ExpectedPhase: state.PhaseBuildingIndexes, NewPhase: state.PhaseBuildingIndexes,
			ExpectedLastID: 0, NewLastID: 0, ScannedDelta: 1, ErrorDelta: 1,
		})
		phase, err := CompleteImport(context.Background(), poolFor(t, dsn), runID, 0)
		if err != nil {
			t.Fatalf("CompleteImport() error = %v", err)
		}
		if phase != state.PhaseCompleteWithErrors {
			t.Fatalf("phase = %s, want import_complete_with_errors", phase)
		}
	})

	t.Run("unresolved integrity incident forces with_errors", func(t *testing.T) {
		t.Parallel()
		dsn := createTarget(t)
		runID := runAtBuildingIndexes(t, dsn)
		inserted, err := state.EnsureIncident(context.Background(), poolFor(t, dsn), runID, state.ErrorRecord{
			Scope: state.ScopeRun, Stage: "preflight", Category: state.CategoryIntegrity,
			Code: "source_orphan_files", Details: map[string]any{"orphan_files": 1},
		})
		if err != nil || !inserted {
			t.Fatalf("EnsureIncident() = (%v, %v), want true, nil", inserted, err)
		}
		phase, err := CompleteImport(context.Background(), poolFor(t, dsn), runID, 0)
		if err != nil {
			t.Fatalf("CompleteImport() error = %v", err)
		}
		if phase != state.PhaseCompleteWithErrors {
			t.Fatalf("phase = %s, want import_complete_with_errors (run_error_count gates)", phase)
		}
	})

	t.Run("operational incident is history only", func(t *testing.T) {
		t.Parallel()
		dsn := createTarget(t)
		runID := runAtBuildingIndexes(t, dsn)
		inserted, err := state.EnsureIncident(context.Background(), poolFor(t, dsn), runID, state.ErrorRecord{
			Scope: state.ScopeRun, Stage: "write", Category: state.CategoryOperational,
			Code: "fatal_incident", Details: map[string]any{"stage": "write"},
		})
		if err != nil || !inserted {
			t.Fatalf("EnsureIncident() = (%v, %v), want true, nil", inserted, err)
		}
		phase, err := CompleteImport(context.Background(), poolFor(t, dsn), runID, 0)
		if err != nil {
			t.Fatalf("CompleteImport() error = %v", err)
		}
		if phase != state.PhaseImportComplete {
			t.Fatalf("phase = %s, want import_complete (operational incidents never gate)", phase)
		}
	})
}

// --- verification window (resume before/after the drop) ------------------------

func TestVerifySchemaDeferredResumeWindow(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	ctx := context.Background()

	// Pending drop (indexes_deferred=false): strict — indexes must be present.
	if err := VerifySchema(ctx, dsn, verifyInput{
		deferRequested: true, indexesDeferred: false, phase: state.PhaseImporting,
	}); err != nil {
		t.Fatalf("pending-drop verification on a strict target error = %v", err)
	}

	dropAllThree(t, dsn)

	// The same pending-drop state against the index-less target refuses
	// (and so does a plain no-deferral verification).
	if err := VerifySchema(ctx, dsn, verifyInput{
		deferRequested: true, indexesDeferred: false, phase: state.PhaseImporting,
	}); err == nil {
		t.Fatal("pending-drop verification must refuse an index-less target")
	}
	if err := VerifySchema(ctx, dsn, verifyInput{phase: state.PhaseImporting}); err == nil {
		t.Fatal("no-deferral verification must refuse an index-less target")
	}

	// Authorized index-less importing: passes.
	if err := VerifySchema(ctx, dsn, verifyInput{
		deferRequested: true, indexesDeferred: true, phase: state.PhaseImporting,
	}); err != nil {
		t.Fatalf("authorized index-less importing error = %v", err)
	}

	// An index recreated before building_indexess is corrupt (the drop is
	// atomic with its marker).
	execTarget(t, dsn, `CREATE INDEX idx_torrents_total_size ON magneticod.torrents (total_size)`)
	if err := VerifySchema(ctx, dsn, verifyInput{
		deferRequested: true, indexesDeferred: true, phase: state.PhaseImporting,
	}); err == nil {
		t.Fatal("a recreated index before building must refuse")
	}

	// Partial progress is valid only while building indexes.
	if err := VerifySchema(ctx, dsn, verifyInput{
		deferRequested: true, indexesDeferred: true, phase: state.PhaseBuildingIndexes,
	}); err != nil {
		t.Fatalf("partial building_indexes verification error = %v", err)
	}

	// A completed run is strict again (deferred indexes are rebuilt).
	if err := VerifySchema(ctx, dsn, verifyInput{
		deferRequested: true, indexesDeferred: true, phase: state.PhaseImportComplete,
	}); err == nil {
		t.Fatal("completed-run verification must refuse an incomplete index set")
	}
}
