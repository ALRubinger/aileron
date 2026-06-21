package auth

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Capability names the discrete authority a caveat token may grant. The
// vocabulary is intentionally small and closed: each value maps to the
// minimum set of `/v1/*` routes the in-container `aileron-mcp` process
// exercises (ADR-0024). A leaked caveat token can do only what its caps
// allow, scoped to its bound session — never the full daemon authority
// the master daemon.json token carries.
type Capability string

const (
	// CapActionsList grants read access to the installed-action catalog:
	// GET /v1/actions and GET /v1/actions/{name}.
	CapActionsList Capability = "actions:list"
	// CapActionsRun grants action invocation: POST /v1/actions/{name}/run.
	CapActionsRun Capability = "actions:run"
	// CapApprovalsPoll grants polling an action-approval result:
	// GET /v1/action-approvals/{id}/result.
	CapApprovalsPoll Capability = "approvals:poll"
	// CapComms grants the whole session-comms surface:
	// /v1/sessions/{session_id}/comms/*. Per ADR-0024 / issue #958 this is
	// a single capability rather than split read/send; the per-call
	// approval gating on send and http already bounds the write paths.
	CapComms Capability = "comms"
)

// SandboxMCPCaps is the full capability set the sandbox `aileron-mcp`
// process needs and the set CreateSession mints into the caveat token.
var SandboxMCPCaps = []Capability{
	CapActionsList,
	CapActionsRun,
	CapApprovalsPoll,
	CapComms,
}

// caveatTokenType is the JWT `typ`-equivalent custom claim that marks a
// token as a session caveat. It prevents a future, differently-shaped
// HS256 token signed with the same key from being accepted as a caveat.
const caveatTokenType = "aileron-caveat"

// CaveatClaims are the signed claims carried by a session caveat token.
// The middleware decodes these statelessly (no server-side table) to
// enforce the route->capability map and the session binding.
type CaveatClaims struct {
	jwt.RegisteredClaims
	// Type is always caveatTokenType for a valid caveat token.
	Type string `json:"typ"`
	// Caps is the set of capabilities this token grants.
	Caps []Capability `json:"caps"`
	// Session is the session id this token is bound to. The middleware
	// rejects requests whose path {session_id} differs from this value.
	Session string `json:"session"`
}

// HasCap reports whether the claims grant the given capability.
func (c *CaveatClaims) HasCap(cap Capability) bool {
	for _, have := range c.Caps {
		if have == cap {
			return true
		}
	}
	return false
}

// CaveatIssuer mints and validates HMAC-signed session caveat tokens
// using a per-daemon signing key distinct from the master daemon.json
// token. The master token never enters the signing path, so a leaked
// caveat token cannot be escalated into master authority.
type CaveatIssuer struct {
	signingKey []byte
	issuer     string
}

// NewCaveatIssuer builds a CaveatIssuer over the given HMAC signing key.
// The key should be per-daemon and randomly generated at startup
// (see GenerateCaveatSigningKey); it is NOT the master daemon token.
func NewCaveatIssuer(signingKey []byte, issuer string) *CaveatIssuer {
	return &CaveatIssuer{signingKey: signingKey, issuer: issuer}
}

// GenerateCaveatSigningKey returns a cryptographically random 32-byte
// HMAC signing key for caveat tokens. Generated once at daemon startup
// and never persisted: caveat tokens are stateless and tied to a single
// daemon process lifetime, so a fresh key on restart is acceptable.
func GenerateCaveatSigningKey() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generating caveat signing key: %w", err)
	}
	return b, nil
}

// Issue mints a signed caveat token bound to the given session and
// carrying the supplied capabilities. The token has no expiry: it lives
// as long as the launch session it scopes, and the daemon signing key is
// itself ephemeral (regenerated each daemon start), so an old token stops
// validating once the daemon restarts.
func (ci *CaveatIssuer) Issue(session string, caps []Capability) (string, error) {
	if session == "" {
		return "", fmt.Errorf("caveat token requires a session id")
	}
	claims := CaveatClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:   ci.issuer,
			Subject:  session,
			IssuedAt: jwt.NewNumericDate(time.Now()),
		},
		Type:    caveatTokenType,
		Caps:    caps,
		Session: session,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(ci.signingKey)
}

// Validate parses and verifies a caveat token, returning its claims. It
// rejects tokens signed with a non-HMAC method, tokens that fail the
// signature check, and tokens missing the caveat type marker or a bound
// session.
func (ci *CaveatIssuer) Validate(tokenString string) (*CaveatClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &CaveatClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return ci.signingKey, nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid caveat token: %w", err)
	}
	claims, ok := token.Claims.(*CaveatClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid caveat token claims")
	}
	if claims.Type != caveatTokenType {
		return nil, fmt.Errorf("not a caveat token")
	}
	if claims.Session == "" {
		return nil, fmt.Errorf("caveat token missing session binding")
	}
	return claims, nil
}
