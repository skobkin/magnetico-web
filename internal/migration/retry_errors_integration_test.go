package migration

// retry-errors runner integration suite: the corrected-record acceptance
// path (repair source → retry → fully-equal verify-content pass), still-
// invalid persistence with append-only attempts, missing-source diagnosis
// under authorized stable-evidence matching, drift refusal/acceptance and
// its stable-evidence bounds, interrupt/resume, external-revision and
// further-source-change refusals (authorized passes need fresh
// authorization), precondition conflicts, and incomplete-import refusals.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
)

// retryFixture seeds source rows {1 multi-file (blob), 2 typical single
// (no blob), 3 invalid total mismatch} and imports, leaving one unresolved
// record error (id 3).
func retryFixture(t *testing.T) (string, string) {
	t.Helper()
	src, tgt := decodeFixture(t)
	seedMultiFileTorrent(t, src, 1, "multi", [][2]any{
		{"a/one.bin", int64(10)}, {"a/one.bin", int64(20)}, {"b/two.bin", int64(30)},
	})
	seedSourceTorrent(t, src, 2, "typical.bin", 40, 1700000002)
	seedSourceFile(t, src, 2, "typical.bin", 40)
	seedSourceTorrent(t, src, 3, "bad-total", 100, 1700000003)
	seedSourceFile(t, src, 3, "x.bin", 50)
	importAll(t, src, tgt)
	return src, tgt
}

// retryInvalidFixture seeds rows {1 multi-file, 2 and 3 invalid total
// mismatches} — two unresolved record errors (ids 2, 3).
func retryInvalidFixture(t *testing.T) (string, string) {
	t.Helper()
	src, tgt := decodeFixture(t)
	seedMultiFileTorrent(t, src, 1, "multi", [][2]any{
		{"a/one.bin", int64(10)}, {"a/one.bin", int64(20)}, {"b/two.bin", int64(30)},
	})
	seedSourceTorrent(t, src, 2, "bad-two", 100, 1700000002)
	seedSourceFile(t, src, 2, "x2.bin", 50)
	seedSourceTorrent(t, src, 3, "bad-three", 100, 1700000003)
	seedSourceFile(t, src, 3, "x3.bin", 60)
	importAll(t, src, tgt)
	return src, tgt
}

func retryPass(t *testing.T, ctx context.Context, src, tgt string, tweak func(*RetryErrorsInput)) (*RetryResult, error) {
	t.Helper()
	in := RetryErrorsInput{SourceDSN: src, TargetDSN: tgt, Workers: 2, BatchSize: 10}
	if tweak != nil {
		tweak(&in)
	}
	return RetryErrors(ctx, in)
}

// runIDFor reads the target's import run ID.
func runIDFor(t *testing.T, tgt string) string {
	t.Helper()
	pool := poolForRunner(t, tgt)
	var runID string
	if err := pool.QueryRow(context.Background(),
		`SELECT run_id::text FROM magnetico_migration.progress`).Scan(&runID); err != nil {
		t.Fatalf("progress run_id: %v", err)
	}
	return runID
}

// latestRetryRun reads the latest retry pass of the target's run.
func latestRetryRun(t *testing.T, tgt string) *state.RetryRun {
	t.Helper()
	pool := poolForRunner(t, tgt)
	run, err := state.LatestRetryRun(context.Background(), pool, runIDFor(t, tgt))
	if err != nil {
		t.Fatalf("LatestRetryRun() error = %v", err)
	}
	if run == nil {
		t.Fatal("no retry run exists")
	}
	return run
}

// errorRow reads one registry row's (attempts, resolved) state.
func errorRow(t *testing.T, tgt string, sourceID int64) (int32, bool) {
	t.Helper()
	pool := poolForRunner(t, tgt)
	var attempts int32
	var resolved bool
	if err := pool.QueryRow(context.Background(),
		`SELECT attempts, resolved FROM magnetico_migration.errors
        WHERE scope = 'record' AND source_id = $1`, sourceID).
		Scan(&attempts, &resolved); err != nil {
		t.Fatalf("error row %d: %v", sourceID, err)
	}
	return attempts, resolved
}

