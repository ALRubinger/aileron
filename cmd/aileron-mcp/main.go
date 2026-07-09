// Package main implements the Aileron MCP server.
//
// Aileron acts as an MCP server installed into agent hosts (Claude Code,
// Cursor, Continue, etc.). When AILERON_URL is set, the server queries
// the Aileron daemon's /v1/actions endpoint at startup, generates an MCP
// tool definition for each installed action, and routes incoming
// tools/call invocations to /v1/actions/{name}/run for synchronous
// execution. This is the "MCP canonical" path for action exposure
// ratified during the working session on 2026-05-03.
//
// When AILERON_COMMS_URL + AILERON_SESSION_ID are set (e.g. when
// launched by `aileron launch`), the server additionally exposes comms
// tools — read_messages, send_message, draft_reply, http_request — that
// reach the daemon-owned comms surface via HTTP. Pre-9B these talked to
// a per-session unix socket; ADR-0012 step 9B-2 moved comms ownership
// to the daemon and switched the wire to HTTP long-poll.
//
// When AILERON_URL is set the server also exposes an always-present
// `aileron_diagnostics` tool. Action discovery is best-effort: if it
// fails (daemon unreachable, 401 unauthorized, daemon error) or returns
// zero actions, the server keeps serving the built-in tools and records
// the classified reason. `aileron_diagnostics` reports that reason and
// the remediation so an agent can answer "why are connector actions
// missing?" without an operator reading the stderr warning.
//
// The binary communicates over stdio using JSON-RPC 2.0, per the MCP
// specification.
//
// Configuration:
//
//	AILERON_URL          - URL of the Aileron daemon (e.g. http://127.0.0.1:54321).
//	                       When set, action tools are discovered and exposed.
//	AILERON_TOKEN        - Optional bearer token for authenticating with
//	                       the Aileron API.
//	AILERON_COMMS_URL    - URL of the daemon's comms surface (typically
//	                       the same as AILERON_URL). Pair with
//	                       AILERON_SESSION_ID to enable comms tools.
//	AILERON_SESSION_ID   - The launch session id the daemon stamps on
//	                       comms approval entries.
//	AILERON_MCP_REFRESH_INTERVAL
//	                     - How often (Go duration, e.g. "5s") the server
//	                       re-discovers actions from the daemon so an
//	                       install/enable/disable/remove mid-session
//	                       refreshes the agent's tool surface without a
//	                       restart. Defaults to 5s. Set to "0" to disable
//	                       the poller and freeze the tool surface at boot.
//	                       Only consulted when AILERON_URL is set.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/observability"
	"github.com/ALRubinger/aileron/internal/version"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracerName names the OTel instrumentation library reported on
// spans this binary emits. Mirrors the convention in
// internal/action and internal/observability.
const tracerName = "github.com/ALRubinger/aileron/cmd/aileron-mcp"

// defaultRefreshInterval is how often the action-refresh poller
// re-discovers actions from the daemon when AILERON_MCP_REFRESH_INTERVAL
// is unset. Short enough that an install/enable/disable surfaces to the
// agent within a couple of poll cycles, long enough that the steady-state
// GET /v1/actions load is negligible.
const defaultRefreshInterval = 5 * time.Second

// --- JSON-RPC types ---

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- MCP types ---

type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema schema `json:"inputSchema"`
}

type schema struct {
	Type       string                `json:"type"`
	Properties map[string]schemaProp `json:"properties,omitempty"`
	Required   []string              `json:"required,omitempty"`
}

type schemaProp struct {
	Type        string      `json:"type"`
	Description string      `json:"description"`
	Items       *schemaItem `json:"items,omitempty"`
}

// schemaItem is the JSON Schema `items` clause emitted for array
// properties. Empty struct serializes as `{}` (any-element), which is
// strictly more permissive than omitting the `items` field — some MCP
// hosts (Codex) project a missing `items` to `string[]`, and that
// breaks actions whose elements are objects.
type schemaItem struct {
	Type string `json:"type,omitempty"`
}

type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// --- Action discovery / execution (Aileron daemon) ---

// actionMeta is the subset of the daemon's /v1/actions response we
// need to derive an MCP tool definition. Mirrors api.Action without
// pulling the generated types into this binary.
type actionMeta struct {
	Name     string                `json:"name"`
	Body     string                `json:"body"`
	Inputs   []actionInput         `json:"inputs"`
	Match    *actionMatch          `json:"match,omitempty"`
	Approval *actionApprovalPolicy `json:"approval,omitempty"`
	// Enabled mirrors the daemon's per-action overlay state. Absent in
	// older responses; treat as enabled in that case so a daemon that
	// predates the toggle feature still exposes every installed action.
	Enabled *bool `json:"enabled,omitempty"`
}

// isEnabled reports whether the daemon currently exposes this action.
// Treats nil (older daemons) and explicit true as enabled.
func (a actionMeta) isEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

type actionApprovalPolicy struct {
	Required *bool `json:"required,omitempty"`
}

// requiresApproval reports whether the action's manifest gates
// execution on user approval. Treats unset / nil as "no approval
// required" — matching the runtime's default behavior for actions
// without an [approval] block.
func (a actionMeta) requiresApproval() bool {
	return a.Approval != nil && a.Approval.Required != nil && *a.Approval.Required
}

type actionInput struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	ItemsType   string `json:"items_type,omitempty"`
	Required    *bool  `json:"required"`
	Description string `json:"description"`
}

type actionMatch struct {
	Intent string `json:"intent"`
}

type actionListResponse struct {
	Items []actionMeta `json:"items"`
}

// --- Flight Plan discovery / launch (Aileron daemon) ---

// flightPlanMeta is the subset of the daemon's /v1/flightplans response we
// need to derive an MCP tool per installed frozen Flight Plan (#2098). It
// mirrors api.FlightPlanSummary without pulling the generated types into this
// binary, matching the actionMeta pattern.
type flightPlanMeta struct {
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	Description string             `json:"description"`
	Inputs      []flightPlanInput  `json:"inputs"`
	Outputs     []flightPlanOutput `json:"outputs"`
}

// flightPlanInput mirrors api.FlightPlanInput. Type carries the raw manifest
// type (including `timestamp`); the MCP side projects it to a JSON Schema type
// when deriving the tool input schema. Required is pointer-typed to distinguish
// an omitted field (older daemons: default required) from an explicit value.
type flightPlanInput struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	ItemsType   string `json:"items_type,omitempty"`
	Description string `json:"description"`
	Required    *bool  `json:"required"`
}

// flightPlanOutput mirrors api.FlightPlanOutput. Used only to enrich the tool
// description; it never enters the input schema.
type flightPlanOutput struct {
	Name        string `json:"name"`
	MimeType    string `json:"mime_type,omitempty"`
	Description string `json:"description,omitempty"`
}

type flightPlanListResponse struct {
	Items []flightPlanMeta `json:"items"`
}

// flightPlanLaunchRequest is the body posted to /v1/flightplans/{name}/launch.
// Version is omitted so the daemon launches the latest frozen version.
type flightPlanLaunchRequest struct {
	Inputs map[string]any `json:"inputs,omitempty"`
}

// flightPlanLaunchResponse decodes the launch result fields worth echoing to
// the LLM. Optional fields are pointer/omitempty so an absent field is
// distinguishable and the formatter surfaces only what the daemon returned.
type flightPlanLaunchResponse struct {
	ContentHash    string                     `json:"content_hash"`
	ResolvedInputs map[string]any             `json:"resolved_inputs,omitempty"`
	StepOutputs    map[string]map[string]any  `json:"step_outputs,omitempty"`
	Artifacts      []flightPlanLaunchArtifact `json:"artifacts,omitempty"`
	AuditIDs       []string                   `json:"audit_ids,omitempty"`
}

type flightPlanLaunchArtifact struct {
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Digest   string `json:"digest"`
}

// flightPlanSeamPendingResponse mirrors the daemon's 200 seam_pending body
// (#2101): the run suspended at an unfulfilled llm-seam step, and the agent
// fulfills it by producing the declared outputs and calling resume_flight_plan.
// The status discriminator lets launchFlightPlanInner branch a 200 body without
// inspecting the HTTP status.
type flightPlanSeamPendingResponse struct {
	Status string                    `json:"status"`
	RunID  string                    `json:"run_id"`
	Seam   flightPlanSeamRequestWire `json:"seam"`
}

type flightPlanSeamRequestWire struct {
	StepID   string         `json:"step_id"`
	Prompt   string         `json:"prompt,omitempty"`
	Model    string         `json:"model,omitempty"`
	Outputs  []string       `json:"outputs"`
	Bindings map[string]any `json:"bindings,omitempty"`
}

// flightPlanResumeRequest is the body posted to
// /v1/flightplans/runs/{runId}/resume. Outputs carries the suspended seam's
// declared named results (stepId → outputName → value); omitted for an
// approval-driven resume.
type flightPlanResumeRequest struct {
	Outputs map[string]map[string]any `json:"outputs,omitempty"`
}

