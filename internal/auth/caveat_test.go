package auth

import (
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// caveat.go contract:
//   - Issue mints a signed token bound to a session carrying the given caps.
//   - Validate round-trips Issue: same key recovers session + caps.
//   - Validate rejects a token signed with a different key (forgery).
//   - Validate rejects a non-caveat HS256 token signed with the same key.
//   - Issue refuses an empty session id.
//   - HasCap reports membership exactly.

func newTestIssuer(t *testing.T) *CaveatIssuer {
	t.Helper()
	key, err := GenerateCaveatSigningKey()
	if err != nil {
		t.Fatalf("GenerateCaveatSigningKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("signing key len = %d, want 32", len(key))
	}
	return NewCaveatIssuer(key, "aileron-daemon")
}

func TestCaveatIssuer_RoundTrip(t *testing.T) {
	ci := newTestIssuer(t)
	tok, err := ci.Issue("sess-A", SandboxMCPCaps)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	claims, err := ci.Validate(tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.Session != "sess-A" {
		t.Errorf("Session = %q, want sess-A", claims.Session)
	}
	for _, want := range SandboxMCPCaps {
		if !claims.HasCap(want) {
			t.Errorf("claims missing capability %q", want)
		}
	}
	if claims.HasCap(Capability("vault:decrypt")) {
		t.Error("claims should not carry an unminted capability")
	}
}

func TestCaveatIssuer_RejectsForgedKey(t *testing.T) {
	minter := newTestIssuer(t)
	tok, err := minter.Issue("sess-A", SandboxMCPCaps)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// A validator with a different key must reject the token.
	other := newTestIssuer(t)
	if _, err := other.Validate(tok); err == nil {
		t.Fatal("Validate accepted a token signed with a different key")
	}
}

func TestCaveatIssuer_RejectsNonCaveatToken(t *testing.T) {
	key, err := GenerateCaveatSigningKey()
	if err != nil {
		t.Fatalf("GenerateCaveatSigningKey: %v", err)
	}
	// A valid HS256 token signed with the caveat key but without the
	// caveat type marker must not be accepted as a caveat.
	plain := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{Subject: "sess-A"})
	signed, err := plain.SignedString(key)
	if err != nil {
		t.Fatalf("sign plain token: %v", err)
	}
	ci := NewCaveatIssuer(key, "aileron-daemon")
	if _, err := ci.Validate(signed); err == nil {
		t.Fatal("Validate accepted a non-caveat token signed with the caveat key")
	}
}

func TestCaveatIssuer_RejectsMissingSessionClaim(t *testing.T) {
	key, err := GenerateCaveatSigningKey()
	if err != nil {
		t.Fatalf("GenerateCaveatSigningKey: %v", err)
	}
	// A caveat-typed token with no session binding must be rejected: the
	// middleware relies on the session claim for its defense-in-depth
	// binding check.
	claims := CaveatClaims{Type: caveatTokenType, Caps: SandboxMCPCaps}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	ci := NewCaveatIssuer(key, "aileron-daemon")
	if _, err := ci.Validate(signed); err == nil {
		t.Fatal("Validate accepted a caveat token with no session binding")
	}
}

func TestCaveatIssuer_RejectsNonHMACAlg(t *testing.T) {
	ci := newTestIssuer(t)
	// An unsigned ("alg: none") token must be rejected by the keyfunc's
	// HMAC method check rather than accepted.
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, CaveatClaims{
		Type:    caveatTokenType,
		Caps:    SandboxMCPCaps,
		Session: "sess-A",
	})
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := ci.Validate(signed); err == nil {
		t.Fatal("Validate accepted an alg:none token")
	}
}

func TestCaveatIssuer_Issue_EmptySession(t *testing.T) {
	ci := newTestIssuer(t)
	if _, err := ci.Issue("", SandboxMCPCaps); err == nil {
		t.Fatal("Issue accepted an empty session id")
	}
}

func TestCaveatClaims_HasCap(t *testing.T) {
	c := &CaveatClaims{Caps: []Capability{CapActionsRun}}
	if !c.HasCap(CapActionsRun) {
		t.Error("HasCap(actions:run) = false, want true")
	}
	if c.HasCap(CapComms) {
		t.Error("HasCap(comms) = true, want false")
	}
}
