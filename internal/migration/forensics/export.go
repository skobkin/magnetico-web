package forensics

// ExportErrors is the `migrate errors export` engine: a forensic, fail-open
// JSONL export of the migration operational state. The metadata pass streams
// every registry collection through keyset reads into a capped, checksummed
// sink with a no-clobber publish lifecycle. Record/blob expansion
// (--include-records) is added in a later slice; the flags exist and are
// validated here.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// ExportOptions is the validated option set (the opflags struct maps onto
// this in the CLI wiring slice).
type ExportOptions struct {
	// OutputPath is "-" (stdout, metadata-only) or a file path.
	OutputPath string
	// WriteManifest emits <output>.manifest.json after a successful publish.
	WriteManifest bool
	// OnlyUnresolved filters import_error events to unresolved rows
	// (everything else stays complete). Makes the export partial.
	OnlyUnresolved bool
	// VerificationID filters verification_run + verification_issue events to
	// that verification. Makes the export partial.
	VerificationID string
	// IncludeRecords / IncludeBlobs opt into source/target row expansion
	// (later slice; the source DSN is consumed only then).
	IncludeRecords    bool
	IncludeBlobs      bool
	SourceDatabaseURL string
	// Limits: the four safety caps; a breach is a hard error.
	Limits Limits
}

// ExportInput carries the validated options plus the target DSN and a
// clock seam for deterministic tests.
type ExportInput struct {
	Options   ExportOptions
	TargetDSN string
	// Now overrides the generated-at clock (tests); nil uses time.Now.
	Now func() time.Time
}

// ExportResult reports what was produced. On error the fields describe how
// far the run got; file mode never leaves a final path behind.
type ExportResult struct {
	SHA256       [32]byte
	Events       int64
	Bytes        int64
	EventCounts  map[string]int64
	Coverage     string
	OutputPath   string
	ManifestPath string // "" when not written
	SchemaClass  state.SchemaClass
	RunID        string
	DataRevision int64
}

