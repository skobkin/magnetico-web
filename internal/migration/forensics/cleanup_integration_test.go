package forensics

// Cleanup integration tests (04C slice 6): the dry-run is read-only, the
// green path drops exactly the operational schema (production bytes
// untouched) and the second run is an honest no-op, every gate refuses its
// own fixture without any DDL, the corrected G7 regressions hold (corrupt
// pass states are never waivable; source-only mismatches are ordinary and
// ARE waivable; aggregate orphan issues count as blocking), a mid-drop
// failure rolls the whole transaction back, and the lost-COMMIT
// classification never reports success without an acknowledgment.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

// initProductionSchema creates the EXACT production storage schema via the
// production path — G4's persistence.MakeDatabase verification demands the
// full object set (indexes, schema_version, pg_trgm), which the records
// tests' minimal DDL deliberately lacks.
func initProductionSchema(t *testing.T, dsn string) {
	t.Helper()
	if err := persistence.InitSchema(context.Background(), dsn); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
}

// seedCleanupTarget builds a v2 run + production schema holding a
// CONSISTENT completed import: 2 torrents (one blobless single-file, one
// multi-file with its blob), checkpoint 5, revision 4, zero errors.
func seedCleanupTarget(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	pool := forensicsPool(t, dsn)
	runID := seedV2Forensics(t, pool)
	initProductionSchema(t, dsn)
	seedTargetRecord(t, pool, recordTarget{id: 1, name: "solo", totalSize: 10, fileCount: 1})
	seedTargetRecord(t, pool, recordTarget{id: 5, name: "multi", totalSize: 30, fileCount: 2,
		blob: encodeTestBlob(t, []codec.File{{Path: "a", Size: 10}, {Path: "b", Size: 20}})})
	if _, err := pool.Exec(context.Background(), `UPDATE magnetico_migration.progress
        SET phase = 'import_complete', scanned_count = 2, success_count = 2,
            error_count = 0, run_error_count = 0, blob_count = 1,
            last_processed_id = 5, data_revision = 4
        WHERE run_id = $1::uuid`, runID); err != nil {
		t.Fatal(err)
	}
	return pool, dsn, runID
}

// verRun describes one seeded verification_runs row.
type verRun struct {
	kind       string
	runState   string
	phase      string
	startRev   int64
	completed  bool
	verified   int64
	mismatched int64
	unverif    int64
	assumed    int64
	issues     int64
	expTarget  *int64
	obsTarget  *int64
	expSource  *int64
	obsSource  *int64
}

