package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
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

func TestListHubActions_ReturnsEntries(t *testing.T) {
	actions := map[string]string{
		"draft.yaml": actionYAML("github://alice/conn/actions/draft-email", "Draft a Gmail", "alice", "github://alice/conn", "communication", []string{"draft email"}),
		"list.yaml":  actionYAML("github://alice/conn/actions/list-emails", "List recent emails", "alice", "github://alice/conn", "communication", nil),
	}
	url := makeHubFixtureMulti(t, nil, actions, nil)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	srv.ListHubActions(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/actions", nil), api.ListHubActionsParams{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubActionList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(got.Actions))
	}
	// Sorted by FQN: draft-email comes before list-emails alphabetically
	if got.Actions[0].Fqn != "github://alice/conn/actions/draft-email" {
		t.Fatalf("entries not sorted by FQN: %+v", got.Actions)
	}
	if got.Actions[0].ConnectorFqn != "github://alice/conn" {
		t.Fatalf("connector_fqn not propagated: %+v", got.Actions[0])
	}
}

func TestListHubActions_FilterAppliesToFQNAndDescription(t *testing.T) {
	actions := map[string]string{
		"a.yaml": actionYAML("github://alice/conn/actions/draft-email", "Draft a Gmail", "alice", "github://alice/conn", "", nil),
		"b.yaml": actionYAML("github://bob/conn/actions/post-slack", "Post a Slack message", "bob", "github://bob/conn", "", nil),
	}
	url := makeHubFixtureMulti(t, nil, actions, nil)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	q := "gmail"
	srv.ListHubActions(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/actions?q=gmail", nil), api.ListHubActionsParams{Q: &q})

	var got api.HubActionList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Actions) != 1 || got.Actions[0].Fqn != "github://alice/conn/actions/draft-email" {
		t.Fatalf("expected single gmail match, got %+v", got.Actions)
	}
}

