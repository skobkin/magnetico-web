#!/bin/sh
# Promote the qualified image to the release repositories.
#
#   ci/promote-image.sh OCI_LAYOUT_DIR TAG
#
# OCI_LAYOUT_DIR is the single build the release gates smoke-tested and scanned
# (an OCI image layout whose index.json names exactly one index: the multi-platform
# image with its attestations). It is pushed unchanged, so the digest published
# is the digest qualified; each repository must report exactly that digest.
# TAG is the release tag; only that one tag is ever applied.
#
# Credentials come from DOCKER_LOGIN/DOCKER_TOKEN and GHCR_LOGIN/GHCR_TOKEN in the
# environment (Woodpecker secrets of the tag-only publication step), are written
# to a private temporary DOCKER_CONFIG that is removed on exit, and are never
# printed or passed as arguments.
set -eu
umask 077

LAYOUT="${1:?usage: promote-image.sh OCI_LAYOUT_DIR TAG}"
TAG="${2:-}"
here="$(dirname "$0")"
# shellcheck source=ci/release-repos.sh
. "$here/release-repos.sh"

fail() {
	echo "promote-image: $*" >&2
	exit 1
}

valid_release_tag "$TAG" || fail "refusing tag '$TAG': only MAJOR.MINOR.PATCH or MAJOR.MINOR.PATCH-rc.N is ever applied (never latest, 0.8, stable or edge)"

[ -f "$LAYOUT/index.json" ] || fail "$LAYOUT is not an OCI image layout (no index.json)"
count="$(grep -Eo '"digest" *: *"sha256:[0-9a-f]{64}"' "$LAYOUT/index.json" | wc -l | tr -d ' ')"
[ "$count" = 1 ] || fail "the layout must name exactly one top-level index, found $count"
digest="$(grep -Eo 'sha256:[0-9a-f]{64}' "$LAYOUT/index.json" | head -n 1)"

cfg="$(mktemp -d)"
trap 'rm -rf "$cfg"' EXIT
export DOCKER_CONFIG="$cfg"
if [ "$REAL_REGISTRIES" = 1 ]; then
	: "${DOCKER_LOGIN:?DOCKER_LOGIN is required}" "${DOCKER_TOKEN:?DOCKER_TOKEN is required}"
	: "${GHCR_LOGIN:?GHCR_LOGIN is required}" "${GHCR_TOKEN:?GHCR_TOKEN is required}"
	dh="$(printf '%s:%s' "$DOCKER_LOGIN" "$DOCKER_TOKEN" | base64 | tr -d '\n')"
	gh="$(printf '%s:%s' "$GHCR_LOGIN" "$GHCR_TOKEN" | base64 | tr -d '\n')"
	printf '{"auths":{"https://index.docker.io/v1/":{"auth":"%s"},"ghcr.io":{"auth":"%s"}}}\n' "$dh" "$gh" >"$cfg/config.json"
else
	echo '{}' >"$cfg/config.json"
fi

for repo in $REPOS; do
	# shellcheck disable=SC2086
	out="$($CRANE push "$LAYOUT" "$repo:$TAG")" || fail "pushing to $repo failed"
	pushed="$(printf '%s\n' "$out" | sed -n 's/.*@\(sha256:[0-9a-f]\{64\}\)$/\1/p' | tail -n 1)"
	[ "$pushed" = "$digest" ] || fail "$repo stored digest ${pushed:-none}, but the qualified digest is $digest"
	echo "promoted $repo:$TAG -> $digest"
done
