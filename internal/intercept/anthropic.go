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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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

	tracer := otel.GetTracerProvider().Tracer(tracerName)
	for round := 0; round < maxRounds; round++ {
		roundCtx, roundSpan := tracer.Start(r.Context(), "aileron.intercept.round",
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(
				attribute.Int("aileron.intercept.round_index", round),
				attribute.String("aileron.intercept.protocol", "anthropic"),
			),
		)
		respStatus, respHeaders, respBody, sendFailure := e.sendAnthropicWithRetry(r.WithContext(roundCtx), currentBody)
		if sendFailure != nil {
			roundSpan.SetStatus(codes.Error, sendFailure.Message())
			roundSpan.End()
			writeFailure(r.Context(), w, e.recorder, sendFailure, gatewayActor)
			return
		}
		if respStatus != http.StatusOK {
			e.log.Warn("upstream non-success",
				"protocol", "anthropic",
				"upstream_status", respStatus,
				"request_id", respHeaders.Get("request-id"),
				"round", round,
			)
			roundSpan.SetAttributes(attribute.Int("aileron.intercept.upstream_status", respStatus))
			roundSpan.End()
			passThrough(w, respStatus, respHeaders, respBody)
			return
		}

		assistantContent, toolUses, err := parseAnthropicResponse(respBody)
		if err != nil {
			// Upstream returned HTTP 200 but the body isn't a usable
			// message response. The two real-world cases:
			//   1. An error envelope wrapped in a 200 (Anthropic's
			//      `{"type":"error","error":{...}}` shape — overloaded
			//      / rate-limit / etc. arriving inside an otherwise-
			//      successful stream).
			//   2. Genuinely malformed JSON.
			// Either way the operator needs an in-band signal: without
			// this, a 200 that took 72s looks identical to a slow-but-
			// successful round in daemon.log, and the agent's SDK retry
			// (or 13-minute hang on stuttering streams) is the first
			// sign anything is wrong. The body preview carries the
			// upstream error type for grep / `aileron sessions watch`
			// without requiring provider-specific parsing here. The
			// body still flows through to the agent verbatim.
			e.log.Warn("upstream returned unparseable success body",
				"protocol", "anthropic",
				"parse_error", err.Error(),
				"body_preview", truncateForLog(string(respBody), 512),
				"request_id", respHeaders.Get("request-id"),
				"round", round,
			)
			roundSpan.SetStatus(codes.Error, "parse upstream response: "+err.Error())
			roundSpan.End()
			passThrough(w, respStatus, respHeaders, respBody)
			return
		}

		ours, theirs := classifyAnthropicToolUses(toolUses, ourNames)

		if len(ours) == 0 {
			roundSpan.SetAttributes(attribute.Bool("aileron.intercept.terminal", true))
			roundSpan.End()
			emitAnthropicTerminal(w, respBody, wantStream)
			return
		}
		if len(theirs) > 0 {
			roundSpan.SetStatus(codes.Error, "mixed_tool_calls_unsupported")
			roundSpan.End()
			writeError(w, http.StatusBadGateway, "mixed_tool_calls_unsupported",
				"the gateway does not yet support a single response that mixes Aileron tool calls with agent-declared tool calls")
			return
		}
		roundSpan.SetAttributes(attribute.Int("aileron.intercept.tool_calls_count", len(ours)))

		toolResultBlocks, execErr := e.executeAnthropicToolUses(roundCtx, ours)
		if execErr != nil {
			roundSpan.SetStatus(codes.Error, "executor fatal: "+execErr.Error())
			roundSpan.End()
			writeFailure(r.Context(), w, e.recorder,
				failure.ConnectorRuntime("action executor fatal error: "+execErr.Error(), false,
					failure.WithBoundary(failure.Runtime),
					failure.WithCause(execErr)),
				gatewayActor)
			return
		}

		nextBody, err := appendAnthropicContinuation(currentBody, assistantContent, toolResultBlocks)
		if err != nil {
			roundSpan.SetStatus(codes.Error, "continuation: "+err.Error())
			roundSpan.End()
			writeError(w, http.StatusInternalServerError, "continuation_error",
				"failed to build continuation request: "+err.Error())
			return
		}
		currentBody = nextBody
		roundSpan.End()
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
	respBody, headers, err := decompressUpstreamBody(resp.Header.Clone(), respBody)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, headers, respBody, nil
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
// Messages response into the SSE event sequence Anthropic clients
// expect when streaming: message_start, then per-content-block
// start / delta(s) / stop, then message_delta and message_stop.
//
// Each block type has its own delta wire shape per the Anthropic
// streaming protocol:
//
//   - text: start carries an empty text; text_delta delivers the
//     content.
//   - thinking: start carries empty thinking + empty signature;
//     thinking_delta delivers the reasoning text; signature_delta
//     delivers the model's signature (required when the conversation
//     involves tool use — the API rejects thinking blocks in
//     subsequent turns whose signature does not match).
//   - tool_use: start carries id + name + empty input; input_json_delta
//     delivers the input JSON.
//   - redacted_thinking: no deltas — the opaque `data` field rides on
//     the start event since it is not progressively streamed.
//
// Skipping the deltas (Aileron's pre-#638 behavior of stuffing the
// full block into content_block_start) breaks clients that accumulate
// content from delta events — Claude Code's session ends up with
// thinking blocks whose `thinking` field is empty, and the API
// rejects the next turn with "each thinking block must contain
// thinking" (#638).
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
		events = append(events, anthropicEventsForBlock(i, bm)...)
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

// anthropicEventsForBlock produces the SSE event sequence for one
// content block: a metadata-only content_block_start, zero or more
// content_block_delta events carrying the content fields, and a
// content_block_stop. The split between start and deltas matches
// Anthropic's real streaming protocol — clients accumulate content
// from deltas, not from the start event.
//
// Unknown block types fall back to the pre-#638 behavior (stuff the
// whole block into the start event, no deltas) on the principle that
// emitting something the client can ignore is better than dropping a
// block entirely. Future-protocol blocks land here.
func anthropicEventsForBlock(index int, bm map[string]any) []anthropicEvent {
	blockType, _ := bm["type"].(string)

	startBlock := map[string]any{"type": blockType}
	var deltas []map[string]any

	switch blockType {
	case "text":
		startBlock["text"] = ""
		if text, ok := bm["text"].(string); ok && text != "" {
			deltas = append(deltas, map[string]any{
				"type": "text_delta",
				"text": text,
			})
		}
	case "thinking":
		// start: empty thinking + empty signature placeholder.
		// thinking_delta: the reasoning text.
		// signature_delta: the model-issued signature (required for
		// thinking blocks that participate in a tool-use turn).
		startBlock["thinking"] = ""
		startBlock["signature"] = ""
		if thinking, ok := bm["thinking"].(string); ok && thinking != "" {
			deltas = append(deltas, map[string]any{
				"type":     "thinking_delta",
				"thinking": thinking,
			})
		}
		if sig, ok := bm["signature"].(string); ok && sig != "" {
			deltas = append(deltas, map[string]any{
				"type":      "signature_delta",
				"signature": sig,
			})
		}
	case "tool_use":
		// start: id + name + empty input. input_json_delta carries the
		// serialized input JSON in one chunk (Anthropic's real stream
		// would chunk it; one chunk is a valid sequence).
		if id, ok := bm["id"].(string); ok {
			startBlock["id"] = id
		}
		if name, ok := bm["name"].(string); ok {
			startBlock["name"] = name
		}
		startBlock["input"] = map[string]any{}
		input, hasInput := bm["input"]
		if hasInput {
			inputJSON, err := json.Marshal(input)
			if err == nil && len(inputJSON) > 0 && string(inputJSON) != "null" && string(inputJSON) != "{}" {
				deltas = append(deltas, map[string]any{
					"type":         "input_json_delta",
					"partial_json": string(inputJSON),
				})
			}
		}
	case "redacted_thinking":
		// The opaque `data` field is not progressively streamed;
		// Anthropic ships it on the start event. No deltas.
		if data, ok := bm["data"].(string); ok {
			startBlock["data"] = data
		}
	default:
		// Unknown block type. Preserve the full block in the start
		// event so the client at least sees its content; future
		// protocol blocks land here without breaking the gateway.
		startBlock = bm
	}

	startData, _ := json.Marshal(map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": startBlock,
	})
	events := []anthropicEvent{{Name: "content_block_start", Data: string(startData)}}

	for _, d := range deltas {
		deltaData, _ := json.Marshal(map[string]any{
			"type":  "content_block_delta",
			"index": index,
			"delta": d,
		})
		events = append(events, anthropicEvent{
			Name: "content_block_delta",
			Data: string(deltaData),
		})
	}

	stopData, _ := json.Marshal(map[string]any{
		"type":  "content_block_stop",
		"index": index,
	})
	events = append(events, anthropicEvent{Name: "content_block_stop", Data: string(stopData)})
	return events
}
