package target

// Completed-run preflight for the 04B commands (verify-decode,
// verify-content, retry-errors): every one requires a completed 04A import
// (import_complete or import_complete_with_errors), strict production
// schema, consistent run state and the run lock. The stored
// defer-secondary-indexes decision is READ, never required again.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// ErrRunNotComplete reports that the target has no completed 04A import run:
// verification and retry commands refuse rather than scan a moving dataset.
var ErrRunNotComplete = errors.New("no completed migration import run on this target")

// CompletedResult is the completed-run preflight outcome: verified run
// state, the held run lock and the caller's pool. The caller owns both for
// the command's lifetime (Release/Close).
type CompletedResult struct {
	Progress *state.Progress
	Lock     *state.RunLock
	Pool     *pgxpool.Pool
	Identity pgidentity.Identity
}

// CompletedRunPreflight verifies a target for a 04B command and returns the
// held lock plus the caller's pool:
//
//   - operational schema brought to the current version first (definition-
//     validated; 04A databases upgrade transparently);
//   - run state present and in a completed phase, else ErrRunNotComplete;
//   - strict production schema (the stored index-deferral decision applies);
//   - full Reconcile of target data against the persisted state.
//
// No source connection is involved: source/target distinctness is checked
// by the commands that connect to a source, never here.
func CompletedRunPreflight(ctx context.Context, dsn string) (*CompletedResult, error) {
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		return nil, err
	}
	identity, err := pgidentity.Read(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("%w: %v", ErrTargetIdentityUnknown, err)
	}
	if !identity.Known {
		pool.Close()
		return nil, ErrTargetIdentityUnknown
	}

	lock, err := state.AcquireRunLock(ctx, dsn)
	if err != nil {
		pool.Close()
		return nil, err
	}

	result, err := completedPreflightLocked(ctx, pool, dsn)
	if err != nil {
		lock.Release(context.WithoutCancel(ctx))
		pool.Close()
		return nil, err
	}
	return &CompletedResult{Progress: result, Lock: lock, Pool: pool, Identity: identity}, nil
}

func completedPreflightLocked(ctx context.Context, pool *pgxpool.Pool, dsn string) (*state.Progress, error) {
	if err := state.EnsureOperationalSchema(ctx, pool); err != nil {
		return nil, err
	}
	existing, err := state.Load(ctx, pool)
	if err != nil {
		if errors.Is(err, state.ErrNoRunState) {
			return nil, fmt.Errorf("%w (the target has no migration run state)", ErrRunNotComplete)
		}
		return nil, err
	}
	if !existing.Phase.Complete() {
		return nil, fmt.Errorf("%w (phase %q — finish `migrate torrents` first)", ErrRunNotComplete, existing.Phase)
	}

	// The stored index-deferral decision applies; the 04B commands never
	// re-ask for it.
	if err := VerifySchema(ctx, dsn, verifyInput{
		deferRequested:  existing.DeferIndexes,
		indexesDeferred: existing.IndexesDeferred,
		phase:           existing.Phase,
	}); err != nil {
		return nil, err
	}

	facts, err := TargetFacts(ctx, pool, existing.LastProcessedID, existing.RunID)
	if err != nil {
		return nil, err
	}
	if err := state.Reconcile(existing, facts); err != nil {
		return nil, err
	}
	return existing, nil
}
