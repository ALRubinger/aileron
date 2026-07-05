package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// installDecisionBody builds a fake /v1/hub/install-decision JSON
// response. Constructed inline so tests can vary trust_state and the
// risk indicator list to cover each ANSI-color path.
func installDecisionBody(fqn, fingerprint, trustState string, risks []string, footprint []string) string {
	risksJSON := "["
	for i, r := range risks {
		if i > 0 {
			risksJSON += ","
		}
		risksJSON += `"` + r + `"`
	}
	risksJSON += "]"
	footprintJSON := "["
	for i, f := range footprint {
		if i > 0 {
			footprintJSON += ","
		}
		footprintJSON += `"` + f + `"`
	}
	footprintJSON += "]"
	return `{
		"fqn":"` + fqn + `",
		"description":"test connector",
		"publisher_github":"acme",
		"fingerprint":"` + fingerprint + `",
		"trust_state":"` + trustState + `",
		"publisher_footprint":` + footprintJSON + `,
		"risk_indicators":` + risksJSON + `
	}`
}

// hubInstallServer routes /v1/hub/install-decision, /v1/connectors/preview,
// and /v1/connectors/install through optional handler hooks. Any hook
// left nil short-circuits with a sensible default — install-decision
// defaults to a 200 "unknown" trust-state response, install to 201,
// and preview to a 404 (so the Hub-confirmed path is exercised without
// accidentally falling back to legacy preview).
func hubInstallServer(t *testing.T, onDecision, onPreview, onInstall http.HandlerFunc) *httptest.Server {
	t.Helper()
	return fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/install-decision":
			if onDecision != nil {
				onDecision(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fqn := r.URL.Query().Get("fqn")
			_, _ = io.WriteString(w, installDecisionBody(
				fqn, "sha256:fingerprintAAAAAAAAAAA", "unknown",
				[]string{"First connector by this publisher you've installed"},
				nil))
		case "/connectors/preview":
			if onPreview != nil {
				onPreview(w, r)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case "/connectors/install":
			if onInstall != nil {
				onInstall(w, r)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","entry_dir":"/path"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// TestRunConnectorInstall_HubFlowYesPassesConfirmedFingerprint: the
// canonical Hub install-decision happy path. The CLI renders the
// decision, the operator answers `y`, and the install POST carries
// `confirmed_fingerprint` so the daemon writes per-FQN trust before
// running the install pipeline.
func TestRunConnectorInstall_HubFlowYesPassesConfirmedFingerprint(t *testing.T) {
	var seenInstallBody []byte
	hubInstallServer(t,
		nil, // default install-decision
		nil, // preview should NOT be called on the Hub path
		func(w http.ResponseWriter, r *http.Request) {
			seenInstallBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","entry_dir":"/path"}`)
		})

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("y\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	body := string(seenInstallBody)
	if !strings.Contains(body, `"confirmed_fingerprint":"sha256:fingerprintAAAAAAAAAAA"`) {
		t.Errorf("install body missing confirmed_fingerprint:\n%s", body)
	}
	if !strings.Contains(stdout.String(), "Hub install-decision") {
		t.Errorf("stdout missing Hub render header:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Installed:") {
		t.Errorf("stdout missing post-install confirmation:\n%s", stdout.String())
	}
}

// TestRunConnectorInstall_HubFlowYesFlagAutoConfirms: `--yes` short-
// circuits the prompt and still passes `confirmed_fingerprint` so the
// daemon writes trust on the operator's behalf. Matches today's
// `aileron action add --yes` semantics: explicit consent up front.
func TestRunConnectorInstall_HubFlowYesFlagAutoConfirms(t *testing.T) {
	var seenInstallBody []byte
	hubInstallServer(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		seenInstallBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","entry_dir":"/path"}`)
	})
	var stdout, stderr bytes.Buffer
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0", "--yes"},
		strings.NewReader(""), // no prompt
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(string(seenInstallBody), "confirmed_fingerprint") {
		t.Errorf("install body missing confirmed_fingerprint under --yes:\n%s", seenInstallBody)
	}
}

// TestRunConnectorInstall_HubFlowDeclinedShortCircuits: a `n` answer
// cancels with no install POST. The "Cancelled." message is the same
// surface as the legacy preview-flow decline so users get a uniform
// terminal experience.
func TestRunConnectorInstall_HubFlowDeclinedShortCircuits(t *testing.T) {
	installCalled := false
	hubInstallServer(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		w.WriteHeader(http.StatusCreated)
	})
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("n\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if installCalled {
		t.Error("install POST should not have been called after `n` answer")
	}
	if !strings.Contains(stdout.String(), "Cancelled") {
		t.Errorf("stdout missing Cancelled:\n%s", stdout.String())
	}
}

// TestRunConnectorInstall_HubFlowDetailsExpandsThenInstalls: typing
// `d` re-renders with the full publisher footprint and any further
// risk lines, then prompts again. A subsequent `y` confirms.
func TestRunConnectorInstall_HubFlowDetailsExpandsThenInstalls(t *testing.T) {
	hubInstallServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, installDecisionBody(
				"github://acme/x", "sha256:fingerprintAAAAAAAAAAA", "unknown",
				[]string{"First connector by this publisher", "Second risk line"},
				[]string{"github://acme/y", "github://acme/z"}))
		}, nil, nil)
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("d\ny\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"github://acme/y", "github://acme/z", // publisher footprint shown only in details
		"Second risk line",                   // additional risk shown only in details
		"Other connectors by this publisher", // details header
		"Installed:",                         // install proceeded after details + y
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// TestRunConnectorInstall_HubFlowConflictRendersRed: conflict trust
// state means a sibling repo by the same publisher carries a
// different key. Render in red so the operator notices before
// approving. The risk indicator also flips to the red marker.
func TestRunConnectorInstall_HubFlowConflictRendersRed(t *testing.T) {
	hubInstallServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, installDecisionBody(
				"github://acme/x", "sha256:fingerprintAAAAAAAAAAA", "conflict",
				[]string{"Key fingerprint differs from one you trust for github://acme/y"},
				[]string{"github://acme/y"}))
		}, nil, nil)
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("n\n")
	_ = runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	out := stdout.String()
	if !strings.Contains(out, "\033[31m") {
		t.Errorf("conflict trust state should render in red (ANSI 31):\n%s", out)
	}
	if !strings.Contains(out, "conflict") {
		t.Errorf("conflict label not surfaced:\n%s", out)
	}
}

// TestRunConnectorInstall_HubFlowAlreadyTrustedRendersGreen: when the
// publisher key already lives in the keyring at this FQN, render in
// green so the operator sees consent has already been given. Install
// still goes through with confirmed_fingerprint — daemon-side trust
// write is idempotent.
func TestRunConnectorInstall_HubFlowAlreadyTrustedRendersGreen(t *testing.T) {
	hubInstallServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, installDecisionBody(
				"github://acme/x", "sha256:fingerprintAAAAAAAAAAA", "already_trusted",
				nil, nil))
		}, nil, nil)
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("y\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "\033[32m") {
		t.Errorf("already_trusted should render in green (ANSI 32):\n%s", stdout.String())
	}
}

