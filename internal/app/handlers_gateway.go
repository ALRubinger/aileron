package app

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/ALRubinger/aileron/internal/augment"
)

// gatewayProtocol identifies which augmenter applies to a request.
type gatewayProtocol int

const (
	protocolOpenAI gatewayProtocol = iota
	protocolAnthropic
)

// PostChatCompletions handles POST /v1/chat/completions.
//
// Stage 3: transparent reverse proxy to the upstream OpenAI provider
// with tool augmentation. The agent's request body is decoded, the
// user's installed actions ([ADR-0003]) are appended to the `tools`
// array per ADR-0008's collision rules, and the modified body is
// forwarded upstream. Tool-call interception lands in stage 4;
// pre-LLM bypass in stage 5.
//
// [ADR-0003]: https://docs.withaileron.ai/adr/0003-action-model
func (s *apiServer) PostChatCompletions(w http.ResponseWriter, r *http.Request) {
	if s.openAIProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"OpenAI gateway upstream is not configured")
		return
	}
	if err := s.augmentRequest(r, protocolOpenAI); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"failed to augment request: "+err.Error())
		return
	}
	s.openAIProxy.ServeHTTP(w, r)
}

// PostMessages handles POST /v1/messages.
//
// Stage 3: transparent reverse proxy to the upstream Anthropic
// provider with tool augmentation. Mirrors PostChatCompletions in
// the Anthropic shape (`tools` array of name/description/input_schema).
func (s *apiServer) PostMessages(w http.ResponseWriter, r *http.Request) {
	if s.anthropicProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"Anthropic gateway upstream is not configured")
		return
	}
	if err := s.augmentRequest(r, protocolAnthropic); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"failed to augment request: "+err.Error())
		return
	}
	s.anthropicProxy.ServeHTTP(w, r)
}

// augmentRequest reads the request body, appends installed actions to
// the appropriate `tools` shape, and rewires the body so the proxy
// forwards the augmented payload. When no actions are installed the
// body is left untouched (passthrough — the stage-2 behavior).
func (s *apiServer) augmentRequest(r *http.Request, proto gatewayProtocol) error {
	if s.actions == nil {
		return nil
	}
	loaded := s.actions.List()
	if len(loaded) == 0 {
		return nil
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	_ = r.Body.Close()

	derived := augment.DeriveAll(loaded)

	var augmented []byte
	switch proto {
	case protocolOpenAI:
		augmented, err = augment.AugmentOpenAI(body, derived, s.log)
	case protocolAnthropic:
		augmented, err = augment.AugmentAnthropic(body, derived, s.log)
	}
	if err != nil {
		return err
	}

	r.Body = io.NopCloser(bytes.NewReader(augmented))
	r.ContentLength = int64(len(augmented))
	r.Header.Set("Content-Length", strconv.Itoa(len(augmented)))
	return nil
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