// progressPhase reads the current import phase.
func progressPhase(t *testing.T, tgt string) string {
	t.Helper()
	pool := poolForRunner(t, tgt)
	var phase string
	if err := pool.QueryRow(context.Background(),
		`SELECT phase FROM magnetico_migration.progress`).Scan(&phase); err != nil {
		t.Fatalf("progress phase: %v", err)
	}
	return phase
}

func TestRetryErrorsCorrectedRecordRepairsAndVerifies(t *testing.T) {
	t.Parallel()

	src, tgt := retryFixture(t)

	// The operator repairs the source record in place: total_size matches
	// the file list again. The fingerprint anchors do not move (counts, ID
	// bounds, anchor hashes), so no drift and no authorization is needed.
	execSourceSQL(t, src, `UPDATE magneticod.torrents SET total_size = 50 WHERE id = 3`)

	res, err := retryPass(t, context.Background(), src, tgt, nil)
	if err != nil {
		t.Fatalf("RetryErrors() error = %v", err)
	}
	if res.Fixed != 1 || res.Attempted != 1 || res.UnresolvedRemaining != 0 {
		t.Fatalf("result = attempted %d fixed %d remaining %d, want 1/1/0",
			res.Attempted, res.Fixed, res.UnresolvedRemaining)
	}
	if res.State != state.RetryCompletedClean {
		t.Fatalf("pass state = %s, want completed_clean", res.State)
	}
	if !res.PhaseFlipped {
		t.Fatal("phase flip did not happen after full repair")
	}
	if phase := progressPhase(t, tgt); phase != "import_complete" {
		t.Fatalf("phase = %s, want import_complete", phase)
	}
	attempts, resolved := errorRow(t, tgt, 3)
	if attempts != 2 || !resolved {
		t.Fatalf("error row attempts %d resolved %v, want 2/true", attempts, resolved)
	}

	run := latestRetryRun(t, tgt)
	if run.State != state.RetryCompletedClean || run.LastSourceID == nil || *run.LastSourceID != 3 {
		t.Fatalf("retry run = %s cursor %v, want completed_clean at 3", run.State, run.LastSourceID)
	}

	// The acceptance criterion: after the repair, a full verify-content
	// pass is completely equal.
	vres, verr := verifyContent(t, context.Background(), src, tgt, nil)
	if verr != nil {
		t.Fatalf("verify-content error = %v", verr)
	}
	if !vres.Pass || vres.SourceObserved != 3 || vres.ObservedTargets != 3 {
		t.Fatalf("verify-content = pass %v %d/%d, want full-equality pass 3/3",
			vres.Pass, vres.SourceObserved, vres.ObservedTargets)
	}
}

