#!/bin/sh
# Scan the published image of the current release for fixable HIGH and CRITICAL
# vulnerabilities, both platforms (.woodpecker/security.yaml, weekly). A finding fails the
# run: rebuild the image with fresh base images (rebuild.yaml, FORCE_REBUILD=true) when the
# fix is in a base-image package, or release a new version when it is in Go or a Go module.
#
#   ci/scan-release-image.sh TAG_FILE
#
# TAG_FILE holds the release tag (written by ci/release-tag.sh); an empty file means there
# is no Go-line release yet, and nothing is scanned. SCAN_IMAGE overrides the image name.
set -eu

tag_file="${1:?usage: scan-release-image.sh TAG_FILE}"
image="${SCAN_IMAGE:-docker.io/skobkin/magnetico-web}"
tag="$(cat "$tag_file")"
if [ -z "$tag" ]; then
	echo "scan-release-image: there is no Go-line release yet: nothing to scan"
	exit 0
fi

status=0
for platform in linux/amd64 linux/arm64; do
	echo "scan-release-image: $image:$tag ($platform)"
	trivy image --image-src remote --platform "$platform" --scanners vuln \
		--severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 --no-progress "$image:$tag" || status=1
done
if [ "$status" -ne 0 ]; then
	echo "scan-release-image: fixable vulnerabilities in $image:$tag: rebuild it (rebuild.yaml) or release a fix" >&2
fi
exit "$status"
