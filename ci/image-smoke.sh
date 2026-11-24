#!/bin/sh
# Smoke test of a magnetico-web container image.
#
#   ci/image-smoke.sh [--platform linux/amd64] IMAGE_REF
#
# IMAGE_REF is a tag or an immutable repo@sha256:... reference. The release
# workflow runs this against the *staged digest* that is later promoted, and
# again against the published digest; it never builds anything itself.
#
# Needs: a docker CLI with a reachable daemon. For the web-service check it also
# starts a throwaway PostgreSQL container (PG_IMAGE, default postgres:17-alpine)
# on a private network; it is removed on exit. Exit status 0 only if every check
# passed.
set -u

PLATFORM=""
if [ "${1:-}" = "--platform" ]; then
	PLATFORM="$2"
	shift 2
fi
IMAGE="${1:?usage: image-smoke.sh [--platform P] IMAGE_REF}"
PG_IMAGE="${PG_IMAGE:-postgres:17-alpine}"
RUN_ID="smoke$$"
NET="$RUN_ID-net"
FAILS=0
PLAT_FLAG=""
[ -n "$PLATFORM" ] && PLAT_FLAG="--platform $PLATFORM"

pass() { printf 'ok    %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; FAILS=$((FAILS + 1)); }
check() { # check "description" command...
	desc="$1"
	shift
	if "$@" >/dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

# shellcheck disable=SC2086
mag() { docker run --rm $PLAT_FLAG --network none --read-only --cap-drop ALL --security-opt no-new-privileges "$IMAGE" "$@"; }
# shellcheck disable=SC2086
sh_in() { docker run --rm $PLAT_FLAG --network none --entrypoint /bin/sh "$IMAGE" -c "$1"; }

cleanup() {
	docker rm -fv "$RUN_ID-pg" "$RUN_ID-web" "$RUN_ID-ingest" >/dev/null 2>&1
	docker network rm "$NET" >/dev/null 2>&1
	docker volume rm "$RUN_ID-data" "$RUN_ID-data2" >/dev/null 2>&1
	if [ -n "${BINDDIR:-}" ]; then
		# the probe directory may be owned by another uid: hand it back, then remove it
		docker run --rm -v "$BINDDIR:/w" --user 0:0 --entrypoint /bin/sh "$IMAGE" -c "chown -R $(id -u):$(id -g) /w" >/dev/null 2>&1
		rm -rf "$BINDDIR"
	fi
}
trap cleanup EXIT

echo "== image metadata ($IMAGE)"
insp() { docker image inspect --format "$1" "$IMAGE"; }
[ "$(insp '{{.Config.User}}')" = "10001:10001" ] && pass "runs as 10001:10001" || fail "user is $(insp '{{.Config.User}}'), want 10001:10001"
[ "$(insp '{{json .Config.Entrypoint}}')" = '["/usr/local/bin/magnetico"]' ] && pass "exec-form entrypoint" || fail "entrypoint is $(insp '{{json .Config.Entrypoint}}')"
[ "$(insp '{{json .Config.Cmd}}')" = "null" ] && pass "no default command" || fail "a default command is set: $(insp '{{json .Config.Cmd}}')"
[ "$(insp '{{json .Config.Healthcheck}}')" = "null" ] && pass "no universal healthcheck" || fail "a healthcheck is declared"
[ "$(insp '{{json .Config.Volumes}}')" = "null" ] && pass "no VOLUME declared" || fail "a VOLUME is declared: $(insp '{{json .Config.Volumes}}')"
for l in title description source licenses version revision; do
	[ -n "$(insp "{{index .Config.Labels \"org.opencontainers.image.$l\"}}")" ] && pass "label $l" || fail "label org.opencontainers.image.$l is missing or empty"
done
if insp '{{range .Config.Env}}{{println .}}{{end}}' | grep -iE '(PASSWORD|TOKEN|SECRET|DSN|CREDENTIAL)[A-Z_]*='; then fail "an environment variable of the image looks like a secret"; else pass "no secret-like environment variable in the image"; fi
if docker history --no-trunc --format '{{.CreatedBy}}' "$IMAGE" | grep -iE '(password|token|secret|credential)'; then fail "image history mentions a secret-like word"; else pass "no secret-like word in the image history"; fi

echo "== runtime user and filesystem"
ID="$(docker run --rm $PLAT_FLAG --network none --entrypoint /usr/bin/id "$IMAGE" 2>&1)"
case "$ID" in *"uid=10001"*"gid=10001"*) pass "id: $ID" ;; *) fail "id reports: $ID" ;; esac
check "no Go toolchain or source tree in the image" sh_in '! command -v go && ! test -e /usr/local/go && ! test -e /src && ! test -e /go'
check "system CA bundle present" sh_in 'test -s /etc/ssl/certs/ca-certificates.crt'
check "/data and /reports exist and belong to 10001:10001" sh_in '[ "$(stat -c %u:%g /data)" = 10001:10001 ] && [ "$(stat -c %u:%g /reports)" = 10001:10001 ]'
check "licence files present" sh_in 'test -s /usr/share/licenses/magnetico/COPYING && test -s /usr/share/licenses/magnetico/swagger-ui-LICENSE && test -s /usr/share/licenses/magnetico/swagger-ui-NOTICE'
check "IANA time zone data is embedded in the executable (no zoneinfo in the image)" sh_in '! test -d /usr/share/zoneinfo && grep -q Europe/Moscow /usr/local/bin/magnetico'
check "Swagger UI bundle is embedded in the executable" sh_in 'grep -q SwaggerUIBundle /usr/local/bin/magnetico'
check "executable is not writable by the runtime user" sh_in '! test -w /usr/local/bin/magnetico'

echo "== executable behaviour (no network, read-only root, all capabilities dropped)"
VOUT="$(mag --version 2>&1)"
VRC=$?
if [ $VRC -eq 0 ] && printf '%s\n' "$VOUT" | grep -qE '^magnetico [^ ]+ \(commit [0-9a-f]{12}(\+dirty)?|unknown, go[0-9.]+, linux/[a-z0-9]+\)$' && [ "$(printf '%s\n' "$VOUT" | wc -l)" -eq 1 ]; then
	pass "--version: $VOUT"
	GOV="$(printf '%s' "$VOUT" | sed -E 's/.*, go([0-9]+\.[0-9]+(\.[0-9]+)?),.*/\1/')"
	# security floor: Go 1.27.4 or newer
	if [ "$(printf '1.27.4\n%s\n' "$GOV" | sort -V | head -n 1)" = "1.27.4" ]; then pass "built with Go $GOV (>= 1.27.4)"; else fail "built with Go $GOV, below the 1.27.4 floor"; fi
else
	fail "--version failed (exit $VRC): $VOUT"
fi
docker volume create "$RUN_ID-data" >/dev/null
# shellcheck disable=SC2086
NOARGS="$(docker run --rm $PLAT_FLAG --network none -v "$RUN_ID-data:/data" "$IMAGE" 2>&1)"
NRC=$?
if [ $NRC -eq 1 ] && printf '%s' "$NOARGS" | grep -q 'Please specify one command'; then pass "no arguments: usage and exit 1"; else fail "no arguments: exit $NRC: $NOARGS"; fi
# shellcheck disable=SC2086
if [ -z "$(docker run --rm $PLAT_FLAG --network none -v "$RUN_ID-data:/data" --entrypoint /bin/ls "$IMAGE" -A /data)" ]; then pass "no arguments: nothing was created in /data"; else fail "no arguments created files in /data"; fi
for cmd in "web --help" "ingest --help" "crawl --help" "migrate --help" "dev --help"; do
	# shellcheck disable=SC2086
	check "magnetico $cmd exits 0" mag $cmd
done

echo "== writable /data for the runtime user"
# shellcheck disable=SC2086
if docker run --rm $PLAT_FLAG --network none -v "$RUN_ID-data:/data" --entrypoint /bin/sh "$IMAGE" -c 'touch /data/probe && rm /data/probe'; then pass "fresh named volume at /data is writable"; else fail "named volume at /data is not writable"; fi
BINDDIR="$(mktemp -d)"
# shellcheck disable=SC2086
docker run --rm -v "$BINDDIR:/w" --user 0:0 --entrypoint /bin/sh "$IMAGE" -c 'chown 10001:10001 /w' >/dev/null 2>&1
# shellcheck disable=SC2086
if docker run --rm $PLAT_FLAG --network none -v "$BINDDIR:/data" --entrypoint /bin/sh "$IMAGE" -c 'touch /data/probe && rm /data/probe'; then pass "bind mount owned by 10001:10001 is writable"; else fail "bind mount owned by 10001:10001 is not writable"; fi
# shellcheck disable=SC2086
docker run --rm -v "$BINDDIR:/w" --user 0:0 --entrypoint /bin/sh "$IMAGE" -c 'chown 0:0 /w && chmod 755 /w' >/dev/null 2>&1
# shellcheck disable=SC2086
if docker run --rm $PLAT_FLAG --network none -v "$BINDDIR:/data" --entrypoint /bin/sh "$IMAGE" -c 'touch /data/probe'; then fail "a root-owned bind mount was writable (the chown requirement would not be real)"; else pass "a root-owned bind mount is refused (chown 10001:10001 is required, as documented)"; fi

echo "== web service against a throwaway PostgreSQL"
docker network create "$NET" >/dev/null
if docker run -d --name "$RUN_ID-pg" --network "$NET" -e POSTGRES_HOST_AUTH_METHOD=trust "$PG_IMAGE" >/dev/null 2>&1; then
	i=0
	until docker exec "$RUN_ID-pg" pg_isready -U postgres >/dev/null 2>&1 || [ $i -ge 60 ]; do i=$((i + 1)); sleep 1; done
	DSN="postgres://postgres@$RUN_ID-pg:5432/postgres?sslmode=disable"
	# shellcheck disable=SC2086
	svc() { docker run --rm $PLAT_FLAG --network "$NET" -e "MAGNETICO_MIGRATE_DATABASE=$DSN" "$IMAGE" "$@"; }
	if svc migrate schema-init >/dev/null 2>&1 && svc migrate web-schema-init >/dev/null 2>&1; then
		pass "migrate schema-init and web-schema-init succeeded in the container"
		# shellcheck disable=SC2086
		docker run -d --name "$RUN_ID-web" $PLAT_FLAG --network "$NET" --read-only --cap-drop ALL --security-opt no-new-privileges \
			-e "MAGNETICO_WEB_DATABASE=$DSN" -e MAGNETICO_WEB_LISTEN=0.0.0.0:8080 -e MAGNETICO_WEB_PUBLIC_URL=https://magnet.example.org \
			"$IMAGE" web >/dev/null
		i=0
		until docker exec "$RUN_ID-web" wget -q -O /dev/null http://127.0.0.1:8080/readyz >/dev/null 2>&1 || [ $i -ge 30 ]; do i=$((i + 1)); sleep 1; done
		check "web /healthz answers" docker exec "$RUN_ID-web" wget -q -O /dev/null http://127.0.0.1:8080/healthz
		check "web /readyz answers (database reachable)" docker exec "$RUN_ID-web" wget -q -O /dev/null http://127.0.0.1:8080/readyz
		ASSETS="$(docker exec "$RUN_ID-web" wget -q -O - http://127.0.0.1:8080/login 2>/dev/null | grep -oE '/static/[A-Za-z0-9._-]+' | sort -u)"
		if [ -n "$ASSETS" ]; then
			ok=1
			for a in $ASSETS; do docker exec "$RUN_ID-web" wget -q -O /dev/null "http://127.0.0.1:8080$a" || ok=0; done
			[ $ok -eq 1 ] && pass "embedded static assets are served ($(echo "$ASSETS" | wc -l | tr -d ' '))" || fail "an embedded static asset is not served"
		else
			fail "the sign-in page references no static assets"
		fi
		docker stop -t 20 "$RUN_ID-web" >/dev/null 2>&1
		WEXIT="$(docker inspect --format '{{.State.ExitCode}}' "$RUN_ID-web" 2>/dev/null)"
		[ "$WEXIT" = "0" ] && pass "SIGTERM: web exits 0 (graceful shutdown)" || fail "web exited $WEXIT after docker stop"
		if svc migrate ingest-schema-init >/dev/null 2>&1; then
			TOK="$(head -c 48 /dev/urandom | base64 | tr -d '\n=+/' | head -c 48)"
			# shellcheck disable=SC2086
			docker run -d --name "$RUN_ID-ingest" $PLAT_FLAG --network "$NET" --read-only --cap-drop ALL --security-opt no-new-privileges \
				-e "MAGNETICO_INGEST_DATABASE=$DSN" -e "MAGNETICO_INGEST_TOKEN=$TOK" -e MAGNETICO_INGEST_LISTEN=0.0.0.0:8080 "$IMAGE" ingest >/dev/null
			sleep 4
			if [ "$(docker inspect --format '{{.State.Running}}' "$RUN_ID-ingest" 2>/dev/null)" = "true" ] && ! docker logs "$RUN_ID-ingest" 2>&1 | grep -qi 'unknown environment variable'; then
				pass "ingest starts with the image's default environment (MAGNETICO_CRAWL_QUEUE_DIR is tolerated)"
			else
				fail "ingest did not stay up: $(docker logs "$RUN_ID-ingest" 2>&1 | tail -2)"
			fi
			docker stop -t 20 "$RUN_ID-ingest" >/dev/null 2>&1
			[ "$(docker inspect --format '{{.State.ExitCode}}' "$RUN_ID-ingest" 2>/dev/null)" = "0" ] && pass "SIGTERM: ingest exits 0 (graceful shutdown)" || fail "ingest exited $(docker inspect --format '{{.State.ExitCode}}' "$RUN_ID-ingest" 2>/dev/null) after docker stop"
		else
			fail "migrate ingest-schema-init failed in the container"
		fi
	else
		fail "migrate schema-init / web-schema-init failed in the container"
	fi
else
	fail "could not start the throwaway PostgreSQL ($PG_IMAGE)"
fi

echo
if [ $FAILS -eq 0 ]; then echo "image smoke test: all checks passed ($IMAGE)"; else echo "image smoke test: $FAILS check(s) FAILED ($IMAGE)"; fi
[ $FAILS -eq 0 ]
