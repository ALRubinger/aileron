package cli_test

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/auth/capture"
	"github.com/ALRubinger/aileron/internal/cli"
)

// ghUnitYAML is the umbrella's gh unit expressed in the customizations.aileron
// .cli wire shape. Its acquisition block mirrors
// internal/auth/capture/defaults/gh.yaml and its sealing block mirrors
// internal/proxybinding/defaults/github.yaml, so a successful parse proves the
// unit type can express both end-to-end targets.
const ghUnitYAML = `
name: gh
key: user/github
presence:
  builtin: base
acquisition:
  mode: device-flow
  container_name: aileron-auth-github
  login_cmd: [gh, auth, login, --hostname, github.com, --git-protocol, https, --web]
  token_cmd: [gh, auth, token, --hostname, github.com]
  browser_shim: echo
sealing:
  - host: github.com
    scheme: basic
    emit_mechanism: inject
    username: x-access-token
  - host: api.github.com
    scheme: bearer
    emit_mechanism: sentinel-swap
    sentinel:
      value: ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA
      env: GH_TOKEN
state: {}
`

func TestParseGhHappyPath(t *testing.T) {
	u, err := cli.Parse([]byte(ghUnitYAML))
	if err != nil {
		t.Fatalf("Parse() returned error: %v", err)
	}
	if u.Name != "gh" {
		t.Errorf("Name = %q, want %q", u.Name, "gh")
	}
	if u.Key != "user/github" {
		t.Errorf("Key = %q, want %q", u.Key, "user/github")
	}
}

func TestGhAcquisitionExpressibility(t *testing.T) {
	u, err := cli.Parse([]byte(ghUnitYAML))
	if err != nil {
		t.Fatalf("Parse() returned error: %v", err)
	}

	desc, err := u.ToCaptureDescriptor()
	if err != nil {
		t.Fatalf("ToCaptureDescriptor() returned error: %v", err)
	}

	// The descriptor must equal the values in the gh capture default,
	// including the key-derived store_at and kind.
	if desc.Version != capture.CaptureSchemaVersion {
		t.Errorf("Version = %q, want %q", desc.Version, capture.CaptureSchemaVersion)
	}
	if desc.Name != "gh" {
		t.Errorf("Name = %q, want %q", desc.Name, "gh")
	}
	if desc.ContainerName != "aileron-auth-github" {
		t.Errorf("ContainerName = %q, want %q", desc.ContainerName, "aileron-auth-github")
	}
	wantLogin := []string{"gh", "auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web"}
	if !equalStrings(desc.LoginCmd, wantLogin) {
		t.Errorf("LoginCmd = %v, want %v", desc.LoginCmd, wantLogin)
	}
	wantToken := []string{"gh", "auth", "token", "--hostname", "github.com"}
	if !equalStrings(desc.TokenCmd, wantToken) {
		t.Errorf("TokenCmd = %v, want %v", desc.TokenCmd, wantToken)
	}
	if desc.BrowserShim != "echo" {
		t.Errorf("BrowserShim = %q, want %q", desc.BrowserShim, "echo")
	}
	if desc.ConfigDir != "" {
		t.Errorf("ConfigDir = %q, want empty (gh omits it)", desc.ConfigDir)
	}
	if desc.StoreAt != "user/github" {
		t.Errorf("StoreAt = %q, want %q", desc.StoreAt, "user/github")
	}
	if desc.Kind != "user" {
		t.Errorf("Kind = %q, want %q", desc.Kind, "user")
	}

	// The converted descriptor passes capture's canonical validator.
	if err := desc.Validate(); err != nil {
		t.Errorf("capture Validate() returned error: %v", err)
	}
}

