package composition

import (
	"os"
	"strings"
	"testing"
)

// The committed lockfile must parse — package init already panics on a malformed
// embedded file, but this asserts it explicitly and that it yields a map.
func TestEmbeddedLockfileParses(t *testing.T) {
	images, err := parseAgentImageLock(agentImagesLock)
	if err != nil {
		t.Fatalf("parseAgentImageLock(embedded): %v", err)
	}
	if images == nil {
		t.Fatal("parsed images map is nil")
	}
}

// The committed agent-images.lock.json must equal its canonical rendering, so the
// generator's output and the file in the repo can never drift (a hand-edit or a
// stale regenerate fails here).
func TestCommittedLockfileIsCanonical(t *testing.T) {
	onDisk, err := os.ReadFile("agent-images.lock.json")
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	parsed, err := parseAgentImageLock(onDisk)
	if err != nil {
		t.Fatalf("parse lockfile: %v", err)
	}
	canonical, err := FormatAgentImageLock(parsed)
	if err != nil {
		t.Fatalf("FormatAgentImageLock: %v", err)
	}
	if string(onDisk) != string(canonical) {
		t.Fatalf("agent-images.lock.json is not canonical; run `task generate:agent-digests`.\n--- on disk ---\n%s\n--- canonical ---\n%s", onDisk, canonical)
	}
}

func TestParseAgentImageLock(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		images, err := parseAgentImageLock([]byte(`{"images":{"claude":"sha256:abc123","codex":"sha256:def456"}}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if images["claude"] != "sha256:abc123" {
			t.Fatalf("claude = %q", images["claude"])
		}
		if images["codex"] != "sha256:def456" {
			t.Fatalf("codex = %q", images["codex"])
		}
	})

	t.Run("trims whitespace in digest", func(t *testing.T) {
		images, err := parseAgentImageLock([]byte(`{"images":{"claude":"  sha256:abc123  "}}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if images["claude"] != "sha256:abc123" {
			t.Fatalf("claude = %q, want trimmed", images["claude"])
		}
	})

	t.Run("empty images", func(t *testing.T) {
		images, err := parseAgentImageLock([]byte(`{"images":{}}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(images) != 0 {
			t.Fatalf("images = %v, want empty", images)
		}
	})

	for name, body := range map[string]string{
		"missing sha256 prefix": `{"images":{"claude":"abc123"}}`,
		"bare prefix":           `{"images":{"claude":"sha256:"}}`,
		"empty digest":          `{"images":{"claude":""}}`,
		"malformed json":        `{"images":`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := parseAgentImageLock([]byte(body)); err == nil {
				t.Fatalf("parseAgentImageLock(%q) = nil error, want error", body)
			}
		})
	}
}

func TestFormatAgentImageLockRoundTripsAndSorts(t *testing.T) {
	data, err := FormatAgentImageLock(map[string]string{"codex": "sha256:ddd", "claude": "sha256:aaa"})
	if err != nil {
		t.Fatalf("FormatAgentImageLock: %v", err)
	}
	// Deterministic: claude (sorted first) must appear before codex.
	s := string(data)
	if ci, di := strings.Index(s, "claude"), strings.Index(s, "codex"); ci == -1 || di == -1 || ci > di {
		t.Fatalf("keys not sorted in output:\n%s", s)
	}
	images, err := parseAgentImageLock(data)
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if images["claude"] != "sha256:aaa" || images["codex"] != "sha256:ddd" {
		t.Fatalf("round-trip mismatch: %v", images)
	}
}

// pinnedDigest applies a pin only to release builds (imageTag == "latest"); a dev
// or empty version keeps the floating edge tag and is never pinned.
func TestPinnedDigest(t *testing.T) {
	restore := pinnedAgentDigests
	pinnedAgentDigests = map[string]string{"claude": "sha256:pinned"}
	t.Cleanup(func() { pinnedAgentDigests = restore })

	t.Run("release build with recorded pin", func(t *testing.T) {
		d, ok := pinnedDigest("claude", "0.4.0")
		if !ok || d != "sha256:pinned" {
			t.Fatalf("pinnedDigest(claude, 0.4.0) = %q, %v", d, ok)
		}
	})
	t.Run("dev build never pinned", func(t *testing.T) {
		if _, ok := pinnedDigest("claude", "dev"); ok {
			t.Fatal("dev build should not pin")
		}
		if _, ok := pinnedDigest("claude", ""); ok {
			t.Fatal("empty version should not pin")
		}
	})
	t.Run("release build, agent without a pin", func(t *testing.T) {
		if _, ok := pinnedDigest("codex", "0.4.0"); ok {
			t.Fatal("unrecorded agent should not pin")
		}
	})
}

// PublishedAgentImage returns a digest reference for a release build with a
// recorded pin, and the floating tag otherwise (preserving the pre-#1233 path).
func TestPublishedAgentImageDigestPin(t *testing.T) {
	restore := pinnedAgentDigests
	pinnedAgentDigests = map[string]string{"claude": "sha256:pinned"}
	t.Cleanup(func() { pinnedAgentDigests = restore })

	for _, tc := range []struct {
		name    string
		agent   string
		version string
		want    string
	}{
		{"release pinned -> digest", "claude", "0.4.0", "ghcr.io/alrubinger/aileron-sandbox-claude@sha256:pinned"},
		{"dev pinned-agent -> edge tag", "claude", "dev", "ghcr.io/alrubinger/aileron-sandbox-claude:edge"},
		{"release unpinned-agent -> latest tag", "codex", "0.4.0", "ghcr.io/alrubinger/aileron-sandbox-codex:latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PublishedAgentImage(tc.agent, tc.version); got != tc.want {
				t.Fatalf("PublishedAgentImage(%q, %q) = %q, want %q", tc.agent, tc.version, got, tc.want)
			}
		})
	}
}

func TestPublishedAgents(t *testing.T) {
	got := PublishedAgents()
	// Only publishable agents are listed. Claude is recipe'd but not publishable
	// (#1451), so it is excluded; codex (Apache-2.0) remains.
	want := []string{"codex"}
	if len(got) != len(want) {
		t.Fatalf("PublishedAgents() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PublishedAgents()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
