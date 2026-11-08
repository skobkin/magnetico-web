package migration

// verify-decode integration suite: clean pass (mixed row shapes), every
// finding class, the octet_length-first oversized handling (tiny-limit
// classification and a real oversized corrupted blob), solo admission of a
// valid over-budget blob, collectively-over-budget chunk splitting,
// interrupt/resume with issue dedupe, repeated invocations, pass-with-
// legacy-errors and incomplete-import refusals.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// decodeFixture returns a seeded source and a completed-import target.
func decodeFixture(t *testing.T) (string, string) {
	t.Helper()
	src := runnerSource(t)
	tgt := createTargetForRunner(t)
	return src, tgt
}

func importAll(t *testing.T, src, tgt string) {
	t.Helper()
	if err := runTorrents(t, context.Background(), TorrentsInput{SourceDSN: src, TargetDSN: tgt}); err != nil {
		t.Fatalf("import error = %v", err)
	}
}

// seedMultiFileTorrent seeds a multi-file source torrent (duplicate-safe).
func seedMultiFileTorrent(t *testing.T, dsn string, id int32, name string, files [][2]any) {
	t.Helper()
	var total int64
	for _, f := range files {
		total += f[1].(int64)
	}
	seedSourceTorrent(t, dsn, id, name, total, 1700000000+id%1000)
	for _, f := range files {
		seedSourceFile(t, dsn, id, f[0].(string), f[1].(int64))
	}
}

// execTargetSQL runs fixture manipulation SQL on a target.
func execTargetSQL(t *testing.T, dsn, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("target connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("target exec %q: %v", sql, err)
	}
}

// verifyDecoded runs one verify-decode pass with sane test defaults.
func verifyDecoded(t *testing.T, ctx context.Context, tgt string, tweak func(*VerifyDecodeInput)) (*VerificationResult, error) {
	t.Helper()
	in := VerifyDecodeInput{TargetDSN: tgt, Workers: 2, BatchSize: 100}
	if tweak != nil {
		tweak(&in)
	}
	return VerifyDecode(ctx, in)
}

// latestDecodeRun reads the authoritative decode run plus its issues.
func latestDecodeRun(t *testing.T, tgt string) (*state.VerificationRun, []state.VerificationIssue) {
	t.Helper()
	ctx := context.Background()
	pool := poolForRunner(t, tgt)
	run, err := state.LatestVerification(ctx, pool, state.VerificationKindDecode)
	if err != nil {
		t.Fatalf("LatestVerification() error = %v", err)
	}
	if run == nil {
		t.Fatal("no decode verification run exists")
	}
	issues, err := state.ListVerificationIssues(ctx, pool, run.VerificationID, 1000)
	if err != nil {
		t.Fatalf("ListVerificationIssues() error = %v", err)
	}
	return run, issues
}

func issueCodes(issues []state.VerificationIssue) map[string]int {
	codes := map[string]int{}
	for _, is := range issues {
		codes[is.Code]++
	}
	return codes
}

// targetFingerprint is a mutation detector for the whole target dataset
// (progress row as JSONB text + torrent and blob rows), used to prove the
// verification pass never modifies data.
func targetFingerprint(t *testing.T, dsn string) [3]string {
	t.Helper()
	pool := poolForRunner(t, dsn)
	var progJSON, torrentsMD5, blobsMD5 string
	err := pool.QueryRow(context.Background(), `SELECT
        (SELECT to_jsonb(progress)::text FROM magnetico_migration.progress),
        (SELECT md5(COALESCE(string_agg(x, '' ORDER BY id), '')) FROM
            (SELECT row_number() OVER (ORDER BY id)::text || to_jsonb(torrents)::text AS x, id
             FROM magneticod.torrents) d),
        (SELECT md5(COALESCE(string_agg(x, '' ORDER BY torrent_id), '')) FROM
            (SELECT row_number() OVER (ORDER BY torrent_id)::text || to_jsonb(b)::text AS x, torrent_id
             FROM magneticod.torrent_file_blobs b) d2)`).
		Scan(&progJSON, &torrentsMD5, &blobsMD5)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return [3]string{progJSON, torrentsMD5, blobsMD5}
}

