package proxybinding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
)

func writeDescriptor(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func findEntry(entries []Entry, host string) (Entry, bool) {
	for _, e := range entries {
		if e.Host == host {
			return e, true
		}
	}
	return Entry{}, false
}

// Built-in-only load yields the shipped Linear entry: the embedded
// community profile is the floor with no override layers configured.
func TestLoad_BuiltinOnlyYieldsLinear(t *testing.T) {
	entries, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, ok := findEntry(entries, "api.linear.app")
	if !ok {
		t.Fatalf("built-in load missing api.linear.app; got %v", entries)
	}
	if e.Scheme != binding.SchemeHeaderTemplate {
		t.Errorf("linear scheme = %q, want header-template", e.Scheme)
	}
	if e.CredentialRef != "user/linear" {
		t.Errorf("linear credential_ref = %q, want user/linear", e.CredentialRef)
	}
}

// The user layer overrides a built-in host: redefining api.linear.app in
// the user descriptor wins.
func TestLoad_UserOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	userPath := writeDescriptor(t, dir, "user.yaml",
		"version: v1\nbindings:\n  - host: api.linear.app\n    credential_ref: user/linear-override\n    scheme: bearer\n")

	entries, err := Load(LoadOptions{UserPath: userPath})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, ok := findEntry(entries, "api.linear.app")
	if !ok {
		t.Fatal("missing api.linear.app after user override")
	}
	if e.Scheme != binding.SchemeBearer {
		t.Errorf("after user override scheme = %q, want bearer", e.Scheme)
	}
	if e.CredentialRef != "user/linear-override" {
		t.Errorf("credential_ref = %q, want user/linear-override", e.CredentialRef)
	}
}

// Precedence is built-in < project < user. The project layer overrides the
// built-in for a host, and the user layer overrides the project.
func TestLoad_PrecedenceOrder(t *testing.T) {
	dir := t.TempDir()
	projectPath := writeDescriptor(t, dir, "project.yaml",
		"version: v1\nbindings:\n  - host: api.linear.app\n    credential_ref: user/from-project\n    scheme: bearer\n")
	userPath := writeDescriptor(t, dir, "user.yaml",
		"version: v1\nbindings:\n  - host: api.linear.app\n    credential_ref: user/from-user\n    scheme: bearer\n")

	// Project overrides built-in (no user layer).
	entries, err := Load(LoadOptions{ProjectPath: projectPath})
	if err != nil {
		t.Fatalf("Load (project only): %v", err)
	}
	e, _ := findEntry(entries, "api.linear.app")
	if e.CredentialRef != "user/from-project" {
		t.Errorf("project-only credential_ref = %q, want user/from-project", e.CredentialRef)
	}

	// User overrides project.
	entries, err = Load(LoadOptions{ProjectPath: projectPath, UserPath: userPath})
	if err != nil {
		t.Fatalf("Load (project+user): %v", err)
	}
	e, _ = findEntry(entries, "api.linear.app")
	if e.CredentialRef != "user/from-user" {
		t.Errorf("project+user credential_ref = %q, want user/from-user (user wins)", e.CredentialRef)
	}
}

// Absent project/user files are not errors: an absent layer is an empty
// layer, leaving the built-in floor intact.
func TestLoad_AbsentLayersNoError(t *testing.T) {
	dir := t.TempDir()
	entries, err := Load(LoadOptions{
		ProjectPath: filepath.Join(dir, "does-not-exist-project.yaml"),
		UserPath:    filepath.Join(dir, "does-not-exist-user.yaml"),
	})
	if err != nil {
		t.Fatalf("Load with absent layers: %v", err)
	}
	if _, ok := findEntry(entries, "api.linear.app"); !ok {
		t.Error("absent override layers should leave built-in Linear entry")
	}
}

// An invalid layer fails the whole load with a clear error and does not
// silently drop entries.
func TestLoad_InvalidLayerFails(t *testing.T) {
	dir := t.TempDir()
	bad := writeDescriptor(t, dir, "bad.yaml",
		"version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: not-a-scheme\n")

	_, err := Load(LoadOptions{UserPath: bad})
	if err == nil {
		t.Fatal("Load with invalid layer = nil error, want error")
	}
	if !strings.Contains(err.Error(), "user layer") {
		t.Errorf("error %q should name the offending layer", err.Error())
	}
}

// An invalid project layer fails the load and names the project layer.
func TestLoad_InvalidProjectLayerFails(t *testing.T) {
	dir := t.TempDir()
	bad := writeDescriptor(t, dir, "bad-project.yaml",
		"version: v1\nbindings:\n  - host: api.example.com\n    credential_ref: user/example\n    scheme: not-a-scheme\n")

	_, err := Load(LoadOptions{ProjectPath: bad})
	if err == nil {
		t.Fatal("Load with invalid project layer = nil error, want error")
	}
	if !strings.Contains(err.Error(), "project layer") {
		t.Errorf("error %q should name the project layer", err.Error())
	}
}

// LoadHostBindings surfaces a load error rather than returning an empty
// (passthrough) table.
func TestLoadHostBindings_InvalidLayerErrors(t *testing.T) {
	dir := t.TempDir()
	bad := writeDescriptor(t, dir, "bad.yaml",
		"version: v2\nbindings: []\n")
	if _, err := LoadHostBindings(LoadOptions{UserPath: bad}); err == nil {
		t.Fatal("LoadHostBindings with invalid layer = nil error, want error")
	}
}

// A new host in an override layer is added on top of the built-in floor,
// not replacing it (override is per host key).
func TestLoad_NewHostAddedAcrossLayers(t *testing.T) {
	dir := t.TempDir()
	userPath := writeDescriptor(t, dir, "user.yaml",
		"version: v1\nbindings:\n  - host: api.other.com\n    credential_ref: user/other\n    scheme: bearer\n")

	entries, err := Load(LoadOptions{UserPath: userPath})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := findEntry(entries, "api.linear.app"); !ok {
		t.Error("built-in Linear entry dropped when user added a new host")
	}
	if _, ok := findEntry(entries, "api.other.com"); !ok {
		t.Error("user-added host missing")
	}
}

// LoadHostBindings adapts the merged set to a binding table whose Match
// resolves the loaded host.
func TestLoadHostBindings_MatchesLinear(t *testing.T) {
	table, err := LoadHostBindings(LoadOptions{})
	if err != nil {
		t.Fatalf("LoadHostBindings: %v", err)
	}
	hb, ok := table.Match("api.linear.app")
	if !ok {
		t.Fatal("table.Match(api.linear.app) = false, want true")
	}
	if hb.Scheme != binding.SchemeHeaderTemplate {
		t.Errorf("matched scheme = %q, want header-template", hb.Scheme)
	}
	if hb.CredentialRef != "user/linear" {
		t.Errorf("matched credential_ref = %q, want user/linear", hb.CredentialRef)
	}
}
