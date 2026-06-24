package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ALRubinger/aileron/internal/launch"
)

// Goose is the agent definition for Block's Goose CLI.
//
// Per ADR-0015, Aileron is not the trust surface for Goose's local
// shell commands; Goose's developer-extension permission model
// (GOOSE_MODE=auto for autonomous; smart_approve/approve for prompted)
// stays in charge.
//
// MCP wiring uses Goose's `session --with-extension` CLI flag rather
// than writing into `~/.config/goose/config.yaml`. Goose v1.9.x
// deserializes the `extensions:` block in one shot as a HashMap; if
// any single entry fails the schema (e.g. a `type: platform` entry
// written by a newer Goose version), the whole map silently becomes
// empty and no extensions load. The `--with-extension` flag parses
// directly into an ExtensionConfig::Stdio and bypasses that fragile
// map decode.
type Goose struct{}

func (g Goose) Name() string          { return "goose" }
func (g Goose) BinaryNames() []string { return []string{"goose"} }

// Args returns `["session"]` so the launcher invokes `goose session
// --with-extension …`. `--with-extension` is a session-subcommand
// flag in Goose's clap config; without an explicit subcommand the
// flag isn't recognized.
func (g Goose) Args(_ launch.Mode) []string { return []string{"session"} }

// Env sets GOOSE_MODE=auto so Goose runs without per-tool approval
// prompts. Users who want Goose's own approval prompts can override
// this in their shell or use a wrapper script.
func (g Goose) Env() map[string]string {
	return map[string]string{"GOOSE_MODE": "auto"}
}

// LLMEndpointEnv returns "" — Goose configures its LLM provider base
// URL in `config.yaml` per provider, not via a single env var.
// Gateway routing for Goose is not available under launch today.
func (g Goose) LLMEndpointEnv() string { return "" }

// ConfigureMCP returns CLI args that register aileron-mcp as a stdio
// extension via Goose's `--with-extension` flag. The value format is
// `ENV1=val1 ENV2=val2 cmd [args]` per Goose's parse_stdio_extension
// parser. Values containing whitespace or shell metacharacters get
// double-quoted.
//
// As a side effect (host mode only) this removes any stale `aileron`
// entry from `~/.config/goose/config.yaml` left by prior launches that
// wrote to the config file. Without cleanup, a future Goose version
// with a fixed deserializer would load the stale config entry alongside
// the live --with-extension one and spawn aileron-mcp twice (with the
// wrong daemon URL and session ID from the stale entry). Under sandbox
// mode the in-container Goose never reads the host config, so the
// cleanup is skipped to keep the host config untouched.
func (g Goose) ConfigureMCP(mcpBin string, mcpEnv map[string]string, _ string, mode launch.Mode) ([]string, []launch.MCPMount, error) {
	if mode == launch.ModeHost {
		if err := removeStaleAileronEntry(); err != nil {
			return nil, nil, fmt.Errorf("cleaning stale goose config entry: %w", err)
		}
	}
	return []string{"--with-extension", encodeWithExtensionValue(mcpBin, mcpEnv)}, nil, nil
}

// encodeWithExtensionValue builds the `ENV=val ENV=val cmd` string
// Goose's parse_stdio_extension consumes. Env vars are emitted in
// sorted order so the output is deterministic (Go map iteration is
// otherwise randomized, which would make tests flaky and would
// produce different cache keys for what is logically the same
// invocation).
func encodeWithExtensionValue(cmd string, envs map[string]string) string {
	keys := make([]string, 0, len(envs))
	for k := range envs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		parts = append(parts, k+"="+shellQuoteIfNeeded(envs[k]))
	}
	parts = append(parts, shellQuoteIfNeeded(cmd))
	return strings.Join(parts, " ")
}

// shellQuoteIfNeeded wraps s in double quotes (escaping any embedded
// quotes and backslashes) when it contains whitespace or shell
// metacharacters. Goose's split_quoted understands double-quoted
// strings, so the round-trip is lossless for typical paths and env
// values.
func shellQuoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// removeStaleAileronEntry deletes any `extensions.aileron` block
// from `~/.config/goose/config.yaml` (leaving the rest of the file
// untouched). A missing config file is not an error — the user
// just hasn't run Goose yet.
func removeStaleAileronEntry() error {
	configDir, err := gooseConfigDir()
	if err != nil {
		return err
	}
	path := filepath.Join(configDir, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", path, err)
	}
	root := map[string]any{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		// Don't fail the launch over a malformed pre-existing
		// config — leave it alone and let Goose itself surface
		// any parse errors.
		return nil
	}
	extensions, ok := root["extensions"].(map[string]any)
	if !ok {
		return nil
	}
	if _, present := extensions[launch.MCPServerName]; !present {
		return nil
	}
	delete(extensions, launch.MCPServerName)
	root["extensions"] = extensions

	out, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshaling goose config: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// gooseConfigDir returns Goose's per-user config directory:
//
//   - macOS / Linux: `~/.config/goose` (Goose uses XDG on macOS too,
//     not `~/Library/Application Support`).
//   - Windows: `%APPDATA%\Block\goose`.
//
// `os.UserConfigDir` returns the wrong macOS path so we resolve XDG
// manually on Unix-likes.
func gooseConfigDir() (string, error) {
	if runtimeIsWindows() {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("determining user config dir: %w", err)
		}
		return filepath.Join(cfg, "Block", "goose"), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "goose"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining home directory: %w", err)
	}
	return filepath.Join(home, ".config", "goose"), nil
}

// AuthSpec returns Goose's vault-backed credential descriptor. Goose
// authenticates with a provider API key (read from a `<PROVIDER>_API_KEY`
// env var or its keyring), not a rotatable on-disk credential file, so
// the binding is an EnvBinding: the stored key is rendered into the
// container env at launch. There is no Capture (an env-key binding has
// nothing to snapshot back) and no PreLaunchRefresh (a static API key
// does not rotate). Required is false so an unseeded vault falls through
// to Goose's normal provider-key resolution instead of hard-failing.
// SkillsPath returns "" because Goose's Agent Skills directory under the
// sandbox is not yet grounded; the launcher skips the skills mount for it.
// Wiring Goose's skills path is deferred until its layout is confirmed.
func (g Goose) SkillsPath() string { return "" }

func (g Goose) AuthSpec() launch.AuthSpec {
	return launch.AuthSpec{
		EnvBindings: []launch.EnvBinding{{
			VaultPath: gooseVaultPath,
			Required:  false,
			Render:    gooseRender,
		}},
	}
}
