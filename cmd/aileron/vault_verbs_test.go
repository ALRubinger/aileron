package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vault put/delete/list verbs are thin daemon-backed HTTP clients
// scoped to the agents/<name>/oauth namespace (ADR-0025, #981). These
// tests pin the contract:
//
//   - put reads --from-file bytes verbatim (no newline munging) and
//     PUTs them base64-encoded to /v1/vault/agents/<name>/credentials.
//   - put/delete reject any non-agents/<name>/oauth path before any HTTP.
//   - delete confirms interactively unless --yes; n/empty cancels with
//     no HTTP call; 404 surfaces a clear message and non-zero exit.
//   - list mirrors `secret list` output (names by default, NDJSON with
//     --json), rejects non-agents/ prefixes, and reports empty.

// fakeVaultServer stands up an httptest server scoped to AILERON_API_URL
// so vaultDoRequest resolves to the fake.
func fakeVaultServer(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/v1", fn))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")
	t.Setenv("AILERON_TOKEN", "test-token")
	return srv
}

func TestRunVaultPut_RoundTripsFileBytesVerbatim(t *testing.T) {
	// Credential files are exact-byte artifacts: a trailing newline in
	// the file must reach the daemon unchanged.
	fileBytes := []byte("{\"claudeAiOauth\":{\"accessToken\":\"tok\"}}\n")
	var received []byte
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/vault/agents/claude/credentials" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body agentCredentialsBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received = body.Value
		w.WriteHeader(http.StatusNoContent)
	})

	dir := t.TempDir()
	credFile := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(credFile, fileBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "agents/claude/oauth", "--from-file", credFile},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !bytes.Equal(received, fileBytes) {
		t.Errorf("server received %q, want %q (verbatim)", received, fileBytes)
	}
	if !strings.Contains(stdout.String(), "Stored agents/claude/oauth") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunVaultPut_RejectsNonAgentPathBeforeHTTP(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	dir := t.TempDir()
	credFile := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(credFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "some/other/path", "--from-file", credFile},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for non-agent path")
	}
	if called {
		t.Errorf("HTTP request issued for a rejected path")
	}
	if !strings.Contains(stderr.String(), "agents/<name>/<purpose>") {
		t.Errorf("stderr = %q; want agents-only constraint message", stderr.String())
	}
}

func TestRunVaultDelete_YesSkipsPromptAndDeletes(t *testing.T) {
	gotDelete := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/vault/agents/codex/credentials" {
			gotDelete = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/codex/oauth", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !gotDelete {
		t.Errorf("DELETE not issued")
	}
	if !strings.Contains(stdout.String(), "Deleted agents/codex/oauth") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// #1317 flips the canonical form: `vault list` now prints the
// fully-qualified agents/<name>/oauth, and delete accepts ONLY that form.
// Regression for the flip — the bare agent name (what #1302/#1310
// accepted) is now rejected before any HTTP, while the fully-qualified
// line `list` emits round-trips straight into delete.
func TestRunVaultDelete_AcceptsFullyQualifiedFromList(t *testing.T) {
	const bare = "codex"                       // the pre-#1317 form, now rejected
	const listed = "agents/" + bare + "/oauth" // exactly what `vault list` prints

	// The bare name is rejected client-side, no DELETE issued.
	bareCalled := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		bareCalled = true
		w.WriteHeader(http.StatusNoContent)
	})
	var bstdout, bstderr bytes.Buffer
	if code := runVault([]string{"delete", bare, "--yes"},
		strings.NewReader(""), &bstdout, &bstderr); code == 0 {
		t.Errorf("bare name %q: exit = 0, want non-zero (bare form no longer accepted)", bare)
	}
	if bareCalled {
		t.Errorf("bare name %q: HTTP issued for rejected form", bare)
	}

	// The fully-qualified line from `list` round-trips into delete.
	gotDelete := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/vault/agents/"+bare+"/credentials" {
			gotDelete = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", listed, "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !gotDelete {
		t.Errorf("DELETE not issued for fully-qualified %q", listed)
	}
	if !strings.Contains(stdout.String(), "Deleted agents/"+bare+"/oauth") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// The list-output == delete-input contract (#1317): every line `vault
