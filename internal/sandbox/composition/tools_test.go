package composition

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveToolFeatureRef_CuratedTools proves every curated-catalog tool
// resolves to its published Feature reference, whatever (loose) version
// constraint the manifest declares.
func TestResolveToolFeatureRef_CuratedTools(t *testing.T) {
	cases := map[string]string{
		"aws-cli@2.x": "ghcr.io/alrubinger/aileron-features/aws-cli:0",
		"aws-cli@2":   "ghcr.io/alrubinger/aileron-features/aws-cli:0",
		"gh@2.19.1":   "ghcr.io/alrubinger/aileron-features/gh:0",
		"gh@2":        "ghcr.io/alrubinger/aileron-features/gh:0",
	}
	for ref, want := range cases {
		got, err := ResolveToolFeatureRef(ref)
		if err != nil {
			t.Errorf("ResolveToolFeatureRef(%q): %v", ref, err)
			continue
		}
		if got != want {
			t.Errorf("ResolveToolFeatureRef(%q) = %q, want %q", ref, got, want)
		}
	}
}

// TestResolveToolFeatureRef_UnknownToolRejected proves the name vocabulary is
// closed to the curated catalog: an unknown tool name is rejected with an
// error naming the known tools.
func TestResolveToolFeatureRef_UnknownToolRejected(t *testing.T) {
	_, err := ResolveToolFeatureRef("nmap@7.1")
	if err == nil {
		t.Fatal("an unknown tool name must be rejected")
	}
	if !strings.Contains(err.Error(), "curated catalog") {
		t.Errorf("error should cite the curated catalog, got: %v", err)
	}
	if !strings.Contains(err.Error(), "aws-cli") || !strings.Contains(err.Error(), "gh") {
		t.Errorf("error should name the known tools, got: %v", err)
	}
}

// TestResolveToolFeatureRef_MalformedRejected proves the <name>@<version>
// shape is enforced: a missing separator, empty name, or empty version is a
// hard error rather than a coerced lookup.
func TestResolveToolFeatureRef_MalformedRejected(t *testing.T) {
	for _, ref := range []string{"", "aws-cli", "@2.x", "aws-cli@", "@"} {
		if _, err := ResolveToolFeatureRef(ref); err == nil {
			t.Errorf("malformed tool reference %q must be rejected", ref)
		}
	}
}

