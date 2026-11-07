package state

// Operational-state access: load, initialize, reconcile and advance the
// migration bookkeeping. All statements are schema-qualified; every mutation
// runs inside a caller-provided transaction except InitRun, which owns its
// own (creation must be atomic with the first progress row).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Progress is one row of `magnetico_migration.progress`: the authoritative,
// transactional checkpoint and counters.
type Progress struct {
	RunID           string
	Phase           Phase
	FingerprintRaw  json.RawMessage
	SourceAdapter   string
	ContractVersion int
	CodecProfile    string
	ExpectedTotal   int64
	SourceMinID     int64
	SourceMaxID     int64
	SourceFileRows  int64
	LastProcessedID int64
	ScannedCount    int64
	SuccessCount    int64
	ErrorCount      int64
	RunErrorCount   int64
	BlobCount       int64
	DeferIndexes    bool
	IndexesDeferred bool
	OptionsRaw      json.RawMessage
	StartedAt       time.Time
	UpdatedAt       time.Time
}

// InitInput is everything a fresh run state row needs.
type InitInput struct {
	RunID           string
	FingerprintRaw  json.RawMessage
	SourceAdapter   string
	ContractVersion int
	CodecProfile    string
	ExpectedTotal   int64
	SourceMinID     int64
	SourceMaxID     int64
	SourceFileRows  int64
	DeferIndexes    bool
	Options         Options
}

// migrationSchemaDDL creates the operational schema. Only ever executed on a
// target whose Load classified as fresh, under the run lock.
var migrationSchemaDDL = []string{
	`CREATE SCHEMA IF NOT EXISTS magnetico_migration`,
	`CREATE TABLE IF NOT EXISTS magnetico_migration.progress (
        run_id UUID PRIMARY KEY,
        phase TEXT NOT NULL,
        source_fingerprint JSONB NOT NULL,
        source_adapter TEXT NOT NULL,
        contract_version INTEGER NOT NULL,
        codec_profile TEXT NOT NULL,
        expected_total BIGINT NOT NULL,
        source_min_id BIGINT NOT NULL,
        source_max_id BIGINT NOT NULL,
        source_file_rows BIGINT NOT NULL,
        last_processed_id BIGINT NOT NULL,
        scanned_count BIGINT NOT NULL,
        success_count BIGINT NOT NULL,
        error_count BIGINT NOT NULL,
        run_error_count BIGINT NOT NULL DEFAULT 0,
        blob_count BIGINT NOT NULL DEFAULT 0,
        defer_indexes BOOLEAN NOT NULL,
        indexes_deferred BOOLEAN NOT NULL DEFAULT FALSE,
        options JSONB NOT NULL,
        started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`,
	`CREATE TABLE IF NOT EXISTS magnetico_migration.errors (
        id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        run_id UUID NOT NULL REFERENCES magnetico_migration.progress(run_id),
        scope TEXT NOT NULL CHECK (scope IN ('record','run')),
        source_id BIGINT,
        source_info_hash BYTEA,
        stage TEXT NOT NULL,
        category TEXT NOT NULL,
        code TEXT NOT NULL,
        details JSONB,
        attempts INTEGER NOT NULL DEFAULT 1,
        resolved BOOLEAN NOT NULL DEFAULT FALSE,
        resolved_at TIMESTAMPTZ,
        created_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`,
	`CREATE UNIQUE INDEX IF NOT EXISTS errors_run_record_unique
        ON magnetico_migration.errors (run_id, source_id) WHERE scope = 'record'`,
}

// expectedRunTables is the exact relation inventory of the operational
// schema; anything else is corrupt state.
var expectedRunTables = map[string]bool{
	"progress": true,
	"errors":   true,
}

