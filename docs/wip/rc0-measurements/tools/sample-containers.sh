#!/usr/bin/env bash
# Samples the cgroup v2 counters of running containers (RC0 automated acceptance).
#   sample-containers.sh OUT.jsonl INTERVAL_SECONDS NAME_REGEX
# One JSON line per container per interval: CPU seconds, memory.current/peak, the
# memory.events counters, pids, and the host RSS of the container's PID 1. Reads only
# /sys/fs/cgroup and /proc; never execs into a container. Stops when killed.
set -u
out="$1"; every="${2:-5}"; re="${3:-.}"
while :; do
  ts=$(date -u +%FT%T.%3NZ)
  for name in $(docker ps --format '{{.Names}}' | grep -E "$re"); do
    read -r id pid < <(docker inspect -f '{{.Id}} {{.State.Pid}}' "$name" 2>/dev/null) || continue
    cg=/sys/fs/cgroup/system.slice/docker-$id.scope
    [ -d "$cg" ] || continue
    cpu=$(awk '$1=="usage_usec"{print $2}' "$cg/cpu.stat")
    ev=$(awk '{printf "%s\"%s\":%s", (NR>1?",":""), $1, $2}' "$cg/memory.events")
    rss=$(awk '$1=="VmRSS:"{print $2}' /proc/$pid/status 2>/dev/null)
    printf '{"t":"%s","c":"%s","cpu_s":%s,"mem_cur":%s,"mem_peak":%s,"pids":%s,"rss_kib":%s,"events":{%s}}\n' \
      "$ts" "$name" "$(awk -v u="$cpu" 'BEGIN{printf "%.3f", u/1e6}')" "$(cat $cg/memory.current)" "$(cat $cg/memory.peak 2>/dev/null || echo 0)" \
      "$(cat $cg/pids.current)" "${rss:-0}" "$ev" >> "$out"
  done
  sleep "$every"
done
