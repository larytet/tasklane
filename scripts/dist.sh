#!/bin/sh
# Builds release tarballs for linux/amd64 and linux/arm64 into $1 (default dist/). The build is reproducible: the
# same source and Go version give byte-identical archives (trimmed paths, no build id, sorted entries, zero mtimes).
set -eu
OUT=${1:-dist}
VERSION=${VERSION:-0.1.0-dev}
GO=${GO:-go}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
$GO run ./cmd/notice "$STAGE/SBOM.txt" >/dev/null
for ARCH in amd64 arm64; do
  D="$STAGE/tasklane-$VERSION-linux-$ARCH"
  mkdir -p "$D/deploy"
  CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH $GO build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X main.version=$VERSION -X main.versionMarker=tasklane-release-version=$VERSION;" -o "$D/tasklane" ./cmd/tasklane
  cp NOTICE "$STAGE/SBOM.txt" README.md "$D/"
  cp deploy/systemd/tasklane.service deploy/tasklane.conf.example "$D/deploy/"
  cp docs/CONFIGURATION.md "$D/"
  (cd "$STAGE" && find "tasklane-$VERSION-linux-$ARCH" -print | LC_ALL=C sort | \
    tar --no-recursion --mtime='@0' --owner=0 --group=0 --numeric-owner --mode='a+rX,u+w,go-w' -cf - -T - | gzip -n -9 > "$OUT/tasklane-$VERSION-linux-$ARCH.tar.gz")
done
(cd "$OUT" && sha256sum tasklane-"$VERSION"-linux-*.tar.gz > SHA256SUMS)
# with RELEASE_KEY (the private key made by `go run ./cmd/relsign keygen DIR`) the checksum file is signed; an operator checks it
# with `tasklane verify-release release.pub DIR` before installing
if [ -n "${RELEASE_KEY:-}" ]; then
  $GO run ./cmd/relsign sign "$RELEASE_KEY" "$OUT"
fi
cat "$OUT/SHA256SUMS"
