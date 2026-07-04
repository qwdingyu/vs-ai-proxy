#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

usage() {
  cat <<'USAGE'
Usage: .bin/tag-release.sh <version> [--push]

Examples:
  .bin/tag-release.sh 0.2.13
  .bin/tag-release.sh v0.2.13 --push

Creates an annotated vX.Y.Z tag with normalized release notes.
Use --push to push the tag and trigger GitHub Actions release publishing.
USAGE
}

if [ "${1:-}" = "" ] || [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  usage
  exit 0
fi

RAW_VERSION="$1"
PUSH="${2:-}"
VERSION="${RAW_VERSION#v}"
TAG="v${VERSION}"

if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
  echo "error: version must look like 0.2.13 or v0.2.13" >&2
  exit 1
fi
if [ "$PUSH" != "" ] && [ "$PUSH" != "--push" ]; then
  usage >&2
  exit 1
fi
if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "error: working tree is dirty; commit or stash changes before tagging" >&2
  exit 1
fi
if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
  echo "error: tag ${TAG} already exists" >&2
  exit 1
fi

git fetch --tags --quiet || true
PREVIOUS_TAG="$(git tag --sort=-creatordate | grep -v -F -x "$TAG" | head -n 1 || true)"
NOTES_FILE="$(mktemp)"
trap 'rm -f "$NOTES_FILE"' EXIT

.bin/release-notes.sh "$TAG" "$PREVIOUS_TAG" > "$NOTES_FILE"
git tag -a "$TAG" -F "$NOTES_FILE"

echo "Created annotated tag ${TAG}"
if [ -n "$PREVIOUS_TAG" ]; then
  echo "Previous tag: ${PREVIOUS_TAG}"
fi

if [ "$PUSH" = "--push" ]; then
  git push origin "$TAG"
  echo "Pushed ${TAG}; GitHub Actions will publish the release."
else
  echo "Review with: git tag -n99 ${TAG}"
  echo "Push with: git push origin ${TAG}"
fi
