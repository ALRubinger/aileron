package proxybinding

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
)

// A non-GitHub host entry (Linear) adapts to a host binding whose host,
// scheme, credential-ref, and header-template params match the descriptor.
func TestToHostBinding_LinearMapsParams(t *testing.T) {
	e := Entry{
		Host:          "api.linear.app",
		CredentialRef: "user/linear",
		Scheme:        binding.SchemeHeaderTemplate,
		EmitMechanism: "inject",
		Header:        "Authorization",
		Template:      "{token}",
	}
	hb, err := e.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding: %v", err)
	}
	if hb.HostPattern != "api.linear.app" {
		t.Errorf("host = %q, want api.linear.app", hb.HostPattern)
	}
	if hb.Scheme != binding.SchemeHeaderTemplate {
		t.Errorf("scheme = %q, want header-template", hb.Scheme)
	}
	if hb.CredentialRef != "user/linear" {
		t.Errorf("credential_ref = %q, want user/linear", hb.CredentialRef)
	}
	if hb.HeaderName != "Authorization" || hb.HeaderTemplate != "{token}" {
		t.Errorf("header params = (%q,%q), want (Authorization,{token})", hb.HeaderName, hb.HeaderTemplate)
	}
	if hb.EmitMechanism != binding.EmitMechanismInject {
		t.Errorf("emit mechanism = %q, want inject", hb.EmitMechanism)
	}
}

func TestToHostBinding_BasicCarriesUsername(t *testing.T) {
	e := Entry{
		Host:          "git.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeBasic,
		Username:      "x-access-token",
	}
	hb, err := e.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding: %v", err)
	}
	if hb.BasicUsername != "x-access-token" {
		t.Errorf("basic username = %q, want x-access-token", hb.BasicUsername)
	}
}

func TestToHostBinding_QueryParamCarriesName(t *testing.T) {
	e := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeQueryParam,
		QueryParam:    "api_key",
	}
	hb, err := e.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding: %v", err)
	}
	if hb.QueryParamName != "api_key" {
		t.Errorf("query param name = %q, want api_key", hb.QueryParamName)
	}
}

func TestToHostBinding_SigV4ResignCarriesParams(t *testing.T) {
	// A sigv4-resign entry's access key id, region, and service adapt onto
	// the binding. The secret access key is never a descriptor field; it
	// resolves daemon-side from the credential ref.
	e := Entry{
		Host:          "s3.amazonaws.com",
		CredentialRef: "user/aws",
		Scheme:        binding.SchemeSigV4Resign,
		AccessKeyID:   "AKIDEXAMPLE",
		Region:        "us-east-1",
		Service:       "s3",
	}
	hb, err := e.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding: %v", err)
	}
	if hb.Scheme != binding.SchemeSigV4Resign {
		t.Errorf("scheme = %q, want sigv4-resign", hb.Scheme)
	}
	if hb.AccessKeyID != "AKIDEXAMPLE" || hb.Region != "us-east-1" || hb.Service != "s3" {
		t.Errorf("sigv4 params = (%q,%q,%q), want (AKIDEXAMPLE,us-east-1,s3)",
			hb.AccessKeyID, hb.Region, hb.Service)
	}
}

func TestToHostBinding_SigV4ResignRequiresAccessKeyID(t *testing.T) {
	// A sigv4-resign entry with no access_key_id is rejected by the
	// constructor (fail closed, no malformed binding ships).
	missing := Entry{
		Host:          "s3.amazonaws.com",
		CredentialRef: "user/aws",
		Scheme:        binding.SchemeSigV4Resign,
		Region:        "us-east-1",
		Service:       "s3",
		// AccessKeyID omitted.
	}
	if _, err := missing.ToHostBinding(); err == nil {
		t.Fatal("ToHostBinding for sigv4-resign with no access_key_id = nil error, want error")
	}

	// Region and service are now optional: an entry with only an access_key_id
	// adapts successfully because the scope is host-derived at egress.
	noScope := Entry{
		Host:          "s3.amazonaws.com",
		CredentialRef: "user/aws",
		Scheme:        binding.SchemeSigV4Resign,
		AccessKeyID:   "AKIDEXAMPLE",
		// Region and Service omitted.
	}
	hb, err := noScope.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding for sigv4-resign with only an access_key_id: %v", err)
	}
	if hb.AccessKeyID != "AKIDEXAMPLE" || hb.Region != "" || hb.Service != "" {
		t.Errorf("sigv4 params = (%q,%q,%q), want (AKIDEXAMPLE,,)",
			hb.AccessKeyID, hb.Region, hb.Service)
	}
}

