#!/bin/sh
# Usage: sh ci/go-test.sh NAME [go test arguments]
#
# Runs `go test -json ARGUMENTS` for a CI step:
#   - the log shows live progress: one line per package as it finishes (result and elapsed
#     time) and the output of a failed test as soon as it fails (skipcensus --progress);
#   - the complete JSON stream is kept in $GOTEST_DIR/NAME.json (default /tmp) for the
#     census (skipcensus --allow) that the step runs next;
#   - on failure the closing summary of every failed test and package is printed, and
#     the script exits 1.
# The exit status is go test's. POSIX sh has no pipefail and a pipe's status is its last
# command's, so go test's own status is recorded inside the pipe and checked after it; a
# missing record counts as a failure. SKIPCENSUS names a prebuilt skipcensus binary
# (default: built from ./ci/skipcensus).
set -eu

name=$1
shift
dir=${GOTEST_DIR:-/tmp}
json=$dir/$name.json
status=$dir/$name.status
census=${SKIPCENSUS:-}
if [ -z "$census" ]; then
	census=$dir/skipcensus
	go build -o "$census" ./ci/skipcensus
fi

rm -f "$status"
{
	rc=0
	go test -json "$@" || rc=$?
	echo "$rc" >"$status"
} | tee "$json" | "$census" --progress

rc=$(cat "$status" 2>/dev/null || echo "not recorded")
if [ "$rc" != 0 ]; then
	echo "go test failed (status $rc); failures:"
	"$census" --failures <"$json"
	exit 1
fi
