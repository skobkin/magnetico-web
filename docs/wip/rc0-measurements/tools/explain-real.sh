#!/usr/bin/env bash
# EXPLAIN (ANALYZE, BUFFERS, SETTINGS) of the web service's search shapes
# (persistence/webread.go buildSearchSQL) on the migrated real-data sample (RC0
# automated acceptance). Derived from docs/wip/07J-measurements/explain.sh; the terms
# are generic format words chosen from the real name distribution (term-cooccurrence.txt),
# and the plans print no row contents.
#
#   usage: COLD=0|1 explain-real.sh <container> <database> <out-dir>
#
# COLD=1 restarts the PostgreSQL container before each shape (cold shared buffers, OS
# page cache warm), as 07J did; COLD=0 runs warm only. Then four runs per shape.
# The EXECUTE statements of part 2 send their rows to /dev/null (they are real names);
# only timings and plans are kept. ONLY_GENERIC=1 runs part 2 alone.
# Part 2 (generic plans): the service sends the LIKE patterns as bind parameters through
# pgx's cached prepared statements, so after five executions PostgreSQL may switch a
# statement to a generic plan; the script prepares the two-term search exactly as the
# service does, runs it with common patterns, then with a rare pair, and records the
# plan the rare pair gets, and the plan under plan_cache_mode = force_generic_plan.
set -euo pipefail
CT=$1; DB=$2; OUT=$3; COLD=${COLD:-0}
mkdir -p "$OUT"
psql() { docker exec -i "$CT" psql -U postgres -d "$DB" -X -q -A -t "$@"; }
restart() { docker restart "$CT" >/dev/null; until docker exec "$CT" pg_isready -q; do sleep 0.5; done; }

