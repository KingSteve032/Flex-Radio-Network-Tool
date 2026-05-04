#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   bash scripts/install-client-macos.sh            # latest
#   bash scripts/install-client-macos.sh v0.2.6     # specific tag
#
# Optional env:
#   REPO="KingSteve032/Flex-Radio-Network-Tool"

REPO="${REPO:-KingSteve032/Flex-Radio-Network-Tool}"
VERSION="${1:-latest}"

arch="$(uname -m)"
case "${arch}" in
  arm64)
    asset="frnt-darwin-arm64"
    ;;
  x86_64)
    asset="frnt-darwin-amd64"
    ;;
  *)
    echo "Unsupported macOS architecture: ${arch}" >&2
    exit 1
    ;;
esac

if [[ "${VERSION}" == "latest" ]]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
fi

tmp_bin="$(mktemp /tmp/frnt-macos.XXXXXX)"
tmp_icon="$(mktemp /tmp/frnt-icon.XXXXXX)"
trap 'rm -f "${tmp_bin}" "${tmp_icon}"' EXIT

echo "Downloading ${url}"
curl -fL --retry 3 --connect-timeout 10 -o "${tmp_bin}" "${url}"
chmod +x "${tmp_bin}"

sudo mkdir -p "/Applications/Flex Radio Network Tool"
sudo install -m 0755 "${tmp_bin}" "/Applications/Flex Radio Network Tool/frnt"

icon_url="https://raw.githubusercontent.com/${REPO}/main/assets/icon.png"
echo "Downloading ${icon_url}"
curl -fL --retry 3 --connect-timeout 10 -o "${tmp_icon}" "${icon_url}"
sudo install -m 0644 "${tmp_icon}" "/Applications/Flex Radio Network Tool/icon.png"

echo "Installed:"
"/Applications/Flex Radio Network Tool/frnt" --version || true

