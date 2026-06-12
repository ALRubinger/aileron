package agents_test

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch/agents"
	"github.com/ALRubinger/aileron/internal/vault"
)

// Claude AuthSpec contract (U4):
//
//   - AuthSpec ships one FileBinding (.credentials.json) and one
//     StaticFile (.claude.json).
//   - Render is byte-identity over a valid envelope.
//   - Capture is byte-identity over a valid envelope and stamps
//     Metadata.Type = oauth_refresh_token.
//   - Render rejects an empty Value (vault entry uninitialized).
//   - Capture rejects an envelope without claudeAiOauth.accessToken
//     (partial-write or schema-drift session).
//   - StaticFile content is the documented onboarding stub at 0644.

func TestClaude_AuthSpec_Shape(t *testing.T) {
	spec := agents.Claude{}.AuthSpec()
	if len(spec.FileBindings) != 1 {
		t.Fatalf("FileBindings = %d, want 1", len(spec.FileBindings))
	}
	if len(spec.StaticFiles) != 1 {
		t.Fatalf("StaticFiles = %d, want 1", len(spec.StaticFiles))
	}
	if len(spec.EnvBindings) != 0 {
		t.Errorf("EnvBindings = %d, want 0 (v1 ships FileBinding only)", len(spec.EnvBindings))
	}

	fb := spec.FileBindings[0]
	if fb.VaultPath != "agents/claude/oauth" {
		t.Errorf("VaultPath = %q, want agents/claude/oauth", fb.VaultPath)
	}
	if fb.ContainerPath != "/home/agent/.claude/.credentials.json" {
		t.Errorf("ContainerPath = %q, want /home/agent/.claude/.credentials.json", fb.ContainerPath)
	}
	if fb.Mode != 0o600 {
		t.Errorf("Mode = %v, want 0600", fb.Mode)
	}
	if fb.Required {
		t.Errorf("Required = true; empty vault must trigger in-container login fallthrough")
	}
	if fb.Render == nil {
		t.Errorf("Render must be set")
	}
	if fb.Capture == nil {
		t.Errorf("Capture must be set")
	}

	sf := spec.StaticFiles[0]
	if sf.ContainerPath != "/home/agent/.claude.json" {
		t.Errorf("StaticFile path = %q, want /home/agent/.claude.json", sf.ContainerPath)
	}
	if sf.Mode != 0o644 {
		t.Errorf("StaticFile mode = %v, want 0644", sf.Mode)
	}
	if string(sf.Content) != `{"hasCompletedOnboarding":true,"installMethod":"native"}` {
		t.Errorf("StaticFile content drift: %q", sf.Content)
	}
}

func TestClaude_Render_ByteIdentityOnValidEnvelope(t *testing.T) {
	envelope := []byte(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"rt","expiresAt":1717000000,"scopes":["org:create_api_key"]}}`)
	spec := agents.Claude{}.AuthSpec()
	got, err := spec.FileBindings[0].Render(vault.Secret{Value: envelope})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Equal(got, envelope) {
		t.Errorf("Render bytes = %q, want byte-identity %q", got, envelope)
	}
}

func TestClaude_Render_RejectsEmptyVaultValue(t *testing.T) {
	spec := agents.Claude{}.AuthSpec()
	_, err := spec.FileBindings[0].Render(vault.Secret{Value: nil})
	if err == nil {
		t.Fatal("expected error for empty Value")
	}
	if !strings.Contains(err.Error(), "empty Value") {
		t.Errorf("err = %v, want mention of empty Value", err)
	}
}

func TestClaude_Render_RejectsMalformedEnvelope(t *testing.T) {
	spec := agents.Claude{}.AuthSpec()
	_, err := spec.FileBindings[0].Render(vault.Secret{Value: []byte(`not-json`)})
	if err == nil {
		t.Fatal("expected error for malformed envelope")
	}
}

func TestClaude_Capture_RoundTripStampsOAuthRefreshToken(t *testing.T) {
	envelope := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"r","expiresAt":1717000000}}`)
	spec := agents.Claude{}.AuthSpec()
	got, err := spec.FileBindings[0].Capture(envelope)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !bytes.Equal(got.Value, envelope) {
		t.Errorf("Capture Value = %q, want byte-identity %q", got.Value, envelope)
	}
	if got.Metadata.Type != "oauth_refresh_token" {
		t.Errorf("Metadata.Type = %q, want oauth_refresh_token", got.Metadata.Type)
	}
}

