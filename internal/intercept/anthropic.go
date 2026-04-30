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

	"github.com/ALRubinger/aileron/internal/augment"
	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/retry"
)

// HandleAnthropic mirrors HandleOpenAI for the Anthropic Messages
// protocol. Tool calls arrive as `tool_use` content blocks on
// assistant messages; tool results inject as `tool_result` content
// blocks on a synthesized user message.
func (e *Engine) HandleAnthropic(w http.ResponseWriter, r *http.Request) {
	if e.anthropicUpstream == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_configured",
			"Anthropic gateway upstream is not configured")
		return
	}

	origBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"failed to read request body: "+err.Error())
		return
	}
	_ = r.Body.Close()

	wantStream := agentRequestedStreaming(origBody)

	derived := e.derived()
	aug, err := augment.AugmentAnthropic(origBody, derived, e.log)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"failed to augment request: "+err.Error())
		return
	}

	currentBody, err := setStream(aug.Body, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"failed to prepare upstream body: "+err.Error())
		return
	}

	ourNames := nameSet(aug.OurNames)

	gatewayActor := model.ActorRef{ID: "aileron-gateway", Type: model.ActorTypeService}

	for round := 0; round < maxRounds; round++ {
		respStatus, respHeaders, respBody, sendFailure := e.sendAnthropicWithRetry(r, currentBody)
		if sendFailure != nil {
			writeFailure(r.Context(), w, e.recorder, sendFailure, gatewayActor)
			return
		}
		if respStatus != http.StatusOK {
			passThrough(w, respStatus, respHeaders, respBody)
			return
		}

		assistantContent, toolUses, err := parseAnthropicResponse(respBody)
		if err != nil {
			passThrough(w, respStatus, respHeaders, respBody)
			return
		}

		ours, theirs := classifyAnthropicToolUses(toolUses, ourNames)

		if len(ours) == 0 {
			emitAnthropicTerminal(w, respBody, wantStream)
			return
		}
		if len(theirs) > 0 {
			writeError(w, http.StatusBadGateway, "mixed_tool_calls_unsupported",
				"the gateway does not yet support a single response that mixes Aileron tool calls with agent-declared tool calls")
			return
		}

		toolResultBlocks, execErr := e.executeAnthropicToolUses(r.Context(), ours)
		if execErr != nil {
			writeFailure(r.Context(), w, e.recorder,
				failure.ConnectorRuntime("action executor fatal error: "+execErr.Error(), false,
					failure.WithBoundary(failure.Runtime),
					failure.WithCause(execErr)),
				gatewayActor)
			return
		}

		nextBody, err := appendAnthropicContinuation(currentBody, assistantContent, toolResultBlocks)
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

// sendAnthropicWithRetry runs sendAnthropic inside the engine's retry
// policy. Mirror of sendOpenAIWithRetry.
func (e *Engine) sendAnthropicWithRetry(orig *http.Request, body []byte) (int, http.Header, []byte, *failure.Failure) {
	type result struct {
		status  int
		headers http.Header
		body    []byte
	}
	out, f := retry.Do(orig.Context(), e.retryPolicy, e.clock,
		func(ctx context.Context, _ int) (result, *failure.Failure) {
			status, hdrs, body, err := e.sendAnthropic(ctx, orig, body)
			if err != nil {
				return result{}, failure.Network(
					"upstream Anthropic request failed: "+err.Error(),
					failure.WithCause(err),
				)
			}
			if status == http.StatusTooManyRequests || (status >= 500 && status <= 599) {
				return result{}, failure.Upstream(status,
					fmt.Sprintf("upstream returned %d", status),
					failure.WithDetails(map[string]any{"body": string(body)}),
				)
			}
			return result{status: status, headers: hdrs, body: body}, nil
		})
	if f != nil {
		return 0, nil, nil, f
	}
	return out.status, out.headers, out.body, nil
}

// sendAnthropic posts the body to the configured Anthropic upstream
// and returns status, headers, and body.
func (e *Engine) sendAnthropic(ctx context.Context, orig *http.Request, body []byte) (int, http.Header, []byte, error) {
	target := *e.anthropicUpstream
	if target.Path == "" || target.Path == "/" {
		target.Path = "/v1/messages"
	} else {
		target.Path = strings.TrimRight(target.Path, "/") + "/v1/messages"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
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

// parseAnthropicResponse extracts the assistant content blocks and
// the subset that are tool_use blocks.
func parseAnthropicResponse(body []byte) (content []any, toolUses []map[string]any, err error) {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, err
	}
	if t, _ := resp["type"].(string); t != "message" {
		return nil, nil, fmt.Errorf("unexpected response type: %v", resp["type"])
	}
	rawContent, _ := resp["content"].([]any)
	for _, b := range rawContent {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if bm["type"] == "tool_use" {
			toolUses = append(toolUses, bm)
		}
	}
	return rawContent, toolUses, nil
}

func classifyAnthropicToolUses(uses []map[string]any, ours map[string]bool) (mine, theirs []map[string]any) {
	for _, u := range uses {
		name, _ := u["name"].(string)
		if ours[name] {
			mine = append(mine, u)
		} else {
			theirs = append(theirs, u)
		}
	}
	return mine, theirs
}

// executeAnthropicToolUses runs each Aileron tool_use block through
// the engine's Executor and produces matching tool_result content
// blocks for injection as a synthesized user message.
func (e *Engine) executeAnthropicToolUses(ctx context.Context, uses []map[string]any) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(uses))
	for _, u := range uses {
		id, _ := u["id"].(string)
		name, _ := u["name"].(string)
		input, _ := u["input"].(map[string]any)
		if input == nil {
			input = map[string]any{}
		}

		res, err := e.executor.Execute(ctx, name, input)
		if err != nil {
			return nil, err
		}
		if res.Failure != nil {
			e.recorder.RecordFailure(ctx, res.Failure,
				model.ActorRef{ID: name, Type: model.ActorTypeConnectorRuntime})
			out = append(out, failure.ToAnthropicToolResult(res.Failure, id))
			continue
		}
		out = append(out, map[string]any{
			"type":        "tool_result",
			"tool_use_id": id,
			"content":     res.Content,
		})
	}
	return out, nil
}

