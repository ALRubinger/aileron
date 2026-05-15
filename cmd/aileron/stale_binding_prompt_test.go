package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestStaleBindingHeader_PrefersServiceLabel: the CLI prefers the
// human-readable service label ("Google") over the FQN in prompt
// output because the operator reads the prompt under cognitive load.
// FQNs are the fallback when the daemon doesn't populate Service.
func TestStaleBindingHeader_PrefersServiceLabel(t *testing.T) {
	withService := staleBindingWire{
		Name:         "oauth2/google/work",
		ConnectorFQN: "github://acme/google",
		Service:      "google",
	}
	if got := staleBindingHeader(withService); !strings.Contains(got, "(google)") {
		t.Errorf("with service: header = %q, want service-labeled", got)
	}
	noService := staleBindingWire{
		Name:         "oauth2/google/work",
		ConnectorFQN: "github://acme/google",
	}
	if got := staleBindingHeader(noService); !strings.Contains(got, "github://acme/google") {
		t.Errorf("no service: header = %q, want FQN fallback", got)
	}
}

// TestRenderStaleBindingDetail_ScopeDriftNamesMissingScopes: drift
// phrasing must list what's missing so the operator knows what
// reauthorize will unlock. The phrasing is the load-bearing piece —
// without it the operator can't tell whether to reauthorize now or
// wait.
func TestRenderStaleBindingDetail_ScopeDriftNamesMissingScopes(t *testing.T) {
	var buf bytes.Buffer
	renderStaleBindingDetail(&buf, staleBindingWire{
		Name:          "oauth2/google/work",
		ConnectorFQN:  "github://acme/google",
		StaleReason:   "scope_drift",
		MissingScopes: []string{"drive", "documents"},
		Service:       "google",
	})
	out := buf.String()
	if !strings.Contains(out, "additional OAuth scopes") {
		t.Errorf("drift phrasing missing: %q", out)
	}
	if !strings.Contains(out, "drive") || !strings.Contains(out, "documents") {
		t.Errorf("missing scopes not listed: %q", out)
	}
}

// TestRenderStaleBindingDetail_NoGrantRecordExplainsMigration: the
// migration case must phrase the reason as a one-time event so the
// operator doesn't read a `no_grant_record` flip as a sign their
// manifest is churning. Collapsing the two reasons into a generic
// "needs reauthorize" hides that distinction (#741 open question).
func TestRenderStaleBindingDetail_NoGrantRecordExplainsMigration(t *testing.T) {
	var buf bytes.Buffer
	renderStaleBindingDetail(&buf, staleBindingWire{
		Name:         "oauth2/google/work",
		ConnectorFQN: "github://acme/google",
		StaleReason:  "no_grant_record",
		Service:      "google",
	})
	out := buf.String()
	if !strings.Contains(out, "predates") {
		t.Errorf("migration phrasing missing: %q", out)
	}
	if strings.Contains(out, "Missing:") {
		t.Errorf("no_grant_record should not list missing scopes: %q", out)
	}
}

// TestPromptReauthorizeStaleBindings_EmptyListIsNoOp: the canonical
// happy path is "no stale bindings"; the helper must not emit any
// output (and certainly must not prompt) when there's nothing to
// surface.
func TestPromptReauthorizeStaleBindings_EmptyListIsNoOp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	promptReauthorizeStaleBindings(nil, false, false, strings.NewReader(""), &stdout, &stderr)
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// TestPromptReauthorizeStaleBindings_NoBindSuppressesPromptButLists:
// --no-bind is the scripted-install branch — operators piping
// installs through CI want the signal in stdout but not an
// interactive prompt that blocks on stdin.
func TestPromptReauthorizeStaleBindings_NoBindSuppressesPromptButLists(t *testing.T) {
	stale := []staleBindingWire{{
		Name: "oauth2/google/work", ConnectorFQN: "github://acme/google",
		StaleReason: "scope_drift", MissingScopes: []string{"drive"}, Service: "google",
	}}
	var stdout, stderr bytes.Buffer
	// Stdin is empty — if the helper attempted to read it, the prompt
	// would return "" and we'd see the "Skipped" path in stdout. The
	// assertion below catches that regression.
	promptReauthorizeStaleBindings(stale, false /*autoYes*/, true /*noBind*/, strings.NewReader(""), &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "oauth2/google/work") {
		t.Errorf("--no-bind should still list the binding: %q", out)
	}
	if strings.Contains(out, "Reauthorize now?") {
		t.Errorf("--no-bind must not prompt: %q", out)
	}
	if strings.Contains(out, "Skipped") {
		t.Errorf("--no-bind hit the interactive-skip path; helper read stdin when it shouldn't: %q", out)
	}
	if !strings.Contains(out, "aileron binding reauthorize") {
		t.Errorf("--no-bind should advertise the standalone verb: %q", out)
	}
}