func TestVerifyDecodeCleanPass(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	// 1: multi-file with duplicate paths (blob); 2: typical single
	// (no-blob eligible → assumed single); 3: atypical single (blob).
	seedMultiFileTorrent(t, src, 1, "multi", [][2]any{
		{"a/one.bin", int64(10)}, {"a/one.bin", int64(20)}, {"b/two.bin", int64(30)},
	})
	seedSourceTorrent(t, src, 2, "typical.bin", 40, 1700000002)
	seedSourceFile(t, src, 2, "typical.bin", 40)
	seedSourceTorrent(t, src, 3, "atypical", 50, 1700000003)
	seedSourceFile(t, src, 3, "different.bin", 50)
	importAll(t, src, tgt)

	before := targetFingerprint(t, tgt)
	res, err := verifyDecoded(t, context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("VerifyDecode() error = %v", err)
	}
	if !res.Pass {
		t.Fatalf("pass = false, issues %+v", res.IssuesBySeverity)
	}
	if res.ObservedTargets != 3 || res.Verified != 2 || res.AssumedSingle != 1 {
		t.Fatalf("result = observed %d verified %d assumed %d, want 3/2/1",
			res.ObservedTargets, res.Verified, res.AssumedSingle)
	}
	if res.UnresolvedRecordErrors != 0 || res.UnresolvedIntegrity != 0 {
		t.Fatalf("registry counts = %d/%d, want 0/0", res.UnresolvedRecordErrors, res.UnresolvedIntegrity)
	}
	// Verification never modifies target data or run state.
	after := targetFingerprint(t, tgt)
	if before != after {
		t.Fatalf("target mutated by the verification pass:\nbefore %v\nafter  %v", before, after)
	}

	run, issues := latestDecodeRun(t, tgt)
	if run.State != state.VerificationCompletedPass || run.Phase != state.VerificationPhaseComplete {
		t.Fatalf("run = %s/%s, want completed_pass/complete", run.State, run.Phase)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %+v, want none", issues)
	}
}

func TestVerifyDecodeEmptyTargetPass(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	importAll(t, src, tgt) // empty source: import completes with zero rows

	res, err := verifyDecoded(t, context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("VerifyDecode() error = %v", err)
	}
	if !res.Pass || res.ObservedTargets != 0 || res.ExpectedTargets == nil || *res.ExpectedTargets != 0 {
		t.Fatalf("result = pass %v observed %d expected %v, want pass with 0 rows",
			res.Pass, res.ObservedTargets, res.ExpectedTargets)
	}
}

