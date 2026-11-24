#!/bin/sh
# Wait until a PostgreSQL service answers, then prove it executes a query.
#
#   ci/wait-postgres.sh HOST [PORT] [TIMEOUT_SECONDS]
#
# Used by the Woodpecker workflows before the tests run: the test steps must not
# start against a service that is still initialising (their harness would then
# skip or fail for the wrong reason). Exit 1 after the timeout.
set -eu

host="${1:?usage: wait-postgres.sh HOST [PORT] [TIMEOUT_SECONDS]}"
port="${2:-5432}"
timeout="${3:-60}"
interval="${WAIT_POSTGRES_INTERVAL:-1}"

i=0
until pg_isready -h "$host" -p "$port" -U postgres -q; do
	i=$((i + 1))
	if [ "$i" -ge "$timeout" ]; then
		echo "wait-postgres: PostgreSQL at $host:$port is not ready after $timeout attempts" >&2
		exit 1
	fi
	sleep "$interval"
done

answer="$(psql -h "$host" -p "$port" -U postgres -tAc 'select 1')" || {
	echo "wait-postgres: PostgreSQL at $host:$port accepts connections but a query failed" >&2
	exit 1
}
[ "$answer" = 1 ] || {
	echo "wait-postgres: unexpected answer from PostgreSQL at $host:$port" >&2
	exit 1
}
echo "wait-postgres: PostgreSQL at $host:$port is ready"
