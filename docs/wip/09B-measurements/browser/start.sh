#!/bin/sh
set -eu
B=/tmp/mag-browser
export DSN="postgres://postgres@127.0.0.1:55440/browser?sslmode=disable"
export MAGNETICO_WEB_DATABASE="$DSN" MAGNETICO_WEB_PUBLIC_URL=http://127.0.0.1:18081
setsid nohup $B/magnetico ingest --database "$DSN" --listen 127.0.0.1:18080 > $B/ingest.log 2>&1 &
echo $! > $B/ingest.pid
setsid nohup $B/magnetico web --listen 127.0.0.1:18081 > $B/web.log 2>&1 &
echo $! > $B/web.pid
TOKEN=$(sed -n 's/^token: //p' $B/crawler-add.out)
export MAGNETICO_CRAWL_INGEST_TOKEN="$TOKEN"
rm -rf $B/queue; mkdir -p $B/queue
# three hours of synthetic crawling: 8 records/s for a while, then a quiet period, then a burst
setsid nohup $B/crawlerd -url http://127.0.0.1:18080 -queue-dir $B/queue -phases "8:2400,2:200,40:1200" -from 9000000 -precheck-rate 30 -precheck-count 20000 -hold > $B/crawlerd.log 2>&1 &
echo $! > $B/crawlerd.pid
sleep 3
curl -s -o /dev/null -w "web healthz %{http_code}\n" http://127.0.0.1:18081/healthz
curl -s -o /dev/null -w "ingest healthz %{http_code}\n" http://127.0.0.1:18080/healthz
