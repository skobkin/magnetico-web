package state

// Forensic read seam integration tests (04C): read-only operational-schema
// inspection (no DDL, snapshot-proven), the raw progress read, the
// full-history keyset reads, the record-selection UNION and the teardown
// primitive's invariants.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

func forensicsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
}

func insertAttempt(t *testing.T, pool *pgxpool.Pool, a RetryAttempt) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return InsertRetryAttempt(context.Background(), tx, a)
	}); err != nil {
		t.Fatalf("InsertRetryAttempt: %v", err)
	}
}

func mustClass(t *testing.T, pool *pgxpool.Pool) SchemaInspection {
	t.Helper()
	insp, err := InspectOperationalSchema(context.Background(), pool)
	if err != nil {
		t.Fatalf("InspectOperationalSchema() error = %v", err)
	}
	return insp
}

func snapshotIdentical(t *testing.T, pool *pgxpool.Pool, where string) {
	t.Helper()
	before := catalogSnapshot(t, pool)
	_ = mustClass(t, pool) // the inspection under proof
	after := catalogSnapshot(t, pool)
	if len(before) != len(after) {
		t.Fatalf("%s: inspection changed the catalog (%d -> %d lines)", where, len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("%s: inspection mutated the catalog at line %d: %q -> %q", where, i, before[i], after[i])
		}
	}
}

func TestInspectOperationalSchemaClasses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		if insp := mustClass(t, pool); insp.Class != SchemaAbsent {
			t.Fatalf("class = %v, want absent", insp.Class)
		}
	})

	t.Run("exact v1 without upgrade or marker", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		seedV1(t, pool)
		snapshotIdentical(t, pool, "v1")
		if insp := mustClass(t, pool); insp.Class != SchemaV1 {
			t.Fatalf("class = %v, want v1", insp.Class)
		}
		// The marker table must NOT have been created (no DDL side effect).
		var marker bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('magnetico_migration.operational_schema') IS NOT NULL`).Scan(&marker); err != nil {
			t.Fatal(err)
		}
		if marker {
			t.Fatal("inspection created the operational_schema marker table on a v1 database")
		}
	})

	t.Run("exact v2 without marker insert", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		seedVerificationBase(t, pool)
		snapshotIdentical(t, pool, "v2")
		if insp := mustClass(t, pool); insp.Class != SchemaV2 {
			t.Fatalf("class = %v, want v2", insp.Class)
		}
		// Definitionally exact v2 with NO marker row is still v2: definitions
		// are the authority; inspection never repairs the marker.
		var markers int64
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.operational_schema`).Scan(&markers); err != nil {
			t.Fatal(err)
		}
		if markers != 0 {
			t.Fatalf("inspection inserted %d marker rows; want none", markers)
		}
	})

	t.Run("foreign table in the schema", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		seedVerificationBase(t, pool)
		if _, err := pool.Exec(ctx, `CREATE TABLE magnetico_migration.sneaky (i int)`); err != nil {
			t.Fatal(err)
		}
		snapshotIdentical(t, pool, "foreign table")
		insp := mustClass(t, pool)
		if insp.Class != SchemaUnrecognized {
			t.Fatalf("class = %v, want unrecognized", insp.Class)
		}
		joined := strings.Join(insp.Details, "; ")
		if !strings.Contains(joined, "magnetico_migration.sneaky") {
			t.Fatalf("details %q do not name the foreign relation", joined)
		}
	})

	t.Run("unexpected column", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		seedVerificationBase(t, pool)
		if _, err := pool.Exec(ctx, `ALTER TABLE magnetico_migration.errors ADD COLUMN extra int`); err != nil {
			t.Fatal(err)
		}
		snapshotIdentical(t, pool, "extra column")
		insp := mustClass(t, pool)
		if insp.Class != SchemaUnrecognized {
			t.Fatalf("class = %v, want unrecognized", insp.Class)
		}
		if !strings.Contains(strings.Join(insp.Details, "; "), "unexpected column") {
			t.Fatalf("details %q do not name the unexpected column", strings.Join(insp.Details, "; "))
		}
	})

	t.Run("view in the schema", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		seedVerificationBase(t, pool)
		if _, err := pool.Exec(ctx, `CREATE VIEW magnetico_migration.peek AS SELECT 1`); err != nil {
			t.Fatal(err)
		}
		snapshotIdentical(t, pool, "view")
		insp := mustClass(t, pool)
		if insp.Class != SchemaUnrecognized {
			t.Fatalf("class = %v, want unrecognized", insp.Class)
		}
		if !strings.Contains(strings.Join(insp.Details, "; "), "not a table") {
			t.Fatalf("details %q do not refuse the view", strings.Join(insp.Details, "; "))
		}
	})
}

