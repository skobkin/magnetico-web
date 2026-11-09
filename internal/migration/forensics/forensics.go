// Package forensics implements task 04C: `migrate errors export` (the
// forensic, fail-open JSONL export of migration operational state) and
// `migrate cleanup` (the gated, fail-closed teardown of the
// magnetico_migration schema).
//
// Export is diagnostic: it works on interrupted, failed, inconsistent and
// v1 (04A-era) state, NEVER calls the completed-run preflight or the
// schema upgrade path, and contacts the source only when record expansion
// is explicitly requested. Cleanup is destructive and fail-closed: every
// gate must pass under the migration advisory lock, immediately before
// and inside the single drop transaction.
package forensics

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

// Sentinel errors. main.go renders each through its fixed text only —
// never dynamic error content.
var (
	// ErrNoMigrationHistory: the operational schema does not exist at all
	// (a fresh target, or one whose state a completed cleanup removed).
	ErrNoMigrationHistory = errors.New("no migration history on the target")
	// ErrOutputExists: the output (or manifest) path already exists, or
	// appeared during the no-clobber publish.
	ErrOutputExists = errors.New("output path already exists")
	// ErrExportCapExceeded: a configured safety cap was hit — a hard error,
	// never a truncation.
	ErrExportCapExceeded = errors.New("export exceeded a configured safety cap")
	// ErrRecordsRequireSource: --include-records without a source DSN.
	ErrRecordsRequireSource = errors.New("record export requires a source database")
	// ErrSourceUnavailable: record expansion could not open/verify the
	// source, or a required record/file read failed.
	ErrSourceUnavailable = errors.New("source database unavailable or unreadable")
	// ErrEvidenceRejected: the cleanup evidence manifest failed validation.
	ErrEvidenceRejected = errors.New("export evidence rejected")
	// ErrCleanupRefused: at least one cleanup gate failed.
	ErrCleanupRefused = errors.New("cleanup refused by its safety gates")
	// ErrCleanupAmbiguous: the destructive transaction's COMMIT
	// acknowledgement was lost; catalog postconditions are consistent with
	// success but do not prove it — operator inspection required.
	ErrCleanupAmbiguous = errors.New("cleanup outcome is ambiguous; inspect manually before relying on it")
	// ErrLockLost: the advisory-lock connection died mid-run — exclusion is
	// void and the operation aborted. A SAFETY failure (fatal), never
	// reported as an operator interrupt.
	ErrLockLost = errors.New("migration run lock lost")
)

// OptionsError reports an invalid option combination (fixed text; the
// runner maps it to its configuration-error type).
type OptionsError struct{ Detail string }

func (e *OptionsError) Error() string { return "invalid options: " + e.Detail }

// Export safety caps (spec §4: configurable maximums; a breach is a hard
// error, never a successful partial export).
type Limits struct {
	MaxEvents      int64 // total emitted JSONL events (blob chunks included)
	MaxRecords     int64 // distinct expanded source-ID snapshots
	MaxOutputBytes int64 // total JSONL bytes
	MaxBlobBytes   int64 // per-blob raw-byte cap (probe before fetch)
}

// DefaultLimits are the conservative defaults proposed by the plan:
// roughly 3x the worst real registry (the 3.45M-torrent experiment),
// 29% of the real corpus for record expansion, ~3 GiB headroom over a
// full metadata export, and 2x the codec stored-blob cap for raw blobs.
func DefaultLimits() Limits {
	return Limits{
		MaxEvents:      10_000_000,
		MaxRecords:     1_000_000,
		MaxOutputBytes: 8 << 30,
		MaxBlobBytes:   64 << 20,
	}
}

func (l Limits) validate() error {
	switch {
	case l.MaxEvents <= 0:
		return &OptionsError{Detail: "max-events must be positive"}
	case l.MaxRecords <= 0:
		return &OptionsError{Detail: "max-records must be positive"}
	case l.MaxOutputBytes <= 0:
		return &OptionsError{Detail: "max-output-bytes must be positive"}
	case l.MaxBlobBytes <= 0:
		return &OptionsError{Detail: "max-blob-bytes must be positive"}
	}
	return nil
}

func nowOr(in func() time.Time) time.Time {
	if in != nil {
		return in().UTC()
	}
	return time.Now().UTC()
}

func randToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is a system-level problem; a fixed token only
		// risks a temp-name collision, which O_EXCL surfaces as an error.
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Deny-by-default semantic redaction (owner review finding 1).
//
// Stored JSONB (details, settings, fingerprints, identities, options) is
// NEVER echoed as-is: a hostile or buggy writer could have stored a secret
// under any key. Every metadata blob passes a per-kind whitelist of KNOWN
// keys, each with an explicit value constraint. Unknown keys are dropped
// entirely (even a key NAME can carry a secret; only a count survives).
// A known key whose value fails its constraint is replaced by a fixed
// {"redacted":"constraint"} marker. The policy applies identically in
// metadata-only and record-expansion modes — the sensitive opt-in governs
// source/target ROW content, not registry metadata.
// ---------------------------------------------------------------------------

