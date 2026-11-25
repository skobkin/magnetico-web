# Testing

How the repository's tests are classified, which command runs each class, and how historical reports map to the
current names. Commands marked **(run in Task 08)** were executed for `docs/wip/08-ci-oci-report.md`; the others are
documented, not re-run there (the heavy ones are manual by design).

## Classification

| Class | What | Runs | Gate |
|---|---|---|---|
| **Regular CI** | unit tests; PostgreSQL integration tests; real-binary end-to-end tests; short regression, security-regression and fault-injection tests; fuzz **seed corpora**; `govulncheck`, the native archive snapshot and its smoke test, the front-end self-tests | every pull request, push to `master`, tag, manual and cron run (`.woodpecker/ci.yaml`) | blocks merge and release |
| **Migration end-to-end** | the offline migrator's crash-and-resume, fault-injection and binary smoke tests in `internal/e2e/cli` | inside `test` for a push to `master`, tag, manual and cron run; for a pull request skipped there and run by the `migration-e2e` step only when it changes migration paths | blocks the release; for a pull request, the merge when it runs |
| **Race** | the whole suite under `-race` (step `race`) | push to `master`, tag, manual and cron run; never a pull request (owner decision) | blocks the release |
| **Explicit manual only** | large synthetic dataset generation; load, capacity and memory-floor benchmarks; long soak and stress tests; extended fuzzing; the diagnostic build; live-DHT acceptance | by an operator, on a disposable database or host | never in CI |

Security regression tests and ordinary PostgreSQL integration tests are **not** opt-in: they are in the regular class. A
test is opt-in only when it needs something a CI run must not have (a large dataset, hours of runtime, a real DHT) or
when it mutates a database it did not create.

Heavy code is gated twice: by the build tag `loadtest` (dataset, load, soak, web load; it is not even compiled by a
plain `go build`/`go test`) **and** by explicit environment authorization for anything that mutates a database
(`MAGNETICO_BENCH_LARGE=1`, a DSN, and `MAGNETICO_BENCH_CONFIRM_DISPOSABLE_DB` holding the database's *name*; the guards
live untagged in `internal/e2e/cli/benchguard_test.go` and run in regular CI). The diagnostic listener is compiled only
with the tag `diag`; no release build contains it (`TestDefaultBinaryCarriesNoBuildTagsAndNoDiagnosticSymbols`).

## Regular CI: exact commands

A disposable PostgreSQL 17 server with **trust authentication** is required: the harness derives role DSNs without a
password, so a password-protected server fails the tests. It creates and drops only `magnetico_test_<random>` databases.

```sh
# a throwaway server on loopback (docker; the harness can also start one itself when no DSN is set)
docker run -d --rm --name m-pg -e POSTGRES_HOST_AUTH_METHOD=trust -p 127.0.0.1:55432:5432 postgres:17
export MAGNETICO_TEST_POSTGRES_DSN='postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable'
export MAGNETICO_TEST_REQUIRE_POSTGRES=1      # an unreachable database fails instead of skipping

# the whole suite, verified configuration -p 3 (see below)            (run in Task 08)
go test -p 3 -count=1 -json ./... > test.json || go run ./ci/skipcensus --failures < test.json
go run ./ci/skipcensus --allow ci/skip-allowlist.txt < test.json

golangci-lint fmt --diff ./... && golangci-lint run ./...              (run in Task 08: 0 issues)
go vet ./... && go vet -tags loadtest,diag ./...                       (run in Task 08)
```

