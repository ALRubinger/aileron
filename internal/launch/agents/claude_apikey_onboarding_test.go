package agents_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

// onboardingStaticFile returns the /home/agent/.claude.json onboarding
// StaticFile from an AuthSpec. Api-key mode also ships a
// /home/agent/.claude/ writable-dir sentinel (#1700), so callers must
// select the onboarding file by path rather than assuming it is the only
// StaticFile.
func onboardingStaticFile(t *testing.T, spec launch.AuthSpec) launch.StaticFile {
	t.Helper()
	for _, sf := range spec.StaticFiles {
		if sf.ContainerPath == "/home/agent/.claude.json" {
			return sf
		}
	}
	t.Fatalf("no /home/agent/.claude.json StaticFile in spec; got %+v", spec.StaticFiles)
	return launch.StaticFile{}
}

// claudeOnboardingShape mirrors the contractual fields of
// /home/agent/.claude.json for the AuthSpec-level wiring tests.
type claudeOnboardingShape struct {
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

func assertSharedOnboardingFields(t *testing.T, b []byte) claudeOnboardingShape {
	t.Helper()
	var s claudeOnboardingShape
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("unmarshal .claude.json: %v\n%s", err, b)
	}
	if !s.HasCompletedOnboarding {
		t.Error("hasCompletedOnboarding = false")
	}
	if !s.BypassPermissionsModeAccepted {
		t.Error("bypassPermissionsModeAccepted = false")
	}
	if s.InstallMethod != "global" {
		t.Errorf("installMethod = %q, want global", s.InstallMethod)
	}
	if len(s.Projects) == 0 {
		t.Error("projects empty; the trust dialog would block")
	}
	for _, p := range s.Projects {
		if !p.HasTrustDialogAccepted {
			t.Error("a project entry has hasTrustDialogAccepted = false")
		}
	}
	return s
}

// TestClaude_APIKeyAuthSpec_OnboardingIsKeyAware proves the api-key
// AuthSpec wires the key-aware onboarding hook (RenderContent) onto the
// .claude.json StaticFile and that invoking it with a rendered
// ANTHROPIC_API_KEY yields a doc carrying the key's last-20-char suffix
// under customApiKeyResponses.approved. This is the #1695 acceptance path
// at the AuthSpec layer (the launch runtime invokes RenderContent with
// the rendered env additions).
func TestClaude_APIKeyAuthSpec_OnboardingIsKeyAware(t *testing.T) {
	spec := agents.NewClaude(agents.ClaudeAuthModeAPIKey).AuthSpec()
	sf := onboardingStaticFile(t, spec)
	if sf.RenderContent == nil {
		t.Fatal("api-key StaticFile must set RenderContent so the onboarding doc is key-aware")
	}

	key := "sk-ant-api03-THISISALONGTESTKEYABCDEFG"
	b, err := sf.RenderContent(map[string]string{"ANTHROPIC_API_KEY": key})
	if err != nil {
		t.Fatalf("RenderContent: %v", err)
	}
	s := assertSharedOnboardingFields(t, b)
	if s.CustomApiKeyResponses == nil {
		t.Fatalf("customApiKeyResponses absent; the api-key prompt would block. doc: %s", b)
	}
	want := key[len(key)-20:]
	if got := s.CustomApiKeyResponses.Approved; len(got) != 1 || got[0] != want {
		t.Fatalf("approved = %v, want [%q]", got, want)
	}
}

// TestClaude_SubscriptionAuthSpec_OnboardingByteIdentical proves
// subscription mode is unaffected: the StaticFile sets no RenderContent
// hook and its Content is byte-identical to the api-key mode's Content
// fallback (the plain onboarding stub) and carries no
// customApiKeyResponses field.
func TestClaude_SubscriptionAuthSpec_OnboardingByteIdentical(t *testing.T) {
	sub := agents.NewClaude(agents.ClaudeAuthModeSubscription).AuthSpec()
	if len(sub.StaticFiles) != 1 {
		t.Fatalf("subscription StaticFiles = %d, want 1", len(sub.StaticFiles))
	}
	subSF := sub.StaticFiles[0]
	if subSF.RenderContent != nil {
		t.Error("subscription StaticFile must NOT set RenderContent (no env key, no prompt)")
	}
	if strings.Contains(string(subSF.Content), "customApiKeyResponses") {
		t.Errorf("subscription .claude.json leaked customApiKeyResponses: %s", subSF.Content)
	}
	s := assertSharedOnboardingFields(t, subSF.Content)
	if s.CustomApiKeyResponses != nil {
		t.Errorf("subscription .claude.json carries customApiKeyResponses: %v", s.CustomApiKeyResponses)
	}

	// The api-key mode's Content fallback (used only if RenderContent were
	// ever nil) is the same plain stub, so a regression dropping the hook
	// degrades to the pre-#1695 subscription bytes rather than an empty
	// file.
	apiSF := onboardingStaticFile(t, agents.NewClaude(agents.ClaudeAuthModeAPIKey).AuthSpec())
	if string(apiSF.Content) != string(subSF.Content) {
		t.Errorf("api-key Content fallback differs from subscription stub.\n api: %s\n sub: %s", apiSF.Content, subSF.Content)
	}
}
