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

// TestHostPlatformReflectsHostGOARCH pins the contract that hostPlatform() reports
// (linux, hostGOARCH): the os is always the composed-image "linux", the arch is
// whatever the hostGOARCH seam holds. A genuine host of a given arch (e.g. an
// arm64 machine) selects that arch's child of a multi-arch artifact naturally; the
// seam lets a test drive both the native and a foreign arch deterministically
// regardless of the test binary's arch, which is how the foreign-arch #2025 case
// is covered (#2038) — the e2e itself runs host-native.
func TestHostPlatformReflectsHostGOARCH(t *testing.T) {
	t.Run("reports the native host arch", func(t *testing.T) {
		withHostGOARCH(t, runtime.GOARCH)
		os, arch := hostPlatform()
		if os != "linux" {
			t.Errorf("hostPlatform os = %q, want linux", os)
		}
		if arch != runtime.GOARCH {
			t.Errorf("hostPlatform arch = %q, want the host GOARCH %q", arch, runtime.GOARCH)
		}
	})
	t.Run("reports a foreign host arch", func(t *testing.T) {
		withHostGOARCH(t, "arm64")
		os, arch := hostPlatform()
		if os != "linux" {
			t.Errorf("hostPlatform os = %q, want linux", os)
		}
		if arch != "arm64" {
			t.Errorf("hostPlatform arch = %q, want the seam arch arm64", arch)
		}
	})
}

// TestHostConfigDigestSelectsForeignArch proves the arch seam drives the whole
// consumer selection path: an amd64 host selects the arm64 entry of a two-arch pin
// when it runs as arm64, and fails closed for a pin that lacks the running arch.
func TestHostConfigDigestSelectsForeignArch(t *testing.T) {
	twoArch := ImagePin{
		LocalTag: "aileron/sandbox-tools:x",
		ConfigDigests: []PlatformDigest{
			{OS: "linux", Arch: "amd64", Digest: "sha256:aaa"},
			{OS: "linux", Arch: "arm64", Digest: "sha256:bbb"},
		},
	}
	t.Run("an arm64 host selects the arm64 entry", func(t *testing.T) {
		withHostGOARCH(t, "arm64")
		got, platform, ok := twoArch.HostConfigDigest()
		if !ok || got != "sha256:bbb" {
			t.Errorf("HostConfigDigest as arm64 = %q, %v; want sha256:bbb, true", got, ok)
		}
		if platform != "linux/arm64" {
			t.Errorf("platform = %q, want linux/arm64", platform)
		}
	})
	t.Run("an unbuilt host arch fails closed", func(t *testing.T) {
		withHostGOARCH(t, "ppc64le")
		got, platform, ok := twoArch.HostConfigDigest()
		if ok {
			t.Errorf("HostConfigDigest as ppc64le = %q, ok=true; want a fail-closed miss", got)
		}
		if platform != "linux/ppc64le" {
			t.Errorf("platform = %q, want linux/ppc64le for the fail-closed message", platform)
		}
	})
	t.Run("an amd64 host selects the amd64 entry", func(t *testing.T) {
		withHostGOARCH(t, "amd64")
		got, platform, ok := twoArch.HostConfigDigest()
		if !ok || got != "sha256:aaa" {
			t.Errorf("HostConfigDigest as amd64 = %q, %v; want the amd64 entry sha256:aaa, true", got, ok)
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

// TestLocalBootConfigDigest is the #2138 local-boot selector: it returns the
// recorded daemon-loaded digest (LocalHostConfigDigest) when present so the local
// no-publish boot compares against the daemon image rather than the OCI-layout
// ConfigDigests[host], falls back to the host ConfigDigests entry when the field
// is absent (older lock), and stays fail-closed when the host arch is unbuilt.
func TestLocalBootConfigDigest(t *testing.T) {
	base := ImagePin{
		LocalTag: "aileron/sandbox-tools:x",
		ConfigDigests: []PlatformDigest{
			{OS: "linux", Arch: "amd64", Digest: "sha256:aaa"},
			{OS: "linux", Arch: "arm64", Digest: "sha256:bbb"},
		},
	}
	t.Run("prefers LocalHostConfigDigest when set", func(t *testing.T) {
		withHostGOARCH(t, "amd64")
		pin := base
		pin.LocalHostConfigDigest = "sha256:daemon"
		got, platform, ok := pin.LocalBootConfigDigest()
		if !ok || got != "sha256:daemon" {
			t.Errorf("LocalBootConfigDigest = %q, %v; want the daemon digest sha256:daemon, true", got, ok)
		}
		if platform != "linux/amd64" {
			t.Errorf("platform = %q, want linux/amd64", platform)
		}
	})
	t.Run("falls back to the host ConfigDigests entry when the field is absent", func(t *testing.T) {
		withHostGOARCH(t, "amd64")
		got, _, ok := base.LocalBootConfigDigest()
		if !ok || got != "sha256:aaa" {
			t.Errorf("LocalBootConfigDigest = %q, %v; want the OCI-layout amd64 entry sha256:aaa, true", got, ok)
		}
	})
	t.Run("fails closed when the host arch is unbuilt even with a local field", func(t *testing.T) {
		withHostGOARCH(t, "riscv64")
		pin := base
		pin.LocalHostConfigDigest = "sha256:daemon"
		got, platform, ok := pin.LocalBootConfigDigest()
		if ok {
			t.Errorf("LocalBootConfigDigest = %q, ok=true; want a fail-closed miss when the host arch is unbuilt", got)
		}
		if platform != "linux/riscv64" {
			t.Errorf("platform = %q, want linux/riscv64 for the fail-closed message", platform)
		}
	})
}