func TestRetryErrorsStillInvalidStaysUnresolved(t *testing.T) {
	t.Parallel()

	src, tgt := retryInvalidFixture(t)

	res, err := retryPass(t, context.Background(), src, tgt, nil)
	if err == nil || !errors.Is(err, ErrRetryRemaining) {
		t.Fatalf("error = %v, want ErrRetryRemaining", err)
	}
	if res.StillInvalid != 2 || res.Fixed != 0 || res.UnresolvedRemaining != 2 {
		t.Fatalf("result = invalid %d fixed %d remaining %d, want 2/0/2",
			res.StillInvalid, res.Fixed, res.UnresolvedRemaining)
	}
	if res.State != state.RetryCompletedRemaining {
		t.Fatalf("pass state = %s, want completed_remaining", res.State)
	}
	if res.PhaseFlipped {
		t.Fatal("phase flipped with unresolved records")
	}
	if phase := progressPhase(t, tgt); phase != "import_complete_with_errors" {
		t.Fatalf("phase = %s, want import_complete_with_errors", phase)
	}
	for _, id := range []int64{2, 3} {
		attempts, resolved := errorRow(t, tgt, id)
		if attempts != 2 || resolved {
			t.Fatalf("error row %d attempts %d resolved %v, want 2/false", id, attempts, resolved)
		}
	}

	// A later invocation starts a NEW pass over the still-unresolved IDs;
	// attempts keep accumulating (append-only history).
	res2, err := retryPass(t, context.Background(), src, tgt, nil)
	if err == nil || !errors.Is(err, ErrRetryRemaining) {
		t.Fatalf("second pass error = %v, want ErrRetryRemaining", err)
	}
	if res2.Resumed {
		t.Fatal("second invocation resumed a completed pass")
	}
	attempts, _ := errorRow(t, tgt, 2)
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

// TestRetryErrorsSparseBatchSettlesPerWindow: a batch mixing fixable
// records with deleted (missing-source) IDs, forced across multiple
// FetchByIDs windows by a 1 MiB window budget, settles each window
// immediately — interleaving the missing IDs below their fetched neighbors
// so the pass cursor never moves backward. Every registry ID is attempted
// exactly once with the right outcome (Codex review iteration 1, finding 6).
func TestRetryErrorsSparseBatchSettlesPerWindow(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	// Rows 1, 4, 7, 8 import (1 and 8 anchor MinID/MaxID). Rows 2 and 5 are
	// repairable total-size mismatches with ~600 KiB of path data each —
	// two of them exceed the 1 MiB window budget, so the fetch splits.
	// Rows 3 and 6 are mismatches too, deleted after the import (between
	// the fetched IDs, exercising the gap handling).
	bigFiles := func(id int32, name string) {
		seedSourceTorrent(t, src, id, name, 79, 1700000002)
		for i := 0; i < 12; i++ {
			seedSourceFile(t, src, id, fmt.Sprintf("d%d/%s", i, randomASCIIPath(t, 50_000)), int64(1+i))
		}
	}
	smallFiles := func(id int32, name string) {
		seedSourceTorrent(t, src, id, name, 40, 1700000002)
		seedSourceFile(t, src, id, fmt.Sprintf("x%d.bin", id), 40)
	}
	smallFiles(1, "ok1.bin")
	bigFiles(2, "repair2")
	bigFiles(3, "vanish3")
	smallFiles(4, "ok4.bin")
	bigFiles(5, "repair5")
	bigFiles(6, "vanish6")
	smallFiles(7, "ok7.bin")
	smallFiles(8, "ok8.bin")
	importAll(t, src, tgt)

	// Fixture assumptions: rows 2 and 5 exist in the registry, and their
	// combined path bytes exceed the window budget — the fetch MUST split.
	for _, id := range []int64{2, 3, 5, 6} {
		if attempts, resolved := errorRow(t, tgt, id); attempts != 1 || resolved {
			t.Fatalf("registry row %d = attempts %d resolved %v, want 1/false", id, attempts, resolved)
		}
	}
	var pathBytes int64
	pool := poolForRunner(t, src)
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(octet_length(path)), 0) FROM magneticod.files WHERE torrent_id IN (2, 5)`).
		Scan(&pathBytes); err != nil {
		t.Fatal(err)
	}
	if pathBytes <= 1<<20 {
		t.Fatalf("fixture assumption broken: rows 2+5 path bytes = %d, want >1MiB (no window split)", pathBytes)
	}

	// Operator actions: repair 2 and 5; rows 3 and 6 vanish (content anchors
	// move — explicit authorization; database name and ID bounds stay).
	execSourceSQL(t, src, `UPDATE magneticod.torrents SET total_size = 78 WHERE id IN (2, 5)`)
	execSourceSQL(t, src, `DELETE FROM magneticod.torrents WHERE id IN (3, 6)`)

	res, err := retryPass(t, context.Background(), src, tgt, func(in *RetryErrorsInput) {
		in.AcceptSourceRepairs = true
		in.MaxInflightBytes = 1 << 20
	})
	if err == nil || !errors.Is(err, ErrRetryRemaining) {
		t.Fatalf("error = %v, want ErrRetryRemaining", err)
	}
	if res.Attempted != 4 || res.Fixed != 2 || res.Missing != 2 || res.StillInvalid != 0 || res.UnresolvedRemaining != 2 {
		t.Fatalf("result = attempted %d fixed %d missing %d invalid %d remaining %d, want 4/2/2/0/2",
			res.Attempted, res.Fixed, res.Missing, res.StillInvalid, res.UnresolvedRemaining)
	}

	// Every registry ID attempted exactly once with the right outcome.
	for _, id := range []int64{2, 5} {
		attempts, resolved := errorRow(t, tgt, id)
		if attempts != 2 || !resolved {
			t.Fatalf("row %d attempts %d resolved %v, want 2/true", id, attempts, resolved)
		}
	}
	for _, id := range []int64{3, 6} {
		attempts, resolved := errorRow(t, tgt, id)
		if attempts != 2 || resolved {
			t.Fatalf("row %d attempts %d resolved %v, want 2/false (missing never resolves)", id, attempts, resolved)
		}
	}
	for id, want := range map[int64]string{2: "fixed", 3: "missing_source", 5: "fixed", 6: "missing_source"} {
		run := latestRetryRun(t, tgt)
		row, err := state.GetRetryAttempt(context.Background(), poolForRunner(t, tgt), run.RetryID, id)
		if err != nil {
			t.Fatal(err)
		}
		if row == nil || row.Outcome != want {
			t.Fatalf("attempt row %d = %+v, want outcome %s", id, row, want)
		}
	}

	// The cursor ends at the batch end — ascending through 2, 3, 5, 6.
	run := latestRetryRun(t, tgt)
	if run.State != state.RetryCompletedRemaining || run.LastSourceID == nil || *run.LastSourceID != 6 {
		t.Fatalf("retry run = %s cursor %v, want completed_remaining at 6", run.State, run.LastSourceID)
	}
}

func TestRetryErrorsMissingSourceDiagnosed(t *testing.T) {
	t.Parallel()

	// Registry = {2} with the invalid record in the MIDDLE of the ID range:
	// deleting it moves no stable-evidence anchor (database, MinID, MaxID),
	// so an authorized pass can proceed and must diagnose the disappearance
	// explicitly instead of silently resolving the error.
	src, tgt := decodeFixture(t)
	seedMultiFileTorrent(t, src, 1, "multi", [][2]any{
		{"a/one.bin", int64(10)}, {"a/one.bin", int64(20)}, {"b/two.bin", int64(30)},
	})
	seedSourceTorrent(t, src, 2, "bad-middle", 100, 1700000002)
	seedSourceFile(t, src, 2, "x2.bin", 50)
	seedSourceTorrent(t, src, 3, "typical.bin", 40, 1700000003)
	seedSourceFile(t, src, 3, "typical.bin", 40)
	importAll(t, src, tgt)

	execSourceSQL(t, src, `DELETE FROM magneticod.torrents WHERE id = 2`)

	res, err := retryPass(t, context.Background(), src, tgt, func(in *RetryErrorsInput) {
		in.AcceptSourceRepairs = true
	})
	if err == nil || !errors.Is(err, ErrRetryRemaining) {
		t.Fatalf("error = %v, want ErrRetryRemaining", err)
	}
	if res.Missing != 1 || res.StillInvalid != 0 || res.Fixed != 0 || res.UnresolvedRemaining != 1 {
		t.Fatalf("result = missing %d invalid %d fixed %d remaining %d, want 1/0/0/1",
			res.Missing, res.StillInvalid, res.Fixed, res.UnresolvedRemaining)
	}
	attempts, resolved := errorRow(t, tgt, 2)
	if attempts != 2 || resolved {
		t.Fatalf("missing row attempts %d resolved %v, want 2/false", attempts, resolved)
	}
	run := latestRetryRun(t, tgt)
	if !run.AcceptedSourceRepair {
		t.Fatal("pass did not record the accepted source repair")
	}
	if run.PreviousFingerprintRaw == nil {
		t.Fatal("pass did not record the previous fingerprint")
	}
}

func TestRetryErrorsDriftPolicy(t *testing.T) {
	t.Parallel()

	src, tgt := retryFixture(t)
	// NULL-owned source file row: the content anchors move (file rows,
	// NULL-owner counts) while the stable evidence (database, ID bounds)
	// stays intact.

	t.Run("default refuses drifted source", func(t *testing.T) {
		t.Parallel()
		s2, t2 := retryFixture(t)
		execSourceSQL(t, s2, `INSERT INTO magneticod.files (torrent_id, path, size) VALUES (NULL, 'dangling.bin', 5)`)
		if _, err := retryPass(t, context.Background(), s2, t2, nil); !errors.Is(err, ErrSourceDrift) {
			t.Fatalf("error = %v, want ErrSourceDrift", err)
		}
	})

	t.Run("stable-evidence mismatch refuses even authorized", func(t *testing.T) {
		t.Parallel()
		s2, t2 := retryFixture(t)
		execSourceSQL(t, s2, `DELETE FROM magneticod.torrents WHERE id = 3`) // MaxID bound moves
		if _, err := retryPass(t, context.Background(), s2, t2, func(in *RetryErrorsInput) {
			in.AcceptSourceRepairs = true
		}); !errors.Is(err, ErrSourceDrift) {
			t.Fatalf("error = %v, want ErrSourceDrift", err)
		}
	})

	execSourceSQL(t, src, `INSERT INTO magneticod.files (torrent_id, path, size) VALUES (NULL, 'dangling.bin', 5)`)
	res, err := retryPass(t, context.Background(), src, tgt, func(in *RetryErrorsInput) {
		in.AcceptSourceRepairs = true
	})
	if err == nil || !errors.Is(err, ErrRetryRemaining) {
		t.Fatalf("authorized error = %v, want ErrRetryRemaining", err)
	}
	if !res.AcceptedSourceRepair {
		t.Fatal("accepted_source_repair not recorded")
	}
	run := latestRetryRun(t, tgt)
	if run.PreviousFingerprintRaw == nil {
		t.Fatal("previous fingerprint not recorded")
	}
	// The stored import fingerprint is never mutated.
	pool := poolForRunner(t, tgt)
	var fpRaw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT source_fingerprint FROM magnetico_migration.progress`).Scan(&fpRaw); err != nil {
		t.Fatal(err)
	}
	if string(fpRaw) == "{}" || len(fpRaw) == 0 {
		t.Fatal("import fingerprint vanished")
	}
}

