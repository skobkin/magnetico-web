package forensics

// Gated atomic cleanup (`migrate cleanup`, spec §6): a destructive,
// fail-closed single-transaction teardown of magnetico_migration — the
// deliberate safety inverse of the fail-open export.
//
// Gates G1–G10 (plan §6): G1 run lock; G2 schema exactly v2; G3 exactly one
// progress row in a completion phase (import_complete_with_errors only via
// the override, and only when RECONCILED — G5 is never waivable); G4 strict
// magneticod schema via persistence.MakeDatabase (pre-transaction only —
// documented deviation: MakeDatabase is DSN-scoped and the drop statements
// reference only fixed magnetico_migration identifiers); G5 reconcile
// (never waivable); G6 zero unresolved findings (waivable); G7 verification
// verdicts with INDEPENDENT persisted-issue validation (owner finding 3 +
// the final correction: issue_count == persisted rows, severity recounted
// independently, zero blocking-severity issues for a normal pass,
// target-row accounting identity, coverage separate — and a completed_pass
// coexisting with blocking issues is CORRUPT state, never waivable); G8
// backup attestation; G9 evidence (manifest file + live-registry binding
// pre-tx, event counts + full registry-state digest recompare in-tx); G10
// flag coherence (validated before any connection).
//
// A lost COMMIT acknowledgment is NEVER success (owner finding 6): the
// outcome is classified on a fresh connection — schema still present means
// definitively rolled back; schema absent is at best AMBIGUOUS
// (ErrCleanupAmbiguous, exit 1). The destructive DDL is never retried.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/persistence"
)

// CleanupOptions mirrors the CLI flags (authorization booleans are CLI-only;
// no yaml keys).
type CleanupOptions struct {
	// DryRun: non-destructive preview (the default behavior).
	DryRun bool
	// Execute: the single noninteractive confirmation.
	Execute bool
	// ConfirmBackup attests an independent backup of the target exists.
	ConfirmBackup bool
	// ConfirmExport attests a COMPLETE export with a validated manifest is
	// independently retained. Normal evidence path only.
	ConfirmExport bool
	// EvidenceManifest is the retained export's manifest path.
	EvidenceManifest string
	// ForceUnverified waives ORDINARY verification/refusal failures (stale
	// or failed verdicts, unresolved findings) — never corrupt state.
	ForceUnverified bool
	// Reason is the mandatory audit reason for ForceUnverified (1–512 B).
	Reason string
	// AllowMissingExportEvidence + ConfirmHistoryLoss: the exceptional,
	// path-exclusive evidence attestation (no complete export exists; all
	// migration history will be irrecoverably destroyed).
	AllowMissingExportEvidence bool
	ConfirmHistoryLoss         bool
}

// CleanupInput carries the validated options and the target DSN.
type CleanupInput struct {
	Options   CleanupOptions
	TargetDSN string
}

// GateStatus is a gate outcome in the preview/report.
type GateStatus string

const (
	GatePass   GateStatus = "PASS"
	GateFail   GateStatus = "FAIL"
	GateWaived GateStatus = "WAIVED"
	GateSkip   GateStatus = "SKIP"
)

// GateResult is one gate's outcome and reason (secret-free).
type GateResult struct {
	Name     string
	Status   GateStatus
	Waivable bool
	Detail   string
}

// CleanupResult reports what the command observed and did. On a nil error:
// NoOp (nothing to clean) or Dropped (a definite COMMIT acknowledgment).
// Refusals and ambiguous outcomes are errors (ErrCleanupRefused /
// ErrCleanupAmbiguous); the fields still describe how far the run got.
type CleanupResult struct {
	NoOp          bool
	Dropped       bool
	RunID         string
	Phase         string
	DataRevision  int64
	Class         state.SchemaClass
	Counts        state.RegistryCounts
	Latest        map[string]*state.VerificationRun
	Gates         []GateResult
	Blockers      []string
	ExpectedDrops []string
	Reason        string
	Preview       []string
}

// maxReasonBytes bounds the audit reason.
const maxReasonBytes = 512

// expectedDropList mirrors state.DropOperationalSchema's fixed statement
// list for the preview (the executed DDL itself lives in the state package;
// its no-CASCADE/no-IF-EXISTS invariants are pinned by state's own tests).
func expectedDropList() []string {
	return []string{
		"DROP TABLE magnetico_migration.verification_issues",
		"DROP TABLE magnetico_migration.verification_runs",
		"DROP TABLE magnetico_migration.retry_attempts",
		"DROP TABLE magnetico_migration.retry_runs",
		"DROP TABLE magnetico_migration.errors",
		"DROP TABLE magnetico_migration.progress",
		"DROP TABLE magnetico_migration.operational_schema",
		"DROP SCHEMA magnetico_migration RESTRICT",
	}
}

