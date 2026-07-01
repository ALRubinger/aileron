package vaultscope

import "testing"

// Classify is the load-bearing classifier shared by the daemon vault union
// and the `aileron secret list` CLI; pin each namespace and the ordering
// edge case where an agent path also matches the binding grammar.
func TestClassify(t *testing.T) {
	cases := []struct {
		path        string
		wantScope   string
		wantControl bool
	}{
		{"agents/claude/oauth", ScopeAgent, false},
		{"agents/codex/apikey", ScopeAgent, false},
		{"user/github", ScopeUser, false},
		// Connector + OAuth capability bindings are three-segment binding
		// names. They are exactly the entries the old typed-endpoint list
		// silently hid (#1402); the union must classify them as bindings.
		{"connectors/github/default", ScopeBinding, false},
		{"oauth2/google/default", ScopeBinding, false},
		{"github/repo/me", ScopeBinding, false},
		// Control-plane namespaces: classified but flagged for exclusion.
		{"connected-accounts/usr_123/slack", ScopeConnectedAccount, true},
		{"llm-config/user/usr_123", ScopeLlmConfig, true},
		// Secrets set via `aileron secret set` live under `secret/` and are
		// locally-owned (not control-plane).
		{"secret/foo", ScopeSecret, false},
		// A `secret/`-prefixed path that also looks three-segment must still
		// classify as `secret`, proving the prefix check wins over the
		// binding grammar.
		{"secret/github/repo", ScopeSecret, false},
		// Anything matching no known namespace must still surface as `other`
		// rather than vanish.
		{"weird-single-segment", ScopeOther, false},
		{"_internal/reserved", ScopeOther, false},
	}
	for _, c := range cases {
		scope, control := Classify(c.path)
		if scope != c.wantScope || control != c.wantControl {
			t.Errorf("Classify(%q) = (%q, %v), want (%q, %v)",
				c.path, scope, control, c.wantScope, c.wantControl)
		}
	}
}

func TestAgentNameAndPurposeFromVaultPath(t *testing.T) {
	cases := []struct {
		path        string
		wantName    string
		wantPurpose string
		wantOK      bool
	}{
		{"agents/claude/oauth", "claude", "oauth", true},
		{"agents/codex/oauth", "codex", "oauth", true},
		{"agents/claude/apikey", "claude", "apikey", true}, // apikey now conforms
		{"agents//apikey", "", "", false},                  // empty name
		{"agents/claude", "", "", false},                   // missing purpose segment
		{"agents/oauth", "", "", false},                    // only two segments
		{"agents/a/b/oauth", "", "", false},                // nested name / extra segment
		{"some/other/path", "", "", false},                 // wrong prefix
		{"user/github", "", "", false},                     // user namespace, not agent
		{"agents/claude/oauth/extra", "", "", false},       // extra segment
		{"agents/claude/OAUTH", "", "", false},             // purpose fails the allow-list
	}
	for _, c := range cases {
		gotName, gotPurpose, gotOK := AgentNameAndPurposeFromVaultPath(c.path)
		if gotOK != c.wantOK || gotName != c.wantName || gotPurpose != c.wantPurpose {
			t.Errorf("AgentNameAndPurposeFromVaultPath(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.path, gotName, gotPurpose, gotOK, c.wantName, c.wantPurpose, c.wantOK)
		}
	}
}

func TestUserServiceFromVaultPath(t *testing.T) {
	cases := []struct {
		path   string
		want   string
		wantOK bool
	}{
		{"user/github", "github", true},
		{"user/git-hub_1", "git-hub_1", true},
		{"agents/claude/oauth", "", false},
		{"user/", "", false},
		{"user/has/slash", "", false},
		{"user/Bad", "", false},
		{"other/github", "", false},
	}
	for _, c := range cases {
		got, ok := UserServiceFromVaultPath(c.path)
		if ok != c.wantOK || got != c.want {
			t.Errorf("UserServiceFromVaultPath(%q) = (%q, %v), want (%q, %v)",
				c.path, got, ok, c.want, c.wantOK)
		}
	}
}
