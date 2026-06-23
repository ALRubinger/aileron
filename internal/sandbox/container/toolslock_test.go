package container

import (
	"os"
	"strings"
	"testing"
)

// The committed lockfile must parse — package init already panics on a malformed
// embedded file, but this asserts it explicitly and that it yields a pin.
func TestEmbeddedToolsLockParses(t *testing.T) {
	node, err := parseToolsLock(toolsLock)
	if err != nil {
		t.Fatalf("parseToolsLock(embedded): %v", err)
	}
	if node.Version == "" {
		t.Fatal("embedded node version is empty")
	}
	if len(node.Checksums) == 0 {
		t.Fatal("embedded node checksums are empty")
	}
}

// The committed tools.lock.json must equal its canonical rendering, so the
// generator's output and the file in the repo can never drift (a hand-edit or a
// stale regenerate fails here).
func TestCommittedToolsLockIsCanonical(t *testing.T) {
	onDisk, err := os.ReadFile("tools.lock.json")
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	parsed, err := parseToolsLock(onDisk)
	if err != nil {
		t.Fatalf("parse lockfile: %v", err)
	}
	canonical, err := FormatToolsLock(parsed.Version, parsed.Checksums)
	if err != nil {
		t.Fatalf("FormatToolsLock: %v", err)
	}
	if string(onDisk) != string(canonical) {
		t.Fatalf("tools.lock.json is not canonical; run `task generate:tools-lock`.\n--- on disk ---\n%s\n--- canonical ---\n%s", onDisk, canonical)
	}
}

// Parse + lookup acceptance: every supported platform resolves the pinned
// checksum for the pinned version; a wrong version and an unsupported arch both
// resolve to ok=false with no panic. This exercises the GOOS/GOARCH->Node-token
// mapping that is the lookup's core behavior.
func TestPinnedNodeChecksumLookup(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch string
		token        string
	}{
		{"darwin", "amd64", "darwin-x64"},
		{"darwin", "arm64", "darwin-arm64"},
		{"linux", "amd64", "linux-x64"},
		{"linux", "arm64", "linux-arm64"},
		{"windows", "amd64", "win-x64"},
	} {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			sum, ok := pinnedNodeChecksum(pinnedNode.Version, tc.goos, tc.goarch)
			if !ok {
				t.Fatalf("pinnedNodeChecksum(%s, %s, %s) ok=false, want a pin", pinnedNode.Version, tc.goos, tc.goarch)
			}
			if want := pinnedNode.Checksums[tc.token]; sum != want {
				t.Fatalf("checksum = %q, want %q (token %q)", sum, want, tc.token)
			}
			if !isHexSHA256(sum) {
				t.Fatalf("checksum %q is not a hex sha256", sum)
			}
		})
	}

	t.Run("wrong version", func(t *testing.T) {
		if _, ok := pinnedNodeChecksum("0.0.0", "linux", "amd64"); ok {
			t.Fatal("a non-pinned version must not resolve a checksum")
		}
	})

	t.Run("unsupported arch", func(t *testing.T) {
		if _, ok := pinnedNodeChecksum(pinnedNode.Version, "linux", "riscv64"); ok {
			t.Fatal("an unsupported arch must not resolve a checksum")
		}
	})

	t.Run("unsupported os", func(t *testing.T) {
		if _, ok := pinnedNodeChecksum(pinnedNode.Version, "plan9", "amd64"); ok {
			t.Fatal("an unsupported os must not resolve a checksum")
		}
	})
}

