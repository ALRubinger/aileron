package app

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// githubDefaultBindings loads the trusted built-in binding table the same
// way production does (#1248): every binding flows from the descriptor
// layers, and GitHub ships as the trusted built-in defaults/github.yaml.
// Passing the zero LoadOptions selects the built-in layer only (no user
// descriptor), so the returned table is exactly the shipped defaults.
//
// The proxy/sentinel-swap tests use this to source the GitHub bindings
// from the same place production reads them, replacing the deleted bespoke
// Go constructor. Their behavioral assertions (mechanism A basic-auth
// sealing, mechanism B sentinel-swap, foreign-token passthrough) are
// unchanged; only the binding source moved from bespoke Go to the
// descriptor path.
func githubDefaultBindings(t *testing.T) binding.HostBindings {
	t.Helper()
	hbs, err := proxybinding.LoadHostBindings(proxybinding.LoadOptions{})
	if err != nil {
		t.Fatalf("load default host bindings: %v", err)
	}
	return hbs
}