func TestRetryErrorsInterruptResumes(t *testing.T) {
	t.Parallel()

	src, tgt := retryInvalidFixture(t)

	canceled := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := retryPass(t, ctx, src, tgt, func(in *RetryErrorsInput) {
		in.BatchSize = 1
		in.Progress = func(p RetryProgress) {
			if !canceled && p.Cursor >= 2 {
				canceled = true
				cancel()
			}
		}
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v, want ErrInterrupted", err)
	}
	interrupted := latestRetryRun(t, tgt)
	if interrupted.State != state.RetryInterrupted || interrupted.LastSourceID == nil ||
		*interrupted.LastSourceID != 2 || interrupted.AttemptedCount != 1 {
		t.Fatalf("pass = %s cursor %v attempted %d, want interrupted/2/1",
			interrupted.State, interrupted.LastSourceID, interrupted.AttemptedCount)
	}

	res, err := retryPass(t, context.Background(), src, tgt, nil)
	if err == nil || !errors.Is(err, ErrRetryRemaining) {
		t.Fatalf("resume error = %v, want ErrRetryRemaining", err)
	}
	if !res.Resumed || res.RetryID != interrupted.RetryID {
		t.Fatalf("resume = %v/%s, want adopted pass %s", res.Resumed, res.RetryID, interrupted.RetryID)
	}
	if res.Attempted != 2 || res.StillInvalid != 2 {
		t.Fatalf("resume = attempted %d invalid %d, want 2/2", res.Attempted, res.StillInvalid)
	}
}

func TestRetryErrorsResumeAfterExternalRevisionRefuses(t *testing.T) {
	t.Parallel()

	src, tgt := retryInvalidFixture(t)

	canceled := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := retryPass(t, ctx, src, tgt, func(in *RetryErrorsInput) {
		in.BatchSize = 1
		in.Progress = func(p RetryProgress) {
			if !canceled && p.Cursor >= 2 {
				canceled = true
				cancel()
			}
		}
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v, want ErrInterrupted", err)
	}
	interrupted := latestRetryRun(t, tgt)

	// Another mutation moves progress.data_revision past the pass's own
	// revision-tracked cursor: the pass is stale and must abort.
	execTargetSQL(t, tgt, `UPDATE magnetico_migration.progress SET data_revision = data_revision + 2`)

	res, err := retryPass(t, context.Background(), src, tgt, nil)
	if err == nil || !errors.Is(err, ErrRetryRemaining) {
		t.Fatalf("error = %v, want ErrRetryRemaining (a fresh pass)", err)
	}
	if res.Resumed || res.RetryID == interrupted.RetryID {
		t.Fatalf("resumed %v pass %s, want a fresh pass", res.Resumed, res.RetryID)
	}
	pool := poolForRunner(t, tgt)
	old, err := state.GetRetryRun(context.Background(), pool, interrupted.RetryID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != state.RetryAborted {
		t.Fatalf("old pass state = %s, want aborted", old.State)
	}
}

func TestRetryErrorsAuthorizedPassNeedsFreshAuthorization(t *testing.T) {
	t.Parallel()

	src, tgt := retryInvalidFixture(t)
	// Authorized repair pass over a drifted source.
	execSourceSQL(t, src, `INSERT INTO magneticod.files (torrent_id, path, size) VALUES (NULL, 'dangling.bin', 5)`)

	canceled := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := retryPass(t, ctx, src, tgt, func(in *RetryErrorsInput) {
		in.BatchSize = 1
		in.AcceptSourceRepairs = true
		in.Progress = func(p RetryProgress) {
			if !canceled && p.Cursor >= 2 {
				canceled = true
				cancel()
			}
		}
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v, want ErrInterrupted", err)
	}

	t.Run("resume without the flag refuses", func(t *testing.T) {
		t.Parallel()
		// Fresh fixture: identical drift setup, interrupted the same way.
		s2, t2 := retryInvalidFixture(t)
		execSourceSQL(t, s2, `INSERT INTO magneticod.files (torrent_id, path, size) VALUES (NULL, 'dangling.bin', 5)`)
		canceled2 := false
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		_, err := retryPass(t, ctx2, s2, t2, func(in *RetryErrorsInput) {
			in.BatchSize = 1
			in.AcceptSourceRepairs = true
			in.Progress = func(p RetryProgress) {
				if !canceled2 && p.Cursor >= 2 {
					canceled2 = true
					cancel2()
				}
			}
		})
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("setup error = %v, want ErrInterrupted", err)
		}
		if _, err := retryPass(t, context.Background(), s2, t2, nil); !errors.Is(err, ErrSourceDrift) {
			t.Fatalf("error = %v, want ErrSourceDrift (authorization does not carry)", err)
		}
	})

	// A further source change aborts the authorized pass even WITH the
	// flag: the fresh fingerprint no longer matches the pass's own.
	execSourceSQL(t, src, `INSERT INTO magneticod.files (torrent_id, path, size) VALUES (NULL, 'dangling2.bin', 6)`)
	res, err := retryPass(t, context.Background(), src, tgt, func(in *RetryErrorsInput) {
		in.AcceptSourceRepairs = true
	})
	if err == nil || !errors.Is(err, ErrRetryRemaining) {
		t.Fatalf("error = %v, want ErrRetryRemaining (fresh pass)", err)
	}
	if res.Resumed {
		t.Fatal("authorized pass resumed after its source changed")
	}
	pool := poolForRunner(t, tgt)
	old, err := state.GetRetryRun(context.Background(), pool, latestRetryRunBefore(t, tgt, res.RetryID))
	if err != nil {
		t.Fatal(err)
	}
	if old.State != state.RetryAborted {
		t.Fatalf("old pass state = %s, want aborted", old.State)
	}
}

// latestRetryRunBefore reads the retry pass that ran immediately before
// retryID (the aborted predecessor).
func latestRetryRunBefore(t *testing.T, tgt, retryID string) string {
	t.Helper()
	pool := poolForRunner(t, tgt)
	rows, err := pool.Query(context.Background(), `SELECT retry_id::text, started_at
        FROM magnetico_migration.retry_runs WHERE run_id = $1::uuid
        ORDER BY started_at`, runIDFor(t, tgt))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var before string
	for rows.Next() {
		var id string
		var _ignored any
		if err := rows.Scan(&id, &_ignored); err != nil {
			t.Fatal(err)
		}
		if id == retryID {
			break
		}
		before = id
	}
	if before == "" {
		t.Fatal("no predecessor retry pass found")
	}
	return before
}

func TestRetryErrorsConflictsFatal(t *testing.T) {
	t.Parallel()

	src, tgt := retryFixture(t)
	// Repair the source record first, so the retry takes the SUCCESS path
	// and reaches the target-row proof (a still-invalid attempt never gets
	// there). A target row for the unresolved ID then appears outside the
	// retry path (counter-consistent tamper): the attempt must stop as a
	// conflict.
	execSourceSQL(t, src, `UPDATE magneticod.torrents SET total_size = 50 WHERE id = 3`)
	execTargetSQL(t, tgt, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on, file_count)
        VALUES (3, decode('6333','hex'), 'sneaky', 50, 1700000003, 1)`)
	execTargetSQL(t, tgt, `UPDATE magnetico_migration.progress
        SET success_count = success_count + 1, scanned_count = scanned_count + 1`)

	res, err := retryPass(t, context.Background(), src, tgt, nil)
	if err == nil || !errors.Is(err, target.ErrRetryConflict) {
		t.Fatalf("error = %v, want ErrRetryConflict", err)
	}
	if res != nil {
		t.Fatalf("result = %+v, want nil on a fatal conflict", res)
	}
	// Nothing landed: the failed transaction rolled back completely.
	var count int64
	pool := poolForRunner(t, tgt)
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM magnetico_migration.retry_attempts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("retry attempts = %d, want 0 (rollback)", count)
	}
	attempts, resolved := errorRow(t, tgt, 3)
	if attempts != 1 || resolved {
		t.Fatalf("error row attempts %d resolved %v, want 1/false", attempts, resolved)
	}
}

func TestRetryErrorsRequiresCompletedImport(t *testing.T) {
	t.Parallel()

	tgt := createTargetForRunner(t)
	in := RetryErrorsInput{SourceDSN: "postgres://127.0.0.1:1/none", TargetDSN: tgt}
	if _, err := RetryErrors(context.Background(), in); !errors.Is(err, target.ErrRunNotComplete) {
		t.Fatalf("error = %v, want ErrRunNotComplete", err)
	}
}
