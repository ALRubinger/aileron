package agents

// Claude is the agent definition for Claude Code.
type Claude struct{}

func (c Claude) Name() string           { return "claude" }
func (c Claude) BinaryNames() []string  { return []string{"claude"} }
func (c Claude) Env() map[string]string { return nil }
