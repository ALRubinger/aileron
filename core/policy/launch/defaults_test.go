package launch

import (
	"runtime"
	"testing"
)

func TestDefaultPolicy_HasLanguageRules(t *testing.T) {
	pf := DefaultPolicy()

	languages := map[string]string{
		"go":     "go *",
		"npm":    "npm *",
		"python": "python *",
		"cargo":  "cargo *",
		"bundle": "bundle *",
		"mix":    "mix *",
		"mvn":    "mvn *",
	}

	for lang, pattern := range languages {
		found := false
		for _, r := range pf.Allow {
			if r.Command == pattern {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected allow rule for %s (%q)", lang, pattern)
		}
	}
}

func TestDefaultPolicy_HasCommonTools(t *testing.T) {
	pf := DefaultPolicy()

	tools := []string{"git status", "ls *", "cat *", "grep *", "make *", "gh pr *"}
	for _, cmd := range tools {
		found := false
		for _, r := range pf.Allow {
			if r.Command == cmd {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected allow rule for %q", cmd)
		}
	}
}

func TestDefaultPolicy_HasUniversalDenyRules(t *testing.T) {
	pf := DefaultPolicy()

	denies := []string{"rm -rf /*", "git push origin main", "gh repo delete *"}
	for _, cmd := range denies {
		found := false
		for _, r := range pf.Deny {
			if r.Command == cmd {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected deny rule for %q", cmd)
		}
	}
}

func TestDefaultPolicy_HasOSDenyRules(t *testing.T) {
	pf := DefaultPolicy()

	switch runtime.GOOS {
	case "darwin":
		found := false
		for _, r := range pf.Deny {
			if r.Command == "security *" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected darwin Keychain deny rule")
		}
	case "linux":
		found := false
		for _, r := range pf.Deny {
			if r.Command == "systemctl *" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected linux systemctl deny rule")
		}
	}
}

func TestDefaultPolicy_DefaultDisposition(t *testing.T) {
	pf := DefaultPolicy()
	if pf.Default != "ask" {
		t.Errorf("expected default 'ask', got %q", pf.Default)
	}
}

func TestDefaultPolicy_Version(t *testing.T) {
	pf := DefaultPolicy()
	if pf.Version != 1 {
		t.Errorf("expected version 1, got %d", pf.Version)
	}
}

func TestOSDenyRules_ReturnsRules(t *testing.T) {
	rules := osDenyRules()
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if len(rules) == 0 {
			t.Error("expected OS-specific deny rules")
		}
	}
}
