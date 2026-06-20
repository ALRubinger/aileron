package capture

import (
	"os"
	"path/filepath"
	"reflect"
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

// validDescriptor builds a minimal valid CaptureDescriptor for the named
// tool, used to assemble in-memory extra and user layers in precedence tests.
func validDescriptor(name, container string) CaptureDescriptor {
	return CaptureDescriptor{
		Version:       CaptureSchemaVersion,
		Name:          name,
		ContainerName: container,
		LoginCmd:      []string{name, "auth", "login"},
		TokenCmd:      []string{name, "auth", "token"},
		StoreAt:       "user/" + name,
		Kind:          "user",
	}
}

// TestLoadCaptureDescriptors_ExtraLayerPrecedence pins the
// built-in < unit-derived < user ordering for the in-memory extra layer
// (#1322). An extra descriptor overrides a built-in of the same name, a user
// descriptor overrides the extra layer, and an unset extra layer reproduces
// the built-in-only result exactly.
func TestLoadCaptureDescriptors_ExtraLayerPrecedence(t *testing.T) {
	t.Run("extra overrides built-in of same name", func(t *testing.T) {
		extra := []CaptureDescriptor{validDescriptor("gh", "unit-derived-container")}
		byName, _, err := LoadCaptureDescriptors(CaptureLoadOptions{ExtraDescriptors: extra})
		if err != nil {
			t.Fatalf("LoadCaptureDescriptors: %v", err)
		}
		if byName["gh"].ContainerName != "unit-derived-container" {
			t.Errorf("gh ContainerName = %q, want the unit-derived override", byName["gh"].ContainerName)
		}
	})

	t.Run("user overrides extra of same name", func(t *testing.T) {
		dir := t.TempDir()
		userPath := filepath.Join(dir, "capture-descriptors.yaml")
		user := `version: v1
name: gh
container_name: user-container
login_cmd: [gh, auth, login]
token_cmd: [gh, auth, token]
store_at: user/github
kind: user
`
		if err := os.WriteFile(userPath, []byte(user), 0o600); err != nil {
			t.Fatal(err)
		}
		extra := []CaptureDescriptor{validDescriptor("gh", "unit-derived-container")}
		byName, _, err := LoadCaptureDescriptors(CaptureLoadOptions{
			ExtraDescriptors: extra,
			UserPath:         userPath,
		})
		if err != nil {
			t.Fatalf("LoadCaptureDescriptors: %v", err)
		}
		if byName["gh"].ContainerName != "user-container" {
			t.Errorf("gh ContainerName = %q, want the user override (highest precedence)", byName["gh"].ContainerName)
		}
	})

	t.Run("extra adds a new tool alongside built-in", func(t *testing.T) {
		extra := []CaptureDescriptor{validDescriptor("linear", "aileron-auth-linear")}
		byName, _, err := LoadCaptureDescriptors(CaptureLoadOptions{ExtraDescriptors: extra})
		if err != nil {
			t.Fatalf("LoadCaptureDescriptors: %v", err)
		}
		if _, ok := byName["gh"]; !ok {
			t.Error("built-in gh must remain present alongside the unit-derived layer")
		}
		if _, ok := byName["linear"]; !ok {
			t.Error("unit-derived linear must be added")
		}
	})

	t.Run("nil extra layer reproduces built-in-only set", func(t *testing.T) {
		base, _, err := LoadCaptureDescriptors(CaptureLoadOptions{})
		if err != nil {
			t.Fatalf("baseline load: %v", err)
		}
		withNil, _, err := LoadCaptureDescriptors(CaptureLoadOptions{ExtraDescriptors: nil})
		if err != nil {
			t.Fatalf("nil-extra load: %v", err)
		}
		if !reflect.DeepEqual(base, withNil) {
			t.Errorf("nil ExtraDescriptors changed the set:\nbase=%#v\nwithNil=%#v", base, withNil)
		}
	})
}