type actionRunRequest struct {
	Args map[string]any `json:"args"`
}

type actionRunResponse struct {
	AuditID string  `json:"audit_id"`
	Result  *string `json:"result,omitempty"`
}

// actionRunPendingResponse mirrors the 202 body the daemon returns
// for approval-gated actions: a discriminator, the approval id, a
// per-approval review URL, and the message the LLM is meant to
// surface to the user verbatim. JSON shape pinned by the OpenAPI
// spec; only the fields the MCP wrapper needs are decoded.
type actionRunPendingResponse struct {
	Status     string `json:"status"`
	ApprovalID string `json:"approval_id"`
	ReviewURL  string `json:"review_url,omitempty"`
	Message    string `json:"message"`
}

// actionApprovalResult mirrors the daemon's /v1/action-approvals/{id}/result
// body. Optional fields are pointer-typed so the formatter can
// distinguish "field omitted" from "present but empty" — the daemon
// only populates the fields relevant to the current status.
type actionApprovalResult struct {
	Status  string  `json:"status"`
	AuditID *string `json:"audit_id,omitempty"`
	Result  *string `json:"result,omitempty"`
	Reason  *string `json:"reason,omitempty"`
	// Failure is the structured ADR-0010 envelope when the daemon
	// surfaces one; decoded as raw JSON so the formatter can echo it
	// without re-implementing the FailureEnvelope shape here.
	Failure json.RawMessage `json:"failure,omitempty"`
}

// discoveryReason classifies the outcome of a /v1/actions discovery
// attempt so the generic "discovery failed" warning can be replaced
// with one that names the actual failure mode, and so the synthetic
// aileron_diagnostics tool can explain it to the agent.
type discoveryReason int

const (
	// reasonOK means discovery succeeded and returned at least one action.
	reasonOK discoveryReason = iota
	// reasonUnreachable means the HTTP request never completed — the
	// daemon is not listening (connection refused, DNS, timeout).
	reasonUnreachable
	// reasonUnauthorized means the daemon returned 401 — a stale or
	// missing AILERON_TOKEN / session.
	reasonUnauthorized
	// reasonHTTPError means the daemon returned a non-200 status other
	// than 401 (e.g. 5xx), or the response body failed to decode.
	reasonHTTPError
	// reasonEmpty means discovery succeeded over the wire but the daemon
	// reported zero installed actions.
	reasonEmpty
)

// discoveryDiagnostic captures the outcome of the most recent discovery
// attempt: its classified reason, the action count on success, and (for
// failures) the underlying error for the stderr log. It is the single
// source the reason-tagged warning and the aileron_diagnostics tool both
// read from.
type discoveryDiagnostic struct {
	reason discoveryReason
	// count is the number of enabled actions discovered (only meaningful
	// when reason is reasonOK).
	count int
	// err is the transport/decode/status error, if any. Nil for reasonOK
	// and reasonEmpty.
	err error
}

// ok reports whether discovery succeeded with at least one action.
func (d discoveryDiagnostic) ok() bool { return d.reason == reasonOK }

// summary returns a short, reason-tagged phrase naming the failure mode,
// with the daemon URL interpolated where it aids diagnosis. Used as the
// human-facing line in both the stderr warning and the synthetic tool's
// output.
func (d discoveryDiagnostic) summary(url string) string {
	switch d.reason {
	case reasonOK:
		return fmt.Sprintf("discovered %d action(s) from %s", d.count, url)
	case reasonUnreachable:
		return fmt.Sprintf("daemon unreachable at %s", url)
	case reasonUnauthorized:
		return "unauthorized (stale or missing AILERON_TOKEN / session)"
	case reasonHTTPError:
		if d.err != nil {
			return "daemon error: " + d.err.Error()
		}
		return "daemon returned an unexpected response"
	case reasonEmpty:
		return fmt.Sprintf("daemon at %s returned 0 actions", url)
	default:
		return "unknown discovery state"
	}
}

// remediation returns the suggested operator fix for a failed discovery,
// or the empty string when discovery succeeded. Surfaced to the agent via
// aileron_diagnostics so "why do I only see the built-in tools?" is
// answerable without reading stderr.
func (d discoveryDiagnostic) remediation() string {
	switch d.reason {
	case reasonUnreachable:
		return "Start the daemon (e.g. run `aileron launch`) and confirm AILERON_URL points at it."
	case reasonUnauthorized:
		return "Re-authenticate: the AILERON_TOKEN or session is stale. Re-run `aileron launch` to mint a fresh token."
	case reasonHTTPError:
		return "Check the daemon logs; it returned an unexpected response to /v1/actions."
	case reasonEmpty:
		return "Install actions with `aileron action add <name>`, then they will appear without restarting."
	default:
		return ""
	}
}

// discoveryError wraps a discovery failure with its classified reason so
// callers (startup, refresh) can build a reason-tagged warning and record
// the diagnostic. Unwraps to the underlying error for callers that only
// care that discovery failed.
type discoveryError struct {
	reason discoveryReason
	err    error
}

func (e *discoveryError) Error() string { return e.err.Error() }
func (e *discoveryError) Unwrap() error { return e.err }

// aileronDiagnosticsTool is an always-present (when AILERON_URL is set)
// synthetic tool the agent can call to learn why connector actions are
// missing. Mirrors the always-on check_action_status precedent. Its
// description and call result report the classified discovery state and
// the remediation so the degraded surface is answerable from the agent.
var aileronDiagnosticsTool = toolDef{
	Name:        "aileron_diagnostics",
	Description: "Report Aileron's action-discovery health. Call this to learn why connector action tools may be missing — it distinguishes a daemon that is unreachable, an unauthorized (stale token/session) response, a daemon error, and a daemon that returned zero installed actions, and reports how many actions are currently exposed plus the remediation. Useful when the agent sees only the built-in tools and expects connector actions.",
	InputSchema: schema{Type: "object"},
}

// --- Server ---

type server struct {
	aileronURL   string
	aileronToken string
	commsURL     string
	sessionID    string
	httpClient   *http.Client
	// commsHTTPClient is a long-poll-tolerant client for the comms
	// endpoints — daemon caps its waits at the action-approval TTL
	// (5 min default). A dedicated client lets the action-discovery
	// path use a tighter timeout without affecting comms.
	commsHTTPClient *http.Client

	// actionsMu guards actionTools, actionNameMap, and discovery. The
	// request loop reads them (tools/list, tools/call) while the refresh
	// poller (refreshLoop) swaps them on change, so every access is locked.
	actionsMu sync.RWMutex
	// Discovered actions, populated at startup when AILERON_URL is set and
	// refreshed in place by the poller. Keys of actionNameMap are
	// snake_case (LLM-facing) tool names; values are manifest names
	// (kebab-case) used in /v1/actions/{name}/run.
	actionTools   []toolDef
	actionNameMap map[string]string
	// Discovered Flight Plans, exposed one MCP tool per installed frozen plan
	// (#2098), swapped by the same poller under actionsMu. Keys of
	// flightPlanNameMap are snake_case (LLM-facing) tool names; values are plan
	// names used in /v1/flightplans/{name}/launch. A plan whose tool name
	// collides with a discovered action or a static built-in is dropped at
	// discovery (the action/built-in wins), so no colliding entry is cached.
	flightPlanTools   []toolDef
	flightPlanNameMap map[string]string
	// discovery records the outcome of the most recent /v1/actions
	// discovery attempt (startup or a refresh tick). The aileron_diagnostics
	// synthetic tool reports it so the degraded state is answerable from
	// the agent, not just from stderr. Zero value (reasonOK, no count)
	// before the first attempt.
	discovery discoveryDiagnostic

	// out is where JSON-RPC responses and notifications are written
	// (os.Stdout in production). writeMu serializes writes so the
	// request loop's responses and the poller's tools/list_changed
	// notification never interleave on the wire. nil out disables
	// notification emission (the unit tests that don't exercise the
	// wire leave it nil).
	out     io.Writer
	writeMu sync.Mutex
}

// commsAvailable reports whether the env carries enough context to
// reach the daemon's comms endpoints. Both env vars must be set; a
// missing session id yields a 404 from the daemon, so fail-loud with
// "comms not available" beats a confusing 404.
func (s *server) commsAvailable() bool {
	return s.commsURL != "" && s.sessionID != ""
}

var readMessagesTool = toolDef{
	Name:        "read_messages",
	Description: "Read pending messages from communication channels (Slack, Discord). Returns unread messages from the notification queue. Messages with draft_request=true need a reply drafted — call draft_reply with the message ID and your suggested reply.",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"service": {Type: "string", Description: "Filter by service: 'slack', 'discord', or empty for all"},
			"channel": {Type: "string", Description: "Filter by channel name, or empty for all channels"},
		},
	},
}

