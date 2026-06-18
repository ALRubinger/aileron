package composition

import (
	"strings"
	"testing"
)

func TestCacheVolumeNameDeterministic(t *testing.T) {
	a := CacheVolumeName("npm", "workspace-A")
	b := CacheVolumeName("npm", "workspace-A")
	if a != b {
		t.Fatalf("same (cli, identity) yielded different names: %q vs %q", a, b)
	}
}

func TestCacheVolumeNameDiffersByIdentity(t *testing.T) {
	a := CacheVolumeName("npm", "workspace-A")
	b := CacheVolumeName("npm", "workspace-B")
	if a == b {
		t.Fatalf("different identities collided to %q", a)
	}
}

func TestCacheVolumeNameDiffersByCLI(t *testing.T) {
	a := CacheVolumeName("npm", "id")
	b := CacheVolumeName("pip", "id")
	if a == b {
		t.Fatalf("different CLIs collided to %q", a)
	}
}

func TestCacheVolumeNameCarriesPrefixAndCLI(t *testing.T) {
	name := CacheVolumeName("npm", "id")
	if !strings.HasPrefix(name, CacheVolumePrefix+"-") {
		t.Fatalf("name %q missing prefix %q", name, CacheVolumePrefix)
	}
	if !strings.Contains(name, "-npm-") {
		t.Fatalf("name %q does not embed sanitized cli", name)
	}
}

func TestCacheVolumeNameDoesNotLeakIdentity(t *testing.T) {
	identity := "alice@example.com/secret-workspace"
	name := CacheVolumeName("npm", identity)
	if strings.Contains(name, "alice") || strings.Contains(name, "secret") || strings.Contains(name, "@") {
		t.Fatalf("identity leaked into volume name %q", name)
	}
}

func TestCacheVolumeNameSanitizesCLI(t *testing.T) {
	// An arbitrary CLI label must reduce to Docker's accepted character set.
	name := CacheVolumeName("My Weird/CLI!", "id")
	// Everything after the prefix and before the 12-hex digest must be a valid
	// volume-name segment: [a-z0-9_.-].
	if strings.ContainsAny(name, " /!@") {
		t.Fatalf("name %q contains characters Docker rejects in a volume name", name)
	}
}

func TestCacheVolumeNameEmptyCLICollapses(t *testing.T) {
	name := CacheVolumeName("   ", "id")
	// An empty/whitespace CLI must still produce a valid, non-empty name.
	if !strings.HasPrefix(name, CacheVolumePrefix+"-cli-") {
		t.Fatalf("empty cli did not collapse to a valid name: %q", name)
	}
}

func TestCacheVolumeNameDigestLength(t *testing.T) {
	name := CacheVolumeName("npm", "id")
	parts := strings.Split(name, "-")
	digest := parts[len(parts)-1]
	if len(digest) != 12 {
		t.Fatalf("digest %q is %d chars, want 12", digest, len(digest))
	}
}
