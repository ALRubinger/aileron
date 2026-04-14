---
title: "Installation"
description: "Download and install Aileron"
---

Download the latest release for your platform from [GitHub Releases](https://github.com/ALRubinger/aileron/releases).

| Platform | CLI | Shell shim | Archive |
|----------|-----|-----------|---------|
| macOS (Apple Silicon) | `aileron` | `aileron-sh` | `aileron_*_darwin_arm64.tar.gz` |
| macOS (Intel) | `aileron` | `aileron-sh` | `aileron_*_darwin_amd64.tar.gz` |
| Linux (x86_64) | `aileron` | `aileron-sh` | `aileron_*_linux_amd64.tar.gz` |
| Windows (x86_64) | `aileron.exe` | -- | `aileron_*_windows_amd64.zip` |

Place both `aileron` and `aileron-sh` in the same directory on your PATH. The CLI looks for the shim next to itself first.

Each release also includes `aileron-server` and `aileron-mcp` archives.

Verify downloads against the `checksums.txt` file included in each release.