// list` emits for an agent must resolve, via the delete validator, back
// to the same agent the daemon keys the DELETE on. list now prints the
// fully-qualified agents/<name>/oauth, so feed exactly that. The bare
// name (the pre-#1317 form) must no longer validate.
func TestVaultListNamesRoundTripIntoDelete(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "my-agent"} {
		// The fully-qualified line list prints is the sole accepted form.
		listed := "agents/" + agent + "/oauth"
		name, purpose, err := agentPathNameAndPurpose(listed)
		if err != nil {
			t.Errorf("agentPathNameAndPurpose(%q) errored: %v; a line from `vault list` must be deletable", listed, err)
			continue
		}
		if name != agent {
			t.Errorf("agentPathNameAndPurpose(%q) name = %q, want %q", listed, name, agent)
		}
		if purpose != "oauth" {
			t.Errorf("agentPathNameAndPurpose(%q) purpose = %q, want oauth", listed, purpose)
		}
		// The bare name is no longer a valid delete input.
		if gotName, gotPurpose, err := agentPathNameAndPurpose(agent); err == nil {
			t.Errorf("agentPathNameAndPurpose(%q) = (%q, %q, nil), want error (bare form no longer accepted)", agent, gotName, gotPurpose)
		}
	}
}

// Guard the namespace lock survives the bare-name acceptance: a bare
// value that is path-shaped (contains a slash) must still be rejected so
// no non-agent key can slip through the new form.
func TestRunVaultDelete_RejectsBarePathShapedName(t *testing.T) {
	for _, arg := range []string{"user/github", "internal/secret", "a/b/c"} {
		called := false
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"delete", arg, "--yes"},
			strings.NewReader(""), &stdout, &stderr)
		if code == 0 {
			t.Errorf("arg %q: exit = 0, want non-zero", arg)
		}
		if called {
			t.Errorf("arg %q: HTTP issued for rejected name", arg)
		}
	}
}

// Guard the namespace lock for the full-path form: a value that carries
// the agents/ prefix and /oauth suffix but extracts to an empty or
// slash-bearing <name> (e.g. `agents//oauth`, `agents/a/b/oauth`) must
// still be rejected, so no nested or empty key can slip through the path
// form (ADR-0025).
func TestAgentPathNameAndPurpose_RejectsMalformedFullPath(t *testing.T) {
	for _, arg := range []string{"agents//oauth", "agents/a/b/oauth", "agents/claude/", "agents/claude"} {
		if name, purpose, err := agentPathNameAndPurpose(arg); err == nil {
			t.Errorf("agentPathNameAndPurpose(%q) = (%q, %q, nil), want error", arg, name, purpose)
		}
	}
}

// The purpose segment is constrained to the daemon's allow-list
// (^[a-z0-9][a-z0-9_-]*$). A path whose purpose violates it must be
// rejected client-side, before any HTTP call.
func TestAgentPathNameAndPurpose_RejectsInvalidPurpose(t *testing.T) {
	for _, arg := range []string{"agents/claude/OAUTH", "agents/claude/-bad", "agents/claude/has space", "agents/claude/api.key"} {
		if name, purpose, err := agentPathNameAndPurpose(arg); err == nil {
			t.Errorf("agentPathNameAndPurpose(%q) = (%q, %q, nil), want error (invalid purpose)", arg, name, purpose)
		}
	}
}

// The apikey purpose (the case #1361 made addressable) parses to its
// name and purpose so it can be put/deleted just like oauth.
func TestAgentPathNameAndPurpose_AcceptsApikey(t *testing.T) {
	name, purpose, err := agentPathNameAndPurpose("agents/claude/apikey")
	if err != nil {
		t.Fatalf("agentPathNameAndPurpose(agents/claude/apikey) errored: %v", err)
	}
	if name != "claude" || purpose != "apikey" {
		t.Errorf("got (%q, %q), want (claude, apikey)", name, purpose)
	}
}

func TestRunVaultDelete_InteractiveCancelMakesNoCall(t *testing.T) {
	for _, answer := range []string{"n\n", "\n"} {
		called := false
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"delete", "agents/claude/oauth"},
			strings.NewReader(answer), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("answer %q: exit = %d, want 0", answer, code)
		}
		if called {
			t.Errorf("answer %q: DELETE issued despite cancel", answer)
		}
		if !strings.Contains(stdout.String(), "cancelled") {
			t.Errorf("answer %q: stdout = %q, want cancelled", answer, stdout.String())
		}
	}
}

func TestRunVaultDelete_InteractiveYesDeletes(t *testing.T) {
	gotDelete := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			gotDelete = true
		}
		w.WriteHeader(http.StatusNoContent)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/claude/oauth"},
		strings.NewReader("y\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !gotDelete {
		t.Errorf("DELETE not issued on 'y'")
	}
}