var sendMessageTool = toolDef{
	Name:        "send_message",
	Description: "Send a message to a communication channel (Slack, Discord). Requires human approval.",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"service": {Type: "string", Description: "Target service: 'slack' or 'discord'"},
			"channel": {Type: "string", Description: "Channel name or ID to send to"},
			"body":    {Type: "string", Description: "Message text to send"},
		},
		Required: []string{"service", "channel", "body"},
	},
}

var draftReplyTool = toolDef{
	Name:        "draft_reply",
	Description: "Submit a draft reply to a message. The draft is shown to the user for review — they can send, edit, or discard it. Use this when read_messages returns messages with draft_request=true. Do NOT use send_message for drafts; Aileron handles sending after user approval.",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"message_id": {Type: "string", Description: "ID of the message to reply to (from read_messages)"},
			"body":       {Type: "string", Description: "Your suggested reply text"},
		},
		Required: []string{"message_id", "body"},
	},
}

var checkActionStatusTool = toolDef{
	Name:        "check_action_status",
	Description: "Check the status of an approval-gated action call. Call this when an earlier tool returned a `pending_approval` response carrying an approval_id, and you want to know whether the user has approved, denied, or whether the action has finished running. The response carries one of: pending_approval, running, completed (with the result), denied (with the user's reason), failed (with the failure details). Polling is optional — the user knows the next move once they see the approval prompt; this tool exists for agents that want to close the loop on an action they initiated.",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"approval_id": {Type: "string", Description: "Approval id returned from the original tool call's pending_approval response."},
		},
		Required: []string{"approval_id"},
	},
}

// resumeFlightPlanTool resumes a Flight Plan run that suspended mid-plan
// (#2101). The agent calls it after a launch (or a prior resume) returned a
// seam_pending result: it supplies the seam's declared outputs and the run
// continues, returning the next seam_pending, a pending_approval, or the final
// launch result. For an approval-driven resume (after the user approves a gated
// action) the agent calls it with only run_id and no outputs.
var resumeFlightPlanTool = toolDef{
	Name:        "resume_flight_plan",
	Description: "Resume a Flight Plan run that suspended mid-plan. Call this after a launch (or an earlier resume) returned a `seam_pending` result: the run parked at an `llm-seam` step and handed control to you. Produce the seam's declared `outputs` (named in the `seam.outputs` of the pending result), then call this tool with the `run_id` from that result and an `outputs` map keyed `{ \"<step_id>\": { \"<output_name>\": <value> } }`. The run continues and returns either the next `seam_pending` (fulfill it the same way), a `pending_approval` (a gated action needs the user's approval — surface it and, once approved, resume again with just the `run_id`), or the final completed launch result. Also call it with only `run_id` (no `outputs`) to resume after the user approved a gated action.",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"run_id":  {Type: "string", Description: "The run id from the seam_pending or pending_approval result that suspended the run."},
			"outputs": {Type: "object", Description: "Seam outputs to inject, keyed { \"<step_id>\": { \"<output_name>\": <value> } }. Supply the suspended seam step's declared outputs; omit for an approval-driven resume."},
		},
		Required: []string{"run_id"},
	},
}

var httpRequestTool = toolDef{
	Name:        "http_request",
	Description: "Make an authenticated HTTP request to a URL covered by an api_key binding. Aileron matches the URL against configured api_key bindings (vault entries with kind=api_key and a url-pattern label) and injects the secret as a Bearer token. Does NOT inject OAuth credentials — OAuth bindings are scoped per-connector and reachable only via the bound connector's actions (see `aileron action add` for installed actions and `aileron binding list` for OAuth bindings). Requires human approval.",
	InputSchema: schema{
		Type: "object",
		Properties: map[string]schemaProp{
			"method":  {Type: "string", Description: "HTTP method (GET, POST, PUT, DELETE, PATCH)"},
			"url":     {Type: "string", Description: "Target URL"},
			"headers": {Type: "string", Description: "JSON object of request headers (optional)"},
			"body":    {Type: "string", Description: "Request body string (optional)"},
		},
		Required: []string{"method", "url"},
	},
}

// handleEarlyArgs services the few CLI flags aileron-mcp recognizes
// before the JSON-RPC stdio loop starts (--version, --help, and the
// short aliases). Writes to out and returns true when the caller
// should exit. Centralized here for unit-testability.
//
// The sandbox container's validate step (ADR-0024) execs
// `aileron-mcp --version` as a smoke-check that the host-mounted
// binary is actually executable inside the container — catches the
// cross-arch ENOEXEC case `command -v` alone would miss.
func handleEarlyArgs(args []string, out io.Writer) bool {
	if len(args) <= 1 {
		return false
	}
	switch args[1] {
	case "--version", "-v":
		fmt.Fprintln(out, version.Version)
		return true
	case "--help", "-h":
		fmt.Fprintln(out, "aileron-mcp — Aileron MCP server (stdio JSON-RPC). Usage: aileron-mcp [--version|--help]. Configured via env: AILERON_URL, AILERON_TOKEN, AILERON_SESSION_ID.")
		return true
	}
	return false
}

func main() {
	if handleEarlyArgs(os.Args, os.Stdout) {
		return
	}

	// Initialize OpenTelemetry. Off by default; AILERON_OTEL_ENABLED
	// opts in. Outbound HTTP calls inject `traceparent` so the
	// daemon's middleware can root spans into the same trace tree
	// regardless of whether the agent (Claude Code, etc.) propagated
	// context across the MCP transport — MCP itself doesn't carry
	// W3C TraceContext today, so this binary is the trace's root in
	// the typical case.
	obsCfg, err := config.LoadObservabilityConfig()
	if err != nil {
		slog.Warn("observability config invalid; tracing disabled", "error", err.Error())
		obsCfg = nil
	}
	tp := observability.Init(obsCfg, slog.Default())
	defer func() { _ = tp.Shutdown(context.Background()) }()

	s := &server{
		aileronURL:   os.Getenv("AILERON_URL"),
		aileronToken: os.Getenv("AILERON_TOKEN"),
		commsURL:     os.Getenv("AILERON_COMMS_URL"),
		sessionID:    os.Getenv("AILERON_SESSION_ID"),
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		// 6-minute deadline matches the daemon's 5-minute approval TTL
		// + a small buffer so the daemon's bounded response always
		// wins over a transport timeout.
		commsHTTPClient: &http.Client{Timeout: 6 * time.Minute},
		out:             os.Stdout,
	}

	// Discover installed actions from the Aileron daemon. Best-effort:
	// if discovery fails (daemon not running yet, vault locked, etc.)
	// we proceed without action tools. Comms tools remain available.
	if s.aileronURL != "" {
		tools, nameMap, err := s.discoverActions(context.Background())
		diag := classifyDiscovery(tools, err)
		s.setDiscovery(diag)
		if err != nil {
			slog.Warn("action discovery failed; continuing without action tools",
				"url", s.aileronURL, "reason", diag.summary(s.aileronURL), "error", err)
		} else {
			s.setActions(tools, nameMap)
			if !diag.ok() {
				// Wire succeeded but the daemon exposes no actions —
				// distinct from a transport/auth failure, and worth a
				// reason-tagged line so the operator isn't left guessing.
				slog.Warn("action discovery returned no actions",
					"url", s.aileronURL, "reason", diag.summary(s.aileronURL))
			}
		}
		// Discover installed Flight Plans (#2098), exposing one tool per plan.
		// Best-effort and independent of action discovery: a failure leaves the
		// plan surface empty without disturbing the action tools. Collision
		// baseline is the action name map just discovered (nameMap); on action
		// discovery failure nameMap is nil, so only static built-ins collide.
		planTools, planNameMap, planErr := s.discoverFlightPlans(context.Background(), nameMap)
		if planErr != nil {
			slog.Warn("flight plan discovery failed; continuing without plan tools",
				"url", s.aileronURL, "error", planErr)
		} else {
			s.setFlightPlans(planTools, planNameMap)
		}
	}

	// Refresh the action surface in the background so an
	// install/enable/disable/remove mid-session reaches the agent
	// without a restart.
	s.maybeStartRefreshPoller(context.Background(), os.Getenv("AILERON_MCP_REFRESH_INTERVAL"))

	// Handle SIGTERM and SIGINT for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		os.Exit(0)
	}()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		resp := s.handle(req)
		if resp != nil {
			data, _ := json.Marshal(resp)
			s.writeLine(data)
		}
	}
}

// writeLine writes one newline-terminated JSON-RPC frame to s.out
// under writeMu so the request loop's responses and the refresh
// poller's tools/list_changed notification never interleave on the
// shared stdout stream. A nil out makes this a no-op (tests that
// don't exercise the wire).
func (s *server) writeLine(data []byte) {
	if s.out == nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	fmt.Fprintf(s.out, "%s\n", data)
}

