package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/vault"
)

// GET /v1/vault (union view) contract (#1402):
//
//   - Walks vault.List() once and surfaces EVERY locally-owned namespace
//     (agent, user, connector/capability-binding), not just the two typed
//     scopes — connector credentials must no longer be silently hidden.
//   - Classifies each entry by namespace via classifyVaultPath.
//   - Excludes the two control-plane namespaces (connected-accounts/,
//     llm-config/) by default; includes them only with
//     include_control_plane=true.
//   - Never returns credential bytes (ADR-0011).
//   - 503 when the daemon has no vault wired.

// classifyVaultPath is the load-bearing classifier; pin each namespace and
// the ordering edge case where an agent path also matches the binding
// grammar.
func TestClassifyVaultPath(t *testing.T) {
	cases := []struct {
		path        string
		wantScope   string
		wantControl bool
	}{
		{"agents/claude/oauth", vaultScopeAgent, false},
		{"agents/codex/apikey", vaultScopeAgent, false},
		{"user/github", vaultScopeUser, false},
		// Connector + OAuth capability bindings are three-segment binding
		// names. They are exactly the entries the old typed-endpoint list
		// silently hid (#1402); the union must classify them as bindings.
		{"connectors/github/default", vaultScopeBinding, false},
		{"oauth2/google/default", vaultScopeBinding, false},
		{"github/repo/me", vaultScopeBinding, false},
		// Control-plane namespaces: classified but flagged for exclusion.
		{"connected-accounts/usr_123/slack", vaultScopeConnectedAccount, true},
		{"llm-config/user/usr_123", vaultScopeLlmConfig, true},
		// Anything matching no known namespace must still surface as `other`
		// rather than vanish.
		{"weird-single-segment", vaultScopeOther, false},
		{"_internal/reserved", vaultScopeOther, false},
	}
	for _, c := range cases {
		scope, control := classifyVaultPath(c.path)
		if scope != c.wantScope || control != c.wantControl {
			t.Errorf("classifyVaultPath(%q) = (%q, %v), want (%q, %v)",
				c.path, scope, control, c.wantScope, c.wantControl)
		}
	}
}

