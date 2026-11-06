#!/usr/bin/env bash
# Build a static ARM64 FRNT discovery client and a GL-AXT1800/OpenWrt opkg package.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION="0.1.1"
PACKAGE="frnt-openwrt-client"
ARCH="aarch64_cortex-a53_neon-vfpv4"
WORK="$ROOT/dist/package-work"
OUTPUT="$ROOT/dist/${PACKAGE}_${VERSION}_${ARCH}.ipk"

if ! command -v go >/dev/null 2>&1; then
  echo "Go 1.25+ is required on the build host" >&2
  exit 1
fi
if ! command -v ar >/dev/null 2>&1; then
  echo "GNU ar is required on the build host" >&2
  exit 1
fi

mkdir -p "$ROOT/dist"
rm -rf "$WORK"
mkdir -p "$WORK/data/usr/bin" "$WORK/data/etc/config" "$WORK/data/etc/init.d" "$WORK/control"

(
  cd "$ROOT"
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -mod=mod -trimpath -ldflags="-s -w" -o "$WORK/data/usr/bin/frnt-openwrt-client" .
)

install -m 0644 "$ROOT/openwrt/etc/config/frnt-client" "$WORK/data/etc/config/frnt-client"
install -m 0755 "$ROOT/openwrt/etc/init.d/frnt-client" "$WORK/data/etc/init.d/frnt-client"

cat > "$WORK/control/control" <<EOF
Package: $PACKAGE
Version: $VERSION
Architecture: $ARCH
Maintainer: W4CAR
Section: net
Priority: optional
Description: Headless FlexRadio discovery client for OpenWrt, with procd start/stop.
EOF
printf '/etc/config/frnt-client\n' > "$WORK/control/conffiles"
(
  cd "$WORK"
  tar -czf control.tar.gz -C control .
  tar -czf data.tar.gz -C data .
  printf '2.0\n' > debian-binary
  ar cr "$OUTPUT" debian-binary control.tar.gz data.tar.gz
)
echo "Package created: $OUTPUT"
