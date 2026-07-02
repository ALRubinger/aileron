package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Tests for the #1803 fix: a suite whose actions share one credential
// capability gets exactly one bind offer per add-suite run, batched at
// the suite tail next to the #741 stale-binding reauthorize prompt,
// instead of one identical offer per action. The single-action inline
// behavior (covered by TestRunAction_AddAutoPromptsForUnboundCapabilities
// and friends in main_test.go) is unchanged.

// unboundSuiteServer stands up a fake daemon for a suite install
// where every action's install response reports the unbound
// capabilities returned by unboundFor(fqn) (a JSON array literal).
// It returns counters for the binding-setup endpoints so tests can
// assert how many setups ran.
func unboundSuiteServer(t *testing.T, unboundFor func(fqn string) string) (initHits, setupHits *int) {
	t.Helper()
	withSeededKeyring(t, "github://acme/conn")
	var inits, setups int
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch r.URL.Path {
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.1.0","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{
				"name":%q,"fqn":%q,"version":"0.1.0","source":"x","path":"/p",
				"unbound_capabilities":%s
			}`, lastSegment(req.FQN), req.FQN, unboundFor(req.FQN))
		case "/bindings/setup/oauth2/init":
			inits++
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"error":{"code":"not_oauth2"}}`)
		case "/bindings/setup":
			setups++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"created":[{"name":"api_key/conn-a/work","kind":"api_key","service":"conn-a","identity":"work","connector_fqn":"github://acme/conn-a","created_at":"2024-01-01T00:00:00Z"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	return &inits, &setups
}

// sharedCapability reports the same (connector, kind, scope) tuple
// for every action in the suite — the shape of the Athena repro in
// #1803 (N actions, one connector, one credential).
func sharedCapability(string) string {
	return `[{"connector_fqn":"github://acme/conn-a","kind":"api_key","scope":"read"}]`
}

const sharedCapabilitySuite = `
name = "shared-cap-suite"
description = "Three actions, one shared credential capability."

actions = [
  "github://acme/conn/actions/foo@0.1.0",
  "github://acme/conn/actions/bar@0.1.0",
  "github://acme/conn/actions/baz@0.1.0",
]
`

// TestRunActionAddSuite_SharedUnboundCapabilityOffersOnce_Accept: the
// headline #1803 regression. Three actions all report the same
// (connector, kind, scope) unbound capability; the run asks "Set up
// now?" exactly once, at the suite tail, and accepting runs binding
// setup exactly once. Pre-fix, the offer fired inside each per-action
// install: three prompts for one capability.
func TestRunActionAddSuite_SharedUnboundCapabilityOffersOnce_Accept(t *testing.T) {
	manifestPath := writeSuiteManifest(t, sharedCapabilitySuite)
	initHits, setupHits := unboundSuiteServer(t, sharedCapability)

	// Suite consent "y", one bind offer "y", identity "work", api_key value "k".
	stdin := strings.NewReader("y\ny\nwork\nk\n")
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{manifestPath}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if got := strings.Count(out, "Set up now?"); got != 1 {
		t.Errorf("expected exactly 1 bind offer for a shared capability, got %d:\n%s", got, out)
	}
	if *initHits != 1 {
		t.Errorf("oauth2/init hits = %d, want 1", *initHits)
	}
	if *setupHits != 1 {
		t.Errorf("bindings/setup hits = %d, want 1", *setupHits)
	}
	if !strings.Contains(out, "Created: api_key/conn-a/work") {
		t.Errorf("stdout missing binding-created line:\n%s", out)
	}
	// The offer must land at the tail, after the last per-action
	// install line — not inline inside an entry.
	lastEntry := strings.LastIndex(out, "── [3/3]")
	offer := strings.Index(out, "Set up now?")
	if lastEntry == -1 || offer < lastEntry {
		t.Errorf("bind offer should follow the last install entry (entry at %d, offer at %d):\n%s", lastEntry, offer, out)
	}
}

// TestRunActionAddSuite_SharedUnboundCapabilityOffersOnce_Decline: a
// declined capability is asked exactly once per run. The decline
// prints the binding-setup hint once and never re-asks; no setup
// endpoint is hit.
func TestRunActionAddSuite_SharedUnboundCapabilityOffersOnce_Decline(t *testing.T) {
	manifestPath := writeSuiteManifest(t, sharedCapabilitySuite)
	initHits, setupHits := unboundSuiteServer(t, sharedCapability)

	// Suite consent "y", decline the single bind offer.
	stdin := strings.NewReader("y\nn\n")
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{manifestPath}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if got := strings.Count(out, "Set up now?"); got != 1 {
		t.Errorf("expected exactly 1 bind offer, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "Skipped. Run `aileron binding setup github://acme/conn-a` later."); got != 1 {
		t.Errorf("expected the decline hint exactly once, got %d:\n%s", got, out)
	}
	if *initHits != 0 || *setupHits != 0 {
		t.Errorf("decline must not call binding setup (init=%d setup=%d)", *initHits, *setupHits)
	}
}

// TestRunActionAddSuite_NoBindPrintsDedupedCapabilityListOnce:
// --no-bind suppresses the offer but must still surface the signal —
// once, over the deduplicated set, at the suite tail. Pre-fix the
// list printed inside every per-action install.
func TestRunActionAddSuite_NoBindPrintsDedupedCapabilityListOnce(t *testing.T) {
	manifestPath := writeSuiteManifest(t, sharedCapabilitySuite)
	initHits, setupHits := unboundSuiteServer(t, sharedCapability)

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{"--yes", "--no-bind", manifestPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if got := strings.Count(out, "Action has unbound credential capabilities:"); got != 1 {
		t.Errorf("expected the unbound list header exactly once, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "- github://acme/conn-a (api_key)"); got != 1 {
		t.Errorf("expected the capability listed exactly once, got %d:\n%s", got, out)
	}
	if strings.Contains(out, "Set up now?") {
		t.Errorf("--no-bind must not prompt:\n%s", out)
	}
	if *initHits != 0 || *setupHits != 0 {
		t.Errorf("--no-bind must not call binding setup (init=%d setup=%d)", *initHits, *setupHits)
	}
}

// TestRunActionAddSuite_DistinctCapabilitiesEachGetOneOffer: the
// dedup key is (connector_fqn, kind, scope) — a different connector
// or kind is a different conversation and survives the collapse.
// foo and bar share conn-a/api_key (collapses to one), baz adds
// conn-b/oauth2 (its own offer): two offers total, in declaration
// order.
func TestRunActionAddSuite_DistinctCapabilitiesEachGetOneOffer(t *testing.T) {
	manifestPath := writeSuiteManifest(t, sharedCapabilitySuite)
	initHits, setupHits := unboundSuiteServer(t, func(fqn string) string {
		if lastSegment(fqn) == "baz" {
			return `[{"connector_fqn":"github://acme/conn-a","kind":"api_key","scope":"read"},
				{"connector_fqn":"github://acme/conn-b","kind":"oauth2"}]`
		}
		return sharedCapability(fqn)
	})

	// Suite consent "y", decline both offers — this test pins the
	// offer count and identity, not the setup flow.
	stdin := strings.NewReader("y\nn\nn\n")
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{manifestPath}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if got := strings.Count(out, "Set up now?"); got != 2 {
		t.Errorf("expected exactly 2 bind offers for 2 distinct capabilities, got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "needs api_key — read access for github://acme/conn-a") {
		t.Errorf("missing conn-a offer:\n%s", out)
	}
	if !strings.Contains(out, "needs oauth2 access for github://acme/conn-b") {
		t.Errorf("missing conn-b offer:\n%s", out)
	}
	if *initHits != 0 || *setupHits != 0 {
		t.Errorf("declines must not call binding setup (init=%d setup=%d)", *initHits, *setupHits)
	}
}

// TestPromptBindUnboundCapabilities_EmptyListIsNoOp: the canonical
// happy path is "everything already bound"; the helper must not emit
// any output when there is nothing to offer.
func TestPromptBindUnboundCapabilities_EmptyListIsNoOp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	promptBindUnboundCapabilities(nil, false, strings.NewReader(""), &stdout, &stderr)
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("empty list must be silent; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
