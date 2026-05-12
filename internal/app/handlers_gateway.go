package app

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/ALRubinger/aileron/internal/failure"
)

// PostChatCompletions handles `POST /v1/chat/completions` as a
// transparent reverse proxy to the configured upstream OpenAI-shaped
// provider. Aileron does not augment the agent's tool catalog or
// intercept tool calls in the LLM stream — actions are exposed to the
// agent over MCP via `aileron-mcp` and executed through
// `POST /v1/actions/{name}/run`, which is the single chokepoint where
// the manifest's `[approval]` block is honored. Keeping the gateway
// purely pass-through guarantees there is one execution path for
// every action, regardless of which agent is launched.
//
// When the local credential vault is locked (per ADR-0011), the
// endpoint refuses to serve — the runtime cannot resolve any
// credential a connector might need on a subsequent MCP-driven action
// invocation. The agent receives a 423 FailureEnvelope with class
// `binding_required`, prompting them to surface the unlock requirement
// to the user.
func (s *apiServer) PostChatCompletions(w http.ResponseWriter, r *http.Request) {
	if s.vaultLocked {
		writeVaultLocked(w)
		return
	}
	if s.openAIProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"OpenAI gateway upstream is not configured")
		return
	}
	s.openAIProxy.ServeHTTP(w, r)
}

// PostMessages handles `POST /v1/messages` with the same pass-through
// semantics for the Anthropic Messages protocol.
func (s *apiServer) PostMessages(w http.ResponseWriter, r *http.Request) {
	if s.vaultLocked {
		writeVaultLocked(w)
		return
	}
	if s.anthropicProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"Anthropic gateway upstream is not configured")
		return
	}
	s.anthropicProxy.ServeHTTP(w, r)
}

// writeVaultLocked emits the canonical FailureEnvelope for "the
// runtime can't serve this request until the vault is unlocked."
// `binding_required` is the closest fit in ADR-0010's closed
// taxonomy: the user must bind a credential (their passphrase) and
// retry. failure.WriteHTTP maps the class to its canonical status
// (412 Precondition Failed for binding_required).
func writeVaultLocked(w http.ResponseWriter) {
	f := failure.BindingRequiredFailure(
		"local credential vault is locked; run `aileron vault unlock` to resume",
		failure.WithDetails(map[string]any{
			"required": "vault.passphrase",
			"hint":     "the runtime needs the KEK in memory before any connector credential can be resolved",
		}),
	)
	failure.WriteHTTP(w, f)
}

// newGatewayProxy builds an HTTP reverse proxy that forwards requests
// to the configured upstream LLM provider.
//
// The agent's `Authorization` (OpenAI) or `x-api-key`/`anthropic-version`
// (Anthropic) headers ride through unchanged — Aileron does not
// authenticate to upstream itself; the agent's own credentials flow
// through. FlushInterval = -1 ensures SSE chunks reach the agent as
// soon as the upstream emits them, preserving streaming semantics.
//
// providerLabel is used for structured logs only; it does not affect
// request routing.
func newGatewayProxy(upstream *url.URL, providerLabel string, log *slog.Logger) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.WarnContext(r.Context(), "gateway: upstream request failed",
				"provider", providerLabel,
				"upstream", upstream.String(),
				"error", err)
			writeError(w, http.StatusBadGateway, "upstream_error",
				"upstream provider request failed: "+err.Error())
		},
	}
	return rp
}