func TestGhSealingExpressibility(t *testing.T) {
	u, err := cli.Parse([]byte(ghUnitYAML))
	if err != nil {
		t.Fatalf("Parse() returned error: %v", err)
	}

	entries, err := u.ToSealingEntries()
	if err != nil {
		t.Fatalf("ToSealingEntries() returned error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}

	// github.com -> basic, inject, username x-access-token.
	e0 := entries[0]
	if e0.Host != "github.com" || e0.Scheme != "basic" || e0.EmitMechanism != "inject" || e0.Username != "x-access-token" {
		t.Errorf("entry[0] = %+v, want github.com basic/inject/x-access-token", e0)
	}
	if e0.CredentialRef != "user/github" {
		t.Errorf("entry[0].CredentialRef = %q, want %q", e0.CredentialRef, "user/github")
	}

	// api.github.com -> bearer, sentinel-swap, sentinel value+env.
	e1 := entries[1]
	if e1.Host != "api.github.com" || e1.Scheme != "bearer" || e1.EmitMechanism != "sentinel-swap" {
		t.Errorf("entry[1] = %+v, want api.github.com bearer/sentinel-swap", e1)
	}
	if e1.Sentinel == nil || e1.Sentinel.Value != "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA" || e1.Sentinel.Env != "GH_TOKEN" {
		t.Errorf("entry[1].Sentinel = %+v, want value+GH_TOKEN", e1.Sentinel)
	}
	if e1.CredentialRef != "user/github" {
		t.Errorf("entry[1].CredentialRef = %q, want %q", e1.CredentialRef, "user/github")
	}

	// Both entries pass proxybinding's canonical validator and adapt to a
	// HostBinding.
	for i := range entries {
		if err := entries[i].Validate(); err != nil {
			t.Errorf("entry[%d] Validate() returned error: %v", i, err)
		}
		if _, err := entries[i].ToHostBinding(); err != nil {
			t.Errorf("entry[%d] ToHostBinding() returned error: %v", i, err)
		}
	}
}

func TestKeyDerivationConflictRejected(t *testing.T) {
	// Each derived field, when re-declared, fails with an error naming the
	// field. This is the brief's named regression test for single-source key
	// derivation.
	cases := map[string]string{
		"store_at": `
name: gh
key: user/github
store_at: user/github
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
`,
		"credential_ref": `
name: gh
key: user/github
credential_ref: user/github
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
`,
		"kind": `
name: gh
key: user/github
kind: user
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
`,
	}
	for field, doc := range cases {
		t.Run(field, func(t *testing.T) {
			_, err := cli.Parse([]byte(doc))
			if err == nil {
				t.Fatalf("Parse() succeeded, want error naming %q", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("error %q does not name field %q", err.Error(), field)
			}
		})
	}
}

func TestSealingCredentialRefRedeclarationRejected(t *testing.T) {
	doc := `
name: gh
key: user/github
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
sealing:
  - host: github.com
    scheme: basic
    username: x-access-token
    credential_ref: user/github
`
	_, err := cli.Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse() succeeded, want error rejecting a sealing credential_ref re-declaration")
	}
	if !strings.Contains(err.Error(), "credential_ref") {
		t.Errorf("error %q does not name credential_ref", err.Error())
	}
}

func TestMalformedKeyRejected(t *testing.T) {
	cases := map[string]string{
		"empty key": `
name: gh
key: ""
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
`,
		"no slash": `
name: gh
key: github
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
`,
		"empty first segment": `
name: gh
key: /github
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := cli.Parse([]byte(doc)); err == nil {
				t.Fatalf("Parse() succeeded, want error for %s", name)
			}
		})
	}
}

func TestMissingNameRejected(t *testing.T) {
	doc := `
key: user/github
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
`
	_, err := cli.Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse() succeeded, want error for missing name")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error %q does not name the missing field", err.Error())
	}
}

func TestReservedDirectTokenRejected(t *testing.T) {
	doc := `
name: gh
key: user/github
acquisition:
  mode: direct-token
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
`
	_, err := cli.Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse() succeeded, want not-yet-implemented error for direct-token")
	}
	if !strings.Contains(err.Error(), "direct-token") || !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("error %q does not explain direct-token is not yet implemented", err.Error())
	}
}

func TestReservedNonEmptyStateRejected(t *testing.T) {
	doc := `
name: gh
key: user/github
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
state:
  cache: /some/path
`
	_, err := cli.Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse() succeeded, want not-yet-implemented error for non-empty state")
	}
	if !strings.Contains(err.Error(), "state") || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("error %q does not explain state is reserved", err.Error())
	}
}

func TestStateAbsentAndEmptyAreValid(t *testing.T) {
	absent := `
name: gh
key: user/github
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
`
	empty := absent + "state: {}\n"
	for name, doc := range map[string]string{"absent": absent, "empty": empty} {
		t.Run(name, func(t *testing.T) {
			if _, err := cli.Parse([]byte(doc)); err != nil {
				t.Errorf("Parse() returned error for %s state: %v", name, err)
			}
		})
	}
}

func TestDelegatedSealingValidationSurfaces(t *testing.T) {
	bogusScheme := `
name: gh
key: user/github
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
sealing:
  - host: github.com
    scheme: bogus
`
	sentinelSwapNoSentinel := `
name: gh
key: user/github
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
sealing:
  - host: api.github.com
    scheme: bearer
    emit_mechanism: sentinel-swap
`
	for name, doc := range map[string]string{
		"bogus scheme":            bogusScheme,
		"sentinel-swap, no block": sentinelSwapNoSentinel,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cli.Parse([]byte(doc)); err == nil {
				t.Fatalf("Parse() succeeded, want delegated proxybinding error for %s", name)
			}
		})
	}
}

func TestDelegatedAcquisitionValidationSurfaces(t *testing.T) {
	// A missing login_cmd proves delegation to capture's validator: this
	// package never re-implements the required-login_cmd rule.
	doc := `
name: gh
key: user/github
acquisition:
  mode: device-flow
  container_name: c
  token_cmd: [gh]
`
	_, err := cli.Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse() succeeded, want delegated capture error for missing login_cmd")
	}
	if !strings.Contains(err.Error(), "login_cmd") {
		t.Errorf("error %q does not name login_cmd", err.Error())
	}
}

func TestStrictDecodeRejectsUnknownKey(t *testing.T) {
	doc := `
name: gh
key: user/github
bogus_field: oops
acquisition:
  mode: device-flow
  container_name: c
  login_cmd: [gh]
  token_cmd: [gh]
`
	if _, err := cli.Parse([]byte(doc)); err == nil {
		t.Fatal("Parse() succeeded, want strict-decode error for unknown key")
	}
}

func TestParseMalformedYAML(t *testing.T) {
	if _, err := cli.Parse([]byte("name: [unterminated")); err == nil {
		t.Fatal("Parse() succeeded, want error for malformed YAML")
	}
}

// TestAdaptersGuardMalformedKey proves the public adapters refuse a malformed
// key independently of Validate, so a caller cannot derive a half-formed
// descriptor or entry.
func TestAdaptersGuardMalformedKey(t *testing.T) {
	u := cli.Unit{Name: "gh", Key: "github"}
	if _, err := u.ToCaptureDescriptor(); err == nil {
		t.Error("ToCaptureDescriptor() succeeded on malformed key, want error")
	}
	if _, err := u.ToSealingEntries(); err == nil {
		t.Error("ToSealingEntries() succeeded on malformed key, want error")
	}

	empty := cli.Unit{Name: "gh"}
	if _, err := empty.ToCaptureDescriptor(); err == nil {
		t.Error("ToCaptureDescriptor() succeeded on empty key, want error")
	}
	if _, err := empty.ToSealingEntries(); err == nil {
		t.Error("ToSealingEntries() succeeded on empty key, want error")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