DEEP=$(psql -c "SELECT discovered_on||','||id FROM magneticod.torrents ORDER BY discovered_on DESC, id DESC OFFSET 1500000 LIMIT 1")
DEEP_T=${DEEP%,*}; DEEP_I=${DEEP#*,}
DEEPS=$(psql -c "SELECT total_size||','||id FROM magneticod.torrents ORDER BY total_size DESC, id DESC OFFSET 1500000 LIMIT 1")
DEEPS_T=${DEEPS%,*}; DEEPS_I=${DEEPS#*,}
HASH=$(psql -c "SELECT encode(info_hash,'hex') FROM magneticod.torrents ORDER BY id OFFSET 1700000 LIMIT 1")
ANCHOR=$(psql -c "SELECT id FROM magneticod.torrents ORDER BY id OFFSET 1700000 LIMIT 1")
COLS="id, info_hash, name, total_size, discovered_on, file_count"
T="magneticod.torrents"

declare -A Q
Q[01-empty-newest]="SELECT $COLS FROM $T ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[02-empty-oldest]="SELECT $COLS FROM $T ORDER BY discovered_on ASC, id ASC LIMIT 51"
Q[03-empty-size-desc]="SELECT $COLS FROM $T ORDER BY total_size DESC, id DESC LIMIT 51"
Q[05-deep-keyset-newest]="SELECT $COLS FROM $T WHERE (discovered_on, id) < ($DEEP_T, $DEEP_I) ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[06-deep-keyset-backward]="SELECT $COLS FROM $T WHERE (discovered_on, id) > ($DEEP_T, $DEEP_I) ORDER BY discovered_on ASC, id ASC LIMIT 51"
Q[07-deep-keyset-size]="SELECT $COLS FROM $T WHERE (total_size, id) < ($DEEPS_T, $DEEPS_I) ORDER BY total_size DESC, id DESC LIMIT 51"
Q[08-common-term-newest]="SELECT $COLS FROM $T WHERE name ILIKE '%1080p%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[09-common-two-terms-together]="SELECT $COLS FROM $T WHERE name ILIKE '%1080p%' AND name ILIKE '%x264%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[10-common-term-size]="SELECT $COLS FROM $T WHERE name ILIKE '%1080p%' ORDER BY total_size DESC, id DESC LIMIT 51"
Q[11-common-term-name]="SELECT $COLS FROM $T WHERE name ILIKE '%1080p%' ORDER BY name ASC, id ASC LIMIT 51"
Q[12-mid-term]="SELECT $COLS FROM $T WHERE name ILIKE '%flac%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[13-selective-term]="SELECT $COLS FROM $T WHERE name ILIKE '%ddp5%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[15-no-hit]="SELECT $COLS FROM $T WHERE name ILIKE '%zzqxj%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[16-cyrillic-term]="SELECT $COLS FROM $T WHERE name ILIKE '%сезон%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[17-common-term-deep-keyset]="SELECT $COLS FROM $T WHERE name ILIKE '%1080p%' AND (discovered_on, id) < ($DEEP_T, $DEEP_I) ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[18-infohash-lookup]="SELECT $COLS FROM $T WHERE info_hash = decode('$HASH','hex')"
Q[19-anchor-only-lookup]="SELECT discovered_on FROM $T WHERE id = $ANCHOR"
Q[20-rss-max-offset]="SELECT $COLS FROM $T ORDER BY id DESC LIMIT 1000 OFFSET 9000"
# F5: two common words that rarely occur together (counts in term-cooccurrence.txt)
Q[21-rare-pair-mp4-flac]="SELECT $COLS FROM $T WHERE name ILIKE '%mp4%' AND name ILIKE '%flac%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[22-rare-pair-mp3-x265]="SELECT $COLS FROM $T WHERE name ILIKE '%mp3%' AND name ILIKE '%x265%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[23-rare-pair-pdf-1080p]="SELECT $COLS FROM $T WHERE name ILIKE '%pdf%' AND name ILIKE '%1080p%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[24-rare-pair-mp4-flac-size]="SELECT $COLS FROM $T WHERE name ILIKE '%mp4%' AND name ILIKE '%flac%' ORDER BY total_size DESC, id DESC LIMIT 51"
Q[25-rare-pair-epub-720p]="SELECT $COLS FROM $T WHERE name ILIKE '%epub%' AND name ILIKE '%720p%' ORDER BY discovered_on DESC, id DESC LIMIT 51"

[ "${ONLY_GENERIC:-0}" = 1 ] || {
  echo "# EXPLAIN (ANALYZE, BUFFERS, SETTINGS) summary (real-data sample)"; echo
  echo "- container: $CT, database: $DB, taken: $(date -u +%FT%TZ), cold restarts: $COLD"
  psql -c "SELECT '- server: '||version()"; psql -c "SELECT '- '||name||' = '||setting||COALESCE(unit,'') FROM pg_settings WHERE name IN ('shared_buffers','work_mem','effective_cache_size','random_page_cost','max_parallel_workers_per_gather','jit','plan_cache_mode')"
  psql -c "SELECT '- torrents rows (estimate) '||reltuples::bigint||', table '||pg_size_pretty(pg_table_size('magneticod.torrents'))||', indexes '||pg_size_pretty(pg_indexes_size('magneticod.torrents')) FROM pg_class WHERE oid='magneticod.torrents'::regclass"
  echo; echo "| shape | first (ms) | warm 1 | warm 2 | warm 3 | rows | top plan node (last warm) | buffers (last warm) |"; echo "|---|---|---|---|---|---|---|---|"
} > "$OUT/SUMMARY.md"

[ "${ONLY_GENERIC:-0}" = 1 ] && unset Q && declare -A Q
for k in $(printf '%s\n' "${!Q[@]}" | sort); do
  sql=${Q[$k]}
  [ "$COLD" = 1 ] && restart
  t=()
  printf '%s\n' "$sql" | sed -E "s/decode\('[0-9a-f]{40}'/decode('<hash>'/" > "$OUT/$k.txt"
  for run in first warm1 warm2 warm3; do
    plan=$(psql -c "SET statement_timeout = '120s'; EXPLAIN (ANALYZE, BUFFERS, SETTINGS) $sql")
    ms=$(printf '%s\n' "$plan" | sed -n 's/^Execution Time: \([0-9.]*\) ms/\1/p')
    t+=("$ms")
    if [ "$run" = first ] || [ "$run" = warm3 ]; then { echo; echo "---- $run ----"; printf '%s\n' "$plan" | sed -E "s/'\\\\x[0-9a-f]{40}'/'<hash>'/g"; } >> "$OUT/$k.txt"; fi
  done
  rows=$(printf '%s\n' "$plan" | sed -n 's/^Limit .*actual time=[^ ]* rows=\([0-9]*\).*/\1/p' | head -1)
  node=$(printf '%s\n' "$plan" | grep -m1 -E '(Index|Bitmap|Seq) Scan|Sort' | sed -E 's/^[ ->]*//; s/  \(cost.*//' | cut -c1-70)
  bufs=$(printf '%s\n' "$plan" | grep -m1 'Buffers:' | sed 's/^ *//')
  echo "| $k | ${t[0]} | ${t[1]} | ${t[2]} | ${t[3]} | ${rows:-} | ${node} | ${bufs} |" >> "$OUT/SUMMARY.md"
  echo "done $k: first ${t[0]} ms, warm ${t[3]} ms"
done

# ---- Part 2: generic plans of the two-term search (bind parameters, as pgx sends them)
SQL2="SELECT $COLS FROM $T WHERE name ILIKE \$1 AND name ILIKE \$2 ORDER BY discovered_on DESC, id DESC LIMIT \$3"
{
  echo; echo "## Generic-plan check (prepared two-term search)"; echo
  echo "Six executions with common patterns, then the rare pair; then the same with plan_cache_mode = force_generic_plan."
  echo '```'
  psql <<EOF
SET statement_timeout = '120s';
PREPARE s2 AS $SQL2;
\\o /dev/null
\timing on
EXECUTE s2('%1080p%', '%x264%', 51);
EXECUTE s2('%720p%', '%x264%', 51);
EXECUTE s2('%1080p%', '%web%', 51);
EXECUTE s2('%mp4%', '%xxx%', 51);
EXECUTE s2('%1080p%', '%hevc%', 51);
EXECUTE s2('%720p%', '%hdtv%', 51);
\timing off
\\o
EXPLAIN (ANALYZE, BUFFERS) EXECUTE s2('%mp4%', '%flac%', 51);
SET plan_cache_mode = force_generic_plan;
PREPARE g2 AS $SQL2;
EXPLAIN (ANALYZE, BUFFERS) EXECUTE g2('%mp4%', '%flac%', 51);
EXPLAIN (ANALYZE, BUFFERS) EXECUTE g2('%mp3%', '%x265%', 51);
EOF
  echo '```'
} >> "$OUT/SUMMARY.md" 2>&1
echo "summary: $OUT/SUMMARY.md"