func TestListHubActions_HubUnreachableReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: "file:///nonexistent/path"}}
	rec := httptest.NewRecorder()
	srv.ListHubActions(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/actions", nil), api.ListHubActionsParams{})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestListHubActions_HubDisabledReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: nil}
	rec := httptest.NewRecorder()
	srv.ListHubActions(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/actions", nil), api.ListHubActionsParams{})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestGetHubAction_ReturnsEntryForKnownFQN(t *testing.T) {
	actions := map[string]string{
		"draft.yaml": actionYAML("github://alice/conn/actions/draft-email", "Draft a Gmail", "alice", "github://alice/conn", "communication", []string{"draft email", "compose gmail"}),
	}
	url := makeHubFixtureMulti(t, nil, actions, nil)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/actions/draft-email"
	srv.GetHubAction(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action?fqn="+fqn, nil), api.GetHubActionParams{Fqn: fqn})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubActionEntry
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Fqn != fqn || got.ConnectorFqn != "github://alice/conn" {
		t.Fatalf("unexpected entry: %+v", got)
	}
	if got.Intents == nil || len(*got.Intents) != 2 {
		t.Fatalf("intents not propagated: %+v", got.Intents)
	}
}

func TestGetHubAction_EmptyFQNReturns400(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: makeHubFixtureMulti(t, nil, nil, nil)}}
	rec := httptest.NewRecorder()
	srv.GetHubAction(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action", nil), api.GetHubActionParams{Fqn: ""})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestGetHubAction_MissingFQNReturns404(t *testing.T) {
	url := makeHubFixtureMulti(t, nil, map[string]string{
		"x.yaml": actionYAML("github://alice/conn/actions/x", "X", "alice", "github://alice/conn", "", nil),
	}, nil)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://nobody/missing/actions/x"
	srv.GetHubAction(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action?fqn="+fqn, nil), api.GetHubActionParams{Fqn: fqn})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestGetHubAction_HubUnreachableReturns503(t *testing.T) {
	// FetchActionByFQN propagates non-ErrNotFound errors as 503
	// hub_unreachable rather than 404 — surfaces the connectivity
	// problem honestly instead of masking it as "missing entry".
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: "file:///nonexistent/path"}}
	rec := httptest.NewRecorder()
	srv.GetHubAction(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action?fqn=x", nil), api.GetHubActionParams{Fqn: "github://anywhere/x"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 hub_unreachable, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestGetHubAction_HubDisabledReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: nil}
	rec := httptest.NewRecorder()
	srv.GetHubAction(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action?fqn=x", nil), api.GetHubActionParams{Fqn: "x"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestListHubSuites_ReturnsEntries(t *testing.T) {
	suites := map[string]string{
		"alice.yaml": suiteYAML(
			"github://alice/conn/suite", "Alice's suite", "alice", "communication",
			[]string{"github://alice/conn/actions/a", "github://alice/conn/actions/b"},
			[]string{"github://alice/conn"},
		),
	}
	url := makeHubFixtureMulti(t, nil, nil, suites)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	srv.ListHubSuites(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suites", nil), api.ListHubSuitesParams{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubSuiteList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Suites) != 1 {
		t.Fatalf("expected 1 suite, got %d", len(got.Suites))
	}
	if len(got.Suites[0].MemberActions) != 2 {
		t.Fatalf("member_actions not propagated: %+v", got.Suites[0])
	}
	if got.Suites[0].ConnectorsRequired == nil || len(*got.Suites[0].ConnectorsRequired) != 1 {
		t.Fatalf("connectors_required not propagated: %+v", got.Suites[0])
	}
}

func TestListHubSuites_FilterAppliesToFQNAndDescription(t *testing.T) {
	suites := map[string]string{
		"a.yaml": suiteYAML("github://alice/conn/suite", "Gmail and Calendar", "alice", "", []string{"github://alice/conn/actions/a"}, nil),
		"b.yaml": suiteYAML("github://bob/conn/suite", "Slack essentials", "bob", "", []string{"github://bob/conn/actions/b"}, nil),
	}
	url := makeHubFixtureMulti(t, nil, nil, suites)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	q := "calendar"
	srv.ListHubSuites(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suites?q=calendar", nil), api.ListHubSuitesParams{Q: &q})

	var got api.HubSuiteList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Suites) != 1 || got.Suites[0].Fqn != "github://alice/conn/suite" {
		t.Fatalf("expected single calendar match, got %+v", got.Suites)
	}
}

func TestListHubSuites_HubUnreachableReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: "file:///nonexistent/path"}}
	rec := httptest.NewRecorder()
	srv.ListHubSuites(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suites", nil), api.ListHubSuitesParams{})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestListHubSuites_HubDisabledReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: nil}
	rec := httptest.NewRecorder()
	srv.ListHubSuites(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suites", nil), api.ListHubSuitesParams{})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestGetHubSuite_ReturnsEntryForKnownFQN(t *testing.T) {
	suites := map[string]string{
		"alice.yaml": suiteYAML(
			"github://alice/conn/suite", "Alice's suite", "alice", "communication",
			[]string{"github://alice/conn/actions/a"}, nil,
		),
	}
	url := makeHubFixtureMulti(t, nil, nil, suites)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/suite"
	srv.GetHubSuite(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite?fqn="+fqn, nil), api.GetHubSuiteParams{Fqn: fqn})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubSuiteEntry
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Fqn != fqn || got.PublisherGithub != "alice" {
		t.Fatalf("unexpected entry: %+v", got)
	}
}

func TestGetHubSuite_EmptyFQNReturns400(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: makeHubFixtureMulti(t, nil, nil, nil)}}
	rec := httptest.NewRecorder()
	srv.GetHubSuite(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite", nil), api.GetHubSuiteParams{Fqn: ""})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestGetHubSuite_MissingFQNReturns404(t *testing.T) {
	url := makeHubFixtureMulti(t, nil, nil, map[string]string{
		"x.yaml": suiteYAML("github://alice/conn/suite", "x", "alice", "", []string{"github://alice/conn/actions/a"}, nil),
	})
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://nobody/missing/suite"
	srv.GetHubSuite(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite?fqn="+fqn, nil), api.GetHubSuiteParams{Fqn: fqn})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestGetHubSuite_HubUnreachableReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: "file:///nonexistent/path"}}
	rec := httptest.NewRecorder()
	srv.GetHubSuite(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite?fqn=x", nil), api.GetHubSuiteParams{Fqn: "github://anywhere/x/suite"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 hub_unreachable, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestGetHubSuite_HubDisabledReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: nil}
	rec := httptest.NewRecorder()
	srv.GetHubSuite(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite?fqn=x", nil), api.GetHubSuiteParams{Fqn: "x"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

// --- composite install-decision: action ---

func TestGetHubActionInstallDecision_HappyPath(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyServer := httpKeyServer(t, pub)
	defer keyServer.Close()

	url := makeHubFixtureMulti(t,
		map[string]string{
			"google.yaml": entryYAML("github://alice/conn-google", "Google connector", "alice", keyServer.URL+"/publisher.pub"),
		},
		map[string]string{
			"draft.yaml": actionYAML("github://alice/conn-google/actions/draft-email", "Draft a Gmail", "alice", "github://alice/conn-google", "communication", []string{"draft email"}),
		},
		nil,
	)
	srv := &apiServer{
		log:         slog.Default(),
		hub:         &hub.Client{URL: url, HTTP: keyServer.Client()},
		keyringPath: filepath.Join(t.TempDir(), "keyring.json"),
	}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn-google/actions/draft-email"
	srv.GetHubActionInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action-install-decision?fqn="+fqn, nil), api.GetHubActionInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubActionInstallDecision
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != api.HubActionInstallDecisionKindAction {
		t.Fatalf("kind = %s, want action", got.Kind)
	}
	if got.Fqn != fqn {
		t.Fatalf("fqn = %s", got.Fqn)
	}
	if got.ConnectorFqn != "github://alice/conn-google" {
		t.Fatalf("connector_fqn = %s", got.ConnectorFqn)
	}
	if len(got.Authorities) != 1 {
		t.Fatalf("expected exactly 1 authority, got %d", len(got.Authorities))
	}
	auth := got.Authorities[0]
	if auth.Fqn != "github://alice/conn-google" {
		t.Fatalf("authority fqn = %s", auth.Fqn)
	}
	if auth.Fingerprint != hub.Fingerprint(pub) {
		t.Fatalf("authority fingerprint mismatch")
	}
	if auth.TrustState != api.HubTrustStateUnknown {
		t.Fatalf("trust_state = %s, want unknown", auth.TrustState)
	}
}

// TestGetHubActionInstallDecision_PopulatesLatestVersion: the webapp
// install modal needs a concrete version to call /v1/actions/install
// (the endpoint requires strict SemVer). The composite install-decision
// resolves it server-side via the same VersionLister the connector-check
// handler uses. #743 PR 2.
func TestGetHubActionInstallDecision_PopulatesLatestVersion(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyServer := httpKeyServer(t, pub)
	defer keyServer.Close()
	url := makeHubFixtureMulti(t,
		map[string]string{
			"google.yaml": entryYAML("github://alice/conn-google", "Google connector", "alice", keyServer.URL+"/publisher.pub"),
		},
		map[string]string{
			"draft.yaml": actionYAML("github://alice/conn-google/actions/draft-email", "Draft a Gmail", "alice", "github://alice/conn-google", "communication", []string{"draft email"}),
		},
		nil,
	)
	srv := &apiServer{
		log:         slog.Default(),
		hub:         &hub.Client{URL: url, HTTP: keyServer.Client()},
		keyringPath: filepath.Join(t.TempDir(), "keyring.json"),
		versionLister: &fakeLister{byFQN: map[string]fakeListerEntry{
			"github://alice/conn-google": {versions: []string{"2.0.0", "1.2.0-rc.1", "1.1.0"}},
		}},
	}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn-google/actions/draft-email"
	srv.GetHubActionInstallDecision(rec,
		httptest.NewRequest(http.MethodGet, "/v1/hub/action-install-decision?fqn="+fqn, nil),
		api.GetHubActionInstallDecisionParams{Fqn: fqn})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got api.HubActionInstallDecision
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LatestVersion == nil {
		t.Fatalf("LatestVersion = nil; want resolved to 2.0.0")
	}
	if *got.LatestVersion != "2.0.0" {
		t.Errorf("LatestVersion = %q, want 2.0.0 (prereleases skipped)", *got.LatestVersion)
	}
}

// TestGetHubActionInstallDecision_OmitsLatestOnListerFailure: a
// version-listing failure must not break the install-decision render.
// The webapp modal can still surface the trust panel; it just can't
// offer the "Install" button until the version's resolved. The CLI
// always works as a fallback.
func TestGetHubActionInstallDecision_OmitsLatestOnListerFailure(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyServer := httpKeyServer(t, pub)
	defer keyServer.Close()
	url := makeHubFixtureMulti(t,
		map[string]string{
			"google.yaml": entryYAML("github://alice/conn-google", "Google connector", "alice", keyServer.URL+"/publisher.pub"),
		},
		map[string]string{
			"draft.yaml": actionYAML("github://alice/conn-google/actions/draft-email", "Draft a Gmail", "alice", "github://alice/conn-google", "communication", []string{"draft email"}),
		},
		nil,
	)
	srv := &apiServer{
		log:         slog.Default(),
		hub:         &hub.Client{URL: url, HTTP: keyServer.Client()},
		keyringPath: filepath.Join(t.TempDir(), "keyring.json"),
		versionLister: &fakeLister{byFQN: map[string]fakeListerEntry{
			"github://alice/conn-google": {err: errors.New("rate limited")},
		}},
	}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn-google/actions/draft-email"
	srv.GetHubActionInstallDecision(rec,
		httptest.NewRequest(http.MethodGet, "/v1/hub/action-install-decision?fqn="+fqn, nil),
		api.GetHubActionInstallDecisionParams{Fqn: fqn})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got api.HubActionInstallDecision
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LatestVersion != nil {
		t.Errorf("LatestVersion = %q, want nil (graceful degrade on lister failure)",
			*got.LatestVersion)
	}
}

func TestGetHubActionInstallDecision_HubDisabledReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: nil}
	rec := httptest.NewRecorder()
	srv.GetHubActionInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action-install-decision?fqn=x", nil), api.GetHubActionInstallDecisionParams{Fqn: "x"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestGetHubActionInstallDecision_EmptyFQNReturns400(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: makeHubFixtureMulti(t, nil, nil, nil)}}
	rec := httptest.NewRecorder()
	srv.GetHubActionInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action-install-decision", nil), api.GetHubActionInstallDecisionParams{Fqn: ""})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestGetHubActionInstallDecision_MissingActionReturns404(t *testing.T) {
	url := makeHubFixtureMulti(t, nil, map[string]string{
		"a.yaml": actionYAML("github://alice/conn/actions/a", "A", "alice", "github://alice/conn", "", nil),
	}, nil)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://nobody/missing/actions/x"
	srv.GetHubActionInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action-install-decision?fqn="+fqn, nil), api.GetHubActionInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestGetHubActionInstallDecision_MissingConnectorReturns422(t *testing.T) {
	// Action exists in the catalog but its declared connector_fqn has
	// no matching connector entry. The install pipeline could still
	// proceed via direct FQN resolution, but the Hub can't pre-compute
	// the trust panel — surface as 422 with a clear code.
	url := makeHubFixtureMulti(t, nil, map[string]string{
		"a.yaml": actionYAML("github://alice/conn/actions/a", "A", "alice", "github://alice/missing-conn", "", nil),
	}, nil)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/actions/a"
	srv.GetHubActionInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action-install-decision?fqn="+fqn, nil), api.GetHubActionInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing_connector") {
		t.Fatalf("expected missing_connector error code, got %s", rec.Body.String())
	}
}

func TestGetHubActionInstallDecision_ActionWithoutConnectorFQNReturns422(t *testing.T) {
	url := makeHubFixtureMulti(t, nil, map[string]string{
		// Bare YAML without connector_fqn — `actionYAML` always emits it,
		// so build the body inline to test the validation path.
		"a.yaml": "fqn: github://alice/conn/actions/a\ndescription: A\npublisher_github: alice\n",
	}, nil)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/actions/a"
	srv.GetHubActionInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action-install-decision?fqn="+fqn, nil), api.GetHubActionInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestGetHubActionInstallDecision_HubUnreachableReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: "file:///nonexistent/path"}}
	rec := httptest.NewRecorder()
	srv.GetHubActionInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action-install-decision?fqn=x", nil), api.GetHubActionInstallDecisionParams{Fqn: "github://anywhere/x/actions/x"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestGetHubActionInstallDecision_KeyFetchFailureReturns502(t *testing.T) {
	// key_url points at an HTTP server that returns 404 — verify the
	// composite endpoint surfaces it as 502 key_fetch_failed, same
	// shape as the connector-level install-decision endpoint.
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer keyServer.Close()
	url := makeHubFixtureMulti(t,
		map[string]string{
			"c.yaml": entryYAML("github://alice/conn", "C", "alice", keyServer.URL+"/missing.pub"),
		},
		map[string]string{
			"a.yaml": actionYAML("github://alice/conn/actions/a", "A", "alice", "github://alice/conn", "", nil),
		},
		nil,
	)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url, HTTP: keyServer.Client()}}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/actions/a"
	srv.GetHubActionInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/action-install-decision?fqn="+fqn, nil), api.GetHubActionInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// --- composite install-decision: suite ---

func TestGetHubSuiteInstallDecision_HappyPathWithConnectorsRequired(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyServer := httpKeyServer(t, pub)
	defer keyServer.Close()

	url := makeHubFixtureMulti(t,
		map[string]string{
			"c.yaml": entryYAML("github://alice/conn", "C", "alice", keyServer.URL+"/publisher.pub"),
		},
		nil,
		map[string]string{
			"s.yaml": suiteYAML(
				"github://alice/conn/suite", "Alice's suite", "alice", "communication",
				[]string{"github://alice/conn/actions/a", "github://alice/conn/actions/b"},
				[]string{"github://alice/conn"},
			),
		},
	)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url, HTTP: keyServer.Client()}, keyringPath: filepath.Join(t.TempDir(), "keyring.json")}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/suite"
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision?fqn="+fqn, nil), api.GetHubSuiteInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubSuiteInstallDecision
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != "suite" {
		t.Fatalf("kind = %s, want suite", got.Kind)
	}
	if got.Fqn != fqn {
		t.Fatalf("fqn = %s", got.Fqn)
	}
	if len(got.MemberActions) != 2 {
		t.Fatalf("expected 2 member_actions, got %d", len(got.MemberActions))
	}
	if len(got.Authorities) != 1 {
		t.Fatalf("expected 1 authority, got %d", len(got.Authorities))
	}
	if got.Authorities[0].Fingerprint != hub.Fingerprint(pub) {
		t.Fatalf("authority fingerprint mismatch")
	}
}

