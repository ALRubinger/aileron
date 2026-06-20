package launch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

// Unit 3 (#1339 / #1347 fold): a LIVE binding round-trip for a non-oauth
// (api-key) credential through prepareAuthSpec → the real daemonClient →
// an httptest daemon. Unlike the fake-daemon EnvBinding tests, this
// exercises the actual HTTP routing: the daemonClient serializes the
// purpose into the request path/query and the httptest daemon keys
// stored secrets by the FULL destination (name + purpose). A misroute to
// the `oauth` slot therefore lands at a different key and fails the test
// rather than going green.
//
// The api-key binding shape is mirrored inline (VaultPath
// agents/claude/apikey, an ANTHROPIC_API_KEY Render, a paste acquirer)
// because the launch package cannot import the agents package (agents
// imports launch). The contract under test is the live daemon round-trip
// and the disjoint-slot guarantee, both of which are package-local to
// launch.

// httptestVaultDaemon is a minimal in-memory daemon keyed by the full
// (name, purpose) destination. It implements exactly the routes the
// daemonClient drives for agent credentials: GET and PUT at
// /v1/vault/agents/{name}/credentials with an optional ?purpose= query
// (absent ⇒ the canonical oauth purpose).
type httptestVaultDaemon struct {
	mu    sync.Mutex
	store map[string]vault.Secret // key: name + "/" + purpose
	gets  []string                // ordered destination keys observed on GET
	puts  []string                // ordered destination keys observed on PUT
}

func newHTTPTestVaultDaemon() *httptestVaultDaemon {
	return &httptestVaultDaemon{store: map[string]vault.Secret{}}
}

func (d *httptestVaultDaemon) key(name, purpose string) string {
	if purpose == "" {
		purpose = defaultAgentCredentialPurpose
	}
	return name + "/" + purpose
}

