#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
OUTPUT_DIR="./dist"
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
VERSION_TAG="${VERSION#v}"
APP_NAME="vs-ai-proxy"
MAIN_PATH="./cmd/server"
LDFLAGS="-s -w -X main.version=${VERSION}"

PLATFORMS=(
  "darwin/amd64"
  "darwin/arm64"
  "linux/amd64"
  "linux/arm64"
  "windows/amd64"
)

declare -A ALIASES=(
  ["darwin/amd64"]="macos-x64"
  ["darwin/arm64"]="macos-arm64"
  ["linux/amd64"]="linux-x64"
  ["linux/arm64"]="linux-arm64"
  ["windows/amd64"]="windows-x64"
)

rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

echo "🔨 构建 $APP_NAME ${VERSION} 所有平台..."
for plat in "${PLATFORMS[@]}"; do
  GOOS="${plat%%/*}"
  GOARCH="${plat##*/}"
  EXT=""
  if [ "$GOOS" = "windows" ]; then
    EXT=".exe"
  fi
  ALIAS="${ALIASES[$plat]}"
  NAME="${APP_NAME}-v${VERSION_TAG}-${ALIAS}${EXT}"
  echo "  → $GOOS/$GOARCH ..."
  GOOS="$GOOS" GOARCH="$GOARCH" go build -ldflags="$LDFLAGS" -o "$OUTPUT_DIR/$NAME" "$MAIN_PATH"
done
echo "✅ 全部构建完成: $OUTPUT_DIR/"
ls -lh "$OUTPUT_DIR"

echo "📦 打包压缩包..."
TMPDIR="$(mktemp -d)"
for plat in "${PLATFORMS[@]}"; do
  GOOS="${plat%%/*}"
  GOARCH="${plat##*/}"
  EXT=""
  if [ "$GOOS" = "windows" ]; then
    EXT=".exe"
  fi
  ALIAS="${ALIASES[$plat]}"
  BIN="${APP_NAME}-v${VERSION_TAG}-${ALIAS}${EXT}"
  STAGE="$TMPDIR/$ALIAS"
  mkdir -p "$STAGE"
  cp "$OUTPUT_DIR/$BIN" "$STAGE/$APP_NAME$EXT"
  cp README.md LICENSE "$STAGE/" 2>/dev/null || true
  if [ "$GOOS" = "windows" ]; then
    (cd "$TMPDIR" && zip -r "$ALIAS.zip" "$ALIAS" > /dev/null 2>&1)
    mv "$TMPDIR/$ALIAS.zip" "$OUTPUT_DIR/$BIN.zip"
    echo "  📦 $BIN.zip"
  else
    (cd "$TMPDIR" && tar czf "$ALIAS.tar.gz" "$ALIAS")
    mv "$TMPDIR/$ALIAS.tar.gz" "$OUTPUT_DIR/$BIN.tar.gz"
    echo "  📦 $BIN.tar.gz"
  fi
  rm -f "$OUTPUT_DIR/$BIN"
done
rm -rf "$TMPDIR"
echo "✅ 发布包已生成: $OUTPUT_DIR/"
ls -lh "$OUTPUT_DIR"
