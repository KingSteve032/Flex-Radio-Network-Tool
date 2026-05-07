#!/usr/bin/env bash
set -euo pipefail

# Build a macOS .pkg installer from an FRNT binary.
# Usage:
#   scripts/build-macos-installer.sh <version> <input-binary> [output-dir]
#
# Example:
#   scripts/build-macos-installer.sh v0.2.8 dist/frnt-darwin-universal dist/installer

VERSION="${1:-dev}"
INPUT_BIN="${2:-}"
OUT_DIR="${3:-dist/installer}"

if [[ -z "${INPUT_BIN}" ]]; then
  echo "error: input binary is required" >&2
  exit 1
fi
if [[ ! -f "${INPUT_BIN}" ]]; then
  echo "error: input binary not found: ${INPUT_BIN}" >&2
  exit 1
fi
if [[ ! -f "assets/icon.png" ]]; then
  echo "error: assets/icon.png not found" >&2
  exit 1
fi

APP_NAME="Flex Radio Network Tool"
BUNDLE_ID="org.w4car.flexradiotool"
APP_DIR="${OUT_DIR}/${APP_NAME}.app"
CONTENTS_DIR="${APP_DIR}/Contents"
MACOS_DIR="${CONTENTS_DIR}/MacOS"
RES_DIR="${CONTENTS_DIR}/Resources"
ICONSET_DIR="${OUT_DIR}/icon.iconset"
ICNS_PATH="${RES_DIR}/icon.icns"
PKG_PATH="${OUT_DIR}/frnt-macos-universal-installer-${VERSION}.pkg"

rm -rf "${APP_DIR}" "${ICONSET_DIR}" "${PKG_PATH}"
mkdir -p "${MACOS_DIR}" "${RES_DIR}" "${ICONSET_DIR}"

cp "${INPUT_BIN}" "${MACOS_DIR}/frnt"
chmod 0755 "${MACOS_DIR}/frnt"

# Generate .icns from icon.png.
sips -z 16 16 assets/icon.png --out "${ICONSET_DIR}/icon_16x16.png" >/dev/null
sips -z 32 32 assets/icon.png --out "${ICONSET_DIR}/icon_16x16@2x.png" >/dev/null
sips -z 32 32 assets/icon.png --out "${ICONSET_DIR}/icon_32x32.png" >/dev/null
sips -z 64 64 assets/icon.png --out "${ICONSET_DIR}/icon_32x32@2x.png" >/dev/null
sips -z 128 128 assets/icon.png --out "${ICONSET_DIR}/icon_128x128.png" >/dev/null
sips -z 256 256 assets/icon.png --out "${ICONSET_DIR}/icon_128x128@2x.png" >/dev/null
sips -z 256 256 assets/icon.png --out "${ICONSET_DIR}/icon_256x256.png" >/dev/null
sips -z 512 512 assets/icon.png --out "${ICONSET_DIR}/icon_256x256@2x.png" >/dev/null
sips -z 512 512 assets/icon.png --out "${ICONSET_DIR}/icon_512x512.png" >/dev/null
sips -z 1024 1024 assets/icon.png --out "${ICONSET_DIR}/icon_512x512@2x.png" >/dev/null
iconutil -c icns "${ICONSET_DIR}" -o "${ICNS_PATH}"

cat > "${CONTENTS_DIR}/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleDevelopmentRegion</key>
  <string>English</string>
  <key>CFBundleExecutable</key>
  <string>frnt</string>
  <key>CFBundleIdentifier</key>
  <string>${BUNDLE_ID}</string>
  <key>CFBundleInfoDictionaryVersion</key>
  <string>6.0</string>
  <key>CFBundleName</key>
  <string>${APP_NAME}</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>${VERSION}</string>
  <key>CFBundleVersion</key>
  <string>${VERSION}</string>
  <key>CFBundleIconFile</key>
  <string>icon.icns</string>
  <key>LSMinimumSystemVersion</key>
  <string>11.0</string>
</dict>
</plist>
EOF

mkdir -p "${OUT_DIR}"
pkgbuild \
  --component "${APP_DIR}" \
  --install-location "/Applications" \
  --identifier "${BUNDLE_ID}.installer" \
  --version "${VERSION#v}" \
  "${PKG_PATH}"

echo "Created ${PKG_PATH}"