func TestVerifyDecodeFindingsFail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		corrupt      func(t *testing.T, src, tgt string)
		wantCode     string
		wantState    string
		wantMismatch int64
	}{
		{
			name: "garbage blob is a wire-version finding",
			corrupt: func(t *testing.T, src, tgt string) {
				execTargetSQL(t, tgt, `UPDATE magneticod.torrent_file_blobs b
                    SET data = '\x0001020304' FROM magneticod.torrents t
                    WHERE b.torrent_id = t.id AND t.id = 1`)
			},
			wantCode:     "wire_version",
			wantState:    state.VerificationCompletedFail,
			wantMismatch: 1,
		},
		{
			name: "total size drift on a blob row is invalid_metadata",
			corrupt: func(t *testing.T, src, tgt string) {
				execTargetSQL(t, tgt, `UPDATE magneticod.torrents SET total_size = total_size + 1 WHERE id = 1`)
			},
			wantCode:     IssueCodeInvalidMetadata,
			wantState:    state.VerificationCompletedFail,
			wantMismatch: 1,
		},
		{
			name: "malformed metadata on a blobless single is invalid_metadata",
			corrupt: func(t *testing.T, src, tgt string) {
				execTargetSQL(t, tgt, `UPDATE magneticod.torrents SET info_hash = decode('AA','hex') WHERE id = 2`)
			},
			wantCode:     IssueCodeInvalidMetadata,
			wantState:    state.VerificationCompletedFail,
			wantMismatch: 1,
		},
		{
			name: "typical single stored with a blob is noncanonical",
			corrupt: func(t *testing.T, src, tgt string) {
				// Manufacture what the import path can never produce: a blob
				// for a typical single file, with the blob counter kept
				// consistent so the corruption is row-level, not counter-level.
				var name string
				var total int64
				pool := poolForRunner(t, tgt)
				if err := pool.QueryRow(context.Background(),
					`SELECT name, total_size FROM magneticod.torrents WHERE id = 2`).
					Scan(&name, &total); err != nil {
					t.Fatal(err)
				}
				enc := codec.NewEncoder(codec.DefaultProfile(), codec.DefaultLimits(), 1)
				blob, _, err := enc.Encode([]codec.File{{Path: name, Size: total}})
				if err != nil {
					t.Fatal(err)
				}
				execTargetSQL(t, tgt,
					`INSERT INTO magneticod.torrent_file_blobs (torrent_id, data) VALUES (2, $1)`, blob)
				execTargetSQL(t, tgt,
					`UPDATE magnetico_migration.progress SET blob_count = blob_count + 1`)
			},
			wantCode:     IssueCodeNoncanonical,
			wantState:    state.VerificationCompletedFail,
			wantMismatch: 1,
		},
		{
			name: "orphan target blob is an extras finding",
			corrupt: func(t *testing.T, src, tgt string) {
				// The FK would block an orphan blob; dropping it is exactly
				// the tamper the extras phase exists to detect. Reconcile
				// counts raw blob rows, so the counter bump keeps the
				// preflight green while the anomaly stays relational (a blob
				// whose torrent row does not exist).
				execTargetSQL(t, tgt, `ALTER TABLE magneticod.torrent_file_blobs
                    DROP CONSTRAINT torrent_file_blobs_torrent_id_fkey`)
				execTargetSQL(t, tgt,
					`INSERT INTO magneticod.torrent_file_blobs (torrent_id, data) VALUES (999999, '\x78da')`)
				execTargetSQL(t, tgt,
					`UPDATE magnetico_migration.progress SET blob_count = blob_count + 1`)
			},
			wantCode:     IssueCodeOrphanBlobs,
			wantState:    state.VerificationCompletedFail,
			wantMismatch: 0, // an extras finding; no scan row failed
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src, tgt := decodeFixture(t)
			seedMultiFileTorrent(t, src, 1, "multi", [][2]any{
				{"a/one.bin", int64(10)}, {"b/two.bin", int64(20)},
			})
			seedSourceTorrent(t, src, 2, "typical.bin", 40, 1700000002)
			seedSourceFile(t, src, 2, "typical.bin", 40)
			importAll(t, src, tgt)
			tc.corrupt(t, src, tgt)

			res, err := verifyDecoded(t, context.Background(), tgt, nil)
			if !errors.Is(err, ErrVerificationFindings) {
				t.Fatalf("error = %v, want ErrVerificationFindings", err)
			}
			if res == nil || res.Pass {
				t.Fatalf("pass = %v with a nil result", res)
			}
			run, issues := latestDecodeRun(t, tgt)
			if run.State != tc.wantState {
				t.Fatalf("run state = %s, want %s", run.State, tc.wantState)
			}
			codes := issueCodes(issues)
			if codes[tc.wantCode] != 1 {
				t.Fatalf("issue codes = %v, want exactly one %q", codes, tc.wantCode)
			}
			if res.Mismatched != tc.wantMismatch {
				t.Fatalf("mismatch counter = %d, want %d", res.Mismatched, tc.wantMismatch)
			}
		})
	}
}