// ExportErrors runs the export end to end: metadata pass always, record
// and blob expansion when --include-records is set.
func ExportErrors(ctx context.Context, in ExportInput) (*ExportResult, error) {
	opts := in.Options
	if opts.OutputPath == "" {
		return nil, &OptionsError{Detail: "output path is required"}
	}
	if opts.IncludeBlobs && !opts.IncludeRecords {
		return nil, &OptionsError{Detail: "--include-blobs requires --include-records"}
	}
	if opts.IncludeRecords {
		if opts.SourceDatabaseURL == "" {
			return nil, ErrRecordsRequireSource
		}
		if opts.OutputPath == StdoutPath {
			return nil, &OptionsError{Detail: "record/blob export requires a file output path (stdout is metadata-only)"}
		}
	}
	if opts.OutputPath == StdoutPath && opts.WriteManifest {
		return nil, &OptionsError{Detail: "--manifest cannot be combined with stdout output"}
	}
	if err := opts.Limits.validate(); err != nil {
		return nil, err
	}

	now := func() time.Time { return nowOr(in.Now) }

	f, err := Preflight(ctx, in.TargetDSN)
	if err != nil {
		return nil, err
	}
	defer f.Release()

	progress := f.Progress
	v1 := f.Class == state.SchemaV1

	// Records mode: the source opens BEFORE the header so the sanitized
	// source identity lands in the header and the manifest; the typed
	// refusals (unavailable source, same database, missing production
	// schema) fire before any byte is written.
	var src *original.Source
	var sourceIdent = json.RawMessage("null")
	if opts.IncludeRecords {
		var srcID pgidentity.Identity
		src, srcID, err = openRecordSource(ctx, f, opts.SourceDatabaseURL)
		if err != nil {
			return nil, err
		}
		defer src.Close()
		sourceIdent = identityJSON(srcID)
	}

	out, err := openOutput(opts.OutputPath, opts.Limits)
	if err != nil {
		return nil, err
	}
	sink := out.sinkWriter()

	res := &ExportResult{
		OutputPath:   opts.OutputPath,
		SchemaClass:  f.Class,
		RunID:        progress.RunID,
		DataRevision: progress.DataRevision,
	}
	fail := func(err error) (*ExportResult, error) {
		out.abort()
		f.Release()
		return nil, err
	}

	// Header.
	identJSON := identityJSON(f.Identity)
	filters := exportFilters{OnlyUnresolved: opts.OnlyUnresolved, VerificationID: opts.VerificationID}
	coverage := "complete"
	if filters.OnlyUnresolved || filters.VerificationID != "" {
		coverage = "partial"
	}
	header := exportHeaderEvent{
		envelope:               newEnvelope(TypeExportHeader, progress.RunID),
		GeneratedAt:            timeString(now()),
		OperationalSchema:      f.Class.String(),
		UnavailableCollections: []string{},
		TargetIdentity:         identJSON,
		SourceIdentity:         sourceIdent,
		Options: headerOptions{
			IncludeRecords: opts.IncludeRecords,
			IncludeBlobs:   opts.IncludeBlobs,
			OnlyUnresolved: opts.OnlyUnresolved,
			VerificationID: opts.VerificationID,
			Limits:         opts.Limits,
		},
		Order:    canonicalOrder,
		Coverage: coverage,
	}
	if v1 {
		header.UnavailableCollections = v2Collections
	}
	if err := sink.emit(TypeExportHeader, header); err != nil {
		return fail(err)
	}

	// import_run: counters exactly as recorded; never reconciled.
	if err := sink.emit(TypeImportRun, importRunEventFrom(progress, f.Class)); err != nil {
		return fail(err)
	}

	// Full error history (keyset walk; resolved and unresolved alike).
	after := int64(0)
	for {
		rows, err := state.ListErrorRows(ctx, f.Pool, progress.RunID, after, keysetPage)
		if err != nil {
			return fail(fmt.Errorf("reading error history: %w", err))
		}
		for _, r := range rows {
			if filters.OnlyUnresolved && r.Resolved {
				after = r.ID
				continue
			}
			if err := sink.emit(TypeImportError, importErrorEventFrom(r, progress.RunID)); err != nil {
				return fail(err)
			}
			after = r.ID
		}
		if len(rows) < keysetPage {
			break
		}
	}

	// 04B collections: v1 marks them unavailable and walks nothing.
	if !v1 {
		if err := walkVerification(ctx, f, sink, filters); err != nil {
			return fail(err)
		}
		if err := walkRetry(ctx, f.Pool, progress.RunID, sink); err != nil {
			return fail(err)
		}
	}

	// Record/blob expansion (records mode): per selected source_id, the
	// full source/target record blocks.
	if src != nil {
		if err := expandRecords(ctx, f, src, sink, opts); err != nil {
			return fail(err)
		}
	}

	// Registry-state digests bind the manifest to the exact registry bytes.
	digest, err := ComputeRegistryDigest(ctx, f.Pool, progress.RunID, f.Class)
	if err != nil {
		return fail(fmt.Errorf("computing registry digest: %w", err))
	}

	// Footer: actual emission counts; presence of a footer means the whole
	// path completed. Events counts the footer itself, so the number equals
	// the artifact's total line count.
	footer := exportFooterEvent{
		envelope:    newEnvelope(TypeExportFooter, progress.RunID),
		Status:      coverage,
		Events:      sink.events + 1,
		Coverage:    coverage,
		Counts:      sink.counts,
		CompletedAt: timeString(now()),
	}
	if err := sink.emit(TypeExportFooter, footer); err != nil {
		return fail(err)
	}

	if err := out.publish(); err != nil {
		return nil, err
	}
	res.SHA256 = sink.cw.sum()
	res.Events = sink.events
	res.Bytes = sink.cw.n
	res.EventCounts = sink.counts
	res.Coverage = coverage

	if opts.WriteManifest {
		manifestPath := ManifestPathFor(opts.OutputPath)
		m := &Manifest{
			Format:            manifestFormat,
			FormatVersion:     FormatVersion,
			Artifact:          baseName(opts.OutputPath),
			SHA256:            fmt.Sprintf("%x", res.SHA256),
			ByteCount:         res.Bytes,
			EventCounts:       res.EventCounts,
			RunID:             progress.RunID,
			DataRevision:      progress.DataRevision,
			OperationalSchema: f.Class.String(),
			Coverage:          coverage,
			Filters:           filters,
			IncludeRecords:    opts.IncludeRecords,
			IncludeBlobs:      opts.IncludeBlobs,
			Limits:            opts.Limits,
			TargetIdentity:    identJSON,
			SourceIdentity:    sourceIdent,
			RegistryState:     *digest,
			GeneratedAt:       timeString(now()),
		}
		if err := writeManifest(manifestPath, m); err != nil {
			// The JSONL is committed; evidence is incomplete — hard error.
			return res, fmt.Errorf("export published but the manifest failed: %w", err)
		}
		res.ManifestPath = manifestPath
	}
	return res, nil
}

