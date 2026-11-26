#!/usr/bin/env bash
# Which credentials reach Traefik's logs (RC0 follow-up, finding D3).
#
#   traefik-log-check.sh OUTDIR
#
# Starts a throwaway Traefik (the RC0 deployment's image, plain HTTP on a random
# 127.0.0.1 port, attached to the rc0-edge network) once per access-log configuration
# below, sends the same probe requests through it to the RC0 web and ingest services, and
# counts the probe markers in the access log and in Traefik's own log. The probes are fake
# credentials (unique markers), so every request fails authentication: the check is about
# what the proxy writes, not about the application's answer. Needs the RC0 deployment.
#
# A configuration only counts when every probe got an HTTP answer from the backend and the
# access log holds one line per request sent; otherwise it is reported INVALID and the
# script exits non-zero, so a broken run can never read as "no leak". The keep-all
# configuration is the positive control: it must show every marker.
#
# The script owns exactly the containers it created: each run uses a unique name, docker
# writes the container id to a fresh --cidfile, and cleanup removes only that id.
set -euo pipefail
out=${1:?OUTDIR}
mkdir -p "$out"
img=traefik:v3.8.0@sha256:00c9734983e74ae35a1fc18eaee9e9da13f3dde95c4e45b3b8c3b60e0dbd2d17
work=$(mktemp -d "${TMPDIR:-/tmp}/tlog.XXXXXX")
cid=""
cleanup() {
  if [ -n "$cid" ]; then docker rm -f "$cid" >/dev/null 2>&1 || true; fi
  rm -rf "$work"
}
trap cleanup EXIT
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
nprobes=15
all_markers=$(for i in $(seq -w 1 $nprobes); do printf 'PROBE%s ' "$i"; done)

port=""
# Runs in $(...) subshells, so it cannot count requests itself; the caller counts them.
probe() { # host method path [curl args...]; prints the status code, 000 on a transport failure
  local host=$1 method=$2 path=$3; shift 3
  curl -s -o /dev/null -w '%{http_code}' --connect-timeout 3 --max-time 15 \
    -X "$method" -H "Host: $host" "$@" "http://127.0.0.1:$port$path" || true
}