// setActions atomically replaces the cached action tool surface. Used
// by both the startup discovery and the refresh poller; centralizes
// the lock so callers can't forget it.
func (s *server) setActions(tools []toolDef, nameMap map[string]string) {
	s.actionsMu.Lock()
	defer s.actionsMu.Unlock()
	s.actionTools = tools
	s.actionNameMap = nameMap
}

// setFlightPlans atomically replaces the cached Flight Plan tool surface,
// under the same lock setActions uses. Kept separate so a plan-only change can
// swap without touching the action cache (and vice-versa).
func (s *server) setFlightPlans(tools []toolDef, nameMap map[string]string) {
	s.actionsMu.Lock()
	defer s.actionsMu.Unlock()
	s.flightPlanTools = tools
	s.flightPlanNameMap = nameMap
}

// staticBuiltinToolNames is the closed set of always-present built-in tool
// names a discovered Flight Plan tool must never collide with. A plan whose
// normalized name lands here is dropped so the built-in always wins (#2098
// collision precedence). The comms tools are conditionally present, but a plan
// tool is still dropped on their names so the surface is deterministic
// regardless of whether the session was launched with comms.
var staticBuiltinToolNames = map[string]struct{}{
	"read_messages":       {},
	"draft_reply":         {},
	"send_message":        {},
	"http_request":        {},
	"check_action_status": {},
	"resume_flight_plan":  {},
	"aileron_diagnostics": {},
}

// classifyDiscovery turns a discoverActions result into a
// discoveryDiagnostic. A discoveryError carries its own classified
// reason; a success with zero tools is reasonEmpty (the daemon answered
// but exposes no actions); a success with tools is reasonOK. Any error
// that is not a *discoveryError is treated as an HTTP-class failure so
// the diagnostic is never silently dropped.
func classifyDiscovery(tools []toolDef, err error) discoveryDiagnostic {
	if err != nil {
		var de *discoveryError
		if errors.As(err, &de) {
			return discoveryDiagnostic{reason: de.reason, err: err}
		}
		return discoveryDiagnostic{reason: reasonHTTPError, err: err}
	}
	if len(tools) == 0 {
		return discoveryDiagnostic{reason: reasonEmpty}
	}
	return discoveryDiagnostic{reason: reasonOK, count: len(tools)}
}

// setDiscovery records the latest discovery diagnostic under the same
// lock that guards the action cache, so the aileron_diagnostics tool and
// the refresh poller never read a torn state.
func (s *server) setDiscovery(d discoveryDiagnostic) {
	s.actionsMu.Lock()
	defer s.actionsMu.Unlock()
	s.discovery = d
}

// discoveryState returns a copy of the latest discovery diagnostic under
// the read lock.
func (s *server) discoveryState() discoveryDiagnostic {
	s.actionsMu.RLock()
	defer s.actionsMu.RUnlock()
	return s.discovery
}

// refreshInterval parses the AILERON_MCP_REFRESH_INTERVAL env value
// into a poll interval. Empty falls back to the default; "0" (or any
// non-positive duration) disables the poller — ok=false. An
// unparseable value logs a warning and falls back to the default so a
// typo never silently freezes the tool surface.
func refreshInterval(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultRefreshInterval, true
	}
	// Accept a bare "0" as the explicit disable switch even though
	// time.ParseDuration would also accept "0s".
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return 0, false
		}
		return d, true
	}
	// A bare integer ("0") isn't a valid Go duration; treat 0 as
	// disable and any other bare integer as seconds for ergonomics.
	if n, err := strconv.Atoi(raw); err == nil {
		if n <= 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	slog.Warn("AILERON_MCP_REFRESH_INTERVAL is not a valid duration; using default",
		"value", raw, "default", defaultRefreshInterval)
	return defaultRefreshInterval, true
}

// maybeStartRefreshPoller starts the background action-refresh poller
// when both conditions hold: AILERON_URL is set (there is a daemon to
// poll) and the interval knob does not disable it. rawInterval is the
// AILERON_MCP_REFRESH_INTERVAL value. Returns true when a poller was
// started. Split from main so the start decision is unit-testable.
func (s *server) maybeStartRefreshPoller(ctx context.Context, rawInterval string) bool {
	if s.aileronURL == "" {
		return false
	}
	interval, ok := refreshInterval(rawInterval)
	if !ok {
		return false
	}
	go s.refreshLoop(ctx, interval)
	return true
}

// refreshLoop periodically re-discovers actions from the daemon and,
// when the resulting tool surface differs from the cached one,
// atomically swaps the cache and emits notifications/tools/list_changed
// so the host re-pulls tools/list. Hosts only re-list after this
// signal, so the proactive poll-and-notify is the mechanism — a
// refresh on tools/list alone would never fire.
//
// Failure handling honors the "must not corrupt the working tool
// surface" contract: a discovery error logs to stderr and leaves the
// existing cache untouched; the good surface is never overwritten with
// an error state.
func (s *server) refreshLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshOnce(ctx)
		}
	}
}

// refreshOnce performs a single dual-surface discovery + diff + conditional
// swap. It re-discovers both actions and Flight Plans, treating each surface's
// failure independently: a failure on one leaves that surface's cache intact
// and never touches the other. When either surface changed, it performs the
// swaps and emits exactly one notifications/tools/list_changed for the tick;
// when neither changed, it emits nothing. Split from refreshLoop so the
// per-tick behavior is unit-testable without driving a ticker. Returns true
// when the surface changed and a notification was emitted.
func (s *server) refreshOnce(ctx context.Context) bool {
	// --- Action surface (drives aileron_diagnostics) ---
	actionTools, actionNameMap, actionErr := s.discoverActions(ctx)
	diag := classifyDiscovery(actionTools, actionErr)
	s.setDiscovery(diag)
	if actionErr != nil {
		// Leave the working action surface intact; surface the failure on
		// stderr so it's visible without poisoning the tool list. The reason
		// tag distinguishes unreachable / unauthorized / daemon error.
		slog.Warn("action refresh failed; keeping existing tool surface",
			"url", s.aileronURL, "reason", diag.summary(s.aileronURL), "error", actionErr)
	}

	// The collision baseline is the action name map that will be live after
	// this tick: the freshly-discovered one on success, or the cached one when
	// action discovery failed (its surface is left intact). A plan tool colliding
	// with any of those action names, or with a static built-in, is dropped.
	s.actionsMu.RLock()
	collisionNames := s.actionNameMap
	if actionErr == nil {
		collisionNames = actionNameMap
	}
	s.actionsMu.RUnlock()

	// --- Flight Plan surface (independent of the action surface) ---
	planTools, planNameMap, planErr := s.discoverFlightPlans(ctx, collisionNames)
	if planErr != nil {
		// A plan-discovery failure must NOT clobber a healthy action surface or
		// its diagnostic. It is stderr-logged only; the plan cache is left
		// intact. aileron_diagnostics continues to report the action state.
		slog.Warn("flight plan refresh failed; keeping existing plan tool surface",
			"url", s.aileronURL, "error", planErr)
	}

	// Diff each surface independently. actionNameMap / flightPlanNameMap are
	// pure functions of their tool defs, so diffing the defs suffices.
	s.actionsMu.RLock()
	actionChanged := actionErr == nil && !toolDefsEqual(s.actionTools, actionTools)
	planChanged := planErr == nil && !toolDefsEqual(s.flightPlanTools, planTools)
	s.actionsMu.RUnlock()

	if !actionChanged && !planChanged {
		return false
	}
	if actionChanged {
		s.setActions(actionTools, actionNameMap)
	}
	if planChanged {
		s.setFlightPlans(planTools, planNameMap)
	}
	// Exactly one notification per tick regardless of how many surfaces changed.
	s.emitToolsListChanged()
	return true
}

// emitToolsListChanged writes a JSON-RPC notification telling the host
// the tool list changed. Notifications carry no id and expect no
// response, per JSON-RPC 2.0 / the MCP spec.
func (s *server) emitToolsListChanged() {
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/tools/list_changed",
	})
	s.writeLine(data)
}