func (d *httptestVaultDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Parse /v1/vault/agents/{name}/credentials.
		const prefix = "/v1/vault/agents/"
		const suffix = "/credentials"
		if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), suffix)
		purpose := r.URL.Query().Get("purpose") // "" ⇒ canonical oauth
		key := d.key(name, purpose)

		d.mu.Lock()
		defer d.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			d.gets = append(d.gets, key)
			secret, ok := d.store[key]
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"vault_not_found"}}`))
				return
			}
			body := agentCredentialsBody{Value: secret.Value}
			if secret.Metadata.Type != "" {
				body.Metadata = &agentCredentialsMetadataDTO{Type: secret.Metadata.Type}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		case http.MethodPut:
			d.puts = append(d.puts, key)
			var body agentCredentialsBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			secret := vault.Secret{Value: body.Value}
			if body.Metadata != nil {
				secret.Metadata = vault.Metadata{Type: body.Metadata.Type}
			}
			d.store[key] = secret
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// apiKeyPasteAcquirer mirrors claudeAPIKeyHostAcquire's contract: it
// returns an api_key Secret built from a fixed pasted value, with empty
// paste / nil prompter as non-fatal empty Secrets. It is defined inline
// here so the round-trip drives a paste-shaped acquirer through the live
// EnvBinding seam without importing the agents package.
func apiKeyPasteAcquirer(key string) func(context.Context, HostAcquireDeps) (vault.Secret, error) {
	return func(_ context.Context, _ HostAcquireDeps) (vault.Secret, error) {
		if key == "" {
			return vault.Secret{}, nil
		}
		return vault.Secret{Value: []byte(key), Metadata: vault.Metadata{Type: "api_key"}}, nil
	}
}

func anthropicAPIKeyRender(s vault.Secret) (map[string]string, error) {
	key := strings.TrimSpace(string(s.Value))
	if key == "" {
		return nil, errors.New("api key empty")
	}
	return map[string]string{"ANTHROPIC_API_KEY": key}, nil
}

// TestPrepareAuthSpec_APIKeyLiveRoundTrip drives a miss → host-acquire
// seed → live PUT at .../credentials?purpose=apikey → live GET that
// renders ANTHROPIC_API_KEY, all through the real daemonClient and an
// httptest daemon. It asserts the seed landed at the apikey destination
// and that the oauth slot was NEVER written.
func TestPrepareAuthSpec_APIKeyLiveRoundTrip(t *testing.T) {
	daemonSrv := newHTTPTestVaultDaemon()
	srv := daemonSrv.server(t)
	client := newDaemonClient(srv.URL)

	spec := AuthSpec{
		EnvBindings: []EnvBinding{{
			VaultPath:   "agents/claude/apikey",
			Required:    false,
			Render:      anthropicAPIKeyRender,
			HostAcquire: apiKeyPasteAcquirer("sk-ant-live"),
		}},
	}

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, client, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	// The acquired key was rendered into the env.
	if got := prep.EnvAdditions["ANTHROPIC_API_KEY"]; got != "sk-ant-live" {
		t.Errorf("EnvAdditions[ANTHROPIC_API_KEY] = %q, want sk-ant-live", got)
	}
	if !prep.RenderedAnyCredential {
		t.Error("RenderedAnyCredential = false; a successful api-key acquire must set it")
	}

	// The PUT landed at the apikey destination over the live wire.
	daemonSrv.mu.Lock()
	defer daemonSrv.mu.Unlock()
	if len(daemonSrv.puts) != 1 || daemonSrv.puts[0] != "claude/apikey" {
		t.Fatalf("PUTs = %v, want exactly [claude/apikey]", daemonSrv.puts)
	}
	// The oauth slot must never be touched on either GET or PUT.
	for _, k := range daemonSrv.gets {
		if k == "claude/oauth" {
			t.Errorf("GET touched the oauth slot %q during an api-key launch", k)
		}
	}
	for _, k := range daemonSrv.puts {
		if k == "claude/oauth" {
			t.Errorf("PUT wrote the oauth slot %q during an api-key launch", k)
		}
	}
	// The stored secret carries the api_key metadata type, proving the
	// full Secret (not just bytes) survived the live serialization.
	stored, ok := daemonSrv.store["claude/apikey"]
	if !ok {
		t.Fatal("apikey slot empty after seed")
	}
	if stored.Metadata.Type != "api_key" {
		t.Errorf("stored Metadata.Type = %q, want api_key", stored.Metadata.Type)
	}
	if string(stored.Value) != "sk-ant-live" {
		t.Errorf("stored Value = %q, want sk-ant-live", stored.Value)
	}
}

// TestPrepareAuthSpec_APIKeyAndOAuthDistinctDestinations drives a
// two-binding spec (an oauth FileBinding and an apikey EnvBinding for the
// same agent) and asserts each binding's seed lands at its own purpose
// destination over the live wire. This anchors #1338's distinct-slot P0
// at the Claude layer where apikey first becomes real: the same agent,
// two purposes, two physically distinct vault keys.
func TestPrepareAuthSpec_APIKeyAndOAuthDistinctDestinations(t *testing.T) {
	daemonSrv := newHTTPTestVaultDaemon()
	srv := daemonSrv.server(t)
	client := newDaemonClient(srv.URL)

	oauthEnvelope := acquireClaudeEnvelope("oauth-acc", futureExpiresAtMS)
	spec := AuthSpec{
		EnvBindings: []EnvBinding{{
			VaultPath:   "agents/claude/apikey",
			Required:    false,
			Render:      anthropicAPIKeyRender,
			HostAcquire: apiKeyPasteAcquirer("sk-ant-two"),
		}},
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Mode:          0o600,
			Required:      false,
			Render:        claudeShapedRender,
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
			HostAcquire: func(_ context.Context, _ HostAcquireDeps) (vault.Secret, error) {
				return vault.Secret{Value: oauthEnvelope, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}, nil
			},
		}},
	}

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, client, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	daemonSrv.mu.Lock()
	defer daemonSrv.mu.Unlock()

	apikeySlot, okAPI := daemonSrv.store["claude/apikey"]
	oauthSlot, okOAuth := daemonSrv.store["claude/oauth"]
	if !okAPI {
		t.Error("apikey slot empty; the EnvBinding seed did not land at claude/apikey")
	}
	if !okOAuth {
		t.Error("oauth slot empty; the FileBinding seed did not land at claude/oauth")
	}
	if string(apikeySlot.Value) != "sk-ant-two" {
		t.Errorf("apikey slot = %q, want sk-ant-two", apikeySlot.Value)
	}
	if string(oauthSlot.Value) != string(oauthEnvelope) {
		t.Errorf("oauth slot = %q, want the oauth envelope", oauthSlot.Value)
	}
	// The two slots are physically distinct: the api-key value never
	// appears at the oauth destination and vice versa.
	if strings.Contains(string(oauthSlot.Value), "sk-ant-two") {
		t.Error("api-key value leaked into the oauth slot")
	}
}
