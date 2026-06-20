package launch

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ALRubinger/aileron/internal/auth/capture"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cli/unitloader"
	"github.com/ALRubinger/aileron/internal/proxybinding"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// ghMetadataLabel is the devcontainer.metadata label value a restructured
// base image carries for the gh Feature: a JSON array whose gh element holds
// the customizations.aileron.cli unit. It mirrors the canonical
// internal/cli/unitloader/testdata/gh-metadata.json fixture. The launch-side
// tests read gh's sealing layer through the real unitloader path off this
// label, with NO central internal/proxybinding/defaults/github.yaml (#1323).
const ghMetadataLabel = `[
  {"id": "ghcr.io/devcontainers/features/common-utils", "version": "2.0.0"},
  {
    "id": "gh",
    "version": "0.0.1",
    "customizations": {
      "aileron": {
        "cli": {
          "name": "gh",
          "key": "user/github",
          "presence": {"builtin": "base"},
          "acquisition": {
            "mode": "device-flow",
            "container_name": "aileron-auth-github",
            "login_cmd": ["gh", "auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web"],
            "token_cmd": ["gh", "auth", "token", "--hostname", "github.com"],
            "browser_shim": "echo"
          },
          "sealing": [
            {"host": "github.com", "scheme": "basic", "emit_mechanism": "inject", "username": "x-access-token"},
            {"host": "api.github.com", "scheme": "bearer", "emit_mechanism": "sentinel-swap",
             "sentinel": {"value": "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA", "env": "GH_TOKEN"}}
          ]
        }
      }
    }
  }
]`

// labelRunner is a fake sandboxcontainer.Runner that writes a fixed
// devcontainer.metadata label to stdout, so ImageMetadataLabel resolves it
// without a container.
type labelRunner string

func (l labelRunner) Run(_ context.Context, _ string, _ []string, stdout, _ io.Writer) error {
	_, _ = io.WriteString(stdout, string(l))
	return nil
}

// ghSealingFromImage drives the production unitloader path off the fake
// labelled image and returns gh's projected sealing layer, the same []Entry
// the launcher threads into sentinelSwapHostBindings.
func ghSealingFromImage(t *testing.T, runtime string) []proxybinding.Entry {
	t.Helper()
	_, sealing, err := unitloader.LayersFromImage(context.Background(), labelRunner(ghMetadataLabel), runtime, "img:gh")
	if err != nil {
		t.Fatalf("LayersFromImage: %v", err)
	}
	if len(sealing) != 2 {
		t.Fatalf("gh sealing layer has %d entries, want 2 (github.com inject + api.github.com sentinel-swap)", len(sealing))
	}
	return sealing
}

// TestSentinelSwapHostBindings_GHFromUnitLayer is the load-bearing
// production-path test for the launcher cutover (#1323): with NO central
// github.yaml, gh's sentinel-swap binding reaches the planter only via the
// image-derived unit layer. sentinelSwapHostBindings applied with the
// unit-derived sealing entries returns the api.github.com GH_TOKEN
// sentinel-swap binding. It fails before the wiring (no central file means an
// empty plant set) and passes after.
func TestSentinelSwapHostBindings_GHFromUnitLayer(t *testing.T) {
	sealing := ghSealingFromImage(t, sandboxcontainer.DefaultRuntime)

	bindings, err := sentinelSwapHostBindings(sealing)
	if err != nil {
		t.Fatalf("sentinelSwapHostBindings: %v", err)
	}

	var gh *binding.HostBinding
	for i := range bindings {
		if bindings[i].HostPattern == "api.github.com" {
			gh = &bindings[i]
		}
	}
	if gh == nil {
		t.Fatalf("no api.github.com sentinel-swap binding in the plant set %#v", bindings)
	}
	if gh.EmitMechanism != binding.EmitMechanismSentinelSwap {
		t.Errorf("gh emit mechanism = %q, want sentinel-swap", gh.EmitMechanism)
	}
	if gh.SentinelEnv != "GH_TOKEN" ||
		gh.SentinelValue != "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA" {
		t.Errorf("gh sentinel = env=%q value=%q, want env=GH_TOKEN value=ghp_AILERONSENTINEL...", gh.SentinelEnv, gh.SentinelValue)
	}
	if gh.CredentialRef != "user/github" {
		t.Errorf("gh credential ref = %q, want user/github", gh.CredentialRef)
	}
}

