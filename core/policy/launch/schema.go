// Package launch defines the aileron.yaml policy file schema and provides
// a loader that translates it into the core policy engine's rule types.
package launch

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// PolicyFile is the top-level schema for aileron.yaml.
type PolicyFile struct {
	Version       int              `yaml:"version"`
	Default       string           `yaml:"default,omitempty"` // "allow", "deny", "ask"
	Settings      *Settings        `yaml:"settings,omitempty"`
	Env           *EnvConfig       `yaml:"env,omitempty"`
	Notifications *NotifyConfig    `yaml:"notifications,omitempty"`
	Secrets       SecretsConfig    `yaml:"secrets,omitempty"`
	Allow         []Rule           `yaml:"allow,omitempty"`
	Deny          []Rule           `yaml:"deny,omitempty"`
	Ask           []Rule           `yaml:"ask,omitempty"`
}

// SecretsConfig maps secret names to their target URL patterns. When an
// http_request matches a target pattern, the corresponding secret is
// injected into the request's Authorization header.
type SecretsConfig map[string]SecretDef

// SecretDef defines which URL targets a secret can be injected into.
type SecretDef struct {
	Targets []string `yaml:"targets"` // URL patterns (e.g. "slack.com/api/*")
}

// Settings holds launch session configuration.
type Settings struct {
	AskMode  string `yaml:"ask_mode,omitempty"`  // "terminal" or "ui"
	AuditLog string `yaml:"audit_log,omitempty"` // path to audit log file
	Timeout  int    `yaml:"timeout,omitempty"`   // seconds to wait for human response
}

// NotifyConfig holds notification channel configuration for Slack and Discord.
type NotifyConfig struct {
	Slack      *SlackNotifyConfig   `yaml:"slack,omitempty"`
	Discord    *DiscordNotifyConfig `yaml:"discord,omitempty"`
	QuietHours *QuietHoursConfig    `yaml:"quiet_hours,omitempty"`
}

// QuietHoursConfig defines a daily window during which non-high-priority
// notifications are suppressed. Messages are still queued but the status
// bar and onChange callback are not triggered.
type QuietHoursConfig struct {
	Start    string `yaml:"start"`              // "HH:MM" in 24-hour format, e.g. "22:00"
	End      string `yaml:"end"`                // "HH:MM" in 24-hour format, e.g. "08:00"
	Timezone string `yaml:"timezone,omitempty"` // IANA timezone, e.g. "America/New_York"; defaults to local
}

// SlackNotifyConfig configures Slack integration.
type SlackNotifyConfig struct {
	AppToken string         `yaml:"app_token,omitempty"` // xapp-... Socket Mode token
	BotToken string         `yaml:"bot_token,omitempty"` // xoxb-... Bot token
	Channels []ChannelConfig `yaml:"channels,omitempty"`
	Ignore   []string       `yaml:"ignore,omitempty"`
}

// DiscordNotifyConfig configures Discord integration.
type DiscordNotifyConfig struct {
	BotToken string         `yaml:"bot_token,omitempty"`
	Channels []ChannelConfig `yaml:"channels,omitempty"`
	Ignore   []string       `yaml:"ignore,omitempty"`
}

// ChannelConfig defines how a single channel is handled.
type ChannelConfig struct {
	Name      string `yaml:"name"`
	Show      string `yaml:"show,omitempty"`       // "all", "mentions", "none"
	AutoDraft bool   `yaml:"auto_draft,omitempty"` // route to agent for draft reply
	Priority  string `yaml:"priority,omitempty"`   // "normal", "high"
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