func TestGetHubSuiteInstallDecision_HappyPathDerivedFromMemberActions(t *testing.T) {
	// Suite has no connectors_required; closure must come from walking
	// each member action's connector_fqn.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyServer := httpKeyServer(t, pub)
	defer keyServer.Close()

	url := makeHubFixtureMulti(t,
		map[string]string{
			"c.yaml": entryYAML("github://alice/conn", "C", "alice", keyServer.URL+"/publisher.pub"),
		},
		map[string]string{
			"a.yaml": actionYAML("github://alice/conn/actions/a", "A", "alice", "github://alice/conn", "", nil),
			"b.yaml": actionYAML("github://alice/conn/actions/b", "B", "alice", "github://alice/conn", "", nil),
		},
		map[string]string{
			"s.yaml": suiteYAML(
				"github://alice/conn/suite", "Alice's suite", "alice", "",
				[]string{"github://alice/conn/actions/a", "github://alice/conn/actions/b"},
				nil,
			),
		},
	)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url, HTTP: keyServer.Client()}, keyringPath: filepath.Join(t.TempDir(), "keyring.json")}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/suite"
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision?fqn="+fqn, nil), api.GetHubSuiteInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubSuiteInstallDecision
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got.Authorities) != 1 {
		t.Fatalf("expected 1 authority deduped from two member actions sharing a connector, got %d", len(got.Authorities))
	}
	if got.Authorities[0].Fqn != "github://alice/conn" {
		t.Fatalf("authority fqn = %s", got.Authorities[0].Fqn)
	}
}