// validateCleanupFlags is gate G10: flag coherence, before any connection.
// Contradictions and unmet flag-shape requirements are configuration
// errors; MISSING ATTESTATIONS (no --confirm-backup, no evidence) are
// gate refusals, evaluated with the rest.
func validateCleanupFlags(opts CleanupOptions) error {
	if opts.DryRun && opts.Execute {
		return &OptionsError{Detail: "--dry-run and --execute are mutually exclusive"}
	}
	if opts.Reason != "" && strings.TrimSpace(opts.Reason) == "" {
		return &OptionsError{Detail: "--reason is blank"}
	}
	if len(opts.Reason) > maxReasonBytes {
		return &OptionsError{Detail: fmt.Sprintf("--reason exceeds %d bytes", maxReasonBytes)}
	}
	if opts.ForceUnverified {
		if opts.Reason == "" {
			return &OptionsError{Detail: "--force-unverified requires a nonempty --reason"}
		}
		if !opts.Execute {
			return &OptionsError{Detail: "--force-unverified requires --execute"}
		}
	}
	normal := opts.ConfirmExport || opts.EvidenceManifest != ""
	exceptional := opts.AllowMissingExportEvidence || opts.ConfirmHistoryLoss
	if normal && (!opts.ConfirmExport || opts.EvidenceManifest == "") {
		return &OptionsError{Detail: "--confirm-export and --evidence-manifest PATH are a pair: the attestation must name the retained evidence it covers"}
	}
	if exceptional && (!opts.AllowMissingExportEvidence || !opts.ConfirmHistoryLoss) {
		return &OptionsError{Detail: "the missing-evidence exception requires BOTH --allow-missing-export-evidence and --confirm-history-loss"}
	}
	if exceptional && (!opts.ForceUnverified || opts.Reason == "") {
		return &OptionsError{Detail: "--allow-missing-export-evidence requires --force-unverified --reason (history loss is never attested casually)"}
	}
	if normal && exceptional {
		return &OptionsError{Detail: "--confirm-export/--evidence-manifest cannot coexist with --allow-missing-export-evidence/--confirm-history-loss: the evidence attestations are path-exclusive"}
	}
	return nil
}

// issueTally is an independent recount of one verification run's persisted
// issues (owner finding 3: never trust the run's own counters).
type issueTally struct {
	Total      int64
	BySeverity map[string]int64
}

func (t issueTally) blocking() int64 {
	return t.BySeverity[state.IssueMismatch] + t.BySeverity[state.IssueCoverage] + t.BySeverity[state.IssueUnverifiable]
}

// cleanupState is everything the gates read, loaded once per queryer (pool
// pre-transaction, tx in-transaction) so the recheck sees one snapshot.
type cleanupState struct {
	class    state.SchemaClass
	progress *state.Progress
	counts   state.RegistryCounts
	facts    state.Facts
	latest   map[string]*state.VerificationRun
	issues   map[string]issueTally // by verification_id
}

func loadCleanupState(ctx context.Context, q state.Queryer, class state.SchemaClass) (*cleanupState, error) {
	progress, err := state.ReadProgressRaw(ctx, q, class)
	if err != nil {
		return nil, err
	}
	counts, err := state.CountRegistry(ctx, q, progress.RunID)
	if err != nil {
		return nil, err
	}
	facts, err := target.TargetFacts(ctx, q, progress.LastProcessedID, progress.RunID)
	if err != nil {
		return nil, err
	}
	latest := map[string]*state.VerificationRun{}
	for _, kind := range []string{state.VerificationKindDecode, state.VerificationKindContent} {
		run, err := state.LatestVerification(ctx, q, kind)
		if err != nil {
			return nil, err
		}
		latest[kind] = run
	}
	tally, err := tallyVerificationIssues(ctx, q)
	if err != nil {
		return nil, err
	}
	return &cleanupState{
		class: class, progress: progress, counts: counts,
		facts: facts, latest: latest, issues: tally,
	}, nil
}

