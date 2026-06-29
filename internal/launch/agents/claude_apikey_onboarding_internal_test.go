package agents

import (
	"encoding/json"
	"strings"
	"testing"
)

// onboardingFields is the subset of /home/agent/.claude.json the launch
// stub must always carry to keep first-run silent. Both modes must
// satisfy these.
type onboardingFields struct {
	HasCompletedOnboarding        bool   `json:"hasCompletedOnboarding"`
	BypassPermissionsModeAccepted bool   `json:"bypassPermissionsModeAccepted"`
	InstallMethod                 string `json:"installMethod"`
	Projects                      map[string]struct {
		HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
	} `json:"projects"`
	CustomApiKeyResponses *struct {
		Approved []string `json:"approved"`
	} `json:"customApiKeyResponses"`
}

func assertCoreOnboardingFields(t *testing.T, of onboardingFields) {
	t.Helper()
	if !of.HasCompletedOnboarding {
		t.Error("hasCompletedOnboarding = false; the theme picker would paint on launch")
	}
	if !of.BypassPermissionsModeAccepted {
		t.Error("bypassPermissionsModeAccepted = false; the skip-permissions disclaimer would block")
	}
	if of.InstallMethod != "global" {
		t.Errorf("installMethod = %q, want global", of.InstallMethod)
	}
	trust, ok := of.Projects[claudeWorkspacePath]
	if !ok {
		t.Fatalf("projects missing workspace entry %q; got %v", claudeWorkspacePath, of.Projects)
	}
	if !trust.HasTrustDialogAccepted {
		t.Error("workspace hasTrustDialogAccepted = false; the trust dialog would block")
	}
}

// TestClaudeAPIKeySuffix covers the per-key approval-token derivation:
// the last claudeAPIKeySuffixLen characters of the (already-trimmed) key,
// or the whole key when it is shorter.
func TestClaudeAPIKeySuffix(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			name: "long key returns last 20 chars",
			key:  "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
			want: "VWXYZ0123456789", // recomputed below; placeholder replaced
		},
		{
			name: "exactly 20 chars returns whole key",
			key:  "abcdefghij0123456789",
			want: "abcdefghij0123456789",
		},
		{
			name: "shorter than 20 returns whole key",
			key:  "sk-short",
			want: "sk-short",
		},
		{
			name: "empty key returns empty",
			key:  "",
			want: "",
		},
	}
	// Recompute the long-key expectation from the constant so the test
	// does not hardcode a wrong slice if claudeAPIKeySuffixLen changes.
	tests[0].want = tests[0].key[len(tests[0].key)-claudeAPIKeySuffixLen:]

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeAPIKeySuffix(tc.key)
			if got != tc.want {
				t.Fatalf("claudeAPIKeySuffix(%q) = %q, want %q", tc.key, got, tc.want)
			}
			if tc.key != "" && len([]rune(got)) > claudeAPIKeySuffixLen {
				t.Fatalf("suffix longer than %d runes: %q", claudeAPIKeySuffixLen, got)
			}
		})
	}
}

// TestClaudeAPIKeyOnboardingContent_ApprovesRenderedKey is the #1695
// regression: in api-key mode the rendered .claude.json must carry
// customApiKeyResponses.approved holding the last-20-char suffix of the
// key Claude Code actually sees (the same trimmed value the EnvBinding
// rendered into ANTHROPIC_API_KEY), so Claude Code does not show the
// "Detected a custom API key in your environment" prompt.
func TestClaudeAPIKeyOnboardingContent_ApprovesRenderedKey(t *testing.T) {
	tests := []struct {
		name        string
		renderedKey string // value in env[ANTHROPIC_API_KEY] as the binding rendered it
		wantSuffix  string
	}{
		{
			name:        "long key approves last 20 chars",
			renderedKey: "sk-ant-api03-LONGKEYWITHMORETHANTWENTYCHARSXYZ",
			wantSuffix:  "REETHANTWENTYCHARSXYZ"[1:], // last 20 chars, recomputed below
		},
		{
			name:        "short key approves whole key",
			renderedKey: "sk-short-key",
			wantSuffix:  "sk-short-key",
		},
	}
	tests[0].wantSuffix = tests[0].renderedKey[len(tests[0].renderedKey)-claudeAPIKeySuffixLen:]

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{claudeAPIKeyEnv: tc.renderedKey}
			b, err := claudeAPIKeyOnboardingContent(env)
			if err != nil {
				t.Fatalf("claudeAPIKeyOnboardingContent: %v", err)
			}
			var of onboardingFields
			if err := json.Unmarshal(b, &of); err != nil {
				t.Fatalf("unmarshal rendered .claude.json: %v\n%s", err, b)
			}
			assertCoreOnboardingFields(t, of)
			if of.CustomApiKeyResponses == nil {
				t.Fatalf("customApiKeyResponses absent; the custom-API-key prompt would block. doc: %s", b)
			}
			got := of.CustomApiKeyResponses.Approved
			if len(got) != 1 || got[0] != tc.wantSuffix {
				t.Fatalf("approved = %v, want [%q]", got, tc.wantSuffix)
			}
		})
	}
}

