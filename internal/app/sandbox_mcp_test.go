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
// Transports (#960): the round-trip runs over two transports selected by
// spawnMCP. The HOST transport runs `aileron-mcp` as a subprocess of the
// test reaching the daemon over loopback — the always-on, Docker-free
// path. The CONTAINER transport runs it inside a sandbox container via
// `docker run -i` (binary bind-mounted read-only, daemon reached through
// `host.docker.internal`, `--add-host` on Linux); it is Docker-gated and
// skips when Docker or the linux cross-build is unavailable. The
// load-bearing R6/R7 assertions (round-trip + exclusive audit-event
// chain stamped with the launch session id) are identical across
// transports, so the in-container variant asserts the same chain.
//
// Variants covered here:
//   - R6/R7 no-approval and approval round-trips (host + in-container).
//   - R6a denied path (no execution.* after denial).
//   - R6b never-approved: check_action_status stays pending, no
//     execution.* events, queue entry unchanged.
//   - R6c concurrency: two parallel calls yield distinct approval ids
//     with correctly attributed, isolated per-approval event chains.
//
// Still deferred (tracked separately, not by this file):
//   - Per-agent E2E beyond aileron-mcp (Codex/Goose/OpenCode/Pi as MCP
//     client) — unit coverage + the manual recipe (#962) cover those.
//   - Wiring this tag into a CI job — separate follow-up once stable.
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/action"
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
// same compiled binary. It targets the host platform and is used by the
// host-subprocess transport.
var mcpBinaryPath string

// mcpBinaryPathLinux is a linux/<host-arch> cross-build of aileron-mcp,
// resolved by TestMain only when Docker is available. The in-container
// transport bind-mounts this binary — a host-platform build (e.g. a
// darwin binary on macOS) cannot execute inside the linux sandbox image.
var mcpBinaryPathLinux string

// sandboxBaseImage is the container image the in-container transport
// runs aileron-mcp inside. A small glibc image with /bin/sh; distroless
// static images omit the shell aileron-mcp's process path may rely on.
const sandboxBaseImage = "debian:bookworm-slim"

// dockerAvailable reports whether a `docker` CLI is on PATH. The
// in-container variant is skipped (not failed) when it is absent.
func dockerAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// skipWithoutDocker skips the calling test when Docker is unavailable,
// so the host-transport, never-approved, and concurrency variants still
// run on machines and CI lanes without Docker.
func skipWithoutDocker(t *testing.T) {
	t.Helper()
	if !dockerAvailable() {
		t.Skip("docker not on PATH; skipping in-container variant")
	}
}

// TestMain builds the aileron-mcp binary from source so the
// integration test exercises the real production binary, not a mock.
// Skips the whole package if `go build` fails (no Go toolchain in
// the test environment is the only realistic failure mode).
//
// When Docker is available it also (a) cross-builds a linux binary for
// the in-container transport and (b) pre-pulls the sandbox base image,
// so the per-test container run does not pay a first-touch pull (which
// is also where registry rate-limiting bites on shared CI runners).
// Both are best-effort: a failure logs and leaves mcpBinaryPathLinux
// unset, and the in-container variant skips with a clear reason rather
// than hanging mid-test.
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

	if dockerAvailable() {
		linuxBin := filepath.Join(dir, "aileron-mcp-linux")
		build := exec.Command("go", "build", "-o", linuxBin, "github.com/ALRubinger/aileron/cmd/aileron-mcp")
		// Match the container's default platform: Docker Desktop runs the
		// host architecture, so a linux/<host-arch> build executes natively.
		build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: cross-build linux aileron-mcp (in-container variant will skip): %v\n", err)
		} else {
			mcpBinaryPathLinux = linuxBin
		}
		pull := exec.Command("docker", "pull", sandboxBaseImage)
		pull.Stderr = os.Stderr
		if err := pull.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: pre-pull %s (in-container variant may pull on demand or skip): %v\n", sandboxBaseImage, err)
		}
	}

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
	// loopbackURL is the daemon URL the host transport uses
	// (127.0.0.1:<port>). containerURL is the same daemon reached from
	// inside a container via host.docker.internal:<port>. Both resolve
	// to the one listener bound on 0.0.0.0 below.
	loopbackURL  string
	containerURL string
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

	// Bind on 0.0.0.0 (not the httptest default of 127.0.0.1) so a
	// sandbox container can reach the daemon via host.docker.internal.
	// The host transport still connects over loopback to the same port.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen 0.0.0.0: %v", err)
	}
	httpSrv := httptest.NewUnstartedServer(mux)
	httpSrv.Listener.Close()
	httpSrv.Listener = ln
	httpSrv.Start()
	t.Cleanup(httpSrv.Close)

	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	return &daemonHarness{
		srv:          srv,
		auditStore:   store,
		httpServer:   httpSrv,
		loopbackURL:  "http://127.0.0.1:" + port,
		containerURL: "http://host.docker.internal:" + port,
	}
}

