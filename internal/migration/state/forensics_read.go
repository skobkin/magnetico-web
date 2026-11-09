package state

// Forensic read seam (task 04C): read-only operational-schema inspection and
// full-history reads for `migrate errors export` and the cleanup gates.
//
// Everything here is strictly read-only — inspection NEVER issues DDL, never
// inserts the operational marker, and never upgrades a v1 schema (that is
// EnsureOperationalSchema's job, which export must not call). Every read is
// keyset-bounded so the exporter streams without scaling memory.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Queryer is the read interface satisfied by *pgxpool.Pool, *pgx.Conn and
// pgx.Tx. It lets the cleanup gates run the same reads inside the drop
// transaction that validated them outside it.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// SchemaClass is the read-only classification of magnetico_migration.
type SchemaClass int

const (
	// SchemaAbsent: no magnetico_migration namespace at all (a fresh target,
	// or a target whose operational state a completed cleanup removed).
	SchemaAbsent SchemaClass = iota
	// SchemaV1: exact 04A definitions (progress without data_revision + errors).
	SchemaV1
	// SchemaV2: exact 04B definitions (all seven tables).
	SchemaV2
	// SchemaUnrecognized: present, but not exactly v1 or exactly v2 —
	// malformed, partial, or carrying foreign relations.
	SchemaUnrecognized
)

// SchemaInspection is the classification result. Details carries the sorted
// mismatch catalog renderings for SchemaUnrecognized (same renderer as
// EnsureOperationalSchema's refusal).
type SchemaInspection struct {
	Class   SchemaClass
	Details []string
}

// String names the class for logs and previews.
func (c SchemaClass) String() string {
	switch c {
	case SchemaAbsent:
		return "absent"
	case SchemaV1:
		return "v1"
	case SchemaV2:
		return "v2"
	default:
		return "unrecognized"
	}
}

