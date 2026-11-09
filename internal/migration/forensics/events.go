package forensics

// JSONL event contract (spec §3): every line carries the envelope plus the
// event fields; Go struct field order is the deterministic output order;
// infohashes render as lowercase hex; timestamps are RFC 3339 UTC; nullables
// are null, never omitted.

import (
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/skobkin/magnetico-web/internal/migration/state"
)

const (
	// Format identifies the artifact family in every envelope.
	Format = "magnetico-migration-forensics"
	// FormatVersion is bumped on any breaking contract change.
	FormatVersion = 1
)

// Event types, in canonical global order (export_footer last). The record
// events appear per selected source_id ascending, in the per-record order:
// source_record, source_file*, target_record, decode_finding, target_file*,
// target_blob, target_blob_chunk*.
const (
	TypeExportHeader      = "export_header"
	TypeImportRun         = "import_run"
	TypeImportError       = "import_error"
	TypeVerificationRun   = "verification_run"
	TypeVerificationIssue = "verification_issue"
	TypeRetryRun          = "retry_run"
	TypeRetryAttempt      = "retry_attempt"
	TypeSourceRecord      = "source_record"
	TypeSourceFile        = "source_file"
	TypeTargetRecord      = "target_record"
	TypeDecodeFinding     = "decode_finding"
	TypeTargetFile        = "target_file"
	TypeTargetBlob        = "target_blob"
	TypeTargetBlobChunk   = "target_blob_chunk"
	TypeExportFooter      = "export_footer"
)

// Collection names for counts, unavailability lists and manifests.
const (
	CollectionImportRun         = "import_run"
	CollectionImportError       = "import_error"
	CollectionVerificationRun   = "verification_run"
	CollectionVerificationIssue = "verification_issue"
	CollectionRetryRun          = "retry_run"
	CollectionRetryAttempt      = "retry_attempt"
	CollectionSourceRecord      = "source_record"
	CollectionSourceFile        = "source_file"
	CollectionTargetRecord      = "target_record"
	CollectionTargetFile        = "target_file"
	CollectionTargetBlob        = "target_blob"
	CollectionTargetBlobChunk   = "target_blob_chunk"
)

// v2Collections is the complete 04B registry; a v1 schema provides only
// import_run + import_error, and the header must name the rest explicitly —
// never silently empty.
var v2Collections = []string{
	CollectionVerificationRun, CollectionVerificationIssue,
	CollectionRetryRun, CollectionRetryAttempt,
}

// hexBytes renders []byte as lowercase hex or null, never base64.
type hexBytes []byte

func (b hexBytes) MarshalJSON() ([]byte, error) {
	if b == nil {
		return []byte("null"), nil
	}
	out := make([]byte, len(b)*2+2)
	out[0] = '"'
	out[len(out)-1] = '"'
	hex.Encode(out[1:len(out)-1], b)
	return out, nil
}

func nullTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func timeString(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// envelope is embedded FIRST in every event struct so the emitted field
// order is deterministic: format, format_version, type, run_id, then fields.
type envelope struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	Type          string `json:"type"`
	RunID         string `json:"run_id"`
}

// exportHeaderEvent opens the stream: schema class, sanitized identities,
// effective options and the global ordering contract.
type exportHeaderEvent struct {
	envelope
	GeneratedAt string `json:"generated_at"`
	// OperationalSchema is "v1" or "v2" (a fresh/absent schema never exports).
	OperationalSchema string `json:"operational_schema"`
	// UnavailableCollections names collections this schema class cannot
	// provide (the four 04B tables under v1) — never silently empty.
	UnavailableCollections []string      `json:"unavailable_collections"`
	TargetIdentity         any           `json:"target_identity"`
	SourceIdentity         any           `json:"source_identity"` // null without --include-records
	Options                headerOptions `json:"options"`
	// Order documents the canonical emission order (self-describing artifact).
	Order []string `json:"order"`
	// Coverage: "complete" (whole registry) or "partial" (filters applied);
	// cleanup evidence requires complete.
	Coverage string `json:"coverage"`
}

type headerOptions struct {
	IncludeRecords bool   `json:"include_records"`
	IncludeBlobs   bool   `json:"include_blobs"`
	OnlyUnresolved bool   `json:"only_unresolved"`
	VerificationID string `json:"verification_id"` // "" when unset
	Limits         Limits `json:"limits"`
}

