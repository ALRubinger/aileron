// Contract tests for the pure agent-config + command/prompt assembly logic.
// They assert the codex bindings exactly (faithful to codex.sh), the
// EXPECTED_IMAGE override precedence, and that the assembled exec args and prompt
// match the bash byte-for-byte. No docker, no filesystem.
package systestlib_test

import (
	"strings"
	"testing"

	systestlib "github.com/ALRubinger/aileron/test/system/lib"
)

func TestCodexConfigBindings(t *testing.T) {
	cfg := systestlib.CodexConfig("")

	if cfg.Name != "codex" {
		t.Errorf("Name = %q; want %q", cfg.Name, "codex")
	}
	if cfg.ExpectedImage != "ghcr.io/alrubinger/aileron-sandbox-codex:edge" {
		t.Errorf("ExpectedImage = %q; want the codex :edge default", cfg.ExpectedImage)
	}
	if cfg.MCPMode != systestlib.MCPModeConfigFile {
		t.Errorf("MCPMode = %v; want MCPModeConfigFile", cfg.MCPMode)
	}
	if cfg.ConfigPath != "/home/agent/.codex/config.toml" {
		t.Errorf("ConfigPath = %q; want the codex config.toml path", cfg.ConfigPath)
	}
	if cfg.MCPMarker != "[mcp_servers.aileron]" {
		t.Errorf("MCPMarker = %q; want the codex config-file marker", cfg.MCPMarker)
	}
	if cfg.AuthPath != "/home/agent/.codex/auth.json" {
		t.Errorf("AuthPath = %q; want the codex auth.json path", cfg.AuthPath)
	}
	if cfg.WorkspaceContainerPath != "/home/agent/workspace" {
		t.Errorf("WorkspaceContainerPath = %q; want /home/agent/workspace", cfg.WorkspaceContainerPath)
	}
	if cfg.SentinelName != ".aileron-systest-sentinel" {
		t.Errorf("SentinelName = %q; want .aileron-systest-sentinel", cfg.SentinelName)
	}
	wantFlags := []string{"exec", "--skip-git-repo-check"}
	if len(cfg.BatchFlags) != len(wantFlags) {
		t.Fatalf("BatchFlags = %v; want %v", cfg.BatchFlags, wantFlags)
	}
	for i := range wantFlags {
		if cfg.BatchFlags[i] != wantFlags[i] {
			t.Errorf("BatchFlags[%d] = %q; want %q", i, cfg.BatchFlags[i], wantFlags[i])
		}
	}
	// Codex is config-file mode, so MCPFlag is unused (empty).
	if cfg.MCPFlag != "" {
		t.Errorf("MCPFlag = %q; want empty for config-file mode", cfg.MCPFlag)
	}
}

func TestClaudeConfigBindings(t *testing.T) {
	cfg := systestlib.ClaudeConfig("")

	if cfg.Name != "claude" {
		t.Errorf("Name = %q; want %q", cfg.Name, "claude")
	}
	// LOCAL Feature-build ref: no registry host, no digest pin (#1451/#1585).
	if cfg.ExpectedImage != "aileron/sandbox-agent-claude:edge" {
		t.Errorf("ExpectedImage = %q; want the claude local :edge default", cfg.ExpectedImage)
	}
	if strings.Contains(cfg.ExpectedImage, "ghcr.io") || strings.Contains(cfg.ExpectedImage, "@sha256:") {
		t.Errorf("ExpectedImage = %q; claude default must be a local ref with no registry host and no digest", cfg.ExpectedImage)
	}
	// Claude is cmdline mode: ConfigPath is unused (empty), MCPFlag/MCPMarker drive
	// the R8.2 ProbeMCPCmdline decision.
	if cfg.MCPMode != systestlib.MCPModeCmdline {
		t.Errorf("MCPMode = %v; want MCPModeCmdline", cfg.MCPMode)
	}
	if cfg.ConfigPath != "" {
		t.Errorf("ConfigPath = %q; want empty for cmdline mode", cfg.ConfigPath)
	}
	if cfg.MCPFlag != "--mcp-config" {
		t.Errorf("MCPFlag = %q; want --mcp-config", cfg.MCPFlag)
	}
	if cfg.MCPMarker != `"aileron"` {
		t.Errorf("MCPMarker = %q; want the JSON-key form \"aileron\"", cfg.MCPMarker)
	}
	if cfg.AuthPath != "/home/agent/.claude/.credentials.json" {
		t.Errorf("AuthPath = %q; want the claude .credentials.json path", cfg.AuthPath)
	}
	if cfg.WorkspaceContainerPath != "/home/agent/workspace" {
		t.Errorf("WorkspaceContainerPath = %q; want /home/agent/workspace", cfg.WorkspaceContainerPath)
	}
	if cfg.SentinelName != ".aileron-systest-sentinel" {
		t.Errorf("SentinelName = %q; want .aileron-systest-sentinel", cfg.SentinelName)
	}
	wantFlags := []string{"-p"}
	if len(cfg.BatchFlags) != len(wantFlags) {
		t.Fatalf("BatchFlags = %v; want %v", cfg.BatchFlags, wantFlags)
	}
	for i := range wantFlags {
		if cfg.BatchFlags[i] != wantFlags[i] {
			t.Errorf("BatchFlags[%d] = %q; want %q", i, cfg.BatchFlags[i], wantFlags[i])
		}
	}
}

