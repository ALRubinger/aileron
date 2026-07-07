package freeze

import "testing"

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