// importRunEvent reports the progress row exactly as recorded. Export NEVER
// evaluates reconciliation — reconcile_status is fixed "not_reconciled",
// documented as "not evaluated; counters as recorded".
type importRunEvent struct {
	envelope
	Phase             string   `json:"phase"`
	ContractVersion   int      `json:"contract_version"`
	CodecProfile      string   `json:"codec_profile"`
	SourceAdapter     string   `json:"source_adapter"`
	ExpectedTotal     int64    `json:"expected_total"`
	SourceMinID       int64    `json:"source_min_id"`
	SourceMaxID       int64    `json:"source_max_id"`
	SourceFileRows    int64    `json:"source_file_rows"`
	LastProcessedID   int64    `json:"last_processed_id"`
	ScannedCount      int64    `json:"scanned_count"`
	SuccessCount      int64    `json:"success_count"`
	ErrorCount        int64    `json:"error_count"`
	RunErrorCount     int64    `json:"run_error_count"`
	BlobCount         int64    `json:"blob_count"`
	DataRevision      int64    `json:"data_revision"`
	DeferIndexes      bool     `json:"defer_indexes"`
	IndexesDeferred   bool     `json:"indexes_deferred"`
	Options           any      `json:"options"`
	SourceFingerprint any      `json:"source_fingerprint"`
	StartedAt         string   `json:"started_at"`
	UpdatedAt         string   `json:"updated_at"`
	ReconcileStatus   string   `json:"reconcile_status"`
	Notes             []string `json:"notes,omitempty"`
}

// importErrorEvent is one FULL errors row (resolved and unresolved alike).
type importErrorEvent struct {
	envelope
	ErrorID            int64           `json:"error_id"`
	Scope              string          `json:"scope"`
	SourceID           *int64          `json:"source_id"`
	InfoHash           hexBytes        `json:"info_hash"`
	Stage              string          `json:"stage"`
	Category           string          `json:"category"`
	Code               string          `json:"code"`
	Details            json.RawMessage `json:"details"` // redacted
	Attempts           int             `json:"attempts"`
	Resolved           bool            `json:"resolved"`
	ResolvedAt         *string         `json:"resolved_at"`
	CreatedAt          string          `json:"created_at"`
	RecordAvailability string          `json:"record_unavailable_reason,omitempty"` // "no_source_id"
}

// verificationRunEvent is one FULL verification_runs row. Authoritative marks
// the per-kind latest seq; currentPass marks authoritative + completed_pass +
// start_data_revision == progress.data_revision (a historic pass is visibly
// NOT current).
type verificationRunEvent struct {
	envelope
	Seq                int64           `json:"seq"`
	VerificationID     string          `json:"verification_id"`
	Kind               string          `json:"kind"`
	State              string          `json:"state"`
	Phase              string          `json:"phase"`
	StartDataRevision  int64           `json:"start_data_revision"`
	SourceCheckpoint   *int64          `json:"source_checkpoint"`
	TargetCheckpoint   *int64          `json:"target_checkpoint"`
	ExpectedSourceIDs  *int64          `json:"expected_source_ids"`
	ObservedSourceIDs  *int64          `json:"observed_source_ids"`
	ExpectedTargetIDs  *int64          `json:"expected_target_ids"`
	ObservedTargetIDs  *int64          `json:"observed_target_ids"`
	VerifiedCount      int64           `json:"verified_count"`
	MismatchCount      int64           `json:"mismatch_count"`
	UnverifiableCount  int64           `json:"unverifiable_count"`
	AssumedSingleCount int64           `json:"assumed_single_count"`
	IssueCount         int64           `json:"issue_count"`
	Settings           json.RawMessage `json:"settings"`           // redacted
	SourceIdentity     json.RawMessage `json:"source_identity"`    // redacted (content kind)
	SourceFingerprint  json.RawMessage `json:"source_fingerprint"` // redacted (content kind)
	StartedAt          string          `json:"started_at"`
	UpdatedAt          string          `json:"updated_at"`
	CompletedAt        *string         `json:"completed_at"`
	Authoritative      bool            `json:"authoritative"`
	CurrentPass        bool            `json:"current_pass"`
}

// verificationIssueEvent is one FULL verification_issues row (table-wide id).
type verificationIssueEvent struct {
	envelope
	IssueID        int64  `json:"issue_id"`
	VerificationID string `json:"verification_id"`
	// VerificationSeq is denormalized as a grouping aid (resolved during the
	// verification walk); absent from output when the parent run was filtered out.
	VerificationSeq    *int64          `json:"verification_seq"`
	SourceID           *int64          `json:"source_id"`
	InfoHash           hexBytes        `json:"info_hash"`
	Code               string          `json:"code"`
	Stage              string          `json:"stage"`
	Severity           string          `json:"severity"`
	Details            json.RawMessage `json:"details"` // redacted
	CreatedAt          string          `json:"created_at"`
	RecordAvailability string          `json:"record_unavailable_reason,omitempty"` // "no_source_id"
}

