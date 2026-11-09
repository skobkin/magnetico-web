package forensics

// The evidence manifest (spec §5): written ONLY after a successful publish,
// it binds the artifact's exact bytes (SHA-256 + byte count) to the run
// (run_id, data_revision), the schema class, the coverage/filters, the
// effective caps, the sanitized identities and the registry-state digests.
// Cleanup (commit 6) validates it and recomputes the digests inside the drop
// transaction — so a verification run transitioning after the export (say
// running -> completed_pass) refuses cleanup as stale evidence even though
// row counts and data_revision never changed.
//
// The manifest is a checksum and binding aid — not a signature, not a backup.

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/skobkin/magnetico-web/internal/migration/state"
)

const manifestFormat = "magnetico-migration-forensics-manifest"

// Manifest is the on-disk evidence record (JSON, one object).
type Manifest struct {
	Format            string           `json:"format"`
	FormatVersion     int              `json:"format_version"`
	Artifact          string           `json:"artifact"` // JSONL basename; resolved as a manifest sibling
	SHA256            string           `json:"sha256"`
	ByteCount         int64            `json:"byte_count"`
	EventCounts       map[string]int64 `json:"event_counts"`
	RunID             string           `json:"run_id"`
	DataRevision      int64            `json:"data_revision"`
	OperationalSchema string           `json:"operational_schema"`
	Coverage          string           `json:"coverage"` // complete | partial
	Filters           exportFilters    `json:"filters"`
	IncludeRecords    bool             `json:"include_records"`
	IncludeBlobs      bool             `json:"include_blobs"`
	Limits            Limits           `json:"limits"`
	TargetIdentity    json.RawMessage  `json:"target_identity"` // sanitized
	SourceIdentity    json.RawMessage  `json:"source_identity"` // sanitized (null without records)
	RegistryState     RegistryDigest   `json:"registry_state"`
	GeneratedAt       string           `json:"generated_at"`
}

// exportFilters records which partial filters were active; cleanup evidence
// must have both false (complete coverage).
type exportFilters struct {
	OnlyUnresolved bool   `json:"only_unresolved"`
	VerificationID string `json:"verification_id"`
}

// manifestSuffix is appended to the JSONL path.
const manifestSuffix = ".manifest.json"

// ManifestPathFor derives the manifest path of an output path ("-" is never
// a file and never has a manifest — the caller refuses earlier).
func ManifestPathFor(outputPath string) string {
	return outputPath + manifestSuffix
}

// writeManifest publishes a manifest through the same no-clobber lifecycle.
func writeManifest(path string, m *Manifest) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	dir := filepath.Dir(path)
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%w: %s", ErrOutputExists, filepath.Base(path))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tempPath := filepath.Join(dir, fmt.Sprintf(".magnetico-manifest-%d-%s.tmp", os.Getpid(), randToken()))
	f, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrOutputExists, filepath.Base(tempPath))
		}
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tempPath)
		return err
	}
	w := bufio.NewWriter(f)
	if _, err := w.Write(body); err != nil {
		f.Close()
		os.Remove(tempPath)
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tempPath)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tempPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tempPath)
		return err
	}
	if err := os.Link(tempPath, path); err != nil {
		os.Remove(tempPath)
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrOutputExists, filepath.Base(path))
		}
		return err
	}
	if err := os.Remove(tempPath); err != nil {
		return fmt.Errorf("published %s but could not remove the temp link: %w",
			filepath.Base(path), err)
	}
	return syncDir(dir)
}

// --- validation (used by cleanup; commit 6 wires it into gate G9) -----------

// manifestMaxBytes bounds a parse: evidence metadata is small.
const manifestMaxBytes = 1 << 20

