package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ALRubinger/aileron/internal/auth/capture"
	"github.com/ALRubinger/aileron/internal/cli"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// TestGHUnitDriftGuard is the cross-package drift guard for the gh CLI-unit
// cutover (#1323). After the central internal/auth/capture/defaults/gh.yaml
// and internal/proxybinding/defaults/github.yaml were deleted, gh's capture
// descriptor and sealing entries come only from its devcontainer Feature
// manifest. The capture and proxybinding internal tests can't import
// internal/cli or internal/cli/unitloader (import cycle), so they assert
// against in-package literals. This test lives in internal/app (which already
// imports the cli layer) and pins those exact literals to the live Feature
// manifest projection: if the shipped gh manifest changes, this fails until
// the documented literals (and the cycle-bound internal tests) are updated.
//
// This is the single live-manifest anchor for the byte-identical bar:
//
//	capture:  aileron-auth-github, login/token cmds, browser_shim=echo, no
//	          config_dir, store_at=user/github, kind=user
//	sealing:  github.com / basic / inject / x-access-token; api.github.com /
//	          bearer / sentinel-swap / GH_TOKEN / ghp_AILERONSENTINEL...
func TestGHUnitDriftGuard(t *testing.T) {
	unit := parseLiveGHUnit(t)

	// (1) Capture descriptor projection equals the documented literal the
	// capture package's gh_parity_test.go / loader_test.go assert against.
	gotDesc, err := unit.ToCaptureDescriptor()
	if err != nil {
		t.Fatalf("ToCaptureDescriptor: %v", err)
	}
	wantDesc := capture.CaptureDescriptor{
		Version:       capture.CaptureSchemaVersion,
		Name:          "gh",
		ContainerName: "aileron-auth-github",
		LoginCmd:      []string{"gh", "auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web"},
		TokenCmd:      []string{"gh", "auth", "token", "--hostname", "github.com"},
		BrowserShim:   "echo",
		StoreAt:       "user/github",
		Kind:          "user",
	}
	if !reflect.DeepEqual(gotDesc, wantDesc) {
		t.Fatalf("live gh capture descriptor drifted from the documented literal:\n got: %#v\nwant: %#v", gotDesc, wantDesc)
	}

	// (2) Sealing-entry projection equals the documented literal the
	// proxybinding/app/launch tests assert against.
	gotEntries, err := unit.ToSealingEntries()
	if err != nil {
		t.Fatalf("ToSealingEntries: %v", err)
	}
	wantEntries := []proxybinding.Entry{
		{
			Host:          "github.com",
			CredentialRef: "user/github",
			Scheme:        "basic",
			EmitMechanism: "inject",
			Username:      "x-access-token",
		},
		{
			Host:          "api.github.com",
			CredentialRef: "user/github",
			Scheme:        "bearer",
			EmitMechanism: "sentinel-swap",
			Sentinel: &proxybinding.Sentinel{
				Value: "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA",
				Env:   "GH_TOKEN",
			},
		},
	}
	if !reflect.DeepEqual(gotEntries, wantEntries) {
		t.Fatalf("live gh sealing entries drifted from the documented literal:\n got: %#v\nwant: %#v", gotEntries, wantEntries)
	}
}

// parseLiveGHUnit reads the authoritative gh Feature manifest, extracts the
// customizations.aileron.cli block, and parses it through the canonical
// cli.Parse, the same decode path the host loader uses.
func parseLiveGHUnit(t *testing.T) cli.Unit {
	t.Helper()
	manifestPath := filepath.Join(repoRootForDrift(t), "images", "sandbox-features", "gh", "devcontainer-feature.json")
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
	var generic map[string]any
	if err := json.Unmarshal(manifest.Customizations.Aileron.CLI, &generic); err != nil {
		t.Fatalf("decode customizations.aileron.cli: %v", err)
	}
	yamlBytes, err := yaml.Marshal(generic)
	if err != nil {
		t.Fatalf("re-marshal cli block to yaml: %v", err)
	}
	unit, err := cli.Parse(yamlBytes)
	if err != nil {
		t.Fatalf("cli.Parse(gh customizations.aileron.cli): %v\nblock:\n%s", err, yamlBytes)
	}
	return unit
}

// repoRootForDrift walks up from the test working directory to the repo root,
// located by the images/sandbox-features marker directory.
func repoRootForDrift(t *testing.T) string {
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
