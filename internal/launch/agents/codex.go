package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ALRubinger/aileron/internal/launch"
)

// Codex is the agent definition for OpenAI Codex CLI.
//
// Codex CLI does not honour $SHELL — it resolves the user's shell via
// `getpwuid_r` and validates the binary name against a fixed allowlist
// (bash/zsh/sh/pwsh/powershell/cmd). The shell-shim interception model
// is impossible without an upstream patch. Per ADR-0015 the launcher
// no longer attempts shell interception for any agent; Codex's own
// sandbox + approval-policy machinery stays in charge of its local
// exec, and Aileron mediates only the actions the agent calls through
// aileron-mcp + the gateway.
//
// MCP wiring is written into `~/.codex/config.toml` under
// `[mcp_servers.aileron]` — Codex reads MCP servers from this file, not
// from a CLI flag, so ConfigureMCP returns no extra args.
type Codex struct{}

func (c Codex) Name() string          { return "codex" }
func (c Codex) BinaryNames() []string { return []string{"codex"} }

// Args returns no extra arguments. Approval policy, sandbox mode, and
// folder trust for Codex live in `~/.codex/config.toml`, not on the CLI.
//
// In ModeHost we do NOT override them: a host launch runs against the
// user's real machine, so Codex's own approval prompt and folder-trust
// dialog stay in force and the user owns those settings.
//
// In ModeSandbox the generated config.toml (mergeCodexSandboxConfig)
// pre-sets them for an ephemeral, non-interactive run: `approval_policy
// = "never"` suppresses per-tool approval prompts, `sandbox_mode =
// "danger-full-access"` defers isolation to the outer container, and a
// `[projects."/home/agent/workspace"]` block pre-accepts the
// folder-trust prompt. The sandbox container is the trust boundary
// (ADR-0015), so auto-approving inside it is safe.
func (c Codex) Args() []string { return nil }

func (c Codex) Env() map[string]string { return nil }

// LLMEndpointEnv returns the env var Codex CLI reads to override the
// OpenAI API base URL. Routing Codex through Aileron's gateway only
// applies on the API-key auth path; sessions authenticated through
// ChatGPT login do not honour this var and run directly against
// OpenAI.
func (c Codex) LLMEndpointEnv() string { return "OPENAI_BASE_URL" }

// ConfigureMCP writes (or merges) a `[mcp_servers.aileron]` entry into
// `~/.codex/config.toml`. Codex reads MCP servers from config.toml at
// startup; passing them via CLI is not supported. Returns nil args.
//
// Mode branches the destination:
//   - ModeHost: writes the launcher's host `~/.codex/config.toml`,
//     preserving the rest of the file via mergeCodexMCPBlock.
//   - ModeSandbox: writes the generated [mcp_servers.aileron] block
//     to an os.MkdirTemp config.toml and returns a Volume bind-
//     mounting it into the container at /home/agent/.codex/config.toml.
//     The host `~/.codex/config.toml` is never touched. The in-
//     container Codex reads this file at startup. See ADR-0024.
//
// For MCP, sandbox mode emits ONLY the [mcp_servers.aileron] block — no
// merge against a host-side config — so any other [mcp_servers.foo]
// entries the user has on the host don't leak into the container. Users
// wanting extra MCP servers under Codex+sandbox configure them via
// their devcontainer or via a wrapper merge; the manual recipe at
// docs/development/sandbox-mcp-walkthrough.md documents the limitation.
//
// Sandbox mode additionally pre-sets Codex's non-interactive keys
// (approval_policy, sandbox_mode, folder trust_level) so the ephemeral
// container runs end-to-end without operator prompts; see
// mergeCodexSandboxConfig. Host mode leaves all of those to the user.
func (c Codex) ConfigureMCP(mcpBin string, mcpEnv map[string]string, _ string, mode launch.Mode) ([]string, []launch.MCPMount, error) {
	if mode == launch.ModeSandbox {
		return c.configureSandboxMCP(mcpBin, mcpEnv)
	}
	return c.configureHostMCP(mcpBin, mcpEnv)
}

