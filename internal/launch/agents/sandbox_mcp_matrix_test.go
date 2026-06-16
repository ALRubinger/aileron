package agents_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

// TestSandboxMCPRegistration_Matrix is the automated companion to the
// manual sandbox E2E (#962). For every supported agent it asserts the v4
// sandbox-launch MCP registration contract (ADR-0024): under ModeSandbox,
// ConfigureMCP must register the aileron-mcp binary and carry every
// daemon-env var the MCP server needs, no matter which mechanism the agent
// uses (--mcp-config JSON for Claude/Pi, --with-extension for Goose, an
// opencode.json workspace file for OpenCode, or a bind-mounted config.toml
// for Codex).
//
// This is a launcher-side contract test, NOT a live-agent launch. It does
// not run claude/codex/goose/pi/opencode; it checks the registration
// artifact Aileron generates for each. The integration round-trip in
// internal/app/sandbox_mcp_test.go separately proves aileron-mcp itself
// exposes draft_email over tools/list and round-trips through a real
// container (with the test acting as the MCP client, not a real agent).
// Whether each real agent actually honors the registration we generate
// (e.g. Codex reads the bind-mounted config.toml, Goose parses
// --with-extension) is covered by the manual smoke in #962.
//
// Pure Go: no Docker, no agent binaries. The registration logic is
// host-OS-independent, so one CI run covers it; macOS and Windows
// container launch are confirmed by a light manual smoke (see the
// sandbox MCP walkthrough).
func TestSandboxMCPRegistration_Matrix(t *testing.T) {
	const mcpBin = "/usr/local/bin/aileron-mcp"
	mcpEnv := map[string]string{
		"AILERON_URL":          "http://host.docker.internal:7777",
		"AILERON_COMMS_URL":    "http://host.docker.internal:7777",
		"AILERON_SESSION_ID":   "sess-matrix-001",
		"AILERON_APPROVAL_URL": "http://host.docker.internal:7777/approvals",
	}

	cases := []struct {
		name  string
		agent launch.Agent
	}{
		{"claude", agents.Claude{}},
		{"pi", agents.Pi{}},
		{"goose", agents.Goose{}},
		{"opencode", agents.OpenCode{}},
		{"codex", agents.Codex{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Isolate any home / XDG / workspace writes to temp dirs so the
			// test never touches the developer's real agent config.
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			dir := t.TempDir()

			args, mounts, err := tc.agent.ConfigureMCP(mcpBin, mcpEnv, dir, launch.ModeSandbox)
			if err != nil {
				t.Fatalf("ConfigureMCP(ModeSandbox): %v", err)
			}

			blob := registrationSurface(t, args, mounts, dir)

			if !strings.Contains(blob, mcpBin) {
				t.Errorf("%s sandbox registration does not reference the aileron-mcp binary %q\n--- registration surface ---\n%s", tc.name, mcpBin, blob)
			}
			for k, v := range mcpEnv {
				if !strings.Contains(blob, v) {
					t.Errorf("%s sandbox registration is missing daemon env %s (value %q)\n--- registration surface ---\n%s", tc.name, k, v, blob)
				}
			}
		})
	}
}

// registrationSurface flattens everything an agent's ConfigureMCP produces
// into one string for contract assertions: the CLI args it returns, the
// contents of any bind-mount source file (Codex), and any file it writes
// into the workspace dir (OpenCode). The assertion is mechanism-agnostic
// on purpose — the contract is "aileron-mcp is registered with the daemon
// env", not "registered via this specific syntax".
func registrationSurface(t *testing.T, args []string, mounts []launch.MCPMount, dir string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(strings.Join(args, "\n"))
	for _, m := range mounts {
		data, err := os.ReadFile(m.Source)
		if err != nil {
			t.Fatalf("reading MCP mount source %s: %v", m.Source, err)
		}
		b.WriteByte('\n')
		b.Write(data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading workspace dir %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading workspace file %s: %v", e.Name(), err)
		}
		b.WriteByte('\n')
		b.Write(data)
	}
	return b.String()
}