// Load reads the run state, classifying absent (fresh-eligible), present
// (resume) and corrupt (refuse) targets. Never writes.
func Load(ctx context.Context, pool *pgxpool.Pool) (*Progress, error) {
	var schemaExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'magnetico_migration')`,
	).Scan(&schemaExists); err != nil {
		return nil, err
	}
	if !schemaExists {
		return nil, ErrNoRunState
	}

	rows, err := pool.Query(ctx, `SELECT c.relname, c.relkind
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'magnetico_migration' AND c.relkind IN ('r','p','v','m','f')
        ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	inventory := map[string]string{}
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		inventory[name] = kind
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var details []string
	for name, kind := range inventory {
		if !expectedRunTables[name] || kind != "r" {
			details = append(details, fmt.Sprintf("unexpected relation magnetico_migration.%s (%s)", name, kind))
		}
	}
	for name := range expectedRunTables {
		if inventory[name] == "" {
			details = append(details, "missing table magnetico_migration."+name)
		}
	}
	if len(details) > 0 {
		return nil, &StateError{Err: ErrRunStateCorrupt, Details: details}
	}

	var count int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM magnetico_migration.progress`).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		// A schema with no progress row is an uninitialized (fresh-eligible)
		// target: cleanup may have removed the rows but kept the tables.
		return nil, ErrNoRunState
	}
	if count > 1 {
		return nil, &StateError{Err: ErrRunStateCorrupt,
			Details: []string{fmt.Sprintf("magnetico_migration.progress has %d rows, want 1", count)}}
	}

	p := &Progress{}
	var phase string
	err = pool.QueryRow(ctx, `SELECT run_id::text, phase, source_fingerprint, source_adapter,
            contract_version, codec_profile, expected_total, source_min_id, source_max_id,
            source_file_rows, last_processed_id, scanned_count, success_count, error_count,
            run_error_count, blob_count, defer_indexes, indexes_deferred, options,
            started_at, updated_at
        FROM magnetico_migration.progress`).Scan(
		&p.RunID, &phase, &p.FingerprintRaw, &p.SourceAdapter,
		&p.ContractVersion, &p.CodecProfile, &p.ExpectedTotal, &p.SourceMinID, &p.SourceMaxID,
		&p.SourceFileRows, &p.LastProcessedID, &p.ScannedCount, &p.SuccessCount, &p.ErrorCount,
		&p.RunErrorCount, &p.BlobCount, &p.DeferIndexes, &p.IndexesDeferred, &p.OptionsRaw,
		&p.StartedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	p.Phase = Phase(phase)
	if !p.Phase.Valid() {
		return nil, &StateError{Err: ErrRunStateCorrupt,
			Details: []string{"unknown phase " + phase}}
	}
	if p.LastProcessedID < p.SourceMinID-1 || (p.SourceMaxID > 0 && p.LastProcessedID > p.SourceMaxID) {
		return nil, &StateError{Err: ErrRunStateCorrupt, Details: []string{
			fmt.Sprintf("checkpoint %d outside the source ID range [%d,%d]",
				p.LastProcessedID, p.SourceMinID, p.SourceMaxID)}}
	}
	if p.ScannedCount != p.SuccessCount+p.ErrorCount {
		return nil, &StateError{Err: ErrRunStateCorrupt, Details: []string{
			fmt.Sprintf("scanned %d != success %d + error %d",
				p.ScannedCount, p.SuccessCount, p.ErrorCount)}}
	}
	return p, nil
}

// InitRun creates the operational schema and the first progress row in one
// transaction on a fresh target. Caller must hold the run lock; the guarded
// insert makes double-init impossible regardless.
func InitRun(ctx context.Context, pool *pgxpool.Pool, in InitInput) (*Progress, error) {
	opts, err := json.Marshal(in.Options)
	if err != nil {
		return nil, err
	}
	var runID string
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, stmt := range migrationSchemaDDL {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		err := tx.QueryRow(ctx, `INSERT INTO magnetico_migration.progress (
                run_id, phase, source_fingerprint, source_adapter, contract_version,
                codec_profile, expected_total, source_min_id, source_max_id, source_file_rows,
                last_processed_id, scanned_count, success_count, error_count,
                defer_indexes, options)
            SELECT $1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0, 0, 0, 0, $11, $12
            WHERE NOT EXISTS (SELECT 1 FROM magnetico_migration.progress)
            RETURNING run_id::text`,
			in.RunID, string(PhaseImporting), string(in.FingerprintRaw), in.SourceAdapter,
			in.ContractVersion, in.CodecProfile, in.ExpectedTotal, in.SourceMinID,
			in.SourceMaxID, in.SourceFileRows, in.DeferIndexes, string(opts),
		).Scan(&runID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &StateError{Err: ErrRunStateCorrupt,
				Details: []string{"run state appeared while initializing"}}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	p := &Progress{
		RunID:           runID,
		Phase:           PhaseImporting,
		FingerprintRaw:  in.FingerprintRaw,
		SourceAdapter:   in.SourceAdapter,
		ContractVersion: in.ContractVersion,
		CodecProfile:    in.CodecProfile,
		ExpectedTotal:   in.ExpectedTotal,
		SourceMinID:     in.SourceMinID,
		SourceMaxID:     in.SourceMaxID,
		SourceFileRows:  in.SourceFileRows,
		DeferIndexes:    in.DeferIndexes,
		OptionsRaw:      opts,
	}
	return p, nil
}

// Facts is what the target writer knows about the magneticod data; the
// reconcile invariants compare it with the persisted progress.
type Facts struct {
	Torrents             int64
	Blobs                int64
	BloblessMultiFile    int64
	RowsBeyondCheckpoint int64
	RecordErrors         int64
	UnresolvedIntegrity  int64
}

// Reconcile checks that target data and persisted state agree. Any mismatch
// is corrupt state, never auto-repaired.
func Reconcile(p *Progress, f Facts) error {
	var details []string
	check := func(cond bool, format string, args ...any) {
		if !cond {
			details = append(details, fmt.Sprintf(format, args...))
		}
	}
	check(f.Torrents == p.SuccessCount,
		"target torrents %d != success_count %d", f.Torrents, p.SuccessCount)
	check(f.Blobs == p.BlobCount,
		"target blobs %d != blob_count %d", f.Blobs, p.BlobCount)
	check(f.BloblessMultiFile == 0,
		"%d multi-file torrents lack their blob", f.BloblessMultiFile)
	check(f.RowsBeyondCheckpoint == 0,
		"%d torrent rows exist beyond checkpoint %d", f.RowsBeyondCheckpoint, p.LastProcessedID)
	check(f.RecordErrors == p.ErrorCount,
		"error registry %d != error_count %d", f.RecordErrors, p.ErrorCount)
	check(f.UnresolvedIntegrity == p.RunErrorCount,
		"unresolved integrity incidents %d != run_error_count %d",
		f.UnresolvedIntegrity, p.RunErrorCount)
	if len(details) > 0 {
		return &StateError{Err: ErrRunStateCorrupt, Details: details}
	}
	return nil
}

// AdvanceCheckpoint performs the compare-and-swap checkpoint update inside
// the batch transaction. It returns false (and the caller must roll back)
// when the CAS does not affect exactly one row: a concurrent writer moved
// the checkpoint, or the phase changed underneath us.
func AdvanceCheckpoint(ctx context.Context, tx pgx.Tx, in AdvanceInput) (bool, error) {
	if err := validateAdvance(in); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE magnetico_migration.progress SET
            last_processed_id = $3,
            phase = $4,
            scanned_count = scanned_count + $5,
            success_count = success_count + $6,
            error_count = error_count + $7,
            run_error_count = run_error_count + $8,
            blob_count = blob_count + $9,
            updated_at = now()
        WHERE run_id = $1::uuid AND last_processed_id = $2 AND phase = $10`,
		in.RunID, in.ExpectedLastID, in.NewLastID, string(in.NewPhase),
		in.ScannedDelta, in.SuccessDelta, in.ErrorDelta, in.RunErrorDelta, in.BlobDelta,
		string(in.ExpectedPhase),
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