// Checksum-mismatch-vs-lock acceptance / regression guard: VerifyNodeChecksum
// rejects a distribution whose sha256 disagrees with the lock (naming the
// platform and both hashes), accepts the recorded sha, and rejects an unpinned
// platform with an actionable error.
func TestVerifyNodeChecksum(t *testing.T) {
	v := pinnedNode.Version
	recorded := pinnedNode.Checksums["linux-x64"]
	if recorded == "" {
		t.Fatal("test precondition: linux-x64 must be pinned")
	}

	t.Run("matching sha passes", func(t *testing.T) {
		if err := VerifyNodeChecksum(v, "linux", "amd64", recorded); err != nil {
			t.Fatalf("VerifyNodeChecksum(matching) = %v, want nil", err)
		}
	})

	t.Run("matching sha is case-insensitive", func(t *testing.T) {
		if err := VerifyNodeChecksum(v, "linux", "amd64", strings.ToUpper(recorded)); err != nil {
			t.Fatalf("VerifyNodeChecksum(uppercased) = %v, want nil", err)
		}
	})

	t.Run("mismatched sha fails with actionable error", func(t *testing.T) {
		wrong := strings.Repeat("a", 64)
		err := VerifyNodeChecksum(v, "linux", "amd64", wrong)
		if err == nil {
			t.Fatal("VerifyNodeChecksum(wrong sha) = nil, want error")
		}
		msg := err.Error()
		for _, want := range []string{"linux-x64", wrong, recorded, "mismatch"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("error %q does not mention %q", msg, want)
			}
		}
	})

	t.Run("unpinned arch fails with actionable error", func(t *testing.T) {
		err := VerifyNodeChecksum(v, "linux", "riscv64", strings.Repeat("a", 64))
		if err == nil {
			t.Fatal("VerifyNodeChecksum(unsupported arch) = nil, want error")
		}
		if !strings.Contains(err.Error(), "unsupported platform") {
			t.Fatalf("error %q does not flag the unsupported platform", err)
		}
	})

	t.Run("wrong version fails naming the pinned version", func(t *testing.T) {
		err := VerifyNodeChecksum("0.0.0", "linux", "amd64", recorded)
		if err == nil {
			t.Fatal("VerifyNodeChecksum(wrong version) = nil, want error")
		}
		if !strings.Contains(err.Error(), v) {
			t.Fatalf("error %q does not name the pinned version %q", err, v)
		}
	})
}

func TestParseToolsLockRejects(t *testing.T) {
	valid := `{"node":{"version":"22.14.0","checksums":{"linux-x64":"` + strings.Repeat("a", 64) + `"}}}`

	t.Run("valid", func(t *testing.T) {
		node, err := parseToolsLock([]byte(valid))
		if err != nil {
			t.Fatalf("parse(valid) = %v", err)
		}
		if node.Version != "22.14.0" {
			t.Fatalf("version = %q", node.Version)
		}
	})

	t.Run("trims whitespace in checksum", func(t *testing.T) {
		body := `{"node":{"version":"22.14.0","checksums":{"linux-x64":"  ` + strings.Repeat("a", 64) + `  "}}}`
		node, err := parseToolsLock([]byte(body))
		if err != nil {
			t.Fatalf("parse = %v", err)
		}
		if node.Checksums["linux-x64"] != strings.Repeat("a", 64) {
			t.Fatalf("checksum not trimmed: %q", node.Checksums["linux-x64"])
		}
	})

	for name, body := range map[string]string{
		"non-hex checksum":      `{"node":{"version":"22.14.0","checksums":{"linux-x64":"` + strings.Repeat("g", 64) + `"}}}`,
		"uppercase checksum":    `{"node":{"version":"22.14.0","checksums":{"linux-x64":"` + strings.Repeat("A", 64) + `"}}}`,
		"wrong-length checksum": `{"node":{"version":"22.14.0","checksums":{"linux-x64":"abc123"}}}`,
		"empty checksum":        `{"node":{"version":"22.14.0","checksums":{"linux-x64":""}}}`,
		"missing version":       `{"node":{"checksums":{"linux-x64":"` + strings.Repeat("a", 64) + `"}}}`,
		"empty checksum map":    `{"node":{"version":"22.14.0","checksums":{}}}`,
		"no checksum field":     `{"node":{"version":"22.14.0"}}`,
		"malformed json":        `{"node":`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := parseToolsLock([]byte(body)); err == nil {
				t.Fatalf("parseToolsLock(%q) = nil error, want error", body)
			}
		})
	}
}

// FormatToolsLock must be deterministic: platform keys sorted, and the bytes
// round-trip back through parseToolsLock.
func TestFormatToolsLockRoundTripsAndSorts(t *testing.T) {
	sumA := strings.Repeat("a", 64)
	sumB := strings.Repeat("b", 64)
	data, err := FormatToolsLock("22.14.0", map[string]string{
		"win-x64":      sumB,
		"darwin-arm64": sumA,
	})
	if err != nil {
		t.Fatalf("FormatToolsLock: %v", err)
	}
	s := string(data)
	// Deterministic: darwin-arm64 (sorted first) must appear before win-x64.
	if di, wi := strings.Index(s, "darwin-arm64"), strings.Index(s, "win-x64"); di == -1 || wi == -1 || di > wi {
		t.Fatalf("keys not sorted in output:\n%s", s)
	}
	node, err := parseToolsLock(data)
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if node.Version != "22.14.0" || node.Checksums["darwin-arm64"] != sumA || node.Checksums["win-x64"] != sumB {
		t.Fatalf("round-trip mismatch: %+v", node)
	}
}
