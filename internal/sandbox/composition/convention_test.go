package composition

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/credential/inject"
)

// TestParseCredentialConvention_Valid exercises the happy paths: a single-
// placeholder bearer convention and a two-placeholder sigv4-resign convention,
// asserting the typed result matches the declared bytes exactly.
func TestParseCredentialConvention_Valid(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     CredentialConvention
	}{
		{
			name: "bearer single placeholder",
			manifest: `{
				"id": "gh",
				"customizations": {"aileron": {"credential": {
					"scheme": "bearer",
					"placeholders": [{"env": "GH_TOKEN", "value": "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA"}]
				}}}
			}`,
			want: CredentialConvention{
				Scheme: inject.SchemeBearer,
				Placeholders: []CredentialPlaceholder{
					{Env: "GH_TOKEN", Value: "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA"},
				},
			},
		},
		{
			name: "sigv4-resign two placeholders",
			manifest: `{
				"customizations": {"aileron": {"credential": {
					"scheme": "sigv4-resign",
					"placeholders": [
						{"env": "AWS_ACCESS_KEY_ID", "value": "AKIAIOSFODNN7PLACEHLDR"},
						{"env": "AWS_SECRET_ACCESS_KEY", "value": "placeholderAileronInjectsRealSecretXXXXXX"}
					]
				}}}
			}`,
			want: CredentialConvention{
				Scheme: inject.SchemeSigV4Resign,
				Placeholders: []CredentialPlaceholder{
					{Env: "AWS_ACCESS_KEY_ID", Value: "AKIAIOSFODNN7PLACEHLDR"},
					{Env: "AWS_SECRET_ACCESS_KEY", Value: "placeholderAileronInjectsRealSecretXXXXXX"},
				},
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := ParseCredentialConvention([]byte(tt.manifest))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !ok {
				t.Fatalf("ok = false, want true for a present valid convention")
			}
			assertConventionEqual(t, got, tt.want)
		})
	}
}

// TestParseCredentialConvention_Absent covers the manifests that carry no
// convention: a Feature with no customizations at all, and one whose
// customizations.aileron block omits credential (agent Features). Both yield the
// clean (zero, false, nil) result, never an error.
func TestParseCredentialConvention_Absent(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
	}{
		{
			name:     "no customizations",
			manifest: `{"id": "claude", "version": "0.0.1"}`,
		},
		{
			name:     "aileron block without credential",
			manifest: `{"customizations": {"aileron": {"cli": {"name": "gh"}}}}`,
		},
		{
			name:     "customizations without aileron",
			manifest: `{"customizations": {"vscode": {"extensions": []}}}`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := ParseCredentialConvention([]byte(tt.manifest))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok {
				t.Fatalf("ok = true, want false for a manifest with no convention")
			}
			if got.Scheme != "" || len(got.Placeholders) != 0 {
				t.Fatalf("got non-zero convention %+v, want zero value", got)
			}
		})
	}
}

