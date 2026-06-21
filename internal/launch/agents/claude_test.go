package agents_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

func TestClaude_Identity(t *testing.T) {
	c := agents.Claude{}
	if c.Name() != "claude" {
		t.Errorf("Name() = %q, want %q", c.Name(), "claude")
	}
	if got := c.BinaryNames(); len(got) != 1 || got[0] != "claude" {
		t.Errorf("BinaryNames() = %v, want [\"claude\"]", got)
	}
	if c.LLMEndpointEnv() != "ANTHROPIC_BASE_URL" {
		t.Errorf("LLMEndpointEnv() = %q, want %q", c.LLMEndpointEnv(), "ANTHROPIC_BASE_URL")
	}
	if c.Env() != nil {
		t.Errorf("Env() = %v, want nil", c.Env())
	}
}

// TestClaude_Args_HostAllowsBashAndAileronMCP pins the host-launch
// posture: a host launch runs against the user's real machine, so
// Claude keeps the narrower --allowedTools whitelist rather than full
// permission skipping.
func TestClaude_Args_HostAllowsBashAndAileronMCP(t *testing.T) {
	args := agents.Claude{}.Args(launch.ModeHost)
	if len(args) != 2 {
		t.Fatalf("Args(ModeHost) = %v, want 2 entries", args)
	}
	if args[0] != "--allowedTools" {
		t.Errorf("Args(ModeHost)[0] = %q, want --allowedTools", args[0])
	}
	if !strings.Contains(args[1], "Bash(*)") {
		t.Errorf("Args(ModeHost)[1] = %q, should allow Bash(*)", args[1])
	}
	if !strings.Contains(args[1], "mcp__"+launch.MCPServerName) {
		t.Errorf("Args(ModeHost)[1] = %q, should allow mcp__%s", args[1], launch.MCPServerName)
	}
}

// TestClaude_Args_SandboxSkipsPermissions is the regression test for
// the container/sandbox parity fix: under ModeSandbox the container is
// the trust boundary (ADR-0015), so Claude must pass
// --dangerously-skip-permissions alone, mirroring Codex's container
// YOLO posture. The narrower --allowedTools flag must NOT appear: the
// skip flag subsumes it, and combining them would be redundant.
func TestClaude_Args_SandboxSkipsPermissions(t *testing.T) {
	args := agents.Claude{}.Args(launch.ModeSandbox)
	if len(args) != 1 || args[0] != "--dangerously-skip-permissions" {
		t.Fatalf("Args(ModeSandbox) = %v, want [--dangerously-skip-permissions]", args)
	}
	for _, a := range args {
		if a == "--allowedTools" {
			t.Errorf("Args(ModeSandbox) = %v, must not include --allowedTools alongside the skip flag", args)
		}
	}
}

// TestClaude_AuthSpec_SubscriptionMode pins that NewClaude with the
// subscription mode (and the zero-value Claude{}) returns today's exact
// oauth shape: one FileBinding at agents/claude/oauth, the onboarding
// StaticFile, and zero EnvBindings. The zero-value check is the
// regression guard for the unchanged registry registration in
// cmd/aileron/main.go (#1340 swaps that line; this sub-issue must not
// change its behavior).
func TestClaude_AuthSpec_SubscriptionMode(t *testing.T) {
	specs := map[string]launch.AuthSpec{
		"zero value":        agents.Claude{}.AuthSpec(),
		"NewClaude(subscr)": agents.NewClaude(agents.ClaudeAuthModeSubscription).AuthSpec(),
	}
	for name, spec := range specs {
		t.Run(name, func(t *testing.T) {
			if len(spec.FileBindings) != 1 {
				t.Fatalf("FileBindings = %d, want 1", len(spec.FileBindings))
			}
			if spec.FileBindings[0].VaultPath != "agents/claude/oauth" {
				t.Errorf("FileBinding VaultPath = %q, want agents/claude/oauth", spec.FileBindings[0].VaultPath)
			}
			if len(spec.EnvBindings) != 0 {
				t.Errorf("EnvBindings = %d, want 0 in subscription mode", len(spec.EnvBindings))
			}
			if len(spec.StaticFiles) != 1 {
				t.Errorf("StaticFiles = %d, want 1 (onboarding stub)", len(spec.StaticFiles))
			}
			// Disjoint-slot: subscription references neither the apikey
			// slot nor any EnvBinding.
			for _, fb := range spec.FileBindings {
				if fb.VaultPath == "agents/claude/apikey" {
					t.Errorf("subscription FileBinding references the apikey slot %q", fb.VaultPath)
				}
			}
			// The oauth binding declares a CaptureValidate hook so a
			// captured in-container token is liveness-probed host-side
			// before the vault PUT (#1384).
			if spec.FileBindings[0].CaptureValidate == nil {
				t.Errorf("subscription FileBinding must declare CaptureValidate for capture-time liveness validation")
			}
		})
	}
}