func TestVerifyDecodeOversizedBlobFinding(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	seedMultiFileTorrent(t, src, 1, "multi", [][2]any{
		{"a/one.bin", int64(10)}, {"b/two.bin", int64(20)},
	})
	seedSourceTorrent(t, src, 2, "typical.bin", 40, 1700000002)
	seedSourceFile(t, src, 2, "typical.bin", 40)
	importAll(t, src, tgt)

	// A genuinely oversized corrupted blob (far beyond the 32 MiB cap).
	big := make([]byte, 40<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	execTargetSQL(t, tgt,
		`UPDATE magneticod.torrent_file_blobs SET data = $1 WHERE torrent_id = 1`, big)

	// Pass 1 (production limits): the oversized blob is a typed finding
	// WITHOUT its content being decoded; the other rows stay clean.
	res, err := verifyDecoded(t, context.Background(), tgt, nil)
	if !errors.Is(err, ErrVerificationFindings) {
		t.Fatalf("error = %v, want ErrVerificationFindings", err)
	}
	run, issues := latestDecodeRun(t, tgt)
	if run.State != state.VerificationCompletedFail {
		t.Fatalf("run state = %s", run.State)
	}
	codes := issueCodes(issues)
	if codes[IssueCodeBlobOverLimit] != 1 {
		t.Fatalf("issue codes = %v, want exactly one %q", codes, IssueCodeBlobOverLimit)
	}
	for _, is := range issues {
		// Issue details round-trip through JSONB: numbers read back as float64.
		if is.Code == IssueCodeBlobOverLimit && is.Details["length"].(float64) != float64(40<<20) {
			t.Fatalf("oversize detail = %v", is.Details)
		}
	}
	if res.Verified != 0 || res.AssumedSingle != 1 {
		t.Fatalf("verified %d assumed %d, want 0/1 (only the oversized row failed)", res.Verified, res.AssumedSingle)
	}

	// Pass 2 (tiny limits, fresh run): every blob is over the cap — same
	// typed finding, still no content reads.
	tiny := codec.DefaultLimits()
	tiny.MaxStoredBytes = 2
	res2, err := verifyDecoded(t, context.Background(), tgt, func(in *VerifyDecodeInput) {
		in.Limits = &tiny
	})
	if !errors.Is(err, ErrVerificationFindings) {
		t.Fatalf("error = %v, want ErrVerificationFindings", err)
	}
	_ = res2
	_, issues2 := latestDecodeRun(t, tgt)
	codes2 := issueCodes(issues2)
	if codes2[IssueCodeBlobOverLimit] != 1 { // only torrent 1 has a blob
		t.Fatalf("tiny-limit issue codes = %v, want one blob_over_limit", codes2)
	}
}

// randomASCIIPath builds a high-entropy path (brotli-resistant) of the
// requested byte length.
func randomASCIIPath(t *testing.T, n int) string {
	t.Helper()
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, n)
	for i, b := range raw {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}

func seedBlobbyTorrent(t *testing.T, dsn string, id int32, files int, pathBytes int) {
	t.Helper()
	total := int64(0)
	for i := 0; i < files; i++ {
		total += int64(1 + i)
	}
	seedSourceTorrent(t, dsn, id, fmt.Sprintf("blobby%d", id), total, 1700000000+id%1000)
	for i := 0; i < files; i++ {
		seedSourceFile(t, dsn, id, fmt.Sprintf("d%d/%s", i, randomASCIIPath(t, pathBytes)), int64(1+i))
	}
}

func blobLen(t *testing.T, dsn string, torrentID int32) int64 {
	t.Helper()
	pool := poolForRunner(t, dsn)
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT octet_length(data) FROM magneticod.torrent_file_blobs WHERE torrent_id = $1`, torrentID).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestVerifyDecodeSoloOversizedAdmission(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	// One torrent whose encoded blob exceeds the 1 MiB budget but stays
	// well within the codec cap.
	seedBlobbyTorrent(t, src, 1, 60, 50_000)
	importAll(t, src, tgt)

	length := blobLen(t, tgt, 1)
	if length <= 1<<20 || length > 32<<20 {
		t.Fatalf("fixture assumption broken: blob = %d bytes, want >1MiB and ≤32MiB", length)
	}

	res, err := verifyDecoded(t, context.Background(), tgt, func(in *VerifyDecodeInput) {
		in.MaxInflightBytes = 1 << 20
		in.BatchSize = 10
	})
	if err != nil {
		t.Fatalf("VerifyDecode() error = %v (a valid over-budget blob must be admitted solo, never skipped)", err)
	}
	if !res.Pass || res.Verified != 1 {
		t.Fatalf("pass = %v verified = %d, want a clean pass verifying the solo blob", res.Pass, res.Verified)
	}
}

func TestVerifyDecodeCollectivelyOverBudgetSplits(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	// Three individually-legal blobs that TOGETHER exceed the budget: the
	// fetch must split into multiple bounded chunks, not skip or die.
	for id := int32(1); id <= 3; id++ {
		seedBlobbyTorrent(t, src, id, 12, 50_000)
	}
	importAll(t, src, tgt)

	total := int64(0)
	for id := int32(1); id <= 3; id++ {
		total += blobLen(t, tgt, id)
	}
	if total <= 1<<20 {
		t.Fatalf("fixture assumption broken: combined blobs = %d, want >1MiB", total)
	}

	res, err := verifyDecoded(t, context.Background(), tgt, func(in *VerifyDecodeInput) {
		in.MaxInflightBytes = 1 << 20
		in.BatchSize = 10
	})
	if err != nil {
		t.Fatalf("VerifyDecode() error = %v", err)
	}
	if !res.Pass || res.Verified != 3 {
		t.Fatalf("pass = %v verified = %d, want a clean pass over all three", res.Pass, res.Verified)
	}
}

func TestVerifyDecodeInterruptResumes(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	for id := int32(1); id <= 3; id++ {
		seedSourceTorrent(t, src, id, fmt.Sprintf("t%d.bin", id), int64(id), 1700000000+id)
		seedSourceFile(t, src, id, fmt.Sprintf("t%d.bin", id), int64(id)) // typical singles
	}
	importAll(t, src, tgt)
	// Corrupt the middle torrent structurally (Reconcile-neutral): a
	// 2-byte infohash fails production validation on its implied single —
	// an invalid_metadata finding in page 2.
	execTargetSQL(t, tgt, `UPDATE magneticod.torrents SET info_hash = decode('AA','hex') WHERE id = 2`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := true
	var seq1 int64
	res1, err := verifyDecoded(t, ctx, tgt, func(in *VerifyDecodeInput) {
		in.BatchSize = 1
		in.Progress = func(p VerifyProgress) {
			// Cancel right after the first page commits.
			if first && p.Checkpoint > 0 {
				first = false
				cancel()
			}
		}
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v, want ErrInterrupted", err)
	}
	if res1 != nil {
		t.Fatalf("interrupted pass returned a result: %+v", res1)
	}

	pool := poolForRunner(t, tgt)
	interrupted, err := state.LatestVerification(context.Background(), pool, state.VerificationKindDecode)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.State != state.VerificationInterrupted || interrupted.TargetCheckpoint == nil ||
		*interrupted.TargetCheckpoint != 1 {
		t.Fatalf("interrupted run = %s checkpoint %v, want interrupted at 1",
			interrupted.State, interrupted.TargetCheckpoint)
	}
	seq1 = interrupted.Seq

	// Resume: the pass adopts in place and completes with exactly one
	// finding — the settled page is not rescanned, its issues not duplicated.
	res2, err := verifyDecoded(t, context.Background(), tgt, func(in *VerifyDecodeInput) {
		in.BatchSize = 1
	})
	if !errors.Is(err, ErrVerificationFindings) {
		t.Fatalf("resume error = %v, want ErrVerificationFindings", err)
	}
	if !res2.Resumed {
		t.Fatal("resume did not adopt the interrupted pass")
	}
	if res2.VerificationID != interrupted.VerificationID {
		t.Fatalf("resumed into %s, want %s", res2.VerificationID, interrupted.VerificationID)
	}
	if res2.ObservedTargets != 3 || res2.Mismatched != 1 || res2.Verified != 0 || res2.AssumedSingle != 2 {
		t.Fatalf("resumed result = observed %d mismatch %d verified %d assumed %d, want 3/1/0/2",
			res2.ObservedTargets, res2.Mismatched, res2.Verified, res2.AssumedSingle)
	}
	run, issues := latestDecodeRun(t, tgt)
	if run.Seq != seq1 || run.State != state.VerificationCompletedFail {
		t.Fatalf("run = seq %d state %s, want seq %d completed_fail", run.Seq, run.State, seq1)
	}
	if len(issues) != 1 || issues[0].Code != IssueCodeInvalidMetadata {
		t.Fatalf("issues = %+v, want exactly one invalid_metadata", issues)
	}
}

func TestVerifyDecodeRepeatedInvocationCreatesNewRun(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	seedSourceTorrent(t, src, 1, "t.bin", 5, 1700000001)
	seedSourceFile(t, src, 1, "t.bin", 5)
	importAll(t, src, tgt)

	if _, err := verifyDecoded(t, context.Background(), tgt, nil); err != nil {
		t.Fatalf("first pass error = %v", err)
	}
	res2, err := verifyDecoded(t, context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("second pass error = %v", err)
	}
	if res2.Resumed {
		t.Fatal("a completed run must never be resumed")
	}
	run, _ := latestDecodeRun(t, tgt)
	if run.VerificationID != res2.VerificationID || run.Seq != res2.Seq {
		t.Fatalf("latest run = seq %d id %s, want the second invocation's (seq %d id %s)",
			run.Seq, run.VerificationID, res2.Seq, res2.VerificationID)
	}
	// Both passes survive as history; the latest is authoritative.
	pool := poolForRunner(t, tgt)
	count := 0
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM magnetico_migration.verification_runs WHERE kind = 'decode'`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("decode runs = %d, want 2 (history preserved)", count)
	}
}

