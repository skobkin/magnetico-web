#!/bin/sh
# Check, on the OCI layout the release gates qualify, that every platform carries
# BOTH an SBOM (SPDX) and a provenance (SLSA) attestation.
#
#   ci/check-attestations.sh OCI_LAYOUT_DIR
#
# Counting attestation manifests is not enough: with provenance alone there are
# also two of them (one per platform). Each attestation manifest lists its
# in-toto layers with an `in-toto.io/predicate-type` annotation; this requires one
# SPDX and one SLSA predicate in each of the two attestation manifests.
set -eu

LAYOUT="${1:?usage: check-attestations.sh OCI_LAYOUT_DIR}"
fail() {
	echo "check-attestations: $*" >&2
	exit 1
}

[ -f "$LAYOUT/index.json" ] || fail "$LAYOUT is not an OCI image layout (no index.json)"
count="$(grep -Eo '"digest" *: *"sha256:[0-9a-f]{64}"' "$LAYOUT/index.json" | wc -l | tr -d ' ')"
[ "$count" = 1 ] || fail "the layout must name exactly one top-level index, found $count"
digest="$(grep -Eo 'sha256:[0-9a-f]{64}' "$LAYOUT/index.json" | head -n 1)"
index="$LAYOUT/blobs/sha256/${digest#sha256:}"
[ -f "$index" ] || fail "the index blob $digest is missing from the layout"

atts="$(grep -o 'attestation-manifest' "$index" | wc -l | tr -d ' ')"
[ "$atts" = 2 ] || fail "the image index lists $atts attestation manifests, want 2 (one per platform)"

sbom=0
prov=0
both=0
for f in "$LAYOUT"/blobs/sha256/*; do
	[ -f "$f" ] || continue
	# only small JSON manifests carry the annotation; skip layer blobs quickly
	grep -q 'in-toto.io/predicate-type' "$f" 2>/dev/null || continue
	has_sbom=0
	has_prov=0
	grep -Eq '"in-toto.io/predicate-type" *: *"https://spdx.dev/Document' "$f" && has_sbom=1
	grep -Eq '"in-toto.io/predicate-type" *: *"https://slsa.dev/provenance/' "$f" && has_prov=1
	[ "$has_sbom" = 1 ] && sbom=$((sbom + 1))
	[ "$has_prov" = 1 ] && prov=$((prov + 1))
	[ "$has_sbom" = 1 ] && [ "$has_prov" = 1 ] && both=$((both + 1))
done

[ "$sbom" = 2 ] || fail "$sbom attestation manifest(s) carry an SBOM (SPDX) predicate, want 2: the image was built without an SBOM"
[ "$prov" = 2 ] || fail "$prov attestation manifest(s) carry a provenance (SLSA) predicate, want 2"
[ "$both" = 2 ] || fail "SBOM and provenance are not both present in each attestation manifest"
echo "attestations ok: SBOM (SPDX) and provenance (SLSA) for both platforms"