// toolDefsEqual reports whether two discovered tool surfaces are
// identical (same tools, same order, same schemas). discoverActions
// preserves the daemon's response order, so order-sensitive equality
// is correct: a reordering from the daemon is a real change the host
// should see. Compared by value via the JSON shapes the structs carry.
func toolDefsEqual(a, b []toolDef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func (s *server) handle(req jsonrpcRequest) *jsonrpcResponse {
	switch req.Method {
	case "initialize":
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					// listChanged: true tells the host we proactively emit
					// notifications/tools/list_changed when the action set
					// changes mid-session, so it should re-pull tools/list
					// on that signal rather than caching the boot snapshot.
					"tools": map[string]any{
						"listChanged": true,
					},
				},
				"serverInfo": map[string]any{
					"name":    "aileron",
					"version": version.Version,
				},
			},
		}

	case "notifications/initialized":
		return nil // no response for notifications

	case "tools/list":
		tools := s.availableTools()
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": tools,
			},
		}

	case "tools/call":
		var params callToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorResponse(req.ID, -32602, "invalid params: "+err.Error())
		}
		ctx := context.Background()
		result := s.dispatchTool(ctx, params.Name, params.Arguments)
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		}

	case "ping":
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		}

	default:
		return errorResponse(req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *server) availableTools() []toolDef {
	var tools []toolDef
	if s.commsAvailable() {
		tools = append(tools, readMessagesTool, draftReplyTool, sendMessageTool, httpRequestTool)
	}
	// Dynamically discovered Aileron actions from the daemon's
	// /v1/actions endpoint (per ADR-0008 — kept in MCP shape rather
	// than the OpenAI/Anthropic shape because the agent host's MCP
	// integration is the consumer here). Read under the lock the
	// refresh poller swaps the slice under.
	s.actionsMu.RLock()
	tools = append(tools, s.actionTools...)
	// One MCP tool per installed frozen Flight Plan (#2098), appended after the
	// action tools so action ordering is stable and a colliding plan (already
	// dropped at discovery) never displaces an action.
	tools = append(tools, s.flightPlanTools...)
	s.actionsMu.RUnlock()
	// check_action_status is always available — even when no actions
	// are discovered (a fresh daemon), the agent might be working
	// against an approval id minted by an earlier session.
	tools = append(tools, checkActionStatusTool)
	// resume_flight_plan is always available: a suspended run (seam_pending or
	// pending_approval) may need resuming even in a session that discovered no
	// plan tools (the run id was minted by an earlier launch). Mirrors the
	// always-on check_action_status precedent.
	tools = append(tools, resumeFlightPlanTool)
	// aileron_diagnostics is always available whenever there is a daemon
	// to discover against (AILERON_URL set). It answers "why do I only
	// see the built-in tools?" from the agent — reporting whether
	// discovery is degraded and how to fix it — mirroring the always-on
	// check_action_status precedent.
	if s.aileronURL != "" {
		tools = append(tools, aileronDiagnosticsTool)
	}
	return tools
}

func (s *server) dispatchTool(ctx context.Context, name string, args map[string]any) toolResult {
	// Discovered actions: route to /v1/actions/{name}/run on the daemon.
	// Read the map under the lock the refresh poller swaps it under.
	s.actionsMu.RLock()
	manifestName, ok := s.actionNameMap[name]
	planName, isPlan := s.flightPlanNameMap[name]
	s.actionsMu.RUnlock()
	if ok {
		return s.runAction(ctx, manifestName, args)
	}
	// Discovered Flight Plans: route to /v1/flightplans/{name}/launch. Checked
	// after the action map so an action always wins a name collision (the
	// colliding plan is dropped at discovery, so this is defense-in-depth).
	if isPlan {
		return s.launchFlightPlan(ctx, planName, args)
	}
	// Comms tools: handled in-process via the launch product's Unix socket.
	switch name {
	case "read_messages":
		return s.readMessages(args)
	case "draft_reply":
		return s.draftReply(args)
	case "send_message":
		return s.sendMessage(args)
	case "http_request":
		return s.httpRequest(args)
	case "check_action_status":
		return s.checkActionStatus(ctx, args)
	case "resume_flight_plan":
		return s.resumeFlightPlan(ctx, args)
	case "aileron_diagnostics":
		return s.diagnostics()
	default:
		return errorResult("unknown tool: " + name)
	}
}

func (s *server) readMessages(args map[string]any) toolResult {
	if !s.commsAvailable() {
		return errorResult("comms not available (not launched via aileron)")
	}

	service, _ := args["service"].(string)
	channel, _ := args["channel"].(string)

	endpoint := s.commsEndpoint("messages")
	q := url.Values{}
	if service != "" {
		q.Set("service", service)
	}
	if channel != "" {
		q.Set("channel", channel)
	}
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}

	var resp readMessagesResponse
	if err := s.commsGET(endpoint, &resp); err != nil {
		return errorResult(err.Error())
	}
	return jsonResult(resp.Messages)
}

func (s *server) draftReply(args map[string]any) toolResult {
	if !s.commsAvailable() {
		return errorResult("comms not available (not launched via aileron)")
	}

	messageID, _ := args["message_id"].(string)
	body, _ := args["body"].(string)

	if messageID == "" || body == "" {
		return errorResult("message_id and body are required")
	}

	pending, err := s.commsPOST(s.commsEndpoint("draft"), map[string]string{
		"reply_to": messageID,
		"body":     body,
	})
	if err != nil {
		return errorResult(err.Error())
	}
	return pendingApprovalResult(pending)
}

func (s *server) sendMessage(args map[string]any) toolResult {
	if !s.commsAvailable() {
		return errorResult("comms not available (not launched via aileron)")
	}

	service, _ := args["service"].(string)
	channel, _ := args["channel"].(string)
	body, _ := args["body"].(string)

	if service == "" || channel == "" || body == "" {
		return errorResult("service, channel, and body are required")
	}

	pending, err := s.commsPOST(s.commsEndpoint("send"), map[string]string{
		"service": service,
		"channel": channel,
		"body":    body,
	})
	if err != nil {
		return errorResult(err.Error())
	}
	return pendingApprovalResult(pending)
}

func (s *server) httpRequest(args map[string]any) toolResult {
	if !s.commsAvailable() {
		return errorResult("comms not available (not launched via aileron)")
	}

	method, _ := args["method"].(string)
	url, _ := args["url"].(string)
	headers, _ := args["headers"].(string)
	body, _ := args["body"].(string)

	if method == "" || url == "" {
		return errorResult("method and url are required")
	}

	payload := map[string]string{
		"method": method,
		"url":    url,
	}
	if body != "" {
		payload["body"] = body
	}
	if headers != "" {
		payload["headers"] = headers
	}

	pending, err := s.commsPOST(s.commsEndpoint("http"), payload)
	if err != nil {
		return errorResult(err.Error())
	}
	// The HTTP call is approval-gated: the daemon answers 202 and runs
	// the request server-side once the user approves. The upstream
	// response body is carried on the approval result, retrieved later
	// via check_action_status — not returned synchronously here.
	return pendingApprovalResult(pending)
}

// --- Comms wire shapes (mirrors internal/api/openapi.yaml) ---
//
// The approval-gated POST endpoints (/comms/send, /comms/draft,
// /comms/http) return a 202 ActionRunPendingResponse, decoded via the
// shared [actionRunPendingResponse] type. Only the read path
// (/comms/messages) returns a synchronous body, modeled here.

type readMessagesResponse struct {
	Messages []commsMessage `json:"messages"`
}

type commsMessage struct {
	ID           string `json:"id"`
	Service      string `json:"service"`
	Channel      string `json:"channel"`
	Author       string `json:"author"`
	Body         string `json:"body"`
	Timestamp    string `json:"timestamp"`
	DraftRequest bool   `json:"draft_request,omitempty"`
}

// commsEndpoint composes the daemon's per-session comms URL for the
// given suffix ("messages", "send", "draft", "http"). The daemon
// expects `/v1/sessions/{sessionID}/comms/<suffix>`.
func (s *server) commsEndpoint(suffix string) string {
	return strings.TrimRight(s.commsURL, "/") + "/v1/sessions/" + s.sessionID + "/comms/" + suffix
}

// commsGET issues a GET against the daemon's comms surface and decodes
// the JSON body into out. Any non-200 status, transport error, or
// decode failure surfaces as an error so the agent sees the failure
// rather than silently dropping the call.
func (s *server) commsGET(endpoint string, out any) error {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("comms request: %w", err)
	}
	if s.aileronToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.aileronToken)
	}
	resp, err := s.commsHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// commsPOST issues a POST with the JSON-encoded body against one of the
// daemon's approval-gated comms endpoints (/comms/send, /comms/draft,
// /comms/http) and returns the 202 ActionRunPendingResponse. Those
// endpoints register a pending approval and answer 202 immediately — the
// dispatch happens server-side once the user approves, and the outcome
// is reached later via the check_action_status tool
// (GET /v1/action-approvals/{id}/result). This mirrors the
// /v1/actions/{name}/run 202 branch in [runActionInner]: the pending
// state is a successful result, not a failure. Any non-202 status,
// transport error, or decode failure is a real error and surfaces as
// one so the agent sees the call never reached the queue.
func (s *server) commsPOST(endpoint string, body any) (actionRunPendingResponse, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return actionRunPendingResponse{}, fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return actionRunPendingResponse{}, fmt.Errorf("comms request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.aileronToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.aileronToken)
	}
	resp, err := s.commsHTTPClient.Do(req)
	if err != nil {
		return actionRunPendingResponse{}, fmt.Errorf("daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted {
		return actionRunPendingResponse{}, fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(rawBody)))
	}
	var out actionRunPendingResponse
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return actionRunPendingResponse{}, fmt.Errorf("decoding pending response: %w", err)
	}
	return out, nil
}

