package capture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistry_ResolveGh(t *testing.T) {
	// Post-#1323 gh ships in its devcontainer Feature CLI unit, not the
	// embedded built-in layer, so the registry sources it through the
	// unit-derived layer (ExtraDescriptors) the host projects from the image.
	r, err := NewRegistry(CaptureLoadOptions{
		ExtraDescriptors: []CaptureDescriptor{ghCaptureLiteral()},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d, ok := r.Resolve("gh")
	if !ok {
		t.Fatal("Resolve(gh) not ok; want the unit-derived gh descriptor")
	}
	if d.Name != "gh" {
		t.Errorf("Name = %q, want gh", d.Name)
	}
	if d.ContainerName != "aileron-auth-github" {
		t.Errorf("ContainerName = %q", d.ContainerName)
	}
	if d.StoreAt != "user/github" || d.Kind != "user" {
		t.Errorf("store_at/kind = %q/%q", d.StoreAt, d.Kind)
	}
}

func TestRegistry_ResolveUnknown(t *testing.T) {
	r, err := NewRegistry(CaptureLoadOptions{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, ok := r.Resolve("nope"); ok {
		t.Error("Resolve(nope) ok; want not-found for an unknown name")
	}
}

func TestRegistry_Names(t *testing.T) {
	// The empty built-in layer (post-#1323) lists no names; the unit-derived
	// layer supplies gh.
	empty, err := NewRegistry(CaptureLoadOptions{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := empty.Names(); len(got) != 0 {
		t.Errorf("built-in Names = %v, want empty", got)
	}

	r, err := NewRegistry(CaptureLoadOptions{
		ExtraDescriptors: []CaptureDescriptor{ghCaptureLiteral()},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got := r.Names()
	if len(got) != 1 || got[0] != "gh" {
		t.Errorf("Names = %v, want [gh]", got)
	}
}

func TestRegistry_UserOverrideResolves(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "capture-descriptors.yaml")
	override := `version: v1
name: gh
container_name: overridden
login_cmd: [gh, auth, login]
token_cmd: [gh, auth, token]
store_at: user/github
kind: user
`
	if err := os.WriteFile(userPath, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(CaptureLoadOptions{UserPath: userPath})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d, ok := r.Resolve("gh")
	if !ok {
		t.Fatal("Resolve(gh) not ok after override")
	}
	if d.ContainerName != "overridden" {
		t.Errorf("ContainerName = %q, want the user override", d.ContainerName)
	}
}

func TestRegistry_BindUnknownNameErrors(t *testing.T) {
	r, err := NewRegistry(CaptureLoadOptions{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := r.Bind("nope", "auto", "img", (&recordingStore{}).fn()); err == nil {
		t.Fatal("expected an error binding an unknown tool name")
	}
}

func TestRegistry_BindGhProducesConfiguredDriver(t *testing.T) {
	r, err := NewRegistry(CaptureLoadOptions{
		ExtraDescriptors: []CaptureDescriptor{ghCaptureLiteral()},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	store := (&recordingStore{}).fn()
	drv, err := r.Bind("gh", "auto", "ghcr.io/example/gh:latest", store)
	if err != nil {
		// New resolves a real runtime; skip where none is available rather
		// than asserting on the environment.
		t.Skipf("no container runtime resolvable in this environment: %v", err)
	}
	if drv.Runner == nil {
		t.Error("Bind left Runner unset; New should have defaulted it")
	}
	if drv.RuntimeExe == "" {
		t.Error("Bind left RuntimeExe unresolved")
	}
	if drv.Image != "ghcr.io/example/gh:latest" {
		t.Errorf("Image = %q, want the caller-resolved image", drv.Image)
	}
	if drv.ContainerName != "aileron-auth-github" {
		t.Errorf("ContainerName = %q", drv.ContainerName)
	}
	if drv.StoreAt != "user/github" || drv.Kind != "user" {
		t.Errorf("store_at/kind = %q/%q", drv.StoreAt, drv.Kind)
	}
	if drv.Store == nil {
		t.Error("Bind left Store unset")
	}
}

func TestDefaultRegistry_EmptyWithoutUnitOrUserLayer(t *testing.T) {
	// DefaultRegistry reads only the embedded built-in layer plus
	// ~/.aileron/capture-descriptors.yaml (absent in the test env). Post-#1323
	// the built-in layer is empty (gh moved to its Feature unit and is sourced
	// from the image at runtime via the unit-derived layer, which DefaultRegistry
	// does not consult), so with no user file the registry has no descriptors
	// and gh does not resolve. The host wires gh in through NewRegistry with the
	// projected ExtraDescriptors (exercised in TestRegistry_ResolveGh).
	t.Setenv("HOME", t.TempDir())
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	if names := r.Names(); len(names) != 0 {
		t.Errorf("DefaultRegistry names = %v, want empty (built-in layer empty, no user file)", names)
	}
	if _, ok := r.Resolve("gh"); ok {
		t.Error("DefaultRegistry resolved gh; gh now comes from the unit-derived layer, not the built-in defaults")
	}
}