// AuthSpec returns Codex's vault-backed credential descriptor per
// ADR-0025.
//
// A single FileBinding at agents/codex/oauth renders
// /home/agent/.codex/auth.json (mode 0600) with the chatgpt-mode
// envelope. The binding uses a parent-dir mount at /home/agent/.codex/
// (MountAsFile = false). The first-launch bootstrap requires this:
// when the vault is empty the launcher writes no file, the in-
// container Codex login writes auth.json into the mounted dir, and
// Capture reads the result back. A file-mount strategy would have no
// host-side inode to bind, so the login would write into the
// container overlay FS and Capture would see nothing.
//
// ConfigureMCP under ModeSandbox renders config.toml to a temp file
// and returns it as an MCPMount targeting
// /home/agent/.codex/config.toml. The launcher detects that this
// target is nested inside the AuthSpec's /home/agent/.codex/ directory
// mount and relocates the file into that mount's host-side source dir
// (collapseNestedMCPMounts in launcher.go), so a single directory mount
// carries both auth.json and config.toml. Emitting config.toml as a
// separate nested file-inside-dir bind mount is what broke under macOS
// Docker Desktop's virtiofs (runc: "mountpoint outside of rootfs",
// issue #1143).
//
// The PreLaunchRefresh hook exchanges the refresh token for a new
// access token against auth.openai.com before the container starts,
// persists the rotated bundle to vault, and hands Render the new
// secret. AE6 invariant: the rotated bundle is in vault before
// container start; a failed persist aborts the launch.
func (c Codex) AuthSpec() launch.AuthSpec {
	return launch.AuthSpec{
		FileBindings: []launch.FileBinding{{
			VaultPath:        codexVaultPath,
			ContainerPath:    codexAuthContainerPath,
			Mode:             0o600,
			Required:         false,
			Render:           codexRender,
			Capture:          codexCapture,
			Fresher:          codexFresher,
			PreLaunchRefresh: codexPreLaunchRefresh,
		}},
	}
}

func (c Codex) configureHostMCP(mcpBin string, mcpEnv map[string]string) ([]string, []launch.MCPMount, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, fmt.Errorf("determining home directory: %w", err)
	}
	configDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating %s: %w", configDir, err)
	}
	path := filepath.Join(configDir, "config.toml")

	existing, _ := os.ReadFile(path)
	merged := mergeCodexMCPBlock(string(existing), mcpBin, mcpEnv)
	if err := os.WriteFile(path, []byte(merged), 0o600); err != nil {
		return nil, nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return nil, nil, nil
}

// codexSandboxConfigContainerPath is where Codex reads its config from
// inside the sandbox container.
const codexSandboxConfigContainerPath = "/home/agent/.codex/config.toml"

// codexSandboxWorkspacePath is the in-container directory the launcher
// runs Codex in (the bind-mounted project workspace). It must match
// container.WorkspacePath and the runtime `--workdir`; it is spelled
// out literally here, mirroring claudeWorkspacePath, to keep the agents
// package free of an import on the container package. The generated
// sandbox config.toml pre-trusts this folder so Codex does not block on
// its folder-trust prompt on every launch.
const codexSandboxWorkspacePath = "/home/agent/workspace"

func (c Codex) configureSandboxMCP(mcpBin string, mcpEnv map[string]string) ([]string, []launch.MCPMount, error) {
	dir, err := os.MkdirTemp("", "aileron-codex-sandbox-*")
	if err != nil {
		return nil, nil, fmt.Errorf("creating codex sandbox config tempdir: %w", err)
	}
	path := filepath.Join(dir, "config.toml")
	// Sandbox mode emits only our [mcp_servers.aileron] block — no
	// merge with a host-side config. The empty-string baseline mirrors
	// what mergeCodexMCPBlock produces when called with no prior file.
	// Sandbox mode also emits the non-interactive keys that let an
	// ephemeral container run end-to-end without operator prompts:
	// `approval_policy = "never"` suppresses per-tool approval prompts,
	// `sandbox_mode = "danger-full-access"` disables Codex's redundant
	// in-container OS sandbox (the Alpine image ships no bubblewrap, so
	// it would only warn and fall back), `cli_auth_credentials_store =
	// "file"` so the in-container Codex resolves auth from auth.json —
	// the AuthSpec FileBinding writes auth.json into the same directory
	// at launch (R22) — and a [projects."/home/agent/workspace"] block
	// with `trust_level = "trusted"` to pre-accept the folder-trust
	// prompt for the bind-mounted workspace.
	body := mergeCodexSandboxConfig(mcpBin, mcpEnv)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return nil, nil, fmt.Errorf("writing codex sandbox config: %w", err)
	}
	mount := launch.MCPMount{
		Source:   path,
		Target:   codexSandboxConfigContainerPath,
		ReadOnly: true,
	}
	return nil, []launch.MCPMount{mount}, nil
}

