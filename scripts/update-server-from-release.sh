#!/usr/bin/env bash
set -euo pipefail

# Update FRNT server binary from GitHub Releases.
# Usage:
#   ./update-server-from-release.sh            # latest release
#   ./update-server-from-release.sh v0.2.0     # specific tag
#
# Optional env:
#   REPO="owner/repo"            (default: KingSteve032/Flex-Radio-Network-Tool)
#   SERVICE_NAME="frnt-listen.service"
#   INSTALL_PATH="/usr/local/bin/frnt"

REPO="${REPO:-KingSteve032/Flex-Radio-Network-Tool}"
SERVICE_NAME="${SERVICE_NAME:-frnt-listen.service}"
INSTALL_PATH="${INSTALL_PATH:-/usr/local/bin/frnt}"
VERSION="${1:-latest}"

arch="$(uname -m)"
case "${arch}" in
  x86_64|amd64)
    asset="frnt-linux-amd64"
    ;;
  aarch64|arm64)
    asset="frnt-linux-arm64"
    ;;
  armv7l|armv7|armhf)
    asset="frnt-linux-armv7"
    ;;
  *)
    echo "Unsupported architecture: ${arch}" >&2
    exit 1
    ;;
esac

if [[ "${VERSION}" == "latest" ]]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
fi

tmp="$(mktemp /tmp/frnt-update.XXXXXX)"
trap 'rm -f "${tmp}"' EXIT

echo "Downloading ${url}"
curl -fL --retry 3 --connect-timeout 10 -o "${tmp}" "${url}"
chmod +x "${tmp}"

echo "Installing to ${INSTALL_PATH}"
sudo install -m 0755 "${tmp}" "${INSTALL_PATH}"

echo "Restarting ${SERVICE_NAME}"
sudo systemctl restart "${SERVICE_NAME}"
sudo systemctl is-active "${SERVICE_NAME}"

echo "Installed version:"
"${INSTALL_PATH}" --version || true

