package target

// TargetFacts reads the target-side facts Reconcile compares with the
// persisted progress row. All reads are SELECT-only, schema-qualified.

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// TargetFacts aggregates the consistency facts of a partially imported
// target: data counts, the no-blob shape invariant, checkpoint honesty and
// the error registry.
func TargetFacts(ctx context.Context, pool *pgxpool.Pool, checkpoint int64, runID string) (state.Facts, error) {
	var f state.Facts
	if err := pool.QueryRow(ctx, `SELECT
            (SELECT COUNT(*) FROM magneticod.torrents),
            (SELECT COUNT(*) FROM magneticod.torrent_file_blobs),
            (SELECT COUNT(*) FROM magneticod.torrents t
              WHERE t.file_count > 1
                AND NOT EXISTS (SELECT 1 FROM magneticod.torrent_file_blobs b WHERE b.torrent_id = t.id)),
            (SELECT COUNT(*) FROM magneticod.torrents WHERE id > $1)
        `, checkpoint).Scan(&f.Torrents, &f.Blobs, &f.BloblessMultiFile, &f.RowsBeyondCheckpoint); err != nil {
		return state.Facts{}, err
	}
	recordErrors, unresolvedIntegrity, err := state.CountErrors(ctx, pool, runID)
	if err != nil {
		return state.Facts{}, err
	}
	f.RecordErrors = recordErrors
	f.UnresolvedIntegrity = unresolvedIntegrity
	return f, nil
}