// mergeCodexSandboxConfig is the ModeSandbox variant of the merge.
// In addition to the [mcp_servers.aileron] block, it prepends the
// non-interactive top-level keys and a folder-trust block that let an
// ephemeral container run end-to-end without operator prompts:
//
//   - `approval_policy = "never"` suppresses Codex's per-tool approval
//     prompts (the operator hit these on list_recent_emails / get_email).
//     The sandbox container is the trust boundary (ADR-0015) and Aileron
//     still mediates every action the agent calls through aileron-mcp +
//     the gateway, so auto-approving Codex's local exec inside the
//     ephemeral container is safe.
//   - `sandbox_mode = "danger-full-access"` disables Codex's own
//     OS sandbox (bubblewrap/Landlock on the Linux container) for
//     local exec. The Alpine sandbox-base image ships no bwrap binary,
//     so
//     leaving Codex's sandbox enabled prints a "could not find
//     bubblewrap on PATH" warning on every launch and falls back to a
//     bundled copy. The outer Aileron Docker container is the real
//     isolation boundary — Codex's sandbox/approval machinery governs
//     only its local exec while Aileron mediates MCP/gateway actions —
//     so the nested in-container sandbox is redundant. This key
//     silences the warning at the source. It is sandbox-mode only;
//     configureHostMCP is untouched, so host launches keep Codex's
//     normal sandboxing.
//   - `cli_auth_credentials_store = "file"` so the in-container Codex
//     CLI reads auth.json instead of trying to use the macOS Keychain
//     / Linux secret-tool keyring (which do not exist inside the
//     sandbox).
//   - a [projects."/home/agent/workspace"] block with `trust_level =
//     "trusted"` pre-accepts Codex's "trust the current folder?" prompt
//     for the bind-mounted workspace, mirroring how Claude pre-sets
//     hasTrustDialogAccepted for the same path (claude_auth.go).
//
// All of these are sandbox-mode only. The host config is never touched
// under ModeSandbox, and host launches keep Codex's normal approval
// policy, sandboxing, and folder-trust behavior.
func mergeCodexSandboxConfig(mcpBin string, mcpEnv map[string]string) string {
	// Start with the top-level keys, append the workspace folder-trust
	// block, then the [mcp_servers.*] block via the existing merge over
	// an empty baseline.
	return `approval_policy = "never"` + "\n" +
		`sandbox_mode = "danger-full-access"` + "\n" +
		`cli_auth_credentials_store = "file"` + "\n\n" +
		`[projects."` + codexSandboxWorkspacePath + `"]` + "\n" +
		`trust_level = "trusted"` + "\n\n" +
		mergeCodexMCPBlock("", mcpBin, mcpEnv)
}

// mergeCodexMCPBlock replaces existing [mcp_servers.aileron] and
// [mcp_servers.aileron.env] blocks (or appends them) in the given
// config.toml content. The function does not fully parse the rest of
// the file — it scans line-by-line for the section headers, removes
// matching blocks, and emits the rewritten blocks at the end. Other
// sections and comments are preserved.
func mergeCodexMCPBlock(content, mcpBin string, mcpEnv map[string]string) string {
	prefix := "[mcp_servers." + launch.MCPServerName
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines)+8)
	skip := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		isHeader := strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]")
		// Start skipping any block whose header begins with our prefix
		// (covers both [mcp_servers.aileron] and [mcp_servers.aileron.env]).
		if isHeader && strings.HasPrefix(trim, prefix) {
			skip = true
			continue
		}
		if skip {
			if isHeader {
				skip = false
				out = append(out, line)
				continue
			}
			continue
		}
		out = append(out, line)
	}
	// Trim trailing blank lines so the appended block sits cleanly.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(out) > 0 {
		out = append(out, "")
	}
	out = append(out, "[mcp_servers."+launch.MCPServerName+"]")
	out = append(out, fmt.Sprintf("command = %q", mcpBin))
	if len(mcpEnv) > 0 {
		out = append(out, "")
		out = append(out, "[mcp_servers."+launch.MCPServerName+".env]")
		keys := make([]string, 0, len(mcpEnv))
		for k := range mcpEnv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, fmt.Sprintf("%s = %q", k, mcpEnv[k]))
		}
	}
	out = append(out, "")
	return strings.Join(out, "\n")
}
