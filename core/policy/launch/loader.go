package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"gopkg.in/yaml.v3"
)

const (
	// userSettingsDir is the directory under $HOME where user settings live.
	userSettingsDir = ".aileron"
	// userSettingsFile is the filename for user-level policy settings.
	userSettingsFile = "settings.yaml"
)

// Default priorities for each bucket.
const (
	PriorityAllow      = 50
	PriorityAsk        = 100
	PriorityBuiltinAsk = 150 // built-in ask rules sit between user ask and user deny
	PriorityDeny       = 200
)

// Load parses a single aileron.yaml file.
func Load(path string) (*PolicyFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading policy file: %w", err)
	}
	return Parse(data)
}

// Parse parses aileron.yaml content from bytes.
func Parse(data []byte) (*PolicyFile, error) {
	var pf PolicyFile
	if err := yaml.Unmarshal(data, &pf); err != nil {
		return nil, fmt.Errorf("parsing policy file: %w", err)
	}
	if pf.Version == 0 {
		pf.Version = 1
	}
	return &pf, nil
}

// LoadUserSettings loads the user's personal settings from
// ~/.aileron/settings.yaml. If the file does not exist, an empty
// PolicyFile is returned (the user hasn't created one yet). If the
// file exists but contains invalid YAML, an error is returned so the
// user knows their settings are broken.
func LoadUserSettings() (*PolicyFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return &PolicyFile{Version: 1}, nil
	}
	path := filepath.Join(home, userSettingsDir, userSettingsFile)
	_, err = os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return &PolicyFile{Version: 1}, nil
	}
	pf, err := Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading user settings %s: %w", path, err)
	}
	return pf, nil
}

// LoadWithProfiles loads a project policy file and merges it with built-in
// defaults and user settings per ADR-0015.
//
// Composition order (each layer wins over the previous):
//
//	Built-in defaults (DefaultPolicy — all languages, OS-specific rules)
//	  → User settings (~/.aileron/settings.yaml)
//	    → Project aileron.yaml
//	      → Built-in structural deny rules (injected by ToEngineRules)
func LoadWithProfiles(path string) (*PolicyFile, error) {
	project, err := Load(path)
	if err != nil {
		return nil, err
	}

	// Start from built-in defaults.
	base := DefaultPolicy()

	// Layer user settings on top of defaults.
	userSettings, err := LoadUserSettings()
	if err != nil {
		return nil, err
	}
	base = Merge(base, userSettings)

	// Project policy is the final overlay.
	return Merge(base, project), nil
}

// Merge composes two policy files. The overlay's rules are appended to the
// base's rules. Scalar settings use last-writer-wins (overlay wins). Env
// scrub lists are unioned; passthrough at any layer beats scrub.
func Merge(base, overlay *PolicyFile) *PolicyFile {
	result := &PolicyFile{
		Version: overlay.Version,
		Default: base.Default,
		Allow:    append(append([]Rule{}, base.Allow...), overlay.Allow...),
		Deny:     append(append([]Rule{}, base.Deny...), overlay.Deny...),
		Ask:      append(append([]Rule{}, base.Ask...), overlay.Ask...),
	}

	// Last-writer-wins for default and settings.
	if overlay.Default != "" {
		result.Default = overlay.Default
	}

	// Merge settings: overlay wins for non-zero fields.
	switch {
	case base.Settings == nil && overlay.Settings == nil:
		// nothing
	case base.Settings == nil:
		s := *overlay.Settings
		result.Settings = &s
	case overlay.Settings == nil:
		s := *base.Settings
		result.Settings = &s
	default:
		merged := *base.Settings
		if overlay.Settings.AskMode != "" {
			merged.AskMode = overlay.Settings.AskMode
		}
		if overlay.Settings.AuditLog != "" {
			merged.AuditLog = overlay.Settings.AuditLog
		}
		if overlay.Settings.Timeout != 0 {
			merged.Timeout = overlay.Settings.Timeout
		}
		result.Settings = &merged
	}

	// Merge env: union scrub, passthrough beats scrub.
	result.Env = mergeEnv(base.Env, overlay.Env)

	// Merge notifications: overlay wins (last-writer-wins for the whole block).
	result.Notifications = mergeNotify(base.Notifications, overlay.Notifications)

	// Process overrides: collect all override directives from overlay, then
	// remove matching rules from allow and ask. Deny rules cannot be overridden.
	allOverlaySources := append(append([]Rule{}, overlay.Allow...), overlay.Ask...)
	allOverlaySources = append(allOverlaySources, overlay.Deny...)
	result.Allow = applyOverrides(result.Allow, allOverlaySources)
	result.Ask = applyOverrides(result.Ask, allOverlaySources)

	return result
}

