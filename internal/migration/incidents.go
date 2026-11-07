package migration

// Incident recording: run-scope error rows for orphaned source file rows
// (integrity) and fatal failures (operational history). Diagnostics record
// the attempted source-ID range, stage, SQLSTATE and checkpoint — never
// credentials; error text is sanitized defensively before storage.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// ensureOrphanIncident records the run-scope integrity incident for
// orphaned/NULL-owned source file rows, exactly once per run (resume-safe).
func ensureOrphanIncident(ctx context.Context, pool *pgxpool.Pool, runID string, fp original.Fingerprint) (bool, error) {
	if fp.OrphanFiles == 0 && fp.NullOwnerFiles == 0 {
		return false, nil
	}
	details := map[string]any{
		"orphan_files":     fp.OrphanFiles,
		"null_owner_files": fp.NullOwnerFiles,
	}
	if fp.OrphanIDMin != nil {
		details["orphan_id_min"] = *fp.OrphanIDMin
		details["orphan_id_max"] = *fp.OrphanIDMax
	}
	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbOpTimeout)
	defer cancel()
	return state.EnsureIncident(ictx, pool, runID, state.ErrorRecord{
		Scope:    state.ScopeRun,
		Stage:    "preflight",
		Category: state.CategoryIntegrity,
		Code:     CodeSourceOrphanFiles,
		Details:  details,
	})
}

// fatalWithIncident records the operational incident (best-effort, never
// overrides the primary failure) and returns the typed fatal error.
func fatalWithIncident(ctx context.Context, pool *pgxpool.Pool, runID, stage string, fromID, toID int64, cause error) error {
	if runID != "" && pool != nil {
		if recErr := recordFatalIncident(ctx, pool, runID, stage, fromID, toID, cause); recErr != nil {
			cause = errors.Join(cause, fmt.Errorf("incident recording failed: %w", recErr))
		}
	}
	return &FatalError{Stage: stage, FromID: fromID, ToID: toID, Err: cause}
}

// recordFatalIncident writes one run-scope operational incident row.
// Operational incidents are history: they never feed run_error_count and
// never force import_complete_with_errors (owner directive, round 3).
func recordFatalIncident(ctx context.Context, pool *pgxpool.Pool, runID, stage string, fromID, toID int64, cause error) error {
	details := map[string]any{
		"stage":      stage,
		"from_id":    fromID,
		"to_id":      toID,
		"checkpoint": fromID,
		"error":      sanitizeDetail(cause.Error()),
	}
	var pgErr *pgconn.PgError
	if errors.As(cause, &pgErr) && pgErr.Code != "" {
		details["sqlstate"] = pgErr.Code
	}
	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbOpTimeout)
	defer cancel()
	return state.RecordIncident(ictx, pool, runID, state.ErrorRecord{
		Scope:    state.ScopeRun,
		Stage:    stage,
		Category: state.CategoryOperational,
		Code:     CodeFatalIncident,
		Details:  details,
	})
}

var (
	passwordKV = regexp.MustCompile(`(?i)(password|passfile|sslpassword)\s*[=:]\s*\S+`)
	userinfo   = regexp.MustCompile(`(?i)(postgres(?:ql)?|uri)://[^/\s:@]+:[^/\s@]+@`)
)

const maxDetailLen = 500

// sanitizeDetail strips credential-shaped fragments from an error string
// before it is stored or printed, and truncates the result.
func sanitizeDetail(s string) string {
	s = passwordKV.ReplaceAllString(s, "$1=[redacted]")
	s = userinfo.ReplaceAllString(s, "$1://[redacted]@")
	if len(s) > maxDetailLen {
		s = s[:maxDetailLen]
	}
	return strings.ToValidUTF8(s, "�")
}