func TestVerifyDecodePassesWithLegacyErrors(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	seedSourceTorrent(t, src, 1, "good.bin", 5, 1700000001)
	seedSourceFile(t, src, 1, "good.bin", 5)
	// A record the import refuses (negative file size): import completes
	// WITH errors; the decode pass still covers every TARGET row.
	seedSourceTorrent(t, src, 2, "bad.bin", 5, 1700000002)
	seedSourceFile(t, src, 2, "bad.bin", -1)
	importAll(t, src, tgt)

	snap := snapshotTarget(t, tgt)
	if snap.progress.Phase != state.PhaseCompleteWithErrors || snap.recordErrors != 1 {
		t.Fatalf("fixture: phase %s record errors %d, want with_errors/1",
			snap.progress.Phase, snap.recordErrors)
	}

	res, err := verifyDecoded(t, context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("VerifyDecode() error = %v (a decode pass may succeed while legacy import errors remain)", err)
	}
	if !res.Pass || res.ObservedTargets != 1 {
		t.Fatalf("pass = %v observed %d, want pass over the single target row", res.Pass, res.ObservedTargets)
	}
	if res.UnresolvedRecordErrors != 1 {
		t.Fatalf("unresolved record errors = %d, want 1 (reported, not conflated)", res.UnresolvedRecordErrors)
	}
}