// tallyVerificationIssues recounts the persisted issues by verification_id
// and severity (table-wide keyset walk — with exactly one progress row,
// every verification run belongs to this run).
func tallyVerificationIssues(ctx context.Context, q state.Queryer) (map[string]issueTally, error) {
	out := map[string]issueTally{}
	after := int64(0)
	for {
		issues, err := state.ListVerificationIssuesAfter(ctx, q, after, keysetPage)
		if err != nil {
			return nil, err
		}
		for _, r := range issues {
			t := out[r.VerificationID]
			if t.BySeverity == nil {
				t.BySeverity = map[string]int64{}
			}
			t.Total++
			t.BySeverity[r.Severity]++
			out[r.VerificationID] = t
		}
		if len(issues) < keysetPage {
			return out, nil
		}
		after = issues[len(issues)-1].ID
	}
}

// Cleanup runs the gates, the preview and (with --execute) the atomic drop.
func Cleanup(ctx context.Context, in CleanupInput) (*CleanupResult, error) {
	opts := in.Options
	if err := validateCleanupFlags(opts); err != nil {
		return nil, err // G10: configuration errors precede any connection
	}

	pool, err := target.OpenPool(ctx, in.TargetDSN)
	if err != nil {
		return nil, err
	}
	poolClosed := false
	closePool := func() {
		if !poolClosed {
			pool.Close()
			poolClosed = true
		}
	}

	lock, err := state.AcquireRunLock(ctx, in.TargetDSN)
	if err != nil {
		closePool()
		return nil, err
	}
	defer lock.Release(context.WithoutCancel(ctx))

	inspection, err := state.InspectOperationalSchema(ctx, pool)
	if err != nil {
		closePool()
		return nil, err
	}

	res := &CleanupResult{Class: inspection.Class, ExpectedDrops: expectedDropList()}
	refuse := func(format string, args ...any) (*CleanupResult, error) {
		closePool()
		return nil, fmt.Errorf("%w: %s", ErrCleanupRefused, fmt.Sprintf(format, args...))
	}

	// Already-cleaned: absence is a harmless no-op — and honestly reported
	// as unprovable ("past cleanup cannot be established from absence alone").
	switch inspection.Class {
	case state.SchemaAbsent:
		closePool()
		return &CleanupResult{
			NoOp:          true,
			ExpectedDrops: expectedDropList(),
			Preview: []string{
				"cleanup: no magnetico_migration schema exists — nothing to clean up",
				"note: a past cleanup cannot be established from absence alone; " +
					"this no-op is not proof that one happened",
			},
		}, nil
	case state.SchemaUnrecognized:
		return refuse("magnetico_migration is not an exact v1 or v2 schema — refusing as drift: %s",
			strings.Join(inspection.Details, "; "))
	case state.SchemaV1:
		// Never upgrades, never tears down a v1 schema (preview included).
		return refuse("operational schema is v1 — cleanup requires v2; finish the import " +
			"(the migrator upgrades v1 transparently), or tear the schema down manually")
	case state.SchemaV2:
		// gated below
	default:
		return refuse("unhandled schema class %v", inspection.Class)
	}

	cs, err := loadCleanupState(ctx, pool, state.SchemaV2)
	if err != nil {
		closePool()
		if errors.Is(err, state.ErrNoRunState) || errors.Is(err, state.ErrRunStateCorrupt) {
			return nil, fmt.Errorf("%w: %w", ErrCleanupRefused,
				&state.StateError{Err: state.ErrRunStateCorrupt,
					Details: []string{"v2 operational schema holds no usable single progress row: " + err.Error()}})
		}
		return nil, err
	}
	res.RunID = cs.progress.RunID
	res.Phase = string(cs.progress.Phase)
	res.DataRevision = cs.progress.DataRevision
	res.Counts = cs.counts
	res.Latest = cs.latest
	res.Reason = opts.Reason

	gates, corrupt, blockers, err := evaluateCleanupGates(ctx, pool, in.TargetDSN, opts, cs, true)
	if err != nil {
		closePool()
		return nil, err
	}
	res.Gates = gates
	res.Blockers = blockers
	res.Preview = renderCleanupPreview(res, cs, opts)

	if !opts.Execute {
		closePool()
		return res, nil // dry-run: blockers are printed findings, not errors
	}

	if len(corrupt) > 0 {
		closePool()
		return nil, fmt.Errorf("%w: %w", ErrCleanupRefused, &state.StateError{
			Err: state.ErrRunStateCorrupt, Details: corrupt})
	}
	if len(blockers) > 0 {
		return refuse("%s", strings.Join(blockers, "; "))
	}

	dropped, err := runDropTransaction(ctx, pool, in.TargetDSN, opts)
	closePool()
	if err != nil {
		return nil, err
	}
	res.Dropped = dropped
	return res, nil
}