// InspectOperationalSchema classifies the operational schema WITHOUT ever
// issuing DDL: no upgrade, no marker insert, no repair. Definitions are the
// authority — the operational_schema marker is never consulted (a
// definitionally exact schema without a marker is still exact; the marker
// alone proves nothing). Caller should hold the run lock, as with every
// other schema inspection, so classification is not racy against a
// concurrent migrator.
func InspectOperationalSchema(ctx context.Context, q Queryer) (SchemaInspection, error) {
	var schemaExists bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'magnetico_migration')`,
	).Scan(&schemaExists); err != nil {
		return SchemaInspection{}, err
	}
	if !schemaExists {
		return SchemaInspection{Class: SchemaAbsent}, nil
	}

	inventory, details := readInventory(ctx, q)
	if len(details) > 0 {
		return SchemaInspection{Class: SchemaUnrecognized, Details: details}, nil
	}

	switch {
	case sameNames(inventory, expectedSchemaV1):
		if bad := validateOperationalSchema(ctx, q, expectedSchemaV1); len(bad) > 0 {
			return SchemaInspection{Class: SchemaUnrecognized, Details: appendDetails(
				[]string{"existing operational schema is not exact v1"}, bad)}, nil
		}
		return SchemaInspection{Class: SchemaV1}, nil
	case sameNames(inventory, expectedSchemaV2):
		if bad := validateOperationalSchema(ctx, q, expectedSchemaV2); len(bad) > 0 {
			return SchemaInspection{Class: SchemaUnrecognized, Details: appendDetails(
				[]string{"existing operational schema is not exact v2"}, bad)}, nil
		}
		return SchemaInspection{Class: SchemaV2}, nil
	default:
		details = append([]string{
			"magnetico_migration contains an unrecognized relation inventory"}, details...)
		for _, name := range sortedNames(inventory) {
			if _, known := expectedSchemaV2[name]; !known {
				details = append(details, fmt.Sprintf("unexpected relation magnetico_migration.%s (%s)",
					name, relKindName(inventory[name])))
			}
		}
		for name := range expectedSchemaV2 {
			if inventory[name] == "" {
				details = append(details, "missing table magnetico_migration."+name)
			}
		}
		return SchemaInspection{Class: SchemaUnrecognized, Details: details}, nil
	}
}

// ReadProgressRaw reads the single progress row WITHOUT the Load invariants
// and without any inventory classification — forensic export reports
// counters exactly as recorded (marking the run not_reconciled) instead of
// refusing inconsistent state. class selects the projection: a v1 schema has
// no data_revision column, which is reported as 0. Zero rows →
// ErrNoRunState; more than one → corrupt.
func ReadProgressRaw(ctx context.Context, q Queryer, class SchemaClass) (*Progress, error) {
	var count int64
	if err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM magnetico_migration.progress`).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrNoRunState
	}
	if count > 1 {
		return nil, &StateError{Err: ErrRunStateCorrupt,
			Details: []string{fmt.Sprintf("magnetico_migration.progress has %d rows, want 1", count)}}
	}

	p := &Progress{}
	var phase string
	switch class {
	case SchemaV1:
		err := q.QueryRow(ctx, `SELECT run_id::text, phase, source_fingerprint, source_adapter,
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
		p.DataRevision = 0 // v1 predates the revision counter
	case SchemaV2:
		err := q.QueryRow(ctx, `SELECT run_id::text, phase, source_fingerprint, source_adapter,
	            contract_version, codec_profile, expected_total, source_min_id, source_max_id,
	            source_file_rows, last_processed_id, scanned_count, success_count, error_count,
	            run_error_count, blob_count, data_revision, defer_indexes, indexes_deferred, options,
	            started_at, updated_at
	        FROM magnetico_migration.progress`).Scan(
			&p.RunID, &phase, &p.FingerprintRaw, &p.SourceAdapter,
			&p.ContractVersion, &p.CodecProfile, &p.ExpectedTotal, &p.SourceMinID, &p.SourceMaxID,
			&p.SourceFileRows, &p.LastProcessedID, &p.ScannedCount, &p.SuccessCount, &p.ErrorCount,
			&p.RunErrorCount, &p.BlobCount, &p.DataRevision, &p.DeferIndexes, &p.IndexesDeferred, &p.OptionsRaw,
			&p.StartedAt, &p.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("ReadProgressRaw requires a classified schema (v1 or v2), got %v", class)
	}
	p.Phase = Phase(phase) // exported verbatim; validity is NOT checked here
	return p, nil
}

// ErrorRow is one FULL row of `magnetico_migration.errors` — the complete
// history including resolved rows, attempt counts and stored details as
// recorded (raw JSONB; the exporter applies its own redaction).
type ErrorRow struct {
	ID             int64
	Scope          string
	SourceID       *int64
	SourceInfoHash []byte
	Stage          string
	Category       string
	Code           string
	Details        json.RawMessage
	Attempts       int
	Resolved       bool
	ResolvedAt     *time.Time
	CreatedAt      time.Time
}

// ListErrorRows reads one keyset page of the FULL error history of the run
// (ascending by id; resolved and unresolved alike). This is the forensic
// counterpart of UnresolvedRecordIDs, which sees only the unresolved work
// list. Nil Details renders as JSON null.
func ListErrorRows(ctx context.Context, q Queryer, runID string, afterID int64, limit int) ([]ErrorRow, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("error history page limit must be positive, got %d", limit)
	}
	rows, err := q.Query(ctx, `SELECT id, scope, source_id, source_info_hash, stage, category, code,
            details, attempts, resolved, resolved_at, created_at
        FROM magnetico_migration.errors
        WHERE run_id = $1::uuid AND id > $2::bigint
        ORDER BY id
        LIMIT $3`, runID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ErrorRow
	for rows.Next() {
		var r ErrorRow
		var details []byte
		if err := rows.Scan(&r.ID, &r.Scope, &r.SourceID, &r.SourceInfoHash, &r.Stage,
			&r.Category, &r.Code, &details, &r.Attempts, &r.Resolved, &r.ResolvedAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Details = details
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListVerificationRuns reads one keyset page of the run's verification
// history (ascending by seq — every run, terminal or not).
func ListVerificationRuns(ctx context.Context, q Queryer, runID string, afterSeq int64, limit int) ([]*VerificationRun, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("verification history page limit must be positive, got %d", limit)
	}
	rows, err := q.Query(ctx, `SELECT `+verificationProjection+` FROM magnetico_migration.verification_runs
        WHERE run_id = $1::uuid AND seq > $2::bigint
        ORDER BY seq
        LIMIT $3`, runID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*VerificationRun
	for rows.Next() {
		r, err := scanVerificationRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MaxVerificationSeq returns the highest seq per verification kind of the
// run (the authoritative-run markers; at most decode and content).
func MaxVerificationSeq(ctx context.Context, q Queryer, runID string) (map[string]int64, error) {
	rows, err := q.Query(ctx, `SELECT kind, MAX(seq) FROM magnetico_migration.verification_runs
        WHERE run_id = $1::uuid GROUP BY kind`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	maxSeq := map[string]int64{}
	for rows.Next() {
		var kind string
		var seq int64
		if err := rows.Scan(&kind, &seq); err != nil {
			return nil, err
		}
		maxSeq[kind] = seq
	}
	return maxSeq, rows.Err()
}

// VerificationIssueRow is one FULL row of `magnetico_migration.verification_issues`
// with its table-wide identity (id) and run linkage.
type VerificationIssueRow struct {
	ID             int64
	VerificationID string
	SourceID       *int64
	InfoHash       []byte
	Code           string
	Stage          string
	Severity       string
	Details        json.RawMessage
	CreatedAt      time.Time
}

// ListVerificationIssuesAfter reads one keyset page of issues table-wide
// (ascending by id) — the forensic counterpart of the per-run
// ListVerificationIssues, which sees one run and no identity.
func ListVerificationIssuesAfter(ctx context.Context, q Queryer, afterID int64, limit int) ([]VerificationIssueRow, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("issue history page limit must be positive, got %d", limit)
	}
	rows, err := q.Query(ctx, `SELECT id, verification_id::text, source_id, info_hash, code, stage,
            severity, details, created_at
        FROM magnetico_migration.verification_issues
        WHERE id > $1::bigint
        ORDER BY id
        LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VerificationIssueRow
	for rows.Next() {
		var r VerificationIssueRow
		var details []byte
		if err := rows.Scan(&r.ID, &r.VerificationID, &r.SourceID, &r.InfoHash, &r.Code,
			&r.Stage, &r.Severity, &details, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Details = details
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRetryRuns reads one keyset page of the run's retry history (ascending
// by (started_at, retry_id) — retry_runs has no identity column, so the
// composite row-value comparison is the deterministic total order).
func ListRetryRuns(ctx context.Context, q Queryer, runID string, afterStartedAt time.Time, afterRetryID string, limit int) ([]*RetryRun, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("retry history page limit must be positive, got %d", limit)
	}
	if afterRetryID == "" {
		afterRetryID = "00000000-0000-0000-0000-000000000000" // smallest UUID: first page
	}
	rows, err := q.Query(ctx, `SELECT `+retryProjection+` FROM magnetico_migration.retry_runs
        WHERE run_id = $1::uuid AND (started_at, retry_id) > ($2::timestamptz, $3::uuid)
        ORDER BY started_at, retry_id
        LIMIT $4`, runID, afterStartedAt, afterRetryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RetryRun
	for rows.Next() {
		r, err := scanRetryRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RetryAttemptEntry is one FULL row of `magnetico_migration.retry_attempts`
// with its table-wide identity.
type RetryAttemptEntry struct {
	ID             int64
	RetryID        string
	SourceID       int64
	SourceInfoHash []byte
	Outcome        string
	Stage          string
	Code           string
	Details        json.RawMessage
	AttemptedAt    time.Time
}

// ListRetryAttemptsAfter reads one keyset page of attempts table-wide
// (ascending by id — every pass, every outcome).
func ListRetryAttemptsAfter(ctx context.Context, q Queryer, afterID int64, limit int) ([]RetryAttemptEntry, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("attempt history page limit must be positive, got %d", limit)
	}
	rows, err := q.Query(ctx, `SELECT id, retry_id::text, source_id, source_info_hash, outcome,
            stage, code, details, attempted_at
        FROM magnetico_migration.retry_attempts
        WHERE id > $1::bigint
        ORDER BY id
        LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetryAttemptEntry
	for rows.Next() {
		var r RetryAttemptEntry
		var details []byte
		if err := rows.Scan(&r.ID, &r.RetryID, &r.SourceID, &r.SourceInfoHash, &r.Outcome,
			&r.Stage, &r.Code, &details, &r.AttemptedAt); err != nil {
			return nil, err
		}
		r.Details = details
		out = append(out, r)
	}
	return out, rows.Err()
}

// SelectedRecordIDs returns one keyset page of the DISTINCT source IDs
// mentioned by record-scope errors (resolved or not), verification issues
// and retry attempts of the run — the `--include-records` selection set.
// NULL source IDs never qualify (run-scope entries have no record to link).
// One set-oriented UNION query replaces a client-side merge of three
// sorted cursors; the per-branch filters make the page scan bounded.
func SelectedRecordIDs(ctx context.Context, q Queryer, runID string, afterID int64, limit int) ([]int64, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("record selection page limit must be positive, got %d", limit)
	}
	rows, err := q.Query(ctx, `SELECT source_id FROM (
        SELECT source_id FROM magnetico_migration.errors
          WHERE run_id = $1::uuid AND scope = 'record' AND source_id > $2::bigint
        UNION
        SELECT vi.source_id FROM magnetico_migration.verification_issues vi
          JOIN magnetico_migration.verification_runs vr ON vr.verification_id = vi.verification_id
          WHERE vr.run_id = $1::uuid AND vi.source_id > $2::bigint
        UNION
        SELECT ra.source_id FROM magnetico_migration.retry_attempts ra
          JOIN magnetico_migration.retry_runs rr ON rr.retry_id = ra.retry_id
          WHERE rr.run_id = $1::uuid AND ra.source_id > $2::bigint
      ) s
      ORDER BY source_id
      LIMIT $3`, runID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RegistryCounts is the row count of every content table of the run's
// registry (v2 only — a v1 schema has none of the 04B tables).
type RegistryCounts struct {
	Errors             int64
	VerificationRuns   int64
	VerificationIssues int64
	RetryRuns          int64
	RetryAttempts      int64
}

// CountRegistry counts the registry rows of the run in one query — used by
// the cleanup preview, the in-transaction gate recheck and the
// manifest-vs-registry comparison. Requires the v2 tables.
func CountRegistry(ctx context.Context, q Queryer, runID string) (RegistryCounts, error) {
	var c RegistryCounts
	err := q.QueryRow(ctx, `SELECT
        (SELECT COUNT(*) FROM magnetico_migration.errors WHERE run_id = $1::uuid),
        (SELECT COUNT(*) FROM magnetico_migration.verification_runs WHERE run_id = $1::uuid),
        (SELECT COUNT(*) FROM magnetico_migration.verification_issues vi
          JOIN magnetico_migration.verification_runs vr ON vr.verification_id = vi.verification_id
          WHERE vr.run_id = $1::uuid),
        (SELECT COUNT(*) FROM magnetico_migration.retry_runs WHERE run_id = $1::uuid),
        (SELECT COUNT(*) FROM magnetico_migration.retry_attempts WHERE run_id = $1::uuid)`,
		runID).Scan(&c.Errors, &c.VerificationRuns, &c.VerificationIssues, &c.RetryRuns, &c.RetryAttempts)
	if err != nil {
		return RegistryCounts{}, err
	}
	return c, nil
}

// dropOperationalSchemaDDL is the fixed, identifier-literal teardown of the
// operational schema in FK-safe order (children before parents; the 04B
// hand-off order). No CASCADE: an unexpected dependency must abort, never
// silently destroy the blocking object. No IF EXISTS: a missing expected
// object is tampering/drift and aborts — the validated inventory, not the
// statement list, is the authority. Identity sequences (errors.id,
// verification_runs.seq, verification_issues.id, retry_attempts.id) are
// GENERATED ALWAYS and owned by their columns, so they drop with their
// tables. Executed only inside the caller's validated transaction.
var dropOperationalSchemaDDL = []string{
	`DROP TABLE magnetico_migration.verification_issues`,
	`DROP TABLE magnetico_migration.verification_runs`,
	`DROP TABLE magnetico_migration.retry_attempts`,
	`DROP TABLE magnetico_migration.retry_runs`,
	`DROP TABLE magnetico_migration.errors`,
	`DROP TABLE magnetico_migration.progress`,
	`DROP TABLE magnetico_migration.operational_schema`,
	`DROP SCHEMA magnetico_migration RESTRICT`,
}

// DropOperationalSchema executes the teardown inside the caller's
// transaction. Any error — a privilege failure, an unexpected dependency, a
// missing object — aborts the whole transaction, leaving no half-cleaned
// schema.
func DropOperationalSchema(ctx context.Context, tx pgx.Tx) error {
	for _, stmt := range dropOperationalSchemaDDL {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("cleanup drop step failed (%q): %w", stmt, err)
		}
	}
	return nil
}
