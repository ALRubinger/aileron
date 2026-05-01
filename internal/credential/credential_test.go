package credential_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/vault"
)

// VaultResolver contract:
//   - Existing entry, matching kind: returns Credential with the
//     exact bytes and the declared kind.
//   - Missing entry: ErrBindingMissing wrapped with the vault path.
//   - Wrong kind: ErrCredentialKindMismatch.
//   - Nil vault / nil receiver: ErrNoBindingResolver (programming
//     error path; surfaces as binding_required in the gateway).

func TestVaultResolver_Resolve_HappyPath(t *testing.T) {
	v := vault.NewMemVault()
	if err := v.Put(context.Background(), "oauth2/slack/work",
		[]byte("xoxb-test"), vault.Metadata{Type: "oauth2"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	r := &credential.VaultResolver{
		Vault:        v,
		VaultPath:    "oauth2/slack/work",
		ExpectedKind: "oauth2",
	}
	got, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Kind != "oauth2" {
		t.Errorf("Kind = %q, want oauth2", got.Kind)
	}
	if string(got.Value) != "xoxb-test" {
		t.Errorf("Value = %q, want xoxb-test", got.Value)
	}
}

func TestVaultResolver_Resolve_MissingBinding(t *testing.T) {
	r := &credential.VaultResolver{
		Vault:        vault.NewMemVault(),
		VaultPath:    "oauth2/slack/work",
		ExpectedKind: "oauth2",
	}
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrBindingMissing) {
		t.Errorf("err = %v, want ErrBindingMissing", err)
	}
	// The error message names the path so audit context is preserved.
	if err == nil || !contains(err.Error(), "oauth2/slack/work") {
		t.Errorf("err = %v, want path in message", err)
	}
}

func TestVaultResolver_Resolve_KindMismatch(t *testing.T) {
	v := vault.NewMemVault()
	if err := v.Put(context.Background(), "oauth2/slack/work",
		[]byte("xoxb-test"), vault.Metadata{Type: "api_key"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	r := &credential.VaultResolver{
		Vault:        v,
		VaultPath:    "oauth2/slack/work",
		ExpectedKind: "oauth2",
	}
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrCredentialKindMismatch) {
		t.Errorf("err = %v, want ErrCredentialKindMismatch", err)
	}
	if err == nil || !contains(err.Error(), "oauth2") || !contains(err.Error(), "api_key") {
		t.Errorf("err = %v, want both kinds in message", err)
	}
}

func TestVaultResolver_Resolve_NilReceiver(t *testing.T) {
	var r *credential.VaultResolver
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrNoBindingResolver) {
		t.Errorf("err = %v, want ErrNoBindingResolver", err)
	}
}

func TestVaultResolver_Resolve_NilVault(t *testing.T) {
	r := &credential.VaultResolver{}
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrNoBindingResolver) {
		t.Errorf("err = %v, want ErrNoBindingResolver", err)
	}
}

// stubErroringVault returns a non-NotFound error from Get; resolver
// should propagate it instead of treating it as a missing binding.
type stubErroringVault struct{}

func (stubErroringVault) Get(_ context.Context, _ string) (vault.Secret, error) {
	return vault.Secret{}, errors.New("vault read failed")
}
func (stubErroringVault) Put(_ context.Context, _ string, _ []byte, _ vault.Metadata) error {
	return nil
}
func (stubErroringVault) Delete(_ context.Context, _ string) error { return nil }
func (stubErroringVault) List(_ context.Context) ([]vault.Entry, error) {
	return nil, nil
}

func TestVaultResolver_Resolve_GenericVaultErrorPropagates(t *testing.T) {
	r := &credential.VaultResolver{
		Vault:        stubErroringVault{},
		VaultPath:    "irrelevant",
		ExpectedKind: "oauth2",
	}
	_, err := r.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, credential.ErrBindingMissing) {
		t.Errorf("generic errors must not be reported as ErrBindingMissing: %v", err)
	}
	if !contains(err.Error(), "vault read failed") {
		t.Errorf("err = %v, want underlying message", err)
	}
}

// FormatBindingMissing / FormatKindMismatch produce wrap-able errors.

func TestFormatBindingMissing_WrapsSentinel(t *testing.T) {
	err := credential.FormatBindingMissing("oauth2/x")
	if !errors.Is(err, credential.ErrBindingMissing) {
		t.Errorf("err = %v, not Is ErrBindingMissing", err)
	}
}

func TestFormatKindMismatch_WrapsSentinel(t *testing.T) {
	err := credential.FormatKindMismatch("oauth2", "api_key")
	if !errors.Is(err, credential.ErrCredentialKindMismatch) {
		t.Errorf("err = %v, not Is ErrCredentialKindMismatch", err)
	}
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