// appendAnthropicContinuation builds the next-round request by adding
// the assistant message (with the model's content blocks including
// the tool_use) and a synthesized user message carrying the
// tool_result blocks.
func appendAnthropicContinuation(currentBody []byte, assistantContent []any, toolResultBlocks []map[string]any) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(currentBody, &req); err != nil {
		return nil, err
	}
	rawMessages, _ := req["messages"].([]any)
	rawMessages = append(rawMessages, map[string]any{
		"role":    "assistant",
		"content": assistantContent,
	})
	userBlocks := make([]any, 0, len(toolResultBlocks))
	for _, b := range toolResultBlocks {
		userBlocks = append(userBlocks, b)
	}
	rawMessages = append(rawMessages, map[string]any{
		"role":    "user",
		"content": userBlocks,
	})
	req["messages"] = rawMessages
	return json.Marshal(req)
}

// emitAnthropicTerminal writes the terminal Anthropic response back
// to the agent. When the agent asked for streaming, the JSON message
// is reshaped into a minimal SSE event sequence covering the
// content blocks plus message_stop.
func emitAnthropicTerminal(w http.ResponseWriter, body []byte, wantStream bool) {
	if !wantStream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
		return
	}

	events, err := anthropicEventsFromMessage(body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, ev := range events {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// anthropicEvent is a single SSE event with a named type and JSON
// payload, matching Anthropic's streaming protocol.
type anthropicEvent struct {
	Name string
	Data string
}

// anthropicEventsFromMessage converts a non-streamed Anthropic
// Messages response into a minimal event sequence: message_start,
// then per-content-block start/delta/stop, then message_delta and
// message_stop. Sufficient for a simple agent that just needs the
// final assistant message.
func anthropicEventsFromMessage(body []byte) ([]anthropicEvent, error) {
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	content, _ := msg["content"].([]any)
	if content == nil {
		return nil, errors.New("response missing content array")
	}

	startData, _ := json.Marshal(map[string]any{
		"type":    "message_start",
		"message": msg,
	})
	events := []anthropicEvent{{Name: "message_start", Data: string(startData)}}

	for i, b := range content {
		bm, _ := b.(map[string]any)
		if bm == nil {
			continue
		}
		startBlockData, _ := json.Marshal(map[string]any{
			"type":          "content_block_start",
			"index":         i,
			"content_block": bm,
		})
		stopBlockData, _ := json.Marshal(map[string]any{
			"type":  "content_block_stop",
			"index": i,
		})
		events = append(events,
			anthropicEvent{Name: "content_block_start", Data: string(startBlockData)},
			anthropicEvent{Name: "content_block_stop", Data: string(stopBlockData)},
		)
	}

	deltaData, _ := json.Marshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": msg["stop_reason"], "stop_sequence": msg["stop_sequence"]},
		"usage": msg["usage"],
	})
	stopData, _ := json.Marshal(map[string]any{"type": "message_stop"})
	events = append(events,
		anthropicEvent{Name: "message_delta", Data: string(deltaData)},
		anthropicEvent{Name: "message_stop", Data: string(stopData)},
	)
	return events, nil
}