`ci/skipcensus` fails the run when a test was skipped for a reason that is not on `ci/skip-allowlist.txt` (a missing
database above all) or when no test ran. The allow-list holds the opt-in harnesses, the bodies of subprocess helper
tests, fuzz-seed entries a target rejects as invalid input, and three **known coverage gaps of the CI environment**:
`TestOpenRefusesFsyncOff` and `TestIdentitySameDatabaseViaSocketAndTCP` need a locally controlled cluster (they run when the
harness starts docker or `initdb` itself), and `TestFaultRestartedSourceIsNotDrift` restarts a PostgreSQL container and
needs the docker CLI, which the CI test image does not have. The allow-list was derived from a run **inside the CI image**
(`golang:1.27.4-alpine`), not from a developer machine: a test that finds docker on a VM runs there and would be missing. One more entry, `TestSSLCertFileIsHonoured`, skips when the process had already
initialised the system roots; it is only effective in isolation.

Narrower runs while developing: `go test ./internal/web/...`, `go test -run TestWebBinary ./internal/e2e/cli`, the
OpenAPI tests `go test ./internal/web/apidocs/... && go test -run OpenAPI ./internal/e2e/cli`.

**Why `-p 3`.** The suite starts real PostgreSQL clusters and real binaries, and several tests assert on short timing
windows. At the default package parallelism on a busy host a *different* timing-dependent test failed in each of four
runs (07J); at `-p 3` the plain and the race suites were green. This is a workaround that hides timing sensitivity,
**not** a fix: issue #53. Do not "fix" such a failure by lengthening a timeout; a failure that reproduces in isolation is
a real defect.

