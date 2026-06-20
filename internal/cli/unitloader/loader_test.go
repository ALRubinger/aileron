package unitloader

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ALRubinger/aileron/internal/auth/capture"
	"github.com/ALRubinger/aileron/internal/cli"
	"github.com/ALRubinger/aileron/internal/proxybinding"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// TestUnitsFromMetadata_GHFansOutToDefaults is the load-bearing contract
// test: a devcontainer.metadata array carrying gh's customizations.aileron.cli
// unit parses, projects to a capture-descriptor layer and a proxybinding-entry
// layer, and those layers are field-for-field equivalent to the SHIPPED
// embedded defaults the cutover sub-issue (#1323) will remove. This proves the
// unit-derived path is a faithful replacement for the central files before they
// are deleted, and is the regression guard against either source diverging.
func TestUnitsFromMetadata_GHFansOutToDefaults(t *testing.T) {
	metadata := readFixture(t, "gh-metadata.json")

	units, err := UnitsFromMetadata(metadata)
	if err != nil {
		t.Fatalf("UnitsFromMetadata: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	if units[0].Name != "gh" || units[0].Key != "user/github" {
		t.Fatalf("unit = name=%q key=%q, want name=gh key=user/github", units[0].Name, units[0].Key)
	}

	// (1) Capture layer matches the embedded gh.yaml default field-for-field.
	captureLayer, err := CaptureLayer(units)
	if err != nil {
		t.Fatalf("CaptureLayer: %v", err)
	}
	if len(captureLayer) != 1 {
		t.Fatalf("capture layer has %d descriptors, want 1", len(captureLayer))
	}
	ghDefault := loadCaptureDefault(t)
	if !reflect.DeepEqual(captureLayer[0], ghDefault) {
		t.Fatalf("capture descriptor = %#v\nwant (from gh.yaml) %#v", captureLayer[0], ghDefault)
	}

	// (2) Sealing layer matches the embedded github.yaml default field-for-field.
	sealingLayer, err := SealingLayer(units)
	if err != nil {
		t.Fatalf("SealingLayer: %v", err)
	}
	githubDefault := loadProxyBindingDefault(t)
	if len(sealingLayer) != len(githubDefault.Bindings) {
		t.Fatalf("sealing layer has %d entries, want %d (from github.yaml)", len(sealingLayer), len(githubDefault.Bindings))
	}
	for i := range sealingLayer {
		if !reflect.DeepEqual(sealingLayer[i], githubDefault.Bindings[i]) {
			t.Fatalf("sealing[%d] = %#v\nwant (from github.yaml) %#v", i, sealingLayer[i], githubDefault.Bindings[i])
		}
	}
}

// TestGHFixtureMatchesRealFeature guards the static gh-metadata.json fixture
// against drifting from the real gh devcontainer Feature manifest. It parses
// the cli block embedded in the fixture's gh element and the cli block from the
// authoritative images/sandbox-features/gh/devcontainer-feature.json, then
// asserts the two produce the same unit. If the Feature's unit changes, this
// fails until the fixture is refreshed, so the fan-out test keeps exercising
// the real shape.
func TestGHFixtureMatchesRealFeature(t *testing.T) {
	fixtureUnits, err := UnitsFromMetadata(readFixture(t, "gh-metadata.json"))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(fixtureUnits) != 1 {
		t.Fatalf("fixture produced %d units, want 1", len(fixtureUnits))
	}

	// The real Feature manifest is a single object, not the metadata array;
	// wrap it in a one-element array so the same loader path parses it.
	manifestPath := filepath.Join(repoRoot(t), "images", "sandbox-features", "gh", "devcontainer-feature.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}
	wrapped := append(append([]byte("["), manifest...), ']')
	realUnits, err := UnitsFromMetadata(wrapped)
	if err != nil {
		t.Fatalf("parse real gh feature: %v", err)
	}
	if len(realUnits) != 1 {
		t.Fatalf("real feature produced %d units, want 1", len(realUnits))
	}
	if !reflect.DeepEqual(fixtureUnits[0], realUnits[0]) {
		t.Fatalf("fixture unit diverged from real gh Feature:\nfixture=%#v\nreal=%#v", fixtureUnits[0], realUnits[0])
	}
}

// TestUnitsFromMetadata_NoOps proves the two clean no-op shapes: an array with
// no customizations.aileron block, and an empty array. Both yield nil units,
// nil error, and nil projected layers, so the consumers ship the embedded
// defaults alone.
func TestUnitsFromMetadata_NoOps(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
	}{
		{name: "elements without aileron block", fixture: "no-aileron.json"},
		{name: "empty array", fixture: "empty-array.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			units, err := UnitsFromMetadata(readFixture(t, tc.fixture))
			if err != nil {
				t.Fatalf("UnitsFromMetadata: unexpected error: %v", err)
			}
			if units != nil {
				t.Fatalf("units = %#v, want nil (clean no-op)", units)
			}
			captureLayer, err := CaptureLayer(units)
			if err != nil || captureLayer != nil {
				t.Fatalf("CaptureLayer = %#v, %v; want nil, nil", captureLayer, err)
			}
			sealingLayer, err := SealingLayer(units)
			if err != nil || sealingLayer != nil {
				t.Fatalf("SealingLayer = %#v, %v; want nil, nil", sealingLayer, err)
			}
		})
	}
}