// evaluateCleanupGates evaluates G1–G9 on q. preTx enables G4's
// MakeDatabase check (DSN-scoped, pre-transaction only — documented
// deviation A6); G9 re-reads and re-validates the manifest on the given
// queryer either way, so the drop transaction recomputes the registry
// digests from its own snapshot. Waivable failures flip to WAIVED under
// --force-unverified; corrupt findings never do.
func evaluateCleanupGates(ctx context.Context, q state.Queryer, targetDSN string,
	opts CleanupOptions, cs *cleanupState, preTx bool,
) ([]GateResult, []string, []string, error) {
	gates := []GateResult{}
	var corrupt, blockers []string

	fail := func(name string, waivable bool, detail string) {
		status := GateFail
		if waivable && opts.ForceUnverified {
			status = GateWaived
		}
		gates = append(gates, GateResult{Name: name, Status: status, Waivable: waivable, Detail: detail})
		if status == GateFail {
			blockers = append(blockers, name+": "+detail)
		}
	}
	block := func(name, detail string) { fail(name, false, detail) }
	pass := func(name, detail string) {
		gates = append(gates, GateResult{Name: name, Status: GatePass, Detail: detail})
	}

	// G1 — the run lock is held for the whole command (acquired in Cleanup).
	pass("G1 lock", "advisory run lock held for the whole command")

	// G2 — exactly v2, re-classified on this queryer.
	insp, err := state.InspectOperationalSchema(ctx, q)
	if err != nil {
		return nil, nil, nil, err
	}
	if insp.Class == state.SchemaV2 {
		pass("G2 operational schema", "exactly v2")
	} else {
		block("G2 operational schema",
			fmt.Sprintf("operational schema is %s, want exactly v2", insp.Class))
	}

	// G3 — one progress row in a completion phase. with_errors needs the
	// override AND a reconciled state (G5 is independent and never waivable).
	progress, err := state.ReadProgressRaw(ctx, q, cs.class)
	if err != nil {
		if errors.Is(err, state.ErrNoRunState) || errors.Is(err, state.ErrRunStateCorrupt) {
			block("G3 progress row", "no usable single progress row: "+err.Error())
		} else {
			return nil, nil, nil, err
		}
	} else {
		switch progress.Phase {
		case state.PhaseImportComplete:
			pass("G3 progress row", fmt.Sprintf("phase %s, one progress row", progress.Phase))
		case state.PhaseCompleteWithErrors:
			fail("G3 progress row", true, fmt.Sprintf(
				"phase %s — cleanup normally requires %s; the override accepts it only together with a reconciled state (G5)",
				progress.Phase, state.PhaseImportComplete))
		default:
			block("G3 progress row", fmt.Sprintf(
				"phase %q is not a completion phase — the migration never completes in this state", progress.Phase))
		}
	}

	// G4 — strict production schema (pre-transaction only; documented
	// deviation A6: MakeDatabase is DSN-scoped, the drops reference only
	// fixed magnetico_migration identifiers).
	if preTx {
		store, err := persistence.MakeDatabase(ctx, targetDSN)
		if err != nil {
			block("G4 production schema", "strict magneticod verification failed: "+err.Error())
		} else {
			_ = store.Close()
			pass("G4 production schema", "exactly the expected magneticod objects")
		}
	} else {
		gates = append(gates, GateResult{Name: "G4 production schema", Status: GateSkip,
			Detail: "verified pre-transaction; the drops cannot touch magneticod"})
	}

	// G5 — reconcile target data against persisted state (never waivable).
	if err := state.Reconcile(cs.progress, cs.facts); err != nil {
		var se *state.StateError
		detail := err.Error()
		if errors.As(err, &se) {
			detail = strings.Join(se.Details, "; ")
		}
		block("G5 reconcile", detail)
	} else {
		pass("G5 reconcile", fmt.Sprintf("torrents=%d blobs=%d match the recorded counters",
			cs.facts.Torrents, cs.facts.Blobs))
	}

	// G6 — zero unresolved findings (waivable).
	if cs.facts.RecordErrors == 0 && cs.facts.UnresolvedIntegrity == 0 {
		pass("G6 unresolved findings", "no unresolved record errors or integrity incidents")
	} else {
		fail("G6 unresolved findings", true, fmt.Sprintf(
			"unresolved record errors=%d integrity incidents=%d — waivable with --force-unverified --reason",
			cs.facts.RecordErrors, cs.facts.UnresolvedIntegrity))
	}

	// G7 — verification verdicts with independent persisted-issue validation.
	g7Corrupt, g7Reasons, g7Details := evaluateVerificationGates(cs, progress)
	corrupt = append(corrupt, g7Corrupt...)
	if len(g7Corrupt) > 0 {
		fail("G7 verification", false, "CORRUPT verification state — never waivable: "+
			strings.Join(g7Corrupt, "; "))
	} else if len(g7Reasons) > 0 {
		fail("G7 verification", true, strings.Join(g7Reasons, "; ")+
			" — waivable with --force-unverified --reason")
	} else {
		pass("G7 verification", strings.Join(g7Details, "; "))
	}

	// G8 — backup attestation.
	if opts.ConfirmBackup {
		pass("G8 backup attestation", "--confirm-backup provided")
	} else {
		block("G8 backup attestation",
			"--confirm-backup is required: the drop irreversibly destroys the migration history")
	}

	// G9 — evidence.
	exceptional := opts.AllowMissingExportEvidence && opts.ConfirmHistoryLoss
	switch {
	case exceptional:
		gates = append(gates, GateResult{Name: "G9 evidence", Status: GateSkip, Detail: "missing-evidence exception attested (--confirm-history-loss): NO complete export " +
			"evidence exists and ALL migration history will be irrecoverably destroyed"})
	case !opts.ConfirmExport:
		block("G9 evidence",
			"no evidence attestation: provide --confirm-export --evidence-manifest PATH, "+
				"or the explicit --allow-missing-export-evidence --confirm-history-loss exception")
	default:
		m, evalErr := validateEvidence(ctx, q, opts.EvidenceManifest, cs)
		if evalErr != nil {
			block("G9 evidence", evalErr.Error())
		} else {
			pass("G9 evidence", fmt.Sprintf(
				"manifest binds artifact %s (sha256 %s…) to this run at revision %d",
				m.Artifact, m.SHA256[:12], m.DataRevision))
		}
	}

	// G10 — validated before any connection.
	gates = append(gates, GateResult{Name: "G10 flag coherence", Status: GateSkip,
		Detail: "validated before the connection was opened"})

	return gates, corrupt, blockers, nil
}