func TestGetHubSuiteInstallDecision_MultipleAuthoritiesAcrossPublishers(t *testing.T) {
	// Suite spans two publishers' connectors. Expect two authorities
	// in the order declared by connectors_required.
	alicePub, _, _ := ed25519.GenerateKey(rand.Reader)
	bobPub, _, _ := ed25519.GenerateKey(rand.Reader)
	aliceKey := httpKeyServer(t, alicePub)
	bobKey := httpKeyServer(t, bobPub)
	defer aliceKey.Close()
	defer bobKey.Close()

	// Single HTTP client; both keyservers use the default test client.
	// Use a multiplexing HTTP client that hits whichever URL is requested.
	httpClient := aliceKey.Client()

	url := makeHubFixtureMulti(t,
		map[string]string{
			"alice.yaml": entryYAML("github://alice/conn", "Alice conn", "alice", aliceKey.URL+"/publisher.pub"),
			"bob.yaml":   entryYAML("github://bob/conn", "Bob conn", "bob", bobKey.URL+"/publisher.pub"),
		},
		nil,
		map[string]string{
			"s.yaml": suiteYAML(
				"github://alice/conn/suite", "Cross-publisher suite", "alice", "",
				[]string{"github://alice/conn/actions/a", "github://bob/conn/actions/b"},
				[]string{"github://alice/conn", "github://bob/conn"},
			),
		},
	)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url, HTTP: httpClient}, keyringPath: filepath.Join(t.TempDir(), "keyring.json")}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/suite"
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision?fqn="+fqn, nil), api.GetHubSuiteInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.HubSuiteInstallDecision
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got.Authorities) != 2 {
		t.Fatalf("expected 2 authorities, got %d", len(got.Authorities))
	}
	if got.Authorities[0].Fqn != "github://alice/conn" || got.Authorities[1].Fqn != "github://bob/conn" {
		t.Fatalf("authority order/identity wrong: %+v", got.Authorities)
	}
	if got.Authorities[0].Fingerprint != hub.Fingerprint(alicePub) {
		t.Fatalf("alice authority fingerprint mismatch")
	}
	if got.Authorities[1].Fingerprint != hub.Fingerprint(bobPub) {
		t.Fatalf("bob authority fingerprint mismatch")
	}
}