func TestToHostBinding_SentinelSwapCarriesSentinel(t *testing.T) {
	// A sentinel-swap entry's sentinel value and env adapt onto the binding
	// so the launcher and the proxy read one source of truth.
	e := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeBearer,
		EmitMechanism: "sentinel-swap",
		Sentinel:      &Sentinel{Value: "sent_example", Env: "EXAMPLE_TOKEN"},
	}
	hb, err := e.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding: %v", err)
	}
	if hb.EmitMechanism != binding.EmitMechanismSentinelSwap {
		t.Errorf("emit mechanism = %q, want sentinel-swap", hb.EmitMechanism)
	}
	if hb.SentinelValue != "sent_example" {
		t.Errorf("SentinelValue = %q, want sent_example", hb.SentinelValue)
	}
	if hb.SentinelEnv != "EXAMPLE_TOKEN" {
		t.Errorf("SentinelEnv = %q, want EXAMPLE_TOKEN", hb.SentinelEnv)
	}
}

func TestToHostBinding_SentinelSwapWithoutSentinelErrors(t *testing.T) {
	// A sentinel-swap entry that reached adaptation with no sentinel block is
	// rejected by the constructor — a sentinel-swap binding that could never
	// be planted or recognized must fail loudly rather than ship.
	e := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeBearer,
		EmitMechanism: "sentinel-swap",
	}
	if _, err := e.ToHostBinding(); err == nil {
		t.Fatal("ToHostBinding for sentinel-swap entry with no sentinel = nil error, want error")
	}
}

func TestToHostBinding_InjectCarriesNoSentinel(t *testing.T) {
	// An inject entry adapts with empty sentinel fields.
	e := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeBearer,
		EmitMechanism: "inject",
	}
	hb, err := e.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding: %v", err)
	}
	if hb.SentinelValue != "" || hb.SentinelEnv != "" {
		t.Errorf("inject binding carries a sentinel (%q,%q), want none", hb.SentinelValue, hb.SentinelEnv)
	}
}

func TestToHostBinding_TrustContractCarriesScope(t *testing.T) {
	e := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeBearer,
		Effect:        "read",
		AllowedHosts:  []string{"API.Example.com", "other.example.com"},
	}
	hb, err := e.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding: %v", err)
	}
	if hb.Effect != binding.EffectRead {
		t.Errorf("effect = %q, want read", hb.Effect)
	}
	want := []string{"api.example.com", "other.example.com"}
	if len(hb.AllowedHosts) != len(want) {
		t.Fatalf("allowed hosts = %v, want %v", hb.AllowedHosts, want)
	}
	for i := range want {
		if hb.AllowedHosts[i] != want[i] {
			t.Errorf("allowed host[%d] = %q, want %q", i, hb.AllowedHosts[i], want[i])
		}
	}
}

func TestToHostBinding_EmptyTrustScopeIsUnconstrained(t *testing.T) {
	e := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeBearer,
	}
	hb, err := e.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding: %v", err)
	}
	if hb.Effect != "" || len(hb.AllowedHosts) != 0 {
		t.Errorf("scope = (effect %q, hosts %v), want unconstrained", hb.Effect, hb.AllowedHosts)
	}
}

func TestToHostBinding_UnknownEffectErrors(t *testing.T) {
	e := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeBearer,
		Effect:        "mutate",
	}
	if _, err := e.ToHostBinding(); err == nil {
		t.Fatal("ToHostBinding with unknown effect = nil error, want error")
	}
}

// An empty entry slice produces a nil table, which internal/binding treats
// as a valid empty table whose Match always misses (passthrough preserved).
func TestToHostBindings_EmptyIsNilPassthrough(t *testing.T) {
	table, err := ToHostBindings(nil)
	if err != nil {
		t.Fatalf("ToHostBindings(nil): %v", err)
	}
	if table != nil {
		t.Errorf("ToHostBindings(nil) = %v, want nil table", table)
	}
	if _, ok := table.Match("api.linear.app"); ok {
		t.Error("nil table matched a host; want passthrough miss")
	}
}

// An invalid entry surfaces an error from the adapter rather than producing
// a malformed binding.
func TestToHostBindings_InvalidEntryErrors(t *testing.T) {
	_, err := ToHostBindings([]Entry{{
		Host:          "api.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeBasic, // basic with no username is invalid
	}})
	if err == nil {
		t.Fatal("ToHostBindings with invalid entry = nil error, want error")
	}
}
