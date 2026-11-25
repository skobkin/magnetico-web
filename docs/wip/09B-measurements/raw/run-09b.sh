#!/bin/sh
# Task 09B final verification: clean checkout of the final commit.
set -u
cd /home/herdr/qual/09B
export MAGNETICO_TEST_POSTGRES_DSN='postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable'
export MAGNETICO_TEST_REQUIRE_POSTGRES=1
L=/home/herdr/src/magnetico-web/.superpowers/sdd/09B/qualification
stamp() { echo "$(date -u +%FT%TZ) $*" >> $L/progress.txt; }
suite() { # name, go test args...
  n=$1; shift
  stamp "$n"; go test "$@" -count=1 -json ./... > $L/$n.json 2> $L/$n.err; stamp "$n exit $?"
  go run ./ci/skipcensus --failures < $L/$n.json > $L/$n-failures.txt 2>&1
  go run ./ci/skipcensus --allow ci/skip-allowlist.txt < $L/$n.json > $L/$n-census.txt 2>&1; stamp "$n census exit $?"
}
: > $L/progress.txt
stamp "commit $(git rev-parse HEAD)"
stamp build; go build ./... > $L/build.log 2>&1; stamp "build exit $?"
stamp vet; (go vet ./... && go vet -tags loadtest,diag ./...) > $L/vet.log 2>&1; stamp "vet exit $?"
stamp lint; (golangci-lint fmt --diff ./... && golangci-lint run ./...) > $L/lint.log 2>&1; stamp "lint exit $?"
suite test-p3 -p 3
suite race-p3 -race -p 3
stamp govulncheck; (GOTOOLCHAIN=go1.27.4 go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./... && GOTOOLCHAIN=go1.27.4 go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 -tags loadtest,diag ./...) > $L/govulncheck.log 2>&1; stamp "govulncheck exit $?"
suite test-default
suite race-default -race
# artificial CPU load: 6 busy loops on the 8 CPUs while the default-parallelism suites run
for i in 1 2 3 4 5 6; do (while :; do :; done) & echo $! >> $L/load.pids; done
stamp "cpu load started"
suite test-default-load
suite race-default-load -race
kill $(cat $L/load.pids) 2>/dev/null; rm -f $L/load.pids; stamp "cpu load stopped"
# isolated stress of the three tests named by #52/#53 (race detector, 200 runs)
stamp stress
go test -race -count=200 -run '^TestNearExhaustedDeadlineAfterGateWaitStartsNoStatement$' ./internal/web/search > $L/stress-search.txt 2>&1; stamp "stress search exit $?"
go test -race -count=200 -run '^TestConcurrentIdenticalMissesCollapse$' ./internal/ingest/existence > $L/stress-existence.txt 2>&1; stamp "stress existence exit $?"
go test -race -count=20 -run '^TestVerifyDecodeFinalizedRunNotInterruptedByCancel$|^TestVerifyFinish' ./internal/migration > $L/stress-migration.txt 2>&1; stamp "stress migration exit $?"
stamp done