// TestPromptReauthorizeStaleBindings_AutoYesListsButDoesNotDispatch:
// per #741, `--yes` must NOT auto-reauthorize. The reauthorize flow
// opens a browser tab; doing that without explicit Y is a foot-gun.
// The helper surfaces the work to do and exits without dispatching.
func TestPromptReauthorizeStaleBindings_AutoYesListsButDoesNotDispatch(t *testing.T) {
	stale := []staleBindingWire{{
		Name: "oauth2/google/work", ConnectorFQN: "github://acme/google",
		StaleReason: "scope_drift", MissingScopes: []string{"drive"}, Service: "google",
	}}
	var stdout, stderr bytes.Buffer
	promptReauthorizeStaleBindings(stale, true /*autoYes*/, false, strings.NewReader(""), &stdout, &stderr)
	out := stdout.String()
	if strings.Contains(out, "Reauthorize now?") {
		t.Errorf("--yes must not prompt: %q", out)
	}
	if !strings.Contains(out, "aileron binding reauthorize oauth2/google/work") {
		t.Errorf("--yes should advertise the standalone verb with the binding name: %q", out)
	}
}

// TestPromptReauthorizeStaleBindings_DeclineSkipsAndHints: the user
// answers "n" interactively; the helper prints the skip hint and
// does NOT attempt the reauthorize. The reauthorize verb would
// require a live daemon; reaching it under test would be the only
// way this assertion fires.
func TestPromptReauthorizeStaleBindings_DeclineSkipsAndHints(t *testing.T) {
	stale := []staleBindingWire{{
		Name: "oauth2/google/work", ConnectorFQN: "github://acme/google",
		StaleReason: "scope_drift", MissingScopes: []string{"drive"},
	}}
	var stdout, stderr bytes.Buffer
	promptReauthorizeStaleBindings(stale, false, false, strings.NewReader("n\n"), &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "Skipped") {
		t.Errorf("expected skip hint, got: %q", out)
	}
	if !strings.Contains(out, "aileron binding reauthorize oauth2/google/work") {
		t.Errorf("skip hint should name the standalone verb: %q", out)
	}
}

// TestRunAction_AddPromptsForStaleBindings_DeclineSurfacesHint:
// end-to-end through `aileron action add`. The fake daemon returns
// a stale binding on the install response and the CLI presents the
// prompt at the tail of installOneAction. We answer "n" so the test
// doesn't have to mock the reauthorize OAuth dance (which needs a
// browser hand-off the test environment can't supply).
//
// This is the install-side equivalent of issue #741's central UX
// claim: the user must learn about the stale state at install time,
// not at first invocation. Removing the prompt loop without updating
// this test would surface immediately.
func TestRunAction_AddPromptsForStaleBindings_DeclineSurfacesHint(t *testing.T) {
	withSeededKeyring(t, "github://x/y")
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"github://x/y/actions/echo","version":"1.0.0","hash":"sha256:a","name":"echo","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{
				"name":"echo","fqn":"github://x/y/actions/echo",
				"version":"1.0.0","source":"x","path":"/p",
				"newly_stale_bindings":[{
					"name":"oauth2/google/work","connector_fqn":"github://acme/google",
					"stale_reason":"scope_drift","missing_scopes":["drive"],"service":"google"
				}]
			}`)
		case "/hub/action-install-decision":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	// Consent "y" to install, then "n" to the reauthorize prompt.
	stdin := strings.NewReader("y\nn\n")
	var stdout, stderr bytes.Buffer
	code := runAction([]string{"add", "github://x/y/actions/echo@1.0.0"},
		stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"oauth2/google/work",
		"additional OAuth scopes",
		"Missing: drive",
		"Skipped",
		"aileron binding reauthorize oauth2/google/work",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// TestRunAction_AddYesFlagDoesNotAutoReauthorize: per #741, --yes
// must surface the work to do without dispatching the reauthorize.
// A browser-hand-off triggered by --yes alone is a foot-gun: the
// operator scripting installs did not consent to a credential
// operation, only to skipping the consent prompt for the install.
func TestRunAction_AddYesFlagDoesNotAutoReauthorize(t *testing.T) {
	withSeededKeyring(t, "github://x/y")
	hits := map[string]int{}
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		switch r.URL.Path {
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"github://x/y/actions/echo","version":"1.0.0","hash":"sha256:a","name":"echo","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{
				"name":"echo","fqn":"github://x/y/actions/echo",
				"version":"1.0.0","source":"x","path":"/p",
				"newly_stale_bindings":[{
					"name":"oauth2/google/work","connector_fqn":"github://acme/google",
					"stale_reason":"scope_drift","missing_scopes":["drive"]
				}]
			}`)
		case "/hub/action-install-decision":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected path with --yes: %s", r.URL.Path)
		}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "add", "github://x/y/actions/echo@1.0.0", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if hits["/bindings/setup/oauth2/init"] != 0 {
		t.Errorf("--yes attempted to dispatch reauthorize (init endpoint hit %d times)",
			hits["/bindings/setup/oauth2/init"])
	}
	if !strings.Contains(stdout.String(), "aileron binding reauthorize oauth2/google/work") {
		t.Errorf("--yes should advertise the standalone verb: %s", stdout.String())
	}
}
