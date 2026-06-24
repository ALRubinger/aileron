package agents_test

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/launch/agents"
)

// TestSkillsPaths pins each agent's container-side Agent Skills directory.
// Claude and Codex have grounded paths (the launcher bind-mounts the
// canonical skill store read-only there at sandbox launch); the others
// return "" until their skills layout is confirmed, which the launcher
// treats as "skip the skills mount".
func TestSkillsPaths(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"claude subscription", agents.NewClaude(agents.ClaudeAuthModeSubscription).SkillsPath(), "/home/agent/.claude/skills"},
		{"claude apikey", agents.NewClaude(agents.ClaudeAuthModeAPIKey).SkillsPath(), "/home/agent/.claude/skills"},
		{"codex", agents.Codex{}.SkillsPath(), "/home/agent/.codex/skills"},
		{"goose", agents.Goose{}.SkillsPath(), ""},
		{"opencode", agents.OpenCode{}.SkillsPath(), ""},
		{"pi", agents.Pi{}.SkillsPath(), ""},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s SkillsPath() = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}