// mcpTransport selects where aileron-mcp runs for a given spawn.
type mcpTransport int

const (
	// transportHost runs aileron-mcp as a host subprocess reaching the
	// daemon over loopback — the fast, Docker-free path.
	transportHost mcpTransport = iota
	// transportContainer runs aileron-mcp inside a sandbox container via
	// `docker run -i`, reaching the daemon through host.docker.internal.
	transportContainer
)

// spawnMCP starts an aileron-mcp process pointed at this harness's
// daemon and returns a handle for JSON-RPC over its stdio. The transport
// selects host-subprocess vs in-container execution; both yield an
// identical *mcpProcess so every downstream helper is transport-agnostic.
func (h *daemonHarness) spawnMCP(t *testing.T, transport mcpTransport, sessionID, token string) *mcpProcess {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	var cmd *exec.Cmd
	switch transport {
	case transportContainer:
		if mcpBinaryPathLinux == "" {
			cancel()
			t.Skip("linux aileron-mcp cross-build unavailable; skipping in-container variant")
		}
		args := []string{"run", "-i", "--rm"}
		// macOS/Windows Docker Desktop provide host.docker.internal
		// natively; rootful Linux Docker needs this explicit mapping.
		if runtime.GOOS == "linux" {
			args = append(args, "--add-host=host.docker.internal:host-gateway")
		}
		args = append(args,
			"-v", mcpBinaryPathLinux+":/usr/local/bin/aileron-mcp:ro",
			"-e", "AILERON_URL="+h.containerURL,
			"-e", "AILERON_TOKEN="+token,
			"-e", "AILERON_SESSION_ID="+sessionID,
			"-e", "AILERON_OTEL_ENABLED=false",
			sandboxBaseImage,
			"/usr/local/bin/aileron-mcp",
		)
		cmd = exec.CommandContext(ctx, "docker", args...)
	default:
		if mcpBinaryPath == "" {
			cancel()
			t.Fatal("mcpBinaryPath unset; TestMain did not run")
		}
		cmd = exec.CommandContext(ctx, mcpBinaryPath)
		cmd.Env = append(os.Environ(),
			"AILERON_URL="+h.loopbackURL,
			"AILERON_TOKEN="+token,
			"AILERON_SESSION_ID="+sessionID,
			"AILERON_OTEL_ENABLED=false",
		)
	}

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

// request sends a JSON-RPC request and fails the test on any transport
// error. It wraps requestErr for the common case where an error is
// fatal to the test.
func (p *mcpProcess) request(method string, params any) jsonrpcResponse {
	p.t.Helper()
	resp, err := p.requestErr(method, params)
	if err != nil {
		p.t.Fatalf("%v", err)
	}
	return resp
}

// requestErr sends a JSON-RPC request, then reads JSON-RPC frames from
// stdout until it finds one whose id matches. Notifications (id=0) and
// unrelated responses are skipped. It returns an error instead of
// failing the test so callers running in a separate goroutine (the
// concurrency variant) do not call t.Fatalf off the test goroutine.
// Each mcpProcess has its own reader, so distinct processes may run
// requestErr concurrently; a single process must not (the reader is not
// demultiplexed).
func (p *mcpProcess) requestErr(method string, params any) (jsonrpcResponse, error) {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	p.mu.Unlock()

	req := jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return jsonrpcResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	if _, err := p.stdin.Write(append(data, '\n')); err != nil {
		return jsonrpcResponse{}, fmt.Errorf("write request %s: %w", method, err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		line, err := p.reader.ReadString('\n')
		if err != nil {
			return jsonrpcResponse{}, fmt.Errorf("read response (method=%s): %w", method, err)
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
			return resp, nil
		}
	}
	return jsonrpcResponse{}, fmt.Errorf("timed out waiting for response to %s", method)
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
	p := h.spawnMCP(t, transportHost, sessionID, "")

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

// TestSandboxMCP_InContainer_NoApproval_RoundTripsAndEmitsAuditChain is
// the in-container counterpart of the no-approval round-trip (R1): it
// runs aileron-mcp inside a real sandbox container reaching the daemon
// via host.docker.internal, and asserts the identical R6/R7 audit chain.
// The R6/R7 contract is transport-independent, so the assertion set
// matches the host variant exactly — the only added surface under test
// is container networking and the read-only binary bind-mount. Skips
// when Docker is unavailable.
func TestSandboxMCP_InContainer_NoApproval_RoundTripsAndEmitsAuditChain(t *testing.T) {
	skipWithoutDocker(t)
	h := newDaemonHarness(t, draftEmailManifestNoApproval)

	const sessionID = "sess-in-container"
	p := h.spawnMCP(t, transportContainer, sessionID, "")

	initializeMCP(t, p)
	tools := listTools(t, p)
	if !containsTool(tools, "draft_email") {
		t.Fatalf("tools/list missing draft_email; got %v", toolNames(tools))
	}

	result := callTool(t, p, "draft_email", map[string]any{
		"to":      "alice@example.com",
		"subject": "hello from container",
		"body":    "test body",
	})
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("in-container tools/call draft_email returned isError=true: %v", result)
	}

	want := []model.EventType{
		model.EventTypeExecutionStarted,
		model.EventTypeExecutionSucceeded,
	}
	// Container start + networking add latency over the host subprocess;
	// give the round-trip a wider window than the host variant's 5s.
	waitForChain(t, h.auditStore, sessionID, model.EventTypeExecutionSucceeded, 30*time.Second)

	chain := eventChainForSession(t, h.auditStore, sessionID)
	if len(chain) != len(want) {
		t.Fatalf("in-container event chain = %v; want %v", chain, want)
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
	p := h.spawnMCP(t, transportHost, sessionID, "")

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
	if len(chain) != 4 {
		t.Fatalf("event chain = %v; want 4 events (approval.requested, approval.approved, execution.started, execution.succeeded)", chain)
	}
	// approval.requested is always first and execution.succeeded always last.
	// The middle pair (approval.approved from the Decide path, execution.started
	// from the background executor goroutine) is emitted from two goroutines with
	// no guaranteed audit-write ordering, so assert it as an unordered set rather
	// than a fixed sequence. Approval still causally precedes execution; only the
	// two log writes may interleave.
	if chain[0] != model.EventTypeApprovalRequested {
		t.Errorf("chain[0] = %s; want %s", chain[0], model.EventTypeApprovalRequested)
	}
	if chain[3] != model.EventTypeExecutionSucceeded {
		t.Errorf("chain[3] = %s; want %s", chain[3], model.EventTypeExecutionSucceeded)
	}
	middle := map[model.EventType]bool{chain[1]: true, chain[2]: true}
	if !middle[model.EventTypeApprovalApproved] || !middle[model.EventTypeExecutionStarted] {
		t.Errorf("chain middle = [%s %s]; want {approval.approved, execution.started} in either order", chain[1], chain[2])
	}
}

// TestSandboxMCP_Denied exercises R6a: when the user denies the
// approval, the audit chain is exactly approval.requested →
// approval.denied with no execution.* events.
func TestSandboxMCP_Denied(t *testing.T) {
	h := newDaemonHarness(t, draftEmailManifestApproval)

	const sessionID = "sess-approval-denied"
	p := h.spawnMCP(t, transportHost, sessionID, "")

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

// TestSandboxMCP_NeverApproved_StaysPendingNoExecution covers R6b: an
// [approval]-gated action whose approval is never decided must stay
// pending across polls, emit no execution.* events, and leave the
// approval queue unchanged. The contract is transport-independent, so
// this runs on the host transport (Docker-free).
func TestSandboxMCP_NeverApproved_StaysPendingNoExecution(t *testing.T) {
	h := newDaemonHarness(t, draftEmailManifestApproval)

	const sessionID = "sess-never-approved"
	p := h.spawnMCP(t, transportHost, sessionID, "")

	initializeMCP(t, p)
	_ = listTools(t, p)

	result := callTool(t, p, "draft_email", map[string]any{
		"to":      "dave@example.com",
		"subject": "never decided",
		"body":    "pending forever",
	})
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tools/call returned isError=true: %v", result)
	}

	pending := h.srv.actionApprovals.List()
	if len(pending) != 1 {
		t.Fatalf("pending approvals = %d; want 1", len(pending))
	}
	entry := pending[0]

	// Poll check_action_status twice, 100ms apart. Decide is never
	// called: each poll must report a non-terminal (pending) status.
	for i := 0; i < 2; i++ {
		statusResult := callTool(t, p, "check_action_status", map[string]any{"approval_id": entry.ID})
		text := strings.ToLower(firstTextContent(statusResult))
		if !strings.Contains(text, "pending") {
			t.Fatalf("poll %d: check_action_status = %q; want a pending status", i, text)
		}
		for _, terminal := range []string{"completed", "denied", "failed"} {
			if strings.Contains(text, terminal) {
				t.Fatalf("poll %d: undecided approval reported terminal status %q in %q", i, terminal, text)
			}
		}
		if i == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}

	// The audit chain must be exactly approval.requested — no execution.*
	// events fire for an action that was never approved.
	chain := eventChainForSession(t, h.auditStore, sessionID)
	for _, evt := range chain {
		if strings.HasPrefix(string(evt), "execution.") {
			t.Errorf("never-approved path emitted %s; expected no execution.* events", evt)
		}
	}
	want := []model.EventType{model.EventTypeApprovalRequested}
	if len(chain) != len(want) {
		t.Fatalf("event chain = %v; want %v", chain, want)
	}
	if chain[0] != want[0] {
		t.Errorf("chain[0] = %s; want %s", chain[0], want[0])
	}

	// The entry must still be pending and unchanged — polling must not
	// mutate queue state. No explicit delete is needed: newDaemonHarness
	// gives each test a fresh in-memory queue, so the undecided entry
	// cannot leak into another test.
	stillPending := h.srv.actionApprovals.List()
	if len(stillPending) != 1 || stillPending[0].ID != entry.ID {
		t.Fatalf("approval entry should remain pending and unchanged; got %d entries", len(stillPending))
	}
}

// recordingExecutor is an action.Executor double that records every
// Execute call's name and args under a mutex, so the concurrency variant
// can assert exactly-two executions with the right per-call inputs. It
// always succeeds, mirroring StubExecutor's no-error contract.
type recordingExecutor struct {
	mu    sync.Mutex
	calls []recordedCall
}

type recordedCall struct {
	name string
	args map[string]any
}

func (e *recordingExecutor) Execute(_ context.Context, name string, args map[string]any) (action.Result, error) {
	e.mu.Lock()
	e.calls = append(e.calls, recordedCall{name: name, args: args})
	e.mu.Unlock()
	return action.Result{Content: `{"recorded":true}`}, nil
}

func (e *recordingExecutor) recordedRecipients() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.calls))
	for _, c := range e.calls {
		if to, _ := c.args["to"].(string); to != "" {
			out = append(out, to)
		}
	}
	return out
}

