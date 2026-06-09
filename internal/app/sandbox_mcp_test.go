//go:build integration_sandbox

// Package app's sandbox MCP integration test exercises the load-bearing
// R6/R7 contract from the sandbox MCP parity plan (#953): an
// out-of-process `aileron-mcp` subprocess speaks JSON-RPC over stdio to
// an in-process Aileron daemon, invokes an installed action via MCP
// `tools/call`, and the daemon emits the contractual audit-event chain
// stamped with the launch session id.
//
// The test is gated behind the `integration_sandbox` build tag so it
// does not run during the normal `task test:go` suite. Run it with
//
//	task test:integration:sandbox
//
// Docker availability: the plan's preferred execution mode is to run
// `aileron-mcp` inside a sandbox container reaching the daemon via
// `host.docker.internal`. This implementation uses the plan's
// explicitly-approved fallback: `aileron-mcp` runs as a HOST subprocess
// of the test, pointing AILERON_URL at the in-process daemon's
// loopback address. The load-bearing R6/R7 assertions (round-trip
// + exclusive audit-event chain stamped with the launch session id)
// are identical regardless of whether the binary runs on the host or
// in a container; the container variant is deferred as a follow-up.
//
// TODO(#953):
//   - Promote to the in-container variant (bind-mount aileron-mcp at
//     /usr/local/bin/aileron-mcp:ro, run via docker run -i with
//     --add-host=host.docker.internal:host-gateway on Linux, daemon
//     bound to 0.0.0.0:0).
//   - Add R6b (never-approved) and R6c (concurrency) variants. R6b
//     needs a poll-on-demand assertion via the check_action_status
//     tool; R6c needs two parallel tools/call requests with distinct
//     approval_ids.
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

// draftEmailManifestNoApproval is the fixture action used for the
// no-approval round-trip. The manifest matches the plan's R7 / KTD5
// targets: the manifest name is kebab-case `draft-email`, which
// aileron-mcp maps to snake_case `draft_email` for the LLM-facing
// MCP tool name per ADR-0008.
const draftEmailManifestNoApproval = `+++
name = "draft-email"
version = "1.0.0"
source = "hub://aileron/draft-email@1.0.0"

[[requires.connectors]]
name = "github://aileron/google"
version = "1.0.0"
hash = "sha256:abc123"
capabilities = ["gmail:compose"]

[match]
intent = "draft an email"

[[execute]]
id = "draft"
connector = "github://aileron/google"
op = "draft_email"
+++

# Draft Email

Drafts an email to a recipient.
`

// draftEmailManifestApproval is the same fixture annotated with
// `[approval] required = true`, used to exercise the HITL gate.
const draftEmailManifestApproval = `+++
name = "draft-email"
version = "1.0.0"
source = "hub://aileron/draft-email@1.0.0"

[[requires.connectors]]
name = "github://aileron/google"
version = "1.0.0"
hash = "sha256:abc123"
capabilities = ["gmail:compose"]

[match]
intent = "send an email"

[[execute]]
id = "send"
connector = "github://aileron/google"
op = "send_email"

[approval]
required = true
+++

# Draft Email (approval required)

Drafts and sends an email; user must approve before delivery.
`

// mcpBinaryPath is resolved once by TestMain so each test reuses the
// same compiled binary.
var mcpBinaryPath string

// TestMain builds the aileron-mcp binary from source so the
// integration test exercises the real production binary, not a mock.
// Skips the whole package if `go build` fails (no Go toolchain in
// the test environment is the only realistic failure mode).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aileron-mcp-integration-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	bin := filepath.Join(dir, "aileron-mcp")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	cmd := exec.Command("go", "build", "-o", bin, "github.com/ALRubinger/aileron/cmd/aileron-mcp")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: build aileron-mcp: %v\n", err)
		os.Exit(1)
	}
	mcpBinaryPath = bin

	os.Exit(m.Run())
}

// daemonHarness wires up an in-process apiServer, exposes it via
// httptest.NewServer, and constructs a fresh audit MemStore +
// ActionApprovalQueue for each invocation. Mirrors the shape of
// newActionsTestServer with the small additions the integration test
// needs.
type daemonHarness struct {
	srv        *apiServer
	auditStore *audit.MemStore
	httpServer *httptest.Server
	mcpProcess *mcpProcess
}

