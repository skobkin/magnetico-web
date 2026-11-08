package target

// Retry transactions: per-ID atomic repair of unresolved record errors.
// Every attempt is ONE transaction covering the target data (torrent +
// optional blob with the exact historical ID), the error registry update,
// the append-only attempt row, the progress counters and the data revision,
// and the retry pass's own revision-tracked cursor. No ON CONFLICT, no
// overwrites: unexpected target state is a fatal conflict, never a skip.
// The import checkpoint (scanned_count, last_processed_id) is never touched.

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// ErrRetryConflict reports that an attempt's preconditions moved underneath
// it (the error row resolved, or a target row/ID/infohash collision
// appeared): fatal, never auto-resolved.
var ErrRetryConflict = errors.New("retry precondition conflict")

// RetryRecord is one repaired record: the exact historical source ID and
// the production-validated metadata; Blob nil = no-blob eligible single.
type RetryRecord struct {
	SourceID     int64
	InfoHash     []byte
	Name         string
	TotalSize    int64
	DiscoveredOn int64
	FileCount    int32
	Blob         []byte
}

// RetryAttemptCommit seeds one attempt transaction. The cursor CAS input
// (ExpectedLastSourceID) serializes the pass's own progress.
type RetryAttemptCommit struct {
	RetryID string
	RunID   string
	Record  RetryRecord
	// ExpectedLastSourceID is the pass cursor before this attempt (nil at
	// the first attempt).
	ExpectedLastSourceID *int64
	// Stage/Code/Details classify still-invalid and missing-source attempts.
	Stage   string
	Code    string
	Details map[string]any
}

// CommitRetrySuccess atomically inserts the torrent (+blob) with the exact
// historical ID, resolves the original error (attempts+1, timestamp),
// appends the 'fixed' attempt, updates the progress counters
// (success_count+1, error_count-1, blob_count+Δ, data_revision+1 —
// scanned_count and last_processed_id untouched) and advances the pass
// cursor with its own revision bump.
func CommitRetrySuccess(ctx context.Context, pool *pgxpool.Pool, in RetryAttemptCommit) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := proveUnresolvedRecord(ctx, tx, in.RunID, in.Record.SourceID); err != nil {
			return err
		}
		if err := proveNoTargetRow(ctx, tx, in.Record.SourceID, in.Record.InfoHash); err != nil {
			return err
		}
		if err := insertRetryTorrent(ctx, tx, in.Record); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE magnetico_migration.errors
            SET attempts = attempts + 1, resolved = true, resolved_at = now()
            WHERE run_id = $1::uuid AND scope = 'record' AND source_id = $2`,
			in.RunID, in.Record.SourceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: error row for source %d vanished", ErrRetryConflict, in.Record.SourceID)
		}
		if err := state.InsertRetryAttempt(ctx, tx, state.RetryAttempt{
			RetryID:        in.RetryID,
			RunID:          in.RunID,
			SourceID:       in.Record.SourceID,
			SourceInfoHash: in.Record.InfoHash,
			Outcome:        state.RetryOutcomeFixed,
			Stage:          in.Stage,
			Code:           "fixed",
			Details:        in.Details,
		}); err != nil {
			return err
		}
		blobDelta := int64(0)
		if in.Record.Blob != nil {
			blobDelta = 1
		}
		tag, err = tx.Exec(ctx, `UPDATE magnetico_migration.progress SET
            success_count = success_count + 1,
            error_count = error_count - 1,
            blob_count = blob_count + $2,
            data_revision = data_revision + 1,
            updated_at = now()
        WHERE run_id = $1::uuid AND error_count > 0`,
			in.RunID, blobDelta)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: progress error_count is already 0 while record %d is unresolved",
				state.ErrRunStateCorrupt, in.Record.SourceID)
		}
		return advanceRetryCursor(ctx, tx, in, state.AdvanceRetryInput{
			RetryID: in.RetryID, ExpectedLastSourceID: in.ExpectedLastSourceID,
			NewLastSourceID: &in.Record.SourceID, FixedDelta: 1,
		})
	})
}

// CommitRetryInvalid records a still-invalid repair attempt: the record
// error stays unresolved with attempts+1, the typed attempt row is
// appended, data_revision advances (registry state changed) — the import
// counters do not.
func CommitRetryInvalid(ctx context.Context, pool *pgxpool.Pool, in RetryAttemptCommit) error {
	return commitNonRepairRetry(ctx, pool, in, state.RetryOutcomeStillInvalid)
}

// CommitRetryMissing records that the original source record disappeared:
// explicitly diagnosed (never silently resolved), attempts+1, typed
// 'missing_source' attempt, data_revision advances.
func CommitRetryMissing(ctx context.Context, pool *pgxpool.Pool, in RetryAttemptCommit) error {
	return commitNonRepairRetry(ctx, pool, in, state.RetryOutcomeMissing)
}

func commitNonRepairRetry(ctx context.Context, pool *pgxpool.Pool, in RetryAttemptCommit, outcome string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := proveUnresolvedRecord(ctx, tx, in.RunID, in.Record.SourceID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE magnetico_migration.errors
            SET attempts = attempts + 1
            WHERE run_id = $1::uuid AND scope = 'record' AND source_id = $2`,
			in.RunID, in.Record.SourceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: error row for source %d vanished", ErrRetryConflict, in.Record.SourceID)
		}
		if err := state.InsertRetryAttempt(ctx, tx, state.RetryAttempt{
			RetryID:        in.RetryID,
			RunID:          in.RunID,
			SourceID:       in.Record.SourceID,
			SourceInfoHash: in.Record.InfoHash,
			Outcome:        outcome,
			Stage:          in.Stage,
			Code:           in.Code,
			Details:        in.Details,
		}); err != nil {
			return err
		}
		tag, err = tx.Exec(ctx, `UPDATE magnetico_migration.progress SET
            data_revision = data_revision + 1, updated_at = now()
        WHERE run_id = $1::uuid`, in.RunID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: progress row vanished", state.ErrRunStateCorrupt)
		}
		delta := state.AdvanceRetryInput{
			RetryID: in.RetryID, ExpectedLastSourceID: in.ExpectedLastSourceID,
			NewLastSourceID: &in.Record.SourceID,
		}
		if outcome == state.RetryOutcomeStillInvalid {
			delta.StillInvalidDelta = 1
		} else {
			delta.MissingDelta = 1
		}
		return advanceRetryCursor(ctx, tx, in, delta)
	})
}

