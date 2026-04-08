// Package launch defines the aileron.yaml policy file schema and provides
// a loader that translates it into the core policy engine's rule types.
package launch

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// PolicyFile is the top-level schema for aileron.yaml.
type PolicyFile struct {
	Version  int        `yaml:"version"`
	Profiles []string   `yaml:"profiles,omitempty"`
	Default  string     `yaml:"default,omitempty"` // "allow", "deny", "ask"
	Settings *Settings  `yaml:"settings,omitempty"`
	Env      *EnvConfig `yaml:"env,omitempty"`
	Allow    []Rule     `yaml:"allow,omitempty"`
	Deny     []Rule     `yaml:"deny,omitempty"`
	Ask      []Rule     `yaml:"ask,omitempty"`
}

// Settings holds launch session configuration.
type Settings struct {
	AskMode  string `yaml:"ask_mode,omitempty"`  // "terminal" or "ui"
	AuditLog string `yaml:"audit_log,omitempty"` // path to audit log file
	Timeout  int    `yaml:"timeout,omitempty"`   // seconds to wait for human response
}

// EnvConfig controls environment variable scrubbing.
type EnvConfig struct {
	Scrub       []string `yaml:"scrub,omitempty"`
	Passthrough []string `yaml:"passthrough,omitempty"`
}

// Rule supports YAML unmarshaling from both string (short form) and map
// (long form).
//
// Short form:
//
//	allow:
//	  - "go test ./..."
//
// Long form:
//
//	deny:
//	  - command: "rm -rf *"
//	    description: "block recursive force delete"
type Rule struct {
	ID          string `yaml:"id,omitempty"`
	Command     string `yaml:"command,omitempty"`
	Binary      string `yaml:"binary,omitempty"`
	ArgsContain string `yaml:"args_contain,omitempty"`
	WorkingDir  string `yaml:"working_dir,omitempty"`
	Description string `yaml:"description,omitempty"`
	Priority    *int   `yaml:"priority,omitempty"`
	Override    string `yaml:"override,omitempty"`
}

// UnmarshalYAML allows a Rule to be a plain string or a mapping.
func (r *Rule) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		r.Command = value.Value
		return nil
	case yaml.MappingNode:
		// Decode into an alias type to avoid infinite recursion.
		type rawRule Rule
		var raw rawRule
		if err := value.Decode(&raw); err != nil {
			return fmt.Errorf("decoding rule: %w", err)
		}
		*r = Rule(raw)
		return nil
	default:
		return fmt.Errorf("rule must be a string or mapping, got %v", value.Kind)
	}
}