// validateEvidence is gate G9's manifest half: file structure + hash
// (ValidateManifestFile), live binding (run/revision/counts) and — on the
// drop transaction — the FULL registry-state digest recompute (owner
// finding 2). The exception path never calls this.
func validateEvidence(ctx context.Context, q state.Queryer, manifestPath string, cs *cleanupState) (*Manifest, error) {
	m, err := ValidateManifestFile(manifestPath)
	if err != nil {
		return nil, err
	}
	if err := validateAgainstRegistry(m, cs.progress.RunID, cs.progress.DataRevision, cs.counts); err != nil {
		return nil, err
	}
	digest, err := ComputeRegistryDigest(ctx, q, cs.progress.RunID, cs.class)
	if err != nil {
		return nil, fmt.Errorf("registry digest recompute failed: %w", err)
	}
	if *digest != m.RegistryState {
		return nil, fmt.Errorf("%w: registry-state digests differ from the manifest — "+
			"the registry moved since the export (stale evidence; re-export)", ErrEvidenceRejected)
	}
	return m, nil
}

// evaluateVerificationGates is gate G7's core (owner finding 3, corrected):
// per kind decode+content, the authoritative run must carry a current,
// complete completed_pass verdict with exact expected/observed coverage;
// its persisted issues are recounted INDEPENDENTLY (issue_count equality,
// zero blocking severity for a normal pass, target-row accounting
// identity). A completed_pass coexisting with blocking issues is corrupt —
// never waivable; ordinary verdict failures are waivable.
func evaluateVerificationGates(cs *cleanupState, progress *state.Progress) (corrupt, reasons, details []string) {
	for _, kind := range []string{state.VerificationKindDecode, state.VerificationKindContent} {
		run := cs.latest[kind]
		tally := cs.issues[verificationID(run)]
		c, r, d := verificationGate(kind, run, tally, progress)
		corrupt = append(corrupt, c...)
		reasons = append(reasons, r...)
		details = append(details, d...)
	}
	return corrupt, reasons, details
}

// verificationID returns the run's verification_id ("" for a missing run;
// its tally is the zero tally).
func verificationID(run *state.VerificationRun) string {
	if run == nil {
		return ""
	}
	return run.VerificationID
}

