#!/bin/sh
# Prepare the forced rebuild of a published release image (.woodpecker/rebuild.yaml; ported
# from the owner's phanpy-docker pipeline). The rebuild builds the image of an existing
# release again, from the same source, with freshly pulled base images and no cache: it
# picks up security fixes of the base images (Alpine packages) without a new release. A Go
# or Go-dependency fix is a code change and needs a new release instead.
#
#   - refuses unless FORCE_REBUILD is true or 1 (a variable of the manual run);
#   - targets REBUILD_TAG, by default the newest stable Go-line release (ci/release-tag.sh);
#   - refuses a tag that is not a Go-line release tag, a rebuild tag (X.Y.Z-rN), or a
#     release that neither registry has (that is published by release.yaml, not rebuilt);
#   - numbers the rebuild with the next -rN that neither registry has for that tag;
#   - writes the image tags (X.Y.Z-rN, and X.Y.Z, which moves to the rebuild: owner
#     decision; plus latest when latest references this release in both registries, so a
#     rebuild of an older release never moves latest backwards) and the build arguments
#     for the buildx plugin, then checks the tag out.
#
# Registry access is anonymous (both images are public). Overridable for tests:
# DOCKERHUB_REPO, GHCR_REPO, DOCKERHUB_REGISTRY, DOCKERHUB_AUTH, GHCR_REGISTRY,
# TAGS_FILE (.tags), ENV_FILE (.rebuild.env).
set -eu

die() {
	echo "prepare-rebuild: $*" >&2
	exit 1
}

case "${FORCE_REBUILD:-}" in
true | 1) ;;
*) die "FORCE_REBUILD is not set to true: nothing is rebuilt" ;;
esac

hub_repo="${DOCKERHUB_REPO:-skobkin/magnetico-web}"
ghcr_repo="${GHCR_REPO:-skobkin/magnetico-web}"
hub_registry="${DOCKERHUB_REGISTRY:-https://registry-1.docker.io}"
hub_auth="${DOCKERHUB_AUTH:-https://auth.docker.io/token}"
ghcr_registry="${GHCR_REGISTRY:-https://ghcr.io}"
tags_file="${TAGS_FILE:-.tags}"
env_file="${ENV_FILE:-.rebuild.env}"

tag="${REBUILD_TAG:-}"
if [ -z "$tag" ]; then
	tag="$(sh ci/release-tag.sh)"
	[ -n "$tag" ] || die "there is no Go-line release tag: set REBUILD_TAG"
fi
# A release tag that is also a valid image tag (the Docker tag alphabet).
printf '%s\n' "$tag" | grep -Eq '^[0-9]+[.][0-9]+[.][0-9]+[A-Za-z0-9_.-]*$' ||
	die "$tag is not a release tag (MAJOR.MINOR.PATCH, optionally a suffix of [A-Za-z0-9_.-])"
if printf '%s\n' "$tag" | grep -Eq -- '-r[0-9]+$'; then
	die "$tag is a rebuild tag: name the release it rebuilds"
fi
commit="$(git rev-parse -q --verify "refs/tags/$tag^{commit}")" || die "there is no git tag $tag"
git cat-file -e "refs/tags/$tag:cmd/magnetico" 2>/dev/null || die "$tag is not a release of the Go line"

# curl's output is taken first: in a pipeline into jq its failure would be lost (POSIX sh
# has no pipefail), and a registry that cannot be read would look like one without tags.
token() {
	body="$(curl -fsS "$1")" || return 1
	printf '%s' "$body" | jq -r '.token // empty'
}
tags_of() { # registry, repository, token
	body="$(curl -fsS -H "Authorization: Bearer $3" "$1/v2/$2/tags/list?n=10000")" || return 1
	printf '%s' "$body" | jq -r '.tags // [] | .[]'
}
# The digest of a tag's manifest; empty output when the registry has no such tag (404).
manifest_types='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
digest_of() { # registry, repository, token, tag
	headers="$(curl -sSI -H "Authorization: Bearer $3" -H "Accept: $manifest_types" "$1/v2/$2/manifests/$4")" || return 1
	headers="$(printf '%s\n' "$headers" | tr -d '\r')"
	case "$(printf '%s\n' "$headers" | awk 'NR == 1 { print $2 }')" in
	200)
		d="$(printf '%s\n' "$headers" | awk 'tolower($1) == "docker-content-digest:" { print $2 }')"
		[ -n "$d" ] || return 1
		printf '%s\n' "$d"
		;;
	404) ;;
	*) return 1 ;;
	esac
}