type redactStats struct {
	UnknownKeys        int `json:"unknown_keys,omitempty"`
	ConstraintFailures int `json:"constraint_failures,omitempty"`
}

// value constraints
const (
	conInt      = "int"      // JSON number with an int64 value
	conBool     = "bool"     // JSON boolean
	conHex40    = "hex40"    // exactly 40 lowercase hex chars (infohash anchors)
	conRFC3339  = "rfc3339"  // parseable RFC 3339 timestamp
	conDBIdent  = "dbident"  // database identifier: 1-63 chars of [A-Za-z0-9_.:-]
	conHostPort = "hostport" // host or host:port, bounded, no credentials grammar
	conPort     = "port"     // 1-5 digits (the fingerprint stores server_port as string)
	conSQLState = "sqlstate" // 5 alphanumeric uppercase chars
)

// whitelists maps a metadata blob kind to its known keys and constraints.
// Keys are enumerated from what 04A/04B actually write (the census in the
// 04C plan); everything else is unknown and dropped.
var whitelists = map[string]map[string]string{
	// errors.details and verification_issues.details share one whitelist:
	// the writers overlap and every entry is a scalar diagnostic.
	"error_details": detailsWhitelist,
	"issue_details": detailsWhitelist,
	"verify_settings": {
		"workers":                  conInt,
		"batch_size":               conInt,
		"max_inflight_bytes":       conInt,
		"source_fingerprint_drift": conBool,
		"accept_source_repairs":    conBool,
	},
	"retry_settings": {
		"workers":                  conInt,
		"batch_size":               conInt,
		"max_inflight_bytes":       conInt,
		"source_fingerprint_drift": conBool,
		"accept_source_repairs":    conBool,
	},
	"run_options": {
		"workers":            conInt,
		"batch_size":         conInt,
		"max_inflight_bytes": conInt,
	},
	"fingerprint": {
		"server_address":        conHostPort,
		"server_port":           conPort,
		"postmaster_start_time": conRFC3339,
		"database":              conDBIdent,
		"torrents":              conInt,
		"min_id":                conInt,
		"max_id":                conInt,
		"anchor_min_info_hash":  conHex40,
		"anchor_max_info_hash":  conHex40,
		"file_rows":             conInt,
		"null_owner_files":      conInt,
		"orphan_files":          conInt,
		"orphan_id_min":         conInt,
		"orphan_id_max":         conInt,
	},
	"identity": {
		"postmaster_start_time": conRFC3339,
		"database":              conDBIdent,
		"server_address":        conHostPort,
		"server_port":           conInt,
		"known":                 conBool,
	},
}

// detailsWhitelist covers every key the 04A/04B detail writers produce:
// counts, IDs, limit shapes, stage/limit/side enums, and the two sanitized
// free-text keys (reason/error, written through the runner's sanitizer).
var detailsWhitelist = map[string]string{
	"reason":              "safetext",
	"error":               "safetext",
	"stage":               "safetext",
	"limit":               "enum:stored_bytes,uncompressed_bytes,files,path_bytes,aggregate_path_bytes,trie_depth",
	"side":                "enum:source,target,registry",
	"sqlstate":            conSQLState,
	"actual":              conInt,
	"allowed":             conInt,
	"count":               conInt,
	"expected":            conInt,
	"observed":            conInt,
	"at_start":            conInt,
	"at_end":              conInt,
	"length":              conInt,
	"file_count":          conInt,
	"decoded_files":       conInt,
	"fields":              conInt,
	"source_files":        conInt,
	"target_files":        conInt,
	"from_id":             conInt,
	"to_id":               conInt,
	"checkpoint":          conInt,
	"min_id":              conInt,
	"max_id":              conInt,
	"uncovered":           conInt,
	"assumed_single":      conBool,
	"orphan_files":        conInt,
	"orphan_source_files": conInt,
	"null_owner_files":    conInt,
	"orphan_id_min":       conInt,
	"orphan_id_max":       conInt,
	"registry_integrity":  conInt,
	"stored_max_id":       conInt,
	"fresh_max_id":        conInt,
	"stored_torrents":     conInt,
	"fresh_torrents":      conInt,
}

const (
	safetextMaxBytes = 256
	detailsMaxBytes  = 4096
)

