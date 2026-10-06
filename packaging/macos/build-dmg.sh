#!/usr/bin/env bash
# Build "AIO Agent.app" (universal arm64+amd64) and wrap it in a DMG.
#
#   packaging/macos/build-dmg.sh [version] [--app-only]
#
# Output: dist/AIO Agent.app and dist/AIO-Agent-<version>-macos.dmg
# (--app-only skips the DMG, for local testing of the bundle).
#
# The app is ad-hoc signed only. Until a Developer ID is set up, users must
# allow it once via System Settings → Privacy & Security → "Open Anyway"
# (or: xattr -dr com.apple.quarantine "/Applications/AIO Agent.app").
set -euo pipefail

cd "$(dirname "$0")/../.."
VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}"
APP_ONLY="${2:-}"
GO="${GO:-$(command -v go || echo /opt/homebrew/bin/go)}"
export MACOSX_DEPLOYMENT_TARGET=12.3

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
APP="dist/AIO Agent.app"
DMG="dist/AIO-Agent-${VERSION}-macos.dmg"

echo "==> building aio-agent ${VERSION} (arm64 + amd64)"
for arch in arm64 amd64; do
  clang_arch=$([ "$arch" = amd64 ] && echo x86_64 || echo arm64)
  CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" CC="clang -arch $clang_arch" \
    "$GO" build -tags production -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o "$WORK/aio-agent-$arch" ./cmd/desktop
done

echo "==> assembling app bundle"
mkdir -p dist
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
lipo -create -output "$APP/Contents/MacOS/aio-agent" "$WORK/aio-agent-arm64" "$WORK/aio-agent-amd64"
sed "s/__VERSION__/${VERSION}/g" packaging/macos/Info.plist > "$APP/Contents/Info.plist"

ICONSET="$WORK/icon.iconset"
mkdir -p "$ICONSET"
for s in 16 32 128 256 512; do
  sips -z $s $s cmd/desktop/assets/appicon.png --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
  sips -z $((s*2)) $((s*2)) cmd/desktop/assets/appicon.png --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/icon.icns"

# Ad-hoc signature (required to run on Apple Silicon). Replace "-" with a
# "Developer ID Application: …" identity and add notarization when available.
codesign --force --deep --options runtime --sign - "$APP"

if [ "$APP_ONLY" = "--app-only" ]; then
  echo "==> done: ${APP}"
  exit 0
fi

echo "==> creating ${DMG}"
mkdir -p "$WORK/stage"
cp -R "$APP" "$WORK/stage/"
ln -s /Applications "$WORK/stage/Applications"
rm -f "$DMG"
hdiutil create -volname "AIO Agent" -srcfolder "$WORK/stage" -ov -format UDZO "$DMG" >/dev/null
echo "==> done: ${DMG} ($(du -h "$DMG" | cut -f1))"