// TestClaude_AuthSpec_APIKeyMode pins the api-key binding set: one
// EnvBinding at agents/claude/apikey, Required==false, non-nil Render +
// HostAcquire, zero FileBindings, and the shared onboarding StaticFile.
// Disjoint-slot assertions enforce the acceptance criterion that
// selecting api-key never touches the oauth slot.
func TestClaude_AuthSpec_APIKeyMode(t *testing.T) {
	spec := agents.NewClaude(agents.ClaudeAuthModeAPIKey).AuthSpec()
	if len(spec.EnvBindings) != 1 {
		t.Fatalf("EnvBindings = %d, want 1", len(spec.EnvBindings))
	}
	eb := spec.EnvBindings[0]
	if eb.VaultPath != "agents/claude/apikey" {
		t.Errorf("EnvBinding VaultPath = %q, want agents/claude/apikey", eb.VaultPath)
	}
	if eb.Required {
		t.Errorf("Required = true; empty vault must trigger host-paste / in-container fallback")
	}
	if eb.Render == nil {
		t.Errorf("Render must be set")
	}
	if eb.HostAcquire == nil {
		t.Errorf("HostAcquire must be set")
	}
	if len(spec.FileBindings) != 0 {
		t.Errorf("FileBindings = %d, want 0 in api-key mode", len(spec.FileBindings))
	}
	// Disjoint-slot: api-key references neither the oauth slot nor any
	// FileBinding.
	if spec.EnvBindings[0].VaultPath == "agents/claude/oauth" {
		t.Errorf("api-key EnvBinding references the oauth slot")
	}
	// The onboarding StaticFile is shared so an api-key launch is silent
	// first-run too.
	if len(spec.StaticFiles) != 1 {
		t.Errorf("StaticFiles = %d, want 1 (shared onboarding stub)", len(spec.StaticFiles))
	}
}

func TestClaude_ConfigureMCP_EmitsMCPConfigFlag(t *testing.T) {
	mcpEnv := map[string]string{
		"AILERON_URL":        "http://127.0.0.1:7000",
		"AILERON_SESSION_ID": "sess-abc",
	}
	args, mounts, err := agents.Claude{}.ConfigureMCP("/usr/local/bin/aileron-mcp", mcpEnv, "", launch.ModeHost)
	if err != nil {
		t.Fatalf("ConfigureMCP returned error: %v", err)
	}
	if len(mounts) != 0 {
		t.Errorf("ConfigureMCP returned %d mounts under ModeHost; want 0", len(mounts))
	}
	if len(args) != 2 || args[0] != "--mcp-config" {
		t.Fatalf("ConfigureMCP() = %v, want [--mcp-config <json>]", args)
	}
	var payload struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[1]), &payload); err != nil {
		t.Fatalf("ConfigureMCP returned non-JSON value: %v\n%s", err, args[1])
	}
	server, ok := payload.MCPServers[launch.MCPServerName]
	if !ok {
		t.Fatalf("MCP config missing entry %q: %+v", launch.MCPServerName, payload)
	}
	if server.Command != "/usr/local/bin/aileron-mcp" {
		t.Errorf("server.Command = %q, want /usr/local/bin/aileron-mcp", server.Command)
	}
	if server.Env["AILERON_URL"] != "http://127.0.0.1:7000" {
		t.Errorf("server.Env[AILERON_URL] = %q, want http://127.0.0.1:7000", server.Env["AILERON_URL"])
	}
	if server.Env["AILERON_SESSION_ID"] != "sess-abc" {
		t.Errorf("server.Env[AILERON_SESSION_ID] = %q, want sess-abc", server.Env["AILERON_SESSION_ID"])
	}
}
