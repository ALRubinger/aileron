package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

// sandboxProxyStepScopeTTL bounds a minted step scope's lifetime. The
// in-container runtime releases a scope right after the step's subprocess
// exits (DELETE, best-effort); the TTL is the backstop for a runtime that
// crashes before releasing, so an orphaned credential always dies on its
// own. Generous enough for a long-running tool step, short enough that an
// orphan is never a standing credential.
const sandboxProxyStepScopeTTL = 15 * time.Minute

// sandboxProxyStepScope is one live step-scoped proxy credential (#1829):
// a daemon-minted ephemeral token bound to a boot proxy session, a tool
// step id, and the step's sealed network reach. A CONNECT authenticated
// with the scope's token may reach ONLY the registered hosts.
type sandboxProxyStepScope struct {
	// SessionID is the boot proxy session the scope rides on (the CONNECT
	// username). Binding the scope to the boot session preserves CA
	// continuity: the MITM leaf is still signed by the boot session CA the
	// container already trusts.
	SessionID string
	// StepID is the tool step the scope was minted for (audit addressing).
	StepID string
	// Hosts is the step's sealed reach: the deny-by-default CONNECT
	// allowlist for this scope, in the lock stepTrust host(:port) vocabulary.
	Hosts []string
	// Token is the ephemeral CONNECT password. Random (crypto/rand),
	// compared constant-time, never logged or audited.
	Token string
	// ExpiresAt is the TTL boundary; an expired scope authenticates as 407.
	ExpiresAt time.Time
}

// sandboxProxyStepScopeReasonHostDenied is the stable reject reason carried
// on the sandbox.proxy.trust_denied audit record when a step-scoped CONNECT
// targets a host outside its sealed reach.
const sandboxProxyStepScopeReasonHostDenied = "step_scope_host_denied"

// sandboxProxyStepScopeStepIDPattern mirrors the manifest step-id shape so a
// scope is only ever keyed by a well-formed step id.
var sandboxProxyStepScopeStepIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// sandboxProxyStepScopeHostPattern mirrors the lock stepTrust host shape:
// a host with an optional port, no scheme, no path.
var sandboxProxyStepScopeHostPattern = regexp.MustCompile(`^[a-zA-Z0-9.-]+(:[0-9]+)?$`)

// CreateSandboxProxyStepScope handles POST /v1/sandbox-proxy/step-scopes.
// It mints a fresh, TTL-bounded step-scoped proxy credential for one tool
// step's sealed reach (#1829). Not idempotent by design: each mint is a
// fresh ephemeral credential; the TTL cleans up an unreleased scope. The
// registry is in-memory only — a per-step ephemeral credential needs no
// persistence across a daemon restart (the step fails closed and re-mints).
//
// The caller-supplied hosts[] is trusted without daemon-side re-verification
// against the sealed lock because the in-container runtime that mints already
// holds the full daemon token, so declaring a scope NARROWER than that
// authority is no privilege escalation — the mint can only shrink reach, never
// widen it. (Were minting ever reachable without full-token auth, scoping
// would become caller-defined; the forward-proxy auth path keeps the mint
// behind that token, so that gap does not exist today.)
func (s *apiServer) CreateSandboxProxyStepScope(w http.ResponseWriter, r *http.Request) {
	var req api.SandboxProxyStepScopeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}

	sessionID := strings.TrimSpace(req.SessionId)
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "session_id field is required")
		return
	}
	if err := validateSandboxProxySessionID(sessionID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	stepID := strings.TrimSpace(req.StepId)
	if stepID == "" || !sandboxProxyStepScopeStepIDPattern.MatchString(stepID) {
		writeError(w, http.StatusBadRequest, "invalid_body", "step_id must be a non-empty [a-zA-Z0-9_-]+ step id")
		return
	}
	if len(req.Hosts) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_body", "hosts must declare at least one sealed host (the scope is deny-by-default)")
		return
	}
	hosts := make([]string, 0, len(req.Hosts))
	for _, h := range req.Hosts {
		h = strings.TrimSpace(h)
		if h == "" || !sandboxProxyStepScopeHostPattern.MatchString(h) {
			writeError(w, http.StatusBadRequest, "invalid_body", "hosts entries must be host or host:port (no scheme, no path)")
			return
		}
		hosts = append(hosts, h)
	}

	scopeID, err := sandboxProxyStepScopeRandomHex(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not mint scope id")
		return
	}
	token, err := sandboxProxyStepScopeRandomHex(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not mint scope token")
		return
	}
	expires := time.Now().Add(sandboxProxyStepScopeTTL)

	s.stepScopesMu.Lock()
	if s.stepScopes == nil {
		s.stepScopes = map[string]sandboxProxyStepScope{}
	}
	s.stepScopes[scopeID] = sandboxProxyStepScope{
		SessionID: sessionID,
		StepID:    stepID,
		Hosts:     hosts,
		Token:     token,
		ExpiresAt: expires,
	}
	s.stepScopesMu.Unlock()

	writeJSON(w, http.StatusCreated, api.SandboxProxyStepScopeResponse{
		ScopeId:   scopeID,
		Token:     token,
		ExpiresAt: expires,
	})
}