func TestGetHubSuiteInstallDecision_HubDisabledReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: nil}
	rec := httptest.NewRecorder()
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision?fqn=x", nil), api.GetHubSuiteInstallDecisionParams{Fqn: "x"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestGetHubSuiteInstallDecision_EmptyFQNReturns400(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: makeHubFixtureMulti(t, nil, nil, nil)}}
	rec := httptest.NewRecorder()
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision", nil), api.GetHubSuiteInstallDecisionParams{Fqn: ""})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestGetHubSuiteInstallDecision_MissingSuiteReturns404(t *testing.T) {
	url := makeHubFixtureMulti(t, nil, nil, map[string]string{
		"s.yaml": suiteYAML("github://alice/conn/suite", "S", "alice", "", []string{"github://alice/conn/actions/a"}, nil),
	})
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://nobody/missing/suite"
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision?fqn="+fqn, nil), api.GetHubSuiteInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestGetHubSuiteInstallDecision_MissingMemberActionReturns422(t *testing.T) {
	// Suite has no connectors_required, so closure derivation walks
	// member_actions. The member action isn't in the catalog — surface
	// the gap honestly rather than masking it.
	url := makeHubFixtureMulti(t, nil, nil, map[string]string{
		"s.yaml": suiteYAML("github://alice/conn/suite", "S", "alice", "", []string{"github://alice/conn/actions/missing"}, nil),
	})
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/suite"
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision?fqn="+fqn, nil), api.GetHubSuiteInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing_member_action") {
		t.Fatalf("expected missing_member_action code, got %s", rec.Body.String())
	}
}

