#!/bin/bash
cd /home/herdr/src/magnetico-web
export TMPDIR=/home/herdr/w4bench/tmp GOTMPDIR=/home/herdr/w4bench/tmp
export MAGNETICO_BENCH_LARGE=1 MAGNETICO_LOAD_DSN='postgres://postgres:benchpw@127.0.0.1:55432/bench'
export MAGNETICO_LOAD_DURATION=60s
OUT=/home/herdr/w4bench/out
run() { # label scenarios [env...]
  label=$1; sc=$2; shift 2
  echo "=== $label $(date +%T)" >> $OUT/campaign.log
  env "$@" MAGNETICO_LOAD_LABEL=$label MAGNETICO_LOAD_SCENARIOS=$sc MAGNETICO_LOAD_OUT=$OUT/campaign.jsonl go test -count=1 -run TestWebLoad -timeout 1h . >> $OUT/campaign.log 2>&1
}
run nocg cold,warm,conc8,conc32,mixed32,cancel
for lim in "" 640MiB 768MiB 896MiB; do
  run "cg1g-gml${lim:-unset}" conc32,mixed32,cancel MAGNETICO_LOAD_CGROUP_MEM=1G MAGNETICO_LOAD_CPU_PERCENT=200 MAGNETICO_LOAD_GOMEMLIMIT=$lim
done
echo "=== done $(date +%T)" >> $OUT/campaign.log
