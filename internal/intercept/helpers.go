package intercept

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

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

// decompressUpstreamBody returns the body in its decoded form when
// the upstream applied a Content-Encoding the gateway recognizes.
// v1 supports `gzip` (the only encoding observed in the wild from
// Anthropic and OpenAI), plus the no-op `identity` and absent header.
//
// The Claude / OpenAI SDKs send `Accept-Encoding: gzip` and
// copyForwardableHeaders forwards it upstream verbatim. Go's
// http.Transport only auto-decompresses when *it* added the header,
// so when the agent's SDK sets it explicitly the response body comes
// back compressed and io.ReadAll yields the raw gzipped bytes — which
// then fails JSON parse and breaks the gateway's tool-call intercept
// for that round.
//
// On a recognized encoding the returned headers strip Content-Encoding
// and rewrite Content-Length so passThrough emits a coherent
// uncompressed response to the agent. Unknown encodings (e.g. `br`,
// `zstd`, `deflate`) and absent headers return the body and headers
// unchanged — the parse path will still fail and log a body preview,
// matching the pre-fix behavior for those cases.
func decompressUpstreamBody(headers http.Header, body []byte) ([]byte, http.Header, error) {
	enc := strings.TrimSpace(strings.ToLower(headers.Get("Content-Encoding")))
	if enc == "" || enc == "identity" || enc != "gzip" {
		return body, headers, nil
	}
	if len(body) == 0 {
		// gzip.NewReader on an empty stream returns io.EOF; treat
		// that as "nothing to decompress" rather than an error.
		return body, headers, nil
	}
	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gr.Close()
	decoded, err := io.ReadAll(gr)
	if err != nil {
		return nil, nil, fmt.Errorf("gzip read: %w", err)
	}
	out := headers.Clone()
	out.Del("Content-Encoding")
	out.Set("Content-Length", strconv.Itoa(len(decoded)))
	return decoded, out, nil
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

// truncateForLog clips s to maxRunes runes, appending "…" when
// truncation happened. Used to bound upstream body previews in the
// daemon log when an upstream returns a non-success body inside an
// HTTP 200 envelope (e.g. Anthropic's overloaded_error wrapper) —
// the body itself carries the operator-actionable signal but can be
// arbitrarily large.
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
