package agents

// Claude is the agent definition for Claude Code.
// Claude Code sends bash-specific commands (shopt, etc.), so the real
// shell must be bash regardless of the user's login shell.
type Claude struct{}

func (c Claude) Name() string          { return "claude" }
func (c Claude) BinaryNames() []string { return []string{"claude"} }
func (c Claude) Env() map[string]string {
	return map[string]string{
		"AILERON_REAL_SHELL": "/bin/bash",
	}
}
