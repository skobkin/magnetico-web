#!/bin/sh
# Verify, read-only, that the Forgejo release holds what was qualified.
#
#   ci/verify-native.sh TAG QUALIFIED_CHECKSUMS_FILE
#
# Reads CI_FORGE_URL and CI_REPO (owner/name). Checks: the release for TAG is
# published (not a draft); it is a prerelease exactly when TAG is a release
# candidate (MAJOR.MINOR.PATCH-rc.N) and a stable release otherwise; both Linux
# archives and checksums.txt are attached; the published checksums.txt is
# byte-identical to the one the release gate qualified; and each published
# archive is downloaded and its SHA-256 is compared with the qualified checksums
# (checksums.txt is only a list: equal lists say nothing about the attached bytes).
set -eu

TAG="${1:?usage: verify-native.sh TAG QUALIFIED_CHECKSUMS_FILE}"
QUALIFIED="${2:?usage: verify-native.sh TAG QUALIFIED_CHECKSUMS_FILE}"
here="$(dirname "$0")"
# shellcheck source=ci/release-repos.sh
. "$here/release-repos.sh"

fail() {
	echo "verify-native: $*" >&2
	exit 1
}

valid_release_tag "$TAG" || fail "refusing tag '$TAG'"
: "${CI_FORGE_URL:?CI_FORGE_URL is required}" "${CI_REPO:?CI_REPO is required}"
[ -f "$QUALIFIED" ] || fail "no qualified checksums file $QUALIFIED"
base="${CI_FORGE_URL%/}"

release="$(wget -q -O - "$base/api/v1/repos/$CI_REPO/releases/tags/$TAG")" || fail "cannot read the release $TAG from the forge"

if printf '%s' "$release" | grep -Eq '"draft" *: *true'; then fail "release $TAG is still a draft"; fi
case "$TAG" in
*-rc.*)
	printf '%s' "$release" | grep -Eq '"prerelease" *: *true' || fail "release $TAG is a release candidate but is not marked as a prerelease"
	kind="prerelease"
	;;
*)
	printf '%s' "$release" | grep -Eq '"prerelease" *: *false' || fail "release $TAG is a stable release but is marked as a prerelease"
	kind="stable release"
	;;
esac

for asset in "magnetico-web_${TAG}_linux_amd64.tar.gz" "magnetico-web_${TAG}_linux_arm64.tar.gz" "checksums.txt"; do
	pattern="$(printf '%s' "$asset" | sed 's/\./\\./g')"
	printf '%s' "$release" | grep -Eq "\"name\" *: *\"$pattern\"" || fail "release $TAG has no asset $asset"
done

published="$(wget -q -O - "$base/$CI_REPO/releases/download/$TAG/checksums.txt")" || fail "cannot download the published checksums.txt"
if [ "$published" != "$(cat "$QUALIFIED")" ]; then
	fail "the published checksums.txt differs from the qualified one: the published archives are not the qualified build"
fi

# The archives themselves: hash the downloaded bytes against the qualified list.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
for asset in "magnetico-web_${TAG}_linux_amd64.tar.gz" "magnetico-web_${TAG}_linux_arm64.tar.gz"; do
	want="$(awk -v n="$asset" '$2 == n { print $1 }' "$QUALIFIED")"
	printf '%s' "$want" | grep -Eq '^[0-9a-f]{64}$' || fail "the qualified checksums do not list $asset"
	wget -q -O "$tmp/$asset" "$base/$CI_REPO/releases/download/$TAG/$asset" || fail "cannot download the published $asset"
	got="$(sha256sum "$tmp/$asset" | awk '{ print $1 }')"
	[ "$got" = "$want" ] || fail "the published $asset differs from the qualified one (sha256 $got, qualified $want)"
	rm -f "$tmp/$asset"
done
echo "verified release $TAG ($kind): both archives and checksums.txt attached; the downloaded archives and checksums.txt equal the qualified build"
