package toolchain

import (
	"github.com/ALRubinger/aileron/internal/sandbox/nodedist"
	"golang.org/x/crypto/openpgp" //nolint:staticcheck // openpgp is the GPG verification primitive nodedist requires.
)

// keyringList is the trusted-key set type the nodedist fetcher consumes. It is
// aliased here so Options.Keyring callers (and the package's defaulting) do not
// spell out the openpgp type, while the production default routes to the
// embedded Node release keyring.
type keyringList = openpgp.EntityList

// defaultKeyring is the production keyring source: the embedded Node release
// keyring shipped by the nodedist package.
func defaultKeyring() (keyringList, error) {
	return nodedist.DefaultKeyring()
}
