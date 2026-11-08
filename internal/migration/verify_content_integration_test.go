package migration

// verify-content integration suite: clean full-equality pass (mixed row
// shapes, duplicate paths, source-readonly proof), every classification
// (missing/extra/mismatched metadata, missing atypical blob, unresolved
// registry membership, not-representable overflow, NULL-owned source file
// rows), source-only rows carried by their issues alone (target-side
// counters and coverage mirrors stay exact), fingerprint drift as info
// provenance (a pass WITH drift under full equality), interrupt/resume with
// Equivalent-based adoption, refusal to resume after source content-anchor
// changes, latest-run authority, and incomplete-import refusals.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
)

// verifyContent runs one verify-content pass with sane test defaults.
func verifyContent(t *testing.T, ctx context.Context, src, tgt string, tweak func(*VerifyContentInput)) (*VerificationResult, error) {
	t.Helper()
	in := VerifyContentInput{SourceDSN: src, TargetDSN: tgt, Workers: 2, BatchSize: 100}
	if tweak != nil {
		tweak(&in)
	}
	return VerifyContent(ctx, in)
}

// latestContentRun reads the authoritative content run plus its issues.
func latestContentRun(t *testing.T, tgt string) (*state.VerificationRun, []state.VerificationIssue) {
	t.Helper()
	ctx := context.Background()
	pool := poolForRunner(t, tgt)
	run, err := state.LatestVerification(ctx, pool, state.VerificationKindContent)
	if err != nil {
		t.Fatalf("LatestVerification() error = %v", err)
	}
	if run == nil {
		t.Fatal("no content verification run exists")
	}
	issues, err := state.ListVerificationIssues(ctx, pool, run.VerificationID, 1000)
	if err != nil {
		t.Fatalf("ListVerificationIssues() error = %v", err)
	}
	return run, issues
}

// sourceRowMD5 is a mutation detector for the source dataset (torrent and
// file rows), used to prove verify-content never writes the source.
func sourceRowMD5(t *testing.T, dsn string) [2]string {
	t.Helper()
	pool := poolForRunner(t, dsn)
	var torrentsMD5, filesMD5 string
	err := pool.QueryRow(context.Background(), `SELECT
        (SELECT md5(COALESCE(string_agg(x, '' ORDER BY id), '')) FROM
            (SELECT row_number() OVER (ORDER BY id)::text || to_jsonb(torrents)::text AS x, id
             FROM magneticod.torrents) d),
        (SELECT md5(COALESCE(string_agg(x, '' ORDER BY id), '')) FROM
            (SELECT row_number() OVER (ORDER BY id)::text || to_jsonb(files)::text AS x, id
             FROM magneticod.files) d2)`).
		Scan(&torrentsMD5, &filesMD5)
	if err != nil {
		t.Fatalf("source fingerprint: %v", err)
	}
	return [2]string{torrentsMD5, filesMD5}
}

