package app

import (
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// githubCredentialRef is the vault path the user-level GitHub token lives
// at (`user/github`). It aliases the canonical proxybinding constant so
// the app-side tests and helpers name one source of truth. Both GitHub
// host bindings resolve this same entry daemon-side; only the injection
// scheme differs per host. It is a credential *reference*, never the
// credential bytes.
const githubCredentialRef = proxybinding.GitHubCredentialRef

// gitHubHostBindings returns the two user-level host->credential bindings
// that seal GitHub traffic at the TLS forward-proxy boundary. The
// canonical assembly lives in proxybinding so the daemon recognizer and
// the launch-side sentinel planter read one Go source of truth (#1247);
// this thin wrapper preserves the app-package call sites and tests.
func gitHubHostBindings() (binding.HostBindings, error) {
	return proxybinding.GitHubHostBindings()
}
