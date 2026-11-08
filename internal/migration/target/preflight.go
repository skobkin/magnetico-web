// Package target owns the migration's target-side concerns: preflight
// verification, run-state initialization/resume decisions, the bulk writer
// and index finalization. It never uses persistence.Store's AddTorrent
// (which generates new IDs) — the import path is explicit-ID bulk insert.
package target

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/persistence"
)

var (
	// ErrSameDatabase reports source and target resolving to the same actual
	// PostgreSQL database (any transport or endpoint alias).
	ErrSameDatabase = errors.New(
		"source and target resolve to the same PostgreSQL database; migration requires two distinct databases")

	// ErrTargetIdentityUnknown reports that the target's server identity
	// could not be established; fail closed, never assume distinctness.
	ErrTargetIdentityUnknown = errors.New(
		"cannot establish the target database's identity (refusing to proceed)")

	// ErrTargetNotEmpty reports torrents in a target that has no migration
	// run state; such a database is never silently adopted.
	ErrTargetNotEmpty = errors.New(
		"target contains torrents but no migration run state; refusing to import into a nonempty database")

	// ErrSourceRequired reports a missing source DSN (never falls back to
	// the target DSN).
	ErrSourceRequired = errors.New(
		"no source database configured (MAGNETICO_MIGRATION_SOURCE_DSN or config migrateSourceDatabaseURL); " +
			"the source never falls back to the target")
)

// Input is everything preflight needs from the caller.
type Input struct {
	// DSN is the target DSN.
	DSN string
	// SourceIdentity is the source's server identity (pgidentity.Read).
	SourceIdentity pgidentity.Identity
	// SourceFingerprint is the freshly read source fingerprint.
	SourceFingerprint original.Fingerprint
	// DeferIndexes is the requested --defer-secondary-indexes flag.
	DeferIndexes bool
	// SourceAdapter and ContractVersion must match the stored run on resume.
	SourceAdapter   string
	ContractVersion int
	CodecProfile    string
	// Options are the tunables recorded with the run.
	Options state.Options
}

// Result is the preflight outcome: a verified, lock-held target ready for
// import (Fresh) or resume (Existing).
type Result struct {
	// Progress is the initialized (fresh) or loaded (resume) run state.
	Progress *state.Progress
	// Fresh reports that a new run state was created.
	Fresh bool
	// Lock is the run-scoped advisory lock; the caller holds it for the
	// migration's lifetime and must Release it.
	Lock *state.RunLock
	// Identity is the target's server identity (for the final report).
	Identity pgidentity.Identity
}

// Preflight verifies the target and either initializes a fresh run state or
// reconciles a resume. It acquires the run-scoped advisory lock for the
// caller. Failures are typed and secret-free.
func Preflight(ctx context.Context, in Input) (*Result, error) {
	pool, err := OpenPool(ctx, in.DSN)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	identity, err := pgidentity.Read(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTargetIdentityUnknown, err)
	}
	if !identity.Known {
		return nil, ErrTargetIdentityUnknown
	}
	same, ok := pgidentity.SameDatabase(in.SourceIdentity, identity)
	if !ok {
		return nil, fmt.Errorf("%w (source identity unreadable)", ErrTargetIdentityUnknown)
	}
	if same {
		return nil, ErrSameDatabase
	}

	// The lock excludes concurrent migrators from here on.
	lock, err := state.AcquireRunLock(ctx, in.DSN)
	if err != nil {
		return nil, err
	}

	result, err := preflightLocked(ctx, pool, in)
	if err != nil {
		lock.Release(context.WithoutCancel(ctx))
		return nil, err
	}
	result.Lock = lock
	result.Identity = identity
	return result, nil
}

