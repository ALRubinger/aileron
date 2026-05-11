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

// twoHubEntriesJSON is the canonical list payload used by tests that
// don't care about specific entry contents beyond there being two.
const twoHubEntriesJSON = `{"connectors":[
	{"fqn":"github://alice/a","description":"A connector","publisher_github":"alice","key_url":"https://example.com/alice.pub","release_pattern":"v*"},
	{"fqn":"github://bob/b","description":"B connector","publisher_github":"bob","key_url":"https://example.com/bob.pub","release_pattern":"v*"}
]}`

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

// TestRunHub_ListEmpty: the empty case prints a friendly message,
// not blank output, so a fresh user knows the call succeeded but the
// Hub has no entries (or none yet visible to their daemon).
func TestRunHub_ListEmpty(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hub/connectors" {
			t.Errorf("path = %q, want /hub/connectors", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("unexpected query: %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"connectors":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No connectors published") {
		t.Errorf("missing empty-state message:\n%s", stdout.String())
	}
}

// TestRunHub_ListJSONEmpty: scripts detect the empty set with a JSON
// parser, not by grepping prose, so the empty `--json` case is `[]`.
func TestRunHub_ListJSONEmpty(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"connectors":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimRight(stdout.String(), "\n"); got != "[]" {
		t.Errorf("stdout = %q, want %q", got, "[]")
	}
}

// TestRunHub_ListShowsTable: the human path renders a table with FQN,
// PUBLISHER, and DESCRIPTION columns. We assert presence of headers
// and row content rather than exact column widths so the rendering
// can evolve without breaking the test.
func TestRunHub_ListShowsTable(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, twoHubEntriesJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"FQN", "PUBLISHER", "DESCRIPTION",
		"github://alice/a", "alice", "A connector",
		"github://bob/b", "bob", "B connector",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunHub_ListJSONIsNDJSON: `--json` emits one JSON-encoded entry
// per line, round-trippable through json.Unmarshal. The shape exactly
// matches the wire shape — no field renaming on the way out.
func TestRunHub_ListJSONIsNDJSON(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, twoHubEntriesJSON)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), stdout.String())
	}
	for _, line := range lines {
		var e hubEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("line %q is not a hubEntry: %v", line, err)
		}
		if e.FQN == "" || e.Description == "" || e.PublisherGithub == "" || e.KeyURL == "" || e.ReleasePattern == "" {
			t.Errorf("decoded entry has empty required field: %+v", e)
		}
	}
}

// TestRunHub_ListServerError: a non-200 from the daemon surfaces both
// the status code and the response body to stderr so the operator can
// distinguish "hub disabled" from "hub unreachable" without re-running
// with extra flags.
func TestRunHub_ListServerError(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"hub_unreachable","message":"clone failed"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on 503")
	}
	if !strings.Contains(stderr.String(), "503") || !strings.Contains(stderr.String(), "hub_unreachable") {
		t.Errorf("stderr missing status or upstream code:\n%s", stderr.String())
	}
}

// TestRunHub_SearchPropagatesQuery: the search subcommand forwards its
// positional argument as the `q` query parameter so the daemon-side
// filter can apply its case-insensitive FQN/description match.
func TestRunHub_SearchPropagatesQuery(t *testing.T) {
	var seenQuery url.Values
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hub/connectors" {
			t.Errorf("path = %q, want /hub/connectors", r.URL.Path)
		}
		seenQuery = r.URL.Query()
		_, _ = io.WriteString(w, `{"connectors":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "search", "calendar"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if got := seenQuery.Get("q"); got != "calendar" {
		t.Errorf("q query = %q, want %q", got, "calendar")
	}
}

// TestRunHub_SearchEmptyResultEchoesQuery: when the daemon returns no
// matches, the human empty-state echoes the user's query so they
// know which keyword produced the empty set.
func TestRunHub_SearchEmptyResultEchoesQuery(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"connectors":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "search", "zzz"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"zzz"`) {
		t.Errorf("empty-state should echo query, got:\n%s", stdout.String())
	}
}

// TestRunHub_SearchRequiresQuery: the search subcommand must reject
// invocations with no positional argument or an all-whitespace one;
// otherwise `hub search` is indistinguishable from `hub list` but
// silently emits an unfiltered result.
func TestRunHub_SearchRequiresQuery(t *testing.T) {
	for _, args := range [][]string{
		{"hub", "search"},
		{"hub", "search", "  "},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, newTestRegistry(), &stdout, &stderr)
		if code == 0 {
			t.Errorf("args %v: expected nonzero exit; stdout=%q stderr=%q",
				args, stdout.String(), stderr.String())
		}
	}
}

// TestRunHub_ShowPropagatesFQN: the show subcommand passes its FQN
// positional as a query parameter, URL-encoding the `://` and `/`
// because Go's mux can't pattern-match those in a path segment.
func TestRunHub_ShowPropagatesFQN(t *testing.T) {
	var seenQuery url.Values
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hub/connector" {
			t.Errorf("path = %q, want /hub/connector", r.URL.Path)
		}
		seenQuery = r.URL.Query()
		_, _ = io.WriteString(w, `{"fqn":"github://alice/a","description":"A connector","publisher_github":"alice","key_url":"https://example.com/alice.pub","release_pattern":"v*"}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://alice/a"}, newTestRegistry(), &stdout, &stderr)
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

// TestRunHub_ShowJSON: `--json` emits the raw entry as a single
// JSON object, round-trippable through json.Unmarshal into hubEntry.
func TestRunHub_ShowJSON(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"fqn":"github://alice/a","description":"A connector","publisher_github":"alice","key_url":"https://example.com/alice.pub","release_pattern":"v*"}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "--json", "github://alice/a"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	var entry hubEntry
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &entry); err != nil {
		t.Fatalf("stdout not JSON-decodable: %v\n%s", err, stdout.String())
	}
	if entry.FQN != "github://alice/a" {
		t.Errorf("decoded FQN = %q, want %q", entry.FQN, "github://alice/a")
	}
}

// TestRunHub_ShowNotFound: 404 from the daemon (no Hub entry for the
// FQN) is surfaced as a clear error referencing the FQN. The user
// gets the same diagnostic whether they typed the wrong owner or the
// wrong repo.
func TestRunHub_ShowNotFound(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"no Hub entry"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "show", "github://nobody/nothing"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on 404")
	}
	if !strings.Contains(stderr.String(), "github://nobody/nothing") {
		t.Errorf("stderr should echo the requested FQN:\n%s", stderr.String())
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

// TestRunHub_ListRejectsPositional: `aileron hub list foo` is almost
// certainly a typo for `aileron hub search foo`. Rejecting it points
// the user at the right subcommand rather than silently doing what
// the unfiltered-list does and discarding the argument.
func TestRunHub_ListRejectsPositional(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"hub", "list", "calendar"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for stray positional")
	}
}