// advanceRetryCursor applies the pass's revision-tracked cursor update in
// the attempt's transaction; a lost CAS is fatal (the pass moved
// underneath us).
func advanceRetryCursor(ctx context.Context, tx pgx.Tx, in RetryAttemptCommit, delta state.AdvanceRetryInput) error {
	advanced, err := state.AdvanceRetryRun(ctx, tx, delta)
	if err != nil {
		return err
	}
	if !advanced {
		return fmt.Errorf("%w: retry pass cursor moved underneath attempt for source %d",
			ErrRetryConflict, in.Record.SourceID)
	}
	return nil
}

// proveUnresolvedRecord locks and verifies the error row: present and
// unresolved, or the attempt refuses.
func proveUnresolvedRecord(ctx context.Context, tx pgx.Tx, runID string, sourceID int64) error {
	var resolved bool
	err := tx.QueryRow(ctx, `SELECT resolved FROM magnetico_migration.errors
        WHERE run_id = $1::uuid AND scope = 'record' AND source_id = $2
        FOR UPDATE`, runID, sourceID).Scan(&resolved)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no record error for source %d", state.ErrRunStateCorrupt, sourceID)
	}
	if err != nil {
		return err
	}
	if resolved {
		return fmt.Errorf("%w: record error for source %d is already resolved", ErrRetryConflict, sourceID)
	}
	return nil
}

// proveNoTargetRow verifies the repair target is really absent: no torrent
// row with the historical ID and no infohash collision.
func proveNoTargetRow(ctx context.Context, tx pgx.Tx, sourceID int64, infoHash []byte) error {
	var byID, byHash int64
	if err := tx.QueryRow(ctx, `SELECT
            (SELECT COUNT(*) FROM magneticod.torrents WHERE id = $1),
            (SELECT COUNT(*) FROM magneticod.torrents WHERE info_hash = $2)`,
		sourceID, infoHash).Scan(&byID, &byHash); err != nil {
		return err
	}
	if byID > 0 {
		return fmt.Errorf("%w: target already has torrent id %d", ErrRetryConflict, sourceID)
	}
	if byHash > 0 {
		return fmt.Errorf("%w: target already has the info hash of source %d", ErrRetryConflict, sourceID)
	}
	return nil
}

// insertRetryTorrent inserts the repaired torrent with its exact historical
// ID plus the blob when present. No ON CONFLICT: proveNoTargetRow ran first.
func insertRetryTorrent(ctx context.Context, tx pgx.Tx, rec RetryRecord) error {
	if rec.SourceID > math.MaxInt32 || rec.SourceID < math.MinInt32 {
		return fmt.Errorf("%w: source ID %d exceeds the target's int32 id range",
			state.ErrRunStateCorrupt, rec.SourceID)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO magneticod.torrents
        (id, info_hash, name, total_size, discovered_on, file_count)
        VALUES ($1, $2, $3, $4, $5, $6)`,
		int32(rec.SourceID), rec.InfoHash, rec.Name, rec.TotalSize, rec.DiscoveredOn, rec.FileCount); err != nil {
		return err
	}
	if rec.Blob != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO magneticod.torrent_file_blobs (torrent_id, data)
            VALUES ($1, $2)`, int32(rec.SourceID), rec.Blob); err != nil {
			return err
		}
	}
	return nil
}

