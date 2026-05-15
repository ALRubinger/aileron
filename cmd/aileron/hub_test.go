package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// twoHubConnectorsJSON is the canonical connectors list payload used
// by tests that don't care about specific entry contents beyond there
// being two of them.
const twoHubConnectorsJSON = `{"connectors":[
	{"fqn":"github://alice/a","description":"A connector","publisher_github":"alice","key_url":"https://example.com/alice.pub","release_pattern":"v*"},
	{"fqn":"github://bob/b","description":"B connector","publisher_github":"bob","key_url":"https://example.com/bob.pub","release_pattern":"v*"}
]}`

const twoHubActionsJSON = `{"actions":[
	{"fqn":"github://alice/a/actions/draft-email","description":"Draft a Gmail","publisher_github":"alice","connector_fqn":"github://alice/a","intents":["draft email"],"category":"communication"},
	{"fqn":"github://bob/b/actions/post-slack","description":"Post a Slack message","publisher_github":"bob","connector_fqn":"github://bob/b"}
]}`

const twoHubSuitesJSON = `{"suites":[
	{"fqn":"github://alice/a/suite","description":"Alice's bundle","publisher_github":"alice","member_actions":["github://alice/a/actions/x","github://alice/a/actions/y"],"connectors_required":["github://alice/a"],"category":"communication"},
	{"fqn":"github://bob/b/suite","description":"Bob's bundle","publisher_github":"bob","member_actions":["github://bob/b/actions/z"]}
]}`

const emptyHubConnectorsJSON = `{"connectors":[]}`
const emptyHubActionsJSON = `{"actions":[]}`
const emptyHubSuitesJSON = `{"suites":[]}`

// hubMultiFixture returns a fakeBindingServer handler that dispatches
// on r.URL.Path, returning the supplied body for each catalog path.
// An empty body for a path means that endpoint returns `[]`-equivalent
// empty-payload JSON.
func hubMultiFixture(t *testing.T, connectorsBody, actionsBody, suitesBody string) http.HandlerFunc {
	t.Helper()
	if connectorsBody == "" {
		connectorsBody = emptyHubConnectorsJSON
	}
	if actionsBody == "" {
		actionsBody = emptyHubActionsJSON
	}
	if suitesBody == "" {
		suitesBody = emptyHubSuitesJSON
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/hub/connectors":
			_, _ = io.WriteString(w, connectorsBody)
		case "/hub/actions":
			_, _ = io.WriteString(w, actionsBody)
		case "/hub/suites":
			_, _ = io.WriteString(w, suitesBody)
		default:
			t.Errorf("unexpected path: %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

// TestRunHub_NoSubcommandPrintsUsage: bare `aileron hub` returns
// non-zero and prints usage. Mirrors the keyring/binding shape.
func TestRunHub_NoSubcommandPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for missing subcommand")
	}
	for _, want := range []string{"list", "search", "show"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("usage missing %q:\n%s", want, stderr.String())
		}
	}
}

// TestRunHub_UnknownSubcommand: a typo lands in the default branch and
// surfaces the legal subcommands so the user can self-correct.
func TestRunHub_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "lst"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for unknown subcommand")
	}
	if !strings.Contains(stderr.String(), "unknown hub subcommand") {
		t.Errorf("stderr missing diagnostic:\n%s", stderr.String())
	}
}

// --- list: combined (default) ---

// TestRunHub_ListCombinedEmpty: bare `aileron hub list` queries all
// three catalogs and prints the friendly empty-state when the Hub
// is empty.
func TestRunHub_ListCombinedEmpty(t *testing.T) {
	fakeBindingServer(t, hubMultiFixture(t, "", "", ""))
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Hub catalog is empty") {
		t.Errorf("missing empty-state message:\n%s", stdout.String())
	}
}