func newDaemonHarness(t *testing.T, manifest string) *daemonHarness {
	t.Helper()
	srv := newActionsTestServer(t, map[string]string{
		"draft-email.md": manifest,
	})
	store := audit.NewMemStore()
	srv.auditRecorder = audit.NewRecorder(store, nil, nil)
	srv.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	srv.actionApprovals.SetAuditRecorder(srv.auditRecorder)
	srv.webappURL = "http://127.0.0.1:54321"

	mux := http.NewServeMux()
	api.HandlerFromMux(srv, mux)
	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)

	return &daemonHarness{
		srv:        srv,
		auditStore: store,
		httpServer: httpSrv,
	}
}

// spawnMCP starts an aileron-mcp host subprocess pointed at this
// harness's daemon URL. Returns a handle the test uses to send/receive
// JSON-RPC messages.
func (h *daemonHarness) spawnMCP(t *testing.T, sessionID, token string) *mcpProcess {
	t.Helper()
	if mcpBinaryPath == "" {
		t.Fatal("mcpBinaryPath unset; TestMain did not run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, mcpBinaryPath)
	cmd.Env = append(os.Environ(),
		"AILERON_URL="+h.httpServer.URL,
		"AILERON_TOKEN="+token,
		"AILERON_SESSION_ID="+sessionID,
		"AILERON_OTEL_ENABLED=false",
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdout pipe: %v", err)
	}
	// Drain stderr into the test log so MCP-side warnings are
	// visible on failure without polluting stdout.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start aileron-mcp: %v", err)
	}

	go func() {
		buf, _ := io.ReadAll(stderr)
		if len(buf) > 0 {
			t.Logf("aileron-mcp stderr: %s", string(buf))
		}
	}()

	p := &mcpProcess{
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReader(stdout),
		cancel: cancel,
		t:      t,
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
	})
	h.mcpProcess = p
	return p
}

// mcpProcess wraps the aileron-mcp subprocess with helpers for sending
// JSON-RPC requests over stdio and reading the matching response.
type mcpProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	cancel context.CancelFunc
	t      *testing.T
	mu     sync.Mutex
	nextID int
}

type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// request sends a JSON-RPC request, then reads JSON-RPC frames from
// stdout until it finds one whose id matches. Notifications (id=0) and
// unrelated responses are skipped.
func (p *mcpProcess) request(method string, params any) jsonrpcResponse {
	p.t.Helper()
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	p.mu.Unlock()

	req := jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		p.t.Fatalf("marshal request: %v", err)
	}
	if _, err := p.stdin.Write(append(data, '\n')); err != nil {
		p.t.Fatalf("write request %s: %v", method, err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		line, err := p.reader.ReadString('\n')
		if err != nil {
			p.t.Fatalf("read response (method=%s): %v", method, err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var resp jsonrpcResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			p.t.Logf("non-JSON line from aileron-mcp: %s", line)
			continue
		}
		if resp.ID == id {
			return resp
		}
	}
	p.t.Fatalf("timed out waiting for response to %s", method)
	return jsonrpcResponse{}
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (p *mcpProcess) notify(method string, params any) {
	p.t.Helper()
	req := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		p.t.Fatalf("marshal notification: %v", err)
	}
	if _, err := p.stdin.Write(append(data, '\n')); err != nil {
		p.t.Fatalf("write notification %s: %v", method, err)
	}
}

// initializeMCP performs the standard MCP handshake (initialize +
// notifications/initialized). Returns once the server has acknowledged
// initialize.
func initializeMCP(t *testing.T, p *mcpProcess) {
	t.Helper()
	resp := p.request("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "aileron-integration-test",
			"version": "0.0.0",
		},
	})
	if resp.Error != nil {
		t.Fatalf("initialize: %d %s", resp.Error.Code, resp.Error.Message)
	}
	p.notify("notifications/initialized", map[string]any{})
}

// listTools returns the tools the MCP server advertises.
func listTools(t *testing.T, p *mcpProcess) []map[string]any {
	t.Helper()
	resp := p.request("tools/list", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("tools/list: %d %s", resp.Error.Code, resp.Error.Message)
	}
	var payload struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		t.Fatalf("decode tools/list result: %v", err)
	}
	return payload.Tools
}

