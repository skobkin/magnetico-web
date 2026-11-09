package forensics

// Forensic preflight: the export-side counterpart of the completed-run
// preflight. Fail-open on run content (interrupted, failed, inconsistent
// and v1 state all export), fail-closed on connection and schema structure.
// NEVER calls CompletedRunPreflight, Reconcile or EnsureOperationalSchema,
// and never touches the source in metadata mode.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
)

// Forensics is everything the export and cleanup commands work with after
// the preflight; Release must be called on every exit path.
type Forensics struct {
	Pool       *pgxpool.Pool
	Lock       *state.RunLock
	Identity   pgidentity.Identity
	Inspection state.SchemaInspection
	Class      state.SchemaClass
	Progress   *state.Progress
}

// Release drops the advisory lock and closes the pool. Safe to call twice.
func (f *Forensics) Release() {
	if f.Lock != nil {
		f.Lock.Release(context.Background())
		f.Lock = nil
	}
	if f.Pool != nil {
		f.Pool.Close()
		f.Pool = nil
	}
}

// Preflight opens the target, establishes identity, takes the run lock,
// classifies the operational schema (never DDL) and reads the progress row
// raw — bypassing every Load invariant so damaged state remains exportable.
// Absent schema → ErrNoMigrationHistory; unrecognized → ErrRunStateCorrupt
// with the inspection details; schema present but zero progress rows →
// inconsistent (a completed cleanup leaves NO schema at all).
func Preflight(ctx context.Context, targetDSN string) (*Forensics, error) {
	pool, err := target.OpenPool(ctx, targetDSN)
	if err != nil {
		return nil, err
	}
	f := &Forensics{Pool: pool}
	fail := func(err error) (*Forensics, error) {
		f.Release()
		return nil, err
	}

	identity, err := pgidentity.Read(ctx, pool)
	if err != nil {
		return fail(err)
	}
	f.Identity = identity

	lock, err := state.AcquireRunLock(ctx, targetDSN)
	if err != nil {
		return fail(err)
	}
	f.Lock = lock

	inspection, err := state.InspectOperationalSchema(ctx, pool)
	if err != nil {
		return fail(err)
	}
	f.Inspection = inspection
	f.Class = inspection.Class

	switch inspection.Class {
	case state.SchemaAbsent:
		return fail(fmt.Errorf("%w (schema magnetico_migration does not exist)",
			ErrNoMigrationHistory))
	case state.SchemaUnrecognized:
		return fail(&state.StateError{Err: state.ErrRunStateCorrupt,
			Details: append([]string{"magnetico_migration is not an exact v1 or v2 schema"},
				inspection.Details...)})
	case state.SchemaV1, state.SchemaV2:
		// exportable
	default:
		return fail(fmt.Errorf("unhandled schema class %v", inspection.Class))
	}

	progress, err := state.ReadProgressRaw(ctx, pool, inspection.Class)
	if err != nil {
		if errors.Is(err, state.ErrNoRunState) {
			return fail(&state.StateError{Err: state.ErrRunStateCorrupt, Details: []string{
				"operational schema exists but holds no progress row — history is inconsistent",
			}})
		}
		return fail(err)
	}
	f.Progress = progress
	return f, nil
}
