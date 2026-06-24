// Package nodedist fetches a pinned Node.js distribution by version and
// target platform from nodejs.org, verifies it against Node's GPG-signed
// SHASUMS256.txt, SHA-256-matches the downloaded archive, unpacks it, and
// caches the unpacked tree content-addressed under the shared
// atomic-rename store primitive (cstore.CommitDir).
//
// This package is the supply-chain-verified Node provisioning layer for the
// sandbox toolchain (issue #1525, sub-issue 1). It is self-contained and
// deliberately not wired into any build path here — the call site lands in a
// later sub-issue. The pipeline shape mirrors cstore's connector install:
//
//	fetch (cstore.Fetcher) → verify (ChecksumVerifier) → hash-match → unpack → cache (cstore.CommitDir)
//
// Trust property: the cache key is the archive's SHA-256 as published in
// Node's signed SHASUMS256.txt. The signature is checked first, then the
// download is matched against the signed hash, then the unpacked tree is
// stored under that same supply-chain-verified hash. A request whose
// verified hash is already cached short-circuits without re-downloading the
// archive.
package nodedist

import (
	"fmt"
	"strings"
)

// baseURL is the nodejs.org distribution root. Released artifacts for a
// version v<ver> live under baseURL + "v<ver>/".
const baseURL = "https://nodejs.org/dist/"

// Target identifies the platform a Node distribution is built for, using
// Go's GOOS/GOARCH vocabulary at the boundary. The mapping to Node's own
// os/arch naming (e.g. linux/amd64 → linux-x64) is internal to this
// package (see nodePlatform).
type Target struct {
	GOOS   string
	GOARCH string
}

// nodePlatform is Node's own (os, arch, archiveExt) naming for a Target.
type nodePlatform struct {
	os   string
	arch string
	ext  string // "tar.gz" on Unix, "zip" on Windows
}

// supportedTargets is the total, explicit GOOS/GOARCH → Node-platform
// mapping. Unsupported pairs fail fast (no silent fallback). This is kept
// minimal on purpose; broader arch resolution is sub-issue 2.
var supportedTargets = map[Target]nodePlatform{
	{GOOS: "linux", GOARCH: "amd64"}:   {os: "linux", arch: "x64", ext: "tar.gz"},
	{GOOS: "linux", GOARCH: "arm64"}:   {os: "linux", arch: "arm64", ext: "tar.gz"},
	{GOOS: "darwin", GOARCH: "amd64"}:  {os: "darwin", arch: "x64", ext: "tar.gz"},
	{GOOS: "darwin", GOARCH: "arm64"}:  {os: "darwin", arch: "arm64", ext: "tar.gz"},
	{GOOS: "windows", GOARCH: "amd64"}: {os: "win", arch: "x64", ext: "zip"},
	{GOOS: "windows", GOARCH: "arm64"}: {os: "win", arch: "arm64", ext: "zip"},
}

// resolve maps a Target to its Node platform naming, or returns a typed
// error for an unsupported pair.
func (t Target) resolve() (nodePlatform, error) {
	p, ok := supportedTargets[t]
	if !ok {
		return nodePlatform{}, &Error{
			Kind:    ErrUnsupportedTarget,
			Message: fmt.Sprintf("no Node distribution mapping for %s/%s", t.GOOS, t.GOARCH),
		}
	}
	return p, nil
}

// normalizeVersion strips a leading "v" from a version string so callers
// may pass either "24.2.0" or "v24.2.0". The empty version is rejected by
// the callers that build URLs.
func normalizeVersion(version string) string {
	return strings.TrimPrefix(version, "v")
}

// DistroName returns the top-level directory name a Node archive unpacks
// to, e.g. "node-v24.2.0-linux-x64". The unpack step strips this prefix.
func DistroName(version string, target Target) (string, error) {
	v := normalizeVersion(version)
	if v == "" {
		return "", &Error{Kind: ErrInvalidVersion, Message: "empty Node version"}
	}
	p, err := target.resolve()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("node-v%s-%s-%s", v, p.os, p.arch), nil
}

// ArchiveName returns the archive filename for a version+target, e.g.
// "node-v24.2.0-linux-x64.tar.gz" (Unix) or "node-v24.2.0-win-x64.zip"
// (Windows). This is the exact key that appears in SHASUMS256.txt.
func ArchiveName(version string, target Target) (string, error) {
	distro, err := DistroName(version, target)
	if err != nil {
		return "", err
	}
	p, _ := target.resolve() // already validated by DistroName
	return distro + "." + p.ext, nil
}

// URLs holds the three nodejs.org URLs needed to fetch and verify one
// distribution.
type URLs struct {
	// Archive is the platform-specific distribution archive.
	Archive string
	// Checksums is the SHASUMS256.txt covering every artifact for the
	// version. Resolved for reference; verification uses the clearsigned
	// Signature below, which embeds these checksums.
	Checksums string
	// Signature is the clearsigned SHASUMS256.txt.asc — a PGP "SIGNED MESSAGE"
	// that embeds the checksum lines inside the signed body (not a detached
	// signature over a separate file).
	Signature string
}

// ResolveURLs computes the archive, checksums, and signature URLs for a
// version+target. Returns a typed error for an empty version or an
// unsupported target.
func ResolveURLs(version string, target Target) (URLs, error) {
	archiveName, err := ArchiveName(version, target)
	if err != nil {
		return URLs{}, err
	}
	v := normalizeVersion(version)
	verRoot := baseURL + "v" + v + "/"
	return URLs{
		Archive:   verRoot + archiveName,
		Checksums: verRoot + "SHASUMS256.txt",
		Signature: verRoot + "SHASUMS256.txt.asc",
	}, nil
}