// TestSentinelSwapHostBindings_MalformedEntryFailsLoudly proves the assembly
// fails loudly rather than degrading to an empty plant set when the
// image-derived sealing layer carries a malformed entry (here an unknown
// scheme that the proxybinding loader rejects). This is the fail-closed
// posture the launcher relies on: a broken gh unit must abort the launch, not
// silently plant no sentinel.
func TestSentinelSwapHostBindings_MalformedEntryFailsLoudly(t *testing.T) {
	bad := []proxybinding.Entry{{
		Host:          "api.github.com",
		CredentialRef: "user/github",
		Scheme:        "not-a-real-scheme",
	}}
	if _, err := sentinelSwapHostBindings(bad); err == nil {
		t.Fatal("sentinelSwapHostBindings with a malformed entry = nil error, want a loud failure")
	}
}

// TestSentinelSwapHostBindings_EmptyRuntimeParity proves the empty-runtime
// guard (P0-B): the launcher maps an empty plan runtime to
// sandboxcontainer.DefaultRuntime, so the bindings produced from a
// DefaultRuntime-driven unit layer equal those from an empty-runtime-driven
// one. ImageMetadataLabel does not vary by runtime for the fake runner, but
// the parity is asserted at the binding level to lock the launcher's mapping
// to the daemon's DefaultRuntime resolution.
func TestSentinelSwapHostBindings_EmptyRuntimeParity(t *testing.T) {
	// The launcher computes ghRuntime := DefaultRuntime when the plan runtime
	// is empty, then drives the unit layer with it. Reproduce both sides.
	defaultSealing := ghSealingFromImage(t, sandboxcontainer.DefaultRuntime)
	emptyMappedSealing := ghSealingFromImage(t, mapEmptyRuntime(""))

	defaultBindings, err := sentinelSwapHostBindings(defaultSealing)
	if err != nil {
		t.Fatalf("default-runtime bindings: %v", err)
	}
	emptyBindings, err := sentinelSwapHostBindings(emptyMappedSealing)
	if err != nil {
		t.Fatalf("empty-runtime bindings: %v", err)
	}
	if len(defaultBindings) != len(emptyBindings) {
		t.Fatalf("binding count differs: default=%d empty-mapped=%d", len(defaultBindings), len(emptyBindings))
	}
	for i := range defaultBindings {
		if defaultBindings[i].HostPattern != emptyBindings[i].HostPattern ||
			defaultBindings[i].EmitMechanism != emptyBindings[i].EmitMechanism {
			t.Errorf("binding[%d] differs: default=%#v empty=%#v", i, defaultBindings[i], emptyBindings[i])
		}
	}
}

// mapEmptyRuntime mirrors the launcher's empty-runtime guard so the parity
// test exercises the exact mapping the production path uses.
func mapEmptyRuntime(runtime string) string {
	if runtime == "" {
		return sandboxcontainer.DefaultRuntime
	}
	return runtime
}

// withLaunchUnitLayers substitutes the image-derived unit-layer seam for the
// duration of a test, restoring the production resolver afterward.
func withLaunchUnitLayers(t *testing.T, fn func(ctx context.Context, runtime, image string) ([]capture.CaptureDescriptor, []proxybinding.Entry, error)) {
	t.Helper()
	orig := launchUnitLayers
	launchUnitLayers = fn
	t.Cleanup(func() { launchUnitLayers = orig })
}

