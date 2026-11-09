package migration

// cleanup (04C): gated, fail-closed atomic teardown of the
// magnetico_migration operational schema. The migration package wrapper
// exists so main.go talks to one boundary: it maps context cancellation to
// the shared ErrInterrupted contract and otherwise forwards the typed
// forensics results/refusals verbatim. Interruption before the drop
// transaction commits always leaves the schema intact (the engine's deferred
// rollback runs on a cancellation-immune context).

import (
	"context"
	"errors"

	"github.com/skobkin/magnetico-web/internal/migration/forensics"
)

// Cleanup runs the gates, the preview and (with --execute) the atomic drop.
// Typed outcomes (cleanup side): forensics.ErrCleanupRefused (a gate or the
// in-transaction recheck refused; nothing dropped),
// forensics.ErrCleanupAmbiguous (a lost COMMIT acknowledgment; the outcome
// is classified, never assumed), *forensics.OptionsError, *state.StateError
// — all rendered by main.go's fixed-text detail table (wiring slice). A
// cancelled context surfaces as ErrInterrupted (exit 2, nothing dropped).
func Cleanup(ctx context.Context, in forensics.CleanupInput) (*forensics.CleanupResult, error) {
	res, err := forensics.Cleanup(ctx, in)
	if err != nil {
		// A cancelled context surfaces as the shared interruption contract
		// (exit 2): the engine's deferred rollback has already discarded the
		// transaction, so nothing is half-dropped. A cancellation DURING the
		// COMMIT round-trip is classified by the engine (refusal or
		// ambiguous) and does NOT wrap context.Canceled — it passes through.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrInterrupted
		}
		return nil, err
	}
	return res, nil
}