// pendingApprovalResult turns a 202 ActionRunPendingResponse into the
// successful tool result the agent sees. The daemon's `message` is the
// human-readable instruction the LLM is meant to surface verbatim; pass
// it through unchanged. When the daemon omits it (older builds), fall
// back to a minimal instruction naming the approval id so the agent can
// still reach the outcome via check_action_status. Mirrors the 202
// branch of [runActionInner].
func pendingApprovalResult(pending actionRunPendingResponse) toolResult {
	text := pending.Message
	if text == "" {
		text = "Approval requested. Approval id: " + pending.ApprovalID
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: text}},
	}
}

// --- Action discovery / execution against the Aileron daemon ---

// setActionAuthHeaders sets the auth + launch-session headers daemon
// /v1/actions/* and /v1/action-approvals/* endpoints expect. Bearer
// token always; X-Aileron-Session-Id only when a session id is
// configured (host launch with a session, sandbox launch). The header
// name matches the shims surface (internal/sandbox/discovery/tools.go).
// Comms endpoints encode the session id in the path and don't take this
// header.
func (s *server) setActionAuthHeaders(req *http.Request) {
	if s.aileronToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.aileronToken)
	}
	if s.sessionID != "" {
		req.Header.Set("X-Aileron-Session-Id", s.sessionID)
	}
}

// discoverActions queries /v1/actions and returns one MCP tool def per
// installed action plus a snake_case → manifest-name lookup map. Per
// ADR-0008 the LLM-facing tool name is snake_case (mapped from the
// kebab-case manifest name); /v1/actions/{name}/run uses the manifest
// name, so the map lets dispatchTool route correctly.
func (s *server) discoverActions(ctx context.Context) ([]toolDef, map[string]string, error) {
	if s.aileronURL == "" {
		return nil, nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.aileronURL+"/v1/actions", nil)
	if err != nil {
		return nil, nil, err
	}
	s.setActionAuthHeaders(req)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, nil, &discoveryError{reason: reasonUnreachable, err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		reason := reasonHTTPError
		if resp.StatusCode == http.StatusUnauthorized {
			reason = reasonUnauthorized
		}
		return nil, nil, &discoveryError{reason: reason, err: fmt.Errorf("/v1/actions: %s", resp.Status)}
	}
	var alr actionListResponse
	if err := json.NewDecoder(resp.Body).Decode(&alr); err != nil {
		return nil, nil, &discoveryError{reason: reasonHTTPError, err: fmt.Errorf("decoding /v1/actions: %w", err)}
	}
	tools := make([]toolDef, 0, len(alr.Items))
	nameMap := make(map[string]string, len(alr.Items))
	for _, a := range alr.Items {
		// Disabled actions are hidden from tools/list so the LLM never
		// learns they exist this session. The refresh poller
		// (refreshLoop) re-runs this discovery on AILERON_MCP_REFRESH_INTERVAL
		// and emits notifications/tools/list_changed on a diff, so a
		// re-enable resurfaces the action without a restart. A restart
		// is only needed for static config the poller does not re-read
		// (AILERON_URL, comms env), per the sandbox MCP walkthrough.
		if !a.isEnabled() {
			continue
		}
		td := actionToolDef(a)
		tools = append(tools, td)
		nameMap[td.Name] = a.Name
	}
	return tools, nameMap, nil
}

// discoverFlightPlans queries /v1/flightplans and returns one MCP tool def per
// installed frozen Flight Plan plus a snake_case → plan-name lookup map for
// dispatch. It mirrors discoverActions: the same discoveryError classification,
// transport/status/decode failure handling, and snake_case tool naming.
//
// collisionActionNames is the action name map (snake_case tool name → manifest
// name) that will be live this tick. A plan whose normalized tool name collides
// with a discovered action or a static built-in is dropped — the action or
// built-in wins (#2098 collision precedence) — and the drop is logged so it is
// observable. Nil collisionActionNames means no action collides (e.g. actions
// discovered nothing); the static built-in set still applies.
func (s *server) discoverFlightPlans(ctx context.Context, collisionActionNames map[string]string) ([]toolDef, map[string]string, error) {
	if s.aileronURL == "" {
		return nil, nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.aileronURL+"/v1/flightplans", nil)
	if err != nil {
		return nil, nil, err
	}
	s.setActionAuthHeaders(req)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, nil, &discoveryError{reason: reasonUnreachable, err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		reason := reasonHTTPError
		if resp.StatusCode == http.StatusUnauthorized {
			reason = reasonUnauthorized
		}
		return nil, nil, &discoveryError{reason: reason, err: fmt.Errorf("/v1/flightplans: %s", resp.Status)}
	}
	var fplr flightPlanListResponse
	if err := json.NewDecoder(resp.Body).Decode(&fplr); err != nil {
		return nil, nil, &discoveryError{reason: reasonHTTPError, err: fmt.Errorf("decoding /v1/flightplans: %w", err)}
	}
	tools := make([]toolDef, 0, len(fplr.Items))
	nameMap := make(map[string]string, len(fplr.Items))
	for _, p := range fplr.Items {
		name := toolName(p.Name)
		// Collision precedence: a discovered-action tool or a static built-in
		// with the same normalized name wins; the plan is dropped.
		if _, taken := collisionActionNames[name]; taken {
			slog.Warn("dropping Flight Plan tool: name collides with a discovered action",
				"plan", p.Name, "tool", name)
			continue
		}
		if _, builtin := staticBuiltinToolNames[name]; builtin {
			slog.Warn("dropping Flight Plan tool: name collides with a built-in tool",
				"plan", p.Name, "tool", name)
			continue
		}
		// Two plans normalizing to the same tool name would also collide; the
		// first (in the daemon's sorted order) wins, the rest are dropped.
		if _, dup := nameMap[name]; dup {
			slog.Warn("dropping Flight Plan tool: name collides with another plan",
				"plan", p.Name, "tool", name)
			continue
		}
		tools = append(tools, flightPlanToolDef(p))
		nameMap[name] = p.Name
	}
	return tools, nameMap, nil
}

// flightPlanToolDef builds the MCP tool def for one Flight Plan: the snake_case
// tool name, a description naming that it launches a Flight Plan (and listing
// declared outputs), and the derived input schema.
func flightPlanToolDef(p flightPlanMeta) toolDef {
	return toolDef{
		Name:        toolName(p.Name),
		Description: deriveFlightPlanDescription(p),
		InputSchema: deriveFlightPlanInputSchema(p),
	}
}

// deriveFlightPlanDescription builds the LLM-facing description: the plan's
// declared description (or its name when empty), a note that calling the tool
// launches the deterministic Flight Plan, and the declared outputs so the LLM
// knows what the launch returns.
func deriveFlightPlanDescription(p flightPlanMeta) string {
	desc := strings.TrimSpace(p.Description)
	if desc == "" {
		desc = p.Name
	}
	desc += "\n\nLaunches the installed deterministic Flight Plan \"" + p.Name +
		"\". The plan runs its sealed steps and returns the resolved inputs, " +
		"per-step outputs, and materialized artifacts."
	if len(p.Outputs) > 0 {
		names := make([]string, 0, len(p.Outputs))
		for _, o := range p.Outputs {
			names = append(names, o.Name)
		}
		desc += " Declared outputs: " + strings.Join(names, ", ") + "."
	}
	return desc
}

// deriveFlightPlanInputSchema mirrors deriveInputSchema for a plan's declared
// inputs, projecting the raw manifest type to a JSON Schema type: `timestamp` →
// `string` (JSON Schema has no timestamp type; strict hosts like Codex reject
// unknown types), `array` emits a permissive `items: {}` (or the element type
// when declared), and every other type passes through. `required` follows the
// action convention: default true when the daemon omits the field.
func deriveFlightPlanInputSchema(p flightPlanMeta) schema {
	s := schema{Type: "object"}
	if len(p.Inputs) == 0 {
		return s
	}
	s.Properties = make(map[string]schemaProp, len(p.Inputs))
	for _, in := range p.Inputs {
		jsonType := in.Type
		if jsonType == "timestamp" {
			// timestamp is not a JSON Schema type; project to string.
			jsonType = "string"
		}
		prop := schemaProp{
			Type:        jsonType,
			Description: in.Description,
		}
		if in.Type == "array" {
			prop.Items = &schemaItem{}
			if in.ItemsType != "" {
				prop.Items.Type = in.ItemsType
			}
		}
		s.Properties[in.Name] = prop
		required := true
		if in.Required != nil {
			required = *in.Required
		}
		if required {
			s.Required = append(s.Required, in.Name)
		}
	}
	return s
}