// TestRunHub_ListCombinedShowsAllThreeSections: with entries in every
// catalog, the default `hub list` renders Suites first (per the
// intent-first IA), then Actions, then Providers (connectors).
func TestRunHub_ListCombinedShowsAllThreeSections(t *testing.T) {
	fakeBindingServer(t, hubMultiFixture(t, twoHubConnectorsJSON, twoHubActionsJSON, twoHubSuitesJSON))
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"SUITES", "ACTIONS", "PROVIDERS"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q section:\n%s", want, out)
		}
	}
	suitesIdx := strings.Index(out, "SUITES")
	actionsIdx := strings.Index(out, "ACTIONS")
	providersIdx := strings.Index(out, "PROVIDERS")
	if !(suitesIdx < actionsIdx && actionsIdx < providersIdx) {
		t.Errorf("expected Suites → Actions → Providers order; got positions %d / %d / %d", suitesIdx, actionsIdx, providersIdx)
	}
	for _, want := range []string{
		"github://alice/a/suite", "github://alice/a/actions/draft-email", "github://alice/a",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunHub_ListCombinedJSON: combined `--json` emits a single object
// with `suites`, `actions`, and `connectors` keys.
func TestRunHub_ListCombinedJSON(t *testing.T) {
	fakeBindingServer(t, hubMultiFixture(t, twoHubConnectorsJSON, twoHubActionsJSON, twoHubSuitesJSON))
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	var got struct {
		Suites     []hubSuiteEntry     `json:"suites"`
		Actions    []hubActionEntry    `json:"actions"`
		Connectors []hubConnectorEntry `json:"connectors"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout not JSON-decodable: %v\n%s", err, stdout.String())
	}
	if len(got.Suites) != 2 || len(got.Actions) != 2 || len(got.Connectors) != 2 {
		t.Errorf("expected 2 of each type, got suites=%d actions=%d connectors=%d", len(got.Suites), len(got.Actions), len(got.Connectors))
	}
}

// TestRunHub_ListUnknownCategoryReturnsError: a typo'd catalog name
// surfaces a clear error rather than silently doing what the
// no-arg case does.
func TestRunHub_ListUnknownCategoryReturnsError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "calendar"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for unknown catalog")
	}
	if !strings.Contains(stderr.String(), "unknown catalog") {
		t.Errorf("stderr missing diagnostic:\n%s", stderr.String())
	}
}

// --- list: per-catalog ---

// TestRunHub_ListConnectors: `hub list connectors` queries only the
// connectors endpoint and renders the classic FQN/PUBLISHER/DESCRIPTION
// table.
func TestRunHub_ListConnectors(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hub/connectors" {
			t.Errorf("path = %q, want /hub/connectors", r.URL.Path)
		}
		_, _ = io.WriteString(w, twoHubConnectorsJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "connectors"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"FQN", "PUBLISHER", "DESCRIPTION", "github://alice/a", "A connector"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunHub_ListConnectorsEmpty: empty connectors catalog prints the
// friendly empty-state and does not poison the human output with
// blank lines or stray formatting.
func TestRunHub_ListConnectorsEmpty(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, emptyHubConnectorsJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "connectors"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No connectors published") {
		t.Errorf("missing empty-state message:\n%s", stdout.String())
	}
}

// TestRunHub_ListConnectorsNDJSON: single-type `--json` emits one entry
// per line so the output streams cleanly through `jq -c`.
func TestRunHub_ListConnectorsNDJSON(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, twoHubConnectorsJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "connectors", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), stdout.String())
	}
	for _, line := range lines {
		var e hubConnectorEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("line %q is not a hubConnectorEntry: %v", line, err)
		}
	}
}

// TestRunHub_ListActions: `hub list actions` queries only the actions
// endpoint.
func TestRunHub_ListActions(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hub/actions" {
			t.Errorf("path = %q, want /hub/actions", r.URL.Path)
		}
		_, _ = io.WriteString(w, twoHubActionsJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "actions"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"github://alice/a/actions/draft-email", "Draft a Gmail", "alice"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunHub_ListSuites: `hub list suites` queries only the suites
// endpoint and prints the action-count column.
func TestRunHub_ListSuites(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hub/suites" {
			t.Errorf("path = %q, want /hub/suites", r.URL.Path)
		}
		_, _ = io.WriteString(w, twoHubSuitesJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "suites"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"github://alice/a/suite", "ACTIONS", "Alice's bundle"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunHub_ListConnectorsServerError: a non-200 from the daemon
// surfaces both the status code and the response body to stderr so the
// operator can distinguish "hub disabled" from "hub unreachable"
// without re-running with extra flags.
func TestRunHub_ListConnectorsServerError(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"hub_unreachable","message":"clone failed"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "connectors"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on 503")
	}
	if !strings.Contains(stderr.String(), "503") || !strings.Contains(stderr.String(), "hub_unreachable") {
		t.Errorf("stderr missing status or upstream code:\n%s", stderr.String())
	}
}

// TestRunHub_ListActionsEmpty / TestRunHub_ListSuitesEmpty: empty
// catalogs print a catalog-specific empty-state.
func TestRunHub_ListActionsEmpty(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, emptyHubActionsJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "actions"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No actions published") {
		t.Errorf("missing actions empty-state:\n%s", stdout.String())
	}
}

func TestRunHub_ListSuitesEmpty(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, emptyHubSuitesJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "suites"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No suites published") {
		t.Errorf("missing suites empty-state:\n%s", stdout.String())
	}
}

// TestRunHub_ListActionsNDJSON / TestRunHub_ListSuitesNDJSON: scripts
// piping into jq -c need one JSON object per line.
func TestRunHub_ListActionsNDJSON(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, twoHubActionsJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "actions", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), stdout.String())
	}
	for _, line := range lines {
		var e hubActionEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("line %q is not a hubActionEntry: %v", line, err)
		}
		if e.ConnectorFQN == "" {
			t.Errorf("connector_fqn missing in NDJSON:\n%s", line)
		}
	}
}

func TestRunHub_ListSuitesNDJSON(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, twoHubSuitesJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "suites", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), stdout.String())
	}
	for _, line := range lines {
		var e hubSuiteEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("line %q is not a hubSuiteEntry: %v", line, err)
		}
	}
}

// TestRunHub_ListActionsServerError / TestRunHub_ListSuitesServerError:
// non-200 from the daemon surfaces both the status and the upstream
// error code so the operator can distinguish hub-disabled from
// hub-unreachable.
func TestRunHub_ListActionsServerError(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"hub_unreachable","message":"clone failed"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "actions"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on 503")
	}
	if !strings.Contains(stderr.String(), "503") {
		t.Errorf("stderr missing status:\n%s", stderr.String())
	}
}

func TestRunHub_ListSuitesServerError(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"hub_unreachable","message":"clone failed"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "suites"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on 503")
	}
	if !strings.Contains(stderr.String(), "503") {
		t.Errorf("stderr missing status:\n%s", stderr.String())
	}
}

// --- search ---

// TestRunHub_SearchCrossTypePropagatesQueryToAllEndpoints: bare
// `hub search <q>` queries connectors, actions, AND suites — each with
// the same `q` parameter.
func TestRunHub_SearchCrossTypePropagatesQueryToAllEndpoints(t *testing.T) {
	seen := map[string]string{}
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.URL.Query().Get("q")
		switch r.URL.Path {
		case "/hub/connectors":
			_, _ = io.WriteString(w, emptyHubConnectorsJSON)
		case "/hub/actions":
			_, _ = io.WriteString(w, emptyHubActionsJSON)
		case "/hub/suites":
			_, _ = io.WriteString(w, emptyHubSuitesJSON)
		default:
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "search", "calendar"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	for _, path := range []string{"/hub/connectors", "/hub/actions", "/hub/suites"} {
		if seen[path] != "calendar" {
			t.Errorf("expected q=calendar at %s; got %q (visited=%v)", path, seen[path], seen)
		}
	}
}

// TestRunHub_SearchCrossTypeEmptyEchoesQuery: when nothing matches in
// any catalog, the empty-state echoes the user's query.
func TestRunHub_SearchCrossTypeEmptyEchoesQuery(t *testing.T) {
	fakeBindingServer(t, hubMultiFixture(t, "", "", ""))
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "search", "zzz"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"zzz"`) {
		t.Errorf("empty-state should echo query, got:\n%s", stdout.String())
	}
}

// TestRunHub_SearchCrossTypeGroupsResults: when results land in
// multiple catalogs the rendered output sections by type.
func TestRunHub_SearchCrossTypeGroupsResults(t *testing.T) {
	fakeBindingServer(t, hubMultiFixture(t, twoHubConnectorsJSON, twoHubActionsJSON, twoHubSuitesJSON))
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "search", "anything"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"SUITES", "ACTIONS", "PROVIDERS"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q section:\n%s", want, out)
		}
	}
}