// callTool issues a tools/call and returns the parsed result.
func callTool(t *testing.T, p *mcpProcess, name string, args map[string]any) map[string]any {
	t.Helper()
	resp := p.request("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if resp.Error != nil {
		t.Fatalf("tools/call %s: %d %s", name, resp.Error.Code, resp.Error.Message)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decode tools/call result: %v", err)
	}
	return out
}

// containsTool returns true when the tools list advertises a tool with
// the given name.
func containsTool(tools []map[string]any, name string) bool {
	for _, t := range tools {
		if got, _ := t["name"].(string); got == name {
			return true
		}
	}
	return false
}

// eventChainForSession returns the chronological list of event types
// stamped with the given session id. Mirrors the helper in
// handlers_actions_test.go but local here so this file is self-contained.
func eventChainForSession(t *testing.T, store *audit.MemStore, sessionID string) []model.EventType {
	t.Helper()
	all, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	out := make([]model.EventType, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if got, _ := all[i].Payload["aileron.session.id"].(string); got == sessionID {
			out = append(out, all[i].EventType)
		}
	}
	return out
}

// waitForChain polls until the audit store contains the expected
// terminal event for the session, or the deadline expires.
func waitForChain(t *testing.T, store *audit.MemStore, sessionID string, terminal model.EventType, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		chain := eventChainForSession(t, store, sessionID)
		for _, e := range chain {
			if e == terminal {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s in session %q; chain so far: %v",
		terminal, sessionID, eventChainForSession(t, store, sessionID))
}

// TestSandboxMCP_NoApproval_RoundTripsAndEmitsAuditChain covers R6/R7
// for the synchronous path: an MCP tools/call against an installed
// action without [approval] round-trips through the daemon and emits
// exactly execution.started → execution.succeeded stamped with the
// launch session id.
func TestSandboxMCP_NoApproval_RoundTripsAndEmitsAuditChain(t *testing.T) {
	h := newDaemonHarness(t, draftEmailManifestNoApproval)

	const sessionID = "sess-no-approval"
	p := h.spawnMCP(t, sessionID, "")

	initializeMCP(t, p)
	tools := listTools(t, p)
	if !containsTool(tools, "draft_email") {
		t.Fatalf("tools/list missing draft_email; got %v", toolNames(tools))
	}

	result := callTool(t, p, "draft_email", map[string]any{
		"to":      "alice@example.com",
		"subject": "hello",
		"body":    "test body",
	})
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tools/call draft_email returned isError=true: %v", result)
	}

	want := []model.EventType{
		model.EventTypeExecutionStarted,
		model.EventTypeExecutionSucceeded,
	}
	waitForChain(t, h.auditStore, sessionID, model.EventTypeExecutionSucceeded, 5*time.Second)

	chain := eventChainForSession(t, h.auditStore, sessionID)
	if len(chain) != len(want) {
		t.Fatalf("event chain = %v; want %v", chain, want)
	}
	for i, w := range want {
		if chain[i] != w {
			t.Errorf("chain[%d] = %s; want %s", i, chain[i], w)
		}
	}
}

