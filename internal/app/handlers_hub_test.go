package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/hub"
)

func TestListHubConnectors_ReturnsEntries(t *testing.T) {
	url := makeHubFixture(t, twoEntryFixture)
	srv := &apiServer{
		log: slog.Default(),
		hub: &hub.Client{URL: url},
	}
	rec := httptest.NewRecorder()
	srv.ListHubConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/connectors", nil), api.ListHubConnectorsParams{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubConnectorList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Connectors) != 2 {
		t.Fatalf("expected 2 connectors, got %d", len(got.Connectors))
	}
}

func TestListHubConnectors_FilterAppliesToFQNAndDescription(t *testing.T) {
	url := makeHubFixture(t, map[string]string{
		"a.yaml": entryYAML("github://alice/google", "Google Workspace", "alice", "https://example.com/alice.pub"),
		"b.yaml": entryYAML("github://bob/slack", "Slack messaging", "bob", "https://example.com/bob.pub"),
	})
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	q := "google"
	srv.ListHubConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/connectors?q=google", nil), api.ListHubConnectorsParams{Q: &q})

	var got api.HubConnectorList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Connectors) != 1 || got.Connectors[0].Fqn != "github://alice/google" {
		t.Fatalf("expected single google match, got %+v", got.Connectors)
	}
}