// launchFlightPlan launches an installed frozen Flight Plan via the daemon's
// /v1/flightplans/{name}/launch endpoint and returns an MCP tool result. It
// mirrors runActionInner's auth, tracing, and error-surfacing shape.
//
// A completed launch returns the launch result; a suspend returns the pending
// envelope verbatim (seam_pending on 200, pending_approval on 202) so the agent
// knows to fulfill the seam via resume_flight_plan or approve the gated action
// (#2101). A real non-2xx error still surfaces as an IsError tool result — this
// is how a denied approval's 403 FailureEnvelope reaches the LLM.
func (s *server) launchFlightPlan(ctx context.Context, planName string, args map[string]any) toolResult {
	tracer := otel.GetTracerProvider().Tracer(tracerName)
	ctx, span := tracer.Start(ctx, "aileron.mcp.tool.call",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("aileron.flightplan.name", planName)),
	)
	defer span.End()

	res := s.launchFlightPlanInner(ctx, planName, args)
	if res.IsError {
		span.SetStatus(codes.Error, errorText(res))
	}
	return res
}

func (s *server) launchFlightPlanInner(ctx context.Context, planName string, args map[string]any) toolResult {
	if s.aileronURL == "" {
		return errorResult("Aileron daemon not configured (AILERON_URL not set)")
	}
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(flightPlanLaunchRequest{Inputs: args})
	if err != nil {
		return errorResult("encoding request: " + err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.aileronURL+"/v1/flightplans/"+planName+"/launch", bytes.NewReader(body))
	if err != nil {
		return errorResult("creating request: " + err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	s.setActionAuthHeaders(req)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return errorResult("daemon unreachable: " + err.Error())
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResult("reading response: " + err.Error())
	}
	return flightPlanRunResult(resp.StatusCode, resp.Status, rawBody, "flight plan launch failed: ")
}

// flightPlanRunResult decodes a launch-or-resume daemon response into the tool
// result the agent sees (#2101). It branches on the discriminator so launch and
// resume speak an identical suspend/complete contract:
//
//   - 200 with `status: seam_pending` → surface the seam envelope verbatim so
//     the agent produces the declared outputs and calls resume_flight_plan.
//   - 200 without a status → a completed launch result.
//   - 202 → a pending_approval; surface it via pendingApprovalResult (the agent
//     already knows to poll check_action_status).
//   - any other status → the daemon's FailureEnvelope (or body) as an IsError.
func flightPlanRunResult(statusCode int, statusLine string, rawBody []byte, failPrefix string) toolResult {
	switch statusCode {
	case http.StatusOK:
		// Probe the discriminator: a seam suspend carries status:"seam_pending";
		// a completed run carries no status.
		var probe struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rawBody, &probe); err != nil {
			return errorResult("decoding response: " + err.Error())
		}
		if probe.Status == "seam_pending" {
			var seam flightPlanSeamPendingResponse
			if err := json.Unmarshal(rawBody, &seam); err != nil {
				return errorResult("decoding seam_pending response: " + err.Error())
			}
			return jsonResult(seam)
		}
		var launched flightPlanLaunchResponse
		if err := json.Unmarshal(rawBody, &launched); err != nil {
			return errorResult("decoding response: " + err.Error())
		}
		return jsonResult(launched)
	case http.StatusAccepted:
		var pending actionRunPendingResponse
		if err := json.Unmarshal(rawBody, &pending); err != nil {
			return errorResult("decoding pending response: " + err.Error())
		}
		return pendingApprovalResult(pending)
	default:
		// Surface the FailureEnvelope (or whatever body the daemon returned)
		// verbatim so the agent sees the actionable detail. A denied approval's
		// 403 FailureEnvelope reaches the LLM here as an IsError result.
		if len(bytes.TrimSpace(rawBody)) == 0 {
			return errorResult(failPrefix + statusLine)
		}
		return errorResult(string(rawBody))
	}
}

// resumeFlightPlan resumes a suspended Flight Plan run via the daemon's
// /v1/flightplans/runs/{runId}/resume endpoint (#2101). It POSTs the seam
// outputs (if any) and handles the response identically to launch: seam_pending,
// pending_approval, completed, or error.
func (s *server) resumeFlightPlan(ctx context.Context, args map[string]any) toolResult {
	if s.aileronURL == "" {
		return errorResult("Aileron daemon not configured (AILERON_URL not set)")
	}
	runID, _ := args["run_id"].(string)
	if runID == "" {
		return errorResult("resume_flight_plan requires a run_id (from an earlier seam_pending or pending_approval result)")
	}
	var reqBody flightPlanResumeRequest
	if raw, ok := args["outputs"]; ok && raw != nil {
		outputs, ok := coerceResumeOutputs(raw)
		if !ok {
			return errorResult("resume_flight_plan outputs must be an object keyed { \"<step_id>\": { \"<output_name>\": <value> } }")
		}
		reqBody.Outputs = outputs
	}

	tracer := otel.GetTracerProvider().Tracer(tracerName)
	ctx, span := tracer.Start(ctx, "aileron.mcp.tool.call",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("aileron.flightplan.run_id", runID)),
	)
	defer span.End()

	body, err := json.Marshal(reqBody)
	if err != nil {
		return errorResult("encoding request: " + err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.aileronURL+"/v1/flightplans/runs/"+runID+"/resume", bytes.NewReader(body))
	if err != nil {
		return errorResult("creating request: " + err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	s.setActionAuthHeaders(req)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return errorResult("daemon unreachable: " + err.Error())
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResult("reading response: " + err.Error())
	}
	res := flightPlanRunResult(resp.StatusCode, resp.Status, rawBody, "flight plan resume failed: ")
	if res.IsError {
		span.SetStatus(codes.Error, errorText(res))
	}
	return res
}

// coerceResumeOutputs converts the agent-supplied outputs argument (a generic
// map decoded from JSON) into the typed stepId → outputName → value shape. It
// tolerates the JSON object shape MCP hosts deliver (map[string]any of
// map[string]any) and rejects anything that is not a nested object.
func coerceResumeOutputs(raw any) (map[string]map[string]any, bool) {
	top, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	out := make(map[string]map[string]any, len(top))
	for step, v := range top {
		named, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		out[step] = named
	}
	return out, true
}

// runAction synchronously executes an action via the Aileron daemon's
// /v1/actions/{name}/run endpoint and returns an MCP tool result.
// Failures are surfaced as toolResult{IsError: true} so the agent host
// reports them to the LLM as normal tool errors per MCP semantics.
//
// Emits an `aileron.mcp.tool.call` span around the outbound HTTP call
// and injects W3C TraceContext (`traceparent`) so the daemon's
// middleware extracts the trace and parents its action.execute /
// connector.call spans to this one. With tracing off (the default)
// span operations are no-ops — call shape unchanged.
func (s *server) runAction(ctx context.Context, manifestName string, args map[string]any) toolResult {
	tracer := otel.GetTracerProvider().Tracer(tracerName)
	ctx, span := tracer.Start(ctx, "aileron.mcp.tool.call",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("aileron.action.name", manifestName)),
	)
	defer span.End()

	res := s.runActionInner(ctx, manifestName, args)
	if res.IsError {
		span.SetStatus(codes.Error, errorText(res))
	}
	return res
}

// runActionInner is the unwrapped implementation, separated so the
// span lifecycle in [runAction] reads cleanly. Inject traceparent on
// the outbound request via the registered text-map propagator —
// W3C TraceContext is always installed (even when emission is off)
// so this is safe regardless of OTel state.
func (s *server) runActionInner(ctx context.Context, manifestName string, args map[string]any) toolResult {
	if s.aileronURL == "" {
		return errorResult("Aileron daemon not configured (AILERON_URL not set)")
	}
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(actionRunRequest{Args: args})
	if err != nil {
		return errorResult("encoding request: " + err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.aileronURL+"/v1/actions/"+manifestName+"/run", bytes.NewReader(body))
	if err != nil {
		return errorResult("creating request: " + err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	s.setActionAuthHeaders(req)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return errorResult("daemon unreachable: " + err.Error())
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResult("reading response: " + err.Error())
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var arr actionRunResponse
		if err := json.Unmarshal(rawBody, &arr); err != nil {
			return errorResult("decoding response: " + err.Error())
		}
		content := ""
		if arr.Result != nil {
			content = *arr.Result
		}
		return toolResult{
			Content: []toolContent{{Type: "text", Text: content}},
		}
	case http.StatusAccepted:
		// Approval-gated path: the daemon registered a pending entry
		// and returned the agent-facing instruction in `message`. Pass
		// it through verbatim so the LLM surfaces it to the user. Not
		// an error result — the tool call succeeded (we successfully
		// requested approval); the action's outcome is a separate
		// concern reachable via check_action_status.
		var pending actionRunPendingResponse
		if err := json.Unmarshal(rawBody, &pending); err != nil {
			return errorResult("decoding pending response: " + err.Error())
		}
		text := pending.Message
		if text == "" {
			text = "Approval requested. Approval id: " + pending.ApprovalID
		}
		return toolResult{
			Content: []toolContent{{Type: "text", Text: text}},
		}
	default:
		// Non-2xx: surface the FailureEnvelope (or whatever body the
		// daemon returned) so the agent sees the actionable detail.
		return errorResult(string(rawBody))
	}
}

// checkActionStatus implements the check_action_status MCP tool. It
// calls the daemon's `GET /v1/action-approvals/{id}/result` endpoint
// and formats the response as a single text block for the agent.
//
// The response shape varies by status: completed entries return the
// result payload; denied entries return the user's reason; failed
// entries return the failure envelope or executor-error text;
// transient statuses return only the status word. The agent decides
// how to react.
func (s *server) checkActionStatus(ctx context.Context, args map[string]any) toolResult {
	if s.aileronURL == "" {
		return errorResult("Aileron daemon not configured (AILERON_URL not set)")
	}
	approvalID, _ := args["approval_id"].(string)
	if approvalID == "" {
		return errorResult("check_action_status requires approval_id")
	}
	url := s.aileronURL + "/v1/action-approvals/" + approvalID + "/result"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errorResult("creating request: " + err.Error())
	}
	s.setActionAuthHeaders(req)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return errorResult("daemon unreachable: " + err.Error())
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResult("reading response: " + err.Error())
	}
	if resp.StatusCode == http.StatusNotFound {
		return errorResult("approval id not found: " + approvalID +
			" (the approval queue is in-memory; ids from a previous daemon process do not survive restart)")
	}
	if resp.StatusCode != http.StatusOK {
		return errorResult(string(rawBody))
	}
	var result actionApprovalResult
	if err := json.Unmarshal(rawBody, &result); err != nil {
		return errorResult("decoding response: " + err.Error())
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: formatActionApprovalResult(result)}},
	}
}

