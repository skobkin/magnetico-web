#!/bin/sh
# Verify, read-only and without credentials, that what is published is what was
# qualified.
#
#   ci/verify-published.sh QUALIFIED_DIGEST TAG
#
# For each release repository: the tag resolves to exactly QUALIFIED_DIGEST, and
# that index holds the linux/amd64 and linux/arm64 images and two attestation
# manifests (SBOM and provenance, one per platform). The workflow additionally
# runs ci/image-smoke.sh on the published digest.
set -eu

EXPECTED="${1:?usage: verify-published.sh QUALIFIED_DIGEST TAG}"
TAG="${2:-}"
here="$(dirname "$0")"
# shellcheck source=ci/release-repos.sh
. "$here/release-repos.sh"

fail() {
	echo "verify-published: $*" >&2
	exit 1
}

valid_release_tag "$TAG" || fail "refusing tag '$TAG'"
printf '%s' "$EXPECTED" | grep -Eq '^sha256:[0-9a-f]{64}$' || fail "QUALIFIED_DIGEST is not a sha256 digest"

for repo in $REPOS; do
	# shellcheck disable=SC2086
	got="$($CRANE digest "$repo:$TAG")" || fail "cannot resolve $repo:$TAG"
	[ "$got" = "$EXPECTED" ] || fail "$repo:$TAG is $got, the qualified digest is $EXPECTED"
	# shellcheck disable=SC2086
	manifest="$($CRANE manifest "$repo@$EXPECTED")" || fail "cannot read the manifest of $repo@$EXPECTED"
	for arch in amd64 arm64; do
		printf '%s' "$manifest" | grep -Eq "\"architecture\" *: *\"$arch\"" || fail "$repo@$EXPECTED has no linux/$arch image"
	done
	atts="$(printf '%s' "$manifest" | grep -o 'attestation-manifest' | wc -l | tr -d ' ')"
	[ "$atts" = 2 ] || fail "$repo@$EXPECTED has $atts attestation manifests, want 2"
	echo "verified $repo:$TAG -> $EXPECTED (linux/amd64, linux/arm64, 2 attestation manifests)"
done
