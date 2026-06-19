// Package sentinel holds the reserved, non-secret, format-mimicking
// placeholder tokens that the launcher plants in a sandbox so a CLI's
// own local validation passes, and that the daemon's forward proxy
// recognizes and swaps for the real credential at egress (ADR-0019,
// umbrella #1191).
//
// # Why a sentinel exists (sentinel-swap)
//
// Some CLIs short-circuit locally when they hold no auth token: they
// never issue the network request, so the proxy has nothing to seal.
// `gh` is the canonical example. `gh api user` with no configured token
// fails locally and emits no request to api.github.com.
//
// The inject mechanism (the launcher plants nothing; the CLI emits an
// unauthenticated request the proxy seals at egress) therefore does not
// work for such CLIs. The sentinel-swap mechanism closes the gap: the
// launcher plants a reserved, non-secret placeholder that passes the
// CLI's local format check, so the CLI does issue the request. The proxy
// then
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
// This package supplies the canonical sentinel *value*, but the
// recognizer no longer lives here. Recognition is binding-driven
// at the proxy seam: a sentinel-swap host binding carries the value it was
// planted with, and the egress recognizer compares the inbound carrier
// against that binding's value. The launch-side planter reads the same
// value from the same binding, so the plant and the match cannot drift.
// The match is an exact comparison; because the value is non-secret, a
// plain comparison is sufficient (there is no secret to protect against a
// timing side-channel).
//
// [ADR-0019]: https://docs.withaileron.ai/adr/0019-v4-https-data-plane
package sentinel

// GitHubTokenSentinel is the reserved, non-secret placeholder the
// launcher plants as GH_TOKEN so `gh` treats itself as authenticated and
// issues its request instead of short-circuiting. It is the canonical
// GitHub sentinel value the GitHub host binding declares via WithSentinel
// in github.yaml. The daemon proxy recognizes it at egress by comparing
// the inbound carrier against the matched binding's SentinelValue and
// swaps in the real user/github credential before the request leaves the
// host.
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