func TestGetHubSuiteInstallDecision_MissingConnectorReturns422(t *testing.T) {
	url := makeHubFixtureMulti(t,
		nil,
		nil,
		map[string]string{
			"s.yaml": suiteYAML(
				"github://alice/conn/suite", "S", "alice", "",
				[]string{"github://alice/conn/actions/a"},
				[]string{"github://alice/missing-conn"},
			),
		},
	)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/suite"
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision?fqn="+fqn, nil), api.GetHubSuiteInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing_connector") {
		t.Fatalf("expected missing_connector code, got %s", rec.Body.String())
	}
}

func TestGetHubSuiteInstallDecision_NoMembersAndNoConnectorsReturns422(t *testing.T) {
	// Suite YAML with no connectors_required AND no member_actions is
	// degenerate — we have no closure to compute. Inline-author the
	// YAML since suiteYAML insists on at least one member.
	url := makeHubFixtureMulti(t, nil, nil, map[string]string{
		"s.yaml": "fqn: github://alice/conn/suite\ndescription: empty\npublisher_github: alice\nmember_actions: []\n",
	})
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/suite"
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision?fqn="+fqn, nil), api.GetHubSuiteInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestGetHubSuiteInstallDecision_MemberActionWithoutConnectorFQNReturns422(t *testing.T) {
	// Suite without connectors_required walks member_actions; one of
	// the member actions is missing connector_fqn. Surface the gap
	// rather than producing an empty authority list.
	url := makeHubFixtureMulti(t,
		nil,
		map[string]string{
			"a.yaml": "fqn: github://alice/conn/actions/a\ndescription: A\npublisher_github: alice\n",
		},
		map[string]string{
			"s.yaml": suiteYAML(
				"github://alice/conn/suite", "S", "alice", "",
				[]string{"github://alice/conn/actions/a"},
				nil,
			),
		},
	)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url}}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/suite"
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision?fqn="+fqn, nil), api.GetHubSuiteInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_action_entry") {
		t.Fatalf("expected invalid_action_entry code, got %s", rec.Body.String())
	}
}