// eventChainForApproval returns the chronological event types correlated
// to a single approval id. Both approval.* and execution.* events stamp
// the same aileron.approval.id payload key (see action_queue.go
// recordRequested/recordDecided and handlers.go actionExecutionPayload),
// so this isolates one call's chain even when several share a session.
func eventChainForApproval(t *testing.T, store *audit.MemStore, approvalID string) []model.EventType {
	t.Helper()
	all, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	out := make([]model.EventType, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if got, _ := all[i].Payload["aileron.approval.id"].(string); got == approvalID {
			out = append(out, all[i].EventType)
		}
	}
	return out
}

// waitForApprovalChain polls until the given approval's chain contains
// the terminal event, or the deadline expires.
func waitForApprovalChain(t *testing.T, store *audit.MemStore, approvalID string, terminal model.EventType, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range eventChainForApproval(t, store, approvalID) {
			if e == terminal {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s on approval %q; chain so far: %v",
		terminal, approvalID, eventChainForApproval(t, store, approvalID))
}

// TestSandboxMCP_Concurrency_DistinctApprovalsAttributedCorrectly covers
// R6c: two concurrent draft_email calls produce two distinct approval
// ids; approving them in reverse order emits the correct, isolated
// per-approval event chain for each; and the executor runs exactly twice
// with the two distinct recipients. Host transport, Docker-free. Genuine
// concurrency uses two separate aileron-mcp processes (one per call)
// since a single process's stdio reader is not demultiplexed.
func TestSandboxMCP_Concurrency_DistinctApprovalsAttributedCorrectly(t *testing.T) {
	h := newDaemonHarness(t, draftEmailManifestApproval)
	rec := &recordingExecutor{}
	h.srv.executor = rec

	const sessionID = "sess-concurrency"
	p1 := h.spawnMCP(t, transportHost, sessionID, "")
	p2 := h.spawnMCP(t, transportHost, sessionID, "")
	initializeMCP(t, p1)
	initializeMCP(t, p2)

	// Fire both tools/call requests concurrently. requestErr returns an
	// error rather than calling t.Fatalf so neither goroutine touches the
	// test goroutine's failure path.
	type callOutcome struct {
		resp jsonrpcResponse
		err  error
	}
	results := make([]callOutcome, 2)
	recipients := []string{"erin@example.com", "frank@example.com"}
	var wg sync.WaitGroup
	for i, p := range []*mcpProcess{p1, p2} {
		wg.Add(1)
		go func(i int, p *mcpProcess) {
			defer wg.Done()
			resp, err := p.requestErr("tools/call", map[string]any{
				"name": "draft_email",
				"arguments": map[string]any{
					"to":      recipients[i],
					"subject": "concurrent",
					"body":    "body",
				},
			})
			results[i] = callOutcome{resp: resp, err: err}
		}(i, p)
	}
	wg.Wait()
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("concurrent tools/call %d: %v", i, r.err)
		}
		if r.resp.Error != nil {
			t.Fatalf("concurrent tools/call %d: %d %s", i, r.resp.Error.Code, r.resp.Error.Message)
		}
	}

	// Both calls registered a pending approval; ids must be distinct.
	pending := h.srv.actionApprovals.List()
	if len(pending) != 2 {
		t.Fatalf("pending approvals = %d; want 2", len(pending))
	}
	if pending[0].ID == pending[1].ID {
		t.Fatalf("concurrent calls collapsed to one approval id %q; want two distinct ids", pending[0].ID)
	}

	// Approve in reverse registration order. Each approval's chain must
	// be its own complete, isolated sequence regardless of order.
	if err := h.srv.actionApprovals.Decide(pending[1].ID, true, "", nil); err != nil {
		t.Fatalf("Decide pending[1]: %v", err)
	}
	if err := h.srv.actionApprovals.Decide(pending[0].ID, true, "", nil); err != nil {
		t.Fatalf("Decide pending[0]: %v", err)
	}

	wantChain := []model.EventType{
		model.EventTypeApprovalRequested,
		model.EventTypeApprovalApproved,
		model.EventTypeExecutionStarted,
		model.EventTypeExecutionSucceeded,
	}
	for _, entry := range pending {
		waitForApprovalChain(t, h.auditStore, entry.ID, model.EventTypeExecutionSucceeded, 5*time.Second)
		chain := eventChainForApproval(t, h.auditStore, entry.ID)
		if len(chain) != len(wantChain) {
			t.Fatalf("approval %q chain = %v; want %v", entry.ID, chain, wantChain)
		}
		for i, w := range wantChain {
			if chain[i] != w {
				t.Errorf("approval %q chain[%d] = %s; want %s", entry.ID, i, chain[i], w)
			}
		}
	}

	// The executor ran exactly twice, once per call, with the two
	// distinct recipients — proving per-call args were not crossed.
	got := rec.recordedRecipients()
	if len(got) != 2 {
		t.Fatalf("executor recorded %d calls; want 2 (%v)", len(got), got)
	}
	gotSet := map[string]bool{got[0]: true, got[1]: true}
	for _, want := range recipients {
		if !gotSet[want] {
			t.Errorf("executor did not receive a call for %q; recorded %v", want, got)
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