hub_token="$(token "$hub_auth?service=registry.docker.io&scope=repository:$hub_repo:pull")" || die "no Docker Hub token"
ghcr_token="$(token "$ghcr_registry/token?service=ghcr.io&scope=repository:$ghcr_repo:pull")" || die "no GHCR token"
[ -n "$hub_token" ] && [ -n "$ghcr_token" ] || die "a registry returned an empty token"
hub_tags="$(tags_of "$hub_registry" "$hub_repo" "$hub_token")" || die "cannot list the tags of docker.io/$hub_repo"
ghcr_tags="$(tags_of "$ghcr_registry" "$ghcr_repo" "$ghcr_token")" || die "cannot list the tags of ghcr.io/$ghcr_repo"

published=
if printf '%s\n' "$hub_tags" | grep -Fqx -- "$tag"; then published="docker.io"; fi
if printf '%s\n' "$ghcr_tags" | grep -Fqx -- "$tag"; then published="$published ghcr.io"; fi
published="${published# }"
[ -n "$published" ] || die "neither registry has $tag: a release is published by its tag (release.yaml), not rebuilt"

n="$(printf '%s\n%s\n' "$hub_tags" "$ghcr_tags" | awk -v p="$tag-r" '
	index($0, p) == 1 { s = substr($0, length(p) + 1); if (s ~ /^[0-9]+$/ && s + 0 > m) m = s + 0 }
	END { print m + 1 }')"

# Every release moves latest (release.yaml). The rebuild moves it too only when latest is
# this release in both registries now: never backwards to an older release, and never when
# the registries disagree.
digest() { # label, registry, repository, token, tag
	digest_of "$2" "$3" "$4" "$5" || die "cannot read the manifest of $1/$3:$5"
}
hub_latest="$(digest docker.io "$hub_registry" "$hub_repo" "$hub_token" latest)"
ghcr_latest="$(digest ghcr.io "$ghcr_registry" "$ghcr_repo" "$ghcr_token" latest)"
hub_release="$(digest docker.io "$hub_registry" "$hub_repo" "$hub_token" "$tag")"
ghcr_release="$(digest ghcr.io "$ghcr_registry" "$ghcr_repo" "$ghcr_token" "$tag")"
latest_on=
if [ -n "$hub_latest" ] && [ "$hub_latest" = "$hub_release" ]; then latest_on="docker.io"; fi
if [ -n "$ghcr_latest" ] && [ "$ghcr_latest" = "$ghcr_release" ]; then latest_on="$latest_on ghcr.io"; fi
latest_on="${latest_on# }"

{
	printf '%s-r%s\n%s\n' "$tag" "$n" "$tag"
	if [ "$latest_on" = "docker.io ghcr.io" ]; then echo latest; fi
} >"$tags_file"
printf 'VERSION=%s\nCOMMIT=%s\n' "$tag" "$commit" >"$env_file"
echo "prepare-rebuild: rebuilding $tag (commit $commit, published on $published)"
case "$latest_on" in
"docker.io ghcr.io") echo "prepare-rebuild: image tags $tag-r$n, $tag and latest (moved to the rebuild)" ;;
"") echo "prepare-rebuild: image tags $tag-r$n and $tag (moved to the rebuild); latest does not reference $tag and stays" ;;
*) echo "prepare-rebuild: image tags $tag-r$n and $tag (moved to the rebuild); latest is $tag on $latest_on only, so it is left alone in both registries" ;;
esac
# Last: this replaces the script file being read with the tag's version.
exec git checkout -q --detach "$commit"
