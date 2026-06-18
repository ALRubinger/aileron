package app

import (
	"net/http"
	"strings"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/sentinel"
)

// sentinelSwapDecision is the result of inspecting the inbound credential
// carrier on a matched host binding to decide whether the egress seam
// should inject the bound credential (emit-mechanism B sentinel-swap,
// ADR-0019, #1196).
type sentinelSwapDecision int

const (
	// sentinelSwapInject means proceed with the binding's injection
	// scheme: either the binding is emit-mechanism A (the default, always
	// inject), or it is mechanism B and the inbound carrier is the
	// recognized sentinel (which is stripped before injection so it never
	// reaches upstream), or it is mechanism B with no carrier at all.
	sentinelSwapInject sentinelSwapDecision = iota

	// sentinelSwapPassthroughForeign means the binding is mechanism B and
	// the inbound carrier is a foreign (non-sentinel) token. The proxy
	// must NOT swap it: it seals only tokens it planted. The request is
	// forwarded unchanged with the foreign carrier intact and no real
	// credential injected. This is the load-bearing safety property.
	sentinelSwapPassthroughForeign
)

// carrierHeader is the request header the GitHub bearer/basic schemes
// carry the credential in. The sentinel rides here when `gh` issues its
// request, so it is the header the swap gate inspects and strips.
const carrierHeader = "Authorization"

// decideSentinelSwap inspects the inbound credential carrier on req for a
// matched host binding and returns whether the egress seam should inject
// the bound credential or forward a foreign token unchanged.
//
// For an emit-mechanism A binding it always returns sentinelSwapInject:
// mechanism A plants nothing and injects unconditionally, the pre-#1196
// behavior, so the carrier (if any) is overwritten by the scheme.
//
// For an emit-mechanism B binding it gates on the carrier:
//   - the recognized sentinel  -> strip the carrier and inject (the
//     sentinel value is never forwarded upstream);
//   - a foreign, non-empty token -> do not inject; forward unchanged;
//   - no carrier / empty carrier -> inject per the binding's scheme.
//
// Recognition is delegated to the sentinel package (the single source of
// truth shared with the launcher) so the launch-side plant and the
// proxy-side recognizer cannot drift. Only the GitHub sentinel exists
// today; a future mechanism-B host adds its own recognizer here without
// touching the binding-match wiring.
func decideSentinelSwap(req *http.Request, hb binding.HostBinding) sentinelSwapDecision {
	if hb.EmitMechanism != binding.EmitMechanismB {
		return sentinelSwapInject
	}

	carrier := strings.TrimSpace(req.Header.Get(carrierHeader))
	if carrier == "" {
		// No carrier: nothing to swap, inject per the binding's scheme.
		return sentinelSwapInject
	}

	if bindingSentinelMatches(hb, carrier) {
		// Our own plant: strip it so it never reaches upstream, then let
		// the scheme inject the real credential in its place.
		req.Header.Del(carrierHeader)
		return sentinelSwapInject
	}

	// A foreign token the agent supplied itself. We neither steal-swap it
	// nor leak our secret: forward it unchanged.
	return sentinelSwapPassthroughForeign
}

// bindingSentinelMatches reports whether carrier bears the sentinel the
// launcher plants for this mechanism-B binding. The carrier may be the
// bare sentinel (gh sets GH_TOKEN verbatim and some clients send the
// token alone) or a "Bearer <sentinel>" / "token <sentinel>" form, so
// the scheme prefix is tolerated before the exact sentinel match.
//
// Only the GitHub sentinel is wired today; the credential-ref's GitHub
// shape selects it. A future mechanism-B host pattern adds its own arm.
func bindingSentinelMatches(hb binding.HostBinding, carrier string) bool {
	value := stripAuthScheme(carrier)
	if hb.CredentialRef == githubCredentialRef {
		return sentinel.IsGitHubTokenSentinel(value)
	}
	return false
}

// stripAuthScheme removes a leading "Bearer " or "token " auth-scheme
// prefix (case-insensitive) so the remaining value can be matched
// against the bare sentinel. A carrier with no recognized prefix is
// returned trimmed and unchanged.
func stripAuthScheme(carrier string) string {
	carrier = strings.TrimSpace(carrier)
	for _, prefix := range []string{"Bearer ", "token "} {
		if len(carrier) >= len(prefix) && strings.EqualFold(carrier[:len(prefix)], prefix) {
			return strings.TrimSpace(carrier[len(prefix):])
		}
	}
	return carrier
}
