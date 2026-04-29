---
title: "Releasing"
description: "How Aileron releases are built and published"
---

Releases are automated with [GoReleaser](https://goreleaser.com/) and GitHub Actions. Pushing a version tag builds cross-platform binaries and creates a GitHub Release with notes grouped by conventional commit type.

## Creating a release

```sh
git tag -a v0.0.3 -m "Release v0.0.3"
git push origin v0.0.3
```

This produces:

- Binaries for `aileron`, `aileron-sh`, `aileron-server`, and `aileron-mcp` across Linux and macOS (Intel + Apple Silicon). `aileron` and `aileron-server` also build for Windows.
- `.tar.gz` archives (unix) and `.zip` archives (Windows)
- SHA256 checksums (`checksums.txt`)
- Release notes generated from conventional commits since the last tag

## Testing locally

To test the release pipeline without publishing:

```sh
task release:snapshot
```
