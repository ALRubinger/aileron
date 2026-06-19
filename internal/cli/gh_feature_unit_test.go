package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ALRubinger/aileron/internal/auth/capture"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// TestGHFeatureUnitParsesAndMatchesDefaults is the load-bearing acceptance test
// for the gh devcontainer Feature authored under images/sandbox-features/gh/.
// It proves two contracts:
//
//  1. The customizations.aileron.cli block in gh's manifest parses into this
//     package's Unit type and validates, with the exact acquisition and sealing
//     values the brief specifies.
//  2. The Feature's unit is byte-for-byte in intent with the SHIPPED central
//     defaults it is destined to replace: internal/auth/capture/defaults/gh.yaml
//     (acquisition) and internal/proxybinding/defaults/github.yaml (sealing).
//     This is the regression guard that catches any future edit to either
//     source diverging from the Feature before the cutover sub-issue removes
//     them.
//
// It is a plain `go test` with no build tag: cheap, always-run, no Docker.
func TestGHFeatureUnitParsesAndMatchesDefaults(t *testing.T) {
	unit := parseGHFeatureUnit(t)

	// (1) Envelope identity and reserved-plane values.
	if unit.Name != "gh" {
		t.Fatalf("unit.Name = %q, want %q", unit.Name, "gh")
	}
	if unit.Key != "user/github" {
		t.Fatalf("unit.Key = %q, want %q", unit.Key, "user/github")
	}
	if unit.Presence == nil || unit.Presence.Builtin != "base" {
		t.Fatalf("unit.Presence = %+v, want builtin=base", unit.Presence)
	}
	if unit.Acquisition.Mode != AcquisitionModeDeviceFlow {
		t.Fatalf("unit.Acquisition.Mode = %q, want %q", unit.Acquisition.Mode, AcquisitionModeDeviceFlow)
	}

	// The derived kind is the first path segment of the key. ToCaptureDescriptor
	// is the canonical derivation; assert through it rather than re-deriving.
	desc, err := unit.ToCaptureDescriptor()
	if err != nil {
		t.Fatalf("unit.ToCaptureDescriptor: %v", err)
	}
	if desc.Kind != "user" {
		t.Fatalf("derived kind = %q, want %q", desc.Kind, "user")
	}
	if desc.StoreAt != "user/github" {
		t.Fatalf("derived store_at = %q, want %q", desc.StoreAt, "user/github")
	}

	// (2a) Acquisition matches the shipped capture default field-for-field.
	ghDefault := loadCaptureDefault(t)

	if !reflect.DeepEqual(unit.Acquisition.LoginCmd, ghDefault.LoginCmd) {
		t.Fatalf("login_cmd = %#v, want %#v (from gh.yaml)", unit.Acquisition.LoginCmd, ghDefault.LoginCmd)
	}
	if !reflect.DeepEqual(unit.Acquisition.TokenCmd, ghDefault.TokenCmd) {
		t.Fatalf("token_cmd = %#v, want %#v (from gh.yaml)", unit.Acquisition.TokenCmd, ghDefault.TokenCmd)
	}
	if unit.Acquisition.BrowserShim != ghDefault.BrowserShim {
		t.Fatalf("browser_shim = %q, want %q (from gh.yaml)", unit.Acquisition.BrowserShim, ghDefault.BrowserShim)
	}
	if unit.Acquisition.ContainerName != ghDefault.ContainerName {
		t.Fatalf("container_name = %q, want %q (from gh.yaml)", unit.Acquisition.ContainerName, ghDefault.ContainerName)
	}
	if unit.Acquisition.ConfigDir != ghDefault.ConfigDir {
		t.Fatalf("config_dir = %q, want %q (from gh.yaml; gh omits it)", unit.Acquisition.ConfigDir, ghDefault.ConfigDir)
	}
	// The converted descriptor must reproduce the shipped store_at and kind.
	if desc.StoreAt != ghDefault.StoreAt {
		t.Fatalf("derived store_at = %q, want %q (from gh.yaml)", desc.StoreAt, ghDefault.StoreAt)
	}
	if desc.Kind != ghDefault.Kind {
		t.Fatalf("derived kind = %q, want %q (from gh.yaml)", desc.Kind, ghDefault.Kind)
	}

	// (2b) Sealing matches the shipped proxybinding default field-for-field.
	githubDefault := loadProxyBindingDefault(t)
	entries, err := unit.ToSealingEntries()
	if err != nil {
		t.Fatalf("unit.ToSealingEntries: %v", err)
	}
	if len(entries) != len(githubDefault.Bindings) {
		t.Fatalf("sealing has %d bindings, want %d (from github.yaml)", len(entries), len(githubDefault.Bindings))
	}
	for i := range entries {
		got := entries[i]
		want := githubDefault.Bindings[i]
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("sealing[%d] = %#v, want %#v (from github.yaml)", i, got, want)
		}
	}

	// Spell out the load-bearing literals so a default edit that changes the
	// intent (not just the byte layout) is unmistakable in the failure.
	if entries[0].Host != "github.com" || entries[0].Scheme != "basic" ||
		entries[0].EmitMechanism != "inject" || entries[0].Username != "x-access-token" {
		t.Fatalf("github.com binding = %#v, want host=github.com scheme=basic emit=inject username=x-access-token", entries[0])
	}
	if entries[1].Host != "api.github.com" || entries[1].Scheme != "bearer" ||
		entries[1].EmitMechanism != "sentinel-swap" || entries[1].Sentinel == nil ||
		entries[1].Sentinel.Value != "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA" ||
		entries[1].Sentinel.Env != "GH_TOKEN" {
		t.Fatalf("api.github.com binding = %#v, want host=api.github.com scheme=bearer emit=sentinel-swap sentinel{value=ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA env=GH_TOKEN}", entries[1])
	}
	// The derived credential_ref must equal the key on every binding.
	for i := range entries {
		if entries[i].CredentialRef != "user/github" {
			t.Fatalf("sealing[%d].CredentialRef = %q, want %q (derived from key)", i, entries[i].CredentialRef, "user/github")
		}
	}
}

