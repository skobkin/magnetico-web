package state

// Verification run store integration tests: adopt-or-new resume semantics,
// atomic page commits with the dual-checkpoint CAS (and rollback on
// failure), terminal-state authority via seq, and issue dedupe.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

func i64p(v int64) *int64 { return &v }

// seedVerificationBase installs the v2 operational schema and one fresh
// progress row (the FK parent for verification/retry state), returning the
// run ID.
func seedVerificationBase(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range append(append([]string{}, migrationSchemaDDL...), operationalSchemaV2DDL...) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("v2 DDL: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.progress (run_id, phase,
        source_fingerprint, source_adapter, contract_version, codec_profile, expected_total,
        source_min_id, source_max_id, source_file_rows, last_processed_id, scanned_count,
        success_count, error_count, defer_indexes, options)
        VALUES ('33333333-3333-4333-8333-333333333333', 'import_complete_with_errors', '{}',
                'original-v0.12.0', 1, 'compact', 0, 1, 0, 0, 0, 0, 0, 0, false, '{}')`); err != nil {
		t.Fatalf("seed progress: %v", err)
	}
	return "33333333-3333-4333-8333-333333333333"
}

func TestVerificationCreateCommitFinishPass(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedVerificationBase(t, pool)
	ctx := context.Background()

	run, err := CreateVerification(ctx, pool, CreateVerificationInput{
		RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 7,
		ExpectedTargetIDs: i64p(100), SettingsRaw: []byte(`{"workers":4}`),
	})
	if err != nil {
		t.Fatalf("CreateVerification() error = %v", err)
	}
	if run.State != VerificationRunning || run.Phase != VerificationPhaseScanning ||
		run.Seq == 0 || run.StartDataRevision != 7 {
		t.Fatalf("fresh run = %+v", run)
	}
	if len(run.VerificationID) != 36 {
		t.Fatalf("verification ID = %q, want a UUID", run.VerificationID)
	}

	id10 := int64(10)
	src5 := int64(5)
	ok, err := CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID: run.VerificationID,
		Issues: []VerificationIssue{{
			SourceID: &src5, Code: "blob_over_limit", Stage: "read",
			Severity: IssueMismatch, Details: map[string]any{"octets": 99}},
		},
		VerifiedDelta: 9, MismatchDelta: 1,
		TargetCheckpoint:    nil,
		NewTargetCheckpoint: &id10,
	})
	if err != nil {
		t.Fatalf("CommitVerificationPage() error = %v", err)
	}
	if !ok {
		t.Fatal("first page commit lost the CAS")
	}

	id20 := int64(20)
	ok, err = CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID:   run.VerificationID,
		VerifiedDelta:    1,
		TargetCheckpoint: &id10, NewTargetCheckpoint: &id20,
	})
	if err != nil {
		t.Fatalf("second CommitVerificationPage() error = %v", err)
	}
	if !ok {
		t.Fatal("second page commit lost the CAS")
	}

	finished, err := FinishVerification(ctx, pool, FinishVerificationInput{
		VerificationID: run.VerificationID, Pass: false,
		ObservedTargetIDs: i64p(20),
	})
	if err != nil {
		t.Fatalf("FinishVerification() error = %v", err)
	}
	if finished.State != VerificationCompletedFail || finished.Phase != VerificationPhaseComplete {
		t.Fatalf("finished run = %s/%s, want completed_fail/complete", finished.State, finished.Phase)
	}
	if finished.CompletedAt == nil {
		t.Fatal("completed_at not set")
	}
	if finished.VerifiedCount != 10 || finished.MismatchCount != 1 || finished.IssueCount != 1 {
		t.Fatalf("counters = verified %d mismatch %d issue %d, want 10/1/1",
			finished.VerifiedCount, finished.MismatchCount, finished.IssueCount)
	}
	if finished.TargetCheckpoint == nil || *finished.TargetCheckpoint != 20 {
		t.Fatalf("target checkpoint = %v, want 20", finished.TargetCheckpoint)
	}
	if finished.SourceCheckpoint != nil {
		t.Fatalf("decode source checkpoint = %v, want NULL", finished.SourceCheckpoint)
	}
	if n := len(finished.SettingsRaw); n == 0 {
		t.Fatal("settings lost")
	}

	issues, err := ListVerificationIssues(ctx, pool, run.VerificationID, 10)
	if err != nil {
		t.Fatalf("ListVerificationIssues() error = %v", err)
	}
	if len(issues) != 1 || issues[0].Code != "blob_over_limit" || issues[0].Severity != IssueMismatch {
		t.Fatalf("issues = %+v", issues)
	}
	if issues[0].SourceID == nil || *issues[0].SourceID != 5 {
		t.Fatalf("issue source ID = %v, want 5", issues[0].SourceID)
	}
}

func TestVerificationPageCASRejectsStaleCheckpoint(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedVerificationBase(t, pool)
	ctx := context.Background()

	run, err := CreateVerification(ctx, pool, CreateVerificationInput{
		RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 0,
	})
	if err != nil {
		t.Fatalf("CreateVerification() error = %v", err)
	}
	id10, id20 := int64(10), int64(20)
	if _, err := CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID:   run.VerificationID,
		TargetCheckpoint: nil, NewTargetCheckpoint: &id10,
	}); err != nil {
		t.Fatalf("first commit error = %v", err)
	}

	// Stale expectation (checkpoint already at 10): CAS must lose and the
	// counters must stay untouched.
	ok, err := CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID: run.VerificationID, VerifiedDelta: 100,
		TargetCheckpoint: nil, NewTargetCheckpoint: &id20,
	})
	if err != nil {
		t.Fatalf("stale commit error = %v", err)
	}
	if ok {
		t.Fatal("stale page commit must lose the CAS")
	}
	got, err := GetVerification(ctx, pool, run.VerificationID)
	if err != nil {
		t.Fatal(err)
	}
	if *got.TargetCheckpoint != 10 || got.VerifiedCount != 0 {
		t.Fatalf("run after stale commit = checkpoint %d verified %d, want 10/0",
			*got.TargetCheckpoint, got.VerifiedCount)
	}

	// Correct expectation: the CAS commits.
	ok, err = CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID: run.VerificationID, VerifiedDelta: 10,
		TargetCheckpoint: &id10, NewTargetCheckpoint: &id20,
	})
	if err != nil || !ok {
		t.Fatalf("correct commit = (%v,%v), want (true,nil)", ok, err)
	}
}

// A stale page (lost checkpoint CAS) carrying issue rows must persist
// NOTHING — previously the issue inserts committed even when the CAS
// matched zero rows, leaving issues without their checkpoint or counters.
func TestVerificationPageStaleCASRollsBackIssues(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedVerificationBase(t, pool)
	ctx := context.Background()

	run, err := CreateVerification(ctx, pool, CreateVerificationInput{
		RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 0,
	})
	if err != nil {
		t.Fatalf("CreateVerification() error = %v", err)
	}
	id10 := int64(10)
	if _, err := CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID:   run.VerificationID,
		TargetCheckpoint: nil, NewTargetCheckpoint: &id10,
	}); err != nil {
		t.Fatalf("first commit error = %v", err)
	}

	// Stale expectation WITH a new issue: the CAS must lose and the issue
	// must not persist.
	src42 := int64(42)
	ok, err := CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID: run.VerificationID,
		Issues: []VerificationIssue{{
			SourceID: &src42, Code: "missing_blob", Stage: "decode", Severity: IssueMismatch,
		}},
		TargetCheckpoint: nil, NewTargetCheckpoint: &id10,
	})
	if err != nil {
		t.Fatalf("stale commit error = %v", err)
	}
	if ok {
		t.Fatal("stale page commit must lose the CAS")
	}
	var issueRows int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.verification_issues
        WHERE verification_id = $1::uuid`, run.VerificationID).Scan(&issueRows); err != nil {
		t.Fatal(err)
	}
	if issueRows != 0 {
		t.Fatalf("%d issue rows persisted by a stale page, want 0 (rollback broken)", issueRows)
	}
	got, err := GetVerification(ctx, pool, run.VerificationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IssueCount != 0 || *got.TargetCheckpoint != 10 {
		t.Fatalf("run after stale page = issue_count %d checkpoint %d, want 0/10", got.IssueCount, *got.TargetCheckpoint)
	}
}

