package intercept

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/augment"
)

// HandleOpenAI runs the augmentation + interception loop for an
// OpenAI Chat Completions request. The request body is read once,
// augmented with installed actions, and forwarded upstream. The
// upstream response is parsed; tool calls targeting Aileron-owned
// names are executed via the engine's Executor and the conversation
// continues with a fresh upstream call until the LLM produces a
// terminal assistant message. The final message is then written back
// to the agent — either as JSON or as a single-shot SSE stream
// depending on whether the original request set `stream: true`.
//
// Note: during intercept rounds the engine forces upstream `stream:
// false` so the per-round response is a single JSON document.
// The agent still sees a streaming response (synthesized as SSE)
// when it asked for one. True upstream-side streaming preservation
// for non-intercepted text deltas is a future optimization.
func (e *Engine) HandleOpenAI(w http.ResponseWriter, r *http.Request) {
	if e.openAIUpstream == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"OpenAI gateway upstream is not configured")
		return
	}

	origBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"failed to read request body: "+err.Error())
		return
	}
	_ = r.Body.Close()

	// Detect whether the agent asked for streaming so we can shape
	// the final response correspondingly. We don't fail when the
	// body is non-JSON; the upstream will be the authority on that.
	wantStream := agentRequestedStreaming(origBody)

	derived := e.derived()
	aug, err := augment.AugmentOpenAI(origBody, derived, e.log)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"failed to augment request: "+err.Error())
		return
	}

	// Force non-streaming upstream during intercept rounds so we get
	// a single parsable JSON document per round. The engine
	// synthesizes streaming back to the agent when needed.
	currentBody, err := setStream(aug.Body, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"failed to prepare upstream body: "+err.Error())
		return
	}

	ourNames := nameSet(aug.OurNames)

	for round := 0; round < maxRounds; round++ {
		respStatus, respHeaders, respBody, err := e.sendOpenAI(r, currentBody)
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_error",
				"upstream OpenAI request failed: "+err.Error())
			return
		}
		if respStatus != http.StatusOK {
			passThrough(w, respStatus, respHeaders, respBody)
			return
		}

		assistantMsg, toolCalls, err := parseOpenAIChoice(respBody)
		if err != nil {
			passThrough(w, respStatus, respHeaders, respBody)
			return
		}

		ours, theirs := classifyOpenAIToolCalls(toolCalls, ourNames)

		if len(ours) == 0 {
			// No Aileron tool calls — terminal for this round.
			// Either pure-text response or agent's tools to handle.
			emitOpenAITerminal(w, respBody, wantStream)
			return
		}
		if len(theirs) > 0 {
			writeError(w, http.StatusBadGateway, "mixed_tool_calls_unsupported",
				"the gateway does not yet support a single response that mixes Aileron tool calls with agent-declared tool calls")
			return
		}

		toolMessages, execErr := e.executeOpenAIToolCalls(r.Context(), ours)
		if execErr != nil {
			writeError(w, http.StatusInternalServerError, "executor_error",
				"action executor returned a fatal error: "+execErr.Error())
			return
		}

		nextBody, err := appendOpenAIContinuation(currentBody, assistantMsg, toolMessages)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "continuation_error",
				"failed to build continuation request: "+err.Error())
			return
		}
		currentBody = nextBody
	}

	writeError(w, http.StatusBadGateway, "max_intercept_rounds",
		"interception loop exceeded maximum rounds; aborting to avoid infinite cycle")
}

