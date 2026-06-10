package launch

import (
	"errors"
	"reflect"
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

// TestAuthSpec_ZeroValueIsValid pins the no-op-agent contract: the
// zero-value AuthSpec is a legal return from Agent.AuthSpec() and
// must not panic on field access. Goose, Pi, and OpenCode all ship
// with this shape in v1, so the contract is load-bearing for the
// launcher's default code path.
func TestAuthSpec_ZeroValueIsValid(t *testing.T) {
	spec := AuthSpec{}
	if len(spec.EnvBindings) != 0 {
		t.Fatalf("zero-value AuthSpec EnvBindings non-empty: %v", spec.EnvBindings)
	}
	if len(spec.FileBindings) != 0 {
		t.Fatalf("zero-value AuthSpec FileBindings non-empty: %v", spec.FileBindings)
	}
	if len(spec.StaticFiles) != 0 {
		t.Fatalf("zero-value AuthSpec StaticFiles non-empty: %v", spec.StaticFiles)
	}
	if err := validateAuthSpec(spec); err != nil {
		t.Fatalf("validateAuthSpec(zero) = %v, want nil", err)
	}
}

// TestEnvBinding_RenderRoundTrip verifies the EnvBinding contract
// from a launcher's perspective: Render takes the vault Secret and
// produces a map[string]string the launcher merges into the agent
// env. The contract is "what the binding's Render returns is what
// the launcher merges."
func TestEnvBinding_RenderRoundTrip(t *testing.T) {
	binding := EnvBinding{
		VaultPath: "agents/example/api-key",
		Required:  true,
		Render: func(s vault.Secret) (map[string]string, error) {
			return map[string]string{"EXAMPLE_API_KEY": string(s.Value)}, nil
		},
	}
	secret := vault.Secret{Value: []byte("xoxb-test")}
	got, err := binding.Render(secret)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	want := map[string]string{"EXAMPLE_API_KEY": "xoxb-test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Render = %v, want %v", got, want)
	}
}

