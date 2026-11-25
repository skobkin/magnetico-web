# syntax=docker/dockerfile:1

# One universal image for every subcommand of the `magnetico` executable
# (crawl, ingest, web, migrate, dev). Build:
#
#   docker buildx build --build-arg VERSION=0.8.0-rc.0 --build-arg COMMIT=$(git rev-parse HEAD) .
#
# The compiler stage runs on the build platform and cross-compiles for the
# target platform (no emulation); the final stage holds the static executable
# and nothing else of the build.

ARG GO_VERSION=1.27.4
ARG ALPINE_VERSION=3.24

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build
# A failure anywhere in a pipeline fails the step (busybox ash supports pipefail).
SHELL ["/bin/ash", "-o", "pipefail", "-c"]
ARG TARGETOS
ARG TARGETARCH
# The same linker values as .goreleaser.yaml: internal/buildinfo.
ARG VERSION=dev
ARG COMMIT=
# GOTOOLCHAIN=local: never download a toolchain. Under `local` go.mod's `toolchain`
# line is only a hint (the hard minimum is its `go` line), so the security floor is
# enforced explicitly below.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.sum ./
RUN want="$(sed -n 's/^toolchain go//p' go.mod)" \
 && have="$(go env GOVERSION | sed 's/^go//')" \
 && [ "$(printf '%s\n%s\n' "$want" "$have" | sort -V | head -n 1)" = "$want" ] \
 || { echo "the build image has Go $have, go.mod requires at least $want (the security floor)"; exit 1; }
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X github.com/skobkin/magnetico-web/internal/buildinfo.Version=${VERSION} -X github.com/skobkin/magnetico-web/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/magnetico ./cmd/magnetico \
 && go version -m /out/magnetico >/tmp/buildinfo.txt \
 && grep -q 'CGO_ENABLED=0' /tmp/buildinfo.txt

# Native-architecture helper: it only prepares text files and empty directories,
# so the final stage needs no RUN instruction and building the arm64 image needs
# no emulation (QEMU) at all, not even for the user setup.
FROM --platform=$BUILDPLATFORM alpine:${ALPINE_VERSION} AS rootfs
RUN set -eu; \
    mkdir -p /out/etc /out/data /out/reports; \
    cp /etc/passwd /etc/group /out/etc/; \
    echo 'magnetico:x:10001:10001:magnetico:/nonexistent:/sbin/nologin' >> /out/etc/passwd; \
    echo 'magnetico:x:10001:' >> /out/etc/group

FROM alpine:${ALPINE_VERSION}
ARG VERSION=dev
ARG COMMIT=
# The Alpine base already carries the system CA bundle (ca-certificates-bundle);
# custom trust roots: mount a bundle and set SSL_CERT_FILE (or SSL_CERT_DIR). A
# derived image can `apk add ca-certificates` for update-ca-certificates. IANA
# time zone data is embedded in the executable (time/tzdata).
# Fixed, documented identity: uid 10001, gid 10001 (user and group "magnetico").
COPY --from=rootfs /out/etc/passwd /out/etc/group /etc/
COPY --from=rootfs --chown=10001:10001 /out/data /data
COPY --from=rootfs --chown=10001:10001 /out/reports /reports
COPY --from=build /out/magnetico /usr/local/bin/magnetico
COPY COPYING /usr/share/licenses/magnetico/COPYING
COPY third_party/swagger-ui-dist-5.34.1/LICENSE /usr/share/licenses/magnetico/swagger-ui-LICENSE
COPY third_party/swagger-ui-dist-5.34.1/NOTICE /usr/share/licenses/magnetico/swagger-ui-NOTICE
COPY third_party/swagger-ui-dist-5.34.1/swagger-ui-bundle.js.LICENSE.txt /usr/share/licenses/magnetico/swagger-ui-bundle.js.LICENSE.txt
COPY third_party/uplot-1.6.32/LICENSE /usr/share/licenses/magnetico/uplot-LICENSE

LABEL org.opencontainers.image.title="Magnetico Web" \
      org.opencontainers.image.description="Magnetico: DHT crawler, ingest service, private web UI and API, and the migration tooling, in one executable" \
      org.opencontainers.image.source="https://github.com/skobkin/magnetico-web" \
      org.opencontainers.image.url="https://git.skobk.in/skobkin/magnetico-web" \
      org.opencontainers.image.documentation="https://github.com/skobkin/magnetico-web/blob/master/README.md" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

# /data: the crawler's persistent SQLite queue (mount a volume; a bind mount must
# be owned by 10001:10001). /reports: mount point for migration reports and
# forensic exports (--report-json, --output, --evidence-manifest). Neither is
# declared a VOLUME: an anonymous volume would be silently reused across
# container re-creations.
ENV MAGNETICO_CRAWL_QUEUE_DIR=/data
WORKDIR /data
USER 10001:10001
# Exec form: the executable is PID 1 and receives SIGTERM directly. No default
# command: started without arguments it prints its usage and exits (it never
# starts a crawler, initialises a database or migrates anything on its own).
# No HEALTHCHECK: a universal one cannot fit crawl, ingest, web and migrate;
# see docs/web-deployment.md for service-specific checks.
ENTRYPOINT ["/usr/local/bin/magnetico"]