{
  printf 'traefik %s\n' "$img"
  printf '%-26s %-5s %-8s %6s %8s %8s  %s\n' config level valid lines access traefik "markers found (PROBE01..PROBE$nprobes)"
} >"$out/summary.txt"
invalid=0
for c in "${configs[@]}"; do
  IFS='|' read -r cname level flags <<<"$c"
  mkdir -p "$work/logs" && rm -f "$work/logs/"*
  chmod 777 "$work/logs"
  name="rc0-tlog-$(date +%s)-$$-$RANDOM"
  rm -f "$work/cid"
  # shellcheck disable=SC2086
  if ! docker run -d --cidfile "$work/cid" --name "$name" --network rc0-edge -p 127.0.0.1::8080 \
    --read-only --cap-drop ALL \
    -v "$work/dynamic.yml:/etc/traefik/dynamic.yml:ro" -v "$work/logs:/logs" "$img" \
    --entrypoints.web.address=:8080 --providers.file.filename=/etc/traefik/dynamic.yml \
    --log.level="$level" --accesslog.filepath=/logs/access.log $flags \
    --global.checknewversion=false --global.sendanonymoususage=false >/dev/null; then
    # docker writes the cidfile as soon as the container exists, also when it fails to start
    [ -s "$work/cid" ] && cid=$(cat "$work/cid")
    echo "docker run failed for $cname" >&2
    exit 1
  fi
  cid=$(cat "$work/cid")
  port=$(docker port "$cid" 8080/tcp | head -1 | sed 's/.*://')
  # sent: the requests Traefik answered, each of which must produce one access-log line
  # (a readiness poll refused before Traefik listens never reaches it)
  sent=0
  ready=""
  for _ in $(seq 1 30); do
    code=$(probe magnet.rc0.test GET /healthz)
    [ "$code" = 000 ] || sent=$((sent + 1))
    if [ "$code" = 200 ]; then ready=1; break; fi
    sleep 0.5
  done
  reasons=()
  [ -n "$ready" ] || reasons+=("not-ready")
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
    # a page reached from a reset link: the browser's Referer carries the link
    "$(probe magnet.rc0.test GET /torrents -H 'Referer: https://magnet.rc0.test/password/reset/PROBE14x')"
    # a client-supplied request id (the service ignores it and sends its own X-Request-ID)
    "$(probe magnet.rc0.test GET /torrents -H 'X-Request-Id: PROBE15x')"
  )
  [ "${#codes[@]}" -eq "$nprobes" ] || reasons+=("probe-count")
  sent=$((sent + ${#codes[@]}))
  for code in "${codes[@]}"; do
    case $code in
      000) reasons+=("no-answer") ;;
      502 | 503 | 504) reasons+=("backend-$code") ;;
    esac
  done
  # the access log must hold one line per request sent (readiness polls included)
  lines=0
  for _ in $(seq 1 20); do
    lines=$(wc -l <"$work/logs/access.log" 2>/dev/null || echo 0)
    [ "$lines" -ge "$sent" ] && break
    sleep 0.5
  done
  [ "$lines" -eq "$sent" ] || reasons+=("log-lines-$lines-of-$sent")
  docker logs "$cid" >"$work/logs/traefik.log" 2>&1
  docker rm -f "$cid" >/dev/null
  cid=""
  a=$(grep -o 'PROBE[0-9]*' "$work/logs/access.log" | sort -u | tr '\n' ' ' || true)
  t=$(grep -o 'PROBE[0-9]*' "$work/logs/traefik.log" | sort -u | tr '\n' ' ' || true)
  na=$(grep -c 'PROBE' "$work/logs/access.log" || true)
  nt=$(grep -c 'PROBE' "$work/logs/traefik.log" || true)
  if [ "$cname" = keep-all ] && [ "$a" != "$all_markers" ]; then
    reasons+=("control-missed-markers")
  fi
  # sort -u keeps each reason once; an empty list means the run is valid
  valid=ok
  if [ "${#reasons[@]}" -gt 0 ]; then
    valid="INVALID($(printf '%s\n' "${reasons[@]}" | sort -u | paste -sd, -))"
    invalid=1
  fi
  printf '%-26s %-5s %-8s %6s %8s %8s  access: %s| traefik: %s| http %s\n' "$cname" "$level" "$valid" "$lines/$sent" "$na" "$nt" \
    "${a:-none }" "${t:-none }" "${codes[*]}" >>"$out/summary.txt"
  if head -1 "$work/logs/access.log" | grep -q '^{'; then
    # every field and header name that occurs in any access-log line (JSON formats), never values
    python3 -c 'import json,sys; print(" ".join(sorted({k for l in sys.stdin if l.strip() for k in json.loads(l)})))' \
      <"$work/logs/access.log" >"$out/fields-$cname.txt"
  else
    # the CLF line with every probe marker replaced, to show where the request line sits
    head -1 "$work/logs/access.log" | sed -E 's/PROBE[0-9]+x/<probe>/g' >"$out/line-$cname.txt"
  fi
  # lines with probe markers only: the markers are fake credentials and can be kept as
  # evidence, but a kept Set-Cookie header carries a real cookie the service issued
  # (the anonymous pre-session), so every Set-Cookie value is replaced
  grep -h 'PROBE' "$work/logs/access.log" "$work/logs/traefik.log" | head -40 |
    sed -E 's/(Set-Cookie":"[^=";]+=)[^;"]*/\1<redacted>/g' >"$out/hits-$cname.txt" || true
done
cat "$out/summary.txt"
if [ "$invalid" -ne 0 ]; then
  echo "at least one configuration is INVALID: its marker counts prove nothing" >&2
  exit 1
fi