// retryRunEvent is one FULL retry_runs row.
type retryRunEvent struct {
	envelope
	RetryID              string          `json:"retry_id"`
	State                string          `json:"state"`
	StartDataRevision    int64           `json:"start_data_revision"`
	CurrentDataRevision  int64           `json:"current_data_revision"`
	UnresolvedAtStart    int64           `json:"unresolved_at_start"`
	LastSourceID         *int64          `json:"last_source_id"`
	AttemptedCount       int64           `json:"attempted_count"`
	FixedCount           int64           `json:"fixed_count"`
	StillInvalidCount    int64           `json:"still_invalid_count"`
	MissingCount         int64           `json:"missing_count"`
	SourceFingerprint    json.RawMessage `json:"source_fingerprint"` // redacted
	AcceptedSourceRepair bool            `json:"accepted_source_repair"`
	PreviousFingerprint  json.RawMessage `json:"previous_fingerprint"` // redacted
	Settings             json.RawMessage `json:"settings"`             // redacted
	StartedAt            string          `json:"started_at"`
	UpdatedAt            string          `json:"updated_at"`
	CompletedAt          *string         `json:"completed_at"`
}

// retryAttemptEvent is one FULL retry_attempts row.
type retryAttemptEvent struct {
	envelope
	AttemptID   int64           `json:"attempt_id"`
	RetryID     string          `json:"retry_id"`
	SourceID    int64           `json:"source_id"`
	InfoHash    hexBytes        `json:"info_hash"`
	Outcome     string          `json:"outcome"`
	Stage       string          `json:"stage"`
	Code        string          `json:"code"`
	Details     json.RawMessage `json:"details"` // redacted
	AttemptedAt string          `json:"attempted_at"`
}

// exportFooterEvent closes the stream (last line, inside the checksum):
// actual emission counts, final coverage and status. Written ONLY on the
// complete path — a failed export has no footer, so presence is meaningful.
type exportFooterEvent struct {
	envelope
	Status      string           `json:"status"` // "complete" | "partial"
	Events      int64            `json:"events"`
	Coverage    string           `json:"coverage"`
	Counts      map[string]int64 `json:"counts"`
	CompletedAt string           `json:"completed_at"`
}

// --- record/blob events (--include-records) ----------------------------------

// sourceRecordEvent is the source-side metadata for one selected ID.
// Status "absent" is proven absence (the set-oriented query succeeded).
type sourceRecordEvent struct {
	envelope
	SourceID         int64    `json:"source_id"`
	Status           string   `json:"status"` // present | absent
	InfoHash         hexBytes `json:"info_hash"`
	Name             string   `json:"name"`
	TotalSize        int64    `json:"total_size"`
	DiscoveredOn     int64    `json:"discovered_on"` // source unix seconds (0 when absent)
	DiscoveredOnTime string   `json:"discovered_on_time"`
}

// sourceFileEvent is ONE source file row — every row of the selected
// torrent, no codec caps, duplicates and source file IDs preserved.
type sourceFileEvent struct {
	envelope
	SourceID int64  `json:"source_id"`
	FileID   int32  `json:"file_id"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
}

// targetRecordEvent is the target-side snapshot row. Blob keeps the three
// states distinct: "none" (no row), "null_data" (row with NULL data —
// impossible under the v2 CHECK but reported if it ever appears),
// "stored_bytes" (row with data; StoredBytes exact).
type targetRecordEvent struct {
	envelope
	SourceID         int64    `json:"source_id"`
	Status           string   `json:"status"` // present | absent
	InfoHash         hexBytes `json:"info_hash"`
	Name             string   `json:"name"`
	TotalSize        int64    `json:"total_size"`
	DiscoveredOn     int64    `json:"discovered_on"`
	DiscoveredOnTime string   `json:"discovered_on_time"`
	FileCount        int64    `json:"file_count"`
	Blob             string   `json:"blob"`
	StoredBytes      int64    `json:"stored_bytes"`
}

// decodeFindingEvent is the typed decode verdict, decided independently of
// raw export.
type decodeFindingEvent struct {
	envelope
	SourceID int64  `json:"source_id"`
	Status   string `json:"status"` // decoded | failed | not_attempted
	Code     string `json:"code"`
	Details  any    `json:"details"`
}

// targetFileEvent is one decoded (or assumed) target file entry. Assumed is
// true ONLY for the reconstructed no-blob single entry.
type targetFileEvent struct {
	envelope
	SourceID int64  `json:"source_id"`
	Index    int    `json:"index"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Assumed  bool   `json:"assumed"`
}

