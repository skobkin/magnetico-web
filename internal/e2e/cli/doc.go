// Package cli holds the real-binary and in-process tests of the magnetico
// executable: the migration, fault-injection and verification suites, the web
// service (browse, authentication, account, API, feeds), the security
// regression sweeps, and the benchmark harness.
//
// What runs when:
//
//   - Regular CI (`go test -p 3 ./...`): everything except the files tagged
//     `loadtest`. PostgreSQL integration tests are never opt-in; they need a
//     disposable server (MAGNETICO_TEST_POSTGRES_DSN, see internal/pgtest).
//   - `-tags loadtest` (manual only): dataset generation (TestSeedLargeDataset),
//     the load, capacity, memory-floor and soak harness (TestLoad), the web load
//     campaign (TestWebLoad) and the orchestrator's drain self-test
//     (TestDrainReportsQueryErrorsAndEnforcesItsDeadline, which was an untagged
//     test before Task 08 and is not run by CI). Besides the tag, the three
//     harnesses need explicit environment authorization
//     (MAGNETICO_BENCH_LARGE=1 plus MAGNETICO_BENCH_DSN and
//     MAGNETICO_BENCH_CONFIRM_DISPOSABLE_DB naming the database), because they
//     mutate a disposable database. The guards live in benchguard_test.go and
//     run in regular CI.
//   - The diagnostic build (`go build -tags diag ./cmd/magnetico`) is a separate,
//     never-shipped binary; see internal/app.
//
// docs/testing.md has the exact commands and the old-to-new name mapping for the
// historical reports.
package cli