func TestInspectOperationalSchemaOnTx(t *testing.T) {
	t.Parallel()
	pool := forensicsPool(t)
	seedVerificationBase(t, pool)
	before := catalogSnapshot(t, pool)
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		insp, err := InspectOperationalSchema(context.Background(), tx)
		if err != nil {
			return err
		}
		if insp.Class != SchemaV2 {
			t.Fatalf("in-tx class = %v, want v2", insp.Class)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("in-tx inspection: %v", err)
	}
	after := catalogSnapshot(t, pool)
	if len(before) != len(after) {
		t.Fatalf("in-tx inspection changed the catalog (%d -> %d lines)", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("in-tx inspection mutated the catalog at line %d", i)
		}
	}
}

func TestReadProgressRaw(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("v1 projection with zero revision", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		runID := seedV1(t, pool)
		p, err := ReadProgressRaw(ctx, pool, SchemaV1)
		if err != nil {
			t.Fatalf("ReadProgressRaw(v1) error = %v", err)
		}
		if p.RunID != runID || p.DataRevision != 0 || p.ErrorCount != 2 {
			t.Fatalf("raw progress = run %s revision %d errors %d", p.RunID, p.DataRevision, p.ErrorCount)
		}
		if p.Phase != PhaseCompleteWithErrors {
			t.Fatalf("phase = %v", p.Phase)
		}
	})

	t.Run("v2 projection", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		runID := seedVerificationBase(t, pool)
		p, err := ReadProgressRaw(ctx, pool, SchemaV2)
		if err != nil {
			t.Fatalf("ReadProgressRaw(v2) error = %v", err)
		}
		if p.RunID != runID || p.DataRevision != 0 {
			t.Fatalf("raw progress = run %s revision %d", p.RunID, p.DataRevision)
		}
	})

	t.Run("inconsistent counters readable", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		seedVerificationBase(t, pool)
		if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.progress
            SET scanned_count = 99, success_count = 1, error_count = 2`); err != nil {
			t.Fatal(err)
		}
		// Load refuses (the invariant holder), the raw read reports verbatim.
		if _, err := Load(ctx, pool); !errors.Is(err, ErrRunStateCorrupt) {
			t.Fatalf("Load() error = %v, want ErrRunStateCorrupt", err)
		}
		p, err := ReadProgressRaw(ctx, pool, SchemaV2)
		if err != nil {
			t.Fatalf("ReadProgressRaw error = %v", err)
		}
		if p.ScannedCount != 99 || p.SuccessCount != 1 || p.ErrorCount != 2 {
			t.Fatalf("raw counters = %d/%d/%d, want 99/1/2", p.ScannedCount, p.SuccessCount, p.ErrorCount)
		}
	})

	t.Run("unknown phase exported verbatim", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		seedVerificationBase(t, pool)
		if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.progress SET phase = 'half_done_weird'`); err != nil {
			t.Fatal(err)
		}
		p, err := ReadProgressRaw(ctx, pool, SchemaV2)
		if err != nil {
			t.Fatalf("ReadProgressRaw error = %v", err)
		}
		if p.Phase != Phase("half_done_weird") {
			t.Fatalf("phase = %q, want the recorded string verbatim", p.Phase)
		}
	})

	t.Run("zero rows is ErrNoRunState", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		for _, stmt := range append(append([]string{}, migrationSchemaDDL...), operationalSchemaV2DDL...) {
			if _, err := pool.Exec(ctx, stmt); err != nil {
				t.Fatalf("v2 DDL: %v", err)
			}
		}
		if _, err := ReadProgressRaw(ctx, pool, SchemaV2); !errors.Is(err, ErrNoRunState) {
			t.Fatalf("error = %v, want ErrNoRunState", err)
		}
	})

	t.Run("two rows is corrupt", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		seedVerificationBase(t, pool)
		if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.progress (
            run_id, phase, source_fingerprint, source_adapter, contract_version, codec_profile,
            expected_total, source_min_id, source_max_id, source_file_rows, last_processed_id,
            scanned_count, success_count, error_count, defer_indexes, options)
            VALUES ('44444444-4444-4444-8444-444444444444', 'importing', '{}', 'original-v0.12.0',
                    1, 'compact', 0, 1, 0, 0, 0, 0, 0, 0, false, '{}')`); err != nil {
			t.Fatal(err)
		}
		_, err := ReadProgressRaw(ctx, pool, SchemaV2)
		var stateErr *StateError
		if !errors.As(err, &stateErr) || !strings.Contains(err.Error(), "rows, want 1") {
			t.Fatalf("error = %v, want a two-row corruption refusal", err)
		}
	})
}

func TestListErrorRowsKeyset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := forensicsPool(t)
	runID := seedV1(t, pool)
	// seedV1 marks row 3 resolved without a timestamp; set one so the
	// nullable timestamptz scan is proven end to end.
	if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.errors SET resolved_at = now() WHERE id = 3`); err != nil {
		t.Fatal(err)
	}

	// Walk the full 5-row history in pages of 2 (2+2+1).
	var all []ErrorRow
	after := int64(0)
	for page := 0; ; page++ {
		rows, err := ListErrorRows(ctx, pool, runID, after, 2)
		if err != nil {
			t.Fatalf("ListErrorRows(after=%d) error = %v", after, err)
		}
		if len(rows) == 0 {
			break
		}
		if page > 5 {
			t.Fatal("keyset walk did not terminate")
		}
		for _, r := range rows {
			if r.ID <= after {
				t.Fatalf("row %d not ascending past %d", r.ID, after)
			}
			all = append(all, r)
			after = r.ID
		}
	}
	if len(all) != 5 {
		t.Fatalf("covered %d rows, want all 5 (resolved and unresolved)", len(all))
	}

	byID := map[int64]ErrorRow{}
	for _, r := range all {
		byID[r.ID] = r
	}
	// seedV1 inserts in order, so identity ids 1..5 map to source 2, 3, 4
	// (record rows; source 4 resolved) and two run-scope rows.
	r1 := byID[1]
	if r1.Scope != ScopeRecord || r1.SourceID == nil || *r1.SourceID != 2 || r1.Attempts != 1 || r1.Resolved || r1.Code != "invalid_metadata" {
		t.Fatalf("row 1 = %+v", r1)
	}
	r2 := byID[2]
	if r2.Scope != ScopeRecord || r2.SourceID == nil || *r2.SourceID != 3 || r2.Attempts != 2 || r2.Resolved || r2.Code != "invalid_framing" {
		t.Fatalf("row 2 = %+v", r2)
	}
	r3 := byID[3]
	if !r3.Resolved || r3.ResolvedAt == nil || r3.Category != CategoryOperational {
		t.Fatalf("row 3 = %+v (resolved flag/timestamp lost)", r3)
	}
	r4 := byID[4]
	if r4.Scope != ScopeRun || r4.SourceID != nil || r4.Category != CategoryIntegrity {
		t.Fatalf("row 4 = %+v (run-scope row carries no source ID)", r4)
	}
	r5 := byID[5]
	if r5.Scope != ScopeRun || r5.SourceID != nil || r5.Category != CategoryOperational {
		t.Fatalf("row 5 = %+v", r5)
	}

	if _, err := ListErrorRows(ctx, pool, runID, 0, 0); err == nil {
		t.Fatal("zero page limit accepted")
	}
}

