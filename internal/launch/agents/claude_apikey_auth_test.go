package agents_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
	"github.com/ALRubinger/aileron/internal/vault"
)

// claudeModelsCapture records what the api-key validation probe sent so a
// test can assert the acquirer's contract with the provider (the key
// header and version header on the GET /v1/models request).
type claudeModelsCapture struct {
	apiKey  string
	version string
	path    string
}

// claudeModelsServer is an httptest server standing in for Claude's
// /v1/models endpoint that the api-key validation probe hits. status is
// the HTTP status it returns, letting a test drive the 2xx (valid key)
// and 401 (invalid key) branches. The returned capture records the
// x-api-key / anthropic-version headers and the request path.
func claudeModelsServer(t *testing.T, status int) (*httptest.Server, *claudeModelsCapture) {
	t.Helper()
	cap := &claudeModelsCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.apiKey = r.Header.Get("x-api-key")
		cap.version = r.Header.Get("anthropic-version")
		cap.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status/100 == 2 {
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-3"}]}`)
		} else {
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

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
	srv, capture := claudeModelsServer(t, http.StatusOK)
	var out bytes.Buffer
	secret, err := eb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		HTTPClient:   testHTTPClient(srv.URL),
		Out:          &out,
		CodePrompter: scriptedPrompter("  sk-ant-xyz  ", nil),
	})
	if err != nil {
		t.Fatalf("HostAcquire: %v", err)
	}
	if string(secret.Value) != "sk-ant-xyz" {
		t.Errorf("Secret.Value = %q, want trimmed sk-ant-xyz", secret.Value)
	}
	// The validation probe must present the trimmed key via x-api-key
	// (NOT a Bearer token) and the pinned anthropic-version, against the
	// models endpoint.
	if capture.apiKey != "sk-ant-xyz" {
		t.Errorf("probe x-api-key = %q, want trimmed sk-ant-xyz", capture.apiKey)
	}
	if capture.version == "" {
		t.Errorf("probe anthropic-version header is empty; want it set")
	}
	if capture.path != "/v1/models" {
		t.Errorf("probe path = %q, want /v1/models", capture.path)
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
	srv, _ := claudeModelsServer(t, http.StatusOK)
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
		HTTPClient:   testHTTPClient(srv.URL),
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

// Regression (#1384, sub-req B): a pasted key that authenticates (2xx
// from the models probe) is seeded; a key the provider rejects (401) is
// NOT seeded — the acquirer returns an empty Secret so the launcher logs
// and falls back to the in-container login rather than persisting a dead
// key into the vault.
func TestClaudeAPIKey_HostAcquire_ValidKeyIsSeeded(t *testing.T) {
	eb := claudeAPIKeyBinding(t)
	srv, _ := claudeModelsServer(t, http.StatusOK)
	secret, err := eb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		HTTPClient:   testHTTPClient(srv.URL),
		CodePrompter: scriptedPrompter("sk-ant-valid", nil),
	})
	if err != nil {
		t.Fatalf("HostAcquire: %v", err)
	}
	if string(secret.Value) != "sk-ant-valid" {
		t.Errorf("Secret.Value = %q, want sk-ant-valid", secret.Value)
	}
}

func TestClaudeAPIKey_HostAcquire_InvalidKeyIsNotSeeded(t *testing.T) {
	eb := claudeAPIKeyBinding(t)
	srv, _ := claudeModelsServer(t, http.StatusUnauthorized)
	var out bytes.Buffer
	secret, err := eb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		HTTPClient:   testHTTPClient(srv.URL),
		Out:          &out,
		CodePrompter: scriptedPrompter("sk-ant-revoked", nil),
	})
	// A rejected key is non-fatal: empty Secret (no seed) so the launcher
	// falls back. An accompanying error is permitted as a fallback signal.
	if len(secret.Value) != 0 {
		t.Errorf("invalid key must yield empty Secret (no seed), got %q", secret.Value)
	}
	if err == nil {
		t.Error("invalid key should surface a (non-fatal) error so the launcher logs the fallback")
	}
	// The user sees a message explaining the key did not validate.
	if !strings.Contains(out.String(), "did not validate") {
		t.Errorf("Out = %q, want a message that the key did not validate", out.String())
	}
}

// A 403 (e.g. key valid but lacking model access) is treated the same as
// a 401: not seeded, launcher falls back.
func TestClaudeAPIKey_HostAcquire_ForbiddenKeyIsNotSeeded(t *testing.T) {
	eb := claudeAPIKeyBinding(t)
	srv, _ := claudeModelsServer(t, http.StatusForbidden)
	secret, err := eb.HostAcquire(context.Background(), launch.HostAcquireDeps{
		Ctx:          context.Background(),
		HTTPClient:   testHTTPClient(srv.URL),
		CodePrompter: scriptedPrompter("sk-ant-forbidden", nil),
	})
	if len(secret.Value) != 0 {
		t.Errorf("forbidden key must yield empty Secret (no seed), got %q", secret.Value)
	}
	if err == nil {
		t.Error("forbidden key should surface a (non-fatal) error so the launcher logs the fallback")
	}
}
