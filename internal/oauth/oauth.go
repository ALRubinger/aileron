// Package oauth implements the client-side primitives for the OAuth
// dance the runtime drives at binding-setup time per ADR-0006.
//
// The package is intentionally narrow: it exposes the small set of
// utilities the CLI needs to drive the server-side init/finish flow
// against an interactive user — generate a PKCE verifier+challenge
// pair, open the user's browser to the provider's authorize URL, and
// await the OAuth callback on a loopback HTTP listener.
//
// Token exchange and refresh do not live here; they live in the
// runtime side (server handler + credential resolver) so the same
// machinery serves a future hosted Web UI without re-architecting.
package oauth