// TestParseCredentialConvention_Invalid covers every fail-closed rule: a
// present-but-broken convention must be a loud error, never a silent no-op.
func TestParseCredentialConvention_Invalid(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		// wantUnknownScheme asserts the error additionally wraps
		// inject.ErrUnknownScheme so callers can distinguish that case.
		wantUnknownScheme bool
	}{
		{
			name:              "unknown scheme",
			manifest:          `{"customizations":{"aileron":{"credential":{"scheme":"totp","placeholders":[{"env":"X","value":"y"}]}}}}`,
			wantUnknownScheme: true,
		},
		{
			name:     "missing scheme",
			manifest: `{"customizations":{"aileron":{"credential":{"placeholders":[{"env":"X","value":"y"}]}}}}`,
		},
		{
			name:     "empty scheme string",
			manifest: `{"customizations":{"aileron":{"credential":{"scheme":"","placeholders":[{"env":"X","value":"y"}]}}}}`,
		},
		{
			name:     "no placeholders key",
			manifest: `{"customizations":{"aileron":{"credential":{"scheme":"bearer"}}}}`,
		},
		{
			name:     "empty placeholders array",
			manifest: `{"customizations":{"aileron":{"credential":{"scheme":"bearer","placeholders":[]}}}}`,
		},
		{
			name:     "placeholder missing env",
			manifest: `{"customizations":{"aileron":{"credential":{"scheme":"bearer","placeholders":[{"value":"y"}]}}}}`,
		},
		{
			name:     "placeholder missing value",
			manifest: `{"customizations":{"aileron":{"credential":{"scheme":"bearer","placeholders":[{"env":"X"}]}}}}`,
		},
		{
			name:     "duplicate placeholder env",
			manifest: `{"customizations":{"aileron":{"credential":{"scheme":"sigv4-resign","placeholders":[{"env":"X","value":"a"},{"env":"X","value":"b"}]}}}}`,
		},
		{
			name:     "unknown key in credential block",
			manifest: `{"customizations":{"aileron":{"credential":{"scheme":"bearer","placeholder":[{"env":"X","value":"y"}]}}}}`,
		},
		{
			name:     "malformed json",
			manifest: `{"customizations":{"aileron":{"credential":{`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := ParseCredentialConvention([]byte(tt.manifest))
			if err == nil {
				t.Fatalf("expected error, got nil (result %+v, ok %v)", got, ok)
			}
			if ok {
				t.Fatalf("ok = true on an invalid convention; want false")
			}
			// Malformed JSON is a decode error, not an ErrInvalidCredentialConvention;
			// every other case is a present-but-broken convention.
			if tt.name != "malformed json" && !errors.Is(err, ErrInvalidCredentialConvention) {
				t.Fatalf("error %v does not wrap ErrInvalidCredentialConvention", err)
			}
			if tt.wantUnknownScheme && !errors.Is(err, inject.ErrUnknownScheme) {
				t.Fatalf("error %v does not wrap inject.ErrUnknownScheme", err)
			}
		})
	}
}

// TestLoadCredentialConvention_RepoCatalog asserts the loader yields exactly the
// declared convention for each real catalog entry: aws-cli's sigv4-resign pair,
// gh's bearer GH_TOKEN placeholder, and no convention for the agent Features.
func TestLoadCredentialConvention_RepoCatalog(t *testing.T) {
	root := featuresContext(t)

	t.Run("aws-cli", func(t *testing.T) {
		got, ok, err := LoadCredentialConvention(root, "aws-cli")
		if err != nil {
			t.Fatalf("load aws-cli: %v", err)
		}
		if !ok {
			t.Fatalf("aws-cli must carry a credential convention")
		}
		// The placeholder values must equal the constants
		// cmd/aileron/skill_launch_proxy.go plants today, so #1828 can swap the
		// hardcoded pair for catalog data with zero behavior change at the proxy.
		want := CredentialConvention{
			Scheme: inject.SchemeSigV4Resign,
			Placeholders: []CredentialPlaceholder{
				{Env: "AWS_ACCESS_KEY_ID", Value: "AKIAIOSFODNN7PLACEHLDR"},
				{Env: "AWS_SECRET_ACCESS_KEY", Value: "placeholderAileronInjectsRealSecretXXXXXX"},
			},
		}
		assertConventionEqual(t, got, want)
	})

	t.Run("gh", func(t *testing.T) {
		got, ok, err := LoadCredentialConvention(root, "gh")
		if err != nil {
			t.Fatalf("load gh: %v", err)
		}
		if !ok {
			t.Fatalf("gh must carry a credential convention")
		}
		want := CredentialConvention{
			Scheme: inject.SchemeBearer,
			Placeholders: []CredentialPlaceholder{
				{Env: "GH_TOKEN", Value: "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA"},
			},
		}
		assertConventionEqual(t, got, want)
	})

	for _, agent := range []string{"claude", "codex"} {
		agent := agent
		t.Run(agent, func(t *testing.T) {
			got, ok, err := LoadCredentialConvention(root, agent)
			if err != nil {
				t.Fatalf("load %s: %v", agent, err)
			}
			if ok {
				t.Fatalf("agent Feature %s must carry no credential convention, got %+v", agent, got)
			}
		})
	}
}

// TestLoadCredentialConvention_MissingManifest asserts a nonexistent tool entry
// is an error, not a silent (zero, false, nil).
func TestLoadCredentialConvention_MissingManifest(t *testing.T) {
	root := featuresContext(t)
	if _, _, err := LoadCredentialConvention(root, "does-not-exist"); err == nil {
		t.Fatalf("expected error loading a missing manifest, got nil")
	}
}