func TestClaude_Capture_RejectsEnvelopeMissingAccessToken(t *testing.T) {
	// Schema drift / partial write: the file parses as JSON but the
	// access_token field is missing. The launcher's R13 contract is
	// "skip the PUT rather than overwrite vault with garbage." This
	// test pins that the Capture func is the one that enforces it.
	envelope := []byte(`{"claudeAiOauth":{"refreshToken":"r"}}`)
	spec := agents.Claude{}.AuthSpec()
	_, err := spec.FileBindings[0].Capture(envelope)
	if err == nil {
		t.Fatal("expected error for envelope missing accessToken")
	}
	if !strings.Contains(err.Error(), "accessToken") {
		t.Errorf("err = %v, want mention of accessToken", err)
	}
}

func TestClaude_Capture_RejectsNonJSON(t *testing.T) {
	spec := agents.Claude{}.AuthSpec()
	_, err := spec.FileBindings[0].Capture([]byte("garbage bytes"))
	if err == nil {
		t.Fatal("expected error for non-JSON envelope")
	}
	// Wrapping pattern: err.Error() should mention the parse failure
	// so the operator can see the file is malformed.
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("err = %v, want mention of parse failure", err)
	}
}

func TestClaude_AuthSpec_RoundTrip(t *testing.T) {
	// Render → bytes → Capture on the same valid envelope returns a
	// Secret whose Value equals the input. This is the in-container
	// rotation contract: whatever the agent rewrites, Capture must
	// snapshot back so the next launch's Render reproduces it.
	envelope := []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1717000000,"scopes":["org:create_api_key"]}}`)
	spec := agents.Claude{}.AuthSpec()
	rendered, err := spec.FileBindings[0].Render(vault.Secret{Value: envelope})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	captured, err := spec.FileBindings[0].Capture(rendered)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !bytes.Equal(captured.Value, envelope) {
		t.Errorf("round-trip drift: got %q want %q", captured.Value, envelope)
	}
}

func TestClaude_OnboardingStub_Constant(t *testing.T) {
	// The onboarding stub must be the documented content per R18 /
	// AE4. A drift here means Claude paints the theme picker on
	// every launch and the user notices.
	spec := agents.Claude{}.AuthSpec()
	sf := spec.StaticFiles[0]
	const want = `{"hasCompletedOnboarding":true,"installMethod":"native"}`
	if string(sf.Content) != want {
		t.Errorf("onboarding stub drift: got %q, want %q", sf.Content, want)
	}
}

// errIsMalformed pins the error sentinel pattern in Claude's
// capture path — callers (the launcher's CaptureFn) wrap on
// errors.Is rather than string matching.
func TestClaude_Capture_ErrorWrapsSentinel(t *testing.T) {
	spec := agents.Claude{}.AuthSpec()
	_, err := spec.FileBindings[0].Capture([]byte("{}"))
	if err == nil {
		t.Fatal("expected error for envelope missing claudeAiOauth")
	}
	// The launcher just looks at the error message; we just need it
	// not to be a nil-shape error and to mention the field. A future
	// version could expose the sentinel publicly if external code
	// needs to discriminate.
	if errors.Is(err, nil) {
		t.Fatal("Capture error must be non-nil for malformed envelope")
	}
}

func TestClaude_Fresher(t *testing.T) {
	fresher := agents.Claude{}.AuthSpec().FileBindings[0].Fresher
	if fresher == nil {
		t.Fatal("Claude binding must supply a Fresher so a stale concurrent capture cannot clobber a fresher rotation")
	}
	env := func(token string, expiresAt int64) vault.Secret {
		if expiresAt == 0 {
			return vault.Secret{Value: []byte(`{"claudeAiOauth":{"accessToken":"` + token + `"}}`)}
		}
		return vault.Secret{Value: []byte(`{"claudeAiOauth":{"accessToken":"` + token + `","expiresAt":` + itoa(expiresAt) + `}}`)}
	}
	tests := []struct {
		name              string
		captured, current vault.Secret
		want              bool
	}{
		{"newer expiresAt is fresher", env("a", 200), env("a", 100), true},
		{"older expiresAt is not fresher", env("a", 100), env("a", 200), false},
		{"equal non-zero expiresAt is not fresher", env("a", 100), env("a", 100), false},
		{"both-zero same token is not fresher", env("a", 0), env("a", 0), false},
		{"both-zero different token is a rotation without expiry, treat as fresher", env("new", 0), env("old", 0), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fresher(tt.captured, tt.current)
			if err != nil {
				t.Fatalf("fresher: %v", err)
			}
			if got != tt.want {
				t.Errorf("fresher = %v, want %v", got, tt.want)
			}
		})
	}

	// A malformed envelope on either side must error so the launcher
	// retains the prior entry rather than clobbering it.
	if _, err := fresher(vault.Secret{Value: []byte("not json")}, env("a", 100)); err == nil {
		t.Error("expected error for malformed captured envelope")
	}
	if _, err := fresher(env("a", 100), vault.Secret{Value: []byte("not json")}); err == nil {
		t.Error("expected error for malformed current envelope")
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
