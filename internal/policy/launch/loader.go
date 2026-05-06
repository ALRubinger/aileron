package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"gopkg.in/yaml.v3"
)

const (
	// userSettingsDir is the directory under $HOME where user settings live.
	userSettingsDir = ".aileron"
	// userSettingsFile is the filename for user-level policy settings.
	userSettingsFile = "settings.yaml"
)

// Layer names tag rule IDs for auditability.
const (
	LayerDefault = "default"
	LayerUser    = "user"
	LayerProject = "project"
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
// defaults and user settings per ADR-0015 and ADR-0016.
//
// Composition order (each layer wins over the previous for same-pattern rules):
//
//	Built-in defaults (DefaultPolicy — all languages, OS-specific rules)
//	  → User settings (~/.aileron/settings.yaml)
//	    → Project aileron.yaml
//	      → Built-in structural ask rules (injected by ToEngineRules)
//
// When two layers define a rule with the same command pattern, the later
// layer replaces the earlier one. When patterns differ, the engine picks
// the most specific match at eval time (specificity = non-wildcard chars).
func LoadWithProfiles(path string) (*PolicyFile, error) {
	project, err := Load(path)
	if err != nil {
		return nil, err
	}

	// Start from built-in defaults, tagged as "default" layer.
	base := DefaultPolicy()
	tagRules(base, LayerDefault)

	// Layer user settings on top of defaults.
	userSettings, err := LoadUserSettings()
	if err != nil {
		return nil, err
	}
	tagRules(userSettings, LayerUser)
	base = Merge(base, userSettings)

	// Project policy is the final overlay.
	tagRules(project, LayerProject)
	return Merge(base, project), nil
}

// tagRules prefixes rule IDs with a layer name for auditability.
// Rules that already have a layer prefix (contain ":") are skipped.
func tagRules(pf *PolicyFile, layer string) {
	tagBucket := func(rules []Rule, bucket string) {
		for i := range rules {
			if rules[i].ID == "" {
				rules[i].ID = fmt.Sprintf("%s:%s_%d", layer, bucket, i)
			} else if !strings.Contains(rules[i].ID, ":") {
				rules[i].ID = fmt.Sprintf("%s:%s", layer, rules[i].ID)
			}
		}
	}
	tagBucket(pf.Allow, "allow")
	tagBucket(pf.Deny, "deny")
	tagBucket(pf.Ask, "ask")
}

// Merge composes two policy files. For rules with the same command
// pattern, the overlay rule replaces the base rule (later layer wins).
// Rules with different patterns coexist. Scalar settings use
// last-writer-wins (overlay wins). Env scrub lists are unioned;
// passthrough at any layer beats scrub.
func Merge(base, overlay *PolicyFile) *PolicyFile {
	// Merge rules with pattern-based replacement: overlay replaces base
	// rules that have the same command pattern.
	result := &PolicyFile{
		Version: overlay.Version,
		Default: base.Default,
		Allow:   mergeRuleBuckets(base.Allow, overlay.Allow, collectAllPatterns(overlay)),
		Deny:    mergeRuleBuckets(base.Deny, overlay.Deny, collectAllPatterns(overlay)),
		Ask:     mergeRuleBuckets(base.Ask, overlay.Ask, collectAllPatterns(overlay)),
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

// collectAllPatterns returns the set of command patterns from all buckets
// of a policy file. Used to identify which base rules should be replaced.
func collectAllPatterns(pf *PolicyFile) map[string]bool {
	patterns := make(map[string]bool)
	for _, buckets := range [][]Rule{pf.Allow, pf.Deny, pf.Ask} {
		for _, r := range buckets {
			if r.Command != "" {
				patterns[r.Command] = true
			}
		}
	}
	return patterns
}

// mergeRuleBuckets merges a base and overlay bucket. Base rules whose
// command pattern appears in the overlay's pattern set are dropped
// (the overlay rule in whatever bucket replaces them).
func mergeRuleBuckets(base, overlay []Rule, overlayPatterns map[string]bool) []Rule {
	var result []Rule
	for _, r := range base {
		if r.Command != "" && overlayPatterns[r.Command] {
			continue // overlay replaces this pattern
		}
		result = append(result, r)
	}
	result = append(result, overlay...)
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

// mergeNotify merges notification configs with per-channel granularity.
// Tokens use last-writer-wins. Channels are unioned by name; for channels
// present in both layers the overlay's non-zero fields override the base's.
// Ignore lists are unioned.
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
		result.Slack = mergeSlack(base.Slack, overlay.Slack)
	}
	if overlay.Discord != nil {
		result.Discord = mergeDiscord(base.Discord, overlay.Discord)
	}
	return &result
}

// mergeSlack merges two SlackNotifyConfigs with per-channel granularity.
func mergeSlack(base, overlay *SlackNotifyConfig) *SlackNotifyConfig {
	if base == nil {
		return overlay
	}
	result := *base
	// Tokens: last-writer-wins.
	if overlay.AppToken != "" {
		result.AppToken = overlay.AppToken
	}
	if overlay.BotToken != "" {
		result.BotToken = overlay.BotToken
	}
	// Channels: union by name, overlay fields override per channel.
	result.Channels = mergeChannels(base.Channels, overlay.Channels)
	// Ignore: union.
	result.Ignore = appendUnique(append([]string{}, base.Ignore...), overlay.Ignore...)
	return &result
}

// mergeDiscord merges two DiscordNotifyConfigs with per-channel granularity.
func mergeDiscord(base, overlay *DiscordNotifyConfig) *DiscordNotifyConfig {
	if base == nil {
		return overlay
	}
	result := *base
	// Token: last-writer-wins.
	if overlay.BotToken != "" {
		result.BotToken = overlay.BotToken
	}
	// Channels: union by name, overlay fields override per channel.
	result.Channels = mergeChannels(base.Channels, overlay.Channels)
	// Ignore: union.
	result.Ignore = appendUnique(append([]string{}, base.Ignore...), overlay.Ignore...)
	return &result
}

// mergeChannels unions two channel lists by name. For channels present in
// both, the overlay's non-zero fields override the base's values.
func mergeChannels(base, overlay []ChannelConfig) []ChannelConfig {
	index := make(map[string]int, len(base))
	result := make([]ChannelConfig, len(base))
	copy(result, base)
	for i, ch := range result {
		index[ch.Name] = i
	}
	for _, ch := range overlay {
		if i, ok := index[ch.Name]; ok {
			// Merge: overlay fields win. When a user defines a channel
			// in their settings, all specified fields take effect.
			if ch.Show != "" {
				result[i].Show = ch.Show
			}
			// AutoDraft is always applied from overlay because the user
			// explicitly listing a channel means they intend to control it.
			result[i].AutoDraft = ch.AutoDraft
			if ch.Priority != "" {
				result[i].Priority = ch.Priority
			}
		} else {
			index[ch.Name] = len(result)
			result = append(result, ch)
		}
	}
	return result
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
// suitable for the RuleEngine. It includes built-in structural ask rules
// and a catch-all default rule.
//
// Priority is computed from pattern specificity: count of non-wildcard
// characters in the command pattern. The engine sorts by priority
// descending, so more specific patterns win. For equal specificity,
// a small offset biases toward safety: deny+2, ask+1, allow+0.
func (pf *PolicyFile) ToEngineRules() []api.PolicyRule {
	var rules []api.PolicyRule

	for i, r := range pf.Allow {
		rules = append(rules, ruleToEngine(r, api.PolicyRuleEffectAllow, "allow", i))
	}
	for i, r := range pf.Ask {
		rules = append(rules, ruleToEngine(r, api.PolicyRuleEffectRequireApproval, "ask", i))
	}
	for i, r := range pf.Deny {
		rules = append(rules, ruleToEngine(r, api.PolicyRuleEffectDeny, "deny", i))
	}

	// Add built-in structural ask rules.
	rules = append(rules, BuiltinAskRules()...)

	// Add default catch-all rule.
	rules = append(rules, defaultRule(pf.Default))

	return rules
}

// PatternSpecificity returns the number of non-wildcard characters in
// a pattern. More literal characters means a more specific match.
// Exported for testing.
func PatternSpecificity(pattern string) int {
	count := 0
	for _, ch := range pattern {
		if ch != '*' {
			count++
		}
	}
	return count
}

// effectOffset returns a small tie-breaker offset for equal-specificity
// rules: deny > ask > allow (safety bias).
func effectOffset(effect api.PolicyRuleEffect) int {
	switch effect {
	case api.PolicyRuleEffectDeny:
		return 2
	case api.PolicyRuleEffectRequireApproval:
		return 1
	default:
		return 0
	}
}

func ruleToEngine(r Rule, effect api.PolicyRuleEffect, bucket string, index int) api.PolicyRule {
	id := r.ID
	if id == "" {
		id = fmt.Sprintf("%s_%d", bucket, index)
	}

	// Compute specificity-based priority from the command pattern.
	pattern := r.Command
	if pattern == "" {
		pattern = r.Binary
	}
	priority := PatternSpecificity(pattern)*3 + effectOffset(effect)

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