func TestRunVaultDelete_NotFoundMessage(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"vault_not_found","message":"nope"}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/claude/oauth", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for 404")
	}
	if !strings.Contains(stderr.String(), "no credential entry for agents/claude/oauth") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunVaultList_DefaultAndJSON(t *testing.T) {
	listBody := `{"agents":[{"name":"claude","metadata":{"type":"oauth_refresh_token"}},{"name":"codex"}]}`
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vault/agents" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, listBody)
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Fields(stdout.String())
	if len(lines) != 2 || lines[0] != "agents/claude/oauth" || lines[1] != "agents/codex/oauth" {
		t.Errorf("default output = %q, want fully-qualified agents/<name>/oauth one per line", stdout.String())
	}

	var jout, jerr bytes.Buffer
	code = runVault([]string{"list", "--json"}, strings.NewReader(""), &jout, &jerr)
	if code != 0 {
		t.Fatalf("json exit = %d, want 0; stderr=%s", code, jerr.String())
	}
	jsonLines := strings.Split(strings.TrimSpace(jout.String()), "\n")
	if len(jsonLines) != 2 {
		t.Fatalf("json output = %q, want 2 NDJSON lines", jout.String())
	}
	var first agentSummary
	if err := json.Unmarshal([]byte(jsonLines[0]), &first); err != nil {
		t.Fatalf("NDJSON line 0 not valid JSON: %v", err)
	}
	if first.Name != "claude" {
		t.Errorf("first NDJSON name = %q, want claude", first.Name)
	}
}

// #1361: the daemon is purpose-aware and emits one AgentCredentialSummary
// per (name, purpose). An apikey-only agent must surface in `vault list`
// with its real purpose, not be collapsed to /oauth or dropped.
func TestRunVaultList_ApikeyOnlyAgentSurfaces(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agents":[{"name":"claude","purpose":"apikey","metadata":{"type":"api_key"}}]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "agents/claude/apikey" {
		t.Errorf("stdout = %q, want agents/claude/apikey (apikey-only agent must not be mislabeled oauth)", got)
	}
}

// #1361: an agent that holds both oauth and apikey yields two distinct
// list lines (no duplicate, no collapse) and two NDJSON entries that each
// carry their purpose.
func TestRunVaultList_BothPurposesYieldTwoDistinctEntries(t *testing.T) {
	const body = `{"agents":[{"name":"claude","purpose":"oauth"},{"name":"claude","purpose":"apikey"}]}`
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Fields(stdout.String())
	if len(lines) != 2 || lines[0] != "agents/claude/oauth" || lines[1] != "agents/claude/apikey" {
		t.Errorf("text output = %q, want two distinct lines agents/claude/oauth and agents/claude/apikey", stdout.String())
	}

	var jout, jerr bytes.Buffer
	code = runVault([]string{"list", "--json"}, strings.NewReader(""), &jout, &jerr)
	if code != 0 {
		t.Fatalf("json exit = %d, want 0; stderr=%s", code, jerr.String())
	}
	jsonLines := strings.Split(strings.TrimSpace(jout.String()), "\n")
	if len(jsonLines) != 2 {
		t.Fatalf("json output = %q, want 2 NDJSON lines", jout.String())
	}
	gotPurposes := map[string]bool{}
	for _, line := range jsonLines {
		var s agentSummary
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("NDJSON line not valid: %v (%q)", err, line)
		}
		if s.Name != "claude" {
			t.Errorf("NDJSON name = %q, want claude", s.Name)
		}
		if s.Purpose == nil {
			t.Fatalf("NDJSON entry %q dropped purpose; --json must carry it", line)
		}
		gotPurposes[*s.Purpose] = true
	}
	if !gotPurposes["oauth"] || !gotPurposes["apikey"] {
		t.Errorf("NDJSON purposes = %v, want both oauth and apikey", gotPurposes)
	}
}

// #1361: `vault delete agents/<name>/apikey` must issue the DELETE with
// ?purpose=apikey so the daemon removes the apikey envelope, not oauth.
func TestRunVaultDelete_ThreadsApikeyPurpose(t *testing.T) {
	var gotPurpose string
	gotDelete := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/vault/agents/claude/credentials" {
			gotDelete = true
			gotPurpose = r.URL.Query().Get("purpose")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/claude/apikey", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !gotDelete {
		t.Fatalf("DELETE not issued")
	}
	if gotPurpose != "apikey" {
		t.Errorf("DELETE ?purpose = %q, want apikey", gotPurpose)
	}
	if !strings.Contains(stdout.String(), "Deleted agents/claude/apikey") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// #1361: the oauth path keeps threading ?purpose=oauth — backward-compat
