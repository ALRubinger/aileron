package launch

import (
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

// resolveVaultPromptMode contract (#429):
//
//   - AILERON_VAULT_PROMPT=webapp + vault file present → webapp mode.
//   - AILERON_VAULT_PROMPT=webapp + vault file missing → error
//     (the user explicitly opted into webapp but there's no vault yet).
//   - AILERON_VAULT_PROMPT=stderr → stderr mode (default for the
//     legacy on-tty prompt; ungated by vault-file presence).
//   - Empty env + vault file present + TTY → webapp mode (the new
//     default under launch).
//   - Empty env + vault file missing + TTY → stderr mode (creation
//     flow falls through to EnsureVault, which prompts twice).
//   - Non-TTY contexts always pick stderr mode and return a nil
//     vault (the lazy on-demand path takes over).
//   - Invalid override values return an error.

func TestResolveVaultPromptMode_WebappOverrideWithVault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mode, v, err := resolveVaultPromptMode(path, "webapp", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if mode != vaultPromptModeWebapp {
		t.Errorf("mode = %v, want webapp", mode)
	}
	if v != nil {
		t.Errorf("vault = %T, want nil", v)
	}
}

func TestResolveVaultPromptMode_WebappOverrideWithoutVaultErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	_, _, err := resolveVaultPromptMode(path, "webapp", true)
	if err == nil {
		t.Fatal("want error when AILERON_VAULT_PROMPT=webapp and no vault file exists")
	}
}

func TestResolveVaultPromptMode_StderrOverrideNonTTY(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mode, v, err := resolveVaultPromptMode(path, "stderr", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if mode != vaultPromptModeStderr {
		t.Errorf("mode = %v, want stderr", mode)
	}
	if v != nil {
		t.Errorf("vault = %T, want nil under non-TTY (lazy path)", v)
	}
}

func TestResolveVaultPromptMode_DefaultWithVaultFilePicksWebapp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mode, v, err := resolveVaultPromptMode(path, "", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if mode != vaultPromptModeWebapp {
		t.Errorf("mode = %v, want webapp (default with vault file present)", mode)
	}
	if v != nil {
		t.Errorf("vault = %T, want nil", v)
	}
}

func TestResolveVaultPromptMode_NonTTYAlwaysStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Even with a vault file present and no override, non-TTY
	// callers (CI, headless) drop through to the lazy path.
	mode, v, err := resolveVaultPromptMode(path, "", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if mode != vaultPromptModeStderr {
		t.Errorf("mode = %v, want stderr under non-TTY", mode)
	}
	if v != nil {
		t.Errorf("vault = %T, want nil under non-TTY (lazy path)", v)
	}
}

func TestResolveVaultPromptMode_InvalidOverrideErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, _, err := resolveVaultPromptMode(path, "browser", true)
	if err == nil {
		t.Fatal("want error on unrecognised AILERON_VAULT_PROMPT value")
	}
}

func TestVaultPathForGateway_WebappReturnsCanonicalPath(t *testing.T) {
	got := vaultPathForGateway(vaultPromptModeWebapp)
	if got == "" {
		t.Errorf("vaultPathForGateway(webapp) = empty, want canonical path")
	}
}

func TestVaultPathForGateway_StderrReturnsEmpty(t *testing.T) {
	if got := vaultPathForGateway(vaultPromptModeStderr); got != "" {
		t.Errorf("vaultPathForGateway(stderr) = %q, want empty", got)
	}
}
