package target

// Retry transaction integration tests: per-ID atomic repair invariants,
// conflicts, the cursor CAS, the lost-COMMIT marker, the revision-tracked
// resume matrix and the phase-flip gating.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/migration/state"
)

const retryFixtureRunID = "44444444-4444-4444-8444-444444444444"

// Fixture infohash hex constants for the error registry rows.
const (
	retryHash3 = "3333333333333333333333333333333333333333"
	retryHash4 = "4444444444444444444444444444444444444444"
)

func hexBytes(hex string) []byte {
	b := make([]byte, 20)
	for i := 0; i < 20; i++ {
		hi, lo := hexNibble(hex[2*i]), hexNibble(hex[2*i+1])
		b[i] = byte(hi<<4 | lo)
	}
	return b
}

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	default:
		return int(c-'a') + 10
	}
}

func i64p(v int64) *int64 { return &v }

// completedRetryFixture drives a run state to import_complete_with_errors
// with two imported torrents (IDs 1, 2, each with a blob) and two
// unresolved record errors (source IDs 3 and 4), counters consistent —
// exactly what a finished 04A import with errors looks like. The fixture
// satisfies every Reconcile invariant by construction.
func completedRetryFixture(t *testing.T, dsn string) string {
	t.Helper()
	pool := poolFor(t, dsn)
	ctx := context.Background()

	if _, err := state.InitRun(ctx, pool, state.InitInput{
		RunID:          retryFixtureRunID,
		FingerprintRaw: []byte(`{}`),
		SourceAdapter:  "original-v0.12.0", ContractVersion: 1, CodecProfile: "compact",
		ExpectedTotal: 4, SourceMinID: 1, SourceMaxID: 10, SourceFileRows: 5,
		DeferIndexes: false,
		Options:      state.Options{Workers: 1, BatchSize: 10, MaxInflightBytes: 1 << 20},
	}); err != nil {
		t.Fatalf("InitRun() error = %v", err)
	}

	// importing -> data_loaded -> building_indexes (checkpoint stays 0).
	for _, step := range []struct{ from, to state.Phase }{
		{state.PhaseImporting, state.PhaseDataLoaded},
		{state.PhaseDataLoaded, state.PhaseBuildingIndexes},
	} {
		if err := TransitionPhase(ctx, pool, retryFixtureRunID, state.AdvanceInput{
			RunID: retryFixtureRunID, ExpectedLastID: 0, ExpectedPhase: step.from,
			NewLastID: 0, NewPhase: step.to,
		}); err != nil {
			t.Fatalf("TransitionPhase(%s) error = %v", step.to, err)
		}
	}

	seedImportedTorrent(t, dsn, 1, "1111111111111111111111111111111111111111", 1, true)
	seedImportedTorrent(t, dsn, 2, "2222222222222222222222222222222222222222", 1, true)
	if err := pgxBeginFunc(t, dsn, func(tx pgx.Tx) error {
		return state.InsertErrorRecords(ctx, tx, retryFixtureRunID, []state.ErrorRecord{
			{Scope: state.ScopeRecord, SourceID: i64p(3), SourceInfoHash: hexBytes(retryHash3),
				Stage: "validate", Category: state.CategoryIntegrity, Code: "invalid_metadata"},
			{Scope: state.ScopeRecord, SourceID: i64p(4), SourceInfoHash: hexBytes(retryHash4),
				Stage: "validate", Category: state.CategoryIntegrity, Code: "invalid_framing"},
		})
	}); err != nil {
		t.Fatalf("insert record errors: %v", err)
	}

	// Final advance: counters reflect the 04A outcome (4 scanned, 2 ok,
	// 2 errors), checkpoint at 10 (max source ID), phase with_errors.
	if err := pgxBeginFunc(t, dsn, func(tx pgx.Tx) error {
		advanced, err := state.AdvanceCheckpoint(ctx, tx, state.AdvanceInput{
			RunID: retryFixtureRunID, ExpectedLastID: 0, ExpectedPhase: state.PhaseBuildingIndexes,
			NewLastID: 10, NewPhase: state.PhaseCompleteWithErrors,
			ScannedDelta: 4, SuccessDelta: 2, ErrorDelta: 2, BlobDelta: 2,
		})
		if err != nil {
			return err
		}
		if !advanced {
			return ErrCheckpointConflict
		}
		return nil
	}); err != nil {
		t.Fatalf("final advance error = %v", err)
	}

	p := loadedProgress(t, dsn)
	facts, err := TargetFacts(ctx, pool, p.LastProcessedID, p.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Reconcile(p, facts); err != nil {
		t.Fatalf("fixture does not reconcile: %v", err)
	}
	return retryFixtureRunID
}

// repairedRecord builds a plausible repaired record for a fixture source ID.
func repairedRecord(id int64, hashHex string, withBlob bool) RetryRecord {
	rec := RetryRecord{
		SourceID: id, InfoHash: hexBytes(hashHex),
		Name: "repaired", TotalSize: 99, DiscoveredOn: 1700000500, FileCount: 1,
	}
	if withBlob {
		rec.Blob = []byte{0x78, 0xda, 0x00, 0x01}
	}
	return rec
}

// startRetryPass opens a fresh retry pass over the fixture's two unresolved
// records (no adoption — resumable nil).
func startRetryPass(t *testing.T, dsn, runID string) *state.RetryRun {
	t.Helper()
	run, _, err := state.StartRetryRun(context.Background(), poolFor(t, dsn), state.CreateRetryRunInput{
		RunID:                runID,
		StartDataRevision:    loadedProgress(t, dsn).DataRevision,
		UnresolvedAtStart:    2,
		SourceFingerprintRaw: []byte(`{"database":"legacy_source","torrents":4}`),
	}, nil)
	if err != nil {
		t.Fatalf("StartRetryRun() error = %v", err)
	}
	return run
}

// retryProgress reads the progress counters + revision directly.
func retryProgress(t *testing.T, dsn string) (success, errCount, blobs, scanned, lastID, revision int64) {
	t.Helper()
	err := poolFor(t, dsn).QueryRow(context.Background(), `SELECT
        success_count, error_count, blob_count, scanned_count, last_processed_id, data_revision
        FROM magnetico_migration.progress WHERE run_id = $1::uuid`, retryFixtureRunID).
		Scan(&success, &errCount, &blobs, &scanned, &lastID, &revision)
	if err != nil {
		t.Fatalf("read progress: %v", err)
	}
	return
}

func TestCommitRetrySuccessInvariants(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := completedRetryFixture(t, dsn)
	pool := poolFor(t, dsn)
	ctx := context.Background()
	pass := startRetryPass(t, dsn, runID)

	err := CommitRetrySuccess(ctx, pool, RetryAttemptCommit{
		RetryID: pass.RetryID, RunID: runID,
		Record: repairedRecord(3, retryHash3, true),
	})
	if err != nil {
		t.Fatalf("CommitRetrySuccess() error = %v", err)
	}

	// Target row with the exact historical ID plus its blob.
	var name string
	var blob []byte
	if err := pool.QueryRow(ctx, `SELECT t.name, b.data FROM magneticod.torrents t
        LEFT JOIN magneticod.torrent_file_blobs b ON b.torrent_id = t.id
        WHERE t.id = 3`).Scan(&name, &blob); err != nil {
		t.Fatalf("repaired row read: %v", err)
	}
	if name != "repaired" || len(blob) == 0 {
		t.Fatalf("repaired row = %q blob %d bytes", name, len(blob))
	}

	// Registry: resolved with attempts+1 (the import error was attempt 1)
	// and a timestamp; the row stays as history.
	var attempts int
	var resolved bool
	var resolvedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT attempts, resolved, resolved_at
        FROM magnetico_migration.errors WHERE run_id = $1::uuid AND source_id = 3`,
		runID).Scan(&attempts, &resolved, &resolvedAt); err != nil {
		t.Fatal(err)
	}
	if !resolved || attempts != 2 || resolvedAt == nil {
		t.Fatalf("error row = attempts %d resolved %v at %v", attempts, resolved, resolvedAt)
	}

	// Progress counters: success+1, error-1, blob+1, revision+1 — scanned
	// and the import checkpoint untouched.
	success, errCount, blobs, scanned, lastID, revision := retryProgress(t, dsn)
	if success != 3 || errCount != 1 || blobs != 3 || scanned != 4 || lastID != 10 || revision != 1 {
		t.Fatalf("progress = success %d error %d blob %d scanned %d last %d rev %d, want 3/1/3/4/10/1",
			success, errCount, blobs, scanned, lastID, revision)
	}

	// Retry pass bookkeeping: counters, same-transaction revision bump,
	// cursor at the repaired source ID.
	got, err := state.GetRetryRun(ctx, pool, pass.RetryID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptedCount != 1 || got.FixedCount != 1 || got.CurrentDataRevision != 1 ||
		got.LastSourceID == nil || *got.LastSourceID != 3 {
		t.Fatalf("retry run = %+v", got)
	}

	// Attempt history appended.
	att, err := state.ListRetryAttempts(ctx, pool, pass.RetryID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(att) != 1 || att[0].Outcome != state.RetryOutcomeFixed || att[0].SourceID != 3 {
		t.Fatalf("attempts = %+v", att)
	}

	// Every Reconcile invariant still holds.
	p := loadedProgress(t, dsn)
	facts, err := TargetFacts(ctx, pool, p.LastProcessedID, p.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Reconcile(p, facts); err != nil {
		t.Fatalf("post-retry reconcile = %v", err)
	}
}

func TestCommitRetryInvalidKeepsUnresolved(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := completedRetryFixture(t, dsn)
	pool := poolFor(t, dsn)
	ctx := context.Background()
	pass := startRetryPass(t, dsn, runID)

	err := CommitRetryInvalid(ctx, pool, RetryAttemptCommit{
		RetryID: pass.RetryID, RunID: runID,
		Record: repairedRecord(3, retryHash3, false),
		Stage:  "validate", Code: "still_invalid_metadata",
		Details: map[string]any{"reason": "name empty"},
	})
	if err != nil {
		t.Fatalf("CommitRetryInvalid() error = %v", err)
	}

	var attempts int
	var resolved bool
	if err := pool.QueryRow(ctx, `SELECT attempts, resolved FROM magnetico_migration.errors
        WHERE run_id = $1::uuid AND source_id = 3`, runID).Scan(&attempts, &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved || attempts != 2 {
		t.Fatalf("error row = attempts %d resolved %v, want 2/false (never silently resolved)",
			attempts, resolved)
	}
	// Registry changed (revision+1) but the import counters did not.
	success, errCount, blobs, scanned, lastID, revision := retryProgress(t, dsn)
	if success != 2 || errCount != 2 || blobs != 2 || scanned != 4 || lastID != 10 || revision != 1 {
		t.Fatalf("progress = %d/%d/%d scanned %d last %d rev %d, want counters frozen, rev 1",
			success, errCount, blobs, scanned, lastID, revision)
	}
	got, err := state.GetRetryRun(ctx, pool, pass.RetryID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StillInvalidCount != 1 || got.FixedCount != 0 || got.CurrentDataRevision != 1 {
		t.Fatalf("retry run counters = %+v", got)
	}
	att, err := state.ListRetryAttempts(ctx, pool, pass.RetryID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(att) != 1 || att[0].Outcome != state.RetryOutcomeStillInvalid || att[0].Code != "still_invalid_metadata" {
		t.Fatalf("attempts = %+v", att)
	}
}

func TestCommitRetryMissingSource(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := completedRetryFixture(t, dsn)
	pool := poolFor(t, dsn)
	ctx := context.Background()
	pass := startRetryPass(t, dsn, runID)

	err := CommitRetryMissing(ctx, pool, RetryAttemptCommit{
		RetryID: pass.RetryID, RunID: runID,
		Record: RetryRecord{SourceID: 3, InfoHash: hexBytes(retryHash3)},
		Stage:  "read", Code: "source_row_missing",
	})
	if err != nil {
		t.Fatalf("CommitRetryMissing() error = %v", err)
	}
	var attempts int
	var resolved bool
	if err := pool.QueryRow(ctx, `SELECT attempts, resolved FROM magnetico_migration.errors
        WHERE run_id = $1::uuid AND source_id = 3`, runID).Scan(&attempts, &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved || attempts != 2 {
		t.Fatalf("missing source resolved the error: attempts %d resolved %v", attempts, resolved)
	}
	if _, _, _, _, _, revision := retryProgress(t, dsn); revision != 1 {
		t.Fatalf("data_revision = %d after a missing-source attempt, want 1", revision)
	}
	got, err := state.GetRetryRun(ctx, pool, pass.RetryID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MissingCount != 1 || got.CurrentDataRevision != 1 {
		t.Fatalf("retry run = %+v", got)
	}
	att, err := state.ListRetryAttempts(ctx, pool, pass.RetryID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(att) != 1 || att[0].Outcome != state.RetryOutcomeMissing {
		t.Fatalf("attempts = %+v", att)
	}
}

func TestCommitRetrySuccessConflicts(t *testing.T) {
	t.Parallel()

	t.Run("already resolved", func(t *testing.T) {
		t.Parallel()
		dsn := createTarget(t)
		runID := completedRetryFixture(t, dsn)
		pool := poolFor(t, dsn)
		pass := startRetryPass(t, dsn, runID)
		execTarget(t, dsn, `UPDATE magnetico_migration.errors SET resolved = true, resolved_at = now()
            WHERE run_id = $1::uuid AND source_id = 3`, runID)
		err := CommitRetrySuccess(context.Background(), pool, RetryAttemptCommit{
			RetryID: pass.RetryID, RunID: runID, Record: repairedRecord(3, retryHash3, true),
		})
		if !errors.Is(err, ErrRetryConflict) {
			t.Fatalf("error = %v, want ErrRetryConflict", err)
		}
	})

	t.Run("target ID exists", func(t *testing.T) {
		t.Parallel()
		dsn := createTarget(t)
		runID := completedRetryFixture(t, dsn)
		pool := poolFor(t, dsn)
		pass := startRetryPass(t, dsn, runID)
		seedImportedTorrent(t, dsn, 3, "9999999999999999999999999999999999999999", 1, false)
		err := CommitRetrySuccess(context.Background(), pool, RetryAttemptCommit{
			RetryID: pass.RetryID, RunID: runID, Record: repairedRecord(3, retryHash3, true),
		})
		if !errors.Is(err, ErrRetryConflict) {
			t.Fatalf("error = %v, want ErrRetryConflict", err)
		}
	})

	t.Run("info hash collision", func(t *testing.T) {
		t.Parallel()
		dsn := createTarget(t)
		runID := completedRetryFixture(t, dsn)
		pool := poolFor(t, dsn)
		pass := startRetryPass(t, dsn, runID)
		seedImportedTorrent(t, dsn, 5, retryHash3, 1, false)
		err := CommitRetrySuccess(context.Background(), pool, RetryAttemptCommit{
			RetryID: pass.RetryID, RunID: runID, Record: repairedRecord(3, retryHash3, true),
		})
		if !errors.Is(err, ErrRetryConflict) {
			t.Fatalf("error = %v, want ErrRetryConflict", err)
		}
	})

	t.Run("error row missing", func(t *testing.T) {
		t.Parallel()
		dsn := createTarget(t)
		runID := completedRetryFixture(t, dsn)
		pool := poolFor(t, dsn)
		pass := startRetryPass(t, dsn, runID)
		execTarget(t, dsn, `DELETE FROM magnetico_migration.errors
            WHERE run_id = $1::uuid AND source_id = 3`, runID)
		err := CommitRetrySuccess(context.Background(), pool, RetryAttemptCommit{
			RetryID: pass.RetryID, RunID: runID, Record: repairedRecord(3, retryHash3, true),
		})
		if !errors.Is(err, state.ErrRunStateCorrupt) {
			t.Fatalf("error = %v, want ErrRunStateCorrupt", err)
		}
		// Nothing landed.
		var n int64
		if err := pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM magneticod.torrents WHERE id = 3`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("conflicted attempt still inserted the torrent")
		}
	})
}

