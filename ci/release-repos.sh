# shellcheck shell=sh disable=SC2034
# Sourced by promote-image.sh and verify-published.sh: the approved release
# repositories and the pinned registry tool. Not executable on its own.
#
# The only way to point the scripts elsewhere is the rehearsal pair below, which
# lets the release flow be exercised against throwaway local registries; the
# workflows never set it (internal/repocheck pins that).

# crane (go-containerregistry) is pinned by version; it runs through `go run`, so
# the Go toolchain of the step decides the compiler (the workflows use
# golang:1.27.4-*).
CRANE="${CRANE:-go run github.com/google/go-containerregistry/cmd/crane@v0.21.0}"

REPOS="docker.io/skobkin/magnetico-web ghcr.io/skobkin/magnetico-web"
REAL_REGISTRIES=1
if [ "${MAGNETICO_PROMOTE_REHEARSAL:-}" = 1 ] && [ -n "${MAGNETICO_PROMOTE_REPOS:-}" ]; then
	REPOS="$MAGNETICO_PROMOTE_REPOS"
	REAL_REGISTRIES=0
fi

# Release tag grammar (see docs/release-engineering.md): MAJOR.MINOR.PATCH or
# MAJOR.MINOR.PATCH-rc.N. Nothing else is ever applied to the image: no latest,
# no 0.8, no stable, no edge.
valid_release_tag() {
	case "$1" in
	*"
"*) return 1 ;;
	esac
	printf '%s' "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'
}