// DeleteSandboxProxyStepScope handles
// DELETE /v1/sandbox-proxy/step-scopes/{scope_id}. Idempotent: releasing an
// unknown or already expired scope still returns 204, so the runtime's
// best-effort post-step release never fails a completed step (the TTL is
// the backstop for a release that never arrives).
func (s *apiServer) DeleteSandboxProxyStepScope(w http.ResponseWriter, _ *http.Request, scopeID string) {
	s.stepScopesMu.Lock()
	delete(s.stepScopes, scopeID)
	s.stepScopesMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// lookupSandboxProxyStepScope resolves a CONNECT password against the live
// step-scope registry: the token must match a registered scope
// constant-time, the scope's session must equal the CONNECT username (a
// scope never authenticates a foreign session), and the scope must not have
// expired. Expired entries are reaped lazily here so the registry never
// accumulates dead credentials.
func (s *apiServer) lookupSandboxProxyStepScope(sessionID, token string) (sandboxProxyStepScope, bool) {
	if token == "" {
		return sandboxProxyStepScope{}, false
	}
	now := time.Now()
	s.stepScopesMu.Lock()
	defer s.stepScopesMu.Unlock()
	for id, scope := range s.stepScopes {
		if now.After(scope.ExpiresAt) {
			delete(s.stepScopes, id)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(scope.Token), []byte(token)) == 1 && scope.SessionID == sessionID {
			return scope, true
		}
	}
	return sandboxProxyStepScope{}, false
}

// sandboxProxyStepScopeAllowsHost reports whether a CONNECT target host is
// inside the scope's sealed reach, reusing the host(:port) matching
// semantics the host-binding trust gate uses (sandboxProxyHostAllowed). The
// CONNECT target is always port 443 (enforced by
// sandboxForwardProxyConnectHost), so a sealed `host` or `host:443` entry
// both admit it. Deny-by-default: an empty reach admits nothing.
func sandboxProxyStepScopeAllowsHost(scope sandboxProxyStepScope, targetHost string) bool {
	upstream := &url.URL{Scheme: "https", Host: targetHost}
	return sandboxProxyHostAllowed(upstream, scope.Hosts)
}

// recordSandboxProxyStepScopeTrustDenied emits sandbox.proxy.trust_denied
// for a step-scoped CONNECT refused at the proxy because its target host is
// outside the scope's sealed reach (#1829). The denial happens BEFORE the
// TLS MITM handshake, so the payload carries only the CONNECT-visible
// surface: the target host, the scope's step id, and the session id. It
// never carries the scope token or the sealed host list. nil-recorder safe
// like its siblings.
func (s *apiServer) recordSandboxProxyStepScopeTrustDenied(r *http.Request, scope sandboxProxyStepScope, targetHost string) string {
	if s.auditRecorder == nil {
		if s.newID != nil {
			return s.newID()
		}
		return audit.DefaultIDFn()
	}
	payload := map[string]any{
		"aileron.proxy.boundary":      "https_proxy",
		"aileron.proxy.mediation":     "https_proxy",
		"aileron.proxy.source":        sandboxProxySourceTransparentConnectTLS,
		"aileron.proxy.decision":      "trust_denied",
		"aileron.proxy.reject_reason": sandboxProxyStepScopeReasonHostDenied,
		"aileron.proxy.method":        r.Method,
		"aileron.proxy.upstream.host": targetHost,
		"aileron.step.id":             scope.StepID,
		"aileron.session.id":          scope.SessionID,
	}
	return s.auditRecorder.RecordSuccess(
		r.Context(),
		model.EventTypeSandboxProxyTrustDenied,
		model.ActorRef{Type: model.ActorTypeAgent, ID: "sandbox-proxy"},
		payload,
	)
}

// sandboxProxyStepScopeRandomHex returns n crypto/rand bytes hex-encoded.
func sandboxProxyStepScopeRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
