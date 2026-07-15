# Releasing

The client is distributed as prebuilt binaries attached to a **GitHub Release**. The
`scripts/release.sh` script automates the whole process.

## Requirements

- `go`, `git` and [`gh`](https://cli.github.com) (GitHub CLI) authenticated (`gh auth status`).
- An `origin` remote pointing at the GitHub repo.
- A clean working tree (everything committed).

## Cut a release

```bash
scripts/release.sh vX.Y.Z
```

Example:

```bash
scripts/release.sh v0.1.0
```

The script, in order:

1. **Validates**: version format (`vX.Y.Z`, allows `-rc1`), clean tree, tag does not already
   exist, `gh` authenticated and `origin` remote present. Runs `go vet ./...`.
2. **Builds** the binary for 5 platforms (with `CGO_ENABLED=0`, `-trimpath`, and the version
   stamped via `-ldflags "-X main.version=vX.Y.Z"`):
   - `darwin/amd64`, `darwin/arm64` (macOS Intel and Apple Silicon)
   - `linux/amd64`, `linux/arm64`
   - `windows/amd64`
3. **Packages** each binary: `.tar.gz` on Unix, `.zip` on Windows, under `dist/`.
4. **Generates** `dist/SHA256SUMS` with the checksums.
5. **Tags**: creates an annotated tag `vX.Y.Z` and pushes it (`git push origin vX.Y.Z`).
6. **Publishes** the GitHub Release with `gh release create`, attaching all artifacts and
   auto-generating the notes (`--generate-notes`).

Artifacts are left in `dist/` (git-ignored). The published version is visible with:

```bash
mytunnel version   # → mytunnel vX.Y.Z
```

## Versioning

Follows [SemVer](https://semver.org): `vMAJOR.MINOR.PATCH`. The version is **not** stored in the
code — it is injected at build time from the tag. A hand-built binary (`go build`) reports `dev`.

## Installing a release (for users)

Download the archive for your platform from the releases page, verify the checksum, and extract
the binary:

```bash
# example: macOS Apple Silicon
VER=v0.1.0
curl -LO "https://github.com/onvibe-apps/mytunnel/releases/download/$VER/mytunnel-$VER-darwin-arm64.tar.gz"
curl -LO "https://github.com/onvibe-apps/mytunnel/releases/download/$VER/SHA256SUMS"
shasum -a 256 -c SHA256SUMS --ignore-missing
tar xzf "mytunnel-$VER-darwin-arm64.tar.gz"
./mytunnel setup        # configure endpoint + secret
./mytunnel --local 3000
```

> On macOS, if Gatekeeper blocks the unsigned binary:
> `xattr -d com.apple.quarantine ./mytunnel`.
