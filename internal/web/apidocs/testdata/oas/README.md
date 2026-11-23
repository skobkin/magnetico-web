# Official OpenAPI 3.2 validation assets (test data only)

Downloaded once on 2026-11-23 from `spec.openapis.org` over HTTPS, stored
unmodified, and used only by the tests of this package to validate the embedded
OpenAPI documents. Tests load them from here; **nothing is ever fetched at test
time or at run time** (the test loader refuses every other URL).

| File | Source URL (`$id`) | SHA-256 |
|---|---|---|
| `3.2_schema-base_2025-09-17.json` | `https://spec.openapis.org/oas/3.2/schema-base/2025-09-17` | `43bb51dbd831b2215cf27e734969ff50fe7043a1e60879afe9631c5a6c262214` |
| `3.2_schema_2025-09-17.json` | `https://spec.openapis.org/oas/3.2/schema/2025-09-17` | `0c9d74bf25f9b9388b2d81e421ef60fdefa9feffa94898dadfc501b342b3bfcc` |
| `3.2_dialect_2025-09-17.json` | `https://spec.openapis.org/oas/3.2/dialect/2025-09-17` | `4e2c989f3d1e6489d41bc1ca4ade11743278c612b82fba9144c6116c79f1c273` |
| `3.2_meta_2025-09-17.json` | `https://spec.openapis.org/oas/3.2/meta/2025-09-17` | `a1959c0aa1f9a7ce58f2b75699be5bc5187c8a25901fe04a227f1d9419ba4e9c` |

`schema-base` validates a whole OpenAPI document including its Schema Objects
(through the dialect and meta documents). The JSON Schema 2020-12 meta-schemas
come with `github.com/santhosh-tekuri/jsonschema/v6` (test-only dependency) and
are not downloaded. The vocabulary identifier
`https://spec.openapis.org/oas/3.2/vocab/base` is an identifier, not a document.