// TestRunConnectorInstall_HubMissingFallsBackToPreview: a 404 from
// /v1/hub/install-decision means the FQN is not in the Hub. The CLI
// falls back to the legacy preview-then-install flow without surfacing
// any error — pre-Hub connectors (private repos) keep working the same
// way they did before #488.
func TestRunConnectorInstall_HubMissingFallsBackToPreview(t *testing.T) {
	var previewCalled, installCalled bool
	hubInstallServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		func(w http.ResponseWriter, r *http.Request) {
			previewCalled = true
			_, _ = io.WriteString(w, previewBody("github://acme/x", "1.0.0", "sha256:abc"))
		},
		func(w http.ResponseWriter, r *http.Request) {
			installCalled = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","entry_dir":"/path"}`)
		})
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("y\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !previewCalled {
		t.Error("preview should have been called when Hub returns 404")
	}
	if !installCalled {
		t.Error("install should have been called after preview confirm")
	}
}

// TestRunConnectorInstall_HubServiceUnavailableFallsBack: a 503 from
// /v1/hub/install-decision (hub_disabled or hub_unreachable) emits a
// one-line stderr note and falls back to the legacy preview flow so
// users with broken Hub config can still install pre-trusted
// connectors. Pre-#488 trust must already be in place for the legacy
// path to succeed.
func TestRunConnectorInstall_HubServiceUnavailableFallsBack(t *testing.T) {
	var previewCalled bool
	hubInstallServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"hub_unreachable","message":"clone failed"}}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			previewCalled = true
			_, _ = io.WriteString(w, previewBody("github://acme/x", "1.0.0", "sha256:abc"))
		},
		nil)
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("y\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !previewCalled {
		t.Error("preview should have been called when Hub returns 503")
	}
	if !strings.Contains(stderr.String(), "hub install-decision returned 503") {
		t.Errorf("expected note about Hub fall-back, got: %s", stderr.String())
	}
}