// for the historical form (the daemon defaults to oauth, the CLI is
// explicit).
func TestRunVaultDelete_ThreadsOauthPurpose(t *testing.T) {
	var gotPurpose string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/vault/agents/claude/credentials" {
			gotPurpose = r.URL.Query().Get("purpose")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/claude/oauth", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotPurpose != "oauth" {
		t.Errorf("DELETE ?purpose = %q, want oauth", gotPurpose)
	}
}

// #1361: put threads the purpose into ?purpose= as well, so storing
// agents/<name>/apikey writes the apikey envelope.
func TestRunVaultPut_ThreadsApikeyPurpose(t *testing.T) {
	var gotPurpose string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/vault/agents/claude/credentials" {
			gotPurpose = r.URL.Query().Get("purpose")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
	})
	dir := t.TempDir()
	credFile := filepath.Join(dir, "creds")
	if err := os.WriteFile(credFile, []byte("sk-xxx"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "agents/claude/apikey", "--from-file", credFile},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotPurpose != "apikey" {
		t.Errorf("PUT ?purpose = %q, want apikey", gotPurpose)
	}
	if !strings.Contains(stdout.String(), "Stored agents/claude/apikey") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// #1361 end-to-end: the apikey line `vault list` prints feeds straight
// back into `vault delete`, issuing the DELETE the daemon keys on the
// same (name, purpose).
func TestRunVaultList_ApikeyLineRoundTripsIntoDelete(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agents":[{"name":"claude","purpose":"apikey"}]}`)
	})
	var lout, lerr bytes.Buffer
	if code := runVault([]string{"list"}, strings.NewReader(""), &lout, &lerr); code != 0 {
		t.Fatalf("list exit = %d, want 0; stderr=%s", code, lerr.String())
	}
	listed := strings.TrimSpace(lout.String())
	if listed != "agents/claude/apikey" {
		t.Fatalf("listed = %q, want agents/claude/apikey", listed)
	}

	var gotPurpose string
	gotDelete := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/vault/agents/claude/credentials" {
			gotDelete = true
			gotPurpose = r.URL.Query().Get("purpose")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
	})
	var dout, derr bytes.Buffer
	if code := runVault([]string{"delete", listed, "--yes"}, strings.NewReader(""), &dout, &derr); code != 0 {
		t.Fatalf("delete exit = %d, want 0; stderr=%s", code, derr.String())
	}
	if !gotDelete || gotPurpose != "apikey" {
		t.Errorf("round-trip delete: gotDelete=%v purpose=%q, want true/apikey", gotDelete, gotPurpose)
	}
}

// #1361 backward-compat: an older daemon that omits `purpose` (entries
// predating per-purpose listing) must still render as agents/<name>/oauth.
func TestRunVaultList_NilPurposeRendersOauth(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agents":[{"name":"codex"}]}`)
	})
	var stdout, stderr bytes.Buffer
	if code := runVault([]string{"list"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "agents/codex/oauth" {
		t.Errorf("stdout = %q, want agents/codex/oauth (nil purpose -> oauth)", got)
	}
}

func TestRunVaultList_EmptyMessage(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agents":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No agent credentials stored.") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunVaultList_EmptyJSONIsEmptyArray(t *testing.T) {
	// Mirror `aileron secret list --json`: empty must be parseable JSON.
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agents":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Errorf("stdout = %q, want []", stdout.String())
	}
}

func TestRunVaultDelete_OverlapPathRejectedNoPanic(t *testing.T) {
	// Regression: "agents/oauth" passes both HasPrefix and HasSuffix but
	// the prefix and suffix overlap. The validator must reject it
	// cleanly, not slice out of range.
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/oauth", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for overlap path")
	}
	if called {
		t.Errorf("HTTP issued for rejected overlap path")
	}
}

func TestRunVaultList_RejectsNonAgentsPrefix(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"agents":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--prefix", "internal/"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for non-agents prefix")
	}
	if called {
		t.Errorf("HTTP request issued for a rejected prefix")
	}
	if !strings.Contains(stderr.String(), "agents/") {
		t.Errorf("stderr = %q; want agents/-only message", stderr.String())
	}
}

func TestRunVaultPut_MissingFromFile(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request without --from-file")
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "agents/claude/oauth"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero when --from-file missing")
	}
	if !strings.Contains(stderr.String(), "--from-file") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunVaultPut_EmptyFile(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request for empty file")
	})
	dir := t.TempDir()
	f := filepath.Join(dir, "empty")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "agents/claude/oauth", "--from-file", f},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for empty file")
	}
}

func TestRunVaultPut_LockedAndNoVault(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusLocked, "locked"},
		{http.StatusServiceUnavailable, "no vault"},
	} {
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"put", "agents/claude/oauth", "--from-file", f},
			strings.NewReader(""), &stdout, &stderr)
		if code == 0 {
			t.Errorf("status %d: exit = 0, want non-zero", tc.status)
		}
	}
}