// seedMatchingTargetSingle inserts a target torrent row equal to a source
// typical single (no blob) and keeps the progress counters consistent.
func seedMatchingTargetSingle(t *testing.T, src, tgt string, id int32) {
	t.Helper()
	pool := poolForRunner(t, src)
	var infoHash []byte
	var name string
	var total int64
	var discovered int64
	if err := pool.QueryRow(context.Background(),
		`SELECT info_hash, name, total_size, discovered_on FROM magneticod.torrents WHERE id = $1`, id).
		Scan(&infoHash, &name, &total, &discovered); err != nil {
		t.Fatalf("read source row %d: %v", id, err)
	}
	execTargetSQL(t, tgt, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on, file_count)
        VALUES ($1, $2, $3, $4, $5, 1)`, int64(id), infoHash, name, total, discovered)
	execTargetSQL(t, tgt, `UPDATE magnetico_migration.progress
        SET success_count = success_count + 1, scanned_count = scanned_count + 1,
            last_processed_id = GREATEST(last_processed_id, $1)`, int64(id))
}

// contentFixture seeds the standard clean source: 1 multi-file with
// duplicate paths (blob), 2 typical single (no blob), 3 atypical single
// (blob) — then imports everything.
func contentFixture(t *testing.T) (string, string) {
	t.Helper()
	src, tgt := decodeFixture(t)
	seedMultiFileTorrent(t, src, 1, "multi", [][2]any{
		{"a/one.bin", int64(10)}, {"a/one.bin", int64(20)}, {"b/two.bin", int64(30)},
	})
	seedSourceTorrent(t, src, 2, "typical.bin", 40, 1700000002)
	seedSourceFile(t, src, 2, "typical.bin", 40)
	seedSourceTorrent(t, src, 3, "atypical", 50, 1700000003)
	seedSourceFile(t, src, 3, "different.bin", 50)
	importAll(t, src, tgt)
	return src, tgt
}

// TestVerifyContentDecodesPerRowUnderTinyBudget: the lazy content window
// decodes one budget-bounded chunk at a time as the merge walks the page —
// never the whole decoded page at once. Two over-budget blobs (each a solo
// chunk at a 1 MiB budget) with a no-blob row between them: the merge must
// cross both chunk boundaries, resolve the between-chunk row by
// reconstruction, and classify identically to the page-wide baseline
// (Codex review iteration 1, finding 5).
func TestVerifyContentDecodesPerRowUnderTinyBudget(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	seedBlobbyTorrent(t, src, 1, 60, 50_000)
	seedSourceTorrent(t, src, 2, "typical.bin", 40, 1700000002)
	seedSourceFile(t, src, 2, "typical.bin", 40)
	seedBlobbyTorrent(t, src, 3, 60, 50_000)
	importAll(t, src, tgt)

	if n := blobLen(t, tgt, 1); n <= 1<<20 {
		t.Fatalf("fixture assumption broken: blob = %d bytes, want >1MiB", n)
	}

	res, err := verifyContent(t, context.Background(), src, tgt, func(in *VerifyContentInput) {
		in.MaxInflightBytes = 1 << 20
	})
	if err != nil {
		t.Fatalf("VerifyContent() error = %v", err)
	}
	if !res.Pass || res.Verified != 3 || res.ObservedTargets != 3 || res.SourceObserved != 3 {
		t.Fatalf("result = pass %v %d/%d/%d, want clean 3/3/3",
			res.Pass, res.Verified, res.ObservedTargets, res.SourceObserved)
	}
}

func TestVerifyContentCleanPass(t *testing.T) {
	t.Parallel()

	src, tgt := contentFixture(t)

	srcBefore := sourceRowMD5(t, src)
	tgtBefore := targetFingerprint(t, tgt)
	res, err := verifyContent(t, context.Background(), src, tgt, nil)
	if err != nil {
		t.Fatalf("VerifyContent() error = %v", err)
	}
	if !res.Pass {
		t.Fatalf("pass = false, issues %+v", res.IssuesBySeverity)
	}
	if res.Kind != state.VerificationKindContent {
		t.Fatalf("kind = %s, want content", res.Kind)
	}
	if res.ObservedTargets != 3 || res.SourceObserved != 3 || res.Verified != 3 {
		t.Fatalf("result = observed %d source %d verified %d, want 3/3/3",
			res.ObservedTargets, res.SourceObserved, res.Verified)
	}
	if res.AssumedSingle != 0 {
		t.Fatalf("assumed = %d, want 0 (content compares every matched row)", res.AssumedSingle)
	}
	if res.UnresolvedRecordErrors != 0 || res.UnresolvedIntegrity != 0 {
		t.Fatalf("registry counts = %d/%d, want 0/0", res.UnresolvedRecordErrors, res.UnresolvedIntegrity)
	}
	// Neither database is modified by the pass.
	if tgtBefore != targetFingerprint(t, tgt) {
		t.Fatal("target mutated by the content pass")
	}
	if srcBefore != sourceRowMD5(t, src) {
		t.Fatal("source mutated by the content pass")
	}

	run, issues := latestContentRun(t, tgt)
	if run.State != state.VerificationCompletedPass || run.Phase != state.VerificationPhaseComplete {
		t.Fatalf("run = %s/%s, want completed_pass/complete", run.State, run.Phase)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %+v, want none", issues)
	}
	if run.ObservedSourceIDs == nil || *run.ObservedSourceIDs != 3 ||
		run.ObservedTargetIDs == nil || *run.ObservedTargetIDs != 3 {
		t.Fatalf("observed coverage = %v/%v, want 3/3", run.ObservedSourceIDs, run.ObservedTargetIDs)
	}
}

func TestVerifyContentEmptyBothSidesPass(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	importAll(t, src, tgt) // empty source, empty target

	res, err := verifyContent(t, context.Background(), src, tgt, nil)
	if err != nil {
		t.Fatalf("VerifyContent() error = %v", err)
	}
	if !res.Pass || res.ObservedTargets != 0 || res.SourceObserved != 0 {
		t.Fatalf("result = pass %v observed %d/%d, want pass with 0/0",
			res.Pass, res.ObservedTargets, res.SourceObserved)
	}
}

func TestVerifyContentFindingsFail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		seed         func(t *testing.T, src, tgt string) // before import (optional)
		corrupt      func(t *testing.T, src, tgt string)
		wantCode     string
		wantSeverity string
		wantMismatch int64
		wantField    string // non-empty: the issue details must name this field
	}{
		{
			name: "tampered stored file_count is a content mismatch naming the field",
			corrupt: func(t *testing.T, src, tgt string) {
				// The blob still decodes to the exact source file list, but
				// the stored count desyncs from it — the compare must catch
				// the column, not just the decoded content.
				execTargetSQL(t, tgt, `UPDATE magneticod.torrents SET file_count = 9 WHERE id = 1`)
			},
			wantCode:     IssueCodeContentMismatch,
			wantSeverity: state.IssueMismatch,
			wantMismatch: 1,
			wantField:    "file_count",
		},
		{
			name: "missing target row not in the registry is missing_target_row",
			corrupt: func(t *testing.T, src, tgt string) {
				execTargetSQL(t, tgt, `DELETE FROM magneticod.torrents WHERE id = 2`)
				execTargetSQL(t, tgt, `UPDATE magnetico_migration.progress
                    SET success_count = success_count - 1, scanned_count = scanned_count - 1`)
			},
			wantCode:     IssueCodeMissingTargetRow,
			wantSeverity: state.IssueMismatch,
			// Source-only rows carry their severity on the issue alone; the
			// mismatch COUNTER stays target-side (0 here — the two surviving
			// rows verify).
			wantMismatch: 0,
		},
		{
			name: "extra target row is extra_target_row",
			corrupt: func(t *testing.T, src, tgt string) {
				// The fixture's source has an ID gap at 3; a counter-
				// consistent extra target row must stay inside the recorded
				// source ID range (state.Load refuses anything beyond it).
				execTargetSQL(t, tgt, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on, file_count)
                    VALUES (3, decode('6333','hex'), 'extra', 5, 1700000003, 1)`)
				execTargetSQL(t, tgt, `UPDATE magnetico_migration.progress
                    SET success_count = success_count + 1, scanned_count = scanned_count + 1`)
			},
			wantCode:     IssueCodeExtraTargetRow,
			wantSeverity: state.IssueMismatch,
			wantMismatch: 1,
		},
		{
			name: "name tamper is a content mismatch",
			corrupt: func(t *testing.T, src, tgt string) {
				execTargetSQL(t, tgt, `UPDATE magneticod.torrents SET name = 'renamed' WHERE id = 1`)
			},
			wantCode:     IssueCodeContentMismatch,
			wantSeverity: state.IssueMismatch,
			wantMismatch: 1,
		},
		{
			name: "atypical single missing its blob is a content mismatch",
			corrupt: func(t *testing.T, src, tgt string) {
				// The reconstructed no-blob single (name, total) can never
				// equal the source's atypical file list.
				execTargetSQL(t, tgt, `DELETE FROM magneticod.torrent_file_blobs WHERE torrent_id = 4`)
				execTargetSQL(t, tgt, `UPDATE magnetico_migration.progress SET blob_count = blob_count - 1`)
			},
			wantCode:     IssueCodeContentMismatch,
			wantSeverity: state.IssueMismatch,
			wantMismatch: 1,
		},
		{
			name: "unresolved registry membership is unresolved_record_error",
			seed: func(t *testing.T, src, tgt string) {
				// 1-byte info hash: invalid metadata the import must reject.
				execSourceSQL(t, src, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
                    VALUES (5, decode('AA','hex'), 'bad-hash', 9, 1700000005)`)
				seedSourceFile(t, src, 5, "whatever.bin", 9)
			},
			corrupt:      func(t *testing.T, src, tgt string) {},
			wantCode:     IssueCodeUnresolvedRecord,
			wantSeverity: state.IssueCoverage,
			wantMismatch: 0,
		},
		{
			name: "source file list over production limits is not representable",
			corrupt: func(t *testing.T, src, tgt string) {
				// Tiny limits applied at VERIFY time: the already-imported
				// multi-file torrent's source list no longer fits → drained,
				// classified unverifiable, never claimed compared.
			},
			wantCode:     IssueCodeNotRepresentable,
			wantSeverity: state.IssueUnverifiable,
			wantMismatch: 0,
		},
		{
			name: "NULL-owned source file row is drift plus an integrity finding",
			corrupt: func(t *testing.T, src, tgt string) {
				execSourceSQL(t, src, `INSERT INTO magneticod.files (torrent_id, path, size)
                    VALUES (NULL, 'dangling.bin', 5)`)
			},
			wantCode:     IssueCodeSourceIntegrity,
			wantSeverity: state.IssueCoverage,
			wantMismatch: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src, tgt := decodeFixture(t)
			if tc.seed != nil {
				tc.seed(t, src, tgt)
			}
			// Source IDs {1, 2, 4}: a gap at 3 leaves room for
			// counter-consistent in-range extra-row tampering.
			seedMultiFileTorrent(t, src, 1, "multi", [][2]any{
				{"a/one.bin", int64(10)}, {"a/one.bin", int64(20)}, {"b/two.bin", int64(30)},
			})
			seedSourceTorrent(t, src, 2, "typical.bin", 40, 1700000002)
			seedSourceFile(t, src, 2, "typical.bin", 40)
			seedSourceTorrent(t, src, 4, "atypical", 50, 1700000004)
			seedSourceFile(t, src, 4, "different.bin", 50)
			importAll(t, src, tgt)

			tc.corrupt(t, src, tgt)

			tweak := func(in *VerifyContentInput) {}
			if tc.wantCode == IssueCodeNotRepresentable {
				tweak = func(in *VerifyContentInput) {
					limits := codec.DefaultLimits()
					limits.MaxFiles = 1
					in.Limits = &limits
				}
			}
			res, err := verifyContent(t, context.Background(), src, tgt, tweak)
			if err == nil || !errors.Is(err, ErrVerificationFindings) {
				t.Fatalf("error = %v, want ErrVerificationFindings", err)
			}
			if res.Pass {
				t.Fatal("pass = true, want completed_fail")
			}

			run, issues := latestContentRun(t, tgt)
			if run.State != state.VerificationCompletedFail {
				t.Fatalf("run state = %s, want completed_fail", run.State)
			}
			codes := issueCodes(issues)
			if codes[tc.wantCode] != 1 {
				t.Fatalf("issues = %+v, want exactly one %s", issues, tc.wantCode)
			}
			for _, is := range issues {
				if is.Code == tc.wantCode && is.Severity != tc.wantSeverity {
					t.Fatalf("%s severity = %s, want %s", is.Code, is.Severity, tc.wantSeverity)
				}
			}
			if tc.wantField != "" {
				for _, is := range issues {
					if is.Code != tc.wantCode {
						continue
					}
					b, merr := json.Marshal(is.Details)
					if merr != nil {
						t.Fatal(merr)
					}
					if !strings.Contains(string(b), tc.wantField) {
						t.Fatalf("%s details = %s, want the %s field named", tc.wantCode, b, tc.wantField)
					}
				}
			}
			if res.Mismatched != tc.wantMismatch {
				t.Fatalf("mismatched = %d, want %d", res.Mismatched, tc.wantMismatch)
			}

			// Classification-specific accounting.
			switch tc.wantCode {
			case IssueCodeUnresolvedRecord:
				if res.UnresolvedRecordErrors != 1 || res.SourceObserved != 4 {
					t.Fatalf("registry %d source %d, want 1/4", res.UnresolvedRecordErrors, res.SourceObserved)
				}
			case IssueCodeNotRepresentable:
				if res.Unverifiable != 1 {
					t.Fatalf("unverifiable = %d, want 1", res.Unverifiable)
				}
			case IssueCodeSourceIntegrity:
				if res.IssuesBySeverity[state.IssueInfo] != 1 {
					t.Fatalf("info issues = %d, want 1 (the drift provenance)", res.IssuesBySeverity[state.IssueInfo])
				}
			}
		})
	}
}

// TestVerifyContentSourceOnlyRowsStayOutOfTargetCounters: a source row with
// no target row (deleted mid-range → merge phase; valid row beyond the last
// target ID → drain phase) is carried by its missing_target_row issue alone.
// It must never move the target-side counters or coverage mirrors — otherwise
// observed_target_ids exceeds the target's actual row count and finalize
// grows a spurious coverage finding (Codex review iteration 1, finding 4).
func TestVerifyContentSourceOnlyRowsStayOutOfTargetCounters(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	seedMultiFileTorrent(t, src, 1, "multi", [][2]any{
		{"a/one.bin", int64(10)}, {"a/one.bin", int64(20)}, {"b/two.bin", int64(30)},
	})
	seedSourceTorrent(t, src, 2, "typical.bin", 40, 1700000002)
	seedSourceFile(t, src, 2, "typical.bin", 40)
	seedSourceTorrent(t, src, 4, "atypical", 50, 1700000004)
	seedSourceFile(t, src, 4, "different.bin", 50)
	importAll(t, src, tgt)

	// Merge-phase source-only row: target row 2 deleted mid-range.
	execTargetSQL(t, tgt, `DELETE FROM magneticod.torrents WHERE id = 2`)
	execTargetSQL(t, tgt, `UPDATE magnetico_migration.progress
        SET success_count = success_count - 1, scanned_count = scanned_count - 1`)
	// Drain-phase source-only row: a valid source row beyond the last target
	// ID. Seeding it after the import makes the recorded import fingerprint
	// stale — expected drift provenance (info), not a blocker.
	seedSourceTorrent(t, src, 5, "late.bin", 60, 1700000005)
	seedSourceFile(t, src, 5, "late.bin", 60)

	res, err := verifyContent(t, context.Background(), src, tgt, nil)
	if err == nil || !errors.Is(err, ErrVerificationFindings) {
		t.Fatalf("error = %v, want ErrVerificationFindings", err)
	}
	if res.Pass {
		t.Fatal("pass = true, want completed_fail")
	}
	// Target-side accounting: exactly the two surviving target rows.
	if res.ObservedTargets != 2 || res.Verified != 2 || res.Mismatched != 0 {
		t.Fatalf("result = observed %d verified %d mismatched %d, want 2/2/0",
			res.ObservedTargets, res.Verified, res.Mismatched)
	}
	// All four source rows classified, none folded into target counters.
	if res.SourceObserved != 4 {
		t.Fatalf("source observed = %d, want 4", res.SourceObserved)
	}

	run, issues := latestContentRun(t, tgt)
	if run.State != state.VerificationCompletedFail {
		t.Fatalf("run state = %s, want completed_fail", run.State)
	}
	// The durable mirrors equal the in-memory result (target rows only).
	if run.ObservedTargetIDs == nil || *run.ObservedTargetIDs != 2 ||
		run.ObservedSourceIDs == nil || *run.ObservedSourceIDs != 4 {
		t.Fatalf("durable coverage = %v/%v, want 2/4", run.ObservedTargetIDs, run.ObservedSourceIDs)
	}
	if run.MismatchCount != 0 || run.VerifiedCount != 2 {
		t.Fatalf("durable counters = mismatch %d verified %d, want 0/2",
			run.MismatchCount, run.VerifiedCount)
	}
	// Exactly two missing_target_row issues plus the drift provenance — and
	// NO coverage-severity finding (target coverage is exact).
	codes := issueCodes(issues)
	if len(issues) != 3 || codes[IssueCodeMissingTargetRow] != 2 || codes[IssueCodeSourceDrift] != 1 {
		t.Fatalf("issues = %+v, want two missing_target_row + one drift", issues)
	}
	if codes[IssueCodeCoverage] != 0 {
		t.Fatalf("issues = %+v, want no coverage finding", issues)
	}
	for _, is := range issues {
		if is.Code == IssueCodeMissingTargetRow && is.Severity != state.IssueMismatch {
			t.Fatalf("%s severity = %s, want mismatch", is.Code, is.Severity)
		}
	}
}

// TestVerifyContentVanishedSourceRecordFailsCoverage: an unresolved
// record-scope registry entry whose source row has vanished is never seen
// by the merge, so it gets no per-row unresolved_record_error issue — the
// finalize aggregate check must emit a coverage finding for it instead of
// letting full equality elsewhere pass the scan (Codex review iteration 1,
// finding 1).
func TestVerifyContentVanishedSourceRecordFailsCoverage(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	// Row 1 invalid → the registry holds entry 1; rows 2 and 3 import.
	execSourceSQL(t, src, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        VALUES (1, decode('AA','hex'), 'bad1', 9, 1700000001)`)
	seedSourceTorrent(t, src, 2, "ok2.bin", 40, 1700000002)
	seedSourceFile(t, src, 2, "ok2.bin", 40)
	seedSourceTorrent(t, src, 3, "ok3.bin", 50, 1700000003)
	seedSourceFile(t, src, 3, "ok3.bin", 50)
	importAll(t, src, tgt)

	// The registry entry's source row disappears before verification.
	execSourceSQL(t, src, `DELETE FROM magneticod.torrents WHERE id = 1`)

	res, err := verifyContent(t, context.Background(), src, tgt, nil)
	if err == nil || !errors.Is(err, ErrVerificationFindings) {
		t.Fatalf("error = %v, want ErrVerificationFindings", err)
	}
	if res.Pass {
		t.Fatal("pass = true, want completed_fail with an unaccounted unresolved record")
	}
	// The surviving rows compare equal; the failure is the coverage gap.
	if res.Verified != 2 || res.Mismatched != 0 || res.SourceObserved != 2 || res.ObservedTargets != 2 {
		t.Fatalf("result = %d/%d/%d/%d, want 2/0/2/2",
			res.Verified, res.Mismatched, res.SourceObserved, res.ObservedTargets)
	}
	if res.UnresolvedRecordErrors != 1 {
		t.Fatalf("unresolved record errors = %d, want 1 (report-only count)", res.UnresolvedRecordErrors)
	}

	run, issues := latestContentRun(t, tgt)
	if run.State != state.VerificationCompletedFail {
		t.Fatalf("run state = %s, want completed_fail", run.State)
	}
	codes := issueCodes(issues)
	if codes[IssueCodeUnresolvedCoverage] != 1 {
		t.Fatalf("issues = %+v, want exactly one %s", issues, IssueCodeUnresolvedCoverage)
	}
	if codes[IssueCodeUnresolvedRecord] != 0 || codes[IssueCodeMissingTargetRow] != 0 {
		t.Fatalf("issues = %+v, want no per-row registry/missing findings", issues)
	}
	for _, is := range issues {
		if is.Code != IssueCodeUnresolvedCoverage {
			continue
		}
		if is.Severity != state.IssueCoverage {
			t.Fatalf("%s severity = %s, want coverage", is.Code, is.Severity)
		}
		// Details round-trip through JSONB: numbers come back as float64.
		if is.Details["uncovered"] != float64(1) || is.Details["min_id"] != float64(1) || is.Details["max_id"] != float64(1) {
			t.Fatalf("%s details = %+v, want uncovered/min/max = 1/1/1", is.Code, is.Details)
		}
	}
}

// TestVerifyContentBoundedUnitsFlushMidPageAndMidWindow: with a small
// BatchSize, source-only stretches are committed in bounded units — mid-page
// (target checkpoint at the last classified target row, source-only prefix
// flushes keep it certified) and mid-drain-window (source checkpoint moves,
// target checkpoint stays) — instead of piling a whole sparse-target prefix
// or drain window into one commit. Resume across a mid-page flush boundary
// must neither re-classify nor double-count (Codex review iteration 1,
// finding 7).
func TestVerifyContentBoundedUnitsFlushMidPageAndMidWindow(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	// Rows 1 and 10 import; rows 2..9 are rejected into the registry — a
	// dense source-only stretch between two far-apart target rows. One
	// target page [1,10]; the prefix of row 10 spans registry rows 2..9.
	seedSourceTorrent(t, src, 1, "ok1.bin", 40, 1700000001)
	seedSourceFile(t, src, 1, "ok1.bin", 40)
	seedSourceTorrent(t, src, 10, "ok10.bin", 50, 1700000010)
	seedSourceFile(t, src, 10, "ok10.bin", 50)
	for id := int32(2); id <= 9; id++ {
		execSourceSQL(t, src, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
            VALUES ($1, decode($2, 'hex'), 'bad', 9, 1700000000 + $1)`, id, fmt.Sprintf("%02dAA", id%100))
	}
	importAll(t, src, tgt)

	// Post-import source growth: rows 11 and 12 drain as missing_target_row
	// (never in the registry). Seeding after the import makes the recorded
	// import fingerprint stale — expected drift provenance (info).
	seedSourceTorrent(t, src, 11, "late11.bin", 60, 1700000011)
	seedSourceFile(t, src, 11, "late11.bin", 60)
	seedSourceTorrent(t, src, 12, "late12.bin", 60, 1700000012)
	seedSourceFile(t, src, 12, "late12.bin", 60)

	// Phase A — interrupt exactly at the first mid-page flush: it must carry
	// the last classified target row (1) as its target checkpoint while the
	// source checkpoint already covers the classified prefix (rows 1..3 —
	// row 1 matched, rows 2 and 3 source-only).
	canceled := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := verifyContent(t, ctx, src, tgt, func(in *VerifyContentInput) {
		in.BatchSize = 2
		in.Progress = func(p VerifyProgress) {
			if !canceled && p.Checkpoint == 1 && p.SourceObserved == 3 {
				canceled = true
				cancel()
			}
		}
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v, want ErrInterrupted", err)
	}
	interrupted, err := state.LatestVerification(context.Background(), poolForRunner(t, tgt), state.VerificationKindContent)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.State != state.VerificationInterrupted ||
		interrupted.TargetCheckpoint == nil || *interrupted.TargetCheckpoint != 1 ||
		interrupted.SourceCheckpoint == nil || *interrupted.SourceCheckpoint != 3 {
		t.Fatalf("run = %s checkpoints src %v tgt %v, want interrupted at src 3 tgt 1",
			interrupted.State, interrupted.SourceCheckpoint, interrupted.TargetCheckpoint)
	}

	// Phase B — resume and finish. BatchSize 2 keeps the drain flush
	// mid-window: rows 11 and 12 flush inside their window (source
	// checkpoint 12) while the target checkpoint stays at the page max 10.
	sawDrainFlush := false
	res, err := verifyContent(t, context.Background(), src, tgt, func(in *VerifyContentInput) {
		in.BatchSize = 2
		in.Progress = func(p VerifyProgress) {
			if p.Checkpoint == 10 && p.SourceObserved == 12 {
				sawDrainFlush = true
			}
		}
	})
	if err == nil || !errors.Is(err, ErrVerificationFindings) {
		t.Fatalf("resume error = %v, want ErrVerificationFindings", err)
	}
	if !res.Resumed || res.VerificationID != interrupted.VerificationID {
		t.Fatalf("resume = %v/%s, want adopted run %s", res.Resumed, res.VerificationID, interrupted.VerificationID)
	}
	if !sawDrainFlush {
		t.Fatal("no mid-window drain flush observed (source checkpoint moved without the target)")
	}
	if res.Verified != 2 || res.Mismatched != 0 || res.SourceObserved != 12 || res.ObservedTargets != 2 {
		t.Fatalf("result = %d/%d/%d/%d, want 2/0/12/2",
			res.Verified, res.Mismatched, res.SourceObserved, res.ObservedTargets)
	}

	run, issues := latestContentRun(t, tgt)
	if run.State != state.VerificationCompletedFail {
		t.Fatalf("run state = %s, want completed_fail", run.State)
	}
	codes := issueCodes(issues)
	// Exactly one issue per classified anomaly across BOTH phases — no
	// re-classification, no double-counting of rows 1/10 on resume.
	if codes[IssueCodeUnresolvedRecord] != 8 || codes[IssueCodeMissingTargetRow] != 2 ||
		codes[IssueCodeSourceDrift] != 1 || len(issues) != 11 {
		t.Fatalf("issues = %+v, want 8 unresolved + 2 missing + 1 drift", issues)
	}
	if codes[IssueCodeExtraTargetRow] != 0 || codes[IssueCodeCoverage] != 0 {
		t.Fatalf("issues = %+v, want no extra_target_row (no resume double-count) and no coverage gap", issues)
	}
	if run.VerifiedCount != 2 || run.MismatchCount != 0 ||
		run.ObservedSourceIDs == nil || *run.ObservedSourceIDs != 12 ||
		run.ObservedTargetIDs == nil || *run.ObservedTargetIDs != 2 {
		t.Fatalf("durable = verified %d mismatch %d src %v tgt %v, want 2/0/12/2",
			run.VerifiedCount, run.MismatchCount, run.ObservedSourceIDs, run.ObservedTargetIDs)
	}
}

func TestVerifyContentDriftAsInfoPass(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	// Source IDs {1, 2, 4} (gap at 3): the post-import growth below can
	// then be mirrored on the target while staying inside the recorded
	// source ID range — the only counter-consistent way to simulate it
	// (preflight refuses any target row beyond the import checkpoint).
	seedMultiFileTorrent(t, src, 1, "multi", [][2]any{
		{"a/one.bin", int64(10)}, {"a/one.bin", int64(20)}, {"b/two.bin", int64(30)},
	})
	seedSourceTorrent(t, src, 2, "typical.bin", 40, 1700000002)
	seedSourceFile(t, src, 2, "typical.bin", 40)
	seedSourceTorrent(t, src, 4, "atypical", 50, 1700000004)
	seedSourceFile(t, src, 4, "different.bin", 50)
	importAll(t, src, tgt)

	// Post-import source growth mirrored on the target: full equality
	// holds, but the fresh fingerprint's content anchors (torrent count,
	// file rows) differ from the recorded import fingerprint.
	seedSourceTorrent(t, src, 3, "typical3.bin", 70, 1700000003)
	seedSourceFile(t, src, 3, "typical3.bin", 70)
	seedMatchingTargetSingle(t, src, tgt, 3)

	res, err := verifyContent(t, context.Background(), src, tgt, nil)
	if err != nil {
		t.Fatalf("VerifyContent() error = %v", err)
	}
	if !res.Pass {
		t.Fatalf("pass = false with full equality, issues %+v", res.IssuesBySeverity)
	}
	if res.ObservedTargets != 4 || res.SourceObserved != 4 || res.Verified != 4 {
		t.Fatalf("result = %d/%d/%d, want 4/4/4", res.ObservedTargets, res.SourceObserved, res.Verified)
	}

	run, issues := latestContentRun(t, tgt)
	if run.State != state.VerificationCompletedPass {
		t.Fatalf("run state = %s, want completed_pass", run.State)
	}
	if len(issues) != 1 || issues[0].Code != IssueCodeSourceDrift || issues[0].Severity != state.IssueInfo {
		t.Fatalf("issues = %+v, want exactly one info source_fingerprint_drift", issues)
	}
	if res.IssuesBySeverity[state.IssueInfo] != 1 {
		t.Fatalf("severity counts = %+v, want exactly one info", res.IssuesBySeverity)
	}
}

func TestVerifyContentInterruptResumes(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	for id := int32(1); id <= 3; id++ {
		seedSourceTorrent(t, src, id, fmt.Sprintf("t%d.bin", id), 2, 1700000000+id)
		seedSourceFile(t, src, id, fmt.Sprintf("t%d.bin", id), 2)
	}
	importAll(t, src, tgt)

	canceled := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := verifyContent(t, ctx, src, tgt, func(in *VerifyContentInput) {
		in.BatchSize = 1
		in.Progress = func(p VerifyProgress) {
			if !canceled && p.Checkpoint >= 1 {
				canceled = true
				cancel()
			}
		}
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v, want ErrInterrupted", err)
	}

	pool := poolForRunner(t, tgt)
	interrupted, err := state.LatestVerification(context.Background(), pool, state.VerificationKindContent)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.State != state.VerificationInterrupted ||
		interrupted.TargetCheckpoint == nil || *interrupted.TargetCheckpoint != 1 {
		t.Fatalf("run = %s checkpoint %v, want interrupted at 1", interrupted.State, interrupted.TargetCheckpoint)
	}

	res, err := verifyContent(t, context.Background(), src, tgt, nil)
	if err != nil {
		t.Fatalf("resume error = %v", err)
	}
	if !res.Resumed || res.VerificationID != interrupted.VerificationID {
		t.Fatalf("resume = %v/%s, want adopted run %s", res.Resumed, res.VerificationID, interrupted.VerificationID)
	}
	if !res.Pass || res.ObservedTargets != 3 || res.SourceObserved != 3 || res.Verified != 3 {
		t.Fatalf("resumed result = %v %d/%d/%d, want pass 3/3/3",
			res.Pass, res.ObservedTargets, res.SourceObserved, res.Verified)
	}
	run, issues := latestContentRun(t, tgt)
	if run.State != state.VerificationCompletedPass || len(issues) != 0 {
		t.Fatalf("run = %s issues %+v, want clean pass", run.State, issues)
	}
}

// TestVerifyContentResumeClassifiesRegistryIDsBeyondCheckpoint: on resume,
// the unresolved-registry walk starts at the source checkpoint the merge
// resumes from — entries at or below it were already consumed pre-interrupt.
// A registry entry ABOVE the checkpoint must classify its source row as
// unresolved_record_error on the resumed pass, never missing_target_row
// (Codex review iteration 1, finding 3).
func TestVerifyContentResumeClassifiesRegistryIDsBeyondCheckpoint(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	// Rows 1 and 3 invalid (1-byte hash) → rejected into the registry;
	// rows 2 and 4 import cleanly. Target = {2,4}, registry = {1,3}.
	execSourceSQL(t, src, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        VALUES (1, decode('AA','hex'), 'bad1', 9, 1700000001)`)
	seedSourceTorrent(t, src, 2, "ok2.bin", 40, 1700000002)
	seedSourceFile(t, src, 2, "ok2.bin", 40)
	execSourceSQL(t, src, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        VALUES (3, decode('AB','hex'), 'bad3', 9, 1700000003)`)
	seedSourceTorrent(t, src, 4, "ok4.bin", 50, 1700000004)
	seedSourceFile(t, src, 4, "ok4.bin", 50)
	importAll(t, src, tgt)

	// Interrupt right after the first commit: target checkpoint 2, source
	// checkpoint 2, registry entry 1 consumed and committed.
	canceled := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := verifyContent(t, ctx, src, tgt, func(in *VerifyContentInput) {
		in.BatchSize = 1
		in.Progress = func(p VerifyProgress) {
			if !canceled && p.Checkpoint >= 2 {
				canceled = true
				cancel()
			}
		}
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v, want ErrInterrupted", err)
	}
	interrupted, err := state.LatestVerification(context.Background(), poolForRunner(t, tgt), state.VerificationKindContent)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.State != state.VerificationInterrupted ||
		interrupted.TargetCheckpoint == nil || *interrupted.TargetCheckpoint != 2 ||
		interrupted.SourceCheckpoint == nil || *interrupted.SourceCheckpoint != 2 {
		t.Fatalf("run = %s checkpoints %v/%v, want interrupted at 2/2",
			interrupted.State, interrupted.SourceCheckpoint, interrupted.TargetCheckpoint)
	}

	// Resume: registry entry 3 sits ABOVE the source checkpoint. The
	// registry iterator must resume past the consumed entry 1.
	res, err := verifyContent(t, context.Background(), src, tgt, nil)
	if err == nil || !errors.Is(err, ErrVerificationFindings) {
		t.Fatalf("resume error = %v, want ErrVerificationFindings", err)
	}
	if !res.Resumed || res.VerificationID != interrupted.VerificationID {
		t.Fatalf("resume = %v/%s, want adopted run %s", res.Resumed, res.VerificationID, interrupted.VerificationID)
	}
	if res.Verified != 2 || res.Mismatched != 0 || res.SourceObserved != 4 || res.ObservedTargets != 2 {
		t.Fatalf("resumed result = %d/%d/%d/%d, want 2/0/4/2",
			res.Verified, res.Mismatched, res.SourceObserved, res.ObservedTargets)
	}

	run, issues := latestContentRun(t, tgt)
	if run.State != state.VerificationCompletedFail {
		t.Fatalf("run state = %s, want completed_fail", run.State)
	}
	codes := issueCodes(issues)
	if codes[IssueCodeUnresolvedRecord] != 2 {
		t.Fatalf("issues = %+v, want one unresolved_record_error per registry entry (1 and 3)", issues)
	}
	if codes[IssueCodeMissingTargetRow] != 0 {
		t.Fatalf("issues = %+v, want no missing_target_row on the resumed pass", issues)
	}
	if run.VerifiedCount != 2 || run.MismatchCount != 0 {
		t.Fatalf("durable counters = %d/%d, want 2/0", run.VerifiedCount, run.MismatchCount)
	}
}

func TestVerifyContentResumeAfterSourceChangeRefuses(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	for id := int32(1); id <= 3; id++ {
		seedSourceTorrent(t, src, id, fmt.Sprintf("t%d.bin", id), 2, 1700000000+id)
		seedSourceFile(t, src, id, fmt.Sprintf("t%d.bin", id), 2)
	}
	importAll(t, src, tgt)

	canceled := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := verifyContent(t, ctx, src, tgt, func(in *VerifyContentInput) {
		in.BatchSize = 1
		in.Progress = func(p VerifyProgress) {
			if !canceled && p.Checkpoint >= 1 {
				canceled = true
				cancel()
			}
		}
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v, want ErrInterrupted", err)
	}
	interrupted, err := state.LatestVerification(context.Background(), poolForRunner(t, tgt), state.VerificationKindContent)
	if err != nil {
		t.Fatal(err)
	}

	// Source content-anchor change (a torrent disappears): the recorded
	// fingerprint is no longer Equivalent — the old run aborts, history
	// preserved, and a fresh pass runs.
	execSourceSQL(t, src, `DELETE FROM magneticod.torrents WHERE id = 3`)

	res, err := verifyContent(t, context.Background(), src, tgt, nil)
	if err == nil || !errors.Is(err, ErrVerificationFindings) {
		t.Fatalf("error = %v, want ErrVerificationFindings", err)
	}
	if res.Resumed || res.VerificationID == interrupted.VerificationID {
		t.Fatalf("resumed %v run %s, want a fresh run", res.Resumed, res.VerificationID)
	}

	pool := poolForRunner(t, tgt)
	old, err := state.GetVerification(context.Background(), pool, interrupted.VerificationID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != state.VerificationAborted {
		t.Fatalf("old run state = %s, want aborted", old.State)
	}
	run, issues := latestContentRun(t, tgt)
	if run.Seq <= interrupted.Seq || run.State != state.VerificationCompletedFail {
		t.Fatalf("latest run = seq %d %s, want a newer completed_fail", run.Seq, run.State)
	}
	if issueCodes(issues)[IssueCodeExtraTargetRow] != 1 {
		t.Fatalf("issues = %+v, want exactly one extra_target_row (source row 3 is gone)", issues)
	}
}

func TestVerifyContentRefusesIncompleteImport(t *testing.T) {
	t.Parallel()

	t.Run("no run state", func(t *testing.T) {
		t.Parallel()
		tgt := createTargetForRunner(t)
		err := verifyContentErrOnly(t, tgt)
		if !errors.Is(err, target.ErrRunNotComplete) {
			t.Fatalf("error = %v, want ErrRunNotComplete", err)
		}
	})

	t.Run("import still running", func(t *testing.T) {
		t.Parallel()
		tgt := createTargetForRunner(t)
		pool := poolForRunner(t, tgt)
		if _, err := state.InitRun(context.Background(), pool, state.InitInput{
			RunID: "66666666-6666-4666-8666-666666666666", FingerprintRaw: []byte(`{}`),
			SourceAdapter: SourceAdapterOriginal, ContractVersion: MigrationContractVersion,
			CodecProfile: "compact", ExpectedTotal: 1, SourceMinID: 1, SourceMaxID: 1,
			DeferIndexes: false, Options: state.Options{Workers: 1, BatchSize: 1, MaxInflightBytes: 1 << 20},
		}); err != nil {
			t.Fatal(err)
		}
		if err := verifyContentErrOnly(t, tgt); !errors.Is(err, target.ErrRunNotComplete) {
			t.Fatalf("error = %v, want ErrRunNotComplete", err)
		}
	})
}

// verifyContentErrOnly runs the pass against a target with no source (the
// refusal must fire before any source connection is attempted).
func verifyContentErrOnly(t *testing.T, tgt string) error {
	t.Helper()
	in := VerifyContentInput{SourceDSN: "postgres://127.0.0.1:1/none", TargetDSN: tgt}
	_, err := VerifyContent(context.Background(), in)
	return err
}

// TestPageContentRetentionStaysWithinDecodedBudget pins the segment
// serving bound directly (Codex review iteration 2, findings 1 and 5):
// wire-format-v1 payloads are compressed, so a stored-bounds chunk can
// legitimately retain megabytes of decoded file lists — the earlier
// chunk-at-a-time window passed outcome-level tests while still holding a
// whole compressed page's decoded content. After every get(), the loaded
// window's decoded content must fit the budget.
func TestPageContentRetentionStaysWithinDecodedBudget(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	const rows = 12
	for id := int32(1); id <= rows; id++ {
		// 12 near-identical ~50 KB paths per row: ~600 KB of decoded path
		// bytes per torrent that compress to almost nothing, so the whole
		// page packs into ONE stored-bounds chunk while two rows already
		// exceed the 1 MiB decoded budget.
		seedSourceTorrent(t, src, id, fmt.Sprintf("compressible%d", id), 78, 1700000002)
		for i := 0; i < 12; i++ {
			seedSourceFile(t, src, id, fmt.Sprintf("d%d/%s", i, strings.Repeat("a", 50_000)), int64(1+i))
		}
	}
	importAll(t, src, tgt)

	pool := poolForRunner(t, tgt)
	defer pool.Close()
	ctx := context.Background()
	tp, err := readDecodePage(ctx, pool, 0, rows*2)
	if err != nil {
		t.Fatalf("readDecodePage() error = %v", err)
	}
	if len(tp.rows) != rows {
		t.Fatalf("page rows = %d, want %d", len(tp.rows), rows)
	}
	pc := newPageContent(ctx, pool, tp, codec.DefaultLimits(), 1<<20)
	if len(pc.entries) != rows {
		t.Fatalf("blob-backed entries = %d, want %d", len(pc.entries), rows)
	}

	segments, prevLo, prevHi := 0, -1, -1
	for idx := 0; idx < len(tp.rows); idx++ {
		tc, err := pc.get(idx)
		if err != nil {
			t.Fatalf("get(%d) error = %v", idx, err)
		}
		if tc == nil || tc.files == nil {
			t.Fatalf("get(%d) returned no decoded content (%+v)", idx, tc)
		}
		var retained int64
		for _, c := range pc.cur {
			retained += decodedBytes(c)
		}
		if retained > 1<<20 {
			t.Fatalf("get(%d): retained decoded bytes = %d, budget %d — the serving window leaked",
				idx, retained, int64(1<<20))
		}
		if pc.curLo != prevLo || pc.curHi != prevHi {
			segments++
			prevLo, prevHi = pc.curLo, pc.curHi
		}
	}
	if segments < 2 {
		t.Fatalf("serving used %d segments, want >= 2 (12 rows x ~600KB decoded vs a 1MiB budget)", segments)
	}
}

// TestVerifyContentVanishedRegistryEntryDoesNotPoisonLaterRows: a registry
// entry whose source row vanished (deleted post-import) must not block the
// classification of LATER unresolved rows (Codex review iteration 2,
// finding 3). With entry 2 vanished and entry 3 still live, row 3 is a
// real unresolved_record_error — not a missing_target_row — and only the
// vanished entry stays uncovered at finalize.
func TestVerifyContentVanishedRegistryEntryDoesNotPoisonLaterRows(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	// Rows 1 and 4 import; rows 2 and 3 are rejected into the registry.
	seedSourceTorrent(t, src, 1, "ok1.bin", 40, 1700000001)
	seedSourceFile(t, src, 1, "ok1.bin", 40)
	execSourceSQL(t, src, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        VALUES (2, decode('AA','hex'), 'bad2', 9, 1700000002)`)
	execSourceSQL(t, src, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        VALUES (3, decode('AB','hex'), 'bad3', 9, 1700000003)`)
	seedSourceTorrent(t, src, 4, "ok4.bin", 50, 1700000004)
	seedSourceFile(t, src, 4, "ok4.bin", 50)
	importAll(t, src, tgt)

	// Registry entry 2's source row disappears before verification; entry
	// 3's source row is still live.
	execSourceSQL(t, src, `DELETE FROM magneticod.torrents WHERE id = 2`)

	res, err := verifyContent(t, context.Background(), src, tgt, nil)
	if err == nil || !errors.Is(err, ErrVerificationFindings) {
		t.Fatalf("error = %v, want ErrVerificationFindings", err)
	}
	if res.Pass {
		t.Fatal("pass = true, want completed_fail")
	}
	if res.Verified != 2 || res.SourceObserved != 3 || res.ObservedTargets != 2 {
		t.Fatalf("result = verified %d source %d targets %d, want 2/3/2",
			res.Verified, res.SourceObserved, res.ObservedTargets)
	}

	run, issues := latestContentRun(t, tgt)
	if run.State != state.VerificationCompletedFail {
		t.Fatalf("run state = %s, want completed_fail", run.State)
	}
	codes := issueCodes(issues)
	// Row 3 is classified by its own live registry entry.
	if codes[IssueCodeUnresolvedRecord] != 1 {
		t.Fatalf("issues = %+v, want exactly one %s (for source 3)", issues, IssueCodeUnresolvedRecord)
	}
	if codes[IssueCodeMissingTargetRow] != 0 {
		t.Fatalf("issues = %+v, want no missing_target_row (the vanished entry must not poison row 3)", issues)
	}
	// Only the vanished entry (2) stays uncovered: row 3 was classified.
	if codes[IssueCodeUnresolvedCoverage] != 1 {
		t.Fatalf("issues = %+v, want exactly one %s", issues, IssueCodeUnresolvedCoverage)
	}
	for _, is := range issues {
		switch is.Code {
		case IssueCodeUnresolvedRecord:
			if is.SourceID == nil || *is.SourceID != 3 {
				t.Fatalf("%s source = %v, want 3", is.Code, is.SourceID)
			}
		case IssueCodeUnresolvedCoverage:
			// Details round-trip through JSONB: numbers come back as float64.
			if is.Details["uncovered"] != float64(1) || is.Details["min_id"] != float64(2) || is.Details["max_id"] != float64(2) {
				t.Fatalf("%s details = %+v, want uncovered/min/max = 1/2/2", is.Code, is.Details)
			}
		}
	}
}

// TestVerifyContentExtrasResumeKeepsOrphanCount: the content pass has the
// same extras-phase resume rule as decode — the orphan count must come
// back from the durable issue on resume, not reset to zero (Codex review
// iteration 2, finding 4).
func TestVerifyContentExtrasResumeKeepsOrphanCount(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	seedSourceTorrent(t, src, 1, "t1.bin", 10, 1700000001)
	seedSourceFile(t, src, 1, "t1.bin", 10)
	importAll(t, src, tgt)
	seedDecodeOrphanBlob(t, tgt)

	// Interrupt right after the extras phase commits.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := verifyContent(t, ctx, src, tgt, func(in *VerifyContentInput) {
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
	interrupted, err := state.LatestVerification(context.Background(), pool, state.VerificationKindContent)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.State != state.VerificationInterrupted || interrupted.Phase != state.VerificationPhaseExtras {
		t.Fatalf("interrupted run = %s/%s, want interrupted/extras", interrupted.State, interrupted.Phase)
	}
	seq1 := interrupted.Seq

	res2, err := verifyContent(t, context.Background(), src, tgt, nil)
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

// TestPageContentAccountingCoversFileArrays: the decoded-segment budget is
// charged with the REAL retained memory — the []codec.File backing array
// at capacity plus path data — not a per-file lower bound. Two rows of
// 30,000 one-byte duplicate paths each fit the 1 MiB budget individually
// but not jointly under honest accounting: the segment cut must admit
// only the first (Codex review iteration 3, finding 2 — the earlier
// estimator charged 17 bytes per file, admitted both, and retained
// 1.74 MB of arrays).
func TestPageContentAccountingCoversFileArrays(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	const filesPerRow = 30_000
	// 30,000 duplicate one-byte paths per torrent (duplicate paths are
	// valid: the canonical compare is a multiset). Bulk-seeded in one
	// statement per row.
	for id := int32(1); id <= 2; id++ {
		total := int64(filesPerRow) * int64(filesPerRow+1) / 2
		seedSourceTorrent(t, src, id, fmt.Sprintf("manyfiles%d", id), total, 1700000002)
		execSourceSQL(t, src, `INSERT INTO magneticod.files (torrent_id, path, size)
            SELECT $1, 'a', g FROM generate_series(1, $2) g`, id, int32(filesPerRow))
	}
	importAll(t, src, tgt)

	pool := poolForRunner(t, tgt)
	defer pool.Close()
	ctx := context.Background()
	tp, err := readDecodePage(ctx, pool, 0, 4)
	if err != nil {
		t.Fatalf("readDecodePage() error = %v", err)
	}
	if len(tp.rows) != 2 {
		t.Fatalf("page rows = %d, want 2", len(tp.rows))
	}
	pc := newPageContent(ctx, pool, tp, codec.DefaultLimits(), 1<<20)
	if len(pc.entries) != 2 {
		t.Fatalf("blob-backed entries = %d, want 2", len(pc.entries))
	}

	// The first window must hold ONLY the first row: each row fits solo,
	// but two admitted together exceed the budget under honest accounting.
	tc, err := pc.get(0)
	if err != nil || tc == nil || tc.files == nil {
		t.Fatalf("get(0) = %v, %v; want decoded content", tc, err)
	}
	if pc.curHi != 1 {
		t.Fatalf("first window covers rows [0,%d), want [0,1) — the budget admitted both rows jointly",
			pc.curHi)
	}
	if perRow := decodedBytes(tc); perRow > 1<<20 {
		t.Fatalf("single row charged %d, want within the 1 MiB budget (solo admission)", perRow)
	}

	// Walking the page keeps every window within the budget.
	for idx := 0; idx < len(tp.rows); idx++ {
		if _, err := pc.get(idx); err != nil {
			t.Fatalf("get(%d) error = %v", idx, err)
		}
		var retained int64
		for _, c := range pc.cur {
			retained += decodedBytes(c)
		}
		if retained > 1<<20 {
			t.Fatalf("get(%d): retained decoded bytes = %d, budget %d", idx, retained, int64(1<<20))
		}
	}
}

func TestVerifyContentExtrasCommitCancelPersists(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	seedSourceTorrent(t, src, 1, "t1.bin", 10, 1700000001)
	seedSourceFile(t, src, 1, "t1.bin", 10)
	importAll(t, src, tgt)

	// Same premise as the decode pass: hold the run's row locked from the
	// first committed page, cancel while the extras commit's UPDATE waits —
	// resumable exit 2 with a durable mark, not fatal exit 1 (Codex review
	// iteration 4, finding 3).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := poolForRunner(t, tgt)
	lockTX, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var lockOnce sync.Once
	lockHeld := make(chan struct{})
	go func() {
		<-lockHeld
		waitExtrasCommitBlocked(t, pool)
		cancel()
		time.Sleep(200 * time.Millisecond)
		if err := lockTX.Rollback(context.Background()); err != nil {
			t.Errorf("release the row lock: %v", err)
		}
	}()
	_, err = verifyContent(t, ctx, src, tgt, func(in *VerifyContentInput) {
		in.Progress = func(p VerifyProgress) {
			if p.Checkpoint == 0 {
				return // the pre-scan report
			}
			lockOnce.Do(func() {
				if _, err := lockTX.Exec(context.Background(),
					`SELECT verification_id FROM magnetico_migration.verification_runs WHERE state = 'running' FOR UPDATE`); err != nil {
					t.Errorf("lock the verification run row: %v", err)
				}
				close(lockHeld)
			})
		}
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v, want ErrInterrupted (the extras-commit cancellation must take the exit-2 path)", err)
	}
	run, err := state.LatestVerification(context.Background(), pool, state.VerificationKindContent)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != state.VerificationInterrupted {
		t.Fatalf("pass state = %s, want interrupted (the durable mark must survive an extras-commit cancellation)", run.State)
	}
}

func TestVerifyContentExtrasResumeDoesNotDoubleCountDrift(t *testing.T) {
	t.Parallel()

	src, tgt := decodeFixture(t)
	seedSourceTorrent(t, src, 1, "t1.bin", 10, 1700000001)
	seedSourceFile(t, src, 1, "t1.bin", 10)
	importAll(t, src, tgt)
	// Drift the source after the import: every invocation recomputes the
	// provenance flag against the recorded IMPORT fingerprint, so run 1
	// commits the drift issue (info) during its scan.
	seedSourceTorrent(t, src, 99, "late.bin", 5, 1700000009)
	seedSourceFile(t, src, 99, "late.bin", 5)

	// Run 1: interrupt right after the extras phase commits.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := verifyContent(t, ctx, src, tgt, func(in *VerifyContentInput) {
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
	run1, err := state.LatestVerification(context.Background(), pool, state.VerificationKindContent)
	if err != nil {
		t.Fatal(err)
	}
	if run1.State != state.VerificationInterrupted || run1.Phase != state.VerificationPhaseExtras {
		t.Fatalf("run 1 = %s/%s, want interrupted/extras", run1.State, run1.Phase)
	}
	seq1 := run1.Seq

	// Resume: the run's own recorded fingerprint is unchanged (adopted), but
	// the drift recomputation vs the import fingerprint fires again — the
	// re-proposed issue dedupes in storage, and the reported severity split
	// must count it once (Codex review iteration 4, finding 4).
	res2, err := verifyContent(t, context.Background(), src, tgt, nil)
	if !errors.Is(err, ErrVerificationFindings) {
		t.Fatalf("resume error = %v, want ErrVerificationFindings (row 99 is source-only)", err)
	}
	if !res2.Resumed || res2.Seq != seq1 {
		t.Fatalf("resumed = %v seq %d, want the adopted run (seq %d)", res2.Resumed, res2.Seq, seq1)
	}
	if res2.IssuesBySeverity[state.IssueInfo] != 1 {
		t.Fatalf("resumed info issue count = %d, want 1 (the drift issue is durable once; the recount after the finalize commit is authoritative)",
			res2.IssuesBySeverity[state.IssueInfo])
	}
	var driftCount int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM magnetico_migration.verification_issues
         WHERE verification_id = $1::uuid AND code = 'source_fingerprint_drift'`,
		res2.VerificationID).Scan(&driftCount); err != nil {
		t.Fatal(err)
	}
	if driftCount != 1 {
		t.Fatalf("durable drift issues = %d, want 1", driftCount)
	}
}
