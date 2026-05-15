package binding_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/vault"
)

func TestParse_AcceptsCanonicalName(t *testing.T) {
	cases := []string{
		"api_key/slack/work",
		"oauth2/google/personal",
		"x509/treasury/prod",
		"api_key/linear/team-2",
		"api_key/slack/alr.work",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			n, kind, service, identity, err := binding.Parse(c)
			if err != nil {
				t.Fatalf("Parse(%q) err = %v", c, err)
			}
			if n.String() != c {
				t.Errorf("name = %q, want %q", n, c)
			}
			if kind == "" || service == "" || identity == "" {
				t.Errorf("missing segment: kind=%q service=%q identity=%q", kind, service, identity)
			}
		})
	}
}

func TestParse_RejectsBadShapes(t *testing.T) {
	cases := []string{
		"",
		"oauth2",                  // one segment
		"oauth2/slack",            // two segments
		"oauth2/slack/work/extra", // four segments
		"OAUTH2/slack/work",       // uppercase kind
		"oauth2/Slack/work",       // uppercase service
		"oauth2//work",            // empty service
		"oauth2/slack/",           // empty identity
		"_secret/slack/work",      // leading underscore in kind
		"api key/slack/work",      // space
		"../etc/passwd",           // traversal
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if _, _, _, _, err := binding.Parse(c); err == nil {
				t.Errorf("Parse(%q) accepted; want error", c)
			}
		})
	}
}

func TestMakeName_RoundTripsValidSegments(t *testing.T) {
	got, err := binding.MakeName("api_key", "slack", "work")
	if err != nil {
		t.Fatalf("MakeName: %v", err)
	}
	if got.String() != "api_key/slack/work" {
		t.Errorf("MakeName = %q", got)
	}
}

func TestMakeName_RejectsInvalidSegments(t *testing.T) {
	if _, err := binding.MakeName("", "slack", "work"); err == nil {
		t.Error("empty kind accepted")
	}
	if _, err := binding.MakeName("api_key", "", "work"); err == nil {
		t.Error("empty service accepted")
	}
	if _, err := binding.MakeName("api_key", "slack", ""); err == nil {
		t.Error("empty identity accepted")
	}
}

func TestIsBindingPath(t *testing.T) {
	if !binding.IsBindingPath("api_key/slack/work") {
		t.Error("valid binding path rejected")
	}
	if binding.IsBindingPath("_verification") {
		t.Error("vault verification blob accepted as binding path")
	}
	if binding.IsBindingPath("not-a-binding") {
		t.Error("non-binding path accepted")
	}
}

// --- VaultStore contract tests --------------------------------------

func newStore(t *testing.T) *binding.VaultStore {
	t.Helper()
	return &binding.VaultStore{
		Vault: vault.NewMemVault(),
		Now:   func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func TestVaultStore_PutAndGetRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	b := binding.Binding{
		Name:         "api_key/slack/work",
		Kind:         "api_key",
		Service:      "slack",
		Identity:     "work",
		Scope:        "chat:write",
		ConnectorFQN: "github://aileron/slack",
		Account:      "alr@workplace.com",
	}
	if err := s.Put(ctx, b, []byte("xoxb-secret"), binding.PutCreate); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, "api_key/slack/work")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ConnectorFQN != b.ConnectorFQN {
		t.Errorf("ConnectorFQN = %q, want %q", got.ConnectorFQN, b.ConnectorFQN)
	}
	if got.Scope != "chat:write" {
		t.Errorf("Scope = %q", got.Scope)
	}
	if got.Account != "alr@workplace.com" {
		t.Errorf("Account = %q", got.Account)
	}
	if got.Status != binding.StatusActive {
		t.Errorf("Status = %q, want active", got.Status)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt was not stamped")
	}
}

