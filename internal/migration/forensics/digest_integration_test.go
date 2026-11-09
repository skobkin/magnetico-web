package forensics

// Registry-digest regression tests (owner review finding 2): a state
// transition invisible to counts AND data_revision — `running ->
// completed_pass`, a retry terminal transition, an interrupted-history
// insertion, even an updated_at-only touch — must change the digests, so a
// manifest exported before the transition refuses cleanup as stale.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

func digestOf(t *testing.T, dsn string) (string, *RegistryDigest) {
	t.Helper()
	ctx := context.Background()
	pool := forensicsPool(t, dsn)
	runID := seedV2Forensics(t, pool)
	seedV2Registry(t, pool, runID)

	d1, err := ComputeRegistryDigest(ctx, pool, runID, state.SchemaV2)
	if err != nil {
		t.Fatalf("ComputeRegistryDigest() error = %v", err)
	}
	d2, err := ComputeRegistryDigest(ctx, pool, runID, state.SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if *d1 != *d2 {
		t.Fatalf("digests are not deterministic:\n%+v\n%+v", *d1, *d2)
	}
	return runID, d1
}

func TestRegistryDigestTransitions(t *testing.T) {
	t.Parallel()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	pool := forensicsPool(t, dsn)
	runID, d1 := digestOf(t, dsn)
	ctx := context.Background()

	// Determinism across a fresh pool: recompute through a second pool.
	pool2 := forensicsPool(t, dsn)
	d1b, err := ComputeRegistryDigest(ctx, pool2, runID, state.SchemaV2)
	if err != nil || *d1b != *d1 {
		t.Fatalf("digest differs across pools: %+v vs %+v (%v)", d1b, d1, err)
	}

	t.Run("running to completed_pass with unchanged counts and revision", func(t *testing.T) {
		// The exact scenario finding 2 calls out: the authoritative decode
		// run finishes; mismatch_count/issue_count/data_revision stay put.
		if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.verification_runs
            SET state = 'completed_pass', phase = 'complete', completed_at = now()
            WHERE state = 'running'`); err != nil {
			t.Fatal(err)
		}
		d2, err := ComputeRegistryDigest(ctx, pool, runID, state.SchemaV2)
		if err != nil {
			t.Fatal(err)
		}
		if d2.VerificationRuns == d1.VerificationRuns {
			t.Fatal("verification_runs digest unchanged after a running->completed_pass transition")
		}
		if d2.Combined == d1.Combined {
			t.Fatal("combined digest unchanged after a running->completed_pass transition")
		}
		if d2.Progress != d1.Progress || d2.Errors != d1.Errors || d2.RetryRuns != d1.RetryRuns {
			t.Fatalf("unrelated table digests moved: %+v vs %+v", d1, d2)
		}
	})

	t.Run("retry terminal transition", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.retry_runs
            SET state = 'aborted', updated_at = now()
            WHERE state = 'completed_remaining'`); err != nil {
			t.Fatal(err)
		}
		d3, err := ComputeRegistryDigest(ctx, pool, runID, state.SchemaV2)
		if err != nil {
			t.Fatal(err)
		}
		if d3.RetryRuns == d1.RetryRuns || d3.Combined == d1.Combined {
			t.Fatal("retry_runs digest unchanged after a terminal transition")
		}
	})

	t.Run("interrupted history insertion", func(t *testing.T) {
		id := mustNewID(t)
		if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.verification_runs
            (verification_id, run_id, kind, state, phase, start_data_revision, settings)
            VALUES ($1::uuid, $2::uuid, 'content', 'interrupted', 'scanning', 4, '{}'::jsonb)`,
			id, runID); err != nil {
			t.Fatal(err)
		}
		d4, err := ComputeRegistryDigest(ctx, pool, runID, state.SchemaV2)
		if err != nil {
			t.Fatal(err)
		}
		if d4.VerificationRuns == d1.VerificationRuns || d4.Combined == d1.Combined {
			t.Fatal("verification_runs digest unchanged after inserting an interrupted run")
		}
	})

	t.Run("updated_at-only progress touch", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.progress
            SET updated_at = updated_at + interval '1 microsecond'`); err != nil {
			t.Fatal(err)
		}
		d5, err := ComputeRegistryDigest(ctx, pool, runID, state.SchemaV2)
		if err != nil {
			t.Fatal(err)
		}
		if d5.Progress == d1.Progress {
			t.Fatal("progress digest unchanged after an updated_at-only touch")
		}
	})

	t.Run("new error row", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.errors
            (run_id, scope, source_id, stage, category, code)
            VALUES ($1::uuid, 'record', 42, 'validate', 'integrity', 'invalid_metadata')`, runID); err != nil {
			t.Fatal(err)
		}
		d6, err := ComputeRegistryDigest(ctx, pool, runID, state.SchemaV2)
		if err != nil {
			t.Fatal(err)
		}
		if d6.Errors == d1.Errors || d6.Combined == d1.Combined {
			t.Fatal("errors digest unchanged after inserting an error row")
		}
	})
}

func TestManifestBindsRegistryState(t *testing.T) {
	t.Parallel()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	pool := forensicsPool(t, dsn)
	ctx := context.Background()
	runID := seedV2Forensics(t, pool)
	seedV2Registry(t, pool, runID)

	out := filepath.Join(t.TempDir(), "evidence.jsonl")
	if _, err := ExportErrors(ctx, ExportInput{
		Options:   ExportOptions{OutputPath: out, WriteManifest: true, Limits: DefaultLimits()},
		TargetDSN: dsn,
	}); err != nil {
		t.Fatal(err)
	}
	manifestPath := out + ".manifest.json"

	// Fresh export: manifest validates and binds the live registry.
	m, err := ValidateManifestFile(manifestPath)
	if err != nil {
		t.Fatalf("ValidateManifestFile() error = %v", err)
	}
	counts, err := state.CountRegistry(ctx, pool, runID)
	if err != nil {
		t.Fatal(err)
	}
	live, err := Preflight(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAgainstRegistry(m, live.Progress.RunID, live.Progress.DataRevision, counts); err != nil {
		t.Fatalf("validateAgainstRegistry() error = %v", err)
	}
	digest, err := ComputeRegistryDigest(ctx, pool, runID, state.SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if m.RegistryState != *digest {
		t.Fatalf("manifest digests %+v != live %+v", m.RegistryState, *digest)
	}
	live.Release()

	// Tampered artifact byte: re-hash refuses.
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "orig.jsonl")
	if err := os.WriteFile(backup, body, 0o600); err != nil {
		t.Fatal(err)
	}
	// Rewrite the artifact in place (same size, one flipped byte).
	mutated := append([]byte(nil), body...)
	for i, b := range mutated {
		if b == 'a' {
			mutated[i] = 'b'
			break
		}
	}
	if err := os.Chmod(out, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateManifestFile(manifestPath); !errors.Is(err, ErrEvidenceRejected) {
		t.Fatalf("tampered artifact error = %v, want ErrEvidenceRejected", err)
	}
	// Restore.
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateManifestFile(manifestPath); err != nil {
		t.Fatalf("restored artifact error = %v", err)
	}

	// Registry moves after the export (counts identical, revision identical):
	// the manifest's registry_state is stale; a fresh export rebinding it.
	if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.verification_runs
        SET state = 'aborted', updated_at = now() WHERE state = 'running'`); err != nil {
		t.Fatal(err)
	}
	stale := readManifest(t, manifestPath)
	digestNow, err := ComputeRegistryDigest(ctx, pool, runID, state.SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if stale.RegistryState.VerificationRuns == digestNow.VerificationRuns {
		t.Fatal("fixture transition did not move the digest — the regression would not fire")
	}

	// Event-count drift: add an error row; validateAgainstRegistry refuses.
	if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.errors
        (run_id, scope, source_id, stage, category, code)
        VALUES ($1::uuid, 'record', 99, 'validate', 'integrity', 'invalid_metadata')`, runID); err != nil {
		t.Fatal(err)
	}
	counts2, err := state.CountRegistry(ctx, pool, runID)
	if err != nil {
		t.Fatal(err)
	}
	live2, err := Preflight(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer live2.Release()
	if err := validateAgainstRegistry(&stale, live2.Progress.RunID, live2.Progress.DataRevision, counts2); !errors.Is(err, ErrEvidenceRejected) {
		t.Fatalf("count-drift error = %v, want ErrEvidenceRejected", err)
	}
}

func TestManifestStructuralRefusals(t *testing.T) {
	t.Parallel()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	dir := t.TempDir()
	ctx := context.Background()
	runID := seedV2Forensics(t, forensicsPool(t, dsn))
	seedV2Registry(t, forensicsPool(t, dsn), runID)

	out := filepath.Join(dir, "m.jsonl")
	if _, err := ExportErrors(ctx, ExportInput{
		Options:   ExportOptions{OutputPath: out, WriteManifest: true, Limits: DefaultLimits()},
		TargetDSN: dsn,
	}); err != nil {
		t.Fatal(err)
	}
	manifestPath := out + ".manifest.json"

	t.Run("artifact relocation", func(t *testing.T) {
		moved := filepath.Join(t.TempDir(), "m.jsonl.manifest.json")
		body, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(moved, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateManifestFile(moved); !errors.Is(err, ErrEvidenceRejected) {
			t.Fatalf("relocated manifest error = %v, want ErrEvidenceRejected", err)
		}
	})

	t.Run("hand-edited coverage", func(t *testing.T) {
		broken := filepath.Join(t.TempDir(), "m.jsonl.manifest.json")
		m := readManifest(t, manifestPath)
		m.Coverage = "partial"
		writeJSONFile(t, broken, m)
		if _, err := ValidateManifestFile(broken); !errors.Is(err, ErrEvidenceRejected) {
			t.Fatalf("partial-coverage error = %v, want ErrEvidenceRejected", err)
		}
	})

	t.Run("unknown fields refused", func(t *testing.T) {
		broken := filepath.Join(t.TempDir(), "m.jsonl.manifest.json")
		body, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		poisoned := strings.Replace(string(body), "{", "{\"surprise\":1,", 1)
		if err := os.WriteFile(broken, []byte(poisoned), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateManifestFile(broken); !errors.Is(err, ErrEvidenceRejected) {
			t.Fatalf("unknown-field error = %v, want ErrEvidenceRejected", err)
		}
	})

	t.Run("symlinked manifest refused", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link.manifest.json")
		if err := os.Symlink(manifestPath, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateManifestFile(link); !errors.Is(err, ErrEvidenceRejected) {
			t.Fatalf("symlink error = %v, want ErrEvidenceRejected", err)
		}
	})
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