// sendOpenAI POSTs the given body to the configured OpenAI upstream,
// preserving auth-relevant request headers from the original request.
// Returns status, response headers, and the full response body. The
// caller is responsible for inspecting the status — non-200 responses
// are returned without error so they can be propagated to the agent
// unchanged.
func (e *Engine) sendOpenAI(orig *http.Request, body []byte) (int, http.Header, []byte, error) {
	target := *e.openAIUpstream
	if target.Path == "" || target.Path == "/" {
		target.Path = "/v1/chat/completions"
	} else {
		target.Path = strings.TrimRight(target.Path, "/") + "/v1/chat/completions"
	}
	req, err := http.NewRequestWithContext(orig.Context(), http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	copyForwardableHeaders(orig.Header, req.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header.Clone(), respBody, nil
}

// parseOpenAIChoice extracts the first choice's assistant message and
// tool_calls (if any) from a non-streamed Chat Completions response.
func parseOpenAIChoice(body []byte) (assistantMsg map[string]any, toolCalls []map[string]any, err error) {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, err
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil, errors.New("response has no choices")
	}
	choice0, _ := choices[0].(map[string]any)
	if choice0 == nil {
		return nil, nil, errors.New("choice[0] is not an object")
	}
	msg, _ := choice0["message"].(map[string]any)
	if msg == nil {
		return nil, nil, errors.New("choice[0].message missing")
	}
	rawCalls, _ := msg["tool_calls"].([]any)
	for _, rc := range rawCalls {
		tc, ok := rc.(map[string]any)
		if !ok {
			continue
		}
		toolCalls = append(toolCalls, tc)
	}
	return msg, toolCalls, nil
}

// classifyOpenAIToolCalls partitions tool calls into Aileron-owned
// and agent-declared based on the post-collision name set.
func classifyOpenAIToolCalls(calls []map[string]any, ours map[string]bool) (mine, theirs []map[string]any) {
	for _, tc := range calls {
		fn, _ := tc["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if ours[name] {
			mine = append(mine, tc)
		} else {
			theirs = append(theirs, tc)
		}
	}
	return mine, theirs
}

// executeOpenAIToolCalls runs each Aileron tool call through the
// engine's Executor and produces the matching `tool` role messages
// to inject into the continuation request.
func (e *Engine) executeOpenAIToolCalls(ctx context.Context, calls []map[string]any) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(calls))
	for _, tc := range calls {
		id, _ := tc["id"].(string)
		fn, _ := tc["function"].(map[string]any)
		name, _ := fn["name"].(string)
		argsRaw, _ := fn["arguments"].(string)

		args := map[string]any{}
		if argsRaw != "" {
			if err := json.Unmarshal([]byte(argsRaw), &args); err != nil {
				// LLM produced invalid JSON args. Surface this as a
				// tool-result error so the LLM can correct, rather
				// than failing the whole turn.
				out = append(out, openAIErrorToolMessage(id, name,
					fmt.Sprintf("invalid JSON arguments: %s", err.Error())))
				continue
			}
		}

		res, err := e.executor.Execute(ctx, name, args)
		if err != nil {
			return nil, err
		}
		content := res.Content
		if res.IsError {
			content = fmt.Sprintf(`{"error":true,"content":%s}`, jsonString(res.Content))
		}
		out = append(out, map[string]any{
			"role":         "tool",
			"tool_call_id": id,
			"name":         name,
			"content":      content,
		})
	}
	return out, nil
}

// openAIErrorToolMessage builds a synthesized tool-result message
// surfacing an action-side error to the LLM.
func openAIErrorToolMessage(id, name, msg string) map[string]any {
	return map[string]any{
		"role":         "tool",
		"tool_call_id": id,
		"name":         name,
		"content":      fmt.Sprintf(`{"error":true,"message":%s}`, jsonString(msg)),
	}
}

// appendOpenAIContinuation builds the next-round request body by
// appending the assistant message (with tool_calls) and the
// synthesized tool messages to the existing `messages` array.
func appendOpenAIContinuation(currentBody []byte, assistantMsg map[string]any, toolMessages []map[string]any) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(currentBody, &req); err != nil {
		return nil, err
	}
	rawMessages, _ := req["messages"].([]any)
	rawMessages = append(rawMessages, assistantMsg)
	for _, tm := range toolMessages {
		rawMessages = append(rawMessages, tm)
	}
	req["messages"] = rawMessages
	return json.Marshal(req)
}

// emitOpenAITerminal writes the terminal assistant response to the
// agent. When the agent requested streaming, the JSON response is
// reshaped into a single-shot SSE stream that follows the OpenAI
// chunk format.
func emitOpenAITerminal(w http.ResponseWriter, body []byte, wantStream bool) {
	if !wantStream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
		return
	}

	chunks, err := openAIChunksFromCompletion(body)
	if err != nil {
		// Fall back to JSON delivery; the agent at worst sees a
		// non-streaming response when it asked for streaming.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// openAIChunksFromCompletion converts a non-streamed Chat Completions
// response into the SSE chunk shape the agent expects. We emit two
// chunks: one carrying the message content (or the tool_calls), and
// a finish_reason chunk. This is sufficient for an agent that only
// needs the final assistant message.
func openAIChunksFromCompletion(body []byte) ([]string, error) {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil, errors.New("no choices to stream")
	}
	choice0, _ := choices[0].(map[string]any)
	msg, _ := choice0["message"].(map[string]any)
	finishReason, _ := choice0["finish_reason"].(string)

	id, _ := resp["id"].(string)
	model, _ := resp["model"].(string)
	created, _ := resp["created"].(float64)

	delta := map[string]any{}
	if role, ok := msg["role"]; ok {
		delta["role"] = role
	}
	if content, ok := msg["content"]; ok && content != nil {
		delta["content"] = content
	}
	if tc, ok := msg["tool_calls"]; ok && tc != nil {
		delta["tool_calls"] = tc
	}

	first := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": nil,
			},
		},
	}
	last := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finishReason,
			},
		},
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		return nil, err
	}
	lastJSON, err := json.Marshal(last)
	if err != nil {
		return nil, err
	}
	return []string{string(firstJSON), string(lastJSON)}, nil
}

// jsonString returns the JSON-encoded form of s with surrounding
// quotes — used to safely embed strings into hand-built JSON shapes
// (where we don't want to round-trip through json.Marshal of the
// containing object).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// _ keeps action import in scope for documentation; Executor is used
// transitively via Engine.
var _ = action.Result{}