func TestListHubConnectors_HubUnreachableReturns503(t *testing.T) {
	srv := &apiServer{
		log: slog.Default(),
		hub: &hub.Client{URL: "file:///nonexistent/hub/path"},
	}
	rec := httptest.NewRecorder()
	srv.ListHubConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/connectors", nil), api.ListHubConnectorsParams{})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestGetHubConnector_ReturnsEntryForKnownFQN(t *testing.T) {
	url := makeHubFixture(t, twoEntryFixture)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	srv.GetHubConnector(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/connector?fqn=github://alice/a", nil), api.GetHubConnectorParams{Fqn: "github://alice/a"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubConnectorEntry
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Fqn != "github://alice/a" {
		t.Fatalf("unexpected FQN: %s", got.Fqn)
	}
}

func TestGetHubConnector_EmptyFQNReturns400(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: makeHubFixture(t, twoEntryFixture)}}
	rec := httptest.NewRecorder()
	srv.GetHubConnector(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/connector", nil), api.GetHubConnectorParams{Fqn: ""})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestGetHubConnector_MissingFQNReturns404(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: makeHubFixture(t, twoEntryFixture)}}
	rec := httptest.NewRecorder()
	srv.GetHubConnector(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/connector?fqn=github://nobody/missing", nil), api.GetHubConnectorParams{Fqn: "github://nobody/missing"})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestGetHubInstallDecision_UnknownTrustState(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyServer := httpKeyServer(t, pub)
	defer keyServer.Close()

	url := makeHubFixture(t, map[string]string{
		"alice_a.yaml": entryYAML("github://alice/a", "A connector", "alice", keyServer.URL+"/publisher.pub"),
	})
	srv := &apiServer{
		log:         slog.Default(),
		hub:         &hub.Client{URL: url, HTTP: keyServer.Client()},
		keyringPath: filepath.Join(t.TempDir(), "keyring.json"), // empty keyring file (doesn't exist)
	}
	rec := httptest.NewRecorder()
	srv.GetHubInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/install-decision?fqn=github://alice/a", nil), api.GetHubInstallDecisionParams{Fqn: "github://alice/a"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubInstallDecision
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TrustState != api.HubTrustStateUnknown {
		t.Fatalf("trust_state = %s, want unknown", got.TrustState)
	}
	if got.Fingerprint != hub.Fingerprint(pub) {
		t.Fatalf("fingerprint = %s, want %s", got.Fingerprint, hub.Fingerprint(pub))
	}
	if len(got.RiskIndicators) == 0 {
		t.Fatalf("expected at least one risk indicator (first-publisher)")
	}
	if !strings.Contains(got.RiskIndicators[0], "First connector by this publisher") {
		t.Fatalf("expected 'First connector by this publisher' indicator, got %q", got.RiskIndicators[0])
	}
}

func TestGetHubInstallDecision_AlreadyTrustedWhenKeyringMatches(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyServer := httpKeyServer(t, pub)
	defer keyServer.Close()

	url := makeHubFixture(t, map[string]string{
		"alice_a.yaml": entryYAML("github://alice/a", "A", "alice", keyServer.URL+"/publisher.pub"),
	})
	keyringPath := writeKeyring(t, map[string][]ed25519.PublicKey{
		"github://alice/a": {pub},
	})
	srv := &apiServer{
		log:         slog.Default(),
		hub:         &hub.Client{URL: url, HTTP: keyServer.Client()},
		keyringPath: keyringPath,
	}
	rec := httptest.NewRecorder()
	srv.GetHubInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/install-decision?fqn=github://alice/a", nil), api.GetHubInstallDecisionParams{Fqn: "github://alice/a"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubInstallDecision
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.TrustState != api.HubTrustStateAlreadyTrusted {
		t.Fatalf("trust_state = %s, want already_trusted", got.TrustState)
	}
}

func TestGetHubInstallDecision_ConflictWhenSiblingHasDifferentKey(t *testing.T) {
	currentPub, _, _ := ed25519.GenerateKey(rand.Reader)
	siblingPub, _, _ := ed25519.GenerateKey(rand.Reader)

	keyServer := httpKeyServer(t, currentPub)
	defer keyServer.Close()

	// Hub has two entries by the same publisher.
	url := makeHubFixture(t, map[string]string{
		"alice_a.yaml": entryYAML("github://alice/a", "A", "alice", keyServer.URL+"/publisher.pub"),
		"alice_b.yaml": entryYAML("github://alice/b", "B", "alice", "https://example.com/alice-b.pub"),
	})

	// Keyring trusts sibling (b) under a DIFFERENT key from the one
	// the Hub now declares for (a). That cross-FQN mismatch is the
	// conflict the TOFU check is meant to catch.
	keyringPath := writeKeyring(t, map[string][]ed25519.PublicKey{
		"github://alice/b": {siblingPub},
	})
	srv := &apiServer{
		log:         slog.Default(),
		hub:         &hub.Client{URL: url, HTTP: keyServer.Client()},
		keyringPath: keyringPath,
	}
	rec := httptest.NewRecorder()
	srv.GetHubInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/install-decision?fqn=github://alice/a", nil), api.GetHubInstallDecisionParams{Fqn: "github://alice/a"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubInstallDecision
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.TrustState != api.HubTrustStateConflict {
		t.Fatalf("trust_state = %s, want conflict", got.TrustState)
	}
	foundConflictIndicator := false
	for _, risk := range got.RiskIndicators {
		if strings.Contains(risk, "differs from one you trust") {
			foundConflictIndicator = true
			break
		}
	}
	if !foundConflictIndicator {
		t.Fatalf("expected conflict risk indicator, got %v", got.RiskIndicators)
	}
}

func TestGetHubInstallDecision_PublisherFootprintIncludesSiblings(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyServer := httpKeyServer(t, pub)
	defer keyServer.Close()

	url := makeHubFixture(t, map[string]string{
		"alice_a.yaml": entryYAML("github://alice/a", "A", "alice", keyServer.URL+"/publisher.pub"),
		"alice_b.yaml": entryYAML("github://alice/b", "B", "alice", "https://example.com/b.pub"),
		"bob_x.yaml":   entryYAML("github://bob/x", "X", "bob", "https://example.com/x.pub"),
	})
	srv := &apiServer{
		log:         slog.Default(),
		hub:         &hub.Client{URL: url, HTTP: keyServer.Client()},
		keyringPath: filepath.Join(t.TempDir(), "keyring.json"),
	}
	rec := httptest.NewRecorder()
	srv.GetHubInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/install-decision?fqn=github://alice/a", nil), api.GetHubInstallDecisionParams{Fqn: "github://alice/a"})

	var got api.HubInstallDecision
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got.PublisherFootprint) != 1 || got.PublisherFootprint[0] != "github://alice/b" {
		t.Fatalf("expected publisher_footprint = [github://alice/b], got %v", got.PublisherFootprint)
	}
}