// mergeEnv unions scrub lists and applies passthrough-beats-scrub.
func mergeEnv(base, overlay *EnvConfig) *EnvConfig {
	if base == nil && overlay == nil {
		return nil
	}
	result := &EnvConfig{}
	if base != nil {
		result.Scrub = append(result.Scrub, base.Scrub...)
		result.Passthrough = append(result.Passthrough, base.Passthrough...)
	}
	if overlay != nil {
		result.Scrub = appendUnique(result.Scrub, overlay.Scrub...)
		result.Passthrough = appendUnique(result.Passthrough, overlay.Passthrough...)
	}
	// Passthrough beats scrub: remove any scrub entries that appear in passthrough.
	if len(result.Passthrough) > 0 {
		ptSet := make(map[string]bool, len(result.Passthrough))
		for _, p := range result.Passthrough {
			ptSet[p] = true
		}
		filtered := result.Scrub[:0]
		for _, s := range result.Scrub {
			if !ptSet[s] {
				filtered = append(filtered, s)
			}
		}
		result.Scrub = filtered
	}
	return result
}

// mergeNotify merges notification configs. The overlay wins for each
// service block (Slack, Discord). If the overlay defines a service,
// it replaces the base entirely for that service.
func mergeNotify(base, overlay *NotifyConfig) *NotifyConfig {
	if base == nil && overlay == nil {
		return nil
	}
	if base == nil {
		return overlay
	}
	if overlay == nil {
		return base
	}
	result := *base
	if overlay.Slack != nil {
		result.Slack = overlay.Slack
	}
	if overlay.Discord != nil {
		result.Discord = overlay.Discord
	}
	return &result
}

func appendUnique(base []string, items ...string) []string {
	seen := make(map[string]bool, len(base))
	for _, s := range base {
		seen[s] = true
	}
	for _, s := range items {
		if !seen[s] {
			base = append(base, s)
			seen[s] = true
		}
	}
	return base
}

// applyOverrides removes rules whose ID matches an override directive.
func applyOverrides(rules []Rule, overrideSource []Rule) []Rule {
	overrides := make(map[string]bool)
	for _, r := range overrideSource {
		if r.Override != "" {
			overrides[r.Override] = true
		}
	}
	if len(overrides) == 0 {
		return rules
	}
	filtered := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if r.ID != "" && overrides[r.ID] {
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered
}

// ToEngineRules translates the policy file into api.PolicyRule types
// suitable for the RuleEngine. It includes built-in structural deny rules
// and a catch-all default rule.
func (pf *PolicyFile) ToEngineRules() []api.PolicyRule {
	var rules []api.PolicyRule

	for i, r := range pf.Allow {
		rules = append(rules, ruleToEngine(r, api.PolicyRuleEffectAllow, PriorityAllow, "allow", i))
	}
	for i, r := range pf.Ask {
		rules = append(rules, ruleToEngine(r, api.PolicyRuleEffectRequireApproval, PriorityAsk, "ask", i))
	}
	for i, r := range pf.Deny {
		rules = append(rules, ruleToEngine(r, api.PolicyRuleEffectDeny, PriorityDeny, "deny", i))
	}

	// Add built-in structural deny rules.
	rules = append(rules, BuiltinAskRules()...)

	// Add default catch-all rule.
	rules = append(rules, defaultRule(pf.Default))

	return rules
}

func ruleToEngine(r Rule, effect api.PolicyRuleEffect, defaultPriority int, bucket string, index int) api.PolicyRule {
	id := r.ID
	if id == "" {
		id = fmt.Sprintf("%s_%d", bucket, index)
	}

	priority := defaultPriority
	if r.Priority != nil {
		priority = *r.Priority
	}

	var conditions []api.PolicyCondition

	// Every launch rule matches action.type = "shell.exec".
	conditions = append(conditions, makeCondition("action.type", api.Eq, "shell.exec"))

	if r.Command != "" {
		conditions = append(conditions, makeCondition("shell.command", api.Matches, r.Command))
	}
	if r.Binary != "" {
		conditions = append(conditions, makeCondition("shell.binary", api.Matches, r.Binary))
	}
	if r.ArgsContain != "" {
		conditions = append(conditions, makeCondition("shell.args", api.Contains, r.ArgsContain))
	}
	if r.WorkingDir != "" {
		conditions = append(conditions, makeCondition("shell.working_dir", api.Matches, r.WorkingDir))
	}

	desc := r.Description
	return api.PolicyRule{
		RuleId:      id,
		Description: &desc,
		Effect:      effect,
		Priority:    &priority,
		Conditions:  &conditions,
	}
}

func defaultRule(disposition string) api.PolicyRule {
	effect := api.PolicyRuleEffectRequireApproval // default: ask
	switch disposition {
	case "allow":
		effect = api.PolicyRuleEffectAllow
	case "deny":
		effect = api.PolicyRuleEffectDeny
	}

	priority := 1
	desc := "default disposition"
	return api.PolicyRule{
		RuleId:      "default",
		Description: &desc,
		Effect:      effect,
		Priority:    &priority,
		Conditions:  &[]api.PolicyCondition{makeCondition("action.type", api.Eq, "shell.exec")},
	}
}

func makeCondition(field string, op api.PolicyConditionOperator, value any) api.PolicyCondition {
	f := field
	o := op
	return api.PolicyCondition{
		Field:    &f,
		Operator: &o,
		Value:    makeValue(value),
	}
}

func makeValue(v any) *api.PolicyCondition_Value {
	data, _ := json.Marshal(v)
	val := &api.PolicyCondition_Value{}
	_ = val.UnmarshalJSON(data)
	return val
}
