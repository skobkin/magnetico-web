#!/usr/bin/env bash
# Which credentials reach Traefik's logs (RC0 follow-up, finding D3).
#
#   traefik-log-check.sh OUTDIR
#
# Starts a throwaway Traefik (the RC0 deployment's image, plain HTTP on 127.0.0.1:18090,
# attached to the rc0-edge network) once per access-log configuration below, sends the
# same probe requests through it to the RC0 web and ingest services, and counts the probe
# markers in the access log and in Traefik's own log. The probes are fake credentials
# (unique markers), so every request fails authentication: the check is about what the
# proxy writes, not about the application's answer. Needs the RC0 deployment running.
set -euo pipefail
out=${1:?OUTDIR}
mkdir -p "$out"
img=traefik:v3.8.0@sha256:00c9734983e74ae35a1fc18eaee9e9da13f3dde95c4e45b3b8c3b60e0dbd2d17
name=rc0-tlog
work=$(mktemp -d "${TMPDIR:-/tmp}/tlog.XXXXXX")
trap 'docker rm -f $name >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
cat >"$work/dynamic.yml" <<'EOF'
http:
  routers:
    web: { rule: "Host(`magnet.rc0.test`)", entryPoints: [web], service: web }
    ingest: { rule: "Host(`ingest.rc0.test`) && PathPrefix(`/api/ingest/v1`)", entryPoints: [web], service: ingest }
  services:
    web: { loadBalancer: { servers: [ { url: "http://rc0-web:8081" } ] } }
    ingest: { loadBalancer: { servers: [ { url: "http://rc0-ingest:8080" } ] } }
EOF

# name | log level | access-log flags
configs=(
  "keep-all|INFO|--accesslog.format=json --accesslog.fields.defaultmode=keep --accesslog.fields.headers.defaultmode=keep"
  "recommended|INFO|--accesslog.format=json --accesslog.fields.defaultmode=keep --accesslog.fields.names.RequestPath=drop --accesslog.fields.headers.defaultmode=drop"
  "recommended-request-id|INFO|--accesslog.format=json --accesslog.fields.defaultmode=keep --accesslog.fields.names.RequestPath=drop --accesslog.fields.headers.defaultmode=drop --accesslog.fields.headers.names.X-Request-Id=keep"
  "recommended-debug|DEBUG|--accesslog.format=json --accesslog.fields.defaultmode=keep --accesslog.fields.names.RequestPath=drop --accesslog.fields.headers.defaultmode=drop"
  "path-redact|INFO|--accesslog.format=json --accesslog.fields.defaultmode=keep --accesslog.fields.names.RequestPath=redact --accesslog.fields.headers.defaultmode=drop"
  "path-drop-headers-redact|INFO|--accesslog.format=json --accesslog.fields.defaultmode=keep --accesslog.fields.names.RequestPath=drop --accesslog.fields.headers.defaultmode=keep --accesslog.fields.headers.names.Authorization=redact --accesslog.fields.headers.names.Cookie=redact --accesslog.fields.headers.names.Api-Token=redact"
  "common-path-drop|INFO|--accesslog.format=common --accesslog.fields.defaultmode=keep --accesslog.fields.names.RequestPath=drop --accesslog.fields.headers.defaultmode=drop"
  "genericclf-path-drop|INFO|--accesslog.format=genericCLF --accesslog.fields.defaultmode=keep --accesslog.fields.names.RequestPath=drop --accesslog.fields.headers.defaultmode=drop"
  "default-format|INFO|--accesslog=true"
)

probe() { # host method path [curl args...]
  local host=$1 method=$2 path=$3; shift 3
  curl -s -o /dev/null -w '%{http_code}' -X "$method" -H "Host: $host" "$@" "http://127.0.0.1:18090$path"
}