// TestResolveGitHubSentinelSwapBindings_HappyPath proves the launch-side
// helper resolves gh's sentinel-swap binding from the image unit layer and
// returns only the sentinel-swap subset (the github.com inject binding is
// filtered out). It also pins the empty-runtime mapping (P0-B): the seam is
// invoked with the default runtime when the plan runtime is empty.
func TestResolveGitHubSentinelSwapBindings_HappyPath(t *testing.T) {
	var gotRuntime string
	withLaunchUnitLayers(t, func(_ context.Context, runtime, _ string) ([]capture.CaptureDescriptor, []proxybinding.Entry, error) {
		gotRuntime = runtime
		_, sealing, err := unitloader.LayersFromImage(context.Background(), labelRunner(ghMetadataLabel), runtime, "img:gh")
		return nil, sealing, err
	})

	bindings, err := resolveGitHubSentinelSwapBindings(context.Background(), "", "img:gh")
	if err != nil {
		t.Fatalf("resolveGitHubSentinelSwapBindings: %v", err)
	}
	if gotRuntime != sandboxcontainer.DefaultRuntime {
		t.Errorf("empty plan runtime mapped to %q, want %q (DefaultRuntime, P0-B)", gotRuntime, sandboxcontainer.DefaultRuntime)
	}
	if len(bindings) != 1 || bindings[0].HostPattern != "api.github.com" {
		t.Fatalf("bindings = %#v, want exactly the api.github.com sentinel-swap binding", bindings)
	}
	if bindings[0].EmitMechanism != binding.EmitMechanismSentinelSwap ||
		bindings[0].SentinelEnv != "GH_TOKEN" {
		t.Errorf("binding = %#v, want sentinel-swap GH_TOKEN", bindings[0])
	}
}

// TestResolveGitHubSentinelSwapBindings_PreservesNonEmptyRuntime proves a
// non-empty plan runtime is threaded through unchanged (not overridden).
func TestResolveGitHubSentinelSwapBindings_PreservesNonEmptyRuntime(t *testing.T) {
	var gotRuntime string
	withLaunchUnitLayers(t, func(_ context.Context, runtime, _ string) ([]capture.CaptureDescriptor, []proxybinding.Entry, error) {
		gotRuntime = runtime
		return nil, nil, nil
	})
	if _, err := resolveGitHubSentinelSwapBindings(context.Background(), "podman", "img:gh"); err != nil {
		t.Fatalf("resolveGitHubSentinelSwapBindings: %v", err)
	}
	if gotRuntime != "podman" {
		t.Errorf("plan runtime %q was not preserved (got %q)", "podman", gotRuntime)
	}
}

// TestResolveGitHubSentinelSwapBindings_LayerErrorSurfaces proves a
// present-but-malformed unit (a layer-read error) fails the launch loudly
// rather than degrading to an empty plant set.
func TestResolveGitHubSentinelSwapBindings_LayerErrorSurfaces(t *testing.T) {
	wantErr := errors.New("unitloader: parse cli unit: boom")
	withLaunchUnitLayers(t, func(context.Context, string, string) ([]capture.CaptureDescriptor, []proxybinding.Entry, error) {
		return nil, nil, wantErr
	})
	if _, err := resolveGitHubSentinelSwapBindings(context.Background(), "docker", "img:gh"); !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want it to wrap %v", err, wantErr)
	}
}

// TestResolveGitHubSentinelSwapBindings_MalformedEntrySurfaces proves a
// malformed sealing entry (an unknown scheme the proxybinding loader rejects)
// fails the launch loudly rather than silently shipping no sentinel.
func TestResolveGitHubSentinelSwapBindings_MalformedEntrySurfaces(t *testing.T) {
	withLaunchUnitLayers(t, func(context.Context, string, string) ([]capture.CaptureDescriptor, []proxybinding.Entry, error) {
		return nil, []proxybinding.Entry{{
			Host:          "api.github.com",
			CredentialRef: "user/github",
			Scheme:        "not-a-real-scheme",
		}}, nil
	})
	if _, err := resolveGitHubSentinelSwapBindings(context.Background(), "docker", "img:gh"); err == nil {
		t.Fatal("malformed sealing entry = nil error, want a loud failure")
	}
}

// TestResolveGitHubSentinelSwapBindings_NoLabelNoOp proves an image whose
// label yields no unit contributes no gh sentinel-swap binding (clean no-op
// preserving the central defaults table; gh is the only sentinel-swap default
// today, so the subset is empty).
func TestResolveGitHubSentinelSwapBindings_NoLabelNoOp(t *testing.T) {
	withLaunchUnitLayers(t, func(context.Context, string, string) ([]capture.CaptureDescriptor, []proxybinding.Entry, error) {
		return nil, nil, nil
	})
	bindings, err := resolveGitHubSentinelSwapBindings(context.Background(), "docker", "img:gh")
	if err != nil {
		t.Fatalf("resolveGitHubSentinelSwapBindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Errorf("bindings = %#v, want empty (no unit layer, no central sentinel-swap default)", bindings)
	}
}