// walkVerification emits verification_run and verification_issue events.
// --verification-id filters both; issue verification_seq is nil when the
// parent run was filtered out.
func walkVerification(ctx context.Context, f *Forensics, sink *jsonlSink, filters exportFilters) error {
	progress := f.Progress
	maxSeq, err := state.MaxVerificationSeq(ctx, f.Pool, progress.RunID)
	if err != nil {
		return fmt.Errorf("reading verification authorities: %w", err)
	}
	// verification_id -> seq for the denormalized issue field.
	seqByID := map[string]int64{}
	after := int64(0)
	for {
		runs, err := state.ListVerificationRuns(ctx, f.Pool, progress.RunID, after, keysetPage)
		if err != nil {
			return fmt.Errorf("reading verification history: %w", err)
		}
		for _, r := range runs {
			seqByID[r.VerificationID] = r.Seq
			authoritative := maxSeq[r.Kind] == r.Seq
			currentPass := authoritative && r.State == state.VerificationCompletedPass &&
				r.StartDataRevision == progress.DataRevision
			ev := verificationRunEvent{
				envelope:           newEnvelope(TypeVerificationRun, progress.RunID),
				Seq:                r.Seq,
				VerificationID:     r.VerificationID,
				Kind:               r.Kind,
				State:              r.State,
				Phase:              r.Phase,
				StartDataRevision:  r.StartDataRevision,
				SourceCheckpoint:   r.SourceCheckpoint,
				TargetCheckpoint:   r.TargetCheckpoint,
				ExpectedSourceIDs:  r.ExpectedSourceIDs,
				ObservedSourceIDs:  r.ObservedSourceIDs,
				ExpectedTargetIDs:  r.ExpectedTargetIDs,
				ObservedTargetIDs:  r.ObservedTargetIDs,
				VerifiedCount:      r.VerifiedCount,
				MismatchCount:      r.MismatchCount,
				UnverifiableCount:  r.UnverifiableCount,
				AssumedSingleCount: r.AssumedSingleCount,
				IssueCount:         r.IssueCount,
				Settings:           redactedMetadata("verify_settings", r.SettingsRaw),
				SourceIdentity:     redactedMetadata("identity", r.SourceIdentityRaw),
				SourceFingerprint:  redactedMetadata("fingerprint", r.SourceFingerprintRaw),
				StartedAt:          timeString(r.StartedAt),
				UpdatedAt:          timeString(r.UpdatedAt),
				CompletedAt:        nullTime(r.CompletedAt),
				Authoritative:      authoritative,
				CurrentPass:        currentPass,
			}
			if filters.VerificationID == "" || filters.VerificationID == r.VerificationID {
				if err := sink.emit(TypeVerificationRun, ev); err != nil {
					return err
				}
			}
			after = r.Seq
		}
		if len(runs) < keysetPage {
			break
		}
	}

	iAfter := int64(0)
	for {
		issues, err := state.ListVerificationIssuesAfter(ctx, f.Pool, iAfter, keysetPage)
		if err != nil {
			return fmt.Errorf("reading issue history: %w", err)
		}
		for _, r := range issues {
			if filters.VerificationID != "" && filters.VerificationID != r.VerificationID {
				iAfter = r.ID
				continue
			}
			details, _ := RedactJSON("issue_details", r.Details)
			ev := verificationIssueEvent{
				envelope:       newEnvelope(TypeVerificationIssue, ""),
				IssueID:        r.ID,
				VerificationID: r.VerificationID,
				SourceID:       r.SourceID,
				InfoHash:       hexBytes(r.InfoHash),
				Code:           r.Code,
				Stage:          r.Stage,
				Severity:       r.Severity,
				Details:        details,
				CreatedAt:      timeString(r.CreatedAt),
			}
			if seq, ok := seqByID[r.VerificationID]; ok {
				ev.VerificationSeq = &seq
			}
			if r.SourceID == nil {
				ev.RecordAvailability = "no_source_id"
			}
			if err := sink.emit(TypeVerificationIssue, ev); err != nil {
				return err
			}
			iAfter = r.ID
		}
		if len(issues) < keysetPage {
			return nil
		}
	}
}

