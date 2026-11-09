package migration

// errors export (04C, metadata slice): forensic, fail-open JSONL export of
// the migration operational state. The migration package wrapper exists so
// main.go talks to one boundary: it maps context cancellation to the shared
// ErrInterrupted contract and otherwise forwards the typed forensics
// results/refusals verbatim. Record/blob expansion (--include-records) is a
// later slice; the forensics layer validates the flags meanwhile.

import (
	"context"
	"errors"

	"github.com/skobkin/magnetico-web/internal/migration/forensics"
)

// ExportErrors runs the metadata export. Typed outcomes (export side):
// forensics.ErrNoMigrationHistory, forensics.ErrOutputExists,
// forensics.ErrExportCapExceeded, *forensics.OptionsError, *state.StateError
// — all rendered by main.go's fixed-text detail table (wiring slice).
func ExportErrors(ctx context.Context, in forensics.ExportInput) (*forensics.ExportResult, error) {
	res, err := forensics.ExportErrors(ctx, in)
	if err != nil {
		// A cancelled context surfaces as the shared interruption contract
		// (exit 2, no footer, temp removed by the forensics layer).
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrInterrupted
		}
		return nil, err
	}
	return res, nil
}