func TestListVerificationRunsAndMaxSeq(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := forensicsPool(t)
	runID := seedVerificationBase(t, pool)

	decode, err := CreateVerification(ctx, pool, CreateVerificationInput{RunID: runID, Kind: VerificationKindDecode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FinishVerification(ctx, pool, FinishVerificationInput{VerificationID: decode.VerificationID, Pass: false}); err != nil {
		t.Fatal(err)
	}
	content, err := CreateVerification(ctx, pool, CreateVerificationInput{RunID: runID, Kind: VerificationKindContent})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FinishVerification(ctx, pool, FinishVerificationInput{VerificationID: content.VerificationID, Pass: true}); err != nil {
		t.Fatal(err)
	}
	// A newer decode run must move the decode kind's max seq.
	newer, err := CreateVerification(ctx, pool, CreateVerificationInput{RunID: runID, Kind: VerificationKindDecode})
	if err != nil {
		t.Fatal(err)
	}

	runs, err := ListVerificationRuns(ctx, pool, runID, 0, 10)
	if err != nil {
		t.Fatalf("ListVerificationRuns error = %v", err)
	}
	if len(runs) != 3 || runs[0].Seq >= runs[1].Seq || runs[1].Seq >= runs[2].Seq {
		t.Fatalf("runs = %d entries, not strictly ascending by seq", len(runs))
	}
	// Keyset: everything after the first page boundary.
	tail, err := ListVerificationRuns(ctx, pool, runID, runs[0].Seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 {
		t.Fatalf("keyset tail = %d entries, want 2", len(tail))
	}

	maxSeq, err := MaxVerificationSeq(ctx, pool, runID)
	if err != nil {
		t.Fatal(err)
	}
	if maxSeq[VerificationKindDecode] != newer.Seq || maxSeq[VerificationKindContent] != content.Seq {
		t.Fatalf("max seq = %v", maxSeq)
	}
}

func TestListVerificationIssuesAfterID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := forensicsPool(t)
	runID := seedVerificationBase(t, pool)

	run, err := CreateVerification(ctx, pool, CreateVerificationInput{RunID: runID, Kind: VerificationKindDecode})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID: run.VerificationID,
		Issues: []VerificationIssue{
			{SourceID: i64p(7), InfoHash: []byte("\x01\x02"), Code: "invalid_metadata", Stage: "decode_verify", Severity: IssueMismatch},
			{Code: "orphan_target_blobs", Stage: "extras", Severity: IssueCoverage, Details: map[string]any{"count": 3}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("page commit = %v, %v", ok, err)
	}
	if _, err := FinishVerification(ctx, pool, FinishVerificationInput{VerificationID: run.VerificationID, Pass: true}); err != nil {
		t.Fatal(err)
	}

	issues, err := ListVerificationIssuesAfter(ctx, pool, 0, 10)
	if err != nil {
		t.Fatalf("ListVerificationIssuesAfter error = %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("issues = %d, want 2", len(issues))
	}
	if issues[0].ID >= issues[1].ID || issues[0].VerificationID != run.VerificationID {
		t.Fatalf("issues not identity-ordered/linked: %+v", issues)
	}
	if issues[0].SourceID == nil || *issues[0].SourceID != 7 || issues[0].Severity != IssueMismatch {
		t.Fatalf("issue 0 = %+v", issues[0])
	}
	if issues[1].SourceID != nil || issues[1].Severity != IssueCoverage {
		t.Fatalf("issue 1 = %+v (run-scope finding lost its identity)", issues[1])
	}

	tail, err := ListVerificationIssuesAfter(ctx, pool, issues[0].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].ID != issues[1].ID {
		t.Fatalf("keyset tail = %+v", tail)
	}
}

func TestListRetryRunsCompositeCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := forensicsPool(t)
	runID := seedVerificationBase(t, pool)

	first, err := CreateRetryRun(ctx, pool, CreateRetryRunInput{RunID: runID, StartDataRevision: 0, UnresolvedAtStart: 2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreateRetryRun(ctx, pool, CreateRetryRunInput{RunID: runID, StartDataRevision: 1, UnresolvedAtStart: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Make the started_at order deterministic regardless of UUID ordering.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.retry_runs SET started_at = $1
        WHERE retry_id = $2::uuid`, base, first.RetryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.retry_runs SET started_at = $1
        WHERE retry_id = $2::uuid`, base.Add(time.Minute), second.RetryID); err != nil {
		t.Fatal(err)
	}

	runs, err := ListRetryRuns(ctx, pool, runID, time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("ListRetryRuns error = %v", err)
	}
	if len(runs) != 2 || runs[0].RetryID != first.RetryID || runs[1].RetryID != second.RetryID {
		t.Fatalf("runs not in (started_at, retry_id) order: %+v", runs)
	}

	tail, err := ListRetryRuns(ctx, pool, runID, runs[0].StartedAt, runs[0].RetryID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].RetryID != second.RetryID {
		t.Fatalf("composite-cursor tail = %+v", tail)
	}
}

func TestListRetryAttemptsAfterID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := forensicsPool(t)
	runID := seedVerificationBase(t, pool)

	pass, err := CreateRetryRun(ctx, pool, CreateRetryRunInput{RunID: runID, StartDataRevision: 0, UnresolvedAtStart: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []RetryAttempt{
		{RetryID: pass.RetryID, RunID: runID, SourceID: 3, Outcome: RetryOutcomeStillInvalid, Stage: "validate", Code: "invalid_framing"},
		{RetryID: pass.RetryID, RunID: runID, SourceID: 2, Outcome: RetryOutcomeFixed, Stage: "write", Code: "ok",
			Details: map[string]any{"blobs": 1}},
	} {
		insertAttempt(t, pool, a)
	}

	attempts, err := ListRetryAttemptsAfter(ctx, pool, 0, 10)
	if err != nil {
		t.Fatalf("ListRetryAttemptsAfter error = %v", err)
	}
	if len(attempts) != 2 || attempts[0].ID >= attempts[1].ID {
		t.Fatalf("attempts = %+v, want two ascending identity rows", attempts)
	}
	if attempts[1].RetryID != pass.RetryID || attempts[1].Outcome != RetryOutcomeFixed {
		t.Fatalf("attempt 1 = %+v", attempts[1])
	}
	tail, err := ListRetryAttemptsAfter(ctx, pool, attempts[0].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].ID != attempts[1].ID {
		t.Fatalf("keyset tail = %+v", tail)
	}
}

func TestSelectedRecordIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := forensicsPool(t)
	runID := seedVerificationBase(t, pool)

	// Registry mentions: errors 10 (unresolved) + 20 (resolved), run-scope NULL;
	// issues 20 (dup) + 30, run-scope NULL; attempts 30 (dup) + 40.
	for _, e := range []struct {
		sourceID *int64
		resolved bool
	}{
		{ptrInt64(10), false}, {ptrInt64(20), true}, {nil, false},
	} {
		var sid any
		if e.sourceID != nil {
			sid = *e.sourceID
		}
		if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.errors
            (run_id, scope, source_id, stage, category, code, resolved)
            VALUES ($1::uuid, 'record', $2, 'validate', 'integrity', 'invalid_metadata', $3)`,
			runID, sid, e.resolved); err != nil {
			t.Fatal(err)
		}
	}
	run, err := CreateVerification(ctx, pool, CreateVerificationInput{RunID: runID, Kind: VerificationKindDecode})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID: run.VerificationID,
		Issues: []VerificationIssue{
			{SourceID: ptrInt64(20), Code: "unresolved_record_error", Stage: "compare", Severity: IssueCoverage},
			{SourceID: ptrInt64(30), Code: "missing_blob", Stage: "decode_verify", Severity: IssueMismatch},
			{Code: "orphan_target_blobs", Stage: "extras", Severity: IssueCoverage},
		},
	}); err != nil || !ok {
		t.Fatalf("page commit = %v, %v", ok, err)
	}
	pass, err := CreateRetryRun(ctx, pool, CreateRetryRunInput{RunID: runID, StartDataRevision: 0, UnresolvedAtStart: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []int64{30, 40} {
		insertAttempt(t, pool, RetryAttempt{
			RetryID: pass.RetryID, RunID: runID, SourceID: sid,
			Outcome: RetryOutcomeFixed, Stage: "write", Code: "ok"})
	}

	// Full walk: distinct, ascending, union of all three sources, no NULLs.
	var got []int64
	after := int64(0)
	for {
		page, err := SelectedRecordIDs(ctx, pool, runID, after, 2)
		if err != nil {
			t.Fatalf("SelectedRecordIDs(after=%d) error = %v", after, err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, page...)
		after = page[len(page)-1]
	}
	want := []int64{10, 20, 30, 40}
	if len(got) != len(want) {
		t.Fatalf("selection = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selection = %v, want %v", got, want)
		}
	}
}

func TestCountRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("v2 counts", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		runID := seedVerificationBase(t, pool)
		if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.errors
            (run_id, scope, source_id, stage, category, code)
            VALUES ($1::uuid, 'record', 5, 'validate', 'integrity', 'invalid_metadata')`, runID); err != nil {
			t.Fatal(err)
		}
		run, err := CreateVerification(ctx, pool, CreateVerificationInput{RunID: runID, Kind: VerificationKindDecode})
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := CommitVerificationPage(ctx, pool, VerificationPageCommit{
			VerificationID: run.VerificationID,
			Issues:         []VerificationIssue{{SourceID: ptrInt64(5), Code: "invalid_metadata", Stage: "decode_verify", Severity: IssueMismatch}},
		}); err != nil || !ok {
			t.Fatalf("page commit = %v, %v", ok, err)
		}
		pass, err := CreateRetryRun(ctx, pool, CreateRetryRunInput{RunID: runID, StartDataRevision: 0, UnresolvedAtStart: 1})
		if err != nil {
			t.Fatal(err)
		}
		insertAttempt(t, pool, RetryAttempt{
			RetryID: pass.RetryID, RunID: runID, SourceID: 5,
			Outcome: RetryOutcomeFixed, Stage: "write", Code: "ok"})
		c, err := CountRegistry(ctx, pool, runID)
		if err != nil {
			t.Fatalf("CountRegistry error = %v", err)
		}
		if c.Errors != 1 || c.VerificationRuns != 1 || c.VerificationIssues != 1 || c.RetryRuns != 1 || c.RetryAttempts != 1 {
			t.Fatalf("counts = %+v", c)
		}
	})

	t.Run("v1 has no 04B tables", func(t *testing.T) {
		t.Parallel()
		pool := forensicsPool(t)
		runID := seedV1(t, pool)
		if _, err := CountRegistry(ctx, pool, runID); err == nil {
			t.Fatal("CountRegistry succeeded on a v1 schema; want an explicit error")
		}
	})
}

func TestDropOperationalSchemaDDLInvariants(t *testing.T) {
	t.Parallel()
	for i, stmt := range dropOperationalSchemaDDL {
		up := strings.ToUpper(stmt)
		if strings.Contains(up, "CASCADE") {
			t.Fatalf("DDL %d uses CASCADE: %q", i, stmt)
		}
		if strings.Contains(up, "IF EXISTS") {
			t.Fatalf("DDL %d uses IF EXISTS: %q", i, stmt)
		}
	}
}

func operationalRelationCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'magnetico_migration' AND c.relkind IN ('r','p','v','m','f','S')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func TestDropOperationalSchemaHappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := forensicsPool(t)
	runID := seedVerificationBase(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.errors
        (run_id, scope, source_id, stage, category, code)
        VALUES ($1::uuid, 'record', 5, 'validate', 'integrity', 'invalid_metadata')`, runID); err != nil {
		t.Fatal(err)
	}
	if operationalRelationCount(t, pool) == 0 {
		t.Fatal("fixture created no operational relations")
	}

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return DropOperationalSchema(ctx, tx)
	})
	if err != nil {
		t.Fatalf("DropOperationalSchema error = %v", err)
	}

	var schemaGone bool
	if err := pool.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'magnetico_migration')`).Scan(&schemaGone); err != nil {
		t.Fatal(err)
	}
	if !schemaGone {
		t.Fatal("magnetico_migration schema still exists after the drop")
	}
	// The column-owned identity sequences dropped with their tables.
	var seqs int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pg_class
        WHERE relkind = 'S' AND relname IN ('errors_id_seq', 'verification_runs_seq',
              'verification_issues_id_seq', 'retry_attempts_id_seq')`).Scan(&seqs); err != nil {
		t.Fatal(err)
	}
	if seqs != 0 {
		t.Fatalf("%d identity sequences survived the drop", seqs)
	}
}

func TestDropOperationalSchemaForeignDependencyRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := forensicsPool(t)
	runID := seedVerificationBase(t, pool)
	if _, err := pool.Exec(ctx, `CREATE VIEW public.dependent_view AS
        SELECT id FROM magnetico_migration.errors`); err != nil {
		t.Fatal(err)
	}
	before := operationalRelationCount(t, pool)

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return DropOperationalSchema(ctx, tx)
	})
	if err == nil {
		t.Fatal("drop succeeded despite a foreign dependent view; want an abort")
	}
	// Whole transaction rolled back: every table is intact.
	after := operationalRelationCount(t, pool)
	if after != before {
		t.Fatalf("relation count %d != %d after the aborted drop; the rollback was partial", after, before)
	}
	var p int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.progress WHERE run_id = $1::uuid`, runID).Scan(&p); err != nil || p != 1 {
		t.Fatalf("progress row lost by the aborted drop (count %d, err %v)", p, err)
	}
}

func TestDropOperationalSchemaRestrictTripwire(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := forensicsPool(t)
	runID := seedVerificationBase(t, pool)
	// A leftover foreign object inside the schema: the tables drop, but the
	// final RESTRICT must fail and roll EVERYTHING back.
	if _, err := pool.Exec(ctx, `CREATE TABLE magnetico_migration.sneaky (i int)`); err != nil {
		t.Fatal(err)
	}
	before := operationalRelationCount(t, pool)

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return DropOperationalSchema(ctx, tx)
	})
	if err == nil {
		t.Fatal("drop succeeded despite a foreign object in the schema; want a RESTRICT abort")
	}
	if after := operationalRelationCount(t, pool); after != before {
		t.Fatalf("relation count %d != %d after the aborted drop", after, before)
	}
	var p int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.progress WHERE run_id = $1::uuid`, runID).Scan(&p); err != nil || p != 1 {
		t.Fatalf("progress row lost by the aborted drop (count %d, err %v)", p, err)
	}
}
