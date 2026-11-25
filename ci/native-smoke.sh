#!/bin/sh
# Smoke-test the native release archives built by `goreleaser release --snapshot --skip=publish`
# (the verification workflow) or by a local release build.
#
#   ci/native-smoke.sh DIST_DIR [EXPECTED_VERSION]
#
# Without EXPECTED_VERSION the version is read from the archive name (a snapshot
# build). Checks: the SHA-256 checksums of both archives, the licence and
# attribution files inside them, `--version` of the binary that can run on this
# host (version as expected, built with Go >= 1.27.4), and for BOTH architectures
# the build settings of the binary (CGO_ENABLED=0, the right GOARCH, and no -tags:
# a release never contains the diagnostic listener or the benchmark harness).
set -eu

DIST="${1:?usage: native-smoke.sh DIST_DIR [EXPECTED_VERSION]}"
VERSION="${2:-}"
FLOOR="1.27.4"
fails=0
fail() {
	echo "FAIL  $*" >&2
	fails=$((fails + 1))
}
ok() { echo "ok    $*"; }

cd "$DIST"
[ -f checksums.txt ] || {
	echo "FAIL  no checksums.txt in $DIST" >&2
	exit 1
}

if sha256sum -c checksums.txt >/dev/null 2>&1; then ok "checksums.txt verifies every archive"; else fail "sha256sum -c checksums.txt"; fi

if [ -z "$VERSION" ]; then
	first="$(ls magnetico-web_*_linux_amd64.tar.gz 2>/dev/null | head -n 1 || true)"
	[ -n "$first" ] || {
		echo "FAIL  no linux/amd64 archive in $DIST" >&2
		exit 1
	}
	VERSION="${first#magnetico-web_}"
	VERSION="${VERSION%_linux_amd64.tar.gz}"
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

host=""
case "$(uname -m)" in
x86_64) host=amd64 ;;
aarch64 | arm64) host=arm64 ;;
esac

for arch in amd64 arm64; do
	archive="magnetico-web_${VERSION}_linux_${arch}.tar.gz"
	if [ ! -f "$archive" ]; then
		fail "missing archive $archive"
		continue
	fi
	listing="$(tar -tzf "$archive")" || {
		fail "cannot read $archive"
		continue
	}
	for f in magnetico COPYING README.md UPGRADE.md licenses/swagger-ui-LICENSE licenses/swagger-ui-NOTICE licenses/swagger-ui-bundle.js.LICENSE.txt licenses/uplot-LICENSE; do
		if printf '%s\n' "$listing" | grep -qx "$f"; then :; else fail "$archive lacks $f"; fi
	done
	mkdir -p "$work/$arch"
	tar -xzf "$archive" -C "$work/$arch"
	bin="$work/$arch/magnetico"

	if grep -aq 'github.com/leeoniya/uPlot (v1.6.32)' "$bin"; then ok "$arch: the uPlot script is embedded in the executable"; else fail "$arch: the uPlot script is not embedded in the executable"; fi

	if command -v go >/dev/null 2>&1; then
		info="$(go version -m "$bin" 2>/dev/null || true)"
		if [ -z "$info" ]; then
			fail "$arch binary is not a Go executable"
		else
			printf '%s\n' "$info" | grep -q 'CGO_ENABLED=0' && ok "$arch: CGO_ENABLED=0" || fail "$arch: not built with CGO_ENABLED=0"
			printf '%s\n' "$info" | grep -q "GOARCH=$arch" && ok "$arch: GOARCH=$arch" || fail "$arch: wrong GOARCH"
			if printf '%s\n' "$info" | grep -q -- '-tags'; then fail "$arch: the release binary was built with -tags (diagnostic/harness code may be linked)"; else ok "$arch: no build tags"; fi
		fi
	else
		fail "no go toolchain to read the build settings of the $arch binary"
	fi

	if [ "$arch" = "$host" ]; then
		out="$("$bin" --version 2>&1)" || fail "$arch: --version failed: $out"
		line="$(printf '%s\n' "$out" | head -n 1)"
		if printf '%s\n' "$line" | grep -Eq "^magnetico ${VERSION} \(commit [0-9a-f]{12}(\+dirty)?, go[0-9.]+, linux/${arch}\)$"; then
			ok "$arch: $line"
			gov="$(printf '%s' "$line" | sed -E 's/.*, go([0-9]+\.[0-9]+(\.[0-9]+)?), .*/\1/')"
			if [ "$(printf '%s\n%s\n' "$FLOOR" "$gov" | sort -V | head -n 1)" = "$FLOOR" ]; then ok "$arch: built with Go $gov (>= $FLOOR)"; else fail "$arch: built with Go $gov, below the $FLOOR security floor"; fi
		else
			fail "$arch: --version printed '$line', expected version $VERSION"
		fi
	fi
done

if [ -z "$host" ]; then fail "this host ($(uname -m)) can run neither release binary"; fi
if [ "$fails" -ne 0 ]; then
	echo "native smoke test: $fails check(s) FAILED" >&2
	exit 1
fi
echo "native smoke test: all checks passed ($VERSION)"
