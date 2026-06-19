package app

import (
	"net/http"
	"strings"

	"github.com/ALRubinger/aileron/internal/binding"
)

// sentinelSwapDecision is the result of inspecting the inbound credential
// carrier on a matched host binding to decide whether the egress seam
// should inject the bound credential (sentinel-swap, ADR-0019).
type sentinelSwapDecision int

const (
	// sentinelSwapInject means proceed with the binding's injection
	// scheme: either the binding is emit-mechanism inject (the default,
	// always inject), or it is sentinel-swap and the inbound carrier is the
	// recognized sentinel (which is stripped before injection so it never
	// reaches upstream), or it is sentinel-swap with no carrier at all.
	sentinelSwapInject sentinelSwapDecision = iota

	// sentinelSwapPassthroughForeign means the binding is sentinel-swap and
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
// For an inject binding it always returns sentinelSwapInject: the inject
// mechanism plants nothing and injects unconditionally, so the carrier
// (if any) is overwritten by the scheme.
//
// For a sentinel-swap binding it gates on the carrier:
//   - the recognized sentinel  -> strip the carrier and inject (the
//     sentinel value is never forwarded upstream);
//   - a foreign, non-empty token -> do not inject; forward unchanged;
//   - no carrier / empty carrier -> inject per the binding's scheme.
//
// Recognition is binding-driven: the matched binding carries the sentinel
// value the launcher planted (HostBinding.SentinelValue), so the
// launch-side plant and the proxy-side recognizer read one source of
// truth and cannot drift. There is no GitHub special-casing here. Any
// sentinel-swap host is recognized by its own binding's sentinel value
// with no change to this decision path.
func decideSentinelSwap(req *http.Request, hb binding.HostBinding) sentinelSwapDecision {
	if hb.EmitMechanism != binding.EmitMechanismSentinelSwap {
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
// launcher plants for this sentinel-swap binding. The carrier may be the
// bare sentinel (gh sets GH_TOKEN verbatim and some clients send the
// token alone) or a "Bearer <sentinel>" / "token <sentinel>" form, so
// the scheme prefix is tolerated before the exact sentinel match.
//
// The match reads the binding's own SentinelValue: there is no GitHub
// special-casing. The comparison is exact, case-sensitive, and does no
// trimming beyond the auth-scheme prefix, preserving the foreign-token-safe
// property (only our own plant is swapped). A SentinelValue of "" never
// matches, so an inject-or-misconstructed binding can never match an empty
// or any carrier.
func bindingSentinelMatches(hb binding.HostBinding, carrier string) bool {
	return hb.SentinelValue != "" && stripAuthScheme(carrier) == hb.SentinelValue
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
