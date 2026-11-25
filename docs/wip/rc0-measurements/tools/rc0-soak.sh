#!/usr/bin/env bash
# RC0 published-image soak: control script (RC0 automated acceptance).
#
#   rc0-soak.sh start [SCALE]   build the harness from the checked-out worktree and start
#                               the soak as the transient user unit rc0-soak (it survives
#                               the end of the interactive session; linger is on)
#   rc0-soak.sh status          unit state, elapsed time, the newest monitor line, gate
#                               failures so far, container state
#   rc0-soak.sh stop            SIGTERM to the harness (it stops its containers); the
#                               JSONL written so far stays
#   rc0-soak.sh collect         summary of the JSONL into soak-summary.md (09A's summariser)
#
# The workload is profile soak09a of TestLoad (internal/e2e/cli/soak09a_test.go) in the
# container mode of the load harness (container_harness_test.go): the web and ingest
# services are containers of the published image; crawlerd and the web users are the
# harness's generators. SCALE multiplies every duration of the 310-minute timeline
# (5 = about 26 hours).
#
# Paths (override with environment variables):
#   RC0_ROOT   ~/tmp/magnetico/tmp/rc0        workspace (secrets/, soak/)
#   RC0_SRC    $RC0_ROOT/soak-src             a git worktree of the commit under test
#   RC0_IMAGE  the image reference (default: the RC0 digest)
set -euo pipefail
RC0_ROOT="${RC0_ROOT:-$HOME/tmp/magnetico/tmp/rc0}"
RC0_SRC="${RC0_SRC:-$RC0_ROOT/soak-src}"
RC0_IMAGE="${RC0_IMAGE:-docker.io/skobkin/magnetico-web@sha256:8f4ee86d92cc5fe42144851d80eb0106729eb9da053186d78c4d14cfc19a2ef7}"
OUT="$RC0_ROOT/soak/out"
UNIT=rc0-soak

cmd="${1:-status}"
case "$cmd" in
start)
	scale="${2:-5}"
	mkdir -p "$OUT" "$RC0_ROOT/soak/tmp" "$RC0_ROOT/soak/gocache"
	# shellcheck disable=SC1091
	source "$RC0_ROOT/secrets/soak.env"
	(cd "$RC0_SRC" && GOFLAGS= go test -c -tags loadtest -o "$RC0_ROOT/soak/cli.test" ./internal/e2e/cli)
	# The secrets reach the unit through an environment file readable only by the owner.
	umask 077
	cat >"$RC0_ROOT/secrets/soak-unit.env" <<EOF
MAGNETICO_BENCH_LARGE=1
MAGNETICO_BENCH_DSN=postgres://postgres:${SOAK_PG_PW}@127.0.0.1:56433/rc0soak?sslmode=disable
MAGNETICO_BENCH_CONFIRM_DISPOSABLE_DB=rc0soak
MAGNETICO_BENCH_OUT=$OUT
MAGNETICO_BENCH_LABEL=rc0soak
MAGNETICO_BENCH_PROFILE=soak09a
MAGNETICO_BENCH_SOAK_SCALE=$scale
MAGNETICO_BENCH_IMAGE=$RC0_IMAGE
MAGNETICO_BENCH_BIN=$RC0_SRC/docs/wip/rc0-measurements/tools/magnetico-image-cli
MAGNETICO_BENCH_CONTAINER_NETWORK=host
MAGNETICO_BENCH_CONTAINER_PREFIX=rc0soak
RC0_CLI_IMAGE=$RC0_IMAGE
TMPDIR=$RC0_ROOT/soak/tmp
GOCACHE=$RC0_ROOT/soak/gocache
PATH=$PATH
HOME=$HOME
EOF
	systemd-run --user --unit "$UNIT" --collect --property=EnvironmentFile="$RC0_ROOT/secrets/soak-unit.env" \
		--property=WorkingDirectory="$RC0_SRC/internal/e2e/cli" --property=KillSignal=SIGTERM --property=TimeoutStopSec=300 \
		--property=StandardOutput=append:"$RC0_ROOT/soak/harness.log" --property=StandardError=append:"$RC0_ROOT/soak/harness.log" \
		"$RC0_ROOT/soak/cli.test" -test.run 'TestLoad$' -test.count=1 -test.v -test.timeout 40h
	date -u +%FT%TZ >"$RC0_ROOT/soak/started"
	echo "started: scale $scale; status: $0 status"
	;;
status)
	systemctl --user show "$UNIT" -p ActiveState -p SubState -p ExecMainStartTimestamp -p ExecMainStatus 2>/dev/null || true
	[ -f "$RC0_ROOT/soak/started" ] && echo "started: $(cat "$RC0_ROOT/soak/started")  now: $(date -u +%FT%TZ)"
	f="$OUT/rc0soak.jsonl"
	if [ -f "$f" ]; then
		echo "records: $(wc -l <"$f")  events: $(grep -c '"kind":"event"' "$f")  web rounds: $(grep -c '"kind":"web_round"' "$f")"
		grep '"kind":"monitor"' "$f" | tail -1 | cut -c1-600
		grep '"kind":"event"' "$f" | tail -3 | cut -c1-300
	fi
	echo "gate failures so far: $(grep -c 'GATE FAILED' "$RC0_ROOT/soak/harness.log" 2>/dev/null || echo 0)"
	grep 'GATE FAILED' "$RC0_ROOT/soak/harness.log" 2>/dev/null | tail -5 || true
	docker ps --filter name=rc0soak --format '{{.Names}}\t{{.Status}}'
	;;
stop)
	systemctl --user stop "$UNIT"
	;;
collect)
	python3 -I "$RC0_SRC/docs/wip/09A-measurements/summarize_soak.py" "$OUT/rc0soak.jsonl" >"$RC0_ROOT/soak/soak-summary.md"
	echo "written: $RC0_ROOT/soak/soak-summary.md"
	;;
*)
	echo "usage: $0 start [SCALE] | status | stop | collect" >&2
	exit 2
	;;
esac
