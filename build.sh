#!/usr/bin/env bash
#
# Local build of the rise-mcp-bridge binaries into ./dist, with optional macOS
# codesign + notarize. CI (.github/workflows/release.yml) is the source of truth
# for released artifacts; this script is for local builds and dev.
#
# Asset naming (must match the consumer fetch script):
#   rise-mcp-bridge-darwin-universal       (arm64 + amd64)
#   rise-mcp-bridge-windows-amd64.exe
#   rise-mcp-bridge-linux-amd64
#   SHA256SUMS
#
# macOS sign+notarize env (optional locally, required for distributable mac builds):
#   DEVELOPER_ID_APP   "Developer ID Application: Rise People Inc (TEAMID)"
#   NOTARY_PROFILE     notarytool keychain profile
#
set -euo pipefail
cd "$(dirname "$0")"
DIST="dist"; NAME="rise-mcp-bridge"; LDFLAGS="-s -w"
rm -rf "$DIST"; mkdir -p "$DIST"

echo "==> go vet"; go vet ./...

echo "==> macOS arm64 + amd64 -> universal"
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/$NAME-darwin-arm64" .
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/$NAME-darwin-amd64" .
if command -v lipo >/dev/null; then
  lipo -create -output "$DIST/$NAME-darwin-universal" "$DIST/$NAME-darwin-arm64" "$DIST/$NAME-darwin-amd64"
  rm -f "$DIST/$NAME-darwin-arm64" "$DIST/$NAME-darwin-amd64"
else
  echo "!! lipo not found (not on macOS) — keeping per-arch mac binaries; CI builds the universal."
fi

echo "==> windows/amd64 + linux/amd64"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/$NAME-windows-amd64.exe" .
GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/$NAME-linux-amd64" .

if [[ -n "${DEVELOPER_ID_APP:-}" && -n "${NOTARY_PROFILE:-}" && -f "$DIST/$NAME-darwin-universal" ]]; then
  echo "==> codesign + notarize macOS"
  codesign --force --options runtime --timestamp --entitlements entitlements.plist \
    --sign "$DEVELOPER_ID_APP" "$DIST/$NAME-darwin-universal"
  ZIP="$DIST/$NAME-darwin-universal.zip"
  ditto -c -k --keepParent "$DIST/$NAME-darwin-universal" "$ZIP"
  xcrun notarytool submit "$ZIP" --keychain-profile "$NOTARY_PROFILE" --wait
  rm -f "$ZIP"
  codesign --verify --strict --verbose=2 "$DIST/$NAME-darwin-universal"
fi

echo "==> SHA256SUMS"
( cd "$DIST" && shasum -a 256 * > SHA256SUMS && cat SHA256SUMS )
echo "==> done -> $DIST"
