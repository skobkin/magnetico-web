# Task 07C / W2 — web schema, user store and operator CLI: report

Status: **in progress** (S1 approved with one correction; steps 5–6 pending, S2 next). This report is
extended at each stage and completed in step 9.

## S1 review correction (commit following `6e16f5e2`)

Review of `6e16f5e2` accepted the schema work with one security correction: structural mismatch
diagnostics in `internal/web/webschema/verify.go` included deparsed SQL (column defaults, CHECK and
index definitions), which may contain arbitrary string literals and reach the operator and startup
logs through `appendSchemaStateDetails`.

- Diagnostics now name the object and the mismatch category only: `column magnetico_web.users.role differs`,
  `constraint magnetico_web.users.users_role_check differs`,
  `index magnetico_web.users.users_verified_email_key differs`. The `colSpec.describe` helper was removed.
- The structural comparisons themselves are unchanged. Remaining diagnostics contain catalog identifiers
  (table, column, constraint, index, trigger names) and integer version numbers, never expressions or values.
- Regression tests:
  - `webschema.TestDiagnosticsDoNotLeakCatalogContents`: a distinctive canary in a column default, a CHECK
    body and an index predicate; `Init` and `Verify` both return `ErrWebSchemaUnsupported` naming the object
    and the text never appears in the error or its `Details`.
  - `TestSchemaDiagnosticsDoNotLeakCatalogContentsBinary` (root package): the same canary in a column default;
    `magnetico migrate web-schema-init` and `magnetico web` startup both exit 1 with the sanitized message and
    without the canary in their output.
- Run: `go build ./...`, `go vet ./internal/web/... .`, `go test -race ./internal/web/webschema ./internal/web/webdb`,
  `go test -race -run 'TestWebBinary|TestMigrateWebSchemaInit|TestSchemaDiagnostics' .` — all passed.

Dependency deferral (`x/crypto`, `x/term` until first use) was approved by the owner.
