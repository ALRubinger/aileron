package publish

import (
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"

	"oras.land/oras-go/v2/registry/remote"
)

// newRemoteRepository builds an oras remote repository for ref (registry +
// repository, any tag/digest ignored), authenticated with the operator's
// existing Docker/OCI credentials. It delegates to the shared ociremote helper
// so publish and pull (#1902) construct their remote repositories identically
// and cmd/aileron never imports oras. `publish.Run` constructs both the
// destination and the foreign-base source repositories through it.
func newRemoteRepository(ref string) (*remote.Repository, error) {
	return ociremote.NewRepository(ref)
}