// TestToolsPlan_SynthesizesDevcontainer proves the plan the composer builds
// from: TierDevcontainer, the Features map keyed by the given references, and
// a synthesized devcontainer.json (valid JSON) whose image is the base and
// whose features are exactly the given references.
func TestToolsPlan_SynthesizesDevcontainer(t *testing.T) {
	base := "ghcr.io/alrubinger/aileron-sandbox-base:latest@sha256:" + strings.Repeat("a", 64)
	refs := []string{
		"ghcr.io/alrubinger/aileron-features/gh:0",
		"ghcr.io/alrubinger/aileron-features/aws-cli:0",
	}
	plan := ToolsPlan(base, refs)
	if plan.Tier != TierDevcontainer {
		t.Errorf("Tier = %q, want %q", plan.Tier, TierDevcontainer)
	}
	if plan.BaseImage != base {
		t.Errorf("BaseImage = %q, want %q", plan.BaseImage, base)
	}
	if len(plan.Features) != 2 {
		t.Fatalf("Features = %v, want both references", plan.Features)
	}
	for _, ref := range refs {
		if string(plan.Features[ref]) != "{}" {
			t.Errorf("Features[%q] = %q, want {}", ref, plan.Features[ref])
		}
	}
	if plan.SynthesizedDevcontainer == "" {
		t.Fatal("SynthesizedDevcontainer must be set (no .devcontainer on disk)")
	}
	var cfg struct {
		Image    string                     `json:"image"`
		Features map[string]json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal([]byte(plan.SynthesizedDevcontainer), &cfg); err != nil {
		t.Fatalf("synthesized devcontainer is not valid JSON: %v\n%s", err, plan.SynthesizedDevcontainer)
	}
	if cfg.Image != base {
		t.Errorf("synthesized image = %q, want %q", cfg.Image, base)
	}
	if len(cfg.Features) != 2 {
		t.Errorf("synthesized features = %v, want both references", cfg.Features)
	}
	for _, ref := range refs {
		if _, ok := cfg.Features[ref]; !ok {
			t.Errorf("synthesized features missing %q", ref)
		}
	}
}

// TestToolsPlan_DeterministicTag proves the local image tag is deterministic
// for the same composition (declaration order does not matter) and distinct
// for distinct compositions.
func TestToolsPlan_DeterministicTag(t *testing.T) {
	base := "ghcr.io/alrubinger/aileron-sandbox-base:latest@sha256:" + strings.Repeat("a", 64)
	a := ToolsPlan(base, []string{"ghcr.io/x/aws-cli:0", "ghcr.io/x/gh:0"})
	b := ToolsPlan(base, []string{"ghcr.io/x/gh:0", "ghcr.io/x/aws-cli:0"})
	if a.Image != b.Image {
		t.Errorf("declaration order must not change the tag: %q vs %q", a.Image, b.Image)
	}
	if a.SynthesizedDevcontainer != b.SynthesizedDevcontainer {
		t.Error("declaration order must not change the synthesized devcontainer bytes")
	}
	if !strings.HasPrefix(a.Image, "aileron/") {
		t.Errorf("the composed tag must be a local-daemon aileron/ tag, got %q", a.Image)
	}

	c := ToolsPlan(base, []string{"ghcr.io/x/aws-cli:0"})
	if c.Image == a.Image {
		t.Errorf("distinct feature sets must produce distinct tags, both %q", c.Image)
	}
	d := ToolsPlan("other-base@sha256:"+strings.Repeat("b", 64), []string{"ghcr.io/x/aws-cli:0", "ghcr.io/x/gh:0"})
	if d.Image == a.Image {
		t.Errorf("distinct bases must produce distinct tags, both %q", d.Image)
	}
}

// TestOCILayoutDir_Deterministic proves a tag maps to a stable cache-dir path so
// freeze (write) and publish (read) compute byte-identical paths from one source.
func TestOCILayoutDir_Deterministic(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base) // Linux resolves UserCacheDir here
	t.Setenv("HOME", base)           // darwin resolves $HOME/Library/Caches

	tag := "aileron/sandbox-tools:abc123def4567890"
	a, err := OCILayoutDir(tag)
	if err != nil {
		t.Fatalf("OCILayoutDir: %v", err)
	}
	b, err := OCILayoutDir(tag)
	if err != nil {
		t.Fatalf("OCILayoutDir (2nd): %v", err)
	}
	if a != b {
		t.Errorf("path not deterministic: %q vs %q", a, b)
	}
	if !strings.Contains(a, filepath.Join("aileron", "freeze-oci-layouts")) {
		t.Errorf("path %q is not under aileron/freeze-oci-layouts", a)
	}
}

// TestOCILayoutDir_SlugsTagSeparators proves the `/` and `:` a tag carries but a
// path segment cannot are slugged to `_`, so the dir is a single path segment.
func TestOCILayoutDir_SlugsTagSeparators(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base)
	t.Setenv("HOME", base)

	got, err := OCILayoutDir("aileron/sandbox-tools:tagwith/slash:colon")
	if err != nil {
		t.Fatalf("OCILayoutDir: %v", err)
	}
	slug := filepath.Base(got)
	if strings.ContainsAny(slug, "/:") {
		t.Errorf("slug %q still contains a `/` or `:` separator", slug)
	}
	if slug != "aileron_sandbox-tools_tagwith_slash_colon" {
		t.Errorf("slug = %q, want the `/`+`:`->`_` replacement", slug)
	}
}

// TestOCILayoutDir_CacheDirError proves an unresolvable user cache dir surfaces as
// a wrapped error rather than a bogus path.
func TestOCILayoutDir_CacheDirError(t *testing.T) {
	// Clear every var os.UserCacheDir consults on the supported platforms so it
	// fails deterministically (no $HOME, no $XDG_CACHE_HOME).
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	if _, err := OCILayoutDir("aileron/sandbox-tools:x"); err == nil {
		t.Fatal("want an error when the user cache dir is unresolvable")
	}
}