func TestVaultStore_Get_NotFoundReturnsSentinel(t *testing.T) {
	s := newStore(t)
	_, err := s.Get(context.Background(), "api_key/slack/missing")
	if !errors.Is(err, binding.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestVaultStore_Put_RejectsInvalidName(t *testing.T) {
	s := newStore(t)
	err := s.Put(context.Background(), binding.Binding{Name: "BAD"}, []byte("v"), binding.PutCreate)
	if err == nil {
		t.Fatal("invalid name accepted")
	}
}

func TestVaultStore_Put_CreateModeRejectsExisting(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	b := binding.Binding{
		Name: "api_key/slack/work", Kind: "api_key", Service: "slack",
		Identity: "work", ConnectorFQN: "github://aileron/slack",
	}
	if err := s.Put(ctx, b, []byte("v"), binding.PutCreate); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	err := s.Put(ctx, b, []byte("v"), binding.PutCreate)
	if !errors.Is(err, binding.ErrAlreadyExists) {
		t.Errorf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestVaultStore_Put_UpsertModeReplaces(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	b := binding.Binding{
		Name: "api_key/slack/work", Kind: "api_key", Service: "slack",
		Identity: "work", ConnectorFQN: "github://aileron/slack", Account: "old",
	}
	if err := s.Put(ctx, b, []byte("v1"), binding.PutCreate); err != nil {
		t.Fatalf("Put create: %v", err)
	}
	b.Account = "new"
	if err := s.Put(ctx, b, []byte("v2"), binding.PutUpsert); err != nil {
		t.Fatalf("Put upsert: %v", err)
	}
	got, _ := s.Get(ctx, "api_key/slack/work")
	if got.Account != "new" {
		t.Errorf("upsert did not replace metadata: account = %q", got.Account)
	}
}

func TestVaultStore_Delete_NotFound(t *testing.T) {
	s := newStore(t)
	if err := s.Delete(context.Background(), "api_key/slack/nope"); !errors.Is(err, binding.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestVaultStore_Delete_RemovesEntry(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	b := binding.Binding{
		Name: "api_key/slack/work", Kind: "api_key", Service: "slack",
		Identity: "work", ConnectorFQN: "github://aileron/slack",
	}
	_ = s.Put(ctx, b, []byte("v"), binding.PutCreate)
	if err := s.Delete(ctx, "api_key/slack/work"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "api_key/slack/work"); !errors.Is(err, binding.ErrNotFound) {
		t.Errorf("after delete, err = %v", err)
	}
}

func TestVaultStore_List_FiltersNonBindingEntries(t *testing.T) {
	v := vault.NewMemVault()
	// A non-binding vault entry (e.g. the vault's own verification blob).
	if err := v.Put(context.Background(), "_verification",
		[]byte("blob"), vault.Metadata{Type: "verification"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s := &binding.VaultStore{Vault: v}
	if err := s.Put(context.Background(), binding.Binding{
		Name: "api_key/slack/work", Kind: "api_key", Service: "slack",
		Identity: "work", ConnectorFQN: "github://aileron/slack",
	}, []byte("v"), binding.PutCreate); err != nil {
		t.Fatalf("Put binding: %v", err)
	}
	got, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Name != "api_key/slack/work" {
		t.Errorf("List returned %+v; want only the binding", got)
	}
}

func TestVaultStore_Resolve_NoMatchReturnsNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.Resolve(context.Background(), "github://nobody/x", "api_key")
	if !errors.Is(err, binding.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestVaultStore_Resolve_SingleMatch(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, binding.Binding{
		Name: "api_key/slack/work", Kind: "api_key", Service: "slack",
		Identity: "work", ConnectorFQN: "github://aileron/slack",
	}, []byte("v"), binding.PutCreate); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Resolve(ctx, "github://aileron/slack", "api_key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "api_key/slack/work" {
		t.Errorf("Resolve.Name = %q", got.Name)
	}
}

func TestVaultStore_Resolve_AmbiguousReturnsCandidates(t *testing.T) {
	// Per ADR-0006: the runtime never picks silently. Two bindings of
	// the same connector+kind → ErrAmbiguous with both names so the
	// caller can surface them to the user.
	s := newStore(t)
	ctx := context.Background()
	for _, identity := range []string{"work", "personal"} {
		if err := s.Put(ctx, binding.Binding{
			Name: binding.Name("api_key/slack/" + identity), Kind: "api_key",
			Service: "slack", Identity: identity, ConnectorFQN: "github://aileron/slack",
		}, []byte("v"), binding.PutCreate); err != nil {
			t.Fatalf("Put %s: %v", identity, err)
		}
	}
	_, err := s.Resolve(ctx, "github://aileron/slack", "api_key")
	if !errors.Is(err, binding.ErrAmbiguous) {
		t.Fatalf("err = %v, want ErrAmbiguous", err)
	}
	var ambErr *binding.AmbiguousError
	if !errors.As(err, &ambErr) {
		t.Fatalf("err type = %T", err)
	}
	if len(ambErr.Candidates) != 2 {
		t.Errorf("len(Candidates) = %d, want 2", len(ambErr.Candidates))
	}
	if ambErr.ConnectorFQN != "github://aileron/slack" || ambErr.Kind != "api_key" {
		t.Errorf("AmbiguousError fields = %+v", ambErr)
	}
}

func TestVaultStore_Resolve_DistinguishesByKindAndConnector(t *testing.T) {
	// Bindings under the same kind but different connectors don't
	// collide; bindings under the same connector but different kinds
	// don't collide either.
	s := newStore(t)
	ctx := context.Background()
	mustPut := func(name binding.Name, kind, fqn string) {
		t.Helper()
		_, _, service, identity, _ := binding.Parse(string(name))
		if err := s.Put(ctx, binding.Binding{
			Name: name, Kind: kind, Service: service, Identity: identity, ConnectorFQN: fqn,
		}, []byte("v"), binding.PutCreate); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}
	mustPut("api_key/slack/work", "api_key", "github://aileron/slack")
	mustPut("api_key/linear/team", "api_key", "github://aileron/linear")
	mustPut("oauth2/slack/work", "oauth2", "github://aileron/slack")

	got, err := s.Resolve(ctx, "github://aileron/slack", "api_key")
	if err != nil {
		t.Fatalf("Resolve api_key: %v", err)
	}
	if got.Name != "api_key/slack/work" {
		t.Errorf("api_key resolve = %q", got.Name)
	}
	got, err = s.Resolve(ctx, "github://aileron/slack", "oauth2")
	if err != nil {
		t.Fatalf("Resolve oauth2: %v", err)
	}
	if got.Name != "oauth2/slack/work" {
		t.Errorf("oauth2 resolve = %q", got.Name)
	}
}

func TestVaultStore_NilReceiver(t *testing.T) {
	var s *binding.VaultStore
	if _, err := s.List(context.Background()); err == nil {
		t.Error("nil receiver List = nil err; want failure")
	}
}

func TestAmbiguousError_FormatNamesCandidates(t *testing.T) {
	// Per ADR-0006 the runtime never picks silently — when the store
	// hits multiple matches, the surfaced error must list the
	// candidate names so the agent / CLI can guide the user.
	err := &binding.AmbiguousError{
		ConnectorFQN: "github://aileron/slack",
		Kind:         "oauth2",
		Candidates:   []binding.Name{"oauth2/slack/work", "oauth2/slack/personal"},
	}
	got := err.Error()
	for _, want := range []string{
		"github://aileron/slack",
		"oauth2",
		"oauth2/slack/work",
		"oauth2/slack/personal",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q; missing %q", got, want)
		}
	}
}

func TestBinding_AllOptionalFieldsRoundTripThroughVault(t *testing.T) {
	// Exercises every conditional in toMetadata + fromEntry: scope,
	// account, last_used_at, last_refreshed_at, refresh_token_present.
	// This is the path Inspect/List use to render to the API.
	s := newStore(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()
	b := binding.Binding{
		Name:                "oauth2/slack/work",
		Kind:                "oauth2",
		Service:             "slack",
		Identity:            "work",
		Scope:               "chat:write",
		ConnectorFQN:        "github://aileron/slack",
		Account:             "alr@workplace.com",
		CreatedAt:           now,
		LastUsedAt:          now.Add(time.Hour),
		LastRefreshedAt:     now.Add(2 * time.Hour),
		RefreshTokenPresent: true,
		Status:              binding.StatusStale,
	}
	if err := s.Put(ctx, b, []byte("v"), binding.PutCreate); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, b.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Account != b.Account || got.Scope != b.Scope {
		t.Errorf("optional strings lost: got %+v", got)
	}
	if !got.LastUsedAt.Equal(b.LastUsedAt) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, b.LastUsedAt)
	}
	if !got.LastRefreshedAt.Equal(b.LastRefreshedAt) {
		t.Errorf("LastRefreshedAt = %v, want %v", got.LastRefreshedAt, b.LastRefreshedAt)
	}
	if !got.RefreshTokenPresent {
		t.Errorf("RefreshTokenPresent lost")
	}
	if got.Status != binding.StatusStale {
		t.Errorf("Status = %q, want stale", got.Status)
	}
}

func TestVaultStore_ResolverFor(t *testing.T) {
	// ResolverFor is the seam the executor calls during action
	// execution: nil on miss/ambiguous, a wired VaultResolver on hit.
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, binding.Binding{
		Name: "api_key/linear/team", Kind: "api_key", Service: "linear",
		Identity: "team", ConnectorFQN: "github://aileron/linear",
	}, []byte("lin-secret"), binding.PutCreate); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Miss → nil.
	if r := s.ResolverFor(ctx, "github://aileron/missing", "api_key"); r != nil {
		t.Errorf("ResolverFor on miss = %v, want nil", r)
	}

	// Hit → resolver returns the bound bytes.
	r := s.ResolverFor(ctx, "github://aileron/linear", "api_key")
	if r == nil {
		t.Fatal("ResolverFor on hit = nil")
	}
	cred, err := r.Resolve(ctx)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(cred.Value) != "lin-secret" {
		t.Errorf("Value = %q, want lin-secret", cred.Value)
	}
	if cred.Kind != "api_key" {
		t.Errorf("Kind = %q, want api_key", cred.Kind)
	}

	// Ambiguous → nil.
	if err := s.Put(ctx, binding.Binding{
		Name: "api_key/linear/personal", Kind: "api_key", Service: "linear",
		Identity: "personal", ConnectorFQN: "github://aileron/linear",
	}, []byte("v2"), binding.PutCreate); err != nil {
		t.Fatalf("seed second: %v", err)
	}
	if r := s.ResolverFor(ctx, "github://aileron/linear", "api_key"); r != nil {
		t.Errorf("ResolverFor on ambiguous = %v, want nil", r)
	}
}

func TestVaultStore_ResolverFor_NilSafety(t *testing.T) {
	var s *binding.VaultStore
	if r := s.ResolverFor(context.Background(), "x", "y"); r != nil {
		t.Errorf("nil receiver ResolverFor = %v, want nil", r)
	}
	s2 := &binding.VaultStore{} // nil Vault
	if r := s2.ResolverFor(context.Background(), "x", "y"); r != nil {
		t.Errorf("nil Vault ResolverFor = %v, want nil", r)
	}
}

func TestVaultStore_NilVault(t *testing.T) {
	s := &binding.VaultStore{}
	if _, err := s.List(context.Background()); err == nil {
		t.Error("nil Vault List = nil err; want failure")
	}
	if err := s.Put(context.Background(), binding.Binding{Name: "api_key/x/y"},
		[]byte("v"), binding.PutCreate); err == nil {
		t.Error("nil Vault Put = nil err; want failure")
	}
	if err := s.Delete(context.Background(), "api_key/x/y"); err == nil {
		t.Error("nil Vault Delete = nil err; want failure")
	}
	if _, err := s.Resolve(context.Background(), "x", "y"); err == nil {
		t.Error("nil Vault Resolve = nil err; want failure")
	}
	if _, err := s.Get(context.Background(), "api_key/x/y"); err == nil {
		t.Error("nil Vault Get = nil err; want failure")
	}
}

func TestVaultStore_ResolverFor_BranchesByKind(t *testing.T) {
	// api_key bindings get a VaultResolver; oauth2 bindings get an
	// OAuth2VaultResolver. The two have different runtime semantics —
	// the oauth2 one performs refresh — so the binding store has to
	// pick the right one per kind.
	s := newStore(t)
	ctx := context.Background()

	// Seed an api_key binding.
	if err := s.Put(ctx, binding.Binding{
		Name: "api_key/linear/team", Kind: "api_key", Service: "linear",
		Identity: "team", ConnectorFQN: "github://acme/linear",
	}, []byte("v"), binding.PutCreate); err != nil {
		t.Fatalf("Put api_key: %v", err)
	}
	// Seed an oauth2 binding (with a valid envelope so OAuth2VaultResolver.Resolve will succeed).
	envelope := `{"access_token":"at","refresh_token":"rt","expires_at":"2099-01-01T00:00:00Z","client_id":"cid","token_url":"https://example.test/t"}`
	if err := s.Put(ctx, binding.Binding{
		Name: "oauth2/google/work", Kind: "oauth2", Service: "google",
		Identity: "work", ConnectorFQN: "github://acme/google",
	}, []byte(envelope), binding.PutCreate); err != nil {
		t.Fatalf("Put oauth2: %v", err)
	}

	r1 := s.ResolverFor(ctx, "github://acme/linear", "api_key")
	if r1 == nil {
		t.Fatal("api_key ResolverFor = nil")
	}
	if _, ok := r1.(*credential.VaultResolver); !ok {
		t.Errorf("api_key resolver type = %T, want *credential.VaultResolver", r1)
	}

	r2 := s.ResolverFor(ctx, "github://acme/google", "oauth2")
	if r2 == nil {
		t.Fatal("oauth2 ResolverFor = nil")
	}
	if _, ok := r2.(*credential.OAuth2VaultResolver); !ok {
		t.Errorf("oauth2 resolver type = %T, want *credential.OAuth2VaultResolver", r2)
	}
}

// TestBinding_ScopeDriftLabelsRoundTrip: GrantedScopes, StaleReason,
// and MissingScopes must survive the vault's plaintext-label
// round-trip so listing surfaces them without unlocking.
func TestBinding_ScopeDriftLabelsRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	b := binding.Binding{
		Name:         "oauth2/google/work",
		Kind:         "oauth2",
		Service:      "google",
		Identity:     "work",
		ConnectorFQN: "github://acme/aileron-connector-google",
		GrantedScopes: []string{
			"https://www.googleapis.com/auth/gmail.readonly",
			"https://www.googleapis.com/auth/calendar.events",
		},
		StaleReason: binding.StaleReasonScopeDrift,
		MissingScopes: []string{
			"https://www.googleapis.com/auth/drive",
		},
		Status: binding.StatusStale,
	}
	if err := s.Put(ctx, b, []byte("envelope"), binding.PutCreate); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, b.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got.GrantedScopes, b.GrantedScopes) {
		t.Errorf("GrantedScopes = %v, want %v", got.GrantedScopes, b.GrantedScopes)
	}
	if got.StaleReason != b.StaleReason {
		t.Errorf("StaleReason = %q, want %q", got.StaleReason, b.StaleReason)
	}
	if !reflect.DeepEqual(got.MissingScopes, b.MissingScopes) {
		t.Errorf("MissingScopes = %v, want %v", got.MissingScopes, b.MissingScopes)
	}
	if got.Status != binding.StatusStale {
		t.Errorf("Status = %q, want stale", got.Status)
	}
}

// TestVaultStore_UpdateMetadata_PreservesValue: scope-drift detection
// updates only the metadata labels; the encrypted credential bytes
// must round-trip unchanged so a refresh-token-holding OAuth envelope
// is not clobbered when we mark it stale.
func TestVaultStore_UpdateMetadata_PreservesValue(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	original := []byte(`{"access_token":"at","refresh_token":"rt"}`)
	b := binding.Binding{
		Name: "oauth2/google/work", Kind: "oauth2", Service: "google",
		Identity: "work", ConnectorFQN: "github://acme/g",
		GrantedScopes: []string{"https://www.googleapis.com/auth/gmail.readonly"},
	}
	if err := s.Put(ctx, b, original, binding.PutCreate); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Flip to stale via UpdateMetadata.
	b.Status = binding.StatusStale
	b.StaleReason = binding.StaleReasonScopeDrift
	b.MissingScopes = []string{"https://www.googleapis.com/auth/drive"}
	if err := s.UpdateMetadata(ctx, b); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}
	// Metadata reflects the update.
	got, err := s.Get(ctx, b.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != binding.StatusStale {
		t.Errorf("Status = %q, want stale", got.Status)
	}
	// Value round-trips: read directly from the vault.
	secret, err := s.Vault.Get(ctx, string(b.Name))
	if err != nil {
		t.Fatalf("Vault.Get: %v", err)
	}
	if !reflect.DeepEqual(secret.Value, original) {
		t.Errorf("UpdateMetadata clobbered the credential bytes: got %q, want %q",
			secret.Value, original)
	}
}

// TestVaultStore_UpdateMetadata_NotFound: missing bindings return the
// stable sentinel so callers can branch on errors.Is.
func TestVaultStore_UpdateMetadata_NotFound(t *testing.T) {
	s := newStore(t)
	err := s.UpdateMetadata(context.Background(), binding.Binding{
		Name: "oauth2/google/missing", Kind: "oauth2",
	})
	if !errors.Is(err, binding.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestVaultStore_UpdateMetadata_RejectsEmptyName: defensive — the
// hook composes Binding values from List output and feeds them back
// to UpdateMetadata; an empty name shouldn't reach the vault.
func TestVaultStore_UpdateMetadata_RejectsEmptyName(t *testing.T) {
	s := newStore(t)
	if err := s.UpdateMetadata(context.Background(), binding.Binding{}); err == nil {
		t.Error("empty Name accepted")
	}
}

// TestVaultStore_UpdateMetadata_RejectsInvalidName: a malformed name
// (e.g. uppercase or path-traversal-like) gets rejected before any
// vault I/O.
func TestVaultStore_UpdateMetadata_RejectsInvalidName(t *testing.T) {
	s := newStore(t)
	err := s.UpdateMetadata(context.Background(), binding.Binding{Name: "BAD"})
	if err == nil {
		t.Error("invalid Name accepted")
	}
}

// TestVaultStore_UpdateMetadata_NilVault: parity with the other
// VaultStore methods' nil-vault guards.
func TestVaultStore_UpdateMetadata_NilVault(t *testing.T) {
	var s *binding.VaultStore
	if err := s.UpdateMetadata(context.Background(),
		binding.Binding{Name: "api_key/x/y"}); err == nil {
		t.Error("nil receiver UpdateMetadata = nil err")
	}
	s2 := &binding.VaultStore{}
	if err := s2.UpdateMetadata(context.Background(),
		binding.Binding{Name: "api_key/x/y"}); err == nil {
		t.Error("nil Vault UpdateMetadata = nil err")
	}
}

func TestVaultStore_ResolverFor_UnknownKindReturnsNil(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, binding.Binding{
		Name: "x509/treasury/prod", Kind: "x509", Service: "treasury",
		Identity: "prod", ConnectorFQN: "github://acme/treasury",
	}, []byte("v"), binding.PutCreate); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if r := s.ResolverFor(ctx, "github://acme/treasury", "x509"); r != nil {
		t.Errorf("unknown-kind ResolverFor = %v, want nil", r)
	}
}
