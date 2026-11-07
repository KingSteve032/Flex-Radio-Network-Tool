#!/usr/bin/env bash
# Build Linux/ARM64 FRNT discovery binary and OpenWrt 23.05-compatible ipks.
# IMPORTANT: OpenWrt's ipkg-build uses a gzip-compressed *tar* container,
# not the Debian ar container used for the broken v0.1.1 package.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION="0.1.2"
ARCH="aarch64_cortex-a53_neon-vfpv4"
WORK="$ROOT/dist/package-work"
DIST="$ROOT/dist"

command -v go >/dev/null || { echo "Go 1.25+ required for build" >&2; exit 1; }
command -v tar >/dev/null || { echo "tar required for packaging" >&2; exit 1; }

mkdir -p "$DIST"
rm -rf "$WORK"
mkdir -p "$WORK/base/data" "$WORK/base/control" "$WORK/luci/data" "$WORK/luci/control"

install -d "$WORK/base/data/usr/bin" "$WORK/base/data/etc/config" \
  "$WORK/base/data/etc/init.d" "$WORK/base/data/usr/libexec"
(
  cd "$ROOT"
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -mod=mod -trimpath -ldflags="-s -w" \
    -o "$WORK/base/data/usr/bin/frnt-openwrt-client" .
)
chmod 0755 "$WORK/base/data/usr/bin/frnt-openwrt-client"

install -m 0644 "$ROOT/openwrt/etc/config/frnt-client" "$WORK/base/data/etc/config/frnt-client"
install -m 0755 "$ROOT/openwrt/etc/init.d/frnt-client" "$WORK/base/data/etc/init.d/frnt-client"
install -m 0755 "$ROOT/openwrt/usr/libexec/frnt-client-query" "$WORK/base/data/usr/libexec/frnt-client-query"
install -m 0755 "$ROOT/openwrt/usr/libexec/frnt-client-control" "$WORK/base/data/usr/libexec/frnt-client-control"

cat > "$WORK/base/control/control" <<EOF
Package: frnt-openwrt-client
Version: $VERSION
Architecture: $ARCH
Maintainer: W4CAR
Section: net
Priority: optional
Description: NetBird FRNT discovery client; rebroadcasts FlexRadio discovery to LAN.
EOF
printf '/etc/config/frnt-client\n' > "$WORK/base/control/conffiles"

install -d "$WORK/luci/data/usr/share/luci/menu.d" \
  "$WORK/luci/data/usr/share/rpcd/acl.d" \
  "$WORK/luci/data/www/luci-static/resources/view/frnt"

install -m 0644 "$ROOT/luci/usr/share/luci/menu.d/luci-app-frnt-client.json" \
  "$WORK/luci/data/usr/share/luci/menu.d/luci-app-frnt-client.json"
install -m 0644 "$ROOT/luci/usr/share/rpcd/acl.d/luci-app-frnt-client.json" \
  "$WORK/luci/data/usr/share/rpcd/acl.d/luci-app-frnt-client.json"
install -m 0644 "$ROOT/luci/www/luci-static/resources/view/frnt/client.js" \
  "$WORK/luci/data/www/luci-static/resources/view/frnt/client.js"

cat > "$WORK/luci/control/control" <<EOF
Package: luci-app-frnt-client
Version: $VERSION
Architecture: all
Maintainer: W4CAR
Section: luci
Priority: optional
Depends: frnt-openwrt-client, luci-base, rpcd-mod-file
Description: LuCI web controls, NetBird/FRNT status, configuration and logs.
EOF

make_ipk() {
  local name="$1" arch="$2" stage="$3"
  local output="$DIST/${name}_${VERSION}_${arch}.ipk"
  # Force root ownership for package files even when GitHub runner is unprivileged.
  tar --format=gnu --numeric-owner --owner=0 --group=0 -czf "$stage/control.tar.gz" -C "$stage/control" .
  tar --format=gnu --numeric-owner --owner=0 --group=0 -czf "$stage/data.tar.gz" -C "$stage/data" .
  printf '2.0\n' > "$stage/debian-binary"
  tar --format=gnu --numeric-owner --owner=0 --group=0 -czf "$output" -C "$stage" \
    ./debian-binary ./data.tar.gz ./control.tar.gz
  echo "OpenWrt package created: $output"
}

make_ipk frnt-openwrt-client "$ARCH" "$WORK/base"
make_ipk luci-app-frnt-client all "$WORK/luci"
