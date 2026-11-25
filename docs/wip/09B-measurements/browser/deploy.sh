#!/bin/sh
# Disposable loopback deployment for the browser check (task 09B). Everything is synthetic.
set -eu
B=/tmp/mag-browser
PGPORT=55440
docker rm -f m-pg-browser >/dev/null 2>&1 || true
rm -rf /home/herdr/qual/pgbrowser 2>/dev/null || docker run --rm -v /home/herdr/qual:/q alpine rm -rf /q/pgbrowser
mkdir -p /home/herdr/qual/pgbrowser
docker run -d --name m-pg-browser -e POSTGRES_HOST_AUTH_METHOD=trust -p 127.0.0.1:$PGPORT:5432 \
  -v /home/herdr/qual/pgbrowser:/var/lib/postgresql/data postgres:17-alpine >/dev/null
n=0; until docker exec m-pg-browser pg_isready -U postgres >/dev/null 2>&1; do n=$((n+1)); [ $n -lt 60 ] || exit 1; sleep 1; done
sleep 2
docker exec m-pg-browser psql -U postgres -c "CREATE DATABASE browser" >/dev/null
export DSN="postgres://postgres@127.0.0.1:$PGPORT/browser?sslmode=disable"
export MAGNETICO_MIGRATE_DATABASE="$DSN" MAGNETICO_DEV_DATABASE="$DSN" MAGNETICO_WEB_DATABASE="$DSN"
$B/magnetico migrate schema-init
$B/magnetico migrate web-schema-init
$B/magnetico migrate ingest-schema-init
$B/magnetico migrate stats-schema-init
$B/magnetico dev seed-torrents --confirm-development-database
export MAGNETICO_WEB_PUBLIC_URL=http://127.0.0.1:18081
$B/magnetico web user add browser-admin --email admin@example.org > $B/user-add.out 2>&1
$B/magnetico web user set-role browser-admin admin
export MAGNETICO_INGEST_DATABASE="$DSN"
$B/magnetico ingest --database "$DSN" crawler add browser-crawler > $B/crawler-add.out 2>&1
echo deployed
