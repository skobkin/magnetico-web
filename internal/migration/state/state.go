// Package state owns the migration's operational state: the
// `magnetico_migration` schema (progress and errors), the run identity, the
// phase model, the run-scoped advisory lock and the transactional
// primitives (checkpoint compare-and-swap, error records) the target writer
// composes into its batches.
//
// This state is deliberately separate from the permanent
// `magneticod.schema_version`: it is temporary bookkeeping a later cleanup
// removes, and no production code path ever reads it.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// Phase is the migration lifecycle phase recorded in progress.
type Phase string

const (
	PhaseImporting          Phase = "importing"
	PhaseDataLoaded         Phase = "data_loaded"
	PhaseBuildingIndexes    Phase = "building_indexes"
	PhaseImportComplete     Phase = "import_complete"
	PhaseCompleteWithErrors Phase = "import_complete_with_errors"
)

// Valid reports whether the phase is one of the known values.
func (p Phase) Valid() bool {
	switch p {
	case PhaseImporting, PhaseDataLoaded, PhaseBuildingIndexes,
		PhaseImportComplete, PhaseCompleteWithErrors:
		return true
	}
	return false
}

// Complete reports whether the phase ends the migration.
func (p Phase) Complete() bool {
	return p == PhaseImportComplete || p == PhaseCompleteWithErrors
}

// Error scopes and categories.
const (
	ScopeRecord = "record"
	ScopeRun    = "run"

	// CategoryIntegrity marks unresolved data-quality findings (orphaned
	// source file rows): they force import_complete_with_errors.
	CategoryIntegrity = "integrity"
	// CategoryOperational marks recorded incidents (transient database
	// failures): history only, never affecting completion status.
	CategoryOperational = "operational"
)

var (
	// ErrNoRunState reports that no migration run state exists on the
	// target: the caller may start a fresh run if the target is empty.
	ErrNoRunState = errors.New("no migration run state on the target")

	// ErrRunStateCorrupt reports migration bookkeeping that cannot be
	// trusted (partial schema, multiple rows, inconsistent counters).
	// Never auto-repaired.
	ErrRunStateCorrupt = errors.New("migration run state is corrupt or inconsistent")
)

// StateError reports refused run-state conditions with details.
type StateError struct {
	Err     error
	Details []string
}

func (e *StateError) Error() string {
	out := e.Err.Error()
	for _, d := range e.Details {
		out += "; " + d
	}
	return out
}

func (e *StateError) Unwrap() error { return e.Err }

// migrationLockAdvisoryKey is the fixed project-specific advisory-lock key
// excluding concurrent migrator instances against one target. Session-level
// (pg_advisory_lock), held for the run lifetime on a dedicated connection.
// Distinct from the schema-init key in persistence.
const migrationLockAdvisoryKey int64 = 4048864026886105453 // 'migrat08' little-endian

// NewRunID returns a fresh RFC 4122 v4 UUID string.
func NewRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst), nil
}

// Options is the tunable import configuration recorded with the run
// (informational on resume: workers/batch may change between invocations;
// defer_indexes is a separate immutable column).
type Options struct {
	Workers          int   `json:"workers"`
	BatchSize        int   `json:"batch_size"`
	MaxInflightBytes int64 `json:"max_inflight_bytes"`
}

// ErrorRecord is one entry of the error registry.
type ErrorRecord struct {
	Scope          string         // ScopeRecord or ScopeRun
	SourceID       *int64         // record scope only
	SourceInfoHash []byte         // record scope only
	Stage          string         // preflight|read|validate|encode|decode_verify|write
	Category       string         // CategoryIntegrity or CategoryOperational
	Code           string         // stable error code
	Details        map[string]any // safe values only, no paths, no credentials
}

// AdvanceInput is one compare-and-swap checkpoint update: the batch commit
// advances the checkpoint only if the target is still at the expected
// (last ID, phase), rolling the whole batch back otherwise.
type AdvanceInput struct {
	RunID string
	// ExpectedLastID and ExpectedPhase must match the stored row.
	ExpectedLastID int64
	ExpectedPhase  Phase
	// NewLastID and NewPhase are the post-batch values.
	NewLastID int64
	NewPhase  Phase
	// Deltas added to the stored counters.
	ScannedDelta  int64
	SuccessDelta  int64
	ErrorDelta    int64
	RunErrorDelta int64
	BlobDelta     int64
}

func validatePhaseTransition(from, to Phase) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("%w: unknown phase", ErrRunStateCorrupt)
	}
	switch {
	case from == PhaseCompleteWithErrors && to == PhaseImportComplete:
		// The ONE post-completion edge (04B retry): every unresolved record
		// error was repaired and no run-scope integrity incident remains.
		// Only CompleteRetryPhase reaches it — the import code paths cannot.
		return nil
	case from.Complete():
		return fmt.Errorf("%w: completed run cannot advance", ErrRunStateCorrupt)
	case from == PhaseImporting && to == PhaseImporting:
		return nil
	case from == PhaseImporting && to == PhaseDataLoaded:
		return nil
	case from == PhaseDataLoaded && to == PhaseBuildingIndexes:
		return nil
	case from == PhaseBuildingIndexes && to == PhaseBuildingIndexes:
		return nil
	case from == PhaseBuildingIndexes && (to == PhaseImportComplete || to == PhaseCompleteWithErrors):
		return nil
	default:
		return fmt.Errorf("%w: illegal phase transition %s -> %s", ErrRunStateCorrupt, from, to)
	}
}

// NewLastID must strictly increase within a phase (monotonic checkpoint);
// the data_loaded/building_indexes transitions keep the final checkpoint.
func validateAdvance(in AdvanceInput) error {
	if err := validatePhaseTransition(in.ExpectedPhase, in.NewPhase); err != nil {
		return err
	}
	if in.NewPhase == in.ExpectedPhase && in.NewLastID < in.ExpectedLastID {
		return fmt.Errorf("%w: checkpoint would move backwards (%d -> %d)",
			ErrRunStateCorrupt, in.ExpectedLastID, in.NewLastID)
	}
	if in.NewLastID < in.ExpectedLastID {
		return fmt.Errorf("%w: checkpoint would move backwards (%d -> %d)",
			ErrRunStateCorrupt, in.ExpectedLastID, in.NewLastID)
	}
	return nil
}
