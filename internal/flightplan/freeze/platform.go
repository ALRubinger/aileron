package freeze

import (
	"os"
	"runtime"
)

// hostArchOverrideEnv lets a consumer running on one architecture select and
// verify a foreign architecture's child of a multi-arch composed artifact. When
// set to a non-empty GOARCH string (e.g. "arm64"), hostPlatform() reports that
// arch instead of runtime.GOARCH, so HostConfigDigest()/ConfigDigestFor pick the
// override arch's entry from a composed pin's per-arch set. This is legitimate
// operator behavior: an amd64 operator can pull and verify the arm64 child of a
// multi-arch plan it will hand to an arm64 host. It is also the one honest way
// to drive the cross-arch pull+verify path (issue #2025) end to end in a
// dedicated CI job whose runner is a single architecture, without running a
// foreign-arch aileron binary under QEMU. Empty (the default) selects the true
// host arch.
const hostArchOverrideEnv = "AILERON_FLIGHTPLAN_HOST_ARCH"

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
// GOARCH, unless the hostArchOverrideEnv env var names a foreign arch, in which
// case the consumer selects/verifies that arch's child of a multi-arch artifact.
func hostPlatform() (platformOS, arch string) {
	arch = hostGOARCH
	if override := os.Getenv(hostArchOverrideEnv); override != "" {
		arch = override
	}
	return composedOS, arch
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