func insertVerRun(t *testing.T, pool *pgxpool.Pool, runID string, v verRun) string {
	t.Helper()
	id := mustNewID(t)
	if _, err := pool.Exec(context.Background(), `INSERT INTO magnetico_migration.verification_runs
        (verification_id, run_id, kind, state, phase, start_data_revision, completed_at,
         verified_count, mismatch_count, unverifiable_count, assumed_single_count, issue_count,
         expected_target_ids, observed_target_ids, expected_source_ids, observed_source_ids,
         settings)
        VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, `+coalesceNow(v.completed)+`,
         $7, $8, $9, $10, $11, $12, $13, $14, $15, '{"workers":4}'::jsonb)`,
		id, runID, v.kind, v.runState, v.phase, v.startRev,
		v.verified, v.mismatched, v.unverif, v.assumed, v.issues,
		v.expTarget, v.obsTarget, v.expSource, v.obsSource); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertIssue(t *testing.T, pool *pgxpool.Pool, verID, severity, code string, sourceID *int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO magnetico_migration.verification_issues
        (verification_id, source_id, info_hash, code, stage, severity, details)
        VALUES ($1::uuid, $2, $3, $4, $5, $6, '{"count":1}'::jsonb)`,
		verID, sourceID, []byte{0xab, 0xcd}, code, "compare", severity); err != nil {
		t.Fatal(err)
	}
}

// seedGreenVerification adds the two authoritative completed_pass runs the
// green path needs (decode without source pairs, content with both).
func seedGreenVerification(t *testing.T, pool *pgxpool.Pool, runID string) (decode, content string) {
	t.Helper()
	decode = insertVerRun(t, pool, runID, verRun{
		kind: "decode", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
		verified: 2, issues: 0, expTarget: ptrInt64(2), obsTarget: ptrInt64(2),
	})
	content = insertVerRun(t, pool, runID, verRun{
		kind: "content", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
		verified: 2, issues: 0,
		expTarget: ptrInt64(2), obsTarget: ptrInt64(2),
		expSource: ptrInt64(2), obsSource: ptrInt64(2),
	})
	return decode, content
}

// exportEvidence runs a complete metadata export with a manifest and
// returns the manifest path.
func exportEvidence(t *testing.T, dsn string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "evidence.jsonl")
	if _, err := ExportErrors(context.Background(), ExportInput{
		Options:   ExportOptions{OutputPath: out, WriteManifest: true, Limits: DefaultLimits()},
		TargetDSN: dsn,
	}); err != nil {
		t.Fatalf("evidence export: %v", err)
	}
	return out + ".manifest.json"
}

func cleanupInput(dsn, manifest string, mutate func(*CleanupOptions)) CleanupInput {
	opts := CleanupOptions{
		Execute:          true,
		ConfirmBackup:    true,
		ConfirmExport:    true,
		EvidenceManifest: manifest,
	}
	if mutate != nil {
		mutate(&opts)
	}
	return CleanupInput{Options: opts, TargetDSN: dsn}
}

func assertSchemaIntact(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	insp, err := state.InspectOperationalSchema(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if insp.Class != state.SchemaV2 {
		t.Fatalf("schema class = %v, want intact v2", insp.Class)
	}
}

func assertRefused(t *testing.T, err error, wantCorrupt bool) {
	t.Helper()
	if !errors.Is(err, ErrCleanupRefused) {
		t.Fatalf("error = %v, want ErrCleanupRefused", err)
	}
	if corrupt := errors.Is(err, state.ErrRunStateCorrupt); corrupt != wantCorrupt {
		t.Fatalf("corrupt=%v, want %v (error = %v)", corrupt, wantCorrupt, err)
	}
}

func TestCleanupDryRunIsReadOnly(t *testing.T) {
	t.Parallel()
	pool, dsn, runID := seedCleanupTarget(t)
	defer pool.Close()
	seedGreenVerification(t, pool, runID)

	res, err := Cleanup(context.Background(), CleanupInput{
		Options:   CleanupOptions{DryRun: true},
		TargetDSN: dsn,
	})
	if err != nil {
		t.Fatalf("Cleanup(dry-run) error = %v", err)
	}
	if res.Dropped || res.NoOp {
		t.Fatalf("dry-run reported Dropped=%v NoOp=%v", res.Dropped, res.NoOp)
	}
	if len(res.Gates) == 0 || len(res.Preview) == 0 {
		t.Fatal("dry-run produced no gates or preview")
	}
	// Attestation gates are advisory findings in a dry-run.
	if res.Gates[0].Name != "G1 lock" || res.Gates[0].Status != GatePass {
		t.Fatalf("first gate = %+v", res.Gates[0])
	}

	// Nothing changed: schema, progress row, production rows.
	assertSchemaIntact(t, pool)
	var phase string
	var scanned int64
	if err := pool.QueryRow(context.Background(),
		`SELECT phase, scanned_count FROM magnetico_migration.progress WHERE run_id = $1::uuid`,
		runID).Scan(&phase, &scanned); err != nil {
		t.Fatal(err)
	}
	if phase != "import_complete" || scanned != 2 {
		t.Fatalf("progress mutated by the dry-run: phase=%s scanned=%d", phase, scanned)
	}
	var torrents int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM magneticod.torrents`).Scan(&torrents); err != nil {
		t.Fatal(err)
	}
	if torrents != 2 {
		t.Fatalf("torrents = %d, want 2", torrents)
	}
	// The preview names the missing confirmations as would-be refusals.
	joined := strings.Join(res.Blockers, "\n")
	if !strings.Contains(joined, "G8") || !strings.Contains(joined, "G9") {
		t.Fatalf("blockers = %v, want the attestation gates", res.Blockers)
	}
}

