#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

TAG="${1:-${GITHUB_REF_NAME:-$(git describe --tags --exact-match 2>/dev/null || git describe --tags --always 2>/dev/null || echo dev)}}"
PREVIOUS_TAG="${2:-}"
REPO="${GITHUB_REPOSITORY:-qwdingyu/vs-ai-proxy}"

if [ -z "$PREVIOUS_TAG" ]; then
  PREVIOUS_TAG="$(git tag --sort=-creatordate | grep -v -F -x "$TAG" | head -n 1 || true)"
fi

VERSION="${TAG#v}"
COMPARE_URL=""
if [ -n "$PREVIOUS_TAG" ]; then
  COMPARE_URL="https://github.com/${REPO}/compare/${PREVIOUS_TAG}...${TAG}"
fi

cat <<NOTES
## vs-ai-proxy ${VERSION}

### Highlights
- See the changelog below for commits included in this release.
- Download the asset that matches your platform from the Assets section.

### Downloads
- macOS Apple Silicon: \`vs-ai-proxy-v${VERSION}-macos-arm64.tar.gz\`
- macOS Intel: \`vs-ai-proxy-v${VERSION}-macos-x64.tar.gz\`
- Linux x64: \`vs-ai-proxy-v${VERSION}-linux-x64.tar.gz\`
- Linux ARM64: \`vs-ai-proxy-v${VERSION}-linux-arm64.tar.gz\`
- Windows x64: \`vs-ai-proxy-v${VERSION}-windows-x64.exe.zip\`

### Verify
\`\`\`bash
vs-ai-proxy --version
\`\`\`

NOTES

if [ -n "$PREVIOUS_TAG" ]; then
  echo "### Changes since ${PREVIOUS_TAG}"
  echo
  if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
    git log --no-merges --pretty=format:'- %s (%h)' "${PREVIOUS_TAG}..${TAG}" || true
  else
    git log --no-merges --pretty=format:'- %s (%h)' "${PREVIOUS_TAG}..HEAD" || true
  fi
  echo
  echo
  echo "**Full changelog:** ${COMPARE_URL}"
else
  echo "### Changes"
  echo
  git log --no-merges --pretty=format:'- %s (%h)' "$TAG" || true
fi
