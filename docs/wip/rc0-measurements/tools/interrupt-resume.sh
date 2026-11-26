#!/usr/bin/env bash
# Interruption and resumption of `migrate torrents` with the published image on a
# separate disposable target (RC0 automated acceptance).
#
#   interrupt-resume.sh IMAGE TARGET_DB OUT_DIR
#
# Needs MAGNETICO_MIGRATE_DATABASE (the disposable target, schema-init done) and
# MAGNETICO_MIGRATION_SOURCE_DSN (the read-only legacy source) in the environment; they
# reach the container by name only. Sequence: start with --defer-secondary-indexes,
# SIGTERM (docker stop) after T1 seconds (expect exit 2), resume, SIGKILL (docker kill)
# after T2 seconds, resume, run to completion (expect exit 0). Each step's exit status
# and the last progress line go to OUT_DIR/steps.txt; the target's progress counters
# are read between steps.
set -u
IMG="$1"; DB="$2"; OUT="$3"; T1="${T1:-90}"; T2="${T2:-60}"
mkdir -p "$OUT"
state() {
	docker exec rc0-pg psql -X -U postgres -d "$DB" -At -c \
		"SELECT 'torrents=' || (SELECT count(*) FROM magneticod.torrents) || ' blobs=' || (SELECT count(*) FROM magneticod.torrent_file_blobs) || ' secondary_indexes=' || (SELECT count(*) FROM pg_indexes WHERE schemaname = 'magneticod' AND indexname LIKE 'idx_%')"
}
run() { # name, then an optional "stop:SECONDS" or "kill:SECONDS"
	local name="$1" action="${2:-}"
	docker run -d --name "rc0-ir-$name" --network rc0-net --read-only --cap-drop ALL --security-opt no-new-privileges \
		--stop-timeout 120 --log-driver local -e MAGNETICO_MIGRATE_DATABASE -e MAGNETICO_MIGRATION_SOURCE_DSN \
		"$IMG" migrate torrents --defer-secondary-indexes >/dev/null
	local t0; t0=$(date +%s)
	case "$action" in
	stop:*) sleep "${action#stop:}"; docker stop -t 120 "rc0-ir-$name" >/dev/null ;;
	kill:*) sleep "${action#kill:}"; docker kill "rc0-ir-$name" >/dev/null ;;
	esac
	local code; code=$(docker wait "rc0-ir-$name")
	docker logs "rc0-ir-$name" >"$OUT/$name.log" 2>&1
	docker rm "rc0-ir-$name" >/dev/null
	printf '%s action=%s exit=%s wall_s=%s last=[%s] target=[%s]\n' "$name" "${action:-none}" "$code" "$(($(date +%s) - t0))" \
		"$(grep -v '^\s*$' "$OUT/$name.log" | tail -1 | cut -c1-300)" "$(state)" | tee -a "$OUT/steps.txt"
}
: >"$OUT/steps.txt"
run 1-sigterm "stop:$T1"
run 2-resume-then-sigkill "kill:$T2"
run 3-resume-to-completion
