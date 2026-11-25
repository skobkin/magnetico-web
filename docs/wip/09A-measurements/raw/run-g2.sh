#!/bin/sh
# Task 09A qualification, step G2: real native archives and OCI images from the clean clone.
set -u
cd /home/herdr/qual/09A
L=/home/herdr/src/magnetico-web/.superpowers/sdd/09A/qualification
stamp() { echo "$(date -u +%FT%TZ) $*" >> $L/g2-progress.txt; }
: > $L/g2-progress.txt
SHA=$(git rev-parse HEAD)
stamp "commit $SHA; disk: $(df -h / | tail -1)"
# ---- native archives (snapshot: no tag) in the pinned GoReleaser image
stamp "goreleaser snapshot"
docker run --rm -e GOTOOLCHAIN=go1.27.4+auto -v "$PWD":/src -w /src goreleaser/goreleaser:v2.20.1 check > $L/goreleaser-check.log 2>&1; stamp "goreleaser check exit $?"
docker run --rm -e GOTOOLCHAIN=go1.27.4+auto -v "$PWD":/src -w /src goreleaser/goreleaser:v2.20.1 release --snapshot --clean --skip=publish > $L/goreleaser.log 2>&1; stamp "goreleaser snapshot exit $?"
(cd dist && sha256sum -c checksums.txt) > $L/dist-checksums.log 2>&1; stamp "sha256sum -c exit $?"
sh ci/native-smoke.sh dist > $L/native-smoke.log 2>&1; stamp "native-smoke exit $?"
cp dist/checksums.txt $L/native-checksums.txt
# ---- OCI image: one build, amd64 + arm64, SBOM + provenance, OCI layout
stamp "image build"
rm -rf /home/herdr/qual/image
docker buildx build --platform linux/amd64,linux/arm64 --provenance=mode=max --sbom=true \
  --build-arg VERSION=0.0.0-qual --build-arg COMMIT="$SHA" \
  --output type=oci,tar=false,dest=/home/herdr/qual/image,name=staged.invalid/magnetico-web:staged . > $L/image-build.log 2>&1; stamp "image build exit $?; disk: $(df -h / | tail -1)"
sh ci/check-attestations.sh /home/herdr/qual/image > $L/attestations.log 2>&1; stamp "check-attestations exit $?"
# ---- load the layout into the local daemon and smoke-test the amd64 image
tar -C /home/herdr/qual/image -cf - . | docker load > $L/image-load.log 2>&1; stamp "docker load exit $?"
EXPECT_VERSION=0.0.0-qual EXPECT_COMMIT="$SHA" sh ci/image-smoke.sh --platform linux/amd64 staged.invalid/magnetico-web:staged > $L/image-smoke.log 2>&1; stamp "image-smoke exit $?"
# ---- vulnerability scan with fresh data, both platforms
mkdir -p /home/herdr/qual/trivy-cache
for p in linux/amd64 linux/arm64; do
  n=$(echo $p | tr / -)
  docker run --rm -v /home/herdr/qual/image:/image:ro -v /home/herdr/qual/trivy-cache:/root/.cache/trivy \
    aquasec/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb \
    image --input /image --platform $p --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 --scanners vuln > $L/trivy-$n.log 2>&1; stamp "trivy $p exit $?"
done
docker run --rm -v /home/herdr/qual/trivy-cache:/root/.cache/trivy aquasec/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb version > $L/trivy-version.log 2>&1
stamp done
