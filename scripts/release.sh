#!/usr/bin/env bash
#
# release.sh — cut a release of the tunnel client.
#
#   scripts/release.sh vX.Y.Z
#
# Cross-compiles the binary for the target platforms, archives each with a
# SHA256SUMS file, creates and pushes an annotated git tag, and publishes a
# GitHub release (via `gh`) with all artifacts attached.
#
# Requirements: go, git, gh (authenticated), a clean working tree, and an
# `origin` remote pointing at the GitHub repo.

set -euo pipefail

VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
  echo "usage: $0 vX.Y.Z" >&2
  exit 1
fi
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$ ]]; then
  echo "error: version must look like v1.2.3 (optionally v1.2.3-rc1)" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# ── Pre-flight checks ────────────────────────────────────────────────────────
command -v go  >/dev/null || { echo "error: go not found" >&2; exit 1; }
command -v gh  >/dev/null || { echo "error: gh (GitHub CLI) not found" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "error: gh not authenticated (run: gh auth login)" >&2; exit 1; }
git remote get-url origin >/dev/null 2>&1 || { echo "error: no 'origin' remote" >&2; exit 1; }

if [[ -n "$(git status --porcelain)" ]]; then
  echo "error: working tree is not clean — commit or stash first" >&2
  exit 1
fi
if git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null; then
  echo "error: tag $VERSION already exists" >&2
  exit 1
fi

echo "▶ vetting…"
go vet ./...

# ── Build matrix ─────────────────────────────────────────────────────────────
PLATFORMS=(
  darwin/amd64
  darwin/arm64
  linux/amd64
  linux/arm64
  windows/amd64
)

DIST="$REPO_ROOT/dist"
rm -rf "$DIST"
mkdir -p "$DIST"

LDFLAGS="-s -w -X main.version=${VERSION}"

for platform in "${PLATFORMS[@]}"; do
  os="${platform%/*}"
  arch="${platform#*/}"
  name="mytunnel-${VERSION}-${os}-${arch}"
  bin="mytunnel"
  [[ "$os" == "windows" ]] && bin="mytunnel.exe"

  echo "▶ building ${name}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/$bin" .

  if [[ "$os" == "windows" ]]; then
    (cd "$DIST" && zip -q "${name}.zip" "$bin" && rm "$bin")
  else
    (cd "$DIST" && tar czf "${name}.tar.gz" "$bin" && rm "$bin")
  fi
done

# ── Checksums ────────────────────────────────────────────────────────────────
echo "▶ checksums"
(
  cd "$DIST"
  if command -v shasum >/dev/null; then
    shasum -a 256 ./* > SHA256SUMS
  else
    sha256sum ./* > SHA256SUMS
  fi
)

# ── Tag + push ───────────────────────────────────────────────────────────────
echo "▶ tagging ${VERSION}"
git tag -a "$VERSION" -m "Release $VERSION"
git push origin "$VERSION"

# ── GitHub release ───────────────────────────────────────────────────────────
echo "▶ creating GitHub release"
gh release create "$VERSION" "$DIST"/* \
  --title "$VERSION" \
  --generate-notes

echo "✓ released $VERSION"
gh release view "$VERSION" --web >/dev/null 2>&1 || true
