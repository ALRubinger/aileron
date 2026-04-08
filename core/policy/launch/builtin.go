package launch

import api "github.com/ALRubinger/aileron/core/api/gen"

// BuiltinAskRules returns default rules for commands with dynamic or
// obfuscated patterns. These default to "ask" (prompt the developer) rather
// than hard-deny, because the agent host (e.g., Claude Code) is better
// positioned to evaluate whether a dynamic command is suspicious — it has
// conversation context that a static glob pattern cannot.
//
// These rules are overridable by user policy. A developer who routinely uses
// `python -c` can add it to their allow list.
//
// Aileron's value is the predictable middle: auto-approving safe commands and
// blocking obviously dangerous ones. Obfuscation detection is the agent
// host's job. Aileron captures all approval decisions — from both layers —
// in the audit trail.
func BuiltinAskRules() []api.PolicyRule {
	rules := []struct {
		id      string
		pattern string
		desc    string
	}{
		{
			id:      "builtin_pipe_to_shell",
			pattern: "*| bash*",
			desc:    "pipe to bash execution",
		},
		{
			id:      "builtin_pipe_to_sh",
			pattern: "*| sh*",
			desc:    "pipe to sh execution",
		},
		{
			id:      "builtin_pipe_to_exec",
			pattern: "*| exec*",
			desc:    "pipe to exec",
		},
		{
			id:      "builtin_eval",
			pattern: "eval *",
			desc:    "eval arbitrary code execution",
		},
		{
			id:      "builtin_base64_pipe",
			pattern: "*base64*|*",
			desc:    "base64 decode piped to execution",
		},
		{
			id:      "builtin_curl_subshell",
			pattern: "*$(curl *",
			desc:    "remote fetch into command substitution",
		},
		{
			id:      "builtin_wget_subshell",
			pattern: "*$(wget *",
			desc:    "remote fetch into command substitution",
		},
		{
			id:      "builtin_python_c",
			pattern: "python -c *",
			desc:    "inline Python execution",
		},
		{
			id:      "builtin_python3_c",
			pattern: "python3 -c *",
			desc:    "inline Python execution",
		},
		{
			id:      "builtin_node_e",
			pattern: "node -e *",
			desc:    "inline Node.js execution",
		},
		{
			id:      "builtin_ruby_e",
			pattern: "ruby -e *",
			desc:    "inline Ruby execution",
		},
		{
			id:      "builtin_perl_e",
			pattern: "perl -e *",
			desc:    "inline Perl execution",
		},
	}

	result := make([]api.PolicyRule, 0, len(rules))
	for _, r := range rules {
		priority := PriorityBuiltinAsk
		desc := r.desc
		conditions := []api.PolicyCondition{
			makeCondition("action.type", api.Eq, "shell.exec"),
			makeCondition("shell.command", api.Matches, r.pattern),
		}
		result = append(result, api.PolicyRule{
			RuleId:      r.id,
			Description: &desc,
			Effect:      api.PolicyRuleEffectRequireApproval,
			Priority:    &priority,
			Conditions:  &conditions,
		})
	}
	return result
}