func TestClaudeConfigHonorsExpectedImageOverride(t *testing.T) {
	override := "aileron/sandbox-agent-claude:latest"
	cfg := systestlib.ClaudeConfig(override)
	if cfg.ExpectedImage != override {
		t.Errorf("ExpectedImage = %q; want the override %q", cfg.ExpectedImage, override)
	}
}

// TestBuildExecArgsClaude proves the claude batch flag assembles to
// `-p "<prompt>"`, the post-`--` token list forwarded to the claude CLI.
func TestBuildExecArgsClaude(t *testing.T) {
	cfg := systestlib.ClaudeConfig("")
	args := systestlib.BuildExecArgs(cfg, "PROMPT")
	want := []string{"-p", "PROMPT"}
	if len(args) != len(want) {
		t.Fatalf("BuildExecArgs = %v; want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q; want %q", i, args[i], want[i])
		}
	}
}

// TestBuildPromptClaude proves the shared deterministic prompt builder works with
// the claude config (same workspace/sentinel bindings, same two side effects).
func TestBuildPromptClaude(t *testing.T) {
	cfg := systestlib.ClaudeConfig("")
	sentinel := systestlib.Sentinel("17-42-abc")
	prompt := systestlib.BuildPrompt(cfg, sentinel)

	if !strings.Contains(prompt, sentinel) {
		t.Errorf("prompt missing the sentinel token %q:\n%s", sentinel, prompt)
	}
	wantPath := cfg.WorkspaceContainerPath + "/" + cfg.SentinelName
	if !strings.Contains(prompt, wantPath) {
		t.Errorf("prompt missing the sentinel file path %q:\n%s", wantPath, prompt)
	}
	if !strings.Contains(prompt, "${AILERON_URL}/healthz") {
		t.Errorf("prompt missing the healthz URL (literal ${AILERON_URL}):\n%s", prompt)
	}
}

func TestResolveExpectedImage(t *testing.T) {
	const def = "ghcr.io/alrubinger/aileron-sandbox-codex:edge"
	if got := systestlib.ResolveExpectedImage("", def); got != def {
		t.Errorf("empty override = %q; want default %q", got, def)
	}
	override := "ghcr.io/alrubinger/aileron-sandbox-codex@sha256:abc"
	if got := systestlib.ResolveExpectedImage(override, def); got != override {
		t.Errorf("override = %q; want %q (override beats default)", got, override)
	}
}

func TestCodexConfigHonorsExpectedImageOverride(t *testing.T) {
	override := "ghcr.io/alrubinger/aileron-sandbox-codex:latest"
	cfg := systestlib.CodexConfig(override)
	if cfg.ExpectedImage != override {
		t.Errorf("ExpectedImage = %q; want the override %q", cfg.ExpectedImage, override)
	}
}

func TestBuildExecArgs(t *testing.T) {
	cfg := systestlib.CodexConfig("")
	args := systestlib.BuildExecArgs(cfg, "PROMPT")
	want := []string{"exec", "--skip-git-repo-check", "PROMPT"}
	if len(args) != len(want) {
		t.Fatalf("BuildExecArgs = %v; want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q; want %q", i, args[i], want[i])
		}
	}
}

// TestBuildExecArgsDoesNotAliasBatchFlags proves the returned slice is
// independent, so a caller mutating it cannot corrupt the config's BatchFlags.
func TestBuildExecArgsDoesNotAliasBatchFlags(t *testing.T) {
	cfg := systestlib.CodexConfig("")
	args := systestlib.BuildExecArgs(cfg, "P")
	args[0] = "MUTATED"
	if cfg.BatchFlags[0] != "exec" {
		t.Errorf("mutating the result changed cfg.BatchFlags[0] to %q; want it isolated", cfg.BatchFlags[0])
	}
}

func TestBuildPrompt(t *testing.T) {
	cfg := systestlib.CodexConfig("")
	sentinel := systestlib.Sentinel("17-42-abc")
	prompt := systestlib.BuildPrompt(cfg, sentinel)

	// Side effect 1: write the exact sentinel to the workspace sentinel file.
	if !strings.Contains(prompt, sentinel) {
		t.Errorf("prompt missing the sentinel token %q:\n%s", sentinel, prompt)
	}
	wantPath := cfg.WorkspaceContainerPath + "/" + cfg.SentinelName
	if !strings.Contains(prompt, wantPath) {
		t.Errorf("prompt missing the sentinel file path %q:\n%s", wantPath, prompt)
	}
	// Side effect 2: call the http_request tool GET ${AILERON_URL}/healthz.
	if !strings.Contains(prompt, "http_request") {
		t.Errorf("prompt missing the http_request instruction:\n%s", prompt)
	}
	if !strings.Contains(prompt, "${AILERON_URL}/healthz") {
		t.Errorf("prompt missing the healthz URL (literal ${AILERON_URL}):\n%s", prompt)
	}
	if !strings.Contains(prompt, "method GET") {
		t.Errorf("prompt missing the GET method:\n%s", prompt)
	}
}
