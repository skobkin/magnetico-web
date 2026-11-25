#!/bin/sh
# Task 09A qualification, step G1: clean checkout, sequential gates. Logs next to this script.
set -u
cd /home/herdr/qual/09A
export MAGNETICO_TEST_POSTGRES_DSN='postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable'
export MAGNETICO_TEST_REQUIRE_POSTGRES=1
L=/home/herdr/src/magnetico-web/.superpowers/sdd/09A/qualification
stamp() { echo "$(date -u +%FT%TZ) $*" >> $L/final-progress.txt; }
: > $L/final-progress.txt
stamp "commit $(git rev-parse HEAD)"
stamp build;      go build ./... > $L/final-build.log 2>&1; stamp "build exit $?"
stamp vet;        (go vet ./... && go vet -tags loadtest,diag ./...) > $L/final-vet.log 2>&1; stamp "vet exit $?"
stamp lint;       (golangci-lint fmt --diff ./... && golangci-lint run ./...) > $L/final-lint.log 2>&1; stamp "lint exit $?"
stamp test;       go test -p 3 -count=1 -json ./... > $L/final-test.json 2> $L/final-test.err; stamp "test exit $?"
go run ./ci/skipcensus --failures < $L/final-test.json > $L/final-test-failures.txt 2>&1
go run ./ci/skipcensus --allow ci/skip-allowlist.txt < $L/final-test.json > $L/final-test-census.txt 2>&1; stamp "census exit $?"
stamp race;       go test -race -p 3 -count=1 -json ./... > $L/final-race.json 2> $L/final-race.err; stamp "race exit $?"
go run ./ci/skipcensus --failures < $L/final-race.json > $L/final-race-failures.txt 2>&1
go run ./ci/skipcensus --allow ci/skip-allowlist.txt < $L/final-race.json > $L/final-race-census.txt 2>&1; stamp "race census exit $?"
stamp govulncheck; (GOTOOLCHAIN=go1.27.4 go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./... && GOTOOLCHAIN=go1.27.4 go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 -tags loadtest,diag ./...) > $L/final-govulncheck.log 2>&1; stamp "govulncheck exit $?"
# informational: the same suites at the DEFAULT package parallelism (judges the -p 3 workaround)
stamp "default-parallelism test"; go test -count=1 -json ./... > $L/final-default-test.json 2> $L/final-default-test.err; stamp "default-parallelism test exit $?"
go run ./ci/skipcensus --failures < $L/final-default-test.json > $L/final-default-test-failures.txt 2>&1
stamp "default-parallelism race"; go test -race -count=1 -json ./... > $L/final-default-race.json 2> $L/final-default-race.err; stamp "default-parallelism race exit $?"
go run ./ci/skipcensus --failures < $L/final-default-race.json > $L/final-default-race-failures.txt 2>&1
stamp done
