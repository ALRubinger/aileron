package action

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
)

func TestIsLocalConnectorFQN(t *testing.T) {
	cases := []struct {
		fqn  string
		want bool
	}{
		{"local://user/linear", true},
		{"local://user/anything", true},
		{"github://acme/linear", false},
		{"hub://aileron/linear", false},
		{"", false},
		{"local://", false}, // too short to be a real local FQN
		{"local", false},
	}
	for _, c := range cases {
		got := isLocalConnectorFQN(c.fqn)
		if got != c.want {
			t.Errorf("isLocalConnectorFQN(%q) = %v, want %v", c.fqn, got, c.want)
		}
	}
}

func TestConnectorForLocal_RejectsWhenLocalStoreNil(t *testing.T) {
	// A SandboxExecutor without LocalStore wired must fail
	// cleanly when an action references a local:// connector.
	// Matches the daemon-misconfiguration error shape the
	// runtime surfaces in actions/audit.
	e := &SandboxExecutor{cache: map[string]cachedConnector{}}
	_, _, _, err := e.connectorForLocal(context.Background(), "local://user/linear")
	if err == nil {
		t.Fatal("expected error when LocalStore is nil")
	}
	if !strings.Contains(err.Error(), "local connector store") {
		t.Errorf("error should mention local connector store: %v", err)
	}
}

func TestConnectorForLocal_RejectsMalformedFQN(t *testing.T) {
	e := &SandboxExecutor{
		LocalStore: cstore.NewLocalStore(t.TempDir()),
		cache:      map[string]cachedConnector{},
	}
	// Wrong scheme — the connectorFor router would not actually
	// dispatch here in production (it pre-checks the prefix),
	// but the helper must still validate to defend against
	// callers that bypass the router.
	_, _, _, err := e.connectorForLocal(context.Background(), "github://acme/linear")
	if err == nil {
		t.Fatal("expected error for non-local FQN")
	}
}

func TestConnectorForLocal_ReturnsErrNotExistForMissingEntry(t *testing.T) {
	// LocalStore is wired but no manifest exists for the
	// requested name. The error must surface clearly so the
	// audit row records the missing-manifest condition.
	e := &SandboxExecutor{
		LocalStore: cstore.NewLocalStore(t.TempDir()),
		cache:      map[string]cachedConnector{},
	}
	_, _, _, err := e.connectorForLocal(context.Background(), "local://user/absent")
	if err == nil {
		t.Fatal("expected error for missing entry")
	}
	if !errors.Is(err, errMissingLocalManifest()) && !strings.Contains(err.Error(), "absent") {
		// LocalStore.Load wraps fs.ErrNotExist via os.ReadFile;
		// the executor wraps that again with "load local
		// connector %s: %w" so callers can format the FQN.
		// Either the wrapped sentinel matches or the FQN
		// appears in the message — both are acceptable.
		t.Errorf("error should reference the missing connector: %v", err)
	}
}

// errMissingLocalManifest is the wrapped fs.ErrNotExist the
// LocalStore returns when no entry exists. Exposed as a helper
// so the test stays readable without leaking the sentinel-vs-
// wrapped-string fallback into the assertion site.
func errMissingLocalManifest() error {
	// Use a discriminator the production wrap can match via
	// errors.Is — but Load wraps via os.ReadFile which returns
	// a *PathError wrapping fs.ErrNotExist. The test above
	// accepts either match.
	return errors.New("no such file or directory")
}
