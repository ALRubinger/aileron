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
		EmitMechanism: "A",
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
	if hb.EmitMechanism != binding.EmitMechanismA {
		t.Errorf("emit mechanism = %q, want A", hb.EmitMechanism)
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

func TestToHostBinding_EmitMechanismBCarriesSentinel(t *testing.T) {
	// A mechanism-B entry's sentinel value and env adapt onto the binding
	// (#1247) so the launcher and the proxy read one source of truth.
	e := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeBearer,
		EmitMechanism: "B",
		Sentinel:      &Sentinel{Value: "sent_example", Env: "EXAMPLE_TOKEN"},
	}
	hb, err := e.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding: %v", err)
	}
	if hb.EmitMechanism != binding.EmitMechanismB {
		t.Errorf("emit mechanism = %q, want B", hb.EmitMechanism)
	}
	if hb.SentinelValue != "sent_example" {
		t.Errorf("SentinelValue = %q, want sent_example", hb.SentinelValue)
	}
	if hb.SentinelEnv != "EXAMPLE_TOKEN" {
		t.Errorf("SentinelEnv = %q, want EXAMPLE_TOKEN", hb.SentinelEnv)
	}
}

func TestToHostBinding_EmitMechanismBWithoutSentinelErrors(t *testing.T) {
	// A mechanism-B entry that reached adaptation with no sentinel block is
	// rejected by the constructor — a B binding that could never be planted
	// or recognized must fail loudly rather than ship.
	e := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeBearer,
		EmitMechanism: "B",
	}
	if _, err := e.ToHostBinding(); err == nil {
		t.Fatal("ToHostBinding for mechanism-B entry with no sentinel = nil error, want error")
	}
}

func TestToHostBinding_MechanismACarriesNoSentinel(t *testing.T) {
	// A mechanism-A entry adapts with empty sentinel fields.
	e := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/example",
		Scheme:        binding.SchemeBearer,
		EmitMechanism: "A",
	}
	hb, err := e.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding: %v", err)
	}
	if hb.SentinelValue != "" || hb.SentinelEnv != "" {
		t.Errorf("mechanism-A binding carries a sentinel (%q,%q), want none", hb.SentinelValue, hb.SentinelEnv)
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