func TestRetryCursorCASRefusesStale(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := completedRetryFixture(t, dsn)
	pool := poolFor(t, dsn)
	pass := startRetryPass(t, dsn, runID)

	stale := int64(7) // the pass cursor is still NULL
	err := CommitRetrySuccess(context.Background(), pool, RetryAttemptCommit{
		RetryID: pass.RetryID, RunID: runID,
		Record:               repairedRecord(3, retryHash3, true),
		ExpectedLastSourceID: &stale,
	})
	if err == nil {
		t.Fatal("stale cursor CAS must fail the attempt")
	}
	// The whole attempt rolled back: revision, counters, data.
	if _, _, _, _, _, revision := retryProgress(t, dsn); revision != 0 {
		t.Fatalf("revision = %d after a refused attempt, want 0 (rolled back)", revision)
	}
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM magneticod.torrents WHERE id = 3`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d torrent rows landed with the refused attempt", n)
	}
}

func TestReconcileRetryAttemptMarkers(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := completedRetryFixture(t, dsn)
	pool := poolFor(t, dsn)
	ctx := context.Background()
	pass := startRetryPass(t, dsn, runID)

	// Ambiguous COMMIT that actually landed: the attempt row proves it.
	if err := CommitRetrySuccess(ctx, pool, RetryAttemptCommit{
		RetryID: pass.RetryID, RunID: runID, Record: repairedRecord(3, retryHash3, true),
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := ReconcileRetryAttempt(ctx, pool, RetryReconcileInput{
		RetryID: pass.RetryID, RunID: runID, SourceID: 3, ExpectedDataRevision: 0,
	})
	if err != nil {
		t.Fatalf("ReconcileRetryAttempt(committed) error = %v", err)
	}
	if !rec.Committed {
		t.Fatal("attempt row present must reconcile as committed")
	}

	// Ambiguous COMMIT that did not land: replayable against an unchanged
	// revision (1 after the source-3 repair) and clean preconditions.
	rec, err = ReconcileRetryAttempt(ctx, pool, RetryReconcileInput{
		RetryID: pass.RetryID, RunID: runID, SourceID: 4, ExpectedDataRevision: 1,
	})
	if err != nil {
		t.Fatalf("ReconcileRetryAttempt(replayable) error = %v", err)
	}
	if !rec.Replayable {
		t.Fatal("clean unattempted source must reconcile as replayable")
	}

	// A moved revision is a conflict, never a silent replay.
	_, err = ReconcileRetryAttempt(ctx, pool, RetryReconcileInput{
		RetryID: pass.RetryID, RunID: runID, SourceID: 4, ExpectedDataRevision: 99,
	})
	if !errors.Is(err, ErrRetryConflict) {
		t.Fatalf("error = %v, want ErrRetryConflict", err)
	}

	// Target rows without the attempt commit are fatal drift.
	seedImportedTorrent(t, dsn, 4, retryHash4, 1, false)
	_, err = ReconcileRetryAttempt(ctx, pool, RetryReconcileInput{
		RetryID: pass.RetryID, RunID: runID, SourceID: 4, ExpectedDataRevision: 1,
	})
	if !errors.Is(err, ErrRetryConflict) {
		t.Fatalf("error = %v, want ErrRetryConflict", err)
	}
}

func TestRetryResumeMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		attempt func(t *testing.T, dsn, runID, retryID string)
	}{
		{
			name: "after one successful attempt",
			attempt: func(t *testing.T, dsn, runID, retryID string) {
				if err := CommitRetrySuccess(context.Background(), poolFor(t, dsn), RetryAttemptCommit{
					RetryID: retryID, RunID: runID, Record: repairedRecord(3, retryHash3, true),
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "after one unsuccessful attempt",
			attempt: func(t *testing.T, dsn, runID, retryID string) {
				if err := CommitRetryInvalid(context.Background(), poolFor(t, dsn), RetryAttemptCommit{
					RetryID: retryID, RunID: runID, Record: repairedRecord(3, retryHash3, false),
					Stage: "validate", Code: "still_bad",
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "after a mixture",
			attempt: func(t *testing.T, dsn, runID, retryID string) {
				pool := poolFor(t, dsn)
				if err := CommitRetrySuccess(context.Background(), pool, RetryAttemptCommit{
					RetryID: retryID, RunID: runID, Record: repairedRecord(3, retryHash3, true),
				}); err != nil {
					t.Fatal(err)
				}
				if err := CommitRetryInvalid(context.Background(), pool, RetryAttemptCommit{
					RetryID: retryID, RunID: runID, Record: repairedRecord(4, retryHash4, false),
					ExpectedLastSourceID: i64p(3), // cursor after the source-3 repair
					Stage:                "validate", Code: "still_bad",
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := createTarget(t)
			runID := completedRetryFixture(t, dsn)
			pool := poolFor(t, dsn)
			ctx := context.Background()
			first := startRetryPass(t, dsn, runID)
			tc.attempt(t, dsn, runID, first.RetryID)

			// The revision-tracked resume: the pass's current_data_revision
			// equals progress.data_revision (every attempt bumped both in
			// one transaction), so the pass is adopted in place. (The
			// fingerprint half of the discipline is validated with the
			// runner; here the revision equality is what is under test.)
			p := loadedProgress(t, dsn)
			resumed, wasResumed, err := state.StartRetryRun(ctx, pool, state.CreateRetryRunInput{
				RunID: runID, StartDataRevision: p.DataRevision,
			}, func(c *state.RetryRun) bool {
				return c.CurrentDataRevision == p.DataRevision
			})
			if err != nil {
				t.Fatalf("StartRetryRun() error = %v", err)
			}
			if !wasResumed || resumed.RetryID != first.RetryID {
				t.Fatalf("resume = %s resumed=%v, want the original pass adopted", resumed.RetryID, wasResumed)
			}

			// A settled attempt is mechanically unrepeatable (the unique
			// (retry_id, source_id) index aborts the transaction).
			err = pgxBeginFunc(t, dsn, func(tx pgx.Tx) error {
				return state.InsertRetryAttempt(ctx, tx, state.RetryAttempt{
					RetryID: first.RetryID, RunID: runID, SourceID: 3,
					Outcome: state.RetryOutcomeFixed, Stage: "validate", Code: "fixed",
				})
			})
			if err == nil {
				t.Fatal("repeating a settled attempt must violate the unique index")
			}
		})
	}

	t.Run("externally bumped revision refuses resume", func(t *testing.T) {
		t.Parallel()

		dsn := createTarget(t)
		runID := completedRetryFixture(t, dsn)
		pool := poolFor(t, dsn)
		ctx := context.Background()
		first := startRetryPass(t, dsn, runID)
		if err := CommitRetrySuccess(ctx, pool, RetryAttemptCommit{
			RetryID: first.RetryID, RunID: runID, Record: repairedRecord(3, retryHash3, true),
		}); err != nil {
			t.Fatal(err)
		}
		// Something outside the pass moved the revision (e.g. a second,
		// forgotten pass): the pass is stale — resume must refuse.
		execTarget(t, dsn, `UPDATE magnetico_migration.progress SET data_revision = data_revision + 1`)

		p := loadedProgress(t, dsn)
		fresh, wasResumed, err := state.StartRetryRun(ctx, pool, state.CreateRetryRunInput{
			RunID: runID, StartDataRevision: p.DataRevision, UnresolvedAtStart: 1,
			SourceFingerprintRaw: []byte(`{"database":"legacy_source","torrents":4}`),
		}, func(c *state.RetryRun) bool {
			return c.CurrentDataRevision == p.DataRevision
		})
		if err != nil {
			t.Fatalf("StartRetryRun() error = %v", err)
		}
		if wasResumed || fresh.RetryID == first.RetryID {
			t.Fatalf("stale pass resumed: %s resumed=%v", fresh.RetryID, wasResumed)
		}
		old, err := state.GetRetryRun(ctx, pool, first.RetryID)
		if err != nil {
			t.Fatal(err)
		}
		if old.State != state.RetryAborted || old.CompletedAt == nil {
			t.Fatalf("stale pass = %s completed=%v, want aborted", old.State, old.CompletedAt)
		}
		if fresh.StartDataRevision != 2 || fresh.CurrentDataRevision != 2 {
			t.Fatalf("fresh pass revisions = %d/%d, want 2/2",
				fresh.StartDataRevision, fresh.CurrentDataRevision)
		}
	})
}

func TestCompleteRetryPhaseGating(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := completedRetryFixture(t, dsn)
	pool := poolFor(t, dsn)
	ctx := context.Background()
	pass := startRetryPass(t, dsn, runID)

	// Two unresolved records: no flip.
	flipped, err := CompleteRetryPhase(ctx, pool, runID)
	if err != nil {
		t.Fatalf("CompleteRetryPhase() error = %v", err)
	}
	if flipped {
		t.Fatal("phase flipped with unresolved records")
	}

	// One repaired, one still unresolved: still no flip.
	if err := CommitRetrySuccess(ctx, pool, RetryAttemptCommit{
		RetryID: pass.RetryID, RunID: runID, Record: repairedRecord(3, retryHash3, true),
	}); err != nil {
		t.Fatal(err)
	}
	if flipped, err := CompleteRetryPhase(ctx, pool, runID); err != nil || flipped {
		t.Fatalf("CompleteRetryPhase() = %v, %v; want false, nil", flipped, err)
	}

	// A run-scope integrity incident blocks the flip even with zero
	// unresolved record errors (EnsureIncident bumps run_error_count).
	inserted, err := state.EnsureIncident(ctx, pool, runID, state.ErrorRecord{
		Scope: state.ScopeRun, Stage: "read", Category: state.CategoryIntegrity,
		Code: "orphan_source_files",
	})
	if err != nil {
		t.Fatalf("EnsureIncident() error = %v", err)
	}
	if !inserted {
		t.Fatal("EnsureIncident() did not insert")
	}
	if err := CommitRetrySuccess(ctx, pool, RetryAttemptCommit{
		RetryID: pass.RetryID, RunID: runID, Record: repairedRecord(4, retryHash4, false),
		ExpectedLastSourceID: i64p(3), // cursor after the source-3 repair
	}); err != nil {
		t.Fatal(err)
	}
	if flipped, err := CompleteRetryPhase(ctx, pool, runID); err != nil || flipped {
		t.Fatalf("flip with an integrity incident = %v, %v; want false", flipped, err)
	}

	// Resolving the incident (an operator/04C action; the retry pass never
	// auto-clears incidents) unlocks the flip.
	execTarget(t, dsn, `UPDATE magnetico_migration.errors SET resolved = true, resolved_at = now()
        WHERE run_id = $1::uuid AND scope = 'run' AND code = 'orphan_source_files'`, runID)
	execTarget(t, dsn, `UPDATE magnetico_migration.progress SET run_error_count = 0`)
	flipped, err = CompleteRetryPhase(ctx, pool, runID)
	if err != nil {
		t.Fatalf("CompleteRetryPhase() error = %v", err)
	}
	if !flipped {
		t.Fatal("phase must flip when records and integrity are clean")
	}
	p := loadedProgress(t, dsn)
	if p.Phase != state.PhaseImportComplete {
		t.Fatalf("phase = %s, want import_complete", p.Phase)
	}
	// The frozen import checkpoint stayed frozen.
	if p.LastProcessedID != 10 {
		t.Fatalf("checkpoint = %d after the flip, want 10", p.LastProcessedID)
	}

	// Already-clean runs are a no-op.
	if flipped, err := CompleteRetryPhase(ctx, pool, runID); err != nil || flipped {
		t.Fatalf("second flip = %v, %v; want false, nil", flipped, err)
	}
}
