package app

import (
	"fmt"

	"github.com/ALRubinger/aileron/internal/binding"
)

// githubCredentialRef is the vault path the user-level GitHub token
// lives at (`user/github`), written by `aileron auth github`. Both
// GitHub host bindings resolve this same entry daemon-side; only the
// injection scheme differs per host. It is a credential *reference*,
// never the credential bytes.
const githubCredentialRef = "user/github"

// githubBasicUsername is the HTTP basic-auth username git-over-HTTPS
// uses against github.com: the token rides in the password field while
// the username is a fixed, non-secret sentinel. This is the documented
// convention for authenticating git with a GitHub token.
const githubBasicUsername = "x-access-token"

// gitHubHostBindings returns the two user-level host->credential
// bindings that seal GitHub traffic at the TLS forward-proxy boundary
// (ADR-0019, umbrella #1191). The agent inside the sandbox holds no
// GitHub secret; the daemon resolves user/github from the vault and
// injects the credential per the matched binding's scheme:
//
//   - github.com -> basic, emitting
//     "Authorization: Basic base64(x-access-token:<token>)", the
//     git-over-HTTPS convention used by `git clone`/`git push`.
//   - api.github.com -> bearer, emitting
//     "Authorization: Bearer <token>", the `gh`/REST convention.
//
// Both name the same credential-ref (user/github); the scheme is what
// differs. No secret bytes appear in the returned descriptors: a
// binding names where the credential lives, never its value. Resolution
// fails closed daemon-side when the vault is locked or the entry is
// absent (the binding-injection path writes no Authorization header and
// no secret in that case; see injectSandboxProxyHostBindingCredential).
//
// The two host patterns are exact (no wildcard): only github.com and
// api.github.com are sealed, so a request to any other *.github.com
// host (raw.githubusercontent.com is a different apex entirely) falls
// through to passthrough rather than being injected with a credential
// it was not scoped for.
func gitHubHostBindings() (binding.HostBindings, error) {
	apex, err := binding.NewHostBinding(
		"github.com", githubCredentialRef, binding.SchemeBasic,
		binding.WithBasicUsername(githubBasicUsername),
	)
	if err != nil {
		return nil, fmt.Errorf("github host binding (github.com): %w", err)
	}
	api, err := binding.NewHostBinding(
		"api.github.com", githubCredentialRef, binding.SchemeBearer,
	)
	if err != nil {
		return nil, fmt.Errorf("github host binding (api.github.com): %w", err)
	}
	return binding.HostBindings{apex, api}, nil
}
