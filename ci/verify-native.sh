#!/bin/sh
# Verify, read-only, that the Forgejo release holds what was qualified.
#
#   ci/verify-native.sh TAG QUALIFIED_CHECKSUMS_FILE
#
# Reads CI_FORGE_URL and CI_REPO (owner/name). Checks: the release for TAG is
# published (not a draft); it is a prerelease exactly when TAG is a release
# candidate (MAJOR.MINOR.PATCH-rc.N) and a stable release otherwise; both Linux
# archives and checksums.txt are attached; and the published checksums.txt is
# byte-identical to the one the release gate qualified (the archives are
# reproducible, so equal checksums mean the published archives are the qualified
# ones).
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
echo "verified release $TAG ($kind): both archives and checksums.txt attached, checksums equal to the qualified build"
