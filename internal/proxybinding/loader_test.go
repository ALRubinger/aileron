package proxybinding

import (
	"os"
	"path/filepath"
	"reflect"
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

// Precedence is built-in < user. The user layer overrides the built-in for
// a host.
func TestLoad_PrecedenceOrder(t *testing.T) {
	dir := t.TempDir()
	userPath := writeDescriptor(t, dir, "user.yaml",
		"version: v1\nbindings:\n  - host: api.linear.app\n    credential_ref: user/from-user\n    scheme: bearer\n")

	entries, err := Load(LoadOptions{UserPath: userPath})
	if err != nil {
		t.Fatalf("Load (user): %v", err)
	}
	e, _ := findEntry(entries, "api.linear.app")
	if e.CredentialRef != "user/from-user" {
		t.Errorf("user credential_ref = %q, want user/from-user (user wins)", e.CredentialRef)
	}
}

// An absent user file is not an error: an absent layer is an empty layer,
// leaving the built-in floor intact.
func TestLoad_AbsentLayersNoError(t *testing.T) {
	dir := t.TempDir()
	entries, err := Load(LoadOptions{
		UserPath: filepath.Join(dir, "does-not-exist-user.yaml"),
	})
	if err != nil {
		t.Fatalf("Load with absent layer: %v", err)
	}
	if _, ok := findEntry(entries, "api.linear.app"); !ok {
		t.Error("absent override layer should leave built-in Linear entry")
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

// TestLoad_ExtraLayerPrecedence pins the built-in < unit-derived < user
// ordering for the in-memory extra layer (#1322). An extra entry overrides a
// built-in for the same host, a user entry overrides the extra layer, and an
// unset extra layer reproduces the built-in-only table exactly.
func TestLoad_ExtraLayerPrecedence(t *testing.T) {
	t.Run("extra overrides built-in for same host", func(t *testing.T) {
		extra := []Entry{{
			Host:          "api.linear.app",
			CredentialRef: "user/from-unit",
			Scheme:        binding.SchemeBearer,
		}}
		entries, err := Load(LoadOptions{ExtraEntries: extra})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		e, ok := findEntry(entries, "api.linear.app")
		if !ok {
			t.Fatal("missing api.linear.app")
		}
		if e.CredentialRef != "user/from-unit" {
			t.Errorf("credential_ref = %q, want user/from-unit (unit-derived override)", e.CredentialRef)
		}
	})

	t.Run("user overrides extra for same host", func(t *testing.T) {
		dir := t.TempDir()
		userPath := writeDescriptor(t, dir, "user.yaml",
			"version: v1\nbindings:\n  - host: api.linear.app\n    credential_ref: user/from-user\n    scheme: bearer\n")
		extra := []Entry{{
			Host:          "api.linear.app",
			CredentialRef: "user/from-unit",
			Scheme:        binding.SchemeBearer,
		}}
		entries, err := Load(LoadOptions{ExtraEntries: extra, UserPath: userPath})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		e, _ := findEntry(entries, "api.linear.app")
		if e.CredentialRef != "user/from-user" {
			t.Errorf("credential_ref = %q, want user/from-user (user is highest precedence)", e.CredentialRef)
		}
	})

	t.Run("extra adds a new host alongside built-in", func(t *testing.T) {
		extra := []Entry{{
			Host:          "github.com",
			CredentialRef: "user/github",
			Scheme:        binding.SchemeBasic,
			Username:      "x-access-token",
		}}
		entries, err := Load(LoadOptions{ExtraEntries: extra})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if _, ok := findEntry(entries, "api.linear.app"); !ok {
			t.Error("built-in linear must remain present alongside the unit-derived layer")
		}
		if _, ok := findEntry(entries, "github.com"); !ok {
			t.Error("unit-derived github.com must be added")
		}
	})

	t.Run("nil extra layer reproduces built-in-only table", func(t *testing.T) {
		base, err := Load(LoadOptions{})
		if err != nil {
			t.Fatalf("baseline load: %v", err)
		}
		withNil, err := Load(LoadOptions{ExtraEntries: nil})
		if err != nil {
			t.Fatalf("nil-extra load: %v", err)
		}
		if !reflect.DeepEqual(base, withNil) {
			t.Errorf("nil ExtraEntries changed the table:\nbase=%#v\nwithNil=%#v", base, withNil)
		}
	})
}