func verificationGate(kind string, run *state.VerificationRun, tally issueTally,
	progress *state.Progress,
) (corrupt, reasons, details []string) {
	if run == nil {
		return nil, []string{"no verification run of kind " + kind +
			" exists — the migration was never verified"}, nil
	}

	// Required coverage columns (review round 1, finding 3): a completed
	// verdict must RECORD its coverage — zero counts are valid, NULL is not
	// zero. A corrupted run with NULL coverage could otherwise bypass the
	// coverage and accounting checks below. Missing required coverage is
	// CORRUPT state, never waivable.
	if run.ExpectedTargetIDs == nil || run.ObservedTargetIDs == nil {
		corrupt = append(corrupt, fmt.Sprintf(
			"%s run seq %d: required target coverage is NULL (expected_target_ids=%v observed_target_ids=%v) "+
				"— corrupt verification state", kind, run.Seq, run.ExpectedTargetIDs, run.ObservedTargetIDs))
	}
	if run.Kind == state.VerificationKindContent && (run.ExpectedSourceIDs == nil || run.ObservedSourceIDs == nil) {
		corrupt = append(corrupt, fmt.Sprintf(
			"%s run seq %d: required source coverage is NULL (expected_source_ids=%v observed_source_ids=%v) "+
				"— corrupt verification state", kind, run.Seq, run.ExpectedSourceIDs, run.ObservedSourceIDs))
	}

	// Independent persisted-issue validation.
	if tally.Total != run.IssueCount {
		corrupt = append(corrupt, fmt.Sprintf(
			"%s run seq %d: issue_count %d != %d persisted issue rows",
			kind, run.Seq, run.IssueCount, tally.Total))
	}
	if run.ObservedTargetIDs != nil {
		accounted := run.VerifiedCount + run.MismatchCount + run.UnverifiableCount + run.AssumedSingleCount
		if accounted != *run.ObservedTargetIDs {
			corrupt = append(corrupt, fmt.Sprintf(
				"%s run seq %d: target-row accounting verified=%d+mismatch=%d+unverifiable=%d+assumed_single=%d = %d != observed_target_ids %d",
				kind, run.Seq, run.VerifiedCount, run.MismatchCount, run.UnverifiableCount,
				run.AssumedSingleCount, accounted, *run.ObservedTargetIDs))
		}
	}
	blocking := tally.blocking()
	if blocking > 0 && run.State == state.VerificationCompletedPass {
		corrupt = append(corrupt, fmt.Sprintf(
			"%s run seq %d: state completed_pass coexists with %d blocking-severity issue(s) "+
				"(mismatch=%d coverage=%d unverifiable=%d) — corrupt verification state",
			kind, run.Seq, blocking,
			tally.BySeverity[state.IssueMismatch], tally.BySeverity[state.IssueCoverage],
			tally.BySeverity[state.IssueUnverifiable]))
	}

	// Ordinary verdict gate (waivable). Severity recounts are independent:
	// mismatch_count/unverifiable_count are deliberately NOT compared
	// against the persisted severity histogram (04B semantics: source-only
	// discrepancies and aggregate findings do not increment target-row
	// counters).
	if run.State != state.VerificationCompletedPass {
		reasons = append(reasons, fmt.Sprintf("%s: state %s, want completed_pass", kind, run.State))
	}
	if run.Phase != state.VerificationPhaseComplete {
		reasons = append(reasons, fmt.Sprintf("%s: phase %s, want complete", kind, run.Phase))
	}
	if run.StartDataRevision != progress.DataRevision {
		reasons = append(reasons, fmt.Sprintf("%s: start_data_revision %d != progress data_revision %d (stale verdict)",
			kind, run.StartDataRevision, progress.DataRevision))
	}
	if ok, why := coverageHolds(run); !ok {
		reasons = append(reasons, kind+": "+why)
	}
	if blocking > 0 && run.State != state.VerificationCompletedPass {
		reasons = append(reasons, fmt.Sprintf(
			"%s: authoritative run carries %d blocking-severity issue(s) "+
				"(mismatch=%d coverage=%d unverifiable=%d)",
			kind, blocking, tally.BySeverity[state.IssueMismatch],
			tally.BySeverity[state.IssueCoverage], tally.BySeverity[state.IssueUnverifiable]))
	}

	if len(corrupt) > 0 || len(reasons) > 0 {
		return corrupt, reasons, nil
	}
	detail := fmt.Sprintf("%s: seq %d completed_pass at revision %d", kind, run.Seq, run.StartDataRevision)
	if info := tally.BySeverity[state.IssueInfo]; info > 0 {
		detail += fmt.Sprintf("; %d info issue(s) (permitted)", info)
	}
	return nil, nil, []string{detail}
}

