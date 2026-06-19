package proxybinding

import (
	"fmt"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/sentinel"
)

// GitHubCredentialRef is the vault path the user-level GitHub token lives
// at (`user/github`), written by `aileron auth github`. Both GitHub host
// bindings resolve this same entry daemon-side; only the injection scheme
// differs per host. It is a credential *reference*, never the credential
// bytes.
const GitHubCredentialRef = "user/github"

// gitHubBasicUsername is the HTTP basic-auth username git-over-HTTPS uses
// against github.com: the token rides in the password field while the
// username is a fixed, non-secret sentinel. This is the documented
// convention for authenticating git with a GitHub token.
const gitHubBasicUsername = "x-access-token"

// GitHubSentinelEnv is the environment-variable name the launcher plants
// the GitHub sentinel into so `gh` treats itself as authenticated and
// issues its request. The sentinel value rides on the api.github.com
// binding via sentinel.GitHubTokenSentinel; both the value and this env
// name travel on the binding so the planter and the proxy recognizer read
// one source of truth (#1247).
const GitHubSentinelEnv = "GH_TOKEN"

// GitHubHostBindings returns the two user-level host->credential bindings
// that seal GitHub traffic at the TLS forward-proxy boundary (ADR-0019,
// umbrella #1191). It lives in proxybinding so both the daemon (which
// consults the table at egress) and the launcher (which plants the
// mechanism-B sentinels) read the same Go source of truth, alongside the
// descriptor-loaded bindings. The agent inside the sandbox holds no
// GitHub secret; the daemon resolves user/github from the vault and
// injects the credential per the matched binding's scheme:
//
//   - github.com -> basic, emit-mechanism A, emitting
//     "Authorization: Basic base64(x-access-token:<token>)", the
//     git-over-HTTPS convention used by `git clone`/`git push`.
//     git-over-HTTPS issues an unauthenticated request (its no-op
//     credential helper supplies empty credentials), so the proxy injects
//     unconditionally with no sentinel needed.
//   - api.github.com -> bearer, emit-mechanism B, emitting
//     "Authorization: Bearer <token>", the `gh`/REST convention. `gh`
//     short-circuits locally without a token, so the launcher plants a
//     non-secret sentinel as GH_TOKEN; the proxy swaps the sentinel for
//     the real credential at egress and leaves a foreign token untouched
//     (#1196). The sentinel value (sentinel.GitHubTokenSentinel) and the
//     env name (GH_TOKEN) ride on the binding via WithSentinel, so no
//     GitHub-specific sentinel constant lives in the plant or match path
//     (#1247).
//
// Both name the same credential-ref (user/github); the scheme and emit
// mechanism are what differ. No secret bytes appear in the returned
// descriptors: a binding names where the credential lives, never its
// value. Resolution fails closed daemon-side when the vault is locked or
// the entry is absent.
//
// The two host patterns are exact (no wildcard): only github.com and
// api.github.com are sealed, so a request to any other *.github.com host
// (raw.githubusercontent.com is a different apex entirely) falls through
// to passthrough rather than being injected with a credential it was not
// scoped for.
func GitHubHostBindings() (binding.HostBindings, error) {
	apex, err := binding.NewHostBinding(
		"github.com", GitHubCredentialRef, binding.SchemeBasic,
		binding.WithBasicUsername(gitHubBasicUsername),
	)
	if err != nil {
		return nil, fmt.Errorf("github host binding (github.com): %w", err)
	}
	api, err := binding.NewHostBinding(
		"api.github.com", GitHubCredentialRef, binding.SchemeBearer,
		binding.WithEmitMechanismB(),
		binding.WithSentinel(sentinel.GitHubTokenSentinel, GitHubSentinelEnv),
	)
	if err != nil {
		return nil, fmt.Errorf("github host binding (api.github.com): %w", err)
	}
	return binding.HostBindings{apex, api}, nil
}

// AllHostBindings assembles the canonical host-binding table the daemon
// recognizer and the launch-side sentinel planter both read: the GitHub
// Go bindings followed by the descriptor-loaded bindings (#1197). Sharing
// this assembly keeps the plant and the match reading one source of truth
// across the process boundary. A malformed descriptor fails loudly rather
// than degrading to an empty (passthrough) table.
func AllHostBindings(opts LoadOptions) (binding.HostBindings, error) {
	gh, err := GitHubHostBindings()
	if err != nil {
		return nil, fmt.Errorf("seed github host bindings: %w", err)
	}
	descriptors, err := LoadHostBindings(opts)
	if err != nil {
		return nil, fmt.Errorf("load binding descriptors: %w", err)
	}
	return append(gh, descriptors...), nil
}
