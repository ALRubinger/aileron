package freeze

import (
	"runtime"
)

// composedOS is the operating system a composed container image is always built
// for. `imgconfig.CanonicalConfig.OS` for a container image is `"linux"`
// regardless of the launching host GOOS (v4 supports macOS/Windows/Linux
// hosts), so the platform key that selects an entry from a composed pin's
// per-arch config-digest set is keyed on `(os="linux", arch=runtime.GOARCH)`,
// NOT the host GOOS. A literal GOOS match would never hit on a Mac.
const composedOS = "linux"

// hostGOARCH is the architecture the host selects a composed image for. It is a
// var (not a direct runtime.GOARCH read) so tests can exercise both the
// present-arch and missing-arch selection branches deterministically.
var hostGOARCH = runtime.GOARCH

// hostPlatform returns the `(os, arch)` platform key the freeze producer uses
// to key a composed pin's config-digest set and every consumer (launch boot
// re-check, publish verify, pull verify) uses to select from it. Producer and
// consumer share this one helper so they can never disagree on the platform
// vocabulary. The os is always composedOS ("linux"); the arch is the host
// GOARCH (the hostGOARCH seam), so a genuine host of a given arch selects and
// verifies that arch's child of a multi-arch artifact.
func hostPlatform() (platformOS, arch string) {
	return composedOS, hostGOARCH
}

// ConfigDigestFor returns the config content digest recorded for the given
// `(os, arch)` platform in a composed pin's per-arch set, and whether such an
// entry exists. A foreign-base pin (empty ConfigDigests) always reports
// ok=false.
func (p ImagePin) ConfigDigestFor(os, arch string) (digest string, ok bool) {
	for _, cd := range p.ConfigDigests {
		if cd.OS == os && cd.Arch == arch {
			return cd.Digest, true
		}
	}
	return "", false
}

// HostConfigDigest selects the config content digest for the current host
// platform (hostPlatform) from a composed pin's per-arch set. platform is the
// human-facing `os/arch` string for the host, so a caller can name it in a
// fail-closed error when ok is false (the artifact was not published for this
// host's platform). A foreign-base pin (empty ConfigDigests) reports ok=false.
func (p ImagePin) HostConfigDigest() (digest, platform string, ok bool) {
	os, arch := hostPlatform()
	platform = os + "/" + arch
	digest, ok = p.ConfigDigestFor(os, arch)
	return digest, platform, ok
}

// LocalBootConfigDigest selects the config content digest the #1863 local
// no-publish boot guard compares the daemon-resolved digest against. It exists
// because freeze builds the composed image twice — the multi-arch OCI layout
// (ConfigDigests, the publish/cross-machine pull identity) and a separate
// host-arch daemon-load build — whose non-reproducible composed layers can
// legitimately produce different config content digests. When freeze recorded
// the daemon-loaded image's digest (LocalHostConfigDigest set), the guard must
// compare against THAT so freeze -> launch on the same machine is self-consistent;
// ConfigDigests stays the published identity. When it is absent (an older freeze
// that predates the field), the guard falls back to the host ConfigDigests entry
// (HostConfigDigest), preserving the pre-field behavior.
//
// ok gates on the host ConfigDigests entry existing either way: a plan not built
// for this host's platform still fails closed (the LocalHostConfigDigest is only
// ever recorded alongside a host ConfigDigests entry, so a lock carrying the
// local field but no matching per-arch entry is malformed and correctly reports
// ok=false). platform is the human-facing `os/arch` for the fail-closed error.
func (p ImagePin) LocalBootConfigDigest() (digest, platform string, ok bool) {
	hostDigest, platform, ok := p.HostConfigDigest()
	if !ok {
		return "", platform, false
	}
	if p.LocalHostConfigDigest != "" {
		return p.LocalHostConfigDigest, platform, true
	}
	return hostDigest, platform, true
}
