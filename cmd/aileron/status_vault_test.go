package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

// showStatusVault renders the "Vault" section of `aileron status`. Its
// contract (mirroring `secret list` / runSecretList) is that the
// "Secrets:" count and bullet list reflect ONLY `secret/`-scoped entries,
// classified by the shared vault-scope classifier. Agent credentials
// (agents/<name>/...) and control-plane / capability bindings
// (oauth2/<...>) are stored in the same vault file but are not secrets,
// so they must not leak into the count or the list.
//
// Regression for cw-review-residual #1717: showStatusVault previously
// called the unfiltered fv.Names(), miscounting and listing every stored
// path as a "Secret".
func TestShowStatusVault_CountsAndListsSecretsOnly(t *testing.T) {
	home := setTestHome(t)
	vaultPath := filepath.Join(home, ".aileron", "secrets.json")
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}

	// Populate a mixed vault: one real secret, one agent credential, one
	// capability binding. Only the secret should surface in the status view.
	fv, err := vault.NewFileVault(vaultPath)
	if err != nil {
		t.Fatalf("NewFileVault: %v", err)
	}
	ctx := context.Background()
	for _, path := range []string{
		"secret/linear_token",
		"agents/claude/oauth",
		"oauth2/google/default",
	} {
		if err := fv.Put(ctx, path, []byte("v"), vault.Metadata{}); err != nil {
			t.Fatalf("Put(%q): %v", path, err)
		}
	}

	// Sanity: DefaultVaultPath resolves under the temp HOME so the real
	// ~/.aileron is never touched.
	if got := launch.DefaultVaultPath(); got != vaultPath {
		t.Fatalf("DefaultVaultPath() = %q, want %q", got, vaultPath)
	}

	var out bytes.Buffer
	showStatusVault(home, &out)
	got := out.String()

	if !strings.Contains(got, "Secrets:    1 stored") {
		t.Errorf("status view = %q; want 'Secrets:    1 stored' (only the secret/ entry)", got)
	}
	if !strings.Contains(got, "- secret/linear_token") {
		t.Errorf("status view = %q; want the secret listed", got)
	}
	if strings.Contains(got, "agents/claude/oauth") {
		t.Errorf("status view = %q; agent credential leaked into Secrets list", got)
	}
	if strings.Contains(got, "oauth2/google/default") {
		t.Errorf("status view = %q; capability binding leaked into Secrets list", got)
	}
}