func TestVerificationPageRollsBackWholePage(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedVerificationBase(t, pool)
	ctx := context.Background()

	run, err := CreateVerification(ctx, pool, CreateVerificationInput{
		RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	id10 := int64(10)
	// A severity outside the CHECK fails the issue insert — the whole page
	// (checkpoint advance included) must roll back: issues never persist
	// without their checkpoint.
	_, err = CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID: run.VerificationID,
		Issues: []VerificationIssue{{
			Code: "bad", Stage: "scan", Severity: "urgent",
		}},
		TargetCheckpoint: nil, NewTargetCheckpoint: &id10,
	})
	if err == nil {
		t.Fatal("bad severity must fail the page")
	}
	got, err := GetVerification(ctx, pool, run.VerificationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetCheckpoint != nil || got.IssueCount != 0 {
		t.Fatalf("run after failed page = checkpoint %v issue_count %d, want NULL/0 (rollback broken)",
			got.TargetCheckpoint, got.IssueCount)
	}
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.verification_issues
        WHERE verification_id = $1::uuid`, run.VerificationID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d issue rows persisted after a failed page, want 0", rows)
	}
}

func TestVerificationIssueDedupeCountsOnlyInserted(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedVerificationBase(t, pool)
	ctx := context.Background()

	run, err := CreateVerification(ctx, pool, CreateVerificationInput{
		RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	id10, id20 := int64(10), int64(20)
	src5 := int64(5)
	issue := []VerificationIssue{{
		SourceID: &src5, Code: "decode_failure", Stage: "scan", Severity: IssueMismatch,
	}}
	if _, err := CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID: run.VerificationID, Issues: issue, MismatchDelta: 1,
		TargetCheckpoint: nil, NewTargetCheckpoint: &id10,
	}); err != nil {
		t.Fatal(err)
	}
	// The next page re-reports the same (verification, source, code): the
	// unique index dedupes it and the counter delta counts only what landed.
	if _, err := CommitVerificationPage(ctx, pool, VerificationPageCommit{
		VerificationID: run.VerificationID, Issues: issue, MismatchDelta: 1,
		TargetCheckpoint: &id10, NewTargetCheckpoint: &id20,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := GetVerification(ctx, pool, run.VerificationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IssueCount != 1 {
		t.Fatalf("issue_count = %d, want 1 (deduped insert must not count)", got.IssueCount)
	}
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.verification_issues
        WHERE verification_id = $1::uuid`, run.VerificationID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d issue rows, want 1", rows)
	}
}

