// Package sentinel holds the reserved, non-secret, format-mimicking
// placeholder tokens that the launcher plants in a sandbox so a CLI's
// own local validation passes, and that the daemon's forward proxy
// recognizes and swaps for the real credential at egress (ADR-0019,
// umbrella #1191).
//
// # Why a sentinel exists (emit-mechanism B)
//
// Some CLIs short-circuit locally when they hold no auth token: they
// never issue the network request, so the proxy has nothing to seal.
// `gh` is the canonical example. `gh api user` with no configured token
// fails locally and emits no request to api.github.com.
//
// Emit-mechanism A (the launcher plants nothing; the CLI emits an
// unauthenticated request the proxy seals at egress) therefore does not
// work for such CLIs. Emit-mechanism B closes the gap: the launcher
// plants a reserved, non-secret placeholder that passes the CLI's local
// format check, so the CLI does issue the request. The proxy then
// recognizes the placeholder at egress and swaps in the real credential
// it resolves daemon-side. The placeholder bytes never reach upstream.
//
// # The sentinel is non-secret and safe to embed
//
// Every value in this package is a compile-time constant with no
// entropy and no secret. It is safe to embed in source, print in logs,
// and read inside the sandbox. It carries no authority: presenting the
// sentinel to GitHub authenticates nothing. Its only job is to be a
// well-formed-looking placeholder the proxy can recognize as its own
// plant and swap.
//
// # Single source of truth
//
// The launch-side injector and the proxy-side recognizer both import
// this package, so the value they agree on cannot drift. Recognition is
// an exact match against the reserved value; because the value is
// non-secret, a plain comparison is sufficient (there is no secret to
// protect against a timing side-channel).
//
// [ADR-0019]: https://docs.withaileron.ai/adr/0019-v4-https-data-plane
package sentinel

// GitHubTokenSentinel is the reserved, non-secret placeholder the
// launcher plants as GH_TOKEN so `gh` treats itself as authenticated and
// issues its request instead of short-circuiting. The daemon proxy
// recognizes it at egress (see [IsGitHubTokenSentinel]) and swaps in the
// real user/github credential before the request leaves the host.
//
// Shape: it mimics `gh`'s classic personal-access-token format so `gh`'s
// own local validation accepts it: the `ghp_` prefix followed by a
// 36-character alphanumeric body, 40 characters total. The body is an
// all-`A` filler with an embedded, human-readable "AILERONSENTINEL"
// marker so anyone who sees it in a log or a process listing recognizes
// it as the deliberate Aileron placeholder and not a real leaked token.
// The marker uses only A-Z, staying within `gh`'s accepted charset.
//
// It is NOT a secret. Presenting it to GitHub authenticates nothing.
const GitHubTokenSentinel = "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA"

// IsGitHubTokenSentinel reports whether token is exactly the reserved
// GitHub sentinel. It is the proxy-side recognizer: only a token the
// launcher itself planted (an exact match) is swapped for the real
// credential at egress. Any other value (a real-looking `ghp_…` token a
// user supplied themselves, an empty string, or a whitespace-padded
// variant) returns false and is left untouched, which is the load-
// bearing safety property: the proxy seals only tokens it planted and
// never steal-swaps a foreign token.
//
// The comparison is exact and case-sensitive with no trimming: the
// launcher plants the value verbatim, so a padded or altered variant is
// by definition not the plant and must not be treated as one.
func IsGitHubTokenSentinel(token string) bool {
	return token == GitHubTokenSentinel
}