func TestCleanupGreenPathAndNoOp(t *testing.T) {
	t.Parallel()
	pool, dsn, runID := seedCleanupTarget(t)
	defer pool.Close()
	seedGreenVerification(t, pool, runID)
	manifest := exportEvidence(t, dsn)

	res, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
	if err != nil {
		t.Fatalf("Cleanup(execute) error = %v", err)
	}
	if !res.Dropped {
		t.Fatal("green cleanup did not report Dropped")
	}

	// The operational schema is gone; the production data is untouched.
	insp, err := state.InspectOperationalSchema(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if insp.Class != state.SchemaAbsent {
		t.Fatalf("schema class after cleanup = %v, want absent", insp.Class)
	}
	var torrents, blobs int64
	if err := pool.QueryRow(context.Background(),
		`SELECT (SELECT COUNT(*) FROM magneticod.torrents),
		        (SELECT COUNT(*) FROM magneticod.torrent_file_blobs)`).Scan(&torrents, &blobs); err != nil {
		t.Fatal(err)
	}
	if torrents != 2 || blobs != 1 {
		t.Fatalf("production rows after cleanup: torrents=%d blobs=%d, want 2/1", torrents, blobs)
	}
	if _, err := state.Load(context.Background(), pool); !errors.Is(err, state.ErrNoRunState) {
		t.Fatalf("state.Load after cleanup = %v, want ErrNoRunState", err)
	}

	// The second run is an honest no-op (exit 0 semantics, nil error).
	res2, err := Cleanup(context.Background(), CleanupInput{
		Options: CleanupOptions{Execute: true, ConfirmBackup: true}, TargetDSN: dsn,
	})
	if err != nil {
		t.Fatalf("second Cleanup error = %v", err)
	}
	if !res2.NoOp {
		t.Fatal("second cleanup did not report NoOp")
	}
}

func TestCleanupWithErrorsOverrideAccepted(t *testing.T) {
	t.Parallel()
	pool, dsn, runID := seedCleanupTarget(t)
	defer pool.Close()
	seedGreenVerification(t, pool, runID)

	// One unresolved record error: phase with_errors, counters consistent
	// with the facts (Reconcile passes — the override's precondition).
	if _, err := pool.Exec(context.Background(), `INSERT INTO magnetico_migration.errors
        (run_id, scope, source_id, stage, category, code)
        VALUES ($1::uuid, 'record', 9, 'validate', 'integrity', 'invalid_metadata')`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE magnetico_migration.progress
        SET phase = 'import_complete_with_errors', scanned_count = 3, error_count = 1
        WHERE run_id = $1::uuid`, runID); err != nil {
		t.Fatal(err)
	}
	manifest := exportEvidence(t, dsn)

	// Without the override: refused.
	_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
	assertRefused(t, err, false)

	// With the override (+ backup + evidence): the reconciled with_errors
	// state is acceptable and the drop commits.
	res, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, func(o *CleanupOptions) {
		o.ForceUnverified = true
		o.Reason = "accepted remaining metadata defect; operator decision 2026-11-09"
	}))
	if err != nil {
		t.Fatalf("Cleanup(with-errors override) error = %v", err)
	}
	if !res.Dropped {
		t.Fatal("override cleanup did not drop")
	}
	insp, err := state.InspectOperationalSchema(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if insp.Class != state.SchemaAbsent {
		t.Fatalf("schema class after override cleanup = %v", insp.Class)
	}
}

func TestCleanupGateRefusals(t *testing.T) {
	t.Parallel()

	t.Run("importing phase", func(t *testing.T) {
		t.Parallel()
		dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
		pool := forensicsPool(t, dsn)
		defer pool.Close()
		seedV2Forensics(t, pool)
		initProductionSchema(t, dsn) // empty production: facts == counters
		_, err := Cleanup(context.Background(), CleanupInput{
			Options: CleanupOptions{DryRun: true}, TargetDSN: dsn,
		})
		if err != nil {
			t.Fatalf("dry-run must not error, got %v", err)
		}
		// Execute (with every attestation) refuses on the phase gate.
		_, err = Cleanup(context.Background(), CleanupInput{Options: CleanupOptions{
			Execute: true, ConfirmBackup: true,
			ConfirmExport: true, EvidenceManifest: "/nonexistent/evidence.manifest.json",
		}, TargetDSN: dsn})
		assertRefused(t, err, false)
		assertSchemaIntact(t, pool)
	})

	t.Run("v1 schema", func(t *testing.T) {
		t.Parallel()
		dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
		pool := forensicsPool(t, dsn)
		defer pool.Close()
		seedV1Forensics(t, pool)
		_, err := Cleanup(context.Background(), CleanupInput{
			Options: CleanupOptions{Execute: true, ConfirmBackup: true}, TargetDSN: dsn,
		})
		assertRefused(t, err, false)
		if !strings.Contains(err.Error(), "v1") {
			t.Fatalf("refusal does not mention v1: %v", err)
		}
		// No upgrade happened.
		insp, err2 := state.InspectOperationalSchema(context.Background(), pool)
		if err2 != nil {
			t.Fatal(err2)
		}
		if insp.Class != state.SchemaV1 {
			t.Fatalf("v1 schema was mutated: %v", insp.Class)
		}
	})

	t.Run("unrecognized schema", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		if _, err := pool.Exec(context.Background(),
			`CREATE TABLE magnetico_migration.junk (x INT)`); err != nil {
			t.Fatal(err)
		}
		_ = runID
		_, err := Cleanup(context.Background(), CleanupInput{
			Options: CleanupOptions{Execute: true, ConfirmBackup: true}, TargetDSN: dsn,
		})
		assertRefused(t, err, false)
		// Nothing was dropped and nothing was repaired: the schema stays
		// exactly as unrecognized as before.
		insp, err2 := state.InspectOperationalSchema(context.Background(), pool)
		if err2 != nil {
			t.Fatal(err2)
		}
		if insp.Class != state.SchemaUnrecognized {
			t.Fatalf("schema class = %v, want unchanged unrecognized", insp.Class)
		}
	})

	t.Run("missing content verification", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		insertVerRun(t, pool, runID, verRun{
			kind: "decode", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
			verified: 2, expTarget: ptrInt64(2), obsTarget: ptrInt64(2),
		})
		manifest := exportEvidence(t, dsn)
		_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
		assertRefused(t, err, false)
		if !strings.Contains(err.Error(), "content") {
			t.Fatalf("refusal does not name the missing kind: %v", err)
		}
	})

	t.Run("stale verdict revision", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		insertVerRun(t, pool, runID, verRun{
			kind: "decode", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
			verified: 2, expTarget: ptrInt64(2), obsTarget: ptrInt64(2),
		})
		insertVerRun(t, pool, runID, verRun{
			kind: "content", runState: "completed_pass", phase: "complete", startRev: 3, completed: true,
			verified: 2, expTarget: ptrInt64(2), obsTarget: ptrInt64(2),
			expSource: ptrInt64(2), obsSource: ptrInt64(2),
		})
		manifest := exportEvidence(t, dsn)
		_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
		assertRefused(t, err, false)
		if !strings.Contains(err.Error(), "stale verdict") {
			t.Fatalf("refusal does not name the staleness: %v", err)
		}
	})

	t.Run("missing manifest file", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		seedGreenVerification(t, pool, runID)
		_, err := Cleanup(context.Background(), cleanupInput(dsn, "/nonexistent/evidence.manifest.json", nil))
		assertRefused(t, err, false)
	})

	t.Run("wrong-run manifest", func(t *testing.T) {
		t.Parallel()
		// The foreign evidence is a perfectly valid export of a DIFFERENT
		// run; cleanup must refuse it on the run binding alone.
		otherDSN := pgtest.IntegrationCluster(t).TestDatabase(t)
		otherPool := forensicsPool(t, otherDSN)
		otherRun := seedV2Forensics(t, otherPool)
		seedGreenVerification(t, otherPool, otherRun)
		foreign := exportEvidence(t, otherDSN)

		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		seedGreenVerification(t, pool, runID)
		_, err := Cleanup(context.Background(), cleanupInput(dsn, foreign, nil))
		assertRefused(t, err, false)
		if !strings.Contains(err.Error(), "run_id") {
			t.Fatalf("refusal does not name the run binding: %v", err)
		}
	})

	t.Run("tampered artifact", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		seedGreenVerification(t, pool, runID)
		manifest := exportEvidence(t, dsn)
		artifact := strings.TrimSuffix(manifest, ".manifest.json")
		f, err := os.OpenFile(artifact, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("{}\n"); err != nil {
			t.Fatal(err)
		}
		f.Close()
		_, err = Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
		assertRefused(t, err, false)
	})

	t.Run("filtered manifest", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		seedGreenVerification(t, pool, runID)
		out := filepath.Join(t.TempDir(), "partial.jsonl")
		if _, err := ExportErrors(context.Background(), ExportInput{
			Options:   ExportOptions{OutputPath: out, WriteManifest: true, OnlyUnresolved: true, Limits: DefaultLimits()},
			TargetDSN: dsn,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := Cleanup(context.Background(), cleanupInput(dsn, out+".manifest.json", nil))
		assertRefused(t, err, false)
		if !strings.Contains(err.Error(), "coverage") && !strings.Contains(err.Error(), "filters") {
			t.Fatalf("refusal does not name partial coverage: %v", err)
		}
	})

	t.Run("stale registry digest", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		// A HISTORIC decode run (seq 1) that later transitions — counts and
		// revision stay identical, the authoritative runs stay passing, but
		// the manifest's registry-state digests go stale (owner finding 2).
		historic := insertVerRun(t, pool, runID, verRun{
			kind: "decode", runState: "completed_pass", phase: "complete", startRev: 0, completed: true,
			verified: 1, expTarget: ptrInt64(1), obsTarget: ptrInt64(1),
		})
		seedGreenVerification(t, pool, runID) // seq 2 + 3: authoritative passes
		manifest := exportEvidence(t, dsn)
		if _, err := pool.Exec(context.Background(), `UPDATE magnetico_migration.verification_runs
            SET state = 'running', updated_at = now() WHERE verification_id = $1::uuid`, historic); err != nil {
			t.Fatal(err)
		}
		_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
		assertRefused(t, err, false)
		if !strings.Contains(err.Error(), "registry-state digests differ") {
			t.Fatalf("refusal does not name the stale digest: %v", err)
		}
	})

	t.Run("missing backup attestation", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		seedGreenVerification(t, pool, runID)
		manifest := exportEvidence(t, dsn)
		_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, func(o *CleanupOptions) {
			o.ConfirmBackup = false
		}))
		assertRefused(t, err, false)
		if !strings.Contains(err.Error(), "confirm-backup") {
			t.Fatalf("refusal does not name the backup attestation: %v", err)
		}
	})
}

func TestCleanupFlagContradictions(t *testing.T) {
	t.Parallel()
	bogus := "postgres://127.0.0.1:1/none" // never contacted: G10 fires first
	cases := []struct {
		name string
		opts CleanupOptions
	}{
		{"dry-run and execute", CleanupOptions{DryRun: true, Execute: true}},
		{"override without reason", CleanupOptions{Execute: true, ForceUnverified: true}},
		{"override without execute", CleanupOptions{ForceUnverified: true, Reason: "x"}},
		{"confirm-export without manifest", CleanupOptions{ConfirmExport: true}},
		{"manifest without confirm-export", CleanupOptions{EvidenceManifest: "/x"}},
		{"exception half 1", CleanupOptions{AllowMissingExportEvidence: true}},
		{"exception half 2", CleanupOptions{ConfirmHistoryLoss: true}},
		{"exception without override", CleanupOptions{AllowMissingExportEvidence: true, ConfirmHistoryLoss: true}},
		{"both evidence paths", CleanupOptions{ConfirmExport: true, EvidenceManifest: "/x",
			AllowMissingExportEvidence: true, ConfirmHistoryLoss: true}},
		{"blank reason", CleanupOptions{Execute: true, ForceUnverified: true, Reason: "   "}},
		{"oversized reason", CleanupOptions{Execute: true, ForceUnverified: true,
			Reason: strings.Repeat("r", 513)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Cleanup(context.Background(), CleanupInput{Options: tc.opts, TargetDSN: bogus})
			var optsErr *OptionsError
			if !errors.As(err, &optsErr) {
				t.Fatalf("error = %v, want *OptionsError", err)
			}
		})
	}
}

func TestCleanupCorruptVerificationStates(t *testing.T) {
	t.Parallel()

	// A completed_pass run whose persisted issues contradict it is CORRUPT —
	// never waivable, in every contradiction shape (owner finding 3, item 6).
	t.Run("pass with blocking issue", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		decode, _ := seedGreenVerification(t, pool, runID)
		insertIssue(t, pool, decode, "mismatch", "mismatch_name", ptrInt64(1))
		if _, err := pool.Exec(context.Background(), `UPDATE magnetico_migration.verification_runs
            SET issue_count = 1 WHERE verification_id = $1::uuid`, decode); err != nil {
			t.Fatal(err)
		}
		manifest := exportEvidence(t, dsn)

		_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
		assertRefused(t, err, true)

		// --force-unverified must NOT waive corruption.
		_, err = Cleanup(context.Background(), cleanupInput(dsn, manifest, func(o *CleanupOptions) {
			o.ForceUnverified = true
			o.Reason = "trying to waive a corrupt pass"
		}))
		assertRefused(t, err, true)
		assertSchemaIntact(t, pool)
	})

	t.Run("issue_count mismatch", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		decode, _ := seedGreenVerification(t, pool, runID)
		insertIssue(t, pool, decode, "info", "fingerprint_drift", ptrInt64(1))
		// run.issue_count stays 0 while one row persists — contradiction.
		manifest := exportEvidence(t, dsn)
		_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, func(o *CleanupOptions) {
			o.ForceUnverified = true
			o.Reason = "x"
		}))
		assertRefused(t, err, true)
	})

	t.Run("target-row accounting violated", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		insertVerRun(t, pool, runID, verRun{
			kind: "decode", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
			verified: 1, issues: 0, obsTarget: ptrInt64(2), expTarget: ptrInt64(2), // 1 != 2
		})
		insertVerRun(t, pool, runID, verRun{
			kind: "content", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
			verified: 2, issues: 0,
			expTarget: ptrInt64(2), obsTarget: ptrInt64(2),
			expSource: ptrInt64(2), obsSource: ptrInt64(2),
		})
		manifest := exportEvidence(t, dsn)
		_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, func(o *CleanupOptions) {
			o.ForceUnverified = true
			o.Reason = "x"
		}))
		assertRefused(t, err, true)
	})

	// Source-only mismatch findings with CONSISTENT counters are an
	// ORDINARY failed verdict: refused without the override, WAIVED with it.
	t.Run("source-only mismatch is ordinary and waivable", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		seedGreenVerification(t, pool, runID)
		// Rework the authoritative content run into a failed verdict with a
		// source-only mismatch issue (severity mismatch, counters consistent:
		// issue_count 1 == persisted rows; target accounting 2 == 2).
		if _, err := pool.Exec(context.Background(), `UPDATE magnetico_migration.verification_runs
            SET state = 'completed_fail', issue_count = 1
            WHERE kind = 'content' AND run_id = $1::uuid`, runID); err != nil {
			t.Fatal(err)
		}
		var contentID string
		if err := pool.QueryRow(context.Background(), `SELECT verification_id::text
            FROM magnetico_migration.verification_runs WHERE kind = 'content' AND run_id = $1::uuid`,
			runID).Scan(&contentID); err != nil {
			t.Fatal(err)
		}
		insertIssue(t, pool, contentID, "mismatch", "source_only_extra", ptrInt64(9))
		manifest := exportEvidence(t, dsn)

		_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
		assertRefused(t, err, false) // ordinary, NOT corrupt

		res, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, func(o *CleanupOptions) {
			o.ForceUnverified = true
			o.Reason = "source-only discrepancy accepted"
		}))
		if err != nil {
			t.Fatalf("override cleanup error = %v", err)
		}
		if !res.Dropped {
			t.Fatal("override cleanup did not drop")
		}
	})

	t.Run("aggregate orphan issue is blocking", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		seedGreenVerification(t, pool, runID)
		if _, err := pool.Exec(context.Background(), `UPDATE magnetico_migration.verification_runs
            SET state = 'completed_fail', issue_count = 1
            WHERE kind = 'content' AND run_id = $1::uuid`, runID); err != nil {
			t.Fatal(err)
		}
		var contentID string
		if err := pool.QueryRow(context.Background(), `SELECT verification_id::text
            FROM magnetico_migration.verification_runs WHERE kind = 'content' AND run_id = $1::uuid`,
			runID).Scan(&contentID); err != nil {
			t.Fatal(err)
		}
		// Run-scope orphan finding: NULL source_id, blocking severity.
		insertIssue(t, pool, contentID, "mismatch", "aggregate_orphans", nil)
		manifest := exportEvidence(t, dsn)
		_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
		assertRefused(t, err, false)
		if !strings.Contains(err.Error(), "blocking-severity") {
			t.Fatalf("refusal does not name the blocking severity: %v", err)
		}
	})

	// Info-only issues on a passing run are permitted (provenance notes).
	t.Run("info issues are permitted on a pass", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		decode, _ := seedGreenVerification(t, pool, runID)
		insertIssue(t, pool, decode, "info", "fingerprint_drift", ptrInt64(1))
		if _, err := pool.Exec(context.Background(), `UPDATE magnetico_migration.verification_runs
            SET issue_count = 1 WHERE verification_id = $1::uuid`, decode); err != nil {
			t.Fatal(err)
		}
		manifest := exportEvidence(t, dsn)
		res, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
		if err != nil {
			t.Fatalf("cleanup with info issues error = %v", err)
		}
		if !res.Dropped {
			t.Fatal("info-issue cleanup did not drop")
		}
	})

	// Review round 1, finding 3: required coverage columns must be
	// non-NULL. Zero counts are valid; NULL is not zero. A completed_pass
	// with NULL coverage fields must never slip past the coverage and
	// accounting checks — and the corruption is NOT waivable.
	t.Run("NULL target coverage is corrupt", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		// Decode pass with NULL expected/observed target coverage; content green.
		insertVerRun(t, pool, runID, verRun{
			kind: "decode", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
			verified: 2, issues: 0,
		})
		insertVerRun(t, pool, runID, verRun{
			kind: "content", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
			verified: 2, issues: 0,
			expTarget: ptrInt64(2), obsTarget: ptrInt64(2),
			expSource: ptrInt64(2), obsSource: ptrInt64(2),
		})
		manifest := exportEvidence(t, dsn)

		_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
		assertRefused(t, err, true)
		if !strings.Contains(err.Error(), "coverage is NULL") {
			t.Fatalf("refusal does not name the NULL coverage: %v", err)
		}
		// --force-unverified must NOT waive it.
		_, err = Cleanup(context.Background(), cleanupInput(dsn, manifest, func(o *CleanupOptions) {
			o.ForceUnverified = true
			o.Reason = "trying to waive NULL coverage"
		}))
		assertRefused(t, err, true)
		assertSchemaIntact(t, pool)
	})

	t.Run("NULL source coverage on content is corrupt", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		insertVerRun(t, pool, runID, verRun{
			kind: "decode", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
			verified: 2, issues: 0, expTarget: ptrInt64(2), obsTarget: ptrInt64(2),
		})
		// Content pass missing its REQUIRED source coverage pairs.
		insertVerRun(t, pool, runID, verRun{
			kind: "content", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
			verified: 2, issues: 0, expTarget: ptrInt64(2), obsTarget: ptrInt64(2),
		})
		manifest := exportEvidence(t, dsn)

		_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, func(o *CleanupOptions) {
			o.ForceUnverified = true
			o.Reason = "trying to waive NULL source coverage"
		}))
		assertRefused(t, err, true)
		if !strings.Contains(err.Error(), "source coverage is NULL") {
			t.Fatalf("refusal does not name the NULL source coverage: %v", err)
		}
		assertSchemaIntact(t, pool)
	})

	t.Run("zero coverage is valid, NULL is not zero", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		// Zero counts are legitimate recorded coverage: both pairs present,
		// both zero, accounting identity 0 == 0.
		insertVerRun(t, pool, runID, verRun{
			kind: "decode", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
			verified: 0, issues: 0, expTarget: ptrInt64(0), obsTarget: ptrInt64(0),
		})
		insertVerRun(t, pool, runID, verRun{
			kind: "content", runState: "completed_pass", phase: "complete", startRev: 4, completed: true,
			verified: 0, issues: 0,
			expTarget: ptrInt64(0), obsTarget: ptrInt64(0),
			expSource: ptrInt64(0), obsSource: ptrInt64(0),
		})
		manifest := exportEvidence(t, dsn)
		res, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
		if err != nil {
			t.Fatalf("cleanup with zero coverage error = %v — zero is valid, NULL is not", err)
		}
		if !res.Dropped {
			t.Fatal("zero-coverage cleanup did not drop")
		}
	})
}

func TestCleanupMidDropRollsBack(t *testing.T) {
	t.Parallel()
	pool, dsn, runID := seedCleanupTarget(t)
	defer pool.Close()
	seedGreenVerification(t, pool, runID)
	manifest := exportEvidence(t, dsn)

	// A FOREIGN table holding an inbound FK to a registry table trips the
	// no-CASCADE drop mid-transaction (RESTRICT tripwire).
	if _, err := pool.Exec(context.Background(), `CREATE TABLE public.foreign_dependent (
        id INT PRIMARY KEY,
        error_id BIGINT REFERENCES magnetico_migration.errors(id))`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO public.foreign_dependent (id, error_id) VALUES (1, NULL)`); err != nil {
		t.Fatal(err)
	}

	_, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil))
	assertRefused(t, err, false)
	if !strings.Contains(err.Error(), "rolled back") && !strings.Contains(err.Error(), "drop failed") {
		t.Fatalf("refusal does not state the rollback: %v", err)
	}
	// Nothing was dropped: every registry table still serves queries.
	assertSchemaIntact(t, pool)
	if _, err := state.CountRegistry(context.Background(), pool, runID); err != nil {
		t.Fatalf("registry tables damaged by the aborted drop: %v", err)
	}
}