func TestVerificationAdoptOrNew(t *testing.T) {
	t.Parallel()

	t.Run("resumable candidate adopted in place", func(t *testing.T) {
		t.Parallel()

		pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
		runID := seedVerificationBase(t, pool)
		ctx := context.Background()

		first, err := CreateVerification(ctx, pool, CreateVerificationInput{
			RunID: runID, Kind: VerificationKindContent, StartDataRevision: 3,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Simulate a crash: the row stays `running` (interrupted would be
		// identical for this purpose).
		resumed, wasResumed, err := StartVerification(ctx, pool, CreateVerificationInput{
			RunID: runID, Kind: VerificationKindContent, StartDataRevision: 3,
		}, func(c *VerificationRun) bool {
			return c.StartDataRevision == 3 // the runner's compatibility check
		})
		if err != nil {
			t.Fatalf("StartVerification() error = %v", err)
		}
		if !wasResumed || resumed.VerificationID != first.VerificationID {
			t.Fatalf("adopted = %s resumed=%v, want the original run adopted", resumed.VerificationID, wasResumed)
		}
		if resumed.State != VerificationRunning {
			t.Fatalf("adopted state = %s, want running", resumed.State)
		}
	})

	t.Run("incompatible candidate aborted, fresh run created", func(t *testing.T) {
		t.Parallel()

		pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
		runID := seedVerificationBase(t, pool)
		ctx := context.Background()

		first, err := CreateVerification(ctx, pool, CreateVerificationInput{
			RunID: runID, Kind: VerificationKindContent, StartDataRevision: 3,
		})
		if err != nil {
			t.Fatal(err)
		}
		// The revision moved underneath the pass: refuse the resume.
		fresh, wasResumed, err := StartVerification(ctx, pool, CreateVerificationInput{
			RunID: runID, Kind: VerificationKindContent, StartDataRevision: 4,
		}, func(c *VerificationRun) bool { return false })
		if err != nil {
			t.Fatalf("StartVerification() error = %v", err)
		}
		if wasResumed || fresh.VerificationID == first.VerificationID {
			t.Fatalf("fresh = %s resumed=%v, want a new run", fresh.VerificationID, wasResumed)
		}
		old, err := GetVerification(ctx, pool, first.VerificationID)
		if err != nil {
			t.Fatal(err)
		}
		if old.State != VerificationAborted || old.CompletedAt == nil {
			t.Fatalf("superseded run = %s completed=%v, want aborted with a timestamp",
				old.State, old.CompletedAt)
		}
	})

	t.Run("interrupted candidate adopted", func(t *testing.T) {
		t.Parallel()

		pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
		runID := seedVerificationBase(t, pool)
		ctx := context.Background()

		first, err := CreateVerification(ctx, pool, CreateVerificationInput{
			RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		// A committed page, then the graceful interrupt: checkpoints and
		// counters survive the interruption for the resume.
		id10 := int64(10)
		if ok, err := CommitVerificationPage(ctx, pool, VerificationPageCommit{
			VerificationID: first.VerificationID, VerifiedDelta: 10,
			TargetCheckpoint: nil, NewTargetCheckpoint: &id10,
		}); err != nil || !ok {
			t.Fatalf("page commit = (%v,%v), want (true,nil)", ok, err)
		}
		if err := MarkInterrupted(ctx, pool, first.VerificationID); err != nil {
			t.Fatalf("MarkInterrupted() error = %v", err)
		}
		resumed, wasResumed, err := StartVerification(ctx, pool, CreateVerificationInput{
			RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 1,
		}, func(c *VerificationRun) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		if !wasResumed || resumed.TargetCheckpoint == nil || *resumed.TargetCheckpoint != 10 {
			t.Fatalf("resume lost the checkpoint: resumed=%v checkpoint=%v", wasResumed, resumed.TargetCheckpoint)
		}
	})

	t.Run("completed runs never resumed", func(t *testing.T) {
		t.Parallel()

		pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
		runID := seedVerificationBase(t, pool)
		ctx := context.Background()

		first, err := CreateVerification(ctx, pool, CreateVerificationInput{
			RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := FinishVerification(ctx, pool, FinishVerificationInput{
			VerificationID: first.VerificationID, Pass: true, ObservedTargetIDs: i64p(0),
		}); err != nil {
			t.Fatal(err)
		}
		fresh, wasResumed, err := StartVerification(ctx, pool, CreateVerificationInput{
			RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 1,
		}, func(c *VerificationRun) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		if wasResumed || fresh.VerificationID == first.VerificationID || fresh.Seq <= first.Seq {
			t.Fatalf("completed run resumed: %s resumed=%v seq %d <= %d",
				fresh.VerificationID, wasResumed, fresh.Seq, first.Seq)
		}
	})

	t.Run("multiple non-terminal runs refused", func(t *testing.T) {
		t.Parallel()

		pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
		runID := seedVerificationBase(t, pool)
		ctx := context.Background()
		for i := 0; i < 2; i++ {
			if _, err := CreateVerification(ctx, pool, CreateVerificationInput{
				RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 0,
			}); err != nil {
				t.Fatal(err)
			}
		}
		_, _, err := StartVerification(ctx, pool, CreateVerificationInput{
			RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 0,
		}, func(c *VerificationRun) bool { return true })
		var stateErr *StateError
		if !errors.As(err, &stateErr) || !errors.Is(err, ErrRunStateCorrupt) {
			t.Fatalf("StartVerification() error = %v, want ErrRunStateCorrupt", err)
		}
	})
}

func TestLatestVerificationAuthority(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedVerificationBase(t, pool)
	ctx := context.Background()

	passRun, err := CreateVerification(ctx, pool, CreateVerificationInput{
		RunID: runID, Kind: VerificationKindContent, StartDataRevision: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FinishVerification(ctx, pool, FinishVerificationInput{
		VerificationID: passRun.VerificationID, Pass: true,
	}); err != nil {
		t.Fatal(err)
	}

	failRun, err := CreateVerification(ctx, pool, CreateVerificationInput{
		RunID: runID, Kind: VerificationKindContent, StartDataRevision: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FinishVerification(ctx, pool, FinishVerificationInput{
		VerificationID: failRun.VerificationID, Pass: false,
	}); err != nil {
		t.Fatal(err)
	}

	// The authoritative content result is the LATEST run — a newer
	// completed_fail supersedes the older pass at the same revision.
	latest, err := LatestVerification(ctx, pool, VerificationKindContent)
	if err != nil {
		t.Fatalf("LatestVerification() error = %v", err)
	}
	if latest.Seq != failRun.Seq || latest.State != VerificationCompletedFail {
		t.Fatalf("latest = seq %d state %s, want the newer completed_fail (seq %d)",
			latest.Seq, latest.State, failRun.Seq)
	}
	if latest.Seq <= passRun.Seq {
		t.Fatalf("seq ordering broken: %d <= %d", latest.Seq, passRun.Seq)
	}

	if none, err := LatestVerification(ctx, pool, VerificationKindDecode); err != nil || none != nil {
		t.Fatalf("LatestVerification(decode) = %v, %v; want nil, nil", none, err)
	}
}

func TestMarkInterruptedAndAbortedTransitions(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedVerificationBase(t, pool)
	ctx := context.Background()

	run, err := CreateVerification(ctx, pool, CreateVerificationInput{
		RunID: runID, Kind: VerificationKindDecode, StartDataRevision: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkInterrupted(ctx, pool, run.VerificationID); err != nil {
		t.Fatalf("MarkInterrupted() error = %v", err)
	}
	if err := MarkInterrupted(ctx, pool, run.VerificationID); err == nil {
		t.Fatal("MarkInterrupted on a non-running run must fail")
	}
	got, err := GetVerification(ctx, pool, run.VerificationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != VerificationInterrupted {
		t.Fatalf("state = %s, want interrupted", got.State)
	}

	if err := AbortVerification(ctx, pool, run.VerificationID); err != nil {
		t.Fatalf("AbortVerification() error = %v", err)
	}
	if err := AbortVerification(ctx, pool, run.VerificationID); err == nil {
		t.Fatal("AbortVerification on a terminal run must fail")
	}
	got, err = GetVerification(ctx, pool, run.VerificationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != VerificationAborted || got.CompletedAt == nil {
		t.Fatalf("state = %s completed=%v, want aborted with timestamp", got.State, got.CompletedAt)
	}

	// Finishing a non-running run refuses too.
	if _, err := FinishVerification(ctx, pool, FinishVerificationInput{
		VerificationID: run.VerificationID, Pass: true,
	}); err == nil {
		t.Fatal("FinishVerification on an aborted run must fail")
	}
}
