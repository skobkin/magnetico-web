package target

// Suite ⑤ (writer): batch atomicity (torrents + blobs + error rows +
// checkpoint in one transaction), blob-failure rollback via constraint/
// trigger injection, the checkpoint compare-and-swap under a competing
// writer, ambiguity reconciliation and phase transitions.

import (
	"context"
	"errors"
	"testing"

	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// initRunState initializes the operational schema and one progress row on a
// fresh target, returning the run ID.
func initRunState(t *testing.T, dsn string) string {
	t.Helper()
	pool := poolFor(t, dsn)
	runID := storedRunIDForInit(t)
	_, err := state.InitRun(context.Background(), pool, state.InitInput{
		RunID:           runID,
		FingerprintRaw:  []byte(`{"database":"src"}`),
		SourceAdapter:   "original-v0.12.0",
		ContractVersion: 1,
		CodecProfile:    "compact",
		ExpectedTotal:   3,
		SourceMinID:     1,
		SourceMaxID:     3,
		SourceFileRows:  4,
		DeferIndexes:    false,
		Options:         state.Options{Workers: 2, BatchSize: 10, MaxInflightBytes: 1 << 20},
	})
	if err != nil {
		t.Fatalf("InitRun() error = %v", err)
	}
	return runID
}

// storedRunIDForInit fabricates a v4-shaped run ID for InitRun (the tests
// never depend on its randomness).
func storedRunIDForInit(t *testing.T) string {
	t.Helper()
	id, err := state.NewRunID()
	if err != nil {
		t.Fatalf("NewRunID() error = %v", err)
	}
	return id
}

func batchFor(runID string, lastID int64) Batch {
	return Batch{
		RunID:          runID,
		ExpectedLastID: 0,
		ExpectedPhase:  state.PhaseImporting,
		LastSourceID:   lastID,
	}
}

func TestWriteBatchCommitsAtomically(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := initRunState(t, dsn)
	committer := NewBatchCommitter(poolFor(t, dsn))

	b := batchFor(runID, 3)
	b.Torrents = []TorrentRow{
		{ID: 1, InfoHash: []byte("\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01"),
			Name: "multi", TotalSize: 6, DiscoveredOn: 1700000001, FileCount: 3,
			Blob: []byte("blob-data-for-multi")},
		// ID 2 is a record error (invalid metadata), not a torrent row.
		{ID: 3, InfoHash: []byte("\x03\x03\x03\x03\x03\x03\x03\x03\x03\x03\x03\x03\x03\x03\x03\x03\x03\x03\x03\x03"),
			Name: "single", TotalSize: 4, DiscoveredOn: 1700000003, FileCount: 1},
	}
	b.Errors = []state.ErrorRecord{{
		Scope: state.ScopeRecord, SourceID: ptrInt64(2),
		SourceInfoHash: []byte("\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02"),
		Stage:          "validate", Category: state.CategoryIntegrity, Code: "invalid_metadata",
	}}

	if err := committer.CommitBatch(context.Background(), b); err != nil {
		t.Fatalf("CommitBatch() error = %v", err)
	}

	p := loadedProgress(t, dsn)
	if p.LastProcessedID != 3 || p.ScannedCount != 3 || p.SuccessCount != 2 ||
		p.ErrorCount != 1 || p.BlobCount != 1 {
		t.Fatalf("progress after batch = %+v", p)
	}

	var torrents, blobs, errs int64
	if err := poolFor(t, dsn).QueryRow(context.Background(),
		`SELECT (SELECT COUNT(*) FROM magneticod.torrents),
		        (SELECT COUNT(*) FROM magneticod.torrent_file_blobs),
		        (SELECT COUNT(*) FROM magnetico_migration.errors)`).Scan(&torrents, &blobs, &errs); err != nil {
		t.Fatalf("count: %v", err)
	}
	if torrents != 2 || blobs != 1 || errs != 1 {
		t.Fatalf("rows = torrents %d blobs %d errors %d, want 2/1/1", torrents, blobs, errs)
	}

	var name, blob string
	if err := poolFor(t, dsn).QueryRow(context.Background(),
		`SELECT t.name, b.data::text FROM magneticod.torrents t
         JOIN magneticod.torrent_file_blobs b ON b.torrent_id = t.id WHERE t.id = 1`).Scan(&name, &blob); err != nil {
		t.Fatalf("torrent 1 + blob: %v", err)
	}
	if name != "multi" {
		t.Fatalf("torrent 1 name = %q", name)
	}

	// Torrent 3 (single typical file shape) has no blob row.
	var hasBlob bool
	if err := poolFor(t, dsn).QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM magneticod.torrent_file_blobs WHERE torrent_id = 3)`).Scan(&hasBlob); err != nil {
		t.Fatalf("blob check: %v", err)
	}
	if hasBlob {
		t.Fatal("torrent 3 must not carry a blob")
	}
}

func TestWriteBatchBlobFailureRollsBackEverything(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := initRunState(t, dsn)

	// Constraint/trigger injection: the second blob insert explodes; the
	// whole batch — including the already-copied torrent rows and the
	// error row — must roll back.
	execTarget(t, dsn, `CREATE FUNCTION magneticod.reject_blob_2() RETURNS trigger AS $$
        BEGIN
            IF NEW.torrent_id = 2 THEN
                RAISE EXCEPTION 'injected blob failure for torrent 2';
            END IF;
            RETURN NEW;
        END $$ LANGUAGE plpgsql;
        CREATE TRIGGER tr_reject_blob_2 BEFORE INSERT ON magneticod.torrent_file_blobs
        FOR EACH ROW EXECUTE FUNCTION magneticod.reject_blob_2();`)

	committer := NewBatchCommitter(poolFor(t, dsn))
	b := batchFor(runID, 2)
	b.Torrents = []TorrentRow{
		{ID: 1, InfoHash: []byte("\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01"),
			Name: "a", TotalSize: 1, DiscoveredOn: 1700000001, FileCount: 1,
			Blob: []byte("ok")},
		{ID: 2, InfoHash: []byte("\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02"),
			Name: "b", TotalSize: 1, DiscoveredOn: 1700000002, FileCount: 1,
			Blob: []byte("boom")},
	}
	b.Errors = []state.ErrorRecord{{
		Scope: state.ScopeRecord, SourceID: ptrInt64(3),
		Stage: "read", Category: state.CategoryIntegrity, Code: "missing_files",
	}}

	if err := committer.CommitBatch(context.Background(), b); err == nil {
		t.Fatal("CommitBatch() must fail on the injected blob error")
	}

	var torrents, blobs, errs int64
	if err := poolFor(t, dsn).QueryRow(context.Background(),
		`SELECT (SELECT COUNT(*) FROM magneticod.torrents),
		        (SELECT COUNT(*) FROM magneticod.torrent_file_blobs),
		        (SELECT COUNT(*) FROM magnetico_migration.errors)`).Scan(&torrents, &blobs, &errs); err != nil {
		t.Fatalf("count: %v", err)
	}
	if torrents != 0 || blobs != 0 || errs != 0 {
		t.Fatalf("rollback left rows: torrents %d blobs %d errors %d, want 0/0/0", torrents, blobs, errs)
	}
	if p := loadedProgress(t, dsn); p.LastProcessedID != 0 || p.ScannedCount != 0 {
		t.Fatalf("checkpoint advanced despite rollback: %+v", p)
	}
}

func TestWriteBatchCASConflictUnderCompetingWriter(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := initRunState(t, dsn)

	// A competing writer advances the checkpoint PAST our (stale)
	// expectation between our snapshot and our commit: the CAS must affect
	// zero rows, roll the batch back and surface ErrCheckpointConflict.
	// (Fresh checkpoint is 0; the competing writer moves it to 2 while this
	// batch still expects 1.)
	advanceRun(t, dsn, loadedProgress(t, dsn), state.AdvanceInput{
		RunID: runID, ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseImporting,
		ExpectedLastID: 0, NewLastID: 2, ScannedDelta: 1, SuccessDelta: 1,
	})

	committer := NewBatchCommitter(poolFor(t, dsn))
	b := batchFor(runID, 3) // expects checkpoint 0
	b.ExpectedLastID = 1
	b.Torrents = []TorrentRow{
		{ID: 2, InfoHash: []byte("\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02\x02"),
			Name: "b", TotalSize: 1, DiscoveredOn: 1700000002, FileCount: 1},
	}
	err := committer.CommitBatch(context.Background(), b)
	if !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("CommitBatch() error = %v, want ErrCheckpointConflict", err)
	}

	// Nothing written by the losing batch.
	var torrents int64
	if err := poolFor(t, dsn).QueryRow(context.Background(),
		`SELECT COUNT(*) FROM magneticod.torrents WHERE id = 2`).Scan(&torrents); err != nil {
		t.Fatalf("count: %v", err)
	}
	if torrents != 0 {
		t.Fatal("the losing batch must leave no rows behind")
	}
	if p := loadedProgress(t, dsn); p.LastProcessedID != 2 {
		t.Fatalf("checkpoint = %d, want the competing writer's 2", p.LastProcessedID)
	}
}

func TestReconcileAmbiguousCommit(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := initRunState(t, dsn)
	pool := poolFor(t, dsn)
	ctx := context.Background()

	// Case 1: the commit landed (checkpoint through the attempted ID).
	seedImportedTorrent(t, dsn, 1, "1111111111111111111111111111111111111111", 1, false)
	advanceRun(t, dsn, loadedProgress(t, dsn), state.AdvanceInput{
		RunID: runID, ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseImporting,
		ExpectedLastID: 0, NewLastID: 1, ScannedDelta: 1, SuccessDelta: 1,
	})
	committed, err := ReconcileAmbiguousCommit(ctx, pool, runID, 0, 1)
	if err != nil || !committed {
		t.Fatalf("ReconcileAmbiguousCommit(landed) = (%v, %v), want true, nil", committed, err)
	}

	// Case 2: nothing landed — replayable.
	committed, err = ReconcileAmbiguousCommit(ctx, pool, runID, 1, 3)
	if err != nil || committed {
		t.Fatalf("ReconcileAmbiguousCommit(replayable) = (%v, %v), want false, nil", committed, err)
	}

	// Case 3: rows beyond the checkpoint without an advance — drift.
	seedImportedTorrent(t, dsn, 2, "2222222222222222222222222222222222222222", 1, false)
	_, err = ReconcileAmbiguousCommit(ctx, pool, runID, 1, 3)
	if !errors.Is(err, ErrTargetDrift) {
		t.Fatalf("ReconcileAmbiguousCommit(drift) error = %v, want ErrTargetDrift", err)
	}

	// Case 4: checkpoint neither previous nor attempted — drift.
	advanceRun(t, dsn, loadedProgress(t, dsn), state.AdvanceInput{
		RunID: runID, ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseImporting,
		ExpectedLastID: 1, NewLastID: 2, ScannedDelta: 1, SuccessDelta: 1,
	})
	_, err = ReconcileAmbiguousCommit(ctx, pool, runID, 0, 1)
	if !errors.Is(err, ErrTargetDrift) {
		t.Fatalf("ReconcileAmbiguousCommit(neither) error = %v, want ErrTargetDrift", err)
	}
}

func TestTransitionPhaseCAS(t *testing.T) {
	t.Parallel()

	dsn := createTarget(t)
	runID := initRunState(t, dsn)
	pool := poolFor(t, dsn)
	ctx := context.Background()

	// Legal: importing → data_loaded at the final checkpoint.
	if err := TransitionPhase(ctx, pool, runID, state.AdvanceInput{
		RunID: runID, ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseDataLoaded,
		ExpectedLastID: 0, NewLastID: 0,
	}); err != nil {
		t.Fatalf("TransitionPhase() error = %v", err)
	}
	if p := loadedProgress(t, dsn); p.Phase != state.PhaseDataLoaded {
		t.Fatalf("phase = %s, want data_loaded", p.Phase)
	}

	// Stale expectation: another transition already happened.
	err := TransitionPhase(ctx, pool, runID, state.AdvanceInput{
		RunID: runID, ExpectedPhase: state.PhaseImporting, NewPhase: state.PhaseDataLoaded,
		ExpectedLastID: 0, NewLastID: 0,
	})
	if !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("stale TransitionPhase() error = %v, want ErrCheckpointConflict", err)
	}
}
