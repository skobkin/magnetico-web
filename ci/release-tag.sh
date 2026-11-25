#!/bin/sh
# Print the release tag that a forced rebuild (.woodpecker/rebuild.yaml) or the scheduled
# image scan (.woodpecker/security.yaml) targets by default:
#
#   - the newest stable release tag of the Go line (MAJOR.MINOR.PATCH, no suffix);
#   - else the newest Go-line release tag with a suffix (a release candidate before the
#     first stable release);
#   - else nothing (exit 0 with empty output).
#
# "Newest" is the tag's creation date. The Go line is a tag whose tree has cmd/magnetico:
# the PHP-era tags (0.2.0 … 0.7.x) are never targets. Run it in a clone that has the tags.
set -eu

newest_any=
for tag in $(git for-each-ref --sort=-creatordate --format='%(refname:short)' refs/tags); do
	printf '%s\n' "$tag" | grep -Eq '^[0-9]+[.][0-9]+[.][0-9]+' || continue
	git cat-file -e "refs/tags/$tag:cmd/magnetico" 2>/dev/null || continue
	if printf '%s\n' "$tag" | grep -Eq '^[0-9]+[.][0-9]+[.][0-9]+$'; then
		printf '%s\n' "$tag"
		exit 0
	fi
	[ -n "$newest_any" ] || newest_any="$tag"
done
if [ -n "$newest_any" ]; then
	printf '%s\n' "$newest_any"
fi