// TestFileBinding_RoundTrip verifies the Render -> bytes -> Capture
// loop on the same envelope returns a Secret whose Value equals the
// input. This is the load-bearing contract for the in-container
// rotation snapshot: whatever the agent writes inside, Capture
// must read back as the same bytes for the next launch's Render.
func TestFileBinding_RoundTrip(t *testing.T) {
	envelope := []byte(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"rt","expiresAt":1}}`)
	binding := FileBinding{
		VaultPath:     "agents/claude/oauth",
		ContainerPath: "/home/agent/.claude/.credentials.json",
		Mode:          0o600,
		Required:      false,
		Render: func(s vault.Secret) ([]byte, error) {
			return s.Value, nil
		},
		Capture: func(b []byte) (vault.Secret, error) {
			return vault.Secret{Value: b, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}, nil
		},
	}
	original := vault.Secret{Value: envelope, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}
	rendered, err := binding.Render(original)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	captured, err := binding.Capture(rendered)
	if err != nil {
		t.Fatalf("Capture error: %v", err)
	}
	if !reflect.DeepEqual(captured.Value, original.Value) {
		t.Fatalf("round-trip Value mismatch: got %q want %q", captured.Value, original.Value)
	}
	if captured.Metadata.Type != "oauth_refresh_token" {
		t.Fatalf("round-trip Metadata.Type = %q, want oauth_refresh_token", captured.Metadata.Type)
	}
}

// TestStaticFile_ShapeIsConstant pins the StaticFile invariant: the
// Content slice is what the launcher writes verbatim into the bind
// mount on every launch, regardless of vault state. No render hook.
func TestStaticFile_ShapeIsConstant(t *testing.T) {
	sf := StaticFile{
		ContainerPath: "/home/agent/.claude.json",
		Mode:          0o644,
		Content:       []byte(`{"hasCompletedOnboarding":true,"installMethod":"native"}`),
	}
	if sf.ContainerPath != "/home/agent/.claude.json" {
		t.Fatalf("ContainerPath drift: %q", sf.ContainerPath)
	}
	if sf.Mode != 0o644 {
		t.Fatalf("Mode drift: %v", sf.Mode)
	}
	if string(sf.Content) != `{"hasCompletedOnboarding":true,"installMethod":"native"}` {
		t.Fatalf("Content drift: %q", sf.Content)
	}
}

// TestValidateAuthSpec_RejectsBindingsWithoutRender ensures
// programming-error specs (nil Render, nil Capture, empty path)
// surface at validation time instead of nil-deref'ing in the
// launcher lifecycle.
func TestValidateAuthSpec_RejectsBindingsWithoutRender(t *testing.T) {
	tests := []struct {
		name    string
		spec    AuthSpec
		wantErr error
	}{
		{
			name: "env binding missing vault path",
			spec: AuthSpec{
				EnvBindings: []EnvBinding{{
					Render: func(vault.Secret) (map[string]string, error) { return nil, nil },
				}},
			},
			wantErr: ErrAuthSpecEmptyPath,
		},
		{
			name: "env binding missing render",
			spec: AuthSpec{
				EnvBindings: []EnvBinding{{VaultPath: "agents/example/k"}},
			},
			wantErr: ErrAuthSpecRenderNil,
		},
		{
			name: "file binding missing container path",
			spec: AuthSpec{
				FileBindings: []FileBinding{{
					VaultPath: "agents/example/oauth",
					Render:    func(vault.Secret) ([]byte, error) { return nil, nil },
					Capture:   func([]byte) (vault.Secret, error) { return vault.Secret{}, nil },
				}},
			},
			wantErr: ErrAuthSpecEmptyPath,
		},
		{
			name: "file binding missing render",
			spec: AuthSpec{
				FileBindings: []FileBinding{{
					VaultPath:     "agents/example/oauth",
					ContainerPath: "/x",
					Capture:       func([]byte) (vault.Secret, error) { return vault.Secret{}, nil },
				}},
			},
			wantErr: ErrAuthSpecRenderNil,
		},
		{
			name: "file binding missing capture",
			spec: AuthSpec{
				FileBindings: []FileBinding{{
					VaultPath:     "agents/example/oauth",
					ContainerPath: "/x",
					Render:        func(vault.Secret) ([]byte, error) { return nil, nil },
				}},
			},
			wantErr: ErrAuthSpecCaptureNil,
		},
		{
			name: "static file missing container path",
			spec: AuthSpec{
				StaticFiles: []StaticFile{{Content: []byte("x")}},
			},
			wantErr: ErrAuthSpecEmptyPath,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAuthSpec(tc.spec)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("validateAuthSpec = %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateAuthSpec_AcceptsCompleteSpec proves a spec with all
// required fields satisfied passes validation; this is the path
// the per-agent Claude and Codex specs follow.
func TestValidateAuthSpec_AcceptsCompleteSpec(t *testing.T) {
	spec := AuthSpec{
		EnvBindings: []EnvBinding{{
			VaultPath: "agents/example/api-key",
			Render:    func(vault.Secret) (map[string]string, error) { return nil, nil },
		}},
		FileBindings: []FileBinding{{
			VaultPath:     "agents/example/oauth",
			ContainerPath: "/home/agent/.cfg",
			Mode:          0o600,
			Render:        func(vault.Secret) ([]byte, error) { return nil, nil },
			Capture:       func([]byte) (vault.Secret, error) { return vault.Secret{}, nil },
		}},
		StaticFiles: []StaticFile{{
			ContainerPath: "/home/agent/.state",
			Mode:          0o644,
			Content:       []byte(`{}`),
		}},
	}
	if err := validateAuthSpec(spec); err != nil {
		t.Fatalf("validateAuthSpec on complete spec: %v", err)
	}
}

// TestAuthSpec_AgentConformance asserts that the launch package's
// internal Agent fixtures return a non-panicking AuthSpec value,
// even when the spec is empty. Agents may ship empty specs (Goose,
// Pi, OpenCode in v1), but the method must exist and return
// without panicking — that is what the launcher's "spec is
// independent of seeding" contract leans on.
//
// Per-agent specs in internal/launch/agents/ have their own
// identity tests that assert on the actual binding shape.
func TestAuthSpec_AgentConformance(t *testing.T) {
	cases := []Agent{
		emptyBinaryAgent{},
		namedBinaryAgent{name: "codex"},
	}
	for _, a := range cases {
		t.Run(a.Name(), func(t *testing.T) {
			spec := a.AuthSpec()
			// No assertion on contents — only that the method returned
			// without panic and produced a usable struct.
			_ = spec
		})
	}
}