var (
	safetextControl = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	credentialish   = regexp.MustCompile(`(?i)(://|password|passwd|pass=|pwd=|token|secret|api[_-]?key|authorization|bearer)`)
	sqlStateRe      = regexp.MustCompile(`^[0-9A-Z]{5}$`)
	hex40Re         = regexp.MustCompile(`^[0-9a-f]{40}$`)
	rfc3339Re       = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)
	dbIdentRe       = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,63}$`)
	hostPortRe      = regexp.MustCompile(`^[A-Za-z0-9_.:\[\]]{1,255}(:\d{1,5})?$`)
	portRe          = regexp.MustCompile(`^\d{1,5}$`)
)

// RedactJSON applies the per-kind deny-by-default whitelist to stored JSONB.
// nil/empty input renders as JSON null. A non-object top level is refused
// by shape. The whole redacted value larger than detailsMaxBytes collapses
// to an oversize marker. The returned stats are embedded in the output as
// a "redacted" key when anything was dropped or refused.
func RedactJSON(kind string, raw json.RawMessage) (json.RawMessage, redactStats) {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("null"), redactStats{}
	}
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return json.RawMessage(`{"redacted":"shape"}`), redactStats{}
	}
	obj, ok := top.(map[string]any)
	if !ok {
		return json.RawMessage(`{"redacted":"shape"}`), redactStats{}
	}
	rules := whitelists[kind]
	if rules == nil {
		return json.RawMessage(`{"redacted":"shape"}`), redactStats{}
	}

	stats := redactStats{}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		constraint, known := rules[k]
		if !known {
			stats.UnknownKeys++
			continue // dropped entirely; the key name is never echoed
		}
		if !valueFits(constraint, v) {
			stats.ConstraintFailures++
			out[k] = map[string]string{"redacted": "constraint"}
			continue
		}
		out[k] = v
	}
	if stats.UnknownKeys > 0 || stats.ConstraintFailures > 0 {
		out["redacted"] = stats
	}
	redacted, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{"redacted":"shape"}`), redactStats{}
	}
	return oversizeMarker(redacted, stats, detailsMaxBytes), stats
}

// oversizeMarker applies the whole-value cap (belt-and-braces on top of the
// per-value constraints). Parameterized so tests can exercise the marker at
// a small limit; with the current whitelists, constraint-valid values for
// all known keys stay well under the production limit — the guard exists
// for future whitelist growth and hostile shapes that marshal large.
func oversizeMarker(redacted []byte, stats redactStats, limit int) json.RawMessage {
	if len(redacted) <= limit {
		return json.RawMessage(redacted)
	}
	b, err := json.Marshal(map[string]any{"redacted": "oversize", "bytes": len(redacted)})
	if err != nil {
		return json.RawMessage(`{"redacted":"shape"}`)
	}
	return b
}

// valueFits checks one value against its whitelist constraint. Everything
// not covered by an explicit constraint fails closed.
func valueFits(constraint string, v any) bool {
	switch constraint {
	case conInt:
		if v == nil {
			return true // explicit JSON null is uninformative, never sensitive
		}
		n, ok := v.(float64) // encoding/json decodes JSON numbers as float64
		if !ok || n != trunc(n) || n < -9.2e18 || n > 9.2e18 {
			return false
		}
		return true
	case conBool:
		_, ok := v.(bool)
		return ok
	case "safetext":
		s, ok := v.(string)
		if !ok || len(s) == 0 || len(s) > safetextMaxBytes {
			return false
		}
		return !safetextControl.MatchString(s) && !credentialish.MatchString(s)
	case conHex40:
		s, ok := v.(string)
		return ok && hex40Re.MatchString(s)
	case conRFC3339:
		s, ok := v.(string)
		return ok && len(s) <= 64 && rfc3339Re.MatchString(s)
	case conDBIdent:
		s, ok := v.(string)
		return ok && dbIdentRe.MatchString(s)
	case conHostPort:
		s, ok := v.(string)
		return ok && len(s) <= 255 && hostPortRe.MatchString(s) && !credentialish.MatchString(s)
	case conPort:
		s, ok := v.(string)
		return ok && portRe.MatchString(s)
	case conSQLState:
		s, ok := v.(string)
		return ok && sqlStateRe.MatchString(s)
	default:
		if enum, found := cutEnumPrefix(constraint); found {
			s, ok := v.(string)
			if !ok {
				return false
			}
			for _, allowed := range splitEnum(enum) {
				if s == allowed {
					return true
				}
			}
			return false
		}
		return false // unknown constraint: fail closed
	}
}

func cutEnumPrefix(constraint string) (string, bool) {
	const prefix = "enum:"
	if len(constraint) > len(prefix) && constraint[:len(prefix)] == prefix {
		return constraint[len(prefix):], true
	}
	return "", false
}

func splitEnum(csv string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(csv); i++ {
		if i == len(csv) || csv[i] == ',' {
			if i > start {
				out = append(out, csv[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func trunc(f float64) float64 {
	if f < 0 {
		return -float64(int64(-f))
	}
	return float64(int64(f))
}
