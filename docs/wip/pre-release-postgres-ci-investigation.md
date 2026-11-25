# Pre-release investigation — PostgreSQL integration tests and CI lifecycle

Branch `fix/post-merge-quick-fixes` (PR #56), on `master` `47742edf`. Evidence: the PostgreSQL service log of pipeline #123
supplied by the owner, the public Woodpecker API (`ci.skobk.in`, repository 27; read only), and local runs against a
disposable PostgreSQL 17 (`postgres:17-alpine`, trust authentication, loopback). No production system, tag, release or
published artifact was touched; no CI configuration was changed.

## 1. PostgreSQL service lifecycle

**Confirmed cause: the `race` step was still running.** The pipeline was not stuck, and the service was not leaked.

| Pipeline | Event | What the API shows |
|---|---|---|
| #123 | push to `master` (`47742edf`) | every step finished by 21:23:28 except `race`, started 21:23:28 (its dependencies had just finished), **still running** when the owner cancelled at 21:27:30; `postgres` service killed at 21:27:10 by the cancellation |
| #125 | tag `0.8.0-rc0` | the same workflow: `race` ran 21:29:16–21:37:17 (8 min) and the `postgres` service ended at 21:37:17, the moment `race` finished; then `release` (Forgejo, both registries) succeeded |
| #126 | PR #56 | cancelled during `test`; unrelated |
| #124 | tag `0.8.0-rc0` | compile error `secret "gitea_token" not found` before any step ran; #125 is the retry |

`race` is deliberately last (it depends on `test`, `govulncheck`, `openapi`, `native-smoke`, `workflow-invariants`,
`frontend-selftest`) and takes about 8 minutes, so for its whole duration every other step shows as finished while the
service, rightly, stays up. The service log agrees: it continues until 21:26:32 with migration and ingest test traffic
(the race suite), then stops when the pipeline was cancelled. #125 shows that teardown works normally.

**Why it looked finished.** The owner saw every step as done. The `race` step's own log (API, step 11042) ends 5 s after
the step started, with `go: downloading …` lines: `go test -race … -json > /tmp/race.json` writes nothing to the step log
until the suite ends, and the summary is printed only then. For four minutes the step showed no output at all. Its
state stayed running: the API records it as `killed` with no exit code, and the PostgreSQL log has test traffic up to
21:26:32, after every other step had ended (the last, `native-smoke`, at 21:23:28). The `test` step behaves the same
way for its 7 minutes.

- **Project defect:** none found in the lifecycle. Every `go test` step has `-timeout 20m`, the repository timeout is
  60 min, so a hung test cannot hold the service indefinitely. **Observability gap, addressed afterwards** (commit "ci: live per-package progress…", owner's request): `test`, `race` and
  `migration-e2e` now run through `ci/go-test.sh`. It prints a line per package as it finishes and a failed test's output
  at once, keeps the JSON for the census, and keeps go test's exit status through the pipe.
- **Woodpecker defect:** none found; services end with the workflow as designed.
- **Interference between pipelines:** none through shared state. Each workflow has its own `postgres` service container
  and network, and every test database is `magnetico_test_<random>`. #123, #125 and #126 did run **at the same time on
  agent 1** (21:20–21:23). That violates the "alone on the agent" assumption behind `-p 3` (#53) and slows the
  timing-sensitive suites, but it changes no database outcome. Limiting concurrent workflows per agent is agent
  configuration (not changed here).
- **Lingering test processes:** none in the PostgreSQL log beyond the leaks of section 3.

## 2. The broken source-snapshot SQL (migration tests)

**Confirmed defect, now fixed.** `catalogSnapshot` (`internal/migration/legacy/original/fixture_test.go`) built its data half
as `SELECT string_agg(line …) FROM (SELECT 'row torrents ' || … UNION ALL …) q`. The first branch had no `AS line`, so
the query always failed with `column "line" does not exist`, or with `column "path" does not exist` on the layout that
drops that column (both in the CI log). The error was turned into the marker `data (unreadable)`, which compared equal
before and after. **Every "source left untouched" assertion of the package checked the catalog only, never the data**:
seven tests (the four `TestOpenRefuses*`, with 13 layouts in `TestOpenRefusesUnsupportedLayouts`,
`TestProbeAndRefusalLeaveSourceUntouched`, and the fetch and reader read-only tests).

**Fix** (`dd2f4306`): `sourceSnapshot` renders the catalog plus every table of the `magneticod` schema whole (row-to-text,
partitioned parents once, partitions never alone), so it needs no column list and works on the broken layouts. Any query
failure now fails the test. The other migration snapshot helpers (`state`, `forensics`, runner, verify, e2e fault
snapshots) already fail on query errors; none had the problem.

**Regression tests** (`snapshot_integration_test.go`). Both failed against the old helper (every subtest), and pass now:

- `TestSourceSnapshotDetectsDataChanges`: torrent renamed, added or deleted; file path or size changed; file deleted;
  schema version changed; and changes on three broken layouts (missing column, unexpected table, partitioned `files`).
- `TestSourceSnapshotReportsUnreadableData`: a table that cannot be read (exclusive lock held, `lock_timeout`) makes
  the snapshot an error naming the table, not a marker.

All existing assertions of the package pass with the stronger snapshot: the adapter really leaves source data untouched.

## 3. "database … is not currently accepting connections"

**Confirmed: deliberate, bounded fault injection; no uncontrolled retry loop.** `ALTER DATABASE … ALLOW_CONNECTIONS false`
is set only by three outage tests. Run alone against the disposable server:

| Test | Refusals | Pattern |
|---|---:|---|
| `TestStatsConservedAcrossAFullPostgresOutageSpanningAFlush` (`internal/e2e/cli`) | 559 in 55 s | one per submission paced every 100 ms (546 of 558 gaps are 0.1 s), plus a few from 10 s periodic work; ends with the outage |
| `TestE2EPostgresOutageAndRecovery` (`internal/e2e`) | 4 in 1.7 s | growing gaps (client backoff) within the 4 s outage |
| `TestServiceStorageOutageAndRecovery` (`internal/ingest/server`) | 0 | the outage is observed through 503s before any new connection is tried |

The CI log's burst (`magnetico_test_ba705a3888d8c0d2`, 21:21:11.589–21:22:06.438, about 550 lines at 100 ms) is the
first test, in the `test` step. The two small groups with growing gaps (21:16:05, `3ad099…`; 21:24:59, `9011dc…`) match
the second (ordinary and race runs); that attribution is by pattern, not confirmed. Each refusal is one client request
reaching the database during the outage the test creates. These are the expected errors and are not suppressed.

**Leaked pools (confirmed, fixed).** A temporary probe in the harness listed, before each forced drop, the client backends
still connected 3 s after the test ended. One full run (`-p 3`, all packages) found 8 connections in 4 tests; nothing
else in the suite leaks:

| Test | Leak | Disposition |
|---|---|---|
| `TestInitCreatesTheCrawlersTableInSchemaVersionOne`, `TestReinitializingTheIngestSchemaLeavesMagneticodAndWebUntouched` | a successful `pgqueue.Open` discarded | closed (`26021d96`) |
| `TestRunnerDeferredIndexesInterruptedResume` | a successful `persistence.MakeDatabase` discarded | closed (`26021d96`) |
| `TestShutdownAbandonsUninterruptibleAddTorrent` | 5 connections of the abandoned instance (queue, auth, stats flush, store) | **by design**: the test proves the instance is abandoned with its pools left open; those connections only are declared with `pgtest.AllowConnectionsOpenNow` |

No product pool leak: the refusal paths of `pgqueue.Open` and `MakeDatabase` close their pools, and no test other than
the deliberate case left a database connection behind. This says nothing about workers that hold no connection.

**Regression guard** (`8bedb092`, corrected after review in the follow-up commit). `pgtest.TestDatabase`'s cleanup
fails the test for every **PostgreSQL client connection** still open to its database 10 s after the test ended (pid,
state, application, last query). Before, `DROP DATABASE … WITH (FORCE)` ended such connections silently, and they showed
only as `terminating connection due to administrator command`.

- **Scope.** It detects remaining connections only. A goroutine, worker, timer or file that holds no connection to the
  test database is invisible to it, so a green run does not prove the absence of other background leaks.
- **Budgets.** The inspection and the forced drop each have their own connection and time budget. The drop is attempted
  even when the inspection fails or uses up its budget.
- **Exemption.** `pgtest.AllowConnectionsOpenNow` waives only the connections open when it is called.
  `TestShutdownAbandonsUninterruptibleAddTorrent` calls it after the abandoned instance has settled (5 connections), so a
  connection the replacement instance leaves open still fails the test. The test now also requires the replacement
  instance to shut down cleanly.
- **Tests.** `TestTestDatabaseFailsATestThatLeavesConnectionsOpen` runs a helper subprocess on the parent's own server
  (its `AdminDSN`, PostgreSQL required, never a skip) in six modes:
  - a closed connection passes;
  - a leftover connection fails and is named;
  - a declared connection passes;
  - a connection opened after the declaration fails, naming only that one;
  - an inspection that fails, or hangs until its budget runs out, fails the test and the database is still dropped.

  The leftover modes failed against the original harness. The hanging-inspection mode failed against the reviewed
  shape (one budget for both: the database was left behind). Under the guard, the four leaking tests failed before
  their fixes and pass after. The helper's skip is on `ci/skip-allowlist.txt`. The guard waits only when something is
  left, so it adds no time to a clean run.

**Other log lines, expected and not individually attributed:**

- `terminating connection due to administrator command` comes from `pg_terminate_backend` in the outage and lock-loss
  tests, and from forced drops of the deliberate case.
- `unexpected EOF … with an open transaction` and `PID … in cancel request did not match any process` come from tests
  that cut connections (the proxy fixtures) or kill processes.
- `invalid length of startup packet` comes from raw TCP port probes.
- The `ERROR` lines sampled are the statements of negative tests (constraint, permission and trigger refusals,
  injected failures); the full run surfaced no failing test behind any of them.

## 4. Results

Commits on `fix/post-merge-quick-fixes`: `dd2f4306` (snapshot), `26021d96` (pools closed), `8bedb092` (harness guard), plus
this report and a `docs/testing.md` note.

- Focused: each new test failed first and passes; `go test ./internal/migration/legacy/original` passes; the four leaking
  tests fail under the guard before their fixes and pass after.
- Whole suite once each (the guard is global), `-p 3 -count=1 -timeout 20m`, CI's commands, against the disposable server:
  - ordinary: exit 0, 3,488 passed, 0 failed, 28 skipped (all allowed by `skipcensus`), 6 min 35 s;
  - race: exit 0, 3,488 passed, 0 failed, 28 skipped (allowed), 7 min 41 s.
- `go vet`, `golangci-lint run` and `fmt --diff`: 0 issues.

## 5. Remaining uncertainties

- The guard's 10 s grace was enough in one ordinary and one race run on the development VM. A much slower CI agent could
  in principle retire a closed pool's backends more slowly; the failure would name the connection.
- Concurrent workflows on one agent (section 1) remain possible until the agent's capacity is limited; they affect
  timing-sensitive tests (#53), not correctness.
- The two small refusal groups of the CI log are attributed by pattern only.