func preflightLocked(ctx context.Context, pool *pgxpool.Pool, in Input) (*Result, error) {
	// Bring the operational schema to the current version before touching
	// run state: 04A (v1) databases upgrade transparently, fresh targets
	// pass through (InitRun creates v2), and anything unexpected refuses
	// before any DDL runs.
	if err := state.EnsureOperationalSchema(ctx, pool); err != nil {
		return nil, err
	}
	existing, err := state.Load(ctx, pool)
	switch {
	case errors.Is(err, state.ErrNoRunState):
		return freshRun(ctx, pool, in)
	case err != nil:
		return nil, err
	}

	// Resume path: prove the stored state belongs to this source and this
	// configuration before anything else.
	if existing.SourceAdapter != in.SourceAdapter {
		return nil, &state.StateError{Err: state.ErrRunStateCorrupt, Details: []string{
			fmt.Sprintf("stored source adapter %q != requested %q", existing.SourceAdapter, in.SourceAdapter)}}
	}
	if existing.ContractVersion != in.ContractVersion {
		return nil, &state.StateError{Err: state.ErrRunStateCorrupt, Details: []string{
			fmt.Sprintf("stored contract version %d != requested %d",
				existing.ContractVersion, in.ContractVersion)}}
	}
	if existing.DeferIndexes != in.DeferIndexes {
		return nil, &state.StateError{Err: state.ErrRunStateCorrupt, Details: []string{
			"the deferred-index decision is immutable for a run; stored defer_indexes=" +
				fmt.Sprintf("%v", existing.DeferIndexes)}}
	}
	var storedFP original.Fingerprint
	if err := json.Unmarshal(existing.FingerprintRaw, &storedFP); err != nil {
		return nil, &state.StateError{Err: state.ErrRunStateCorrupt,
			Details: []string{"stored source fingerprint is unreadable"}}
	}
	if !storedFP.Equivalent(in.SourceFingerprint) {
		return nil, &state.StateError{Err: state.ErrRunStateCorrupt, Details: []string{
			"stored source fingerprint does not match the current source " +
				"(postmaster start time, database, counts or anchors differ)"}}
	}

	// Phase-aware schema verification: an authorized deferred run expects
	// secondary indexes absent while importing; partial progress is valid
	// only while building indexes. Everything else stays strict.
	if err := VerifySchema(ctx, in.DSN, verifyInput{
		deferRequested:  existing.DeferIndexes,
		indexesDeferred: existing.IndexesDeferred,
		phase:           existing.Phase,
	}); err != nil {
		return nil, err
	}

	// Every resume reconciles the target against the persisted state —
	// including a completed run's idempotent re-invocation. The completion
	// phase is trust in the recorded counts, not an exemption: silent
	// divergence after completion (a deleted torrent or blob row, a tampered
	// error registry) must refuse as corrupt state, never report success.
	facts, err := TargetFacts(ctx, pool, existing.LastProcessedID, existing.RunID)
	if err != nil {
		return nil, err
	}
	if err := state.Reconcile(existing, facts); err != nil {
		return nil, err
	}
	return &Result{Progress: existing, Fresh: false}, nil
}

// freshRun verifies a fresh target (strict production schema state, empty
// data, no foreign bookkeeping) and initializes the run state under the lock.
func freshRun(ctx context.Context, pool *pgxpool.Pool, in Input) (*Result, error) {
	// Strict production verification: exactly the schema schema-init
	// creates, including all three secondary indexes.
	store, err := persistence.MakeDatabase(ctx, in.DSN)
	if err != nil {
		return nil, err
	}
	_ = store.Close()

	var torrents, blobs int64
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT COUNT(*) FROM magneticod.torrents),
		        (SELECT COUNT(*) FROM magneticod.torrent_file_blobs)`).Scan(&torrents, &blobs); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTargetNotEmpty, err)
	}
	if torrents != 0 || blobs != 0 {
		return nil, fmt.Errorf("%w (torrents=%d blobs=%d)", ErrTargetNotEmpty, torrents, blobs)
	}

	runID, err := state.NewRunID()
	if err != nil {
		return nil, err
	}
	fpRaw, err := json.Marshal(in.SourceFingerprint)
	if err != nil {
		return nil, err
	}
	progress, err := state.InitRun(ctx, pool, state.InitInput{
		RunID:           runID,
		FingerprintRaw:  fpRaw,
		SourceAdapter:   in.SourceAdapter,
		ContractVersion: in.ContractVersion,
		CodecProfile:    in.CodecProfile,
		ExpectedTotal:   in.SourceFingerprint.Torrents,
		SourceMinID:     in.SourceFingerprint.MinID,
		SourceMaxID:     in.SourceFingerprint.MaxID,
		SourceFileRows:  in.SourceFingerprint.FileRows,
		DeferIndexes:    in.DeferIndexes,
		Options:         in.Options,
	})
	if err != nil {
		return nil, err
	}
	return &Result{Progress: progress, Fresh: true}, nil
}

// OpenPool connects the target with a small bounded pool. Exported for the
// migration runner, which drives commits and incident recording on its own
// pool after preflight.
func OpenPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	scheme := ""
	if parsed, err := url.Parse(dsn); err == nil {
		scheme = parsed.Scheme
	}
	switch scheme {
	case "postgres", "postgresql":
	default:
		return nil, fmt.Errorf("%w: %w", persistence.ErrStorageNotImplemented,
			fmt.Errorf("requested scheme %q", scheme))
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("target connect: %w", err)
	}
	return pool, nil
}