// TestUnitsFromMetadata_EmptyLabelNoOp proves an empty label value (the ""
// sentinel ImageMetadataLabel returns for an unlabeled image) is a clean no-op.
func TestUnitsFromMetadata_EmptyLabelNoOp(t *testing.T) {
	units, err := UnitsFromMetadata(nil)
	if err != nil || units != nil {
		t.Fatalf("UnitsFromMetadata(nil) = %#v, %v; want nil, nil", units, err)
	}
	units, err = UnitsFromMetadata([]byte(""))
	if err != nil || units != nil {
		t.Fatalf("UnitsFromMetadata(\"\") = %#v, %v; want nil, nil", units, err)
	}
}

// TestUnitsFromMetadata_Errors proves a present-but-broken metadata fails
// loudly rather than silently shipping nothing: malformed JSON, and a present
// cli block that fails canonical unit validation.
func TestUnitsFromMetadata_Errors(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
	}{
		{name: "malformed json array", fixture: "malformed-json.json"},
		{name: "present but invalid unit", fixture: "malformed-unit.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			units, err := UnitsFromMetadata(readFixture(t, tc.fixture))
			if err == nil {
				t.Fatalf("UnitsFromMetadata = %#v, want error", units)
			}
			if units != nil {
				t.Fatalf("units = %#v on error, want nil", units)
			}
		})
	}
}

// TestLayersFromImage_GH proves the one-call convenience reads the image label
// through a fake runner and fans out to both layers equivalent to the embedded
// defaults.
func TestLayersFromImage_GH(t *testing.T) {
	metadata := readFixture(t, "gh-metadata.json")
	runner := runnerReturning(string(metadata))

	captureLayer, sealingLayer, err := LayersFromImage(context.Background(), runner, "docker", "img:test")
	if err != nil {
		t.Fatalf("LayersFromImage: %v", err)
	}
	ghDefault := loadCaptureDefault(t)
	if len(captureLayer) != 1 || !reflect.DeepEqual(captureLayer[0], ghDefault) {
		t.Fatalf("capture layer = %#v, want one descriptor equal to gh.yaml default", captureLayer)
	}
	githubDefault := loadProxyBindingDefault(t)
	if len(sealingLayer) != len(githubDefault.Bindings) {
		t.Fatalf("sealing layer has %d entries, want %d", len(sealingLayer), len(githubDefault.Bindings))
	}
	for i := range sealingLayer {
		if !reflect.DeepEqual(sealingLayer[i], githubDefault.Bindings[i]) {
			t.Fatalf("sealing[%d] = %#v, want %#v", i, sealingLayer[i], githubDefault.Bindings[i])
		}
	}
}

// TestLayersFromImage_ReadFailureNoOp proves a runner whose inspect fails
// degrades to nil layers, nil error (fail-soft, matching ImageMetadataLabel).
func TestLayersFromImage_ReadFailureNoOp(t *testing.T) {
	runner := runnerFunc(func(context.Context, string, []string, io.Writer, io.Writer) error {
		return errors.New("no such image")
	})
	captureLayer, sealingLayer, err := LayersFromImage(context.Background(), runner, "docker", "absent:img")
	if err != nil {
		t.Fatalf("LayersFromImage on inspect failure: unexpected error %v", err)
	}
	if captureLayer != nil || sealingLayer != nil {
		t.Fatalf("layers = %#v, %#v; want nil, nil (fail-soft no-op)", captureLayer, sealingLayer)
	}
}

// TestLayersFromImage_MalformedLabelErrors proves a present-but-malformed
// label fails construction loudly through the convenience path.
func TestLayersFromImage_MalformedLabelErrors(t *testing.T) {
	runner := runnerReturning(string(readFixture(t, "malformed-unit.json")))
	_, _, err := LayersFromImage(context.Background(), runner, "docker", "img:test")
	if err == nil {
		t.Fatal("LayersFromImage with malformed unit label = nil error, want error")
	}
}

// TestLayerProjectionErrors proves the two projection adapters surface a unit
// conversion error rather than dropping a descriptor or binding. A unit with a
// malformed key fails ToCaptureDescriptor and ToSealingEntries, and both
// CaptureLayer and SealingLayer propagate that error. This is the contract for
// callers that hand the adapters a unit not produced by the validated parse
// path.
func TestLayerProjectionErrors(t *testing.T) {
	// A key with no non-empty first segment is malformed and rejected by both
	// adapters' canonical derivation.
	bad := []cli.Unit{{
		Name: "broken",
		Key:  "/github",
		Sealing: []cli.Binding{{
			Host:   "github.com",
			Scheme: "bearer",
		}},
	}}

	if _, err := CaptureLayer(bad); err == nil {
		t.Error("CaptureLayer with a malformed-key unit = nil error, want error")
	}
	if _, err := SealingLayer(bad); err == nil {
		t.Error("SealingLayer with a malformed-key unit = nil error, want error")
	}
}

// --- helpers ---

type runnerFunc func(context.Context, string, []string, io.Writer, io.Writer) error

func (f runnerFunc) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	return f(ctx, name, args, stdout, stderr)
}

// runnerReturning builds a fake Runner that writes label as the image-inspect
// stdout, so ImageMetadataLabel reads it as the devcontainer.metadata value.
func runnerReturning(label string) sandboxcontainer.Runner {
	return runnerFunc(func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, label)
		return nil
	})
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// loadCaptureDefault parses the shipped capture default for gh, the embedded
// layer the unit-derived layer must reproduce.
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

// repoRoot walks up from the test working directory to the repo root, located
// by the images/sandbox-features marker directory, mirroring the locator in
// internal/cli/gh_feature_unit_test.go.
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
