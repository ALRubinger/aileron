package app

import (
	"context"
	"io"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cli/unitloader"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// ghMetadataLabel is the devcontainer.metadata label value a restructured
// base image carries for the gh Feature: a JSON array whose gh element holds
// the customizations.aileron.cli unit. It mirrors the canonical
// internal/cli/unitloader/testdata/gh-metadata.json fixture. The daemon-side
// proxy/sentinel tests source gh's bindings through the real unitloader path
// off this label, with NO central internal/proxybinding/defaults/github.yaml
// (#1323).
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
// devcontainer.metadata label to stdout so ImageMetadataLabel resolves it
// without a container.
type labelRunner string

func (l labelRunner) Run(_ context.Context, _ string, _ []string, stdout, _ io.Writer) error {
	_, _ = io.WriteString(stdout, string(l))
	return nil
}

// ghSealingLayer drives the production unitloader path off the fake labelled
// image and returns gh's projected sealing layer, the same []Entry the daemon
// threads into assembleHostBindings via LoadOptions.ExtraEntries.
func ghSealingLayer(t *testing.T) []proxybinding.Entry {
	t.Helper()
	_, sealing, err := unitloader.LayersFromImage(context.Background(), labelRunner(ghMetadataLabel), "docker", "img:gh")
	if err != nil {
		t.Fatalf("LayersFromImage: %v", err)
	}
	return sealing
}

// githubDefaultBindings loads the host-binding table gh now flows through
// (#1323): gh moved out of the central internal/proxybinding/defaults/github.yaml
// and into its devcontainer Feature CLI unit, so the host projects its sealing
// entries from the image's devcontainer.metadata label (internal/cli/unitloader)
// and applies them as the unit-derived layer (LoadOptions.ExtraEntries), exactly
// as the daemon's assembleHostBindings does. The unit layer is driven here off a
// fake labelled image so the test exercises the real loader path with no
// container and no central file.
//
// The proxy/sentinel-swap tests use this to source the GitHub bindings from the
// same place production reads them. Their behavioral assertions (inject
// basic-auth sealing, sentinel-swap, foreign-token passthrough) are unchanged;
// only the binding source moved from the central default to the unit layer.
func githubDefaultBindings(t *testing.T) binding.HostBindings {
	t.Helper()
	opts := proxybinding.LoadOptions{ExtraEntries: ghSealingLayer(t)}
	hbs, err := proxybinding.LoadHostBindings(opts)
	if err != nil {
		t.Fatalf("load host bindings with gh unit layer: %v", err)
	}
	return hbs
}
