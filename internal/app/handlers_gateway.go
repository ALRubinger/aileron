package app

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/ALRubinger/aileron/internal/intercept"
)

// PostChatCompletions handles POST /v1/chat/completions.
//
// When no actions are installed, the request is forwarded through a
// transparent reverse proxy (stage 2 behavior). When the user has
// installed actions, the request is delegated to the intercept engine
// which augments the tool catalog (stage 3) and intercepts Aileron
// tool calls (stage 4) before forwarding the final assistant message
// to the agent.
func (s *apiServer) PostChatCompletions(w http.ResponseWriter, r *http.Request) {
	if s.openAIProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"OpenAI gateway upstream is not configured")
		return
	}
	if !s.hasInstalledActions() {
		s.openAIProxy.ServeHTTP(w, r)
		return
	}
	s.interceptEngine.HandleOpenAI(w, r)
}

// PostMessages handles POST /v1/messages with the same logic shaped
// for the Anthropic Messages protocol.
func (s *apiServer) PostMessages(w http.ResponseWriter, r *http.Request) {
	if s.anthropicProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"Anthropic gateway upstream is not configured")
		return
	}
	if !s.hasInstalledActions() {
		s.anthropicProxy.ServeHTTP(w, r)
		return
	}
	s.interceptEngine.HandleAnthropic(w, r)
}

// hasInstalledActions reports whether at least one action is loaded
// in the user's actions store. Without actions there is nothing to
// augment or intercept, so the gateway can stay on the lightweight
// reverse-proxy path.
func (s *apiServer) hasInstalledActions() bool {
	if s.actions == nil || s.interceptEngine == nil {
		return false
	}
	return len(s.actions.List()) > 0
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

// interceptEngineHandle is the contract apiServer relies on. The
// concrete implementation lives in internal/intercept; the interface
// keeps this package decoupled from intercept's internal state.
type interceptEngineHandle interface {
	HandleOpenAI(w http.ResponseWriter, r *http.Request)
	HandleAnthropic(w http.ResponseWriter, r *http.Request)
}

// Compile-time assertion: *intercept.Engine satisfies the handle.
var _ interceptEngineHandle = (*intercept.Engine)(nil)