func TestGetHubInstallDecision_TrustedSiblingProducesPositiveIndicator(t *testing.T) {
	currentPub, _, _ := ed25519.GenerateKey(rand.Reader)

	keyServer := httpKeyServer(t, currentPub)
	defer keyServer.Close()

	url := makeHubFixture(t, map[string]string{
		"alice_a.yaml": entryYAML("github://alice/a", "A", "alice", keyServer.URL+"/publisher.pub"),
		"alice_b.yaml": entryYAML("github://alice/b", "B", "alice", "https://example.com/alice-b.pub"),
	})
	// Sibling b has a trusted key whose fingerprint matches what would
	// be fetched for it (we don't fetch sibling keys; the conflict check
	// only fires when *any* sibling key fingerprint differs from the
	// current entry's fingerprint). Use the same key everywhere to keep
	// trust_state out of conflict territory.
	keyringPath := writeKeyring(t, map[string][]ed25519.PublicKey{
		"github://alice/b": {currentPub},
	})
	srv := &apiServer{
		log:         slog.Default(),
		hub:         &hub.Client{URL: url, HTTP: keyServer.Client()},
		keyringPath: keyringPath,
	}
	rec := httptest.NewRecorder()
	srv.GetHubInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/install-decision?fqn=github://alice/a", nil), api.GetHubInstallDecisionParams{Fqn: "github://alice/a"})

	var got api.HubInstallDecision
	_ = json.NewDecoder(rec.Body).Decode(&got)
	found := false
	for _, r := range got.RiskIndicators {
		if strings.Contains(r, "other connector you already trust") || strings.Contains(r, "other connectors you already trust") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'other connector(s) you already trust' indicator, got %v", got.RiskIndicators)
	}
}

func TestListHubConnectors_HubDisabledReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default()} // hub field nil
	rec := httptest.NewRecorder()
	srv.ListHubConnectors(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/connectors", nil), api.ListHubConnectorsParams{})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when hub client nil, got %d", rec.Code)
	}
}

func TestGetHubInstallDecision_KeyFetchFailureReturns502(t *testing.T) {
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer keyServer.Close()

	url := makeHubFixture(t, map[string]string{
		"alice_a.yaml": entryYAML("github://alice/a", "A", "alice", keyServer.URL+"/missing.pub"),
	})
	srv := &apiServer{
		log: slog.Default(),
		hub: &hub.Client{URL: url, HTTP: keyServer.Client()},
	}
	rec := httptest.NewRecorder()
	srv.GetHubInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/install-decision?fqn=github://alice/a", nil), api.GetHubInstallDecisionParams{Fqn: "github://alice/a"})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// --- helpers ---

// twoEntryFixture is the shared Hub fixture used by tests that don't
// care about the entry contents beyond there being two of them.
var twoEntryFixture = map[string]string{
	"alice_a.yaml": entryYAML("github://alice/a", "A connector", "alice", "https://example.com/alice.pub"),
	"bob_b.yaml":   entryYAML("github://bob/b", "B connector", "bob", "https://example.com/bob.pub"),
}

func entryYAML(fqn, description, publisher, keyURL string) string {
	return "fqn: " + fqn + "\n" +
		"description: " + description + "\n" +
		"publisher_github: " + publisher + "\n" +
		"key_url: " + keyURL + "\n" +
		"release_pattern: v*\n"
}

func makeHubFixture(t *testing.T, entries map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	connectors := filepath.Join(dir, "connectors")
	if err := os.MkdirAll(connectors, 0o755); err != nil {
		t.Fatalf("mkdir connectors: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for name, body := range entries {
		if err := os.WriteFile(filepath.Join(connectors, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("add", "-A")
	runGit("commit", "-m", "seed")
	return "file://" + dir
}

// httpKeyServer returns a test HTTP server that serves the PEM-
// encoded form of pub on every request.
func httpKeyServer(t *testing.T, pub ed25519.PublicKey) *httptest.Server {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(pemBytes)
	}))
}

// writeKeyring writes a v1 keyring JSON file with the given trust map
// and returns the path. Mirrors what `aileron keyring trust` writes.
func writeKeyring(t *testing.T, trust map[string][]ed25519.PublicKey) string {
	t.Helper()
	ring := cstore.NewEd25519Keyring()
	for authority, keys := range trust {
		for _, k := range keys {
			ring.Add(authority, k)
		}
	}
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := ring.SaveKeyring(path); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}
	return path
}

// silenceContext is unused; kept to keep imports stable if helpers
// are added later that take a context.
var _ = context.Background