// TestRunHub_SearchTypeFilterRestrictsToOneCatalog: `--type actions`
// hits ONLY the actions endpoint and renders the action table.
func TestRunHub_SearchTypeFilterRestrictsToOneCatalog(t *testing.T) {
	hits := 0
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/hub/actions" {
			t.Errorf("expected only /hub/actions, got %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, twoHubActionsJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "search", "draft", "--type", "actions"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if hits != 1 {
		t.Errorf("expected exactly 1 endpoint hit, got %d", hits)
	}
}

// TestRunHub_SearchTypeFilterUnknownReturnsError: unknown --type values
// fail loudly. Mirrors the connectors/actions/suites whitelist on
// `hub list` and `hub show`.
func TestRunHub_SearchTypeFilterUnknownReturnsError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "search", "x", "--type", "actoins"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for unknown --type")
	}
	if !strings.Contains(stderr.String(), "unknown --type") {
		t.Errorf("stderr missing diagnostic:\n%s", stderr.String())
	}
}

// TestRunHub_SearchRequiresQuery: the search subcommand must reject
// invocations with no positional argument or an all-whitespace one;
// otherwise `hub search` would be indistinguishable from `hub list`
// but silently emit an unfiltered result.
func TestRunHub_SearchRequiresQuery(t *testing.T) {
	for _, args := range [][]string{
		{"hub", "search"},
		{"hub", "search", "  "},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, newTestRegistry(), &stdout, &stderr)
		if code == 0 {
			t.Errorf("args %v: expected nonzero exit; stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

// --- show ---

// TestRunHub_ShowDispatchHitsConnectorFirst: bare `hub show <fqn>`
// tries the connector endpoint first. A 200 from connectors short-
// circuits the dispatch.
func TestRunHub_ShowDispatchHitsConnectorFirst(t *testing.T) {
	hits := []string{}
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/hub/connector":
			_, _ = io.WriteString(w, `{"fqn":"github://alice/a","description":"A connector","publisher_github":"alice","key_url":"https://example.com/alice.pub","release_pattern":"v*"}`)
		default:
			http.NotFound(w, r)
		}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://alice/a"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if len(hits) != 1 || hits[0] != "/hub/connector" {
		t.Errorf("expected dispatch to stop at /hub/connector after first hit; visited %v", hits)
	}
	if !strings.Contains(stdout.String(), "Kind:            connector") {
		t.Errorf("output missing connector kind header:\n%s", stdout.String())
	}
}

// TestRunHub_ShowDispatchFallsThroughToAction: when connector 404s,
// the dispatcher tries actions next.
func TestRunHub_ShowDispatchFallsThroughToAction(t *testing.T) {
	hits := []string{}
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/hub/connector":
			http.NotFound(w, r)
		case "/hub/action":
			_, _ = io.WriteString(w, `{"fqn":"github://alice/a/actions/draft","description":"D","publisher_github":"alice","connector_fqn":"github://alice/a"}`)
		default:
			http.NotFound(w, r)
		}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://alice/a/actions/draft"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if len(hits) < 2 || hits[0] != "/hub/connector" || hits[1] != "/hub/action" {
		t.Errorf("expected dispatch order connector → action; visited %v", hits)
	}
	if !strings.Contains(stdout.String(), "Kind:           action") {
		t.Errorf("output missing action kind header:\n%s", stdout.String())
	}
}

