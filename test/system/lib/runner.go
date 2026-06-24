// Pure, docker-free scenario configuration and assembly logic for the shell-free
// Go `aileron launch` system-test driver (#1624). The bash scenario bodies
// (codex.sh / claude.sh) hard-code their agent bindings inline and assemble the
// launch command + agent prompt as shell strings. This file re-expresses that
// agent-config wiring and command/prompt assembly as pure Go: an AgentConfig
// struct carries the per-agent bindings, and a small set of pure constructors
// turn it into the post-`--` token list and the deterministic agent prompt.
//
// Everything here is pure (no os/exec, no docker, no filesystem): the impure
// live-Docker plumbing lives in the separate test/system/scenario main module,
// which imports this package and delegates every decision back to it. Keeping
// the wiring pure is what lets the standard `go test ./test/system/lib/...` CI
// job cover the agent-config + assembly contract with no container.
//
// The codex bindings are confirmed against codex.sh: IMAGE_SUFFIX=codex,
// EXPECTED_IMAGE default ghcr.io/alrubinger/aileron-sandbox-codex:edge,
// CONFIG_PATH=/home/agent/.codex/config.toml, MCP_MARKER=[mcp_servers.aileron],
// AUTH_PATH=/home/agent/.codex/auth.json, batch flags exec --skip-git-repo-check,
// workspace container path /home/agent/workspace, sentinel file
// .aileron-systest-sentinel.
package systestlib

import "fmt"

// MCPMode distinguishes how an agent is pointed at the aileron MCP server, which
// in turn selects which R8.2 probe applies. Codex writes a config.toml the host
// can read inside the container (config-file mode); Claude passes `--mcp-config`
// on the command line (cmdline mode). The runner needs both shapes so #1625
// (claude) reuses this struct without a runner change.
type MCPMode int

const (
	// MCPModeConfigFile is the codex shape: the agent reads an MCP config file in
	// the container (ConfigPath) which must contain MCPMarker. Probe via
	// ProbeMCPRuntime + the config-file read (ProbeMCPConfigContains).
	MCPModeConfigFile MCPMode = iota
	// MCPModeCmdline is the claude shape: the agent is wired by a command-line
	// flag (MCPFlag) whose payload references MCPMarker. Probe via ProbeMCPCmdline.
	MCPModeCmdline
)

// AgentConfig carries every per-agent binding the scenario driver needs, so the
// driver itself is agent-agnostic and a new agent is one constructor. The codex
// constructor is CodexConfig; the claude constructor lands in #1625 reusing this
// shape verbatim.
type AgentConfig struct {
	// Name is the `aileron launch <Name>` agent argument (e.g. "codex").
	Name string
	// ExpectedImage is the already-resolved R8.1 expected image ref (default or
	// EXPECTED_IMAGE override). Use ResolveExpectedImage to compute it.
	ExpectedImage string
	// MCPMode selects the R8.2 probe shape (config-file vs cmdline).
	MCPMode MCPMode
	// ConfigPath is the in-container MCP config file path (config-file mode only).
	ConfigPath string
	// MCPFlag is the command-line MCP flag (cmdline mode only, e.g. --mcp-config).
	MCPFlag string
	// MCPMarker is the substring that proves the aileron MCP block/payload is
	// present (config-file marker e.g. "[mcp_servers.aileron]", or cmdline server
	// name marker e.g. `"aileron"`).
	MCPMarker string
	// AuthPath is the in-container credential file path (R8.3).
	AuthPath string
	// BatchFlags are the agent-specific tokens that precede the prompt in the
	// post-`--` forwarding (R7a), e.g. ["exec", "--skip-git-repo-check"] for codex
	// or ["-p"] for claude.
	BatchFlags []string
	// WorkspaceContainerPath is the bind-mounted workspace path inside the
	// container (where the agent writes the sentinel).
	WorkspaceContainerPath string
	// SentinelName is the sentinel file basename (R9), e.g. .aileron-systest-sentinel.
	SentinelName string
}

// codexDefaultImage is the floating dev image ref codex.sh defaults EXPECTED_IMAGE
// to. A dev/empty version build pulls :edge; override with EXPECTED_IMAGE to
// assert a release :latest or a digest pin.
const codexDefaultImage = "ghcr.io/alrubinger/aileron-sandbox-codex:edge"

// claudeDefaultImage is the floating dev image ref claude.sh defaults
// EXPECTED_IMAGE to. Unlike codex's GHCR ref, this is the LOCAL Feature-build
// repo (`aileron/sandbox-agent-claude`, per #1451/#1585): no registry host and
// no digest-pin path, because the claude sandbox image is built locally from the
// devcontainer Feature rather than published to a registry. A dev/empty version
// build pulls :edge; override with EXPECTED_IMAGE to assert a specific local ref.
const claudeDefaultImage = "aileron/sandbox-agent-claude:edge"

