package state

// Retry run store integration tests: pass lifecycle and terminal guards,
// the revision-tracked cursor CAS, adopt-or-new resume semantics and the
// append-only attempt history.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

func TestRetryRunLifecycle(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedVerificationBase(t, pool)
	ctx := context.Background()

	pass, err := CreateRetryRun(ctx, pool, CreateRetryRunInput{
		RunID: runID, StartDataRevision: 3, UnresolvedAtStart: 2,
		SourceFingerprintRaw: []byte(`{"database":"legacy_source"}`),
		SettingsRaw:          []byte(`{"batch_size":32}`),
	})
	if err != nil {
		t.Fatalf("CreateRetryRun() error = %v", err)
	}
	if pass.State != RetryRunning || pass.CurrentDataRevision != 3 || pass.UnresolvedAtStart != 2 {
		t.Fatalf("fresh pass = %+v", pass)
	}
	if len(pass.RetryID) != 36 {
		t.Fatalf("retry ID = %q, want a UUID", pass.RetryID)
	}

	// Graceful interrupt; resumable exactly like a crash.
	if err := MarkRetryInterrupted(ctx, pool, pass.RetryID); err != nil {
		t.Fatalf("MarkRetryInterrupted() error = %v", err)
	}
	// Finishing requires running.
	if _, err := FinishRetryRun(ctx, pool, pass.RetryID, true); !errors.Is(err, ErrRunStateCorrupt) {
		t.Fatalf("FinishRetryRun(interrupted) error = %v, want ErrRunStateCorrupt", err)
	}
	// Double interrupt refuses.
	if err := MarkRetryInterrupted(ctx, pool, pass.RetryID); !errors.Is(err, ErrRunStateCorrupt) {
		t.Fatalf("double interrupt error = %v, want ErrRunStateCorrupt", err)
	}

	// Re-adopt (as StartRetryRun does), attempt, then finish remaining.
	if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.retry_runs SET state = 'running'
        WHERE retry_id = $1::uuid`, pass.RetryID); err != nil {
		t.Fatal(err)
	}
	got, err := GetRetryRun(ctx, pool, pass.RetryID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RetryRunning {
		t.Fatalf("re-adopted pass state = %s", got.State)
	}
	finished, err := FinishRetryRun(ctx, pool, pass.RetryID, false)
	if err != nil {
		t.Fatalf("FinishRetryRun() error = %v", err)
	}
	if finished.State != RetryCompletedRemaining || finished.CompletedAt == nil {
		t.Fatalf("finished pass = %s completed=%v", finished.State, finished.CompletedAt)
	}
	// Terminal rows are frozen.
	if err := MarkRetryAborted(ctx, pool, pass.RetryID); !errors.Is(err, ErrRunStateCorrupt) {
		t.Fatalf("abort of a finished pass error = %v, want ErrRunStateCorrupt", err)
	}

	// A clean finish and the latest-run projection.
	pass2, err := CreateRetryRun(ctx, pool, CreateRetryRunInput{
		RunID: runID, StartDataRevision: 4, UnresolvedAtStart: 2,
		SourceFingerprintRaw: []byte(`{"database":"legacy_source"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FinishRetryRun(ctx, pool, pass2.RetryID, true); err != nil {
		t.Fatal(err)
	}
	latest, err := LatestRetryRun(ctx, pool, runID)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.RetryID != pass2.RetryID || latest.State != RetryCompletedClean {
		t.Fatalf("latest pass = %+v, want %s completed_clean", latest, pass2.RetryID)
	}
	// No passes for a run without retry history.
	none, err := LatestRetryRun(ctx, pool, "99999999-9999-4999-8999-999999999999")
	if err != nil {
		t.Fatal(err)
	}
	if none != nil {
		t.Fatalf("latest for a pass-less run = %+v, want nil", none)
	}
}

func TestAdvanceRetryRunCAS(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedVerificationBase(t, pool)
	ctx := context.Background()

	pass, err := CreateRetryRun(ctx, pool, CreateRetryRunInput{
		RunID: runID, StartDataRevision: 5, UnresolvedAtStart: 1,
		SourceFingerprintRaw: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// First attempt: cursor NULL -> 7, fixed delta, revision bump.
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		ok, err := AdvanceRetryRun(ctx, tx, AdvanceRetryInput{
			RetryID: pass.RetryID, ExpectedLastSourceID: nil,
			NewLastSourceID: i64p(7), FixedDelta: 1,
		})
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("CAS unexpectedly lost")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("first advance error = %v", err)
	}
	got, err := GetRetryRun(ctx, pool, pass.RetryID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptedCount != 1 || got.FixedCount != 1 || got.CurrentDataRevision != 6 ||
		got.LastSourceID == nil || *got.LastSourceID != 7 {
		t.Fatalf("pass after first advance = %+v", got)
	}

	// A stale cursor (NULL again) loses the CAS and must roll back.
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		ok, err := AdvanceRetryRun(ctx, tx, AdvanceRetryInput{
			RetryID: pass.RetryID, ExpectedLastSourceID: nil,
			NewLastSourceID: i64p(9), StillInvalidDelta: 1,
		})
		if err != nil {
			return err
		}
		if ok {
			return errors.New("stale CAS unexpectedly won")
		}
		return nil // roll back
	})
	if err != nil {
		t.Fatalf("stale advance error = %v", err)
	}
	got, err = GetRetryRun(ctx, pool, pass.RetryID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptedCount != 1 || got.StillInvalidCount != 0 || got.CurrentDataRevision != 6 {
		t.Fatalf("stale attempt mutated the pass: %+v", got)
	}

	// Correct cursor but a finished pass: refused.
	if _, err := FinishRetryRun(ctx, pool, pass.RetryID, true); err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		ok, err := AdvanceRetryRun(ctx, tx, AdvanceRetryInput{
			RetryID: pass.RetryID, ExpectedLastSourceID: i64p(7),
			NewLastSourceID: i64p(9), FixedDelta: 1,
		})
		if err != nil {
			return err
		}
		if ok {
			return errors.New("finished pass accepted an advance")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("advance on finished pass error = %v", err)
	}
}

func TestStartRetryRunAdoptOrNew(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedVerificationBase(t, pool)
	ctx := context.Background()
	in := CreateRetryRunInput{
		RunID: runID, StartDataRevision: 0, UnresolvedAtStart: 3,
		SourceFingerprintRaw: []byte(`{"database":"legacy_source"}`),
	}

	// Fresh create (no non-terminal candidate).
	fresh, wasResumed, err := StartRetryRun(ctx, pool, in, nil)
	if err != nil {
		t.Fatalf("StartRetryRun() error = %v", err)
	}
	if wasResumed || fresh.State != RetryRunning {
		t.Fatalf("fresh start = resumed %v state %s", wasResumed, fresh.State)
	}

	// Crash leftover (running) + resumable → adopted in place.
	resumed, wasResumed, err := StartRetryRun(ctx, pool, in, func(c *RetryRun) bool {
		return c.RetryID == fresh.RetryID
	})
	if err != nil {
		t.Fatalf("resume error = %v", err)
	}
	if !wasResumed || resumed.RetryID != fresh.RetryID || resumed.State != RetryRunning {
		t.Fatalf("resume = %s resumed=%v", resumed.RetryID, wasResumed)
	}

	// Interrupted + resumable → adopted too.
	if err := MarkRetryInterrupted(ctx, pool, fresh.RetryID); err != nil {
		t.Fatal(err)
	}
	resumed, wasResumed, err = StartRetryRun(ctx, pool, in, func(c *RetryRun) bool {
		return c.RetryID == fresh.RetryID
	})
	if err != nil {
		t.Fatalf("interrupted resume error = %v", err)
	}
	if !wasResumed || resumed.RetryID != fresh.RetryID {
		t.Fatalf("interrupted pass not adopted: resumed=%v id=%s", wasResumed, resumed.RetryID)
	}

	// Non-resumable candidate → aborted (history preserved) + fresh pass.
	second, wasResumed, err := StartRetryRun(ctx, pool, in, func(*RetryRun) bool { return false })
	if err != nil {
		t.Fatalf("incompatible start error = %v", err)
	}
	if wasResumed || second.RetryID == fresh.RetryID {
		t.Fatalf("incompatible start = resumed %v id %s", wasResumed, second.RetryID)
	}
	old, err := GetRetryRun(ctx, pool, fresh.RetryID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != RetryAborted || old.CompletedAt == nil {
		t.Fatalf("abandoned pass = %s completed=%v, want aborted", old.State, old.CompletedAt)
	}

	// resumable nil also abandons the candidate.
	third, wasResumed, err := StartRetryRun(ctx, pool, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if wasResumed || third.RetryID == second.RetryID {
		t.Fatal("nil resumable must create a fresh pass")
	}
	if _, err := GetRetryRun(ctx, pool, second.RetryID); err != nil {
		t.Fatal(err)
	}
	abandoned, err := GetRetryRun(ctx, pool, second.RetryID)
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.State != RetryAborted {
		t.Fatalf("second pass = %s, want aborted", abandoned.State)
	}

	// Completed passes are never resumed — a fresh pass starts instead.
	if _, err := FinishRetryRun(ctx, pool, third.RetryID, true); err != nil {
		t.Fatal(err)
	}
	fourth, wasResumed, err := StartRetryRun(ctx, pool, in, func(*RetryRun) bool { return true })
	if err != nil {
		t.Fatalf("post-completion start error = %v", err)
	}
	if wasResumed || fourth.RetryID == third.RetryID {
		t.Fatal("a completed pass must never be resumed")
	}

	// Two non-terminal passes is corrupt state.
	stray, err := CreateRetryRun(ctx, pool, in)
	if err != nil {
		t.Fatal(err)
	}
	_ = stray
	// four is running; mark four interrupted, then insert a second
	// non-terminal row directly.
	if err := MarkRetryInterrupted(ctx, pool, fourth.RetryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.retry_runs SET state = 'running'
        WHERE retry_id = $1::uuid`, third.RetryID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := StartRetryRun(ctx, pool, in, nil); !errors.Is(err, ErrRunStateCorrupt) {
		t.Fatalf("two non-terminal passes error = %v, want ErrRunStateCorrupt", err)
	}
}

func TestRetryAttemptHistory(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedVerificationBase(t, pool)
	ctx := context.Background()

	pass, err := CreateRetryRun(ctx, pool, CreateRetryRunInput{
		RunID: runID, StartDataRevision: 0, UnresolvedAtStart: 2,
		SourceFingerprintRaw: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := RetryAttempt{
		RetryID: pass.RetryID, RunID: runID, SourceID: 11,
		SourceInfoHash: []byte{0xaa, 0xbb}, Outcome: RetryOutcomeStillInvalid,
		Stage: "validate", Code: "still_bad", Details: map[string]any{"n": 1},
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return InsertRetryAttempt(ctx, tx, rec)
	}); err != nil {
		t.Fatalf("InsertRetryAttempt() error = %v", err)
	}

	got, err := GetRetryAttempt(ctx, pool, pass.RetryID, 11)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Outcome != RetryOutcomeStillInvalid || got.Code != "still_bad" || got.Stage != "validate" {
		t.Fatalf("attempt = %+v", got)
	}
	if missing, err := GetRetryAttempt(ctx, pool, pass.RetryID, 12); err != nil || missing != nil {
		t.Fatalf("absent attempt = %+v, %v; want nil, nil", missing, err)
	}

	// A settled attempt is never silently repeated.
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return InsertRetryAttempt(ctx, tx, rec)
	})
	if err == nil {
		t.Fatal("duplicate attempt must fail the unique index")
	}

	// Listing is stable-ordered and bounded.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := InsertRetryAttempt(ctx, tx, RetryAttempt{
			RetryID: pass.RetryID, RunID: runID, SourceID: 12,
			Outcome: RetryOutcomeFixed, Stage: "validate", Code: "fixed",
		}); err != nil {
			return err
		}
		return InsertRetryAttempt(ctx, tx, RetryAttempt{
			RetryID: pass.RetryID, RunID: runID, SourceID: 13,
			Outcome: RetryOutcomeMissing, Stage: "read", Code: "missing_source",
		})
	}); err != nil {
		t.Fatal(err)
	}
	att, err := ListRetryAttempts(ctx, pool, pass.RetryID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(att) != 3 || att[0].SourceID != 11 || att[1].SourceID != 12 || att[2].SourceID != 13 {
		t.Fatalf("attempts = %+v", att)
	}
}
