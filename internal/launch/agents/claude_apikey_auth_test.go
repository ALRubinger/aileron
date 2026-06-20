package agents_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
	"github.com/ALRubinger/aileron/internal/vault"
)

// Claude api-key auth contract (#1339):
//
//   - Render maps the raw vault bytes to ANTHROPIC_API_KEY, trimming
//     surrounding whitespace; an empty/whitespace-only Value is rejected
//     with a recovery hint naming agents/claude/apikey.
//   - HostAcquire reads a pasted key from the host terminal and returns
//     a Secret stamped Metadata.Type == "api_key"; a nil prompter,
//     empty/whitespace paste, or a read error is non-fatal (empty
//     Secret) so the launcher falls back to the in-container login.
//
// Exercised through the PUBLIC EnvBinding surface
// (NewClaude(ClaudeAuthModeAPIKey).AuthSpec().EnvBindings[0]) rather than
// white-box-calling the unexported render/acquire funcs (residual #1346
// P1).

func claudeAPIKeyBinding(t *testing.T) launch.EnvBinding {
	t.Helper()
	spec := agents.NewClaude(agents.ClaudeAuthModeAPIKey).AuthSpec()
	if len(spec.EnvBindings) != 1 {
		t.Fatalf("api-key AuthSpec EnvBindings = %d, want 1", len(spec.EnvBindings))
	}
	eb := spec.EnvBindings[0]
	if eb.Render == nil {
		t.Fatal("api-key EnvBinding must declare Render")
	}
	if eb.HostAcquire == nil {
		t.Fatal("api-key EnvBinding must declare HostAcquire for host-side seeding")
	}
	return eb
}

func TestClaudeAPIKey_Render_MapsRawKeyToEnv(t *testing.T) {
	eb := claudeAPIKeyBinding(t)
	got, err := eb.Render(vault.Secret{Value: []byte("sk-ant-xyz")})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got["ANTHROPIC_API_KEY"] != "sk-ant-xyz" {
		t.Errorf("Render = %v, want ANTHROPIC_API_KEY=sk-ant-xyz", got)
	}
	if len(got) != 1 {
		t.Errorf("Render produced %d env vars, want exactly 1", len(got))
	}
}

func TestClaudeAPIKey_Render_TrimsWhitespace(t *testing.T) {
	eb := claudeAPIKeyBinding(t)
	got, err := eb.Render(vault.Secret{Value: []byte("  sk-ant-trim\n")})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got["ANTHROPIC_API_KEY"] != "sk-ant-trim" {
		t.Errorf("Render = %q, want trimmed sk-ant-trim", got["ANTHROPIC_API_KEY"])
	}
}

func TestClaudeAPIKey_Render_RejectsEmpty(t *testing.T) {
	eb := claudeAPIKeyBinding(t)
	for _, name := range []string{"empty", "whitespace"} {
		t.Run(name, func(t *testing.T) {
			val := []byte(nil)
			if name == "whitespace" {
				val = []byte("   \n\t ")
			}
			_, err := eb.Render(vault.Secret{Value: val})
			if err == nil {
				t.Fatal("expected error for empty/whitespace-only Value")
			}
			// Recovery hint must name the apikey slot so an operator
			// re-seeds the right destination.
			if got := err.Error(); !strings.Contains(got, "agents/claude/apikey") {
				t.Errorf("err = %v, want mention of agents/claude/apikey", err)
			}
		})
	}
}

func TestClaudeAPIKey_HostAcquire_ReturnsApiKeySecret(t *testing.T) {
	eb := claudeAPIKeyBinding(t)
	var out bytes.Buffer
	secret, err := eb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		Out:          &out,
		CodePrompter: scriptedPrompter("  sk-ant-xyz  ", nil),
	})
	if err != nil {
		t.Fatalf("HostAcquire: %v", err)
	}
	if string(secret.Value) != "sk-ant-xyz" {
		t.Errorf("Secret.Value = %q, want trimmed sk-ant-xyz", secret.Value)
	}
	if secret.Metadata.Type != "api_key" {
		t.Errorf("Metadata.Type = %q, want api_key", secret.Metadata.Type)
	}
	// The user-facing paste prompt is surfaced on deps.Out (it must
	// mention the Anthropic API key the user is being asked to paste).
	if !strings.Contains(out.String(), "Anthropic API key") {
		t.Errorf("Out = %q, want a paste prompt mentioning the Anthropic API key", out.String())
	}
	// The acquired Secret must round-trip the binding's own Render so the
	// launcher accepts it.
	if _, err := eb.Render(secret); err != nil {
		t.Fatalf("acquired Secret rejected by Render: %v", err)
	}
}

// Regression (#1356): the api-key flow prints its own Anthropic-specific
// paste banner to deps.Out, so it must invoke the CodePrompter with a nil
// promptW. Passing deps.Out would make the production
// defaultHostCodePrompter print a SECOND, wrong-domain banner ("Paste the
// code from your browser..."), double-printing a confusing prompt. The
// contract: the flow that owns the banner must not also hand the prompter
// a writer to print its own.
func TestClaudeAPIKey_HostAcquire_PrompterReceivesNilWriter(t *testing.T) {
	eb := claudeAPIKeyBinding(t)
	var out bytes.Buffer
	gotWriter := io.Writer(&out) // sentinel: must be overwritten to nil
	called := false
	prompter := func(_ context.Context, promptW io.Writer) (string, error) {
		called = true
		gotWriter = promptW
		return "sk-ant-xyz", nil
	}
	if _, err := eb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		Out:          &out,
		CodePrompter: prompter,
	}); err != nil {
		t.Fatalf("HostAcquire: %v", err)
	}
	if !called {
		t.Fatal("CodePrompter was never invoked")
	}
	if gotWriter != nil {
		t.Errorf("CodePrompter promptW = %v, want nil (flow owns its own banner; non-nil double-prints)", gotWriter)
	}
}

func TestClaudeAPIKey_HostAcquire_EmptyPasteIsNonFatal(t *testing.T) {
	eb := claudeAPIKeyBinding(t)
	for _, paste := range []string{"", "   \n\t"} {
		secret, err := eb.HostAcquire(context.Background(), launch.HostAcquireDeps{
			Ctx:          context.Background(),
			CodePrompter: scriptedPrompter(paste, nil),
		})
		if err != nil {
			t.Errorf("empty/whitespace paste must be non-fatal, got err %v", err)
		}
		if len(secret.Value) != 0 {
			t.Errorf("empty/whitespace paste must yield empty Secret, got %q", secret.Value)
		}
	}
}

func TestClaudeAPIKey_HostAcquire_PrompterErrorIsNonFatal(t *testing.T) {
	eb := claudeAPIKeyBinding(t)
	secret, err := eb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		CodePrompter: scriptedPrompter("", io.ErrUnexpectedEOF),
	})
	// Contract: read error is non-fatal — empty Secret so the launcher
	// falls back. An accompanying error is permitted (the launcher treats
	// it as a benign fallback signal), but the Secret must be empty so no
	// partial credential is seeded.
	if len(secret.Value) != 0 {
		t.Errorf("prompter error must yield empty Secret, got %q", secret.Value)
	}
	_ = err
}

func TestClaudeAPIKey_HostAcquire_NilPrompterIsNonFatal(t *testing.T) {
	eb := claudeAPIKeyBinding(t)
	secret, err := eb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx: context.Background(),
		// CodePrompter nil: no way to read the key.
	})
	if len(secret.Value) != 0 {
		t.Errorf("nil prompter must yield empty Secret, got %q", secret.Value)
	}
	_ = err
}