// TestClaudeAPIKeyOnboardingContent_TrimsBeforeSuffix proves the approval
// token is derived from the trimmed key. claudeAPIKeyRender trims
// whitespace before rendering into ANTHROPIC_API_KEY, so the suffix the
// onboarding doc records must match the trimmed value Claude Code sees,
// not the raw env string with surrounding whitespace.
func TestClaudeAPIKeyOnboardingContent_TrimsBeforeSuffix(t *testing.T) {
	// A key whose meaningful suffix is followed by a trailing newline.
	rawKey := "sk-ant-api03-TRAILINGWHITESPACEKEY1234"
	env := map[string]string{claudeAPIKeyEnv: "  " + rawKey + "\n"}

	b, err := claudeAPIKeyOnboardingContent(env)
	if err != nil {
		t.Fatalf("claudeAPIKeyOnboardingContent: %v", err)
	}
	var of onboardingFields
	if err := json.Unmarshal(b, &of); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if of.CustomApiKeyResponses == nil {
		t.Fatalf("customApiKeyResponses absent")
	}
	want := rawKey[len(rawKey)-claudeAPIKeySuffixLen:]
	if got := of.CustomApiKeyResponses.Approved; len(got) != 1 || got[0] != want {
		t.Fatalf("approved = %v, want [%q] (suffix of the trimmed key, not the whitespace-padded env)", got, want)
	}
	// The suffix must never contain the leading/trailing whitespace.
	if strings.ContainsAny(of.CustomApiKeyResponses.Approved[0], " \n\t") {
		t.Fatalf("approval token carries whitespace: %q", of.CustomApiKeyResponses.Approved[0])
	}
}

// TestClaudeAPIKeyOnboardingContent_NoKeyOmitsApproval covers the
// fall-back case: the api-key acquire missed and no ANTHROPIC_API_KEY was
// rendered into the env. With no env key there is nothing for Claude Code
// to prompt about, so the doc must carry the onboarding fields but no
// customApiKeyResponses key (byte-identical to the plain stub).
func TestClaudeAPIKeyOnboardingContent_NoKeyOmitsApproval(t *testing.T) {
	for _, env := range []map[string]string{
		nil,
		{},
		{claudeAPIKeyEnv: ""},
		{claudeAPIKeyEnv: "   \n"},
	} {
		b, err := claudeAPIKeyOnboardingContent(env)
		if err != nil {
			t.Fatalf("claudeAPIKeyOnboardingContent(%v): %v", env, err)
		}
		var of onboardingFields
		if err := json.Unmarshal(b, &of); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		assertCoreOnboardingFields(t, of)
		if of.CustomApiKeyResponses != nil {
			t.Errorf("env %v: customApiKeyResponses present with no rendered key: %v", env, of.CustomApiKeyResponses)
		}
		// No rendered key ⇒ the doc must be byte-identical to the plain
		// onboarding stub.
		if string(b) != string(claudeOnboardingStub) {
			t.Errorf("env %v: doc not byte-identical to plain stub.\n got: %s\nwant: %s", env, b, claudeOnboardingStub)
		}
	}
}

// TestClaudeOnboardingStub_NoCustomApiKeyResponses pins the subscription
// stub's byte shape: it must not carry customApiKeyResponses at all (no
// env key, no prompt). This is the regression guard that the new pointer
// field stays omitted via omitempty in the no-approval path.
func TestClaudeOnboardingStub_NoCustomApiKeyResponses(t *testing.T) {
	if strings.Contains(string(claudeOnboardingStub), "customApiKeyResponses") {
		t.Fatalf("subscription onboarding stub leaked customApiKeyResponses: %s", claudeOnboardingStub)
	}
	var of onboardingFields
	if err := json.Unmarshal(claudeOnboardingStub, &of); err != nil {
		t.Fatalf("unmarshal stub: %v", err)
	}
	assertCoreOnboardingFields(t, of)
	if of.CustomApiKeyResponses != nil {
		t.Fatalf("subscription stub has customApiKeyResponses: %v", of.CustomApiKeyResponses)
	}
}
