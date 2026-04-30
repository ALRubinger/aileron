package app

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// PostChatCompletions handles POST /v1/chat/completions.
//
// Stage 2: transparent reverse proxy to the upstream OpenAI provider.
// Tool augmentation, name-collision handling, pre-LLM bypass, and
// tool-call interception all land in subsequent stages — for now the
// request is forwarded byte-for-byte (including streaming SSE) and the
// upstream response is returned to the agent unchanged.
func (s *apiServer) PostChatCompletions(w http.ResponseWriter, r *http.Request) {
	if s.openAIProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"OpenAI gateway upstream is not configured")
		return
	}
	s.openAIProxy.ServeHTTP(w, r)
}

// PostMessages handles POST /v1/messages.
//
// Stage 2: transparent reverse proxy to the upstream Anthropic provider.
// See PostChatCompletions for the staged rollout plan.
func (s *apiServer) PostMessages(w http.ResponseWriter, r *http.Request) {
	if s.anthropicProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"Anthropic gateway upstream is not configured")
		return
	}
	s.anthropicProxy.ServeHTTP(w, r)
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
