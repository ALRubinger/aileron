package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCaptureDescriptors_BuiltinOnlyYieldsGh(t *testing.T) {
	byName, ordered, err := LoadCaptureDescriptors(CaptureLoadOptions{})
	if err != nil {
		t.Fatalf("LoadCaptureDescriptors: %v", err)
	}
	gh, ok := byName["gh"]
	if !ok {
		t.Fatalf("built-in load missing gh; have %v", names(ordered))
	}
	if gh.ContainerName != "aileron-auth-github" {
		t.Errorf("gh ContainerName = %q", gh.ContainerName)
	}
	if gh.StoreAt != "user/github" || gh.Kind != "user" {
		t.Errorf("gh store_at/kind = %q/%q, want user/github + user", gh.StoreAt, gh.Kind)
	}
	if gh.BrowserShim != "echo" {
		t.Errorf("gh BrowserShim = %q, want echo", gh.BrowserShim)
	}
	if gh.ConfigDir != "" {
		t.Errorf("gh ConfigDir = %q, want empty (parity with bespoke flow)", gh.ConfigDir)
	}
}

func TestLoadCaptureDescriptors_UserLayerOverridesByName(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "capture-descriptors.yaml")
	override := `version: v1
name: gh
container_name: my-override-container
login_cmd: [gh, auth, login]
token_cmd: [gh, auth, token]
store_at: user/github
kind: user
`
	if err := os.WriteFile(userPath, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	byName, _, err := LoadCaptureDescriptors(CaptureLoadOptions{UserPath: userPath})
	if err != nil {
		t.Fatalf("LoadCaptureDescriptors: %v", err)
	}
	gh := byName["gh"]
	if gh.ContainerName != "my-override-container" {
		t.Errorf("ContainerName = %q, want the user override", gh.ContainerName)
	}
}

func TestLoadCaptureDescriptors_MissingUserFileContributesNothing(t *testing.T) {
	byName, _, err := LoadCaptureDescriptors(CaptureLoadOptions{
		UserPath: filepath.Join(t.TempDir(), "does-not-exist.yaml"),
	})
	if err != nil {
		t.Fatalf("missing user file should not error: %v", err)
	}
	if _, ok := byName["gh"]; !ok {
		t.Error("built-in gh should still load when the user file is absent")
	}
	if len(byName) != 1 {
		t.Errorf("descriptor count = %d, want 1 (built-in only)", len(byName))
	}
}

func TestLoadCaptureDescriptors_PresentButInvalidUserFileErrors(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "capture-descriptors.yaml")
	// Invalid: missing required store_at.
	bad := `version: v1
name: gh
container_name: c
login_cmd: [gh, auth, login]
token_cmd: [gh, auth, token]
kind: user
`
	if err := os.WriteFile(userPath, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadCaptureDescriptors(CaptureLoadOptions{UserPath: userPath})
	if err == nil {
		t.Fatal("expected an error for a present-but-invalid user file")
	}
	if !strings.Contains(err.Error(), "store_at") {
		t.Errorf("err = %v, want store_at validation context", err)
	}
}

func TestLoadCaptureDescriptors_OrderedDeterministically(t *testing.T) {
	_, ordered, err := LoadCaptureDescriptors(CaptureLoadOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].Name > ordered[i].Name {
			t.Errorf("ordered slice not sorted by name: %v", names(ordered))
		}
	}
}

func names(ds []CaptureDescriptor) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Name
	}
	return out
}