*State after Task 09A and 09B (the evidence for issue #53, `docs/wip/09B-final-local-preflight-report.md`).* The named
tests are deterministic or were proven to be product defects and fixed (#52); the full suite at the default parallelism,
on a quiet machine, was green for the plain and the race run in 09A and again in 09B. Under artificial CPU load (6 busy
loops on 8 CPUs) the plain run was green but the race run failed twice, for two different reasons, **neither a timing
window of a named test**: the disposable PostgreSQL's default `max_connections = 100` was exhausted by the concurrent
packages (`too many clients already`), and one test's own bookkeeping had an ABA flaw (fixed). The connection budget is a
property of the suite's resource use, not of any test: **`-p 3` stays** until the suite is shown to pass the loaded
default-parallelism race run too (for example with a bounded connection budget per package).

## Release CI: exact commands

`ci.yaml` adds `-timeout 20m` to every suite (`internal/e2e/cli` alone takes 6 to 10 minutes, close to the default of
10). For a pull request the `test` step skips the migration end-to-end tests, which the `migration-e2e` step runs when
the pull request changes migration paths; every other event runs the whole suite in `test`:

```sh
P='^(TestFault|TestImportWithReadOnlySourceRole|TestMigrateTorrentsBinarySmoke|TestMigrateVerifyRetryBinarySmoke|TestMigrateExportCleanupBinarySmoke)'
go test -p 3 -count=1 -timeout 20m -json ./... > test.json                      # master, tag, manual, cron
go test -p 3 -count=1 -timeout 20m -json -skip "$P" ./... > test.json           # pull request
go test -p 3 -count=1 -timeout 20m -json -run "$P" ./internal/e2e/cli > migration.json   # pull request, migration paths
go test -race -p 3 -count=1 -json ./... > race.json                    (run in Task 08: 66 packages ok)
go run ./ci/skipcensus --allow ci/skip-allowlist.txt < race.json
GOTOOLCHAIN=go1.27.4 go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...                          (run in Task 08: none found)
GOTOOLCHAIN=go1.27.4 go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 -tags loadtest,diag ./...      (run in Task 08: none found)
```

The native archive smoke test runs in `ci.yaml` (`ci/native-smoke.sh`, tested in `internal/repocheck`). The container
smoke test (`ci/image-smoke.sh`) and the image scan need a privileged Docker daemon and are run locally before a tag;
the publication steps are `release.yaml` (GoReleaser and the buildx plugin). See
[`release-engineering.md`](release-engineering.md).

## Explicit manual execution only

**Front-end self-tests** (`internal/web/ui/jstest/admin_test.js`: gaps, no-data and error states, summary table, untrusted
strings against a fake DOM, default series, UTC axes, notes and freshness captions). The repository has no JavaScript toolchain, so they are not part of `go test` (the Go suite pins
the source invariants of `admin.js` and the vendored uPlot statically, `internal/web/ui/admin_assets_test.go`); CI runs them in the `frontend-selftest` step of `ci.yaml` (Node pinned by version and digest, under three browser time zones; Node is never part of a build of the product). Run them with
any Node 20+ (no packages):

```sh
docker run --rm -v "$PWD":/w -w /w node:22-alpine node internal/web/ui/jstest/admin_test.js
```

The load and dataset harnesses below are not run by CI. Each needs a disposable target; the load and dataset harnesses additionally refuse any
database that is not the confirmed, local, benchmark-shaped one.

```sh
# Large synthetic dataset ("w9-large-v1": the identifier is frozen, see the mapping below)
MAGNETICO_BENCH_LARGE=1 MAGNETICO_BENCH_DSN=postgres://…/bench MAGNETICO_BENCH_CONFIRM_DISPOSABLE_DB=bench \
  MAGNETICO_BENCH_DATADIR=<data dir> \
  go test -tags loadtest -run TestSeedLargeDataset -count=1 -v -timeout 0 ./internal/e2e/cli
# (MAGNETICO_BENCH_SCALE=0.01 for a pilot; a database seeded before the marker existed is marked once with
#  MAGNETICO_BENCH_MARK=1 … -run TestMarkBenchmarkDatabase)

# Load, capacity, memory-floor, knee and regression profiles, and the soak
MAGNETICO_BENCH_LARGE=1 MAGNETICO_BENCH_DSN=… MAGNETICO_BENCH_CONFIRM_DISPOSABLE_DB=bench \
  MAGNETICO_BENCH_OUT=<dir> MAGNETICO_BENCH_LABEL=<name> MAGNETICO_BENCH_PROFILE=steps \
  go test -tags loadtest -run 'TestLoad$' -count=1 -v -timeout 4h ./internal/e2e/cli
# profiles: steps, soak, memfloor, knee, regress, diag (see docs/wip/07J-measurements/README.md; translate with the
# mapping below)

# The statistics qualification (Task 09): synthetic data, the real binaries, loopback only. Each test names its criterion
# (docs/wip/09-statistics-admin-plan.md, "Acceptance criteria") and fails when it is not met; results are appended as JSON
# lines to $MAGNETICO_STATS_OUT. MAGNETICO_STATS_QUALIFY_SCALE=0.1 shortens every duration for a smoke run.
export MAGNETICO_STATS_OUT=<dir>
#   MAGNETICO_STATS_BASELINE_CRAWLERD=<binary>  the pre-batching crawler client (built from commit 8f339306 with the -stamp patch)
go test -tags loadtest -count=1 -v -timeout 3h -run 'TestQualifyOverheadAndSabotage|TestQualifySteadyStateMemory|TestQualifyWriteFrequency|TestQualifyBatching|TestQualifyAuthFlood|TestQualifyQueueSamplingAdmissionInflation|TestQualifyStorageGrowth|TestQualifyDashboardLatency' ./internal/e2e/cli
go test -tags loadtest -count=1 -v -run TestMeasureCachedVersusUncached ./internal/ingest/crawlerauth
go test -tags loadtest -count=1 -v -run TestMeasureQueueSamplingCost ./internal/ingest/stats
go test -run xxx -bench 'BenchmarkRecorder|BenchmarkAuthenticateCacheHit' -benchmem ./internal/ingest/stats ./internal/ingest/crawlerauth

# The earlier web load campaign
MAGNETICO_BENCH_LARGE=1 MAGNETICO_LOAD_DSN=… go test -tags loadtest -run 'TestWebLoad$' -count=1 -v -timeout 4h ./internal/e2e/cli

# Other opt-in measurements: each skips with a message naming the variable it needs
#   TestMeasure (MAGNETICO_CRAWLQUEUE_MEASURE_DIR), TestE2E* (MAGNETICO_E2E_*_DIR), TestMeasureSearchPlans (MAGNETICO_MEASURE=1),
#   TestColdBuildReport/TestMeasureTreeSize/… (MAGNETICO_BENCH_LARGE=1), TestWebDemoServer (MAGNETICO_DEMO_DIR)

# The pre-RC qualification of Task 09A (docs/wip/09A-pre-rc-qualification-report.md)
#   the locked-statistics sabotage at a production-like rate, with the two controls (not an artificial maximum rate):
MAGNETICO_STATS_OUT=<dir> go test -tags loadtest -count=1 -v -timeout 3h -run TestQualifySabotageAtProductionRate ./internal/e2e/cli
#   the combined-service soak (crawlers, interactive web users, administrator, rotation, restart, statistics outage), profile soak09a
#   of TestLoad; MAGNETICO_BENCH_SOAK_SCALE=1 is about 4 h 50 min, 0.05 a 15-minute smoke run:
MAGNETICO_BENCH_LARGE=1 MAGNETICO_BENCH_DSN=… MAGNETICO_BENCH_CONFIRM_DISPOSABLE_DB=bench MAGNETICO_BENCH_OUT=<dir> \
  MAGNETICO_BENCH_LABEL=soak09a MAGNETICO_BENCH_PROFILE=soak09a MAGNETICO_BENCH_TAGS=diag MAGNETICO_BENCH_SOAK_SCALE=1 \
  go test -tags loadtest -run 'TestLoad$' -count=1 -v -timeout 8h ./internal/e2e/cli

# Extended fuzzing (seed corpora already run in regular CI); one target per invocation:
go test -fuzz=FuzzDecode      -fuzztime=10m ./codec
go test -fuzz=FuzzDecode      -fuzztime=10m ./internal/ingest/record
go test -fuzz=FuzzDecodeCursor -fuzztime=10m ./internal/web/search
go test -fuzz='FuzzStr$'      -fuzztime=10m ./internal/web/fileview
go test -fuzz=FuzzParsePath   -fuzztime=10m ./internal/web/fileview
go test -fuzz=FuzzDisplayLossless -fuzztime=10m ./internal/web/pathfmt
go test -fuzz='^Fuzz$'        -fuzztime=10m ./bencode
go test -fuzz=FuzzInterfaceRoundTrip -fuzztime=10m ./bencode
go test -fuzz=FuzzCheckDepth  -fuzztime=10m ./bencode
go test -fuzz='^Fuzz$'        -fuzztime=10m ./metainfo

# Diagnostic build (never shipped): serves goroutine/heap/pool statistics as JSON on a loopback address
go build -tags diag -o magnetico-diag ./cmd/magnetico
MAGNETICO_DIAG_ADDR=127.0.0.1:6060 ./magnetico-diag web …
```

**Live-DHT acceptance** (crawler → ingest → PostgreSQL → web with CI-built images on a separate DHT-capable host, never
from a development VM) is a manual checklist item of `docs/wip/rc0-acceptance-checklist.md`, not a command.

The behaviour "heavy tests skip with a message naming the missing authorization when run without it" was checked in
Task 08 (`go test -tags loadtest -run 'TestLoad$|TestSeedLargeDataset|TestWebLoad$'`).

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
| env | `MAGNETICO_W5_DEMO` | `MAGNETICO_DEMO_DIR` | `TestWebDemoServer` (`./internal/e2e/cli`) |
| env | `MAGNETICO_W5_OUT` | `MAGNETICO_TREE_SIZE_OUT` | `TestMeasureTreeSize` (`./internal/web/fileview`) |
| env | `MAGNETICO_W5_OUT2` | `MAGNETICO_INDEX_LIMITS_OUT` | `TestMeasureComplexityLimits` (`./internal/web/filetree`) |
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
