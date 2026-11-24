#!/usr/bin/env bash
# EXPLAIN (ANALYZE, BUFFERS, SETTINGS) of the search query shapes of
# persistence/webread.go on the W9 dataset (docs/wip/07J-measurements/README.md).
#
#   usage: explain.sh <container> <database> <out-dir>
#
# Per shape: one run right after a container restart (cold PostgreSQL shared
# buffers; the operating-system page cache is NOT cleared: no root here, so the
# runs are "cold buffers, OS-warm"), then three warm runs. Full plans are kept
# for the first and the last run; the timings of all four are summarised.
set -euo pipefail
CT=$1; DB=$2; OUT=$3
mkdir -p "$OUT"
psql() { docker exec -i "$CT" psql -U postgres -d "$DB" -X -q -A -t "$@"; }
restart() { docker restart "$CT" >/dev/null; until docker exec "$CT" pg_isready -q; do sleep 0.5; done; }

# Anchors: a row deep in each order, and a hash that exists.
DEEP=$(psql -c "SELECT discovered_on||','||id FROM magneticod.torrents ORDER BY discovered_on DESC, id DESC OFFSET 1500000 LIMIT 1")
DEEP_T=${DEEP%,*}; DEEP_I=${DEEP#*,}
DEEPS=$(psql -c "SELECT total_size||','||id FROM magneticod.torrents ORDER BY total_size DESC, id DESC OFFSET 1500000 LIMIT 1")
DEEPS_T=${DEEPS%,*}; DEEPS_I=${DEEPS#*,}
HASH=$(psql -c "SELECT encode(info_hash,'hex') FROM magneticod.torrents WHERE id = 1750000")
COLS="id, info_hash, name, total_size, discovered_on, file_count"

declare -A Q
Q[01-empty-newest]="SELECT $COLS FROM magneticod.torrents ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[02-empty-oldest]="SELECT $COLS FROM magneticod.torrents ORDER BY discovered_on ASC, id ASC LIMIT 51"
Q[03-empty-size-desc]="SELECT $COLS FROM magneticod.torrents ORDER BY total_size DESC, id DESC LIMIT 51"
Q[04-empty-size-asc]="SELECT $COLS FROM magneticod.torrents ORDER BY total_size ASC, id ASC LIMIT 51"
Q[05-deep-keyset-newest]="SELECT $COLS FROM magneticod.torrents WHERE (discovered_on, id) < ($DEEP_T, $DEEP_I) ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[06-deep-keyset-backward]="SELECT $COLS FROM magneticod.torrents WHERE (discovered_on, id) > ($DEEP_T, $DEEP_I) ORDER BY discovered_on ASC, id ASC LIMIT 51"
Q[07-deep-keyset-size]="SELECT $COLS FROM magneticod.torrents WHERE (total_size, id) < ($DEEPS_T, $DEEPS_I) ORDER BY total_size DESC, id DESC LIMIT 51"
Q[08-common-term-newest]="SELECT $COLS FROM magneticod.torrents WHERE name ILIKE '%1080p%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[09-common-two-terms]="SELECT $COLS FROM magneticod.torrents WHERE name ILIKE '%1080p%' AND name ILIKE '%x264%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[10-common-term-size]="SELECT $COLS FROM magneticod.torrents WHERE name ILIKE '%1080p%' ORDER BY total_size DESC, id DESC LIMIT 51"
Q[11-common-term-name]="SELECT $COLS FROM magneticod.torrents WHERE name ILIKE '%1080p%' ORDER BY name ASC, id ASC LIMIT 51"
Q[12-mid-term]="SELECT $COLS FROM magneticod.torrents WHERE name ILIKE '%m17%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[13-selective-term]="SELECT $COLS FROM magneticod.torrents WHERE name ILIKE '%w123456%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[14-selective-plus-common]="SELECT $COLS FROM magneticod.torrents WHERE name ILIKE '%w123456%' AND name ILIKE '%1080p%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[15-no-hit]="SELECT $COLS FROM magneticod.torrents WHERE name ILIKE '%zzqxj%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[16-unicode-term]="SELECT $COLS FROM magneticod.torrents WHERE name ILIKE '%日本語%' ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[17-common-term-deep-keyset]="SELECT $COLS FROM magneticod.torrents WHERE name ILIKE '%1080p%' AND (discovered_on, id) < ($DEEP_T, $DEEP_I) ORDER BY discovered_on DESC, id DESC LIMIT 51"
Q[18-infohash-lookup]="SELECT $COLS FROM magneticod.torrents WHERE info_hash = decode('$HASH','hex')"
Q[19-anchor-only-lookup]="SELECT discovered_on FROM magneticod.torrents WHERE id = 1750000"
Q[20-rss-max-offset]="SELECT $COLS FROM magneticod.torrents ORDER BY id DESC LIMIT 1000 OFFSET 9000"

{
  echo "# EXPLAIN (ANALYZE, BUFFERS, SETTINGS) summary"; echo
  echo "- container: $CT, database: $DB, taken: $(date -u +%FT%TZ)"
  psql -c "SELECT 'server: '||version()" ; psql -c "SELECT name||' = '||setting||COALESCE(unit,'') FROM pg_settings WHERE name IN ('shared_buffers','work_mem','effective_cache_size','random_page_cost','max_parallel_workers_per_gather','jit','fsync','synchronous_commit')"
  psql -c "SELECT 'torrents rows (estimate) '||reltuples::bigint||', table '||pg_size_pretty(pg_table_size('magneticod.torrents'))||', indexes '||pg_size_pretty(pg_indexes_size('magneticod.torrents')) FROM pg_class WHERE oid='magneticod.torrents'::regclass"
  psql -c "SELECT indexrelid::regclass||' '||pg_size_pretty(pg_relation_size(indexrelid)) FROM pg_index WHERE indrelid='magneticod.torrents'::regclass"
  echo; echo "| shape | cold (ms) | warm 1 | warm 2 | warm 3 | rows | buffers hit / read (last warm) |"; echo "|---|---|---|---|---|---|---|"
} > "$OUT/SUMMARY.md"

for k in $(printf '%s\n' "${!Q[@]}" | sort); do
  sql=${Q[$k]}
  restart
  t=()
  : > "$OUT/$k.txt"
  echo "$sql" >> "$OUT/$k.txt"
  for run in cold warm1 warm2 warm3; do
    plan=$(psql -c "EXPLAIN (ANALYZE, BUFFERS, SETTINGS) $sql")
    ms=$(printf '%s\n' "$plan" | sed -n 's/^Execution Time: \([0-9.]*\) ms/\1/p')
    t+=("$ms")
    if [ "$run" = cold ] || [ "$run" = warm3 ]; then { echo; echo "---- $run ----"; printf '%s\n' "$plan"; } >> "$OUT/$k.txt"; fi
  done
  rows=$(printf '%s\n' "$plan" | sed -n 's/^ *-> .*actual time=[^ ]* rows=\([0-9]*\).*/\1/p;s/^Limit .*actual time=[^ ]* rows=\([0-9]*\).*/\1/p' | head -1)
  bufs=$(printf '%s\n' "$plan" | grep -m1 'Buffers:' | sed 's/^ *//')
  echo "| $k | ${t[0]} | ${t[1]} | ${t[2]} | ${t[3]} | ${rows:-} | ${bufs} |" >> "$OUT/SUMMARY.md"
  echo "done $k: cold ${t[0]} ms, warm ${t[3]} ms"
done
