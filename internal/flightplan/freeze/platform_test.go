package freeze

import (
	"runtime"
	"testing"
)

// withHostGOARCH overrides the hostGOARCH test seam for the duration of fn, so a
// test can exercise both the present-arch and missing-arch selection branches
// deterministically regardless of the arch the test binary runs on.
func withHostGOARCH(t *testing.T, arch string) {
	t.Helper()
	prev := hostGOARCH
	hostGOARCH = arch
	t.Cleanup(func() { hostGOARCH = prev })
}

func TestHostPlatform(t *testing.T) {
	withHostGOARCH(t, "arm64")
	os, arch := hostPlatform()
	if os != "linux" {
		t.Errorf("hostPlatform os = %q, want linux (a container image is always linux)", os)
	}
	if arch != "arm64" {
		t.Errorf("hostPlatform arch = %q, want the host GOARCH arm64", arch)
	}
}

// TestHostPlatformArchOverride pins the contract of the AILERON_FLIGHTPLAN_HOST_ARCH
// consumer seam: unset, hostPlatform() reports the true runtime.GOARCH; set, it
// reports the override arch so a consumer selects a foreign arch's child of a
// multi-arch artifact. This is the seam the cross-arch e2e (#2038) drives to
// exercise the #2025 path without a foreign-arch binary under QEMU.
func TestHostPlatformArchOverride(t *testing.T) {
	t.Run("unset selects the true host arch", func(t *testing.T) {
		t.Setenv(hostArchOverrideEnv, "")
		os, arch := hostPlatform()
		if os != "linux" {
			t.Errorf("hostPlatform os = %q, want linux", os)
		}
		if arch != runtime.GOARCH {
			t.Errorf("hostPlatform arch = %q, want the true host GOARCH %q", arch, runtime.GOARCH)
		}
	})
	t.Run("override selects the foreign arch", func(t *testing.T) {
		t.Setenv(hostArchOverrideEnv, "arm64")
		os, arch := hostPlatform()
		if os != "linux" {
			t.Errorf("hostPlatform os = %q, want linux", os)
		}
		if arch != "arm64" {
			t.Errorf("hostPlatform arch = %q, want the override arm64", arch)
		}
	})
}

// TestHostConfigDigestArchOverrideSelectsForeignArch proves the override drives
// the whole consumer selection path: an amd64-defaulting host with the override
// set to arm64 selects the arm64 entry of a two-arch pin, and fails closed for a
// pin that lacks the override arch.
func TestHostConfigDigestArchOverrideSelectsForeignArch(t *testing.T) {
	// Force the true host arch to amd64 so the override to arm64 is unambiguously
	// a cross-arch selection regardless of the arch the test binary runs on.
	withHostGOARCH(t, "amd64")
	twoArch := ImagePin{
		LocalTag: "aileron/sandbox-tools:x",
		ConfigDigests: []PlatformDigest{
			{OS: "linux", Arch: "amd64", Digest: "sha256:aaa"},
			{OS: "linux", Arch: "arm64", Digest: "sha256:bbb"},
		},
	}
	t.Run("override selects the arm64 entry", func(t *testing.T) {
		t.Setenv(hostArchOverrideEnv, "arm64")
		got, platform, ok := twoArch.HostConfigDigest()
		if !ok || got != "sha256:bbb" {
			t.Errorf("HostConfigDigest with arm64 override = %q, %v; want sha256:bbb, true", got, ok)
		}
		if platform != "linux/arm64" {
			t.Errorf("platform = %q, want linux/arm64", platform)
		}
	})
	t.Run("override to an unbuilt arch fails closed", func(t *testing.T) {
		t.Setenv(hostArchOverrideEnv, "ppc64le")
		got, platform, ok := twoArch.HostConfigDigest()
		if ok {
			t.Errorf("HostConfigDigest with ppc64le override = %q, ok=true; want a fail-closed miss", got)
		}
		if platform != "linux/ppc64le" {
			t.Errorf("platform = %q, want linux/ppc64le for the fail-closed message", platform)
		}
	})
	t.Run("empty override falls back to the host arch", func(t *testing.T) {
		t.Setenv(hostArchOverrideEnv, "")
		got, platform, ok := twoArch.HostConfigDigest()
		if !ok || got != "sha256:aaa" {
			t.Errorf("HostConfigDigest with empty override = %q, %v; want the host amd64 entry sha256:aaa, true", got, ok)
		}
		if platform != "linux/amd64" {
			t.Errorf("platform = %q, want linux/amd64", platform)
		}
	})
}

func TestConfigDigestFor(t *testing.T) {
	pin := ImagePin{
		LocalTag: "aileron/sandbox-tools:x",
		ConfigDigests: []PlatformDigest{
			{OS: "linux", Arch: "amd64", Digest: "sha256:aaa"},
			{OS: "linux", Arch: "arm64", Digest: "sha256:bbb"},
		},
	}
	t.Run("hit", func(t *testing.T) {
		got, ok := pin.ConfigDigestFor("linux", "arm64")
		if !ok || got != "sha256:bbb" {
			t.Errorf("ConfigDigestFor(linux, arm64) = %q, %v; want sha256:bbb, true", got, ok)
		}
	})
	t.Run("miss on absent arch", func(t *testing.T) {
		if got, ok := pin.ConfigDigestFor("linux", "riscv64"); ok {
			t.Errorf("ConfigDigestFor(linux, riscv64) = %q, %v; want _, false", got, ok)
		}
	})
	t.Run("miss on wrong os", func(t *testing.T) {
		if _, ok := pin.ConfigDigestFor("windows", "amd64"); ok {
			t.Error("ConfigDigestFor(windows, amd64) = ok; a container image is linux-only")
		}
	})
	t.Run("empty set always misses", func(t *testing.T) {
		empty := ImagePin{LocalTag: "t"}
		if _, ok := empty.ConfigDigestFor("linux", "amd64"); ok {
			t.Error("a pin with an empty ConfigDigests set must report ok=false")
		}
	})
}

func TestHostConfigDigest(t *testing.T) {
	pin := ImagePin{
		LocalTag: "aileron/sandbox-tools:x",
		ConfigDigests: []PlatformDigest{
			{OS: "linux", Arch: "amd64", Digest: "sha256:aaa"},
			{OS: "linux", Arch: "arm64", Digest: "sha256:bbb"},
		},
	}
	t.Run("selects the host arch entry", func(t *testing.T) {
		withHostGOARCH(t, "amd64")
		got, platform, ok := pin.HostConfigDigest()
		if !ok || got != "sha256:aaa" {
			t.Errorf("HostConfigDigest = %q, %v; want sha256:aaa, true", got, ok)
		}
		if platform != "linux/amd64" {
			t.Errorf("platform = %q, want linux/amd64", platform)
		}
	})
	t.Run("fails closed when the host arch is absent", func(t *testing.T) {
		withHostGOARCH(t, "riscv64")
		got, platform, ok := pin.HostConfigDigest()
		if ok {
			t.Errorf("HostConfigDigest = %q, ok=true; want a miss for an unbuilt host arch", got)
		}
		// The human-facing platform is still named so the caller can report it.
		if platform != "linux/riscv64" {
			t.Errorf("platform = %q, want linux/riscv64 for the fail-closed message", platform)
		}
	})
	t.Run("empty set fails closed", func(t *testing.T) {
		withHostGOARCH(t, "amd64")
		empty := ImagePin{LocalTag: "t"}
		if _, _, ok := empty.HostConfigDigest(); ok {
			t.Error("a pin with an empty ConfigDigests set must fail closed")
		}
	})
}
