package launch

import "runtime"

// DefaultPolicy returns the built-in policy defaults compiled into the
// Aileron binary. These provide sensible allow rules for all common
// language toolchains and tools, OS-specific deny rules activated by
// runtime.GOOS, and universal deny rules. The defaults form the base
// layer of the three-layer policy model (defaults → project → user).
func DefaultPolicy() *PolicyFile {
	pf := &PolicyFile{
		Version: 1,
		Default: "ask",
		Allow:   defaultAllowRules(),
		Deny:    append(universalDenyRules(), osDenyRules()...),
	}
	return pf
}

func defaultAllowRules() []Rule {
	return []Rule{
		// Go
		{Command: "go *"},
		{Command: "task *"},
		{Command: "golangci-lint *"},

		// Node
		{Command: "npm *"},
		{Command: "pnpm *"},
		{Command: "yarn *"},
		{Command: "npx *"},

		// Python
		{Command: "python *"},
		{Command: "python3 *"},
		{Command: "pip *"},
		{Command: "pip3 *"},
		{Command: "pytest *"},
		{Command: "uv *"},

		// Rust
		{Command: "cargo *"},
		{Command: "rustc *"},
		{Command: "rustfmt *"},

		// Ruby
		{Command: "bundle *"},
		{Command: "gem *"},
		{Command: "rake *"},

		// Elixir
		{Command: "mix *"},
		{Command: "iex *"},

		// Java
		{Command: "mvn *"},
		{Command: "gradle *"},
		{Command: "java *"},
		{Command: "javac *"},

		// Common tools
		{Command: "git status"},
		{Command: "git diff *"},
		{Command: "git log *"},
		{Command: "git branch *"},
		{Command: "git stash *"},
		{Command: "git show *"},
		{Command: "ls *"},
		{Command: "cat *"},
		{Command: "head *"},
		{Command: "tail *"},
		{Command: "find *"},
		{Command: "grep *"},
		{Command: "rg *"},
		{Command: "wc *"},
		{Command: "make *"},
		{Command: "gh pr *"},
		{Command: "gh issue *"},
		{Command: "gh run *"},
		{Command: "echo *"},
		{Command: "printf *"},
		{Command: "mkdir *"},
		{Command: "touch *"},
		{Command: "cp *"},
		{Command: "mv *"},
		{Command: "sed *"},
		{Command: "awk *"},
		{Command: "sort *"},
		{Command: "uniq *"},
		{Command: "diff *"},
		{Command: "pwd"},
		{Command: "env"},
		{Command: "which *"},
		{Command: "date"},
		{Command: "whoami"},
	}
}

func universalDenyRules() []Rule {
	return []Rule{
		{Command: "rm -rf /*", Description: "block recursive delete at filesystem root"},
		{Command: "git push origin main", Description: "no direct push to main"},
		{Command: "git push origin master", Description: "no direct push to master"},
		{Command: "gh repo delete *", Description: "never delete repos"},
	}
}

func osDenyRules() []Rule {
	switch runtime.GOOS {
	case "darwin":
		return []Rule{
			{Command: "security *", Description: "block Keychain access"},
			{Command: "defaults write *", Description: "block system preference writes"},
			{Command: "rm -rf ~/Library/*", Description: "protect macOS Library directory"},
		}
	case "linux":
		return []Rule{
			{Command: "rm -rf /etc/*", Description: "protect system configuration"},
			{Command: "systemctl *", Description: "block systemd service management"},
			{Command: "passwd *", Description: "block password changes"},
		}
	default:
		return nil
	}
}