func TestGetHubSuiteInstallDecision_KeyFetchFailureReturns502(t *testing.T) {
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer keyServer.Close()

	url := makeHubFixtureMulti(t,
		map[string]string{
			"c.yaml": entryYAML("github://alice/conn", "C", "alice", keyServer.URL+"/missing.pub"),
		},
		nil,
		map[string]string{
			"s.yaml": suiteYAML(
				"github://alice/conn/suite", "S", "alice", "",
				[]string{"github://alice/conn/actions/a"},
				[]string{"github://alice/conn"},
			),
		},
	)
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: url, HTTP: keyServer.Client()}}
	rec := httptest.NewRecorder()
	fqn := "github://alice/conn/suite"
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision?fqn="+fqn, nil), api.GetHubSuiteInstallDecisionParams{Fqn: fqn})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestGetHubSuiteInstallDecision_HubUnreachableReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default(), hub: &hub.Client{URL: "file:///nonexistent/path"}}
	rec := httptest.NewRecorder()
	srv.GetHubSuiteInstallDecision(rec, httptest.NewRequest(http.MethodGet, "/v1/hub/suite-install-decision?fqn=x", nil), api.GetHubSuiteInstallDecisionParams{Fqn: "github://anywhere/x/suite"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
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
	return makeHubFixtureMulti(t, entries, nil, nil)
}

// makeHubFixtureMulti builds a Hub fixture with optional content in any
// of the three catalog directories. Pass nil for a directory to leave
// it absent on disk (mirrors a Hub repo that hasn't been seeded with
// that entry type yet).
func makeHubFixtureMulti(t *testing.T, connectors, actions, suites map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	writeDir := func(sub string, entries map[string]string) {
		if entries == nil {
			return
		}
		path := filepath.Join(dir, sub)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
		for name, body := range entries {
			if err := os.WriteFile(filepath.Join(path, name), []byte(body), 0o644); err != nil {
				t.Fatalf("write %s/%s: %v", sub, name, err)
			}
		}
	}
	writeDir("connectors", connectors)
	writeDir("actions", actions)
	writeDir("suites", suites)
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

func actionYAML(fqn, description, publisher, connectorFQN, category string, intents []string) string {
	out := "fqn: " + fqn + "\n" +
		"description: " + description + "\n" +
		"publisher_github: " + publisher + "\n" +
		"connector_fqn: " + connectorFQN + "\n"
	if len(intents) > 0 {
		out += "intents:\n"
		for _, intent := range intents {
			out += "  - " + intent + "\n"
		}
	}
	if category != "" {
		out += "category: " + category + "\n"
	}
	return out
}

func suiteYAML(fqn, description, publisher, category string, memberActions, connectorsRequired []string) string {
	out := "fqn: " + fqn + "\n" +
		"description: " + description + "\n" +
		"publisher_github: " + publisher + "\n" +
		"member_actions:\n"
	for _, a := range memberActions {
		out += "  - " + a + "\n"
	}
	if len(connectorsRequired) > 0 {
		out += "connectors_required:\n"
		for _, c := range connectorsRequired {
			out += "  - " + c + "\n"
		}
	}
	if category != "" {
		out += "category: " + category + "\n"
	}
	return out
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