// ResolveExpectedImage mirrors codex.sh's `EXPECTED_IMAGE="${EXPECTED_IMAGE:-<default>}"`:
// a non-empty override wins, otherwise the agent default is used. Pure; the
// override is passed in (read from os.Getenv("EXPECTED_IMAGE") by the live driver)
// so this stays host-verifiable.
func ResolveExpectedImage(override, def string) string {
	if override != "" {
		return override
	}
	return def
}

// CodexConfig returns the codex AgentConfig with bindings confirmed against
// codex.sh. override is the EXPECTED_IMAGE value (empty for the default); the
// live driver passes os.Getenv("EXPECTED_IMAGE").
func CodexConfig(expectedImageOverride string) AgentConfig {
	return AgentConfig{
		Name:                   "codex",
		ExpectedImage:          ResolveExpectedImage(expectedImageOverride, codexDefaultImage),
		MCPMode:                MCPModeConfigFile,
		ConfigPath:             "/home/agent/.codex/config.toml",
		MCPMarker:              "[mcp_servers.aileron]",
		AuthPath:               "/home/agent/.codex/auth.json",
		BatchFlags:             []string{"exec", "--skip-git-repo-check"},
		WorkspaceContainerPath: "/home/agent/workspace",
		SentinelName:           ".aileron-systest-sentinel",
	}
}

// ClaudeConfig returns the claude AgentConfig with bindings confirmed against
// claude.sh (issue #1477 decision 2). override is the EXPECTED_IMAGE value (empty
// for the default); the live driver passes os.Getenv("EXPECTED_IMAGE").
//
// Claude differs from codex in three ways the driver reads off this config:
//   - MCP is wired by the `--mcp-config` command-line flag (MCPModeCmdline), not a
//     config file, so ConfigPath is empty and MCPFlag/MCPMarker drive the R8.2
//     cmdline probe (ProbeMCPCmdline). MCPMarker is the JSON-key form `"aileron"`,
//     byte-identical to claude.sh's MCP_MARKER.
//   - The credential file is /home/agent/.claude/.credentials.json.
//   - The non-interactive batch flag is `-p`, so BuildExecArgs yields
//     ["-p", "<prompt>"] forwarded as `aileron launch claude -- -p "<prompt>"`.
//
// The default ExpectedImage is the LOCAL aileron/sandbox-agent-claude:edge ref
// (no registry host, no digest pin); see claudeDefaultImage.
func ClaudeConfig(expectedImageOverride string) AgentConfig {
	return AgentConfig{
		Name:                   "claude",
		ExpectedImage:          ResolveExpectedImage(expectedImageOverride, claudeDefaultImage),
		MCPMode:                MCPModeCmdline,
		MCPFlag:                "--mcp-config",
		MCPMarker:              `"aileron"`,
		AuthPath:               "/home/agent/.claude/.credentials.json",
		BatchFlags:             []string{"-p"},
		WorkspaceContainerPath: "/home/agent/workspace",
		SentinelName:           ".aileron-systest-sentinel",
	}
}

// BuildExecArgs assembles the post-`--` token list forwarded to the agent CLI,
// mirroring codex.sh's `-- exec --skip-git-repo-check "<prompt>"` (R7a). The
// result is BatchFlags followed by the single prompt token. A fresh slice is
// returned so the caller cannot mutate cfg.BatchFlags through the result.
func BuildExecArgs(cfg AgentConfig, prompt string) []string {
	args := make([]string, 0, len(cfg.BatchFlags)+1)
	args = append(args, cfg.BatchFlags...)
	args = append(args, prompt)
	return args
}

// BuildPrompt assembles the deterministic two-step agent prompt: (1) write the
// exact sentinel string (no newline, nothing else) to
// <WorkspaceContainerPath>/<SentinelName> (R9), and (2) call the aileron
// http_request tool GET <healthURL> so a daemon-side audit record lands for the
// run (R10). sentinel is the per-run token from Sentinel(runID).
//
// healthURL MUST be a fully-resolved absolute URL (e.g.
// http://127.0.0.1:60036/healthz), NOT a `${AILERON_URL}` placeholder. Nothing
// in the path expands shell-style placeholders: the agent passes the url tool
// argument verbatim (cmd/aileron-mcp), and the daemon issues the outbound fetch
// against that exact string (internal/app/handlers_comms.go). A literal
// placeholder therefore produces an invalid request and no audit record, which
// is why the original `${AILERON_URL}/healthz` prompt never produced an R10
// record. The caller resolves the running daemon's host URL (via
// `aileron daemon start`) and passes <daemonURL>/healthz here.
func BuildPrompt(cfg AgentConfig, sentinel, healthURL string) string {
	return fmt.Sprintf(
		"Do exactly two things and then stop. "+
			"1) Write the exact text %s (no newline, nothing else) to the file %s/%s. "+
			"2) Call the aileron http_request tool with method GET and url %s.",
		sentinel, cfg.WorkspaceContainerPath, cfg.SentinelName, healthURL)
}