// The default union view must surface agent, user, AND connector/binding
// entries together, and must omit the control-plane namespaces.
func TestListVaultEntries_UnionDefaultExcludesControlPlane(t *testing.T) {
	v := vault.NewMemVault()
	seed := map[string]string{
		"agents/claude/oauth":            "AGENTSECRET",
		"user/github":                    "USERSECRET",
		"connectors/github/default":      "CONNSECRET",
		"oauth2/google/default":          "OAUTHSECRET",
		"connected-accounts/usr_1/slack": "CPSECRET1",
		"llm-config/user/usr_1":          "CPSECRET2",
	}
	for p, val := range seed {
		if err := v.Put(context.Background(), p, []byte(val), vault.Metadata{Type: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	s := &apiServer{log: slog.Default(), vault: v}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault", nil)
	s.ListVaultEntries(rec, req, api.ListVaultEntriesParams{})
	assertStatus(t, rec, http.StatusOK)

	// No credential bytes may leak (ADR-0011).
	for _, secret := range seed {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("union list leaked credential material %q: %s", secret, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), `"value"`) {
		t.Fatalf("union list body carried a value field: %s", rec.Body.String())
	}

	var got api.VaultEntriesList
	mustDecode(t, rec.Body, &got)
	byPath := map[string]string{}
	for _, e := range got.Entries {
		byPath[e.Path] = string(e.Scope)
	}
	// The locally-owned namespaces must all be present and correctly scoped.
	want := map[string]string{
		"agents/claude/oauth":       vaultScopeAgent,
		"user/github":               vaultScopeUser,
		"connectors/github/default": vaultScopeBinding,
		"oauth2/google/default":     vaultScopeBinding,
	}
	for p, scope := range want {
		if byPath[p] != scope {
			t.Errorf("entry %q scope = %q, want %q (present: %v)", p, byPath[p], scope, byPath)
		}
	}
	// The control-plane namespaces must be excluded by default.
	if _, ok := byPath["connected-accounts/usr_1/slack"]; ok {
		t.Errorf("connected-accounts entry leaked into default union view")
	}
	if _, ok := byPath["llm-config/user/usr_1"]; ok {
		t.Errorf("llm-config entry leaked into default union view")
	}
}

// include_control_plane=true must add the two control-plane namespaces.
func TestListVaultEntries_IncludeControlPlane(t *testing.T) {
	v := vault.NewMemVault()
	for _, p := range []string{"agents/claude/oauth", "connected-accounts/usr_1/slack", "llm-config/user/usr_1"} {
		if err := v.Put(context.Background(), p, []byte("x"), vault.Metadata{}); err != nil {
			t.Fatal(err)
		}
	}
	s := &apiServer{log: slog.Default(), vault: v}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault?include_control_plane=true", nil)
	include := true
	s.ListVaultEntries(rec, req, api.ListVaultEntriesParams{IncludeControlPlane: &include})
	assertStatus(t, rec, http.StatusOK)

	var got api.VaultEntriesList
	mustDecode(t, rec.Body, &got)
	scopes := map[string]bool{}
	for _, e := range got.Entries {
		scopes[string(e.Scope)] = true
	}
	for _, want := range []string{vaultScopeAgent, vaultScopeConnectedAccount, vaultScopeLlmConfig} {
		if !scopes[want] {
			t.Errorf("include-control-plane view missing scope %q (got %v)", want, scopes)
		}
	}
}

// Listing must work on a locked vault: List never decrypts a value, so the
// union view surfaces metadata even when the vault is locked (ADR-0011).
func TestListVaultEntries_WorksOnLockedVault(t *testing.T) {
	inner := vault.NewMemVault()
	if err := inner.Put(context.Background(), "user/github", []byte("SECRET"), vault.Metadata{Type: "api_key"}); err != nil {
		t.Fatal(err)
	}
	// FileVault/EncryptedVault keep metadata in plaintext; MemVault models
	// that property for the handler, which only ever calls List().
	s := &apiServer{log: slog.Default(), vault: inner}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault", nil)
	s.ListVaultEntries(rec, req, api.ListVaultEntriesParams{})
	assertStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("locked-vault list leaked credential bytes: %s", rec.Body.String())
	}
	var got api.VaultEntriesList
	mustDecode(t, rec.Body, &got)
	if len(got.Entries) != 1 || got.Entries[0].Path != "user/github" {
		t.Fatalf("entries = %+v, want one user/github entry", got.Entries)
	}
	if got.Entries[0].Metadata == nil || got.Entries[0].Metadata.Type == nil || *got.Entries[0].Metadata.Type != "api_key" {
		t.Errorf("metadata = %+v, want plaintext type api_key", got.Entries[0].Metadata)
	}
}

func TestListVaultEntries_EmptyVaultReturnsEmptyArray(t *testing.T) {
	s := &apiServer{log: slog.Default(), vault: vault.NewMemVault()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault", nil)
	s.ListVaultEntries(rec, req, api.ListVaultEntriesParams{})
	assertStatus(t, rec, http.StatusOK)
	var got api.VaultEntriesList
	mustDecode(t, rec.Body, &got)
	if got.Entries == nil {
		t.Fatal("entries must be a non-nil (empty) array")
	}
	if len(got.Entries) != 0 {
		t.Errorf("entries = %v, want empty", got.Entries)
	}
}

func TestListVaultEntries_NoVaultReturns503(t *testing.T) {
	s := &apiServer{log: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault", nil)
	s.ListVaultEntries(rec, req, api.ListVaultEntriesParams{})
	assertStatus(t, rec, http.StatusServiceUnavailable)
	assertErrorCode(t, rec, "no_local_vault")
}