// targetBlobEvent opens one raw blob block and binds its SHA-256 so the
// Base64 round-trip is checkable.
type targetBlobEvent struct {
	envelope
	SourceID    int64  `json:"source_id"`
	StoredBytes int64  `json:"stored_bytes"`
	SHA256      string `json:"sha256"`
}

// targetBlobChunkEvent carries one ordered Base64 chunk. Lossless rule:
// concat(base64-decode of chunks by ascending chunk_index) == stored bytes.
type targetBlobChunkEvent struct {
	envelope
	SourceID   int64  `json:"source_id"`
	ChunkIndex int    `json:"chunk_index"`
	DataBase64 string `json:"data_base64"`
	ChunkBytes int    `json:"chunk_bytes"`
}

// --- event constructors -----------------------------------------------------

func newEnvelope(evType, runID string) envelope {
	return envelope{Format: Format, FormatVersion: FormatVersion, Type: evType, RunID: runID}
}

// canonicalOrder is the documented global emission order (header field).
var canonicalOrder = []string{
	TypeExportHeader, TypeImportRun, TypeImportError, TypeVerificationRun,
	TypeVerificationIssue, TypeRetryRun, TypeRetryAttempt,
	TypeSourceRecord, TypeSourceFile, TypeTargetRecord, TypeDecodeFinding,
	TypeTargetFile, TypeTargetBlob, TypeTargetBlobChunk,
	TypeExportFooter,
}

func importRunEventFrom(p *state.Progress, class state.SchemaClass) importRunEvent {
	ev := importRunEvent{
		envelope:          newEnvelope(TypeImportRun, p.RunID),
		Phase:             string(p.Phase),
		ContractVersion:   p.ContractVersion,
		CodecProfile:      p.CodecProfile,
		SourceAdapter:     p.SourceAdapter,
		ExpectedTotal:     p.ExpectedTotal,
		SourceMinID:       p.SourceMinID,
		SourceMaxID:       p.SourceMaxID,
		SourceFileRows:    p.SourceFileRows,
		LastProcessedID:   p.LastProcessedID,
		ScannedCount:      p.ScannedCount,
		SuccessCount:      p.SuccessCount,
		ErrorCount:        p.ErrorCount,
		RunErrorCount:     p.RunErrorCount,
		BlobCount:         p.BlobCount,
		DataRevision:      p.DataRevision,
		DeferIndexes:      p.DeferIndexes,
		IndexesDeferred:   p.IndexesDeferred,
		Options:           redactedMetadata("run_options", p.OptionsRaw),
		SourceFingerprint: redactedMetadata("fingerprint", p.FingerprintRaw),
		StartedAt:         timeString(p.StartedAt),
		UpdatedAt:         timeString(p.UpdatedAt),
		ReconcileStatus:   "not_reconciled",
	}
	var notes []string
	if class == state.SchemaV1 {
		notes = append(notes, "v1 schema: data_revision reported as 0 (column predates the revision counter)")
	}
	if len(notes) > 0 {
		ev.Notes = notes
	}
	return ev
}

func importErrorEventFrom(r state.ErrorRow, runID string) importErrorEvent {
	details, _ := RedactJSON("error_details", r.Details)
	ev := importErrorEvent{
		envelope:   newEnvelope(TypeImportError, runID),
		ErrorID:    r.ID,
		Scope:      r.Scope,
		SourceID:   r.SourceID,
		InfoHash:   hexBytes(r.SourceInfoHash),
		Stage:      r.Stage,
		Category:   r.Category,
		Code:       r.Code,
		Details:    details,
		Attempts:   r.Attempts,
		Resolved:   r.Resolved,
		ResolvedAt: nullTime(r.ResolvedAt),
		CreatedAt:  timeString(r.CreatedAt),
	}
	if r.SourceID == nil {
		ev.RecordAvailability = "no_source_id"
	}
	return ev
}

// redactedMetadata marshals a raw JSONB blob through the per-kind whitelist.
// Never fails: refusals render as {"redacted":"shape"} markers.
func redactedMetadata(kind string, raw json.RawMessage) json.RawMessage {
	out, _ := RedactJSON(kind, raw)
	return out
}
