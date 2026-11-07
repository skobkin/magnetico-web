package target

// Batch writer: torrents (explicit source IDs), file blobs, error rows and
// the compare-and-swap checkpoint advance in ONE transaction. A batch never
// uses ON CONFLICT — an already-present ID beyond the checkpoint is a fatal
// conflict, never a silent skip.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// ErrCheckpointConflict reports that the compare-and-swap checkpoint update
// affected no row: the run state moved underneath this batch (a concurrent
// writer or lost advisory lock). The whole batch rolls back.
var ErrCheckpointConflict = errors.New("checkpoint compare-and-swap conflict: run state moved underneath this batch")

// ErrTargetDrift reports target rows that contradict the persisted
// checkpoint — corrupt state, never auto-repaired.
var ErrTargetDrift = errors.New("target state drifted from the migration checkpoint")

// TorrentRow is one torrent to bulk-load, with its exact source ID and
// optional file blob (nil = no-blob eligible single file).
type TorrentRow struct {
	ID           int32
	InfoHash     []byte
	Name         string
	TotalSize    int64
	DiscoveredOn int64
	FileCount    int32
	Blob         []byte
}

// Batch is one commit unit for WriteBatch/CommitBatch.
type Batch struct {
	RunID          string
	ExpectedLastID int64
	ExpectedPhase  state.Phase
	LastSourceID   int64 // new checkpoint after this batch
	Torrents       []TorrentRow
	Errors         []state.ErrorRecord
}

// BatchCommitter commits batches against one target pool.
type BatchCommitter struct {
	pool *pgxpool.Pool
}

func NewBatchCommitter(pool *pgxpool.Pool) *BatchCommitter {
	return &BatchCommitter{pool: pool}
}

// CommitBatch atomically writes torrents + blobs + error rows and advances
// the checkpoint via compare-and-swap. On ErrCheckpointConflict everything
// rolls back and the run must stop as fatal.
func (c *BatchCommitter) CommitBatch(ctx context.Context, b Batch) error {
	return pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		return WriteBatchTx(ctx, tx, b)
	})
}

// WriteBatchTx is the transaction body of CommitBatch, exported for
// in-module composition.
func WriteBatchTx(ctx context.Context, tx pgx.Tx, b Batch) error {
	if len(b.Torrents) > 0 {
		rows := make([][]any, len(b.Torrents))
		blobs := make([][]any, 0, len(b.Torrents))
		for i, t := range b.Torrents {
			rows[i] = []any{t.ID, t.InfoHash, t.Name, t.TotalSize, t.DiscoveredOn, t.FileCount}
			if t.Blob != nil {
				blobs = append(blobs, []any{t.ID, t.Blob})
			}
		}
		if _, err := tx.CopyFrom(ctx,
			pgx.Identifier{"magneticod", "torrents"},
			[]string{"id", "info_hash", "name", "total_size", "discovered_on", "file_count"},
			pgx.CopyFromRows(rows),
		); err != nil {
			return fmt.Errorf("copy torrents: %w", err)
		}
		if len(blobs) > 0 {
			if _, err := tx.CopyFrom(ctx,
				pgx.Identifier{"magneticod", "torrent_file_blobs"},
				[]string{"torrent_id", "data"},
				pgx.CopyFromRows(blobs),
			); err != nil {
				return fmt.Errorf("copy blobs: %w", err)
			}
		}
	}
	if len(b.Errors) > 0 {
		if err := state.InsertErrorRecords(ctx, tx, b.RunID, b.Errors); err != nil {
			return fmt.Errorf("insert error records: %w", err)
		}
	}
	blobCount := int64(0)
	for _, t := range b.Torrents {
		if t.Blob != nil {
			blobCount++
		}
	}
	advanced, err := state.AdvanceCheckpoint(ctx, tx, state.AdvanceInput{
		RunID:          b.RunID,
		ExpectedLastID: b.ExpectedLastID,
		ExpectedPhase:  b.ExpectedPhase,
		NewLastID:      b.LastSourceID,
		NewPhase:       b.ExpectedPhase,
		ScannedDelta:   int64(len(b.Torrents) + len(b.Errors)),
		SuccessDelta:   int64(len(b.Torrents)),
		ErrorDelta:     int64(len(b.Errors)),
		BlobDelta:      blobCount,
	})
	if err != nil {
		return err
	}
	if !advanced {
		return ErrCheckpointConflict
	}
	return nil
}

// ReconcileAmbiguousCommit resolves an uncertain batch outcome against the
// authoritative stored checkpoint: advanced through the attempted ID ⇒ the
// commit landed; still at the previous checkpoint with no rows in the
// attempted range ⇒ the batch never landed and may be replayed; anything
// else is fatal drift.
func ReconcileAmbiguousCommit(ctx context.Context, pool *pgxpool.Pool, runID string, prevCheckpoint, attemptedThrough int64) (committed bool, err error) {
	var ckpt int64
	err = pool.QueryRow(ctx,
		`SELECT last_processed_id FROM magnetico_migration.progress WHERE run_id = $1::uuid`,
		runID).Scan(&ckpt)
	if err != nil {
		return false, fmt.Errorf("%w: read checkpoint: %w", ErrTargetDrift, err)
	}
	switch ckpt {
	case attemptedThrough:
		return true, nil
	case prevCheckpoint:
		var rows int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM magneticod.torrents WHERE id > $1 AND id <= $2`,
			prevCheckpoint, attemptedThrough).Scan(&rows); err != nil {
			return false, fmt.Errorf("%w: count attempted range: %w", ErrTargetDrift, err)
		}
		if rows > 0 {
			return false, fmt.Errorf("%w: %d torrent rows exist in (%d,%d] without a checkpoint advance",
				ErrTargetDrift, rows, prevCheckpoint, attemptedThrough)
		}
		return false, nil
	default:
		return false, fmt.Errorf("%w: checkpoint %d is neither the previous %d nor the attempted %d",
			ErrTargetDrift, ckpt, prevCheckpoint, attemptedThrough)
	}
}

// TransitionPhase performs one compare-and-swap phase advance in its own
// transaction (e.g. importing → data_loaded). ErrCheckpointConflict on any
// mismatch.
func TransitionPhase(ctx context.Context, pool *pgxpool.Pool, runID string, in state.AdvanceInput) error {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		advanced, err := state.AdvanceCheckpoint(ctx, tx, in)
		if err != nil {
			return err
		}
		if !advanced {
			return ErrCheckpointConflict
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("transition to %s: %w", in.NewPhase, err)
	}
	return nil
}