// parseGHFeatureUnit reads the gh Feature manifest, extracts the
// customizations.aileron.cli block, re-encodes it as YAML, and feeds it through
// the canonical Parse so the test exercises the same decode path a loader will.
func parseGHFeatureUnit(t *testing.T) Unit {
	t.Helper()
	manifestPath := filepath.Join(sandboxFeaturesContext(t), "gh", "devcontainer-feature.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}

	var manifest struct {
		Customizations struct {
			Aileron struct {
				CLI json.RawMessage `json:"cli"`
			} `json:"aileron"`
		} `json:"customizations"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("%s is not valid JSON: %v", manifestPath, err)
	}
	if len(manifest.Customizations.Aileron.CLI) == 0 {
		t.Fatalf("%s has no customizations.aileron.cli block", manifestPath)
	}

	// JSON is a subset of YAML, but routing through a generic map and re-marshal
	// to YAML mirrors how a host loader will hand the block to Parse and avoids
	// coupling the test to JSON-vs-YAML decode quirks.
	var generic map[string]any
	if err := json.Unmarshal(manifest.Customizations.Aileron.CLI, &generic); err != nil {
		t.Fatalf("decode customizations.aileron.cli: %v", err)
	}
	yamlBytes, err := yaml.Marshal(generic)
	if err != nil {
		t.Fatalf("re-marshal cli block to yaml: %v", err)
	}

	unit, err := Parse(yamlBytes)
	if err != nil {
		t.Fatalf("Parse(gh customizations.aileron.cli): %v\nblock:\n%s", err, yamlBytes)
	}
	return unit
}

// loadCaptureDefault parses the shipped capture default for gh.
func loadCaptureDefault(t *testing.T) capture.CaptureDescriptor {
	t.Helper()
	path := filepath.Join(repoRoot(t), "internal", "auth", "capture", "defaults", "gh.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	desc, err := capture.ParseCaptureDescriptor(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return desc
}

// loadProxyBindingDefault parses the shipped proxybinding default for github.
func loadProxyBindingDefault(t *testing.T) proxybinding.Descriptor {
	t.Helper()
	path := filepath.Join(repoRoot(t), "internal", "proxybinding", "defaults", "github.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	desc, err := proxybinding.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return desc
}

// sandboxFeaturesContext locates the repo-root images/sandbox-features
// directory by walking up from the test working directory, mirroring
// featuresContext in internal/sandbox/composition/features_test.go.
func sandboxFeaturesContext(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "images", "sandbox-features")
}

// repoRoot walks up from the test working directory to the repo root, located
// by the images/sandbox-features marker directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	start, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := start
	for {
		if info, err := os.Stat(filepath.Join(dir, "images", "sandbox-features")); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repo root (images/sandbox-features marker) not found from %s", start)
		}
		dir = parent
	}
}