// ValidateManifestFile checks a manifest + artifact pair structurally:
// regular files only (symlinks refused), strict format/version/coverage,
// exact re-hash and byte count of the sibling artifact. Content binding
// (run/revision/event counts/registry digests) is checked against the LIVE
// registry by the caller — here only artifact integrity is established.
func ValidateManifestFile(manifestPath string) (*Manifest, error) {
	fi, err := os.Lstat(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("%w: manifest unreadable: %v", ErrEvidenceRejected, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: manifest is not a regular file", ErrEvidenceRejected)
	}
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("%w: manifest unreadable: %v", ErrEvidenceRejected, err)
	}
	if len(body) > manifestMaxBytes {
		return nil, fmt.Errorf("%w: manifest exceeds %d bytes", ErrEvidenceRejected, manifestMaxBytes)
	}
	var m Manifest
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: manifest parse failed: %v", ErrEvidenceRejected, err)
	}
	if m.Format != manifestFormat || m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("%w: manifest format %q version %d is not supported",
			ErrEvidenceRejected, m.Format, m.FormatVersion)
	}
	if m.Coverage != "complete" {
		return nil, fmt.Errorf("%w: manifest coverage is %q — cleanup evidence requires an unfiltered complete export",
			ErrEvidenceRejected, m.Coverage)
	}
	if m.Filters.OnlyUnresolved || m.Filters.VerificationID != "" {
		return nil, fmt.Errorf("%w: manifest records active filters — cleanup evidence requires complete coverage",
			ErrEvidenceRejected)
	}
	if len(m.SHA256) != 64 {
		return nil, fmt.Errorf("%w: manifest sha256 is malformed", ErrEvidenceRejected)
	}
	if _, err := hex.DecodeString(m.SHA256); err != nil {
		return nil, fmt.Errorf("%w: manifest sha256 is malformed", ErrEvidenceRejected)
	}

	artifactPath := filepath.Join(filepath.Dir(manifestPath), m.Artifact)
	if filepath.Base(artifactPath) != m.Artifact {
		return nil, fmt.Errorf("%w: manifest artifact %q is not a plain name", ErrEvidenceRejected, m.Artifact)
	}
	afi, err := os.Lstat(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("%w: artifact %s unreadable: %v", ErrEvidenceRejected, m.Artifact, err)
	}
	if !afi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: artifact %s is not a regular file", ErrEvidenceRejected, m.Artifact)
	}
	if afi.Size() != m.ByteCount {
		return nil, fmt.Errorf("%w: artifact %s is %d bytes, manifest records %d",
			ErrEvidenceRejected, m.Artifact, afi.Size(), m.ByteCount)
	}
	sum, err := sha256File(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("%w: artifact re-hash failed: %v", ErrEvidenceRejected, err)
	}
	if hex.EncodeToString(sum[:]) != m.SHA256 {
		return nil, fmt.Errorf("%w: artifact %s does not match the manifest SHA-256",
			ErrEvidenceRejected, m.Artifact)
	}
	return &m, nil
}

// validateAgainstRegistry is the live-registry half of manifest validation:
// run identity, revision and per-collection counts must match the export
// context. (The registry-state DIGEST recompute runs inside the drop
// transaction in cleanup.go.)
func validateAgainstRegistry(m *Manifest, f *Forensics, counts state.RegistryCounts) error {
	if m.RunID != f.Progress.RunID {
		return fmt.Errorf("%w: manifest run_id %s is not this run (%s)",
			ErrEvidenceRejected, m.RunID, f.Progress.RunID)
	}
	if m.DataRevision != f.Progress.DataRevision {
		return fmt.Errorf("%w: manifest data_revision %d does not match the live progress row (%d) — state moved since the export",
			ErrEvidenceRejected, m.DataRevision, f.Progress.DataRevision)
	}
	if m.EventCounts[CollectionImportError] != counts.Errors {
		return fmt.Errorf("%w: manifest records %d import_error events, registry holds %d error rows",
			ErrEvidenceRejected, m.EventCounts[CollectionImportError], counts.Errors)
	}
	if m.EventCounts[CollectionVerificationRun] != counts.VerificationRuns {
		return fmt.Errorf("%w: manifest records %d verification_run events, registry holds %d rows",
			ErrEvidenceRejected, m.EventCounts[CollectionVerificationRun], counts.VerificationRuns)
	}
	if m.EventCounts[CollectionVerificationIssue] != counts.VerificationIssues {
		return fmt.Errorf("%w: manifest records %d verification_issue events, registry holds %d rows",
			ErrEvidenceRejected, m.EventCounts[CollectionVerificationIssue], counts.VerificationIssues)
	}
	if m.EventCounts[CollectionRetryRun] != counts.RetryRuns {
		return fmt.Errorf("%w: manifest records %d retry_run events, registry holds %d rows",
			ErrEvidenceRejected, m.EventCounts[CollectionRetryRun], counts.RetryRuns)
	}
	if m.EventCounts[CollectionRetryAttempt] != counts.RetryAttempts {
		return fmt.Errorf("%w: manifest records %d retry_attempt events, registry holds %d rows",
			ErrEvidenceRejected, m.EventCounts[CollectionRetryAttempt], counts.RetryAttempts)
	}
	return nil
}