// TestRunHub_ShowDispatchFallsThroughToSuite: when connector and action
// both 404, the dispatcher tries suites last.
func TestRunHub_ShowDispatchFallsThroughToSuite(t *testing.T) {
	hits := []string{}
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/hub/connector", "/hub/action":
			http.NotFound(w, r)
		case "/hub/suite":
			_, _ = io.WriteString(w, `{"fqn":"github://alice/a/suite","description":"S","publisher_github":"alice","member_actions":["github://alice/a/actions/x"]}`)
		default:
			http.NotFound(w, r)
		}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://alice/a/suite"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if len(hits) != 3 || hits[0] != "/hub/connector" || hits[1] != "/hub/action" || hits[2] != "/hub/suite" {
		t.Errorf("expected dispatch order connector → action → suite; visited %v", hits)
	}
	if !strings.Contains(stdout.String(), "Kind:                suite") {
		t.Errorf("output missing suite kind header:\n%s", stdout.String())
	}
}

// TestRunHub_ShowDispatchAllNotFoundReturnsError: when none of the
// three catalogs has the FQN, the error mentions both the FQN and
// that no catalog matched.
func TestRunHub_ShowDispatchAllNotFoundReturnsError(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://nobody/nothing"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on all-404")
	}
	if !strings.Contains(stderr.String(), "github://nobody/nothing") {
		t.Errorf("stderr should echo the requested FQN:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "any catalog") {
		t.Errorf("stderr should mention all-catalog search exhausted:\n%s", stderr.String())
	}
}