{
  printf 'traefik %s\n' "$img"
  printf '%-26s %-5s %8s %8s  %s\n' config level access traefik "markers found (P1..P13)"
} >"$out/summary.txt"
for c in "${configs[@]}"; do
  IFS='|' read -r cname level flags <<<"$c"
  mkdir -p "$work/logs" && rm -f "$work/logs/"*
  chmod 777 "$work/logs"
  # shellcheck disable=SC2086
  docker run -d --name $name --network rc0-edge -p 127.0.0.1:18090:8080 --read-only --cap-drop ALL \
    -v "$work/dynamic.yml:/etc/traefik/dynamic.yml:ro" -v "$work/logs:/logs" "$img" \
    --entrypoints.web.address=:8080 --providers.file.filename=/etc/traefik/dynamic.yml \
    --log.level="$level" --accesslog.filepath=/logs/access.log $flags \
    --global.checknewversion=false --global.sendanonymoususage=false >/dev/null
  for _ in $(seq 1 30); do probe magnet.rc0.test GET /healthz >/dev/null 2>&1 && break; sleep 0.5; done
  codes=(
    "$(probe magnet.rc0.test GET '/api/v2/feeds/latest.rss?feed_token=mgf_PROBE01x')"
    "$(probe magnet.rc0.test GET '/api/v1/feed/rss/last?api-token=PROBE02x')"
    "$(probe magnet.rc0.test GET /api/v1/feed/rss/last -H 'api-token: PROBE03x')"
    "$(probe magnet.rc0.test GET /api/v1/feed/rss/last -H 'Cookie: api-token=PROBE04x')"
    "$(probe magnet.rc0.test GET /register/PROBE05x)"
    "$(probe magnet.rc0.test GET /password/reset/PROBE06x)"
    "$(probe magnet.rc0.test GET /password/setup/PROBE07x)"
    "$(probe magnet.rc0.test GET /email/verify/PROBE08x)"
    "$(probe magnet.rc0.test POST /email/verify/PROBE09x -d csrf_token=x)"
    "$(probe ingest.rc0.test POST /api/ingest/v1/torrents/exists -H 'Authorization: Bearer PROBE10x' -H 'Content-Type: application/json' -d '{"info_hashes":[]}')"
    "$(probe magnet.rc0.test GET /api/v2/torrents -H 'Authorization: Bearer mgt_PROBE11x')"
    "$(probe magnet.rc0.test GET /torrents -H 'Cookie: __Host-magnetico_session=PROBE12x')"
    "$(probe magnet.rc0.test GET '/torrents?q=PROBE13x')"
  )
  sleep 2
  docker logs $name >"$work/logs/traefik.log" 2>&1
  docker rm -f $name >/dev/null
  a=$(grep -o 'PROBE[0-9]*' "$work/logs/access.log" | sort -u | tr '\n' ' ' || true)
  t=$(grep -o 'PROBE[0-9]*' "$work/logs/traefik.log" | sort -u | tr '\n' ' ' || true)
  na=$(grep -c 'PROBE' "$work/logs/access.log" || true)
  nt=$(grep -c 'PROBE' "$work/logs/traefik.log" || true)
  printf '%-26s %-5s %8s %8s  access: %s| traefik: %s| http %s\n' "$cname" "$level" "$na" "$nt" "${a:-none }" "${t:-none }" "${codes[*]}" >>"$out/summary.txt"
  # the field and header names of one access-log line (JSON formats), never values
  if head -1 "$work/logs/access.log" | grep -q '^{'; then
    head -1 "$work/logs/access.log" | python3 -c 'import json,sys; print(" ".join(sorted(json.load(sys.stdin))))' >"$out/fields-$cname.txt"
  else
    # the CLF line with every probe marker replaced, to show where the request line sits
    head -1 "$work/logs/access.log" | sed -E 's/PROBE[0-9]+x/<probe>/g' >"$out/line-$cname.txt"
  fi
  # probe markers only: these are fake credentials, so the lines can be kept as evidence
  grep -h 'PROBE' "$work/logs/access.log" "$work/logs/traefik.log" | head -40 >"$out/hits-$cname.txt" || true
done
cat "$out/summary.txt"