func TestRunVaultDelete_LockedAndNoVault(t *testing.T) {
	for _, status := range []int{http.StatusLocked, http.StatusServiceUnavailable} {
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"delete", "agents/claude/oauth", "--yes"},
			strings.NewReader(""), &stdout, &stderr)
		if code == 0 {
			t.Errorf("status %d: exit = 0, want non-zero", status)
		}
	}
}

func TestRunVaultList_NoVault(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for 503")
	}
	if !strings.Contains(stderr.String(), "vault") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunVaultDelete_RejectsNonAgentPath(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "internal/secret", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for non-agent path")
	}
	if called {
		t.Errorf("HTTP issued for rejected path")
	}
}

func TestRunVault_DispatchUnknownAndEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runVault([]string{"explode"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Errorf("unknown subcommand exit = 0, want non-zero")
	}
	stderr.Reset()
	if code := runVault(nil, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Errorf("empty args exit = 0, want non-zero")
	}
}

// Guards the base64 wire encoding contract: the daemon expects
// `value` as a base64 string (spec `format: byte`), and encoding/json
// produces exactly that for a []byte field.
func TestAgentCredentialsBody_ValueIsBase64(t *testing.T) {
	raw, err := json.Marshal(agentCredentialsBody{Value: []byte("abc")})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Value != base64.StdEncoding.EncodeToString([]byte("abc")) {
		t.Errorf("value = %q, want base64 of abc", wire.Value)
	}
}

// vault list --scope user|all contract (#1180):
//
//   - --scope user routes to GET /vault/user and surfaces service names,
//     never the credential bytes.
//   - --scope all surfaces both agent names and user services.
//   - --scope user --json emits NDJSON keyed by service, and [] when empty.
//   - An invalid --scope is rejected before any HTTP call.
//   - --prefix agents/ together with a user-bearing scope is a usage error.
//   - 503 from /vault/user maps to the daemon-not-configured message.

func TestRunVaultList_ScopeUser(t *testing.T) {
	const secret = "gho_SECRETBYTES"
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vault/user" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"services":[{"service":"github","metadata":{"type":"oauth_access_token"}}]}`)
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "user"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "user/github" {
		t.Errorf("stdout = %q, want user/github", stdout.String())
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stdout.String(), "value") {
		t.Errorf("stdout leaked credential material: %q", stdout.String())
	}
}

func TestRunVaultList_ScopeUserJSON(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"services":[{"service":"github"}]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "user", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	var u userSummary
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &u); err != nil {
		t.Fatalf("NDJSON not valid: %v (%q)", err, stdout.String())
	}
	if u.Service != "github" {
		t.Errorf("service = %q, want github", u.Service)
	}
}

func TestRunVaultList_ScopeUserEmpty(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"services":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "user"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No user credentials stored.") {
		t.Errorf("stdout = %q", stdout.String())
	}

	var jout, jerr bytes.Buffer
	code = runVault([]string{"list", "--scope", "user", "--json"}, strings.NewReader(""), &jout, &jerr)
	if code != 0 {
		t.Fatalf("json exit = %d, want 0", code)
	}
	if strings.TrimSpace(jout.String()) != "[]" {
		t.Errorf("json stdout = %q, want []", jout.String())
	}
}

func TestRunVaultList_ScopeAll(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vault/agents":
			_, _ = io.WriteString(w, `{"agents":[{"name":"claude"}]}`)
		case "/vault/user":
			_, _ = io.WriteString(w, `{"services":[{"service":"github"}]}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "all"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Fields(stdout.String())
	if len(lines) != 2 || lines[0] != "agents/claude/oauth" || lines[1] != "user/github" {
		t.Errorf("output = %q, want agents/claude/oauth then user/github", stdout.String())
	}
}

func TestRunVaultList_InvalidScopeRejectedNoCall(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "bogus"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for invalid scope")
	}
	if called {
		t.Errorf("HTTP issued for rejected scope")
	}
	if !strings.Contains(stderr.String(), "scope") {
		t.Errorf("stderr = %q; want scope error", stderr.String())
	}
}

func TestRunVaultList_PrefixAgentsConflictsWithUserScope(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--prefix", "agents/", "--scope", "all"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for prefix/scope conflict")
	}
	if called {
		t.Errorf("HTTP issued despite conflicting flags")
	}
}

func TestRunVaultList_ScopeUserNoVault(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "user"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for 503")
	}
	if !strings.Contains(stderr.String(), "vault") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
