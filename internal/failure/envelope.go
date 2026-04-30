package failure

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Envelope is the wire shape of the failure response described by
// ADR-0010. The JSON serialization matches the OpenAPI
// `FailureEnvelope` schema; tests in this package assert no drift.
type Envelope struct {
	Error EnvelopeError `json:"error"`
}

// EnvelopeError is the inner object of [Envelope].
type EnvelopeError struct {
	Class     FailureClass   `json:"class"`
	Message   string         `json:"message"`
	Retriable bool           `json:"retriable"`
	Boundary  Boundary       `json:"boundary"`
	AuditID   string         `json:"audit_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// ToEnvelope renders the failure as the wire-format envelope.
func ToEnvelope(f *Failure) Envelope {
	return Envelope{
		Error: EnvelopeError{
			Class:     f.class,
			Message:   f.message,
			Retriable: f.retriable,
			Boundary:  f.boundary,
			AuditID:   f.auditID,
			Details:   f.details,
		},
	}
}

// HTTPStatus returns the HTTP status code most appropriate for the
// failure's class, per ADR-0010's mapping. Boundaries don't affect
// the status; only class does.
func HTTPStatus(f *Failure) int {
	switch f.class {
	case CapabilityDenied:
		return http.StatusForbidden
	case BindingRequired, BindingFailed:
		// Both are precondition failures from the runtime's view: the
		// request is well-formed but the credential isn't usable.
		return http.StatusPreconditionFailed
	case ResourceLimitExceeded:
		return http.StatusInsufficientStorage
	case ConnectorRuntimeError:
		return http.StatusInternalServerError
	case NetworkError, ExternalAPIError:
		return http.StatusBadGateway
	case HashMismatch, SignatureFailure:
		// Integrity violations refuse to serve.
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

// WriteHTTP serialises the failure as JSON and writes it to w with
// the per-class HTTP status. Sets Content-Type to application/json.
func WriteHTTP(w http.ResponseWriter, f *Failure) {
	body, _ := json.Marshal(ToEnvelope(f))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(HTTPStatus(f))
	_, _ = w.Write(body)
}

// ToOpenAIToolMessage builds an OpenAI Chat Completions `tool` role
// message that surfaces a failure to the agent's LLM as a tool
// result. Used by the gateway's intercept layer when an Aileron tool
// call fails — the LLM sees the failure as a normal tool result and
// can decide what to do.
func ToOpenAIToolMessage(f *Failure, callID, toolName string) map[string]any {
	return map[string]any{
		"role":         "tool",
		"tool_call_id": callID,
		"name":         toolName,
		"content":      string(mustJSON(toolResultPayload(f))),
	}
}

// ToAnthropicToolResult builds an Anthropic `tool_result` content
// block carrying a failure. Anthropic uses the `is_error` flag to
// mark error results explicitly; ADR-0010's envelope is encoded into
// the content text.
func ToAnthropicToolResult(f *Failure, useID string) map[string]any {
	block := map[string]any{
		"type":        "tool_result",
		"tool_use_id": useID,
		"content":     string(mustJSON(toolResultPayload(f))),
	}
	if f.class != "" { // any structured failure is an error to the LLM
		block["is_error"] = true
	}
	return block
}

// toolResultPayload is the JSON body the LLM sees as the tool's
// content. It mirrors the envelope but without the outer `error`
// wrapper, which is HTTP-specific.
func toolResultPayload(f *Failure) any {
	return EnvelopeError{
		Class:     f.class,
		Message:   f.message,
		Retriable: f.retriable,
		Boundary:  f.boundary,
		AuditID:   f.auditID,
		Details:   f.details,
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Failures are constructed in-package; marshal can only fail
		// if a Detail value is non-marshalable. We surface that as
		// best-effort prose rather than panicking.
		return []byte(fmt.Sprintf(`{"class":"connector_runtime_error","message":"failure marshaling failed: %s"}`, err.Error()))
	}
	return b
}