// TestSandboxMCP_Approval_RoundTripsWithApprovedDecide covers R6's
// load-bearing approval claim: an MCP tools/call against an
// [approval]-gated action returns the 202 pending response, registers
// an approval entry stamped with the session id, and the full event
// chain on Decide(approved=true) is approval.requested →
// approval.approved → execution.started → execution.succeeded.
func TestSandboxMCP_Approval_RoundTripsWithApprovedDecide(t *testing.T) {
	h := newDaemonHarness(t, draftEmailManifestApproval)

	const sessionID = "sess-approval-approved"
	p := h.spawnMCP(t, sessionID, "")

	initializeMCP(t, p)
	tools := listTools(t, p)
	if !containsTool(tools, "draft_email") {
		t.Fatalf("tools/list missing draft_email; got %v", toolNames(tools))
	}

	result := callTool(t, p, "draft_email", map[string]any{
		"to":      "bob@example.com",
		"subject": "needs approval",
		"body":    "body text",
	})
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tools/call returned isError=true: %v", result)
	}
	// The MCP server surfaces the daemon's 202 pending response as a
	// non-error tool result with the approval id in the text content.
	gotText := firstTextContent(result)
	if !strings.Contains(strings.ToLower(gotText), "approv") {
		t.Errorf("tool result text should mention approval; got %q", gotText)
	}

	// Daemon should now have one pending approval stamped with the
	// session id.
	pending := h.srv.actionApprovals.List()
	if len(pending) != 1 {
		t.Fatalf("pending approvals = %d; want 1", len(pending))
	}
	entry := pending[0]
	if entry.SessionID != sessionID {
		t.Errorf("pending entry SessionID = %q; want %q", entry.SessionID, sessionID)
	}
	if entry.ActionName != "draft-email" {
		t.Errorf("pending entry ActionName = %q; want draft-email", entry.ActionName)
	}

	// Approve. Background executor in executeApprovedAction runs the
	// action and emits execution.started/succeeded asynchronously.
	if err := h.srv.actionApprovals.Decide(entry.ID, true, "", nil); err != nil {
		t.Fatalf("Decide approved: %v", err)
	}

	waitForChain(t, h.auditStore, sessionID, model.EventTypeExecutionSucceeded, 5*time.Second)

	chain := eventChainForSession(t, h.auditStore, sessionID)
	want := []model.EventType{
		model.EventTypeApprovalRequested,
		model.EventTypeApprovalApproved,
		model.EventTypeExecutionStarted,
		model.EventTypeExecutionSucceeded,
	}
	if len(chain) != len(want) {
		t.Fatalf("event chain = %v; want %v", chain, want)
	}
	for i, w := range want {
		if chain[i] != w {
			t.Errorf("chain[%d] = %s; want %s", i, chain[i], w)
		}
	}
}

// TestSandboxMCP_Denied exercises R6a: when the user denies the
// approval, the audit chain is exactly approval.requested →
// approval.denied with no execution.* events.
func TestSandboxMCP_Denied(t *testing.T) {
	h := newDaemonHarness(t, draftEmailManifestApproval)

	const sessionID = "sess-approval-denied"
	p := h.spawnMCP(t, sessionID, "")

	initializeMCP(t, p)
	_ = listTools(t, p)

	result := callTool(t, p, "draft_email", map[string]any{
		"to":      "carol@example.com",
		"subject": "to deny",
		"body":    "deny me",
	})
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tools/call returned isError=true: %v", result)
	}

	pending := h.srv.actionApprovals.List()
	if len(pending) != 1 {
		t.Fatalf("pending approvals = %d; want 1", len(pending))
	}
	entry := pending[0]
	if entry.SessionID != sessionID {
		t.Errorf("pending entry SessionID = %q; want %q", entry.SessionID, sessionID)
	}

	if err := h.srv.actionApprovals.Decide(entry.ID, false, "user rejected", nil); err != nil {
		t.Fatalf("Decide denied: %v", err)
	}

	waitForChain(t, h.auditStore, sessionID, model.EventTypeApprovalDenied, 5*time.Second)
	// Give the background goroutine a small grace window — it should
	// short-circuit on the denial and emit zero execution.* events.
	time.Sleep(100 * time.Millisecond)

	chain := eventChainForSession(t, h.auditStore, sessionID)
	for _, evt := range chain {
		if strings.HasPrefix(string(evt), "execution.") {
			t.Errorf("denied path emitted %s; expected no execution.* events", evt)
		}
	}
	want := []model.EventType{
		model.EventTypeApprovalRequested,
		model.EventTypeApprovalDenied,
	}
	if len(chain) != len(want) {
		t.Fatalf("event chain = %v; want %v", chain, want)
	}
	for i, w := range want {
		if chain[i] != w {
			t.Errorf("chain[%d] = %s; want %s", i, chain[i], w)
		}
	}
}

// firstTextContent extracts the first text-typed content entry from an
// MCP tool result payload. Returns the empty string when no text
// content is present.
func firstTextContent(result map[string]any) string {
	content, _ := result["content"].([]any)
	for _, c := range content {
		entry, _ := c.(map[string]any)
		if entry == nil {
			continue
		}
		if t, _ := entry["type"].(string); t != "text" {
			continue
		}
		if txt, ok := entry["text"].(string); ok {
			return txt
		}
	}
	return ""
}

// toolNames extracts tool names from a tools/list response for
// diagnostic messages.
func toolNames(tools []map[string]any) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if n, _ := t["name"].(string); n != "" {
			out = append(out, n)
		}
	}
	return out
}
