package webschema

import (
	"context"
	"fmt"

	"github.com/skobkin/magnetico-web/persistence"
)

// Querier is what VerifyIn needs from a transaction or connection.
type Querier = querier

// VerifyIn checks, inside the caller's transaction, that magnetico_web is
// exactly the supported version-1 structure (the same check Verify and Init
// apply). It is read-only. The caller must already have run
// `SET LOCAL search_path = pg_catalog` in that transaction so deparsed
// definitions are fully qualified.
//
//   - ErrWebSchemaMissing: the schema or web_version table is absent;
//   - ErrWebSchemaUnsupported (a *StateError): the structure differs.
func VerifyIn(ctx context.Context, q Querier) error {
	st, details, err := inspect(ctx, q)
	if err != nil {
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	switch st {
	case stateAbsent, stateNoVersion:
		return ErrWebSchemaMissing
	case stateBad:
		return &StateError{Err: ErrWebSchemaUnsupported, Details: details}
	}
	return nil
}
