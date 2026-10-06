#!/usr/bin/env bash
# Cross-compile the Windows desktop app and build its NSIS installer. Runs on
# macOS, Linux or Windows (Git Bash); needs Go and makensis (NSIS 3).
#
#   packaging/windows/build-installer.sh [version]
#
# Output: dist/AIO-Agent-<version>-windows-amd64-setup.exe
#
# Unsigned until a code-signing certificate is set up, so Windows SmartScreen
# shows "Windows protected your PC" → "More info" → "Run anyway" on first run.
set -euo pipefail

cd "$(dirname "$0")/../.."
VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}"
GO="${GO:-$(command -v go || echo /opt/homebrew/bin/go)}"
# NSIS and Windows resources need a numeric x.y.z version.
NUMVER="$(echo "$VERSION" | grep -Eo '^[0-9]+\.[0-9]+\.[0-9]+' || echo 0.0.0)"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK" cmd/desktop/rsrc_windows_*.syso' EXIT
OUT="dist/AIO-Agent-${VERSION}-windows-amd64-setup.exe"

echo "==> embedding icon + manifest"
# Writes cmd/desktop/rsrc_windows_{amd64,386}.syso, linked only into Windows builds.
"$GO" run github.com/tc-hib/go-winres@v0.3.3 simply \
  --icon cmd/desktop/assets/appicon.png \
  --manifest gui \
  --product-name "AIO Agent" \
  --file-description "AIO Agent" \
  --product-version "$NUMVER" \
  --file-version "$NUMVER" \
  --out cmd/desktop/rsrc

echo "==> building AIO Agent.exe ${VERSION}"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  "$GO" build -tags production -trimpath \
    -ldflags "-s -w -H windowsgui -X main.version=${VERSION}" \
    -o "$WORK/aio-agent.exe" ./cmd/desktop

echo "==> building installer"
mkdir -p dist
makensis -V2 \
  "-DVERSION=${NUMVER}" \
  "-DEXE=${WORK}/aio-agent.exe" \
  "-DICON=$(pwd)/cmd/desktop/assets/icon.ico" \
  "-DOUTFILE=$(pwd)/${OUT}" \
  packaging/windows/installer.nsi
echo "==> done: ${OUT} ($(du -h "$OUT" | cut -f1))"
