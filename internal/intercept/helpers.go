package intercept

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/model"
)

// nameSet builds a set lookup from a slice for O(1) membership tests.
func nameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// agentRequestedStreaming reports whether the agent's request body
// declared `stream: true`. A malformed body returns false so the
// engine defaults to non-streaming responses.
func agentRequestedStreaming(body []byte) bool {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	v, _ := req["stream"].(bool)
	return v
}

// setStream rewrites the request body's top-level `stream` field
// (creating it if missing). Used to force `stream: false` on upstream
// requests during intercept rounds.
func setStream(body []byte, stream bool) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	req["stream"] = stream
	return json.Marshal(req)
}

// hopByHopHeaders are headers that must not be propagated when proxying
// per RFC 7230. Connection-related metadata belongs to the immediate
// hop, not the request.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
}

// copyForwardableHeaders copies headers from the agent's request to
// the upstream request, dropping hop-by-hop headers and ones the
// engine sets explicitly (Content-Type, Content-Length, Accept, Host).
func copyForwardableHeaders(src, dst http.Header) {
	for k, vs := range src {
		if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Content-Type", "Accept", "Host":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// passThrough writes an upstream response (status, headers, body)
// to the agent unchanged. Used when the upstream produced a non-200
// status or a response shape we couldn't parse — the agent should
// see the upstream's error verbatim rather than a synthesized one.
func passThrough(w http.ResponseWriter, status int, headers http.Header, body []byte) {
	for k, vs := range headers {
		if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	w.Write(body)
}

// errorBody is the JSON shape this package writes for gateway-side
// errors, kept compatible with the api.Error schema in
// internal/api/openapi.yaml.
type errorBody struct {
	Error errorBodyInner `json:"error"`
}

type errorBodyInner struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError emits a structured JSON error matching the Aileron
// `api.Error` schema. Used for gateway-protocol errors (request
// validation, configuration) that don't fit ADR-0010's runtime
// failure taxonomy. Action-execution and upstream-call failures use
// [writeFailure] instead, which writes the [failure.Envelope] shape.
func writeError(w http.ResponseWriter, status int, code, message string) {
	body, _ := json.Marshal(errorBody{Error: errorBodyInner{Code: code, Message: message}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// peekAnthropicError returns the upstream error type and message
// when body parses as an Anthropic error envelope —
// `{"type":"error","error":{"type":"...","message":"..."}}` — and
// (false) for valid messages or unparseable bodies.
//
// Anthropic surfaces some failure modes (overloaded_error,
// rate_limit_error, …) inside an HTTP 200 response body when the
// failure arrives mid-stream and the upstream wraps the streamed
// error event into a single JSON document. Without an explicit
// detection step the gateway sees a "successful" 200, the agent's
// SDK retries silently on the inner error, and the operator gets no
// visible signal that anything went wrong. peekAnthropicError gives
// the gateway a place to log the upstream cause before passing the
// envelope through to the agent.
func peekAnthropicError(body []byte) (errType, message string, ok bool) {
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", "", false
	}
	if env.Type != "error" {
		return "", "", false
	}
	return env.Error.Type, env.Error.Message, true
}

// peekOpenAIError returns the upstream error type/code/message when
// body parses as an OpenAI error envelope —
// `{"error":{"type":"...","code":"...","message":"..."}}` — and
// (false) for valid completions or unparseable bodies.
//
// OpenAI primarily surfaces errors as HTTP 4xx/5xx, so this helper
// exists mainly for symmetry with the Anthropic shape and for the
// rare envelope-on-200 cases that arrive via streaming.
func peekOpenAIError(body []byte) (errType, message string, ok bool) {
	// Reject obvious successful completions before unmarshaling so
	// we don't accidentally classify a `choices: [...]` body as an
	// error just because its serialization happens to include an
	// "error" key in some downstream content payload.
	var env struct {
		Choices []any `json:"choices"`
		Error   struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", "", false
	}
	if len(env.Choices) > 0 {
		return "", "", false
	}
	if env.Error.Type == "" && env.Error.Code == "" && env.Error.Message == "" {
		return "", "", false
	}
	t := env.Error.Type
	if t == "" {
		t = env.Error.Code
	}
	return t, env.Error.Message, true
}

// truncateForLog clips s to maxRunes runes, appending "…" when
// truncation happened. Used to keep upstream-error messages short
// enough that they don't blow up daemon.log when the upstream
// returns a wall-of-text body (rare but observed).
func truncateForLog(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// writeFailure records the failure to the audit log and writes the
// ADR-0010 envelope to the response. The audit_id stamped on the
// failure is included in the envelope so callers can correlate.
//
// `actor` identifies who/what produced the failure for audit purposes.
// Pass model.ActorRef{} when no concrete actor exists; the audit
// store will record the failure under the unknown-actor namespace.
func writeFailure(ctx context.Context, w http.ResponseWriter, recorder audit.Recorder, f *failure.Failure, actor model.ActorRef) {
	if recorder != nil {
		recorder.RecordFailure(ctx, f, actor)
	}
	failure.WriteHTTP(w, f)
}