// coverageHolds validates expected/observed coverage separately (owner
// finding 3, item 5) for each kind's id pairs. Required NULL coverage is
// rejected upstream as corrupt (review round 1, finding 3); this comparison
// runs on whatever pairs are present.
func coverageHolds(run *state.VerificationRun) (bool, string) {
	check := func(label string, exp, obs *int64) (bool, string) {
		if exp == nil || obs == nil {
			return true, ""
		}
		if *exp != *obs {
			return false, fmt.Sprintf("%s coverage expected=%d observed=%d", label, *exp, *obs)
		}
		return true, ""
	}
	if ok, why := check("target", run.ExpectedTargetIDs, run.ObservedTargetIDs); !ok {
		return false, why
	}
	if run.Kind == state.VerificationKindContent {
		if ok, why := check("source", run.ExpectedSourceIDs, run.ObservedSourceIDs); !ok {
			return false, why
		}
	}
	return true, ""
}

// runDropTransaction opens the drop transaction, rechecks every gate
// against the transaction's own snapshot, drops, and commits. A lost
// COMMIT acknowledgment is classified — never assumed success.
func runDropTransaction(ctx context.Context, pool *pgxpool.Pool, targetDSN string, opts CleanupOptions) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	cs, err := loadCleanupState(ctx, tx, state.SchemaV2)
	if err != nil {
		if errors.Is(err, state.ErrRunStateCorrupt) || errors.Is(err, state.ErrNoRunState) {
			return false, fmt.Errorf("%w: in-transaction recheck found inconsistent state: %w "+
				"(rolled back; nothing dropped)", ErrCleanupRefused, err)
		}
		return false, fmt.Errorf("in-transaction state load failed: %w", err)
	}

	_, corrupt, blockers, err := evaluateCleanupGates(ctx, tx, targetDSN, opts, cs, false)
	if err != nil {
		return false, err
	}
	if len(corrupt) > 0 {
		return false, fmt.Errorf("%w: in-transaction recheck found corrupt state: %s "+
			"(rolled back; nothing dropped)", ErrCleanupRefused, strings.Join(corrupt, "; "))
	}
	if len(blockers) > 0 {
		return false, fmt.Errorf("%w: in-transaction recheck refused: %s "+
			"(rolled back; nothing dropped)", ErrCleanupRefused, strings.Join(blockers, "; "))
	}

	if err := state.DropOperationalSchema(ctx, tx); err != nil {
		return false, fmt.Errorf("%w: drop failed — the transaction rolled back and nothing was dropped: %w",
			ErrCleanupRefused, err)
	}

	if err := tx.Commit(ctx); err != nil {
		// Owner finding 6: verified postconditions ≠ proven acknowledgment.
		return false, classifyCommitOutcome(ctx, targetDSN, cs.facts)
	}
	committed = true
	return true, nil
}

// classifyCommitOutcome resolves a lost COMMIT acknowledgment (owner
// finding 6) on a FRESH connection: schema still present → definitively
// rolled back; schema absent → AMBIGUOUS even with consistent production
// postconditions (absence + unchanged counts cannot prove byte-for-byte
// production equality or transaction provenance). Never auto-retries.
func classifyCommitOutcome(ctx context.Context, targetDSN string, facts state.Facts) error {
	insCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(insCtx, targetDSN)
	if err != nil {
		// The driver fragment is sanitized at composition so the CLI's
		// bounded wrap-detail rendering stays controlled content (the same
		// trust rule as the 04B engine-composed wrap suffixes).
		return fmt.Errorf("%w: the COMMIT acknowledgment was lost and the target could not "+
			"be re-inspected (%s) — inspect the target before relying on the result; do not re-run blindly",
			ErrCleanupAmbiguous, sanitizeDetailText(err.Error()))
	}
	defer conn.Close(insCtx)

	insp, err := state.InspectOperationalSchema(insCtx, conn)
	if err != nil {
		return fmt.Errorf("%w: the COMMIT acknowledgment was lost and the schema inspection "+
			"failed (%s) — inspect the target before relying on the result; do not re-run blindly",
			ErrCleanupAmbiguous, sanitizeDetailText(err.Error()))
	}
	if insp.Class != state.SchemaAbsent {
		return fmt.Errorf("%w: the operational schema is still present (%s), so the drop "+
			"transaction rolled back — nothing was dropped",
			ErrCleanupRefused, insp.Class)
	}
	consistent, why := productionSnapshotMatches(insCtx, conn, facts)
	if !consistent {
		return fmt.Errorf("%w: the COMMIT acknowledgment was lost; the operational schema is "+
			"gone but the production postconditions are SUSPICIOUS (%s) — inspect the target "+
			"immediately; do not re-run", ErrCleanupAmbiguous, sanitizeDetailText(why))
	}
	return fmt.Errorf("%w: the COMMIT acknowledgment was lost; catalog postconditions are "+
		"consistent with success (schema gone, production counts unchanged) but do not prove "+
		"it — inspect the target before relying on the result; do not re-run blindly",
		ErrCleanupAmbiguous)
}