// ghSealingManifest decodes the api.github.com sentinel-swap entry from gh's
// customizations.aileron.cli.sealing block so the drift guard can compare it
// against the credential convention without importing internal/cli.
type ghSealingManifest struct {
	Customizations struct {
		Aileron struct {
			CLI struct {
				Sealing []struct {
					Host          string `json:"host"`
					Scheme        string `json:"scheme"`
					EmitMechanism string `json:"emit_mechanism"`
					Sentinel      struct {
						Value string `json:"value"`
						Env   string `json:"env"`
					} `json:"sentinel"`
				} `json:"sealing"`
			} `json:"cli"`
		} `json:"aileron"`
	} `json:"customizations"`
}

// TestGHConventionMatchesSealingSentinel is the Unit 2 drift guard: gh's new
// customizations.aileron.credential convention must not silently diverge from
// the sentinel-swap entry in its own customizations.aileron.cli.sealing block.
// The credential convention's scheme, placeholder env, and placeholder value
// must equal the api.github.com sealing entry's scheme, sentinel env, and
// sentinel value. This keeps the two declarations in lockstep without forking
// the loader into a derive-from-sealing second path.
func TestGHConventionMatchesSealingSentinel(t *testing.T) {
	root := featuresContext(t)
	path := filepath.Join(root, "gh", "devcontainer-feature.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	conv, ok, err := ParseCredentialConvention(raw)
	if err != nil {
		t.Fatalf("parse gh convention: %v", err)
	}
	if !ok {
		t.Fatalf("gh must carry a credential convention")
	}
	if len(conv.Placeholders) != 1 {
		t.Fatalf("gh convention has %d placeholders, want 1", len(conv.Placeholders))
	}

	var sealing ghSealingManifest
	if err := json.Unmarshal(raw, &sealing); err != nil {
		t.Fatalf("decode gh sealing block: %v", err)
	}
	var swap *struct {
		Host          string `json:"host"`
		Scheme        string `json:"scheme"`
		EmitMechanism string `json:"emit_mechanism"`
		Sentinel      struct {
			Value string `json:"value"`
			Env   string `json:"env"`
		} `json:"sentinel"`
	}
	for i := range sealing.Customizations.Aileron.CLI.Sealing {
		if sealing.Customizations.Aileron.CLI.Sealing[i].Host == "api.github.com" &&
			sealing.Customizations.Aileron.CLI.Sealing[i].EmitMechanism == "sentinel-swap" {
			swap = &sealing.Customizations.Aileron.CLI.Sealing[i]
			break
		}
	}
	if swap == nil {
		t.Fatalf("gh cli.sealing has no api.github.com sentinel-swap entry to compare against")
	}

	if string(conv.Scheme) != swap.Scheme {
		t.Fatalf("convention scheme %q != sealing sentinel-swap scheme %q", conv.Scheme, swap.Scheme)
	}
	if conv.Placeholders[0].Env != swap.Sentinel.Env {
		t.Fatalf("convention placeholder env %q != sealing sentinel env %q", conv.Placeholders[0].Env, swap.Sentinel.Env)
	}
	if conv.Placeholders[0].Value != swap.Sentinel.Value {
		t.Fatalf("convention placeholder value %q != sealing sentinel value %q", conv.Placeholders[0].Value, swap.Sentinel.Value)
	}
}

func assertConventionEqual(t *testing.T, got, want CredentialConvention) {
	t.Helper()
	if got.Scheme != want.Scheme {
		t.Fatalf("scheme = %q, want %q", got.Scheme, want.Scheme)
	}
	if len(got.Placeholders) != len(want.Placeholders) {
		t.Fatalf("placeholders len = %d, want %d (%+v)", len(got.Placeholders), len(want.Placeholders), got.Placeholders)
	}
	for i := range want.Placeholders {
		if got.Placeholders[i] != want.Placeholders[i] {
			t.Fatalf("placeholder %d = %+v, want %+v", i, got.Placeholders[i], want.Placeholders[i])
		}
	}
}