// diagnostics implements the aileron_diagnostics MCP tool. It reports
// the classified outcome of the most recent /v1/actions discovery so the
// agent can answer "why are connector actions missing?" without the
// operator reading stderr. Not an error result even when discovery is
// degraded — the tool call itself succeeded; the degraded state rides in
// the text.
func (s *server) diagnostics() toolResult {
	if s.aileronURL == "" {
		return toolResult{Content: []toolContent{{Type: "text",
			Text: "Aileron daemon not configured (AILERON_URL not set); no connector actions are expected. Only built-in tools are available."}}}
	}
	d := s.discoveryState()
	var b strings.Builder
	if d.ok() {
		fmt.Fprintf(&b, "Action discovery healthy: %s.", d.summary(s.aileronURL))
		return toolResult{Content: []toolContent{{Type: "text", Text: b.String()}}}
	}
	fmt.Fprintf(&b, "Action discovery degraded: %s.", d.summary(s.aileronURL))
	if d.reason == reasonEmpty {
		b.WriteString(" The daemon is reachable and authorized, but no actions are installed, so only the built-in tools are exposed.")
	} else {
		b.WriteString(" Only the built-in tools are exposed; connector actions could not be discovered.")
	}
	if rem := d.remediation(); rem != "" {
		b.WriteString("\nRemediation: ")
		b.WriteString(rem)
	}
	return toolResult{Content: []toolContent{{Type: "text", Text: b.String()}}}
}

// formatActionApprovalResult turns the API response into an LLM-
// friendly text block. The status word is always the first line; for
// terminal states the relevant payload follows. Kept terse — the LLM
// is the consumer here, not a human reading logs.
func formatActionApprovalResult(r actionApprovalResult) string {
	switch r.Status {
	case "completed":
		audit := ""
		if r.AuditID != nil && *r.AuditID != "" {
			audit = " (audit_id=" + *r.AuditID + ")"
		}
		result := ""
		if r.Result != nil {
			result = *r.Result
		}
		return "status: completed" + audit + "\nresult: " + result
	case "denied":
		reason := ""
		if r.Reason != nil {
			reason = *r.Reason
		}
		if reason == "" {
			return "status: denied (user did not provide a reason)"
		}
		return "status: denied\nreason: " + reason
	case "failed":
		if r.Failure != nil {
			b, _ := json.Marshal(r.Failure)
			return "status: failed\nfailure: " + string(b)
		}
		reason := ""
		if r.Reason != nil {
			reason = *r.Reason
		}
		return "status: failed\nreason: " + reason
	default:
		// pending_approval, running — terminal statuses are above.
		return "status: " + r.Status
	}
}

// errorText extracts the human-readable error text from a
// toolResult{IsError: true} for use as a span status description.
// Falls back to a generic message if the result is malformed.
func errorText(r toolResult) string {
	for _, c := range r.Content {
		if c.Type == "text" && c.Text != "" {
			return c.Text
		}
	}
	return "tool error"
}

// actionToolDef converts an action manifest into an MCP tool
// definition, mirroring augment.Derive (internal/augment/augment.go)
// but in MCP shape (name/description/inputSchema).
func actionToolDef(a actionMeta) toolDef {
	return toolDef{
		Name:        toolName(a.Name),
		Description: deriveDescription(a),
		InputSchema: deriveInputSchema(a),
	}
}

// toolName maps a manifest's kebab-case name to the snake_case
// identifier the LLM sees, per ADR-0008.
func toolName(manifestName string) string {
	return strings.ReplaceAll(manifestName, "-", "_")
}

// deriveDescription extracts the LLM-facing description from the
// action body. Strips a leading "# Heading" line; falls back to
// match.intent when the body is empty.
//
// When the action's manifest declares `[approval] required = true`,
// appends a notice describing the asynchronous approval contract: the
// tool call returns immediately with a `pending_approval` response
// (carrying the approval id and a verbatim message for the user),
// the action runs server-side after the user approves, and
// check_action_status is available for closing the loop. Tool
// descriptions are part of the MCP system context the LLM factors
// into planning, so this is the natural place for the signal — no
// mid-conversation injection.
func deriveDescription(a actionMeta) string {
	desc := deriveBaseDescription(a)
	if a.requiresApproval() {
		notice := "\n\nThis action requires user approval. Calling it does NOT block: " +
			"the daemon returns a `pending_approval` response with an `approval_id` and a `message` " +
			"naming the review URL and an `aileron open approval <id>` shell alternative. " +
			"Surface the `message` to the user verbatim — do not paraphrase the URL or the command. " +
			"The action runs server-side once the user approves; you may continue with other work. " +
			"Call `check_action_status` with the `approval_id` later if you want to learn the outcome."
		desc = strings.TrimSpace(desc) + notice
	}
	return desc
}

// deriveBaseDescription is the original deriveDescription logic
// without the approval-notice templating. Split out so the templating
// step is the only thing future readers need to understand to follow
// the approval signaling path.
func deriveBaseDescription(a actionMeta) string {
	body := strings.TrimSpace(a.Body)
	if body == "" {
		if a.Match != nil {
			return strings.TrimSpace(a.Match.Intent)
		}
		return ""
	}
	lines := strings.SplitN(body, "\n", 2)
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "# ") {
		if len(lines) == 1 {
			if a.Match != nil {
				return strings.TrimSpace(a.Match.Intent)
			}
			return ""
		}
		return strings.TrimSpace(lines[1])
	}
	return body
}

// deriveInputSchema builds the JSON Schema parameters object from the
// manifest's inputs. Mirrors augment.deriveParameters: type=object
// always, properties only when inputs exist, required when the input
// has Required=true (default true when omitted, per ADR-0003).
func deriveInputSchema(a actionMeta) schema {
	s := schema{Type: "object"}
	if len(a.Inputs) == 0 {
		return s
	}
	s.Properties = make(map[string]schemaProp, len(a.Inputs))
	for _, in := range a.Inputs {
		prop := schemaProp{
			Type:        in.Type,
			Description: in.Description,
		}
		// Array inputs always emit an `items` clause. When the manifest
		// declares `items_type`, the clause carries the element type;
		// otherwise it is an empty object (any-element). The empty
		// object is strictly more permissive than omitting `items`
		// entirely — strict-defaulting MCP hosts (Codex) treat a
		// missing `items` as `string[]`, which silently breaks
		// object-element arrays.
		if in.Type == "array" {
			prop.Items = &schemaItem{}
			if in.ItemsType != "" {
				prop.Items.Type = in.ItemsType
			}
		}
		s.Properties[in.Name] = prop
		required := true
		if in.Required != nil {
			required = *in.Required
		}
		if required {
			s.Required = append(s.Required, in.Name)
		}
	}
	return s
}

// --- Helpers ---

func jsonResult(v any) toolResult {
	data, _ := json.MarshalIndent(v, "", "  ")
	return toolResult{
		Content: []toolContent{{Type: "text", Text: string(data)}},
	}
}

func errorResult(msg string) toolResult {
	return toolResult{
		Content: []toolContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}

func errorResponse(id json.RawMessage, code int, msg string) *jsonrpcResponse {
	return &jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonrpcError{Code: code, Message: msg},
	}
}