func TestCleanupAmbiguousClassification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("schema still present means rolled back", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		_ = runID
		facts := state.Facts{Torrents: 2, Blobs: 1}
		err := classifyCommitOutcome(ctx, dsn, facts)
		if !errors.Is(err, ErrCleanupRefused) || !strings.Contains(err.Error(), "rolled back") {
			t.Fatalf("error = %v, want the rolled-back refusal", err)
		}
	})

	t.Run("absent schema is never success", func(t *testing.T) {
		t.Parallel()
		pool, dsn, runID := seedCleanupTarget(t)
		defer pool.Close()
		seedGreenVerification(t, pool, runID)
		manifest := exportEvidence(t, dsn)
		if _, err := Cleanup(context.Background(), cleanupInput(dsn, manifest, nil)); err != nil {
			t.Fatalf("green cleanup: %v", err)
		}
		facts := state.Facts{Torrents: 2, Blobs: 1}
		err := classifyCommitOutcome(ctx, dsn, facts)
		if !errors.Is(err, ErrCleanupAmbiguous) {
			t.Fatalf("error = %v, want ErrCleanupAmbiguous", err)
		}
		if !strings.Contains(err.Error(), "do not prove it") {
			t.Fatalf("ambiguous message lacks the honesty clause: %v", err)
		}

		// A production mutation makes the postconditions SUSPICIOUS — still
		// ambiguous, stronger warning, still never success.
		if _, err := pool.Exec(ctx,
			`INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on, file_count)
            VALUES (99, decode('ff','hex'), 'post', 1, 1, 1)`); err != nil {
			t.Fatal(err)
		}
		err = classifyCommitOutcome(ctx, dsn, facts)
		if !errors.Is(err, ErrCleanupAmbiguous) || !strings.Contains(err.Error(), "SUSPICIOUS") {
			t.Fatalf("error = %v, want the suspicious ambiguity", err)
		}
	})

	t.Run("re-inspection failure is ambiguous", func(t *testing.T) {
		t.Parallel()
		err := classifyCommitOutcome(ctx, "postgres://127.0.0.1:1/none", state.Facts{})
		if !errors.Is(err, ErrCleanupAmbiguous) {
			t.Fatalf("error = %v, want ErrCleanupAmbiguous", err)
		}
	})
}

func TestCleanupNoOpOnAbsentSchema(t *testing.T) {
	t.Parallel()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t) // fresh: no registry at all
	res, err := Cleanup(context.Background(), CleanupInput{
		Options: CleanupOptions{Execute: true, ConfirmBackup: true}, TargetDSN: dsn,
	})
	if err != nil {
		t.Fatalf("Cleanup on absent schema error = %v", err)
	}
	if !res.NoOp {
		t.Fatal("absent schema did not report NoOp")
	}
	if !strings.Contains(strings.Join(res.Preview, "\n"), "cannot be established from absence alone") {
		t.Fatalf("no-op preview lacks the honesty note: %v", res.Preview)
	}
}