// TestRunConnectorInstall_HubFlowInstallServerErrorSurfaces: when the
// install endpoint rejects the confirmed fingerprint (e.g., the daemon
// looked up the publisher key and its fingerprint doesn't match the
// one the operator confirmed), the CLI surfaces the status + body to
// stderr and exits non-zero. No silent fall-through.
func TestRunConnectorInstall_HubFlowInstallServerErrorSurfaces(t *testing.T) {
	hubInstallServer(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"fingerprint_mismatch","message":"publisher key fingerprint changed"}}`)
	})
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("y\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code == 0 {
		t.Errorf("expected nonzero exit on install 422; stdout=%s", stdout.String())
	}
	for _, want := range []string{"422", "fingerprint_mismatch"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

// TestRunConnectorInstall_HubFlowAlreadyInstalledVerb: when the daemon
// returns 200 with already_installed=true (Hub-confirmed reinstall of
// an offline-cached hash), the CLI renders "Already installed:" rather
// than "Installed:" so the operator sees that nothing new hit the
// store.
func TestRunConnectorInstall_HubFlowAlreadyInstalledVerb(t *testing.T) {
	hubInstallServer(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","entry_dir":"/path","already_installed":true}`)
	})
	var stdout, stderr bytes.Buffer
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0", "--yes"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Already installed:") {
		t.Errorf("stdout missing 'Already installed:' verb:\n%s", stdout.String())
	}
}

// TestRunConnectorInstall_HubMalformedDecisionFallsBack: a 200 with
// unparseable body shouldn't crash or mislead — emit a one-line note
// and fall back to the legacy preview flow so users with a flaky Hub
// proxy can still install pre-trusted connectors.
func TestRunConnectorInstall_HubMalformedDecisionFallsBack(t *testing.T) {
	var previewCalled bool
	hubInstallServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{not valid json`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			previewCalled = true
			_, _ = io.WriteString(w, previewBody("github://acme/x", "1.0.0", "sha256:abc"))
		},
		nil)
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("y\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !previewCalled {
		t.Error("preview should have been called after malformed Hub response")
	}
	if !strings.Contains(stderr.String(), "could not parse hub install-decision") {
		t.Errorf("expected parse-failure note in stderr, got: %s", stderr.String())
	}
}

// TestRunConnectorInstall_HubFlowSecondDetailsAnswerCancels: typing
// `d` a second time (after the verbose render has already shown
// everything) is treated as cancel rather than looping forever. The
// loop has a documented two-state contract — there is no third level
// of detail to expand to.
func TestRunConnectorInstall_HubFlowSecondDetailsAnswerCancels(t *testing.T) {
	installCalled := false
	hubInstallServer(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		w.WriteHeader(http.StatusCreated)
	})
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("d\nd\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if installCalled {
		t.Error("install should not have been called after two `d` answers")
	}
	if !strings.Contains(stdout.String(), "Cancelled") {
		t.Errorf("expected Cancelled message, got:\n%s", stdout.String())
	}
}

// TestRunConnectorInstall_HubFlowInvalidAnswerCancels: an answer that
// isn't y/n/d (e.g., "maybe") falls into the default branch and
// cancels. Mirrors the legacy preview-flow behavior where anything
// other than `y` is treated as `no`. Prevents typos from accidentally
// confirming an install.
func TestRunConnectorInstall_HubFlowInvalidAnswerCancels(t *testing.T) {
	installCalled := false
	hubInstallServer(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		w.WriteHeader(http.StatusCreated)
	})
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("maybe\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if installCalled {
		t.Error("install should not have been called for an invalid answer")
	}
}

// TestRunConnectorInstall_HubFlowYesWordAlsoAccepted: full-word `yes`
// is equivalent to `y`. Common shell habit; explicit so the prompt
// doesn't quietly reject the word the user typed.
func TestRunConnectorInstall_HubFlowYesWordAlsoAccepted(t *testing.T) {
	installCalled := false
	hubInstallServer(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","entry_dir":"/path"}`)
	})
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("yes\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !installCalled {
		t.Error("install should have been called for `yes` answer")
	}
}

// TestRunConnectorInstall_HubDecisionURLEncodesFQN: FQNs carry `://`
// and `/`, which the daemon's mux can't match in a single path
// segment. The CLI must URL-encode the fqn into the query string for
// /v1/hub/install-decision so the daemon sees the original value.
func TestRunConnectorInstall_HubDecisionURLEncodesFQN(t *testing.T) {
	var seenQuery url.Values
	hubInstallServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			seenQuery = r.URL.Query()
			_, _ = io.WriteString(w, installDecisionBody(
				"github://acme/x", "sha256:fingerprintAAAAAAAAAAA", "unknown", nil, nil))
		}, nil, nil)
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("n\n")
	_ = runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if got := seenQuery.Get("fqn"); got != "github://acme/x" {
		t.Errorf("fqn query = %q, want %q", got, "github://acme/x")
	}
}