// RetryReconciliation classifies an ambiguous attempt COMMIT (the database
// accepted it but the client never learned): the attempt row is the
// lost-COMMIT marker.
type RetryReconciliation struct {
	// Committed: the attempt row exists — the transaction landed.
	Committed bool
	// Replayable: provably not landed (unresolved error, no target row,
	// revision unchanged) — safe to replay.
	Replayable bool
}

// RetryReconcileInput probes one ambiguous attempt.
type RetryReconcileInput struct {
	RetryID  string
	RunID    string
	SourceID int64
	// ExpectedDataRevision is progress.data_revision BEFORE the ambiguous
	// attempt (a bumped revision means the attempt — or something else —
	// committed).
	ExpectedDataRevision int64
}

// ReconcileRetryAttempt decides whether an ambiguous attempt transaction
// committed:
//
//   - attempt row present  ⇒ committed (idempotent: settled);
//   - absent, error still unresolved, no target row by ID or infohash, and
//     progress.data_revision unchanged ⇒ replayable;
//   - anything else is corrupt/conflict state — fatal, never guessed past.
func ReconcileRetryAttempt(ctx context.Context, pool *pgxpool.Pool, in RetryReconcileInput) (RetryReconciliation, error) {
	attempt, err := state.GetRetryAttempt(ctx, pool, in.RetryID, in.SourceID)
	if err != nil {
		return RetryReconciliation{}, err
	}
	if attempt != nil {
		return RetryReconciliation{Committed: true}, nil
	}

	var resolved bool
	var infoHash []byte
	err = pool.QueryRow(ctx, `SELECT resolved, source_info_hash FROM magnetico_migration.errors
        WHERE run_id = $1::uuid AND scope = 'record' AND source_id = $2`,
		in.RunID, in.SourceID).Scan(&resolved, &infoHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return RetryReconciliation{}, fmt.Errorf("%w: error row for source %d vanished",
			state.ErrRunStateCorrupt, in.SourceID)
	}
	if err != nil {
		return RetryReconciliation{}, err
	}
	if resolved {
		return RetryReconciliation{}, fmt.Errorf("%w: error for source %d resolved without its attempt row",
			ErrRetryConflict, in.SourceID)
	}

	var byID, byHash int64
	if err := pool.QueryRow(ctx, `SELECT
            (SELECT COUNT(*) FROM magneticod.torrents WHERE id = $1),
            (SELECT COUNT(*) FROM magneticod.torrents WHERE info_hash = $2)`,
		in.SourceID, infoHash).Scan(&byID, &byHash); err != nil {
		return RetryReconciliation{}, err
	}
	if byID > 0 || byHash > 0 {
		return RetryReconciliation{}, fmt.Errorf("%w: target rows for source %d appeared without the attempt commit",
			ErrRetryConflict, in.SourceID)
	}

	var revision int64
	if err := pool.QueryRow(ctx, `SELECT data_revision FROM magnetico_migration.progress
        WHERE run_id = $1::uuid`, in.RunID).Scan(&revision); err != nil {
		return RetryReconciliation{}, err
	}
	if revision != in.ExpectedDataRevision {
		return RetryReconciliation{}, fmt.Errorf("%w: data_revision %d != expected %d for source %d",
			ErrRetryConflict, revision, in.ExpectedDataRevision, in.SourceID)
	}
	return RetryReconciliation{Replayable: true}, nil
}

// CompleteRetryPhase flips import_complete_with_errors → import_complete
// when a retry pass repaired everything: zero unresolved record errors AND
// zero unresolved integrity incidents. It CAS-advances the frozen
// checkpoint (same last_processed_id, new phase) — the one edge the import
// code paths can never reach. Returns false (no-op) when the phase is not
// import_complete_with_errors; ErrCheckpointConflict on a lost CAS.
func CompleteRetryPhase(ctx context.Context, pool *pgxpool.Pool, runID string) (bool, error) {
	var flipped bool
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var phase string
		var lastID int64
		err := tx.QueryRow(ctx, `SELECT phase, last_processed_id FROM magnetico_migration.progress
            WHERE run_id = $1::uuid FOR UPDATE`, runID).Scan(&phase, &lastID)
		if err != nil {
			return err
		}
		if state.Phase(phase) != state.PhaseCompleteWithErrors {
			return nil // nothing to flip (already clean, or still importing)
		}
		recordErrors, integrity, err := state.CountErrors(ctx, tx, runID)
		if err != nil {
			return err
		}
		if recordErrors != 0 || integrity != 0 {
			return nil // gating: unresolved work remains
		}
		advanced, err := state.AdvanceCheckpoint(ctx, tx, state.AdvanceInput{
			RunID:          runID,
			ExpectedLastID: lastID,
			ExpectedPhase:  state.PhaseCompleteWithErrors,
			NewLastID:      lastID,
			NewPhase:       state.PhaseImportComplete,
		})
		if err != nil {
			return err
		}
		if !advanced {
			return ErrCheckpointConflict
		}
		flipped = true
		return nil
	})
	return flipped, err
}