// productionSnapshotMatches compares the live production counts with the
// pre-drop snapshot (best-effort consistency evidence, explicitly NOT proof).
func productionSnapshotMatches(ctx context.Context, q state.Queryer, facts state.Facts) (bool, string) {
	var torrentsReg, blobsReg *string
	if err := q.QueryRow(ctx,
		`SELECT to_regclass('magneticod.torrents')::text, to_regclass('magneticod.torrent_file_blobs')::text`,
	).Scan(&torrentsReg, &blobsReg); err != nil {
		return false, "production schema unreadable: " + err.Error()
	}
	if torrentsReg == nil || blobsReg == nil {
		return false, "production tables missing"
	}
	var torrents, blobs int64
	if err := q.QueryRow(ctx,
		`SELECT (SELECT COUNT(*) FROM magneticod.torrents),
		        (SELECT COUNT(*) FROM magneticod.torrent_file_blobs)`,
	).Scan(&torrents, &blobs); err != nil {
		return false, "production counts unreadable: " + err.Error()
	}
	if torrents != facts.Torrents || blobs != facts.Blobs {
		return false, fmt.Sprintf("production counts moved since the pre-drop snapshot "+
			"(torrents %d→%d, blobs %d→%d)", facts.Torrents, torrents, facts.Blobs, blobs)
	}
	return true, ""
}

// renderCleanupPreview builds the operator-facing preview (secret-safe: no
// DSNs, no paths beyond the evidence basename is the runner's business).
func renderCleanupPreview(res *CleanupResult, cs *cleanupState, opts CleanupOptions) []string {
	lines := []string{
		"cleanup preview — nothing has been changed (pass --execute to drop)",
		"",
		fmt.Sprintf("run_id: %s", res.RunID),
		fmt.Sprintf("phase: %s   data_revision: %d", res.Phase, res.DataRevision),
		fmt.Sprintf("registry rows: errors=%d verification_runs=%d verification_issues=%d retry_runs=%d retry_attempts=%d",
			cs.counts.Errors, cs.counts.VerificationRuns, cs.counts.VerificationIssues,
			cs.counts.RetryRuns, cs.counts.RetryAttempts),
		fmt.Sprintf("target facts: torrents=%d blobs=%d blobless_multi_file=%d rows_beyond_checkpoint=%d "+
			"unresolved_record_errors=%d unresolved_integrity=%d",
			cs.facts.Torrents, cs.facts.Blobs, cs.facts.BloblessMultiFile,
			cs.facts.RowsBeyondCheckpoint, cs.facts.RecordErrors, cs.facts.UnresolvedIntegrity),
	}
	for _, kind := range []string{state.VerificationKindDecode, state.VerificationKindContent} {
		run := res.Latest[kind]
		if run == nil {
			lines = append(lines, fmt.Sprintf("verification %s: none", kind))
			continue
		}
		verdict := "STALE revision"
		if run.StartDataRevision == res.DataRevision {
			verdict = "current revision"
		}
		lines = append(lines, fmt.Sprintf(
			"verification %s: seq=%d state=%s phase=%s start_revision=%d (%s) issue_count=%d",
			kind, run.Seq, run.State, run.Phase, run.StartDataRevision, verdict, run.IssueCount))
	}
	if res.Reason != "" {
		lines = append(lines, "override reason: "+res.Reason)
	}
	lines = append(lines, "", "gates:")
	for _, g := range res.Gates {
		note := ""
		if g.Waivable && g.Status == GateFail {
			note = " (waivable with --force-unverified --reason)"
		}
		lines = append(lines, fmt.Sprintf("  %-6s %-22s %s%s", g.Status, g.Name, g.Detail, note))
	}
	lines = append(lines, "", "expected drops (one transaction; any failure rolls everything back):")
	for _, d := range res.ExpectedDrops {
		lines = append(lines, "  "+d)
	}
	lines = append(lines, "  (the identity sequences drop with their tables)")

	if len(res.Blockers) > 0 && !opts.Execute {
		lines = append(lines, "", "blockers — `--execute` would refuse:")
		for _, b := range res.Blockers {
			lines = append(lines, "  - "+b)
		}
	}
	return lines
}
