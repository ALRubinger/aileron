// Package config loads and validates YAML configuration for the Aileron
// MCP gateway.
//
// A configuration file defines the set of downstream MCP servers that
// Aileron proxies, along with optional policy mappings and environment
// variable overrides. Environment values prefixed with "vault://" are
// resolved from the Aileron vault at launch time via [ResolveVaultEnv].
package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// namePattern matches valid downstream server names: starts with a letter,
// followed by zero or more alphanumeric characters or underscores.
var namePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// vaultPrefix is the sentinel prefix for environment variable values that
// should be resolved from the vault.
const vaultPrefix = "vault://"

// Execution mode constants for downstream servers.
const (
	// ModeLocal runs the MCP server as a local subprocess (stdio transport).
	// This is the default when Mode is omitted.
	ModeLocal = "local"

	// ModeRemote connects to a centrally-managed MCP server over HTTP
	// (Streamable HTTP transport). The control plane manages the server
	// instance; no local command is needed.
	ModeRemote = "remote"
)

// Config is the top-level Aileron gateway configuration.
type Config struct {
	// Version is the config file format version. Currently "1".
	Version string `yaml:"version"`

	// WorkspaceID is the default workspace for policy evaluation.
	// Defaults to "default" if empty.
	WorkspaceID string `yaml:"workspace_id"`

	// DownstreamServers lists the MCP servers that Aileron proxies.
	DownstreamServers []DownstreamServer `yaml:"downstream_servers"`
}

// DownstreamServer defines a single downstream MCP server.
type DownstreamServer struct {
	// Name is a unique identifier for this server, used as tool name prefix.
	// Must be a valid identifier (alphanumeric + underscore, no spaces).
	Name string `yaml:"name"`

	// Mode is the execution mode: "local" (default) or "remote".
	// Local servers are spawned as subprocesses; remote servers are accessed
	// over HTTP via the control plane. Omitting Mode defaults to "local".
	Mode string `yaml:"mode,omitempty"`

	// Command is the command to execute, including any arguments.
	// Required for local mode; must be empty for remote mode.
	// Example: ["npx", "-y", "@modelcontextprotocol/server-github"]
	Command []string `yaml:"command"`

	// Env is environment variables to set for the subprocess.
	// Values prefixed with "vault://" are resolved from the Aileron vault at launch time.
	Env map[string]string `yaml:"env"`

	// PolicyMapping configures how this server's tools map to policy evaluation.
	PolicyMapping *PolicyMapping `yaml:"policy_mapping,omitempty"`
}

// PolicyMapping configures how tools from a downstream server map to the
// policy engine's action type namespace.
type PolicyMapping struct {
	// ToolPrefix maps tool calls to the action type namespace.
	// For example, with tool_prefix "git", a tool "create_pull_request"
	// becomes action type "git.create_pull_request" for policy evaluation.
	ToolPrefix string `yaml:"tool_prefix"`
}

// Load reads a YAML configuration file from path, unmarshals it, and
// validates the result. It returns the parsed Config or the first error
// encountered.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// LoadFromEnvOrDefault loads configuration using the following precedence:
//
//  1. The path in the AILERON_CONFIG environment variable.
//  2. "aileron.yaml" in the current working directory.
//  3. An empty Config (no downstream servers) — this is not an error; it
//     allows the gateway to start with no downstream servers configured.
func LoadFromEnvOrDefault() (*Config, error) {
	if path := os.Getenv("AILERON_CONFIG"); path != "" {
		return Load(path)
	}

	const defaultFile = "aileron.yaml"
	if _, err := os.Stat(defaultFile); err == nil {
		return Load(defaultFile)
	}

	return &Config{}, nil
}

// Validate checks the configuration for structural correctness. It returns
// a descriptive error for the first validation failure found.
func (c *Config) Validate() error {
	if c.Version != "" && c.Version != "1" {
		return fmt.Errorf("unsupported config version %q: must be \"1\"", c.Version)
	}

	seen := make(map[string]bool, len(c.DownstreamServers))
	for i, ds := range c.DownstreamServers {
		if ds.Name == "" {
			return fmt.Errorf("downstream_servers[%d]: name must not be empty", i)
		}
		if !namePattern.MatchString(ds.Name) {
			return fmt.Errorf("downstream_servers[%d]: name %q must match %s", i, ds.Name, namePattern.String())
		}
		if seen[ds.Name] {
			return fmt.Errorf("downstream_servers[%d]: duplicate name %q", i, ds.Name)
		}
		seen[ds.Name] = true

		// Normalize and validate mode.
		mode := ds.Mode
		if mode == "" {
			mode = ModeLocal
		}
		if mode != ModeLocal && mode != ModeRemote {
			return fmt.Errorf("downstream_servers[%d] (%s): invalid mode %q: must be %q or %q", i, ds.Name, ds.Mode, ModeLocal, ModeRemote)
		}

		// Local servers require a command; remote servers must not have one.
		if mode == ModeLocal && len(ds.Command) == 0 {
			return fmt.Errorf("downstream_servers[%d] (%s): command must not be empty for local mode", i, ds.Name)
		}
		if mode == ModeRemote && len(ds.Command) > 0 {
			return fmt.Errorf("downstream_servers[%d] (%s): command must be empty for remote mode", i, ds.Name)
		}
	}

	return nil
}

// ResolveVaultEnv returns a copy of env with all "vault://"-prefixed values
// resolved through the provided vaultGet function. Non-vault values are
// copied unchanged. The original map is never modified.
func ResolveVaultEnv(env map[string]string, vaultGet func(path string) ([]byte, error)) (map[string]string, error) {
	resolved := make(map[string]string, len(env))

	for k, v := range env {
		if len(v) > len(vaultPrefix) && v[:len(vaultPrefix)] == vaultPrefix {
			vaultPath := v[len(vaultPrefix):]
			secret, err := vaultGet(vaultPath)
			if err != nil {
				return nil, fmt.Errorf("resolving vault env %q (path %q): %w", k, vaultPath, err)
			}
			resolved[k] = string(secret)
		} else {
			resolved[k] = v
		}
	}

	return resolved, nil
}