func TestVerifyDecodeRefusesIncompleteImport(t *testing.T) {
	t.Parallel()

	t.Run("no run state", func(t *testing.T) {
		t.Parallel()
		tgt := createTargetForRunner(t)
		_, err := verifyDecoded(t, context.Background(), tgt, nil)
		if !errors.Is(err, target.ErrRunNotComplete) {
			t.Fatalf("error = %v, want ErrRunNotComplete", err)
		}
	})

	t.Run("import still running", func(t *testing.T) {
		t.Parallel()
		tgt := createTargetForRunner(t)
		pool := poolForRunner(t, tgt)
		if _, err := state.InitRun(context.Background(), pool, state.InitInput{
			RunID: "55555555-5555-4555-8555-555555555555", FingerprintRaw: []byte(`{}`),
			SourceAdapter: SourceAdapterOriginal, ContractVersion: MigrationContractVersion,
			CodecProfile: "compact", ExpectedTotal: 1, SourceMinID: 1, SourceMaxID: 1,
			DeferIndexes: false, Options: state.Options{Workers: 1, BatchSize: 1, MaxInflightBytes: 1 << 20},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := verifyDecoded(t, context.Background(), tgt, nil)
		if !errors.Is(err, target.ErrRunNotComplete) {
			t.Fatalf("error = %v, want ErrRunNotComplete", err)
		}
	})
}

// TestVerifyDecodeRowClassifiers covers the row-level verdict classes a
// preflight-passing target cannot exhibit end-to-end (Reconcile refuses the
// counter/relational states that would produce them): the classifiers stay
// exercised directly as defense in depth.
func TestVerifyDecodeRowClassifiers(t *testing.T) {
	t.Parallel()

	limits := codec.DefaultLimits()
	hash := make([]byte, 20)
	for i := range hash {
		hash[i] = byte(i)
	}
	baseRow := func() decodeRow {
		return decodeRow{id: 1, infoHash: hash, name: "n", totalSize: 5, discoveredOn: 1700000001, fileCount: 1}
	}

	// missing_blob: file_count != 1 with no blob row.
	r := baseRow()
	r.fileCount = 3
	v := verifyNoBlobRow(&r, limits)
	if v.outcome != outcomeMismatch || len(v.issues) != 1 || v.issues[0].Code != IssueCodeMissingBlob {
		t.Fatalf("missing_blob verdict = %+v", v)
	}

	// page-level anomaly branches (no fetchable blobs → the pool is unused).
	page := func(r decodeRow) *decodePage { return &decodePage{rows: []decodeRow{r}} }

	r = baseRow()
	r.hasBlobRow = true
	r.blobLen = nil
	res, err := processDecodePage(context.Background(), nil, page(r), limits, 1, 1<<20)
	if err != nil {
		t.Fatalf("null-data page error = %v", err)
	}
	if res.mismatch != 1 || len(res.issues) != 1 || res.issues[0].Code != IssueCodeBlobNullData {
		t.Fatalf("null-data verdict = %+v", res)
	}

	r = baseRow()
	r.hasBlobRow = true
	over := int64(limits.MaxStoredBytes) + 1
	r.blobLen = &over
	res, err = processDecodePage(context.Background(), nil, page(r), limits, 1, 1<<20)
	if err != nil {
		t.Fatalf("over-limit page error = %v", err)
	}
	if res.mismatch != 1 || len(res.issues) != 1 || res.issues[0].Code != IssueCodeBlobOverLimit {
		t.Fatalf("over-limit verdict = %+v", res)
	}
	if res.issues[0].Details["length"].(int64) != over || res.issues[0].Details["allowed"].(int) != limits.MaxStoredBytes {
		t.Fatalf("over-limit details = %v", res.issues[0].Details)
	}

	// blob rows: verified / noncanonical / wire_version / file-count.
	enc := codec.NewEncoder(codec.DefaultProfile(), codec.DefaultLimits(), 1)
	atypical, _, err := enc.Encode([]codec.File{{Path: "other.bin", Size: 5}})
	if err != nil {
		t.Fatal(err)
	}
	typical, _, err := enc.Encode([]codec.File{{Path: "n", Size: 5}})
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := enc.Encode([]codec.File{{Path: "a", Size: 2}, {Path: "b", Size: 3}})
	if err != nil {
		t.Fatal(err)
	}

	r = baseRow()
	if v := verifyBlobRow(&r, atypical, limits); v.outcome != outcomeVerified {
		t.Fatalf("atypical single verdict = %+v, want verified", v)
	}
	if v := verifyBlobRow(&r, typical, limits); v.outcome != outcomeMismatch ||
		v.issues[0].Code != IssueCodeNoncanonical {
		t.Fatalf("typical-single-with-blob verdict = %+v, want noncanonical", v)
	}
	if v := verifyBlobRow(&r, []byte{0x00, 0x01, 0x02, 0x03}, limits); v.outcome != outcomeMismatch ||
		v.issues[0].Code != "wire_version" {
		t.Fatalf("garbage blob verdict = %+v, want wire_version", v)
	}
	r.fileCount = 3
	if v := verifyBlobRow(&r, two, limits); v.outcome != outcomeMismatch ||
		v.issues[0].Code != IssueCodeFileCount {
		t.Fatalf("file-count verdict = %+v, want file_count_mismatch", v)
	}

	// assumed single: file_count==1, no blob, valid metadata.
	good := baseRow()
	v = verifyNoBlobRow(&good, limits)
	if v.outcome != outcomeAssumed || len(v.issues) != 0 {
		t.Fatalf("assumed-single verdict = %+v", v)
	}
	// ...but malformed metadata must produce a finding, not a pass.
	bad := baseRow()
	bad.totalSize = -5
	if v := verifyNoBlobRow(&bad, limits); v.outcome != outcomeMismatch ||
		v.issues[0].Code != IssueCodeInvalidMetadata {
		t.Fatalf("malformed single verdict = %+v, want invalid_metadata", v)
	}
}

// seedDecodeOrphanBlob installs one orphan blob in the target: drop the
// FK (exactly the tamper the extras phase exists to detect), insert a
// blob with no owning torrent, and keep the preflight green — Reconcile
// counts raw blob rows.
func seedDecodeOrphanBlob(t *testing.T, tgt string) {
	t.Helper()
	execTargetSQL(t, tgt, `ALTER TABLE magneticod.torrent_file_blobs
        DROP CONSTRAINT torrent_file_blobs_torrent_id_fkey`)
	execTargetSQL(t, tgt,
		`INSERT INTO magneticod.torrent_file_blobs (torrent_id, data) VALUES (999999, '\x78da')`)
	execTargetSQL(t, tgt, `UPDATE magnetico_migration.progress SET blob_count = blob_count + 1`)
}

// TestVerifyDecodeExtrasResumeKeepsOrphanCount: a pass interrupted right
// after the extras checkpoint must still report the orphan count on
// resume — the durable issue is the history, and the summary must match
// it (Codex review iteration 2, finding 4).
func TestVerifyDecodeExtrasResumeKeepsOrphanCount(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	seedSourceTorrent(t, src, 1, "t1.bin", 10, 1700000001)
	seedSourceFile(t, src, 1, "t1.bin", 10)
	importAll(t, src, tgt)
	seedDecodeOrphanBlob(t, tgt)

	// Interrupt right after the extras phase commits.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := verifyDecoded(t, ctx, tgt, func(in *VerifyDecodeInput) {
		in.Progress = func(p VerifyProgress) {
			if p.Phase == state.VerificationPhaseExtras {
				cancel()
			}
		}
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v, want ErrInterrupted", err)
	}
	pool := poolForRunner(t, tgt)
	interrupted, err := state.LatestVerification(context.Background(), pool, state.VerificationKindDecode)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.State != state.VerificationInterrupted || interrupted.Phase != state.VerificationPhaseExtras {
		t.Fatalf("interrupted run = %s/%s, want interrupted/extras", interrupted.State, interrupted.Phase)
	}
	seq1 := interrupted.Seq

	// Resume: the extras scan is not re-run; the orphan count is restored
	// from the durable issue.
	res2, err := verifyDecoded(t, context.Background(), tgt, nil)
	if !errors.Is(err, ErrVerificationFindings) {
		t.Fatalf("resume error = %v, want ErrVerificationFindings", err)
	}
	if !res2.Resumed || res2.Seq != seq1 {
		t.Fatalf("resumed = %v seq %d, want the adopted run (seq %d)", res2.Resumed, res2.Seq, seq1)
	}
	if res2.OrphanTargetBlobs != 1 {
		t.Fatalf("resumed orphan count = %d, want 1 (restored from the durable issue)", res2.OrphanTargetBlobs)
	}
}
