# Testing

(The test classification and the exact commands per category are added by the Task 08 documentation commit; this file already carries the mapping needed to reproduce historical reports.)

## Reproducing historical reports

Task 08 removed milestone names (`w9`, `07g`, `w5`) from permanent test infrastructure and moved the former root-package tests out of the repository root. The reports under `docs/wip/` are historical records and are **not** rewritten; use this table to translate their commands, or run them verbatim at the last commit before Task 08:

    git worktree add ../magnetico-pre08 35b457dd    # every historical command works unchanged there

Persisted benchmark-dataset identifiers were deliberately **not** renamed, so databases seeded by the 07J harness stay valid: the database comment marker `magnetico-w9-benchmark:w9-large-v1`, the generated accounts `w9u00`…`w9u31`, the seeded torrent-name pattern `w9ing`, the info-hash derivation seed and the `w9` label of the credentials the harness mints. `TestFrozenDatasetIdentifiersAreUnchanged` pins them.

`internal/repocheck` fails if a historical report mentions an old name that has no row below (`TestHistoricalNamesAreMapped`) or if a new name in a row does not exist in the code (`TestMappedNewNamesExist`).

| kind | old | new | notes |
|---|---|---|---|
| env | `MAGNETICO_W9_CONFIRM_DISPOSABLE_DB` | `MAGNETICO_BENCH_CONFIRM_DISPOSABLE_DB` | still holds the *name of the database* in the DSN |
| env | `MAGNETICO_W9_DATADIR` | `MAGNETICO_BENCH_DATADIR` |  |
| env | `MAGNETICO_W9_DSN` | `MAGNETICO_BENCH_DSN` |  |
| env | `MAGNETICO_W9_LABEL` | `MAGNETICO_BENCH_LABEL` |  |
| env | `MAGNETICO_W9_MARK` | `MAGNETICO_BENCH_MARK` |  |
| env | `MAGNETICO_W9_NO_CONTINUATION` | `MAGNETICO_BENCH_NO_CONTINUATION` |  |
| env | `MAGNETICO_W9_OUT` | `MAGNETICO_BENCH_OUT` |  |
| env | `MAGNETICO_W9_PROFILE` | `MAGNETICO_BENCH_PROFILE` |  |
| env | `MAGNETICO_W9_SCALE` | `MAGNETICO_BENCH_SCALE` |  |
| env | `MAGNETICO_W9_SOAK_SCALE` | `MAGNETICO_BENCH_SOAK_SCALE` |  |
| env | `MAGNETICO_W9_STEP_SECONDS` | `MAGNETICO_BENCH_STEP_SECONDS` |  |
| env | `MAGNETICO_W9_TAGS` | `MAGNETICO_BENCH_TAGS` |  |
| env | `MAGNETICO_W9_WEB_GOMEMLIMIT` | `MAGNETICO_BENCH_WEB_GOMEMLIMIT` |  |
| env | `MAGNETICO_W9_WEB_MEM` | `MAGNETICO_BENCH_WEB_MEM` |  |
| env | `MAGNETICO_W9_DIAG_ADDR` | `MAGNETICO_DIAG_ADDR` | diagnostic build only (see `diag` below); not in the list scanned from reports, kept for completeness |
| test | `TestW9AccountStateAndCredentialLifecycle` | `TestAccountStateAndCredentialLifecycle` | `./internal/e2e/cli` |
| test | `TestW9AdministratorHasNoExtraWebCapability` | `TestAdministratorHasNoExtraWebCapability` | `./internal/e2e/cli` |
| test | `TestW9AuthenticationFailuresAreIndistinguishable` | `TestAuthenticationFailuresAreIndistinguishable` | `./internal/e2e/cli` |
| test | `TestW9CredentialSeparationMatrix` | `TestCredentialSeparationMatrix` | `./internal/e2e/cli` |
| test | `TestW9DevFixturesToWeb` | `TestDevFixturesToWeb` | `./internal/e2e/cli` |
| test | `TestW9ForgedHostAndForwardedHeadersDoNotChangeLinks` | `TestForgedHostAndForwardedHeadersDoNotChangeLinks` | `./internal/e2e/cli` |
| test | `TestW9GracefulShutdownUnderLoad` | `TestGracefulShutdownUnderLoad` | `./internal/e2e/cli` |
| test | `TestW9HostileTorrentNamesRenderSafely` | `TestHostileTorrentNamesRenderSafely` | `./internal/e2e/cli` |
| test | `TestW9IngestToWebAndAPI` | `TestIngestToWebAndAPI` | `./internal/e2e/cli` |
| test | `TestW9Load` | `TestLoad` | `./internal/e2e/cli`; build tag `loadtest` required |
| test | `TestW9MalformedAndOversizedRequests` | `TestMalformedAndOversizedRequests` | `./internal/e2e/cli` |
| test | `TestW9MarkBenchmarkDatabase` | `TestMarkBenchmarkDatabase` | `./internal/e2e/cli` |
| test | `TestW9NoSecretsInUnintendedLocations` | `TestNoSecretsInUnintendedLocations` | `./internal/e2e/cli` |
| test | `TestW9RealBinaryResponsesMatchOpenAPI` | `TestRealBinaryResponsesMatchOpenAPI` | `./internal/e2e/cli` |
| test | `TestW9RouteTableAnonymousAndMethodRestrictions` | `TestRouteTableAnonymousAndMethodRestrictions` | `./internal/e2e/cli` |
| test | `TestW9RouteTableCSRFAndCrossOrigin` | `TestRouteTableCSRFAndCrossOrigin` | `./internal/e2e/cli` |
| test | `TestW9SMTPFailures` | `TestSMTPFailures` | `./internal/e2e/cli`; the old name is a `-run` prefix: it selects every test whose name starts with the new name |
| test | `TestW9SeedLargeDataset` | `TestSeedLargeDataset` | `./internal/e2e/cli`; build tag `loadtest` required |
| test | `TestW9ServicesFailClosedWithoutTheirSchemas` | `TestServicesFailClosedWithoutTheirSchemas` | `./internal/e2e/cli` |
| test | `TestW9SessionExpiryIdleAndRemember` | `TestSessionExpiryIdleAndRemember` | `./internal/e2e/cli` |
| test | `TestW9SlowHeaderConnectionsAreClosed` | `TestSlowHeaderConnectionsAreClosed` | `./internal/e2e/cli` |
| test | `TestW9TorrentMigrationToWeb` | `TestTorrentMigrationToWeb` | `./internal/e2e/cli` |
| test | `TestW9TrustedProxyBoundaryDecidesTheRateLimitIdentity` | `TestTrustedProxyBoundaryDecidesTheRateLimitIdentity` | `./internal/e2e/cli` |
| test | `TestWeb` | `TestWeb` | `./internal/e2e/cli`; the old name is a `-run` prefix: it selects every test whose name starts with the new name |
| test | `TestWebDemoServer` | `TestWebDemoServer` | `./internal/e2e/cli`; still gated by its own environment variables |
| test | `TestWebLoad` | `TestWebLoad` | `./internal/e2e/cli`; build tag `loadtest` required |
| test | `TestMigrateWebSchemaInit` | `TestMigrateWebSchemaInit` | `./internal/e2e/cli`; the old name is a `-run` prefix: it selects every test whose name starts with the new name |
| test | `TestSchemaDiagnostics` | `TestSchemaDiagnostics` | `./internal/e2e/cli`; the old name is a `-run` prefix: it selects every test whose name starts with the new name |
| tag | `w9diag` | `diag` | `go build -tags w9diag .` becomes `go build -tags diag -o magnetico-diag ./cmd/magnetico`; the listener is configured with `MAGNETICO_DIAG_ADDR` |
| command | `go test .` | `go test ./internal/e2e/cli` (add `-tags loadtest` for the heavy harness) | the root package no longer exists; the five message-formatting unit tests are in `./internal/app`. Historical commands also run verbatim at commit `35b457dd` |
