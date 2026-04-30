// Package intercept implements the dual-protocol tool-call interception
// loop ratified by [ADR-0008]. When the upstream LLM emits a tool call
// that targets an Aileron-augmented action, the engine intercepts it,
// runs the action through an [action.Executor], injects the synthetic
// tool result back into the conversation, and continues with a fresh
// upstream call until the LLM produces a final assistant message.
//
// Tool calls for agent-declared tools pass through unchanged — the
// agent's host code executes them as it always has.
//
// [ADR-0008]: https://docs.withaileron.ai/adr/0008-intent-matching
package intercept

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/augment"
)

// maxRounds caps the number of intercept iterations a single agent
// request can drive. Without a cap, a misbehaving LLM/action pair
// could loop forever; ten back-to-back tool calls is well above the
// observed ceiling for realistic conversations.
const maxRounds = 10

// Engine runs the augmentation+interception loop for both OpenAI and
// Anthropic protocol shapes.
type Engine struct {
	openAIUpstream    *url.URL
	anthropicUpstream *url.URL
	actions           *action.Store
	executor          action.Executor
	httpClient        *http.Client
	log               *slog.Logger
}

// Config groups the dependencies required to build an Engine. The
// constructor validates that the required fields are present so
// misconfiguration is caught at startup rather than on the first
// request.
type Config struct {
	OpenAIUpstream    *url.URL
	AnthropicUpstream *url.URL
	Actions           *action.Store
	Executor          action.Executor
	HTTPClient        *http.Client // optional; defaults to http.DefaultClient
	Log               *slog.Logger // optional; defaults to slog.Default()
}

// New builds an Engine from a Config.
func New(cfg Config) (*Engine, error) {
	if cfg.Actions == nil {
		return nil, errors.New("intercept: Actions store is required")
	}
	if cfg.Executor == nil {
		return nil, errors.New("intercept: Executor is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		openAIUpstream:    cfg.OpenAIUpstream,
		anthropicUpstream: cfg.AnthropicUpstream,
		actions:           cfg.Actions,
		executor:          cfg.Executor,
		httpClient:        client,
		log:               log,
	}, nil
}

// derived returns the protocol-neutral DerivedAction list for every
// installed action. An empty list means the engine should defer to a
// transparent passthrough caller — there is nothing to augment or
// intercept.
func (e *Engine) derived() []augment.DerivedAction {
	return augment.DeriveAll(e.actions.List())
}