// TestRunHub_ShowConnectorPropagatesFQN: `hub show <fqn> --type
// connectors` queries only the connector endpoint and URL-encodes the
// FQN's `://` and `/`.
func TestRunHub_ShowConnectorPropagatesFQN(t *testing.T) {
	var seenQuery url.Values
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hub/connector" {
			t.Errorf("path = %q, want /hub/connector", r.URL.Path)
		}
		seenQuery = r.URL.Query()
		_, _ = io.WriteString(w, `{"fqn":"github://alice/a","description":"A connector","publisher_github":"alice","key_url":"https://example.com/alice.pub","release_pattern":"v*"}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://alice/a", "--type", "connectors"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if got := seenQuery.Get("fqn"); got != "github://alice/a" {
		t.Errorf("fqn query = %q, want %q", got, "github://alice/a")
	}
	for _, want := range []string{"github://alice/a", "alice", "A connector", "v*"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %q:\n%s", want, stdout.String())
		}
	}
}

// TestRunHub_ShowActionRendersIntentsAndCategory: `--type actions`
// hits the actions endpoint and renders intents + category alongside
// the connector dependency.
func TestRunHub_ShowActionRendersIntentsAndCategory(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hub/action" {
			t.Errorf("path = %q, want /hub/action", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"fqn":"github://alice/a/actions/draft-email","description":"Draft a Gmail","publisher_github":"alice","connector_fqn":"github://alice/a","intents":["draft email","compose gmail"],"category":"communication"}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://alice/a/actions/draft-email", "--type", "actions"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"action", "github://alice/a/actions/draft-email", "Connector:      github://alice/a", "draft email", "compose gmail", "communication"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunHub_ShowSuiteRendersMembersAndConnectors: `--type suites`
// hits the suites endpoint and prints both the member-action list and
// the connectors_required closure.
func TestRunHub_ShowSuiteRendersMembersAndConnectors(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hub/suite" {
			t.Errorf("path = %q, want /hub/suite", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"fqn":"github://alice/a/suite","description":"Alice's bundle","publisher_github":"alice","member_actions":["github://alice/a/actions/x","github://alice/a/actions/y"],"connectors_required":["github://alice/a"],"category":"communication"}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://alice/a/suite", "--type", "suites"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Kind:                suite", "Member actions (2)", "github://alice/a/actions/x", "Connectors required (1)", "github://alice/a"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunHub_ShowTypedSuiteNotFoundReturnsError: explicit --type suites
// with a 404 fails without falling through.
func TestRunHub_ShowTypedSuiteNotFoundReturnsError(t *testing.T) {
	hits := 0
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://nobody/x/suite", "--type", "suites"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on 404")
	}
	if hits != 1 {
		t.Errorf("expected exactly 1 endpoint hit, got %d", hits)
	}
	if !strings.Contains(stderr.String(), "suite") {
		t.Errorf("stderr should mention the suite catalog:\n%s", stderr.String())
	}
}

// TestRunHub_ShowTypedConnectorNotFoundReturnsError: explicit
// --type connectors 404 does not fall through.
func TestRunHub_ShowTypedConnectorNotFoundReturnsError(t *testing.T) {
	hits := 0
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://nobody/x", "--type", "connectors"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on 404")
	}
	if hits != 1 {
		t.Errorf("expected exactly 1 endpoint hit, got %d", hits)
	}
	if !strings.Contains(stderr.String(), "connector") {
		t.Errorf("stderr should mention the connector catalog:\n%s", stderr.String())
	}
}

// TestRunHub_ShowJSONConnector: `hub show --json --type connectors`
// emits the raw entry as a single JSON object that round-trips through
// hubConnectorEntry.
func TestRunHub_ShowJSONConnector(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"fqn":"github://alice/a","description":"A connector","publisher_github":"alice","key_url":"https://example.com/alice.pub","release_pattern":"v*"}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "--type", "connectors", "--json", "github://alice/a"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	var entry hubConnectorEntry
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &entry); err != nil {
		t.Fatalf("stdout not JSON-decodable: %v\n%s", err, stdout.String())
	}
	if entry.FQN != "github://alice/a" {
		t.Errorf("decoded FQN = %q, want %q", entry.FQN, "github://alice/a")
	}
}

// TestRunHub_ShowTypedNotFoundReturnsError: an explicit `--type`
// query that 404s should error rather than fall through to the other
// catalogs (the user opted out of dispatch by specifying --type).
func TestRunHub_ShowTypedNotFoundReturnsError(t *testing.T) {
	hits := 0
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://nobody/nothing/actions/x", "--type", "actions"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on 404")
	}
	if hits != 1 {
		t.Errorf("expected exactly 1 endpoint hit (no fallthrough), got %d", hits)
	}
	if !strings.Contains(stderr.String(), "action") {
		t.Errorf("stderr should mention the action catalog:\n%s", stderr.String())
	}
}

// TestRunHub_ShowRequiresFQN: a bare `aileron hub show` exits non-zero
// and prints usage. An empty/whitespace FQN argument is rejected too.
func TestRunHub_ShowRequiresFQN(t *testing.T) {
	for _, args := range [][]string{
		{"hub", "show"},
		{"hub", "show", "  "},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, newTestRegistry(), &stdout, &stderr)
		if code == 0 {
			t.Errorf("args %v: expected nonzero exit", args)
		}
	}
}

// TestRunHub_ShowUnknownTypeReturnsError: a typo'd --type value fails
// before any network call.
func TestRunHub_ShowUnknownTypeReturnsError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://x/y", "--type", "actoins"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for unknown --type")
	}
	if !strings.Contains(stderr.String(), "unknown --type") {
		t.Errorf("stderr missing diagnostic:\n%s", stderr.String())
	}
}