// walkRetry emits retry_run ((started_at, retry_id) ASC) and retry_attempt
// (id ASC) events — the full history, every outcome.
func walkRetry(ctx context.Context, pool *pgxpool.Pool, runID string, sink *jsonlSink) error {
	var afterStartedAt time.Time
	afterRetryID := ""
	for {
		runs, err := state.ListRetryRuns(ctx, pool, runID, afterStartedAt, afterRetryID, keysetPage)
		if err != nil {
			return fmt.Errorf("reading retry history: %w", err)
		}
		for _, r := range runs {
			ev := retryRunEvent{
				envelope:             newEnvelope(TypeRetryRun, runID),
				RetryID:              r.RetryID,
				State:                r.State,
				StartDataRevision:    r.StartDataRevision,
				CurrentDataRevision:  r.CurrentDataRevision,
				UnresolvedAtStart:    r.UnresolvedAtStart,
				LastSourceID:         r.LastSourceID,
				AttemptedCount:       r.AttemptedCount,
				FixedCount:           r.FixedCount,
				StillInvalidCount:    r.StillInvalidCount,
				MissingCount:         r.MissingCount,
				SourceFingerprint:    redactedMetadata("fingerprint", r.SourceFingerprintRaw),
				AcceptedSourceRepair: r.AcceptedSourceRepair,
				PreviousFingerprint:  redactedMetadata("fingerprint", r.PreviousFingerprintRaw),
				Settings:             redactedMetadata("retry_settings", r.SettingsRaw),
				StartedAt:            timeString(r.StartedAt),
				UpdatedAt:            timeString(r.UpdatedAt),
				CompletedAt:          nullTime(r.CompletedAt),
			}
			if err := sink.emit(TypeRetryRun, ev); err != nil {
				return err
			}
			afterStartedAt = r.StartedAt
			afterRetryID = r.RetryID
		}
		if len(runs) < keysetPage {
			break
		}
	}

	aAfter := int64(0)
	for {
		attempts, err := state.ListRetryAttemptsAfter(ctx, pool, aAfter, keysetPage)
		if err != nil {
			return fmt.Errorf("reading attempt history: %w", err)
		}
		for _, r := range attempts {
			details, _ := RedactJSON("error_details", r.Details)
			ev := retryAttemptEvent{
				envelope:    newEnvelope(TypeRetryAttempt, runID),
				AttemptID:   r.ID,
				RetryID:     r.RetryID,
				SourceID:    r.SourceID,
				InfoHash:    hexBytes(r.SourceInfoHash),
				Outcome:     r.Outcome,
				Stage:       r.Stage,
				Code:        r.Code,
				Details:     details,
				AttemptedAt: timeString(r.AttemptedAt),
			}
			if err := sink.emit(TypeRetryAttempt, ev); err != nil {
				return err
			}
			aAfter = r.ID
		}
		if len(attempts) < keysetPage {
			return nil
		}
	}
}

const keysetPage = 5000

// identityJSON renders a sanitized identity object through the identity
// whitelist (finding 1: identities are metadata and pass the same policy).
func identityJSON(ident pgidentity.Identity) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"postmaster_start_time": ident.PostmasterStart.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		"database":              ident.Database,
		"server_address":        ident.ServerAddress,
		"server_port":           ident.ServerPort,
		"known":                 ident.Known,
	})
	if err != nil {
		return json.RawMessage(`{"redacted":"shape"}`)
	}
	out, _ := RedactJSON("identity", b)
	return out
}

func baseName(p string) string {
	// filepath.Base of an already-validated non-stdout path.
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
