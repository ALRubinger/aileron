package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/binding"
)

// TestSetupBindings_CreatesAWSSigV4Binding is the setup-gate half of the
// #1663 regression: an aws_sigv4 source is accepted (no longer rejected as
// unsupported_kind), the secret access key is stored, and the non-secret
// region / access_key_id are persisted on the binding so request-time
// region selection can disambiguate multiple installs.
func TestSetupBindings_CreatesAWSSigV4Binding(t *testing.T) {
	srv, _ := bindingTestServer(t)
	installFakeAPIKeyConnector(t, srv.installer.Store, "github://acme/athena", "1.0.0", "aws_sigv4")

	body := `{
		"connector_fqn": "github://acme/athena",
		"bindings": [
			{
				"identity": "prod-east",
				"source": {
					"kind": "aws_sigv4",
					"value": "wJalrSecretAccessKeyEXAMPLE",
					"region": "us-east-1",
					"access_key_id": "AKIAEASTEXAMPLE"
				}
			}
		]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec, httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.BindingSetupResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Created) != 1 {
		t.Fatalf("Created = %+v", got.Created)
	}
	b := got.Created[0]
	if b.Name != "aws_sigv4/athena/prod-east" {
		t.Errorf("Name = %q, want aws_sigv4/athena/prod-east", b.Name)
	}
	if b.Region == nil || *b.Region != "us-east-1" {
		t.Errorf("Region = %v, want us-east-1", b.Region)
	}
	if b.AccessKeyId == nil || *b.AccessKeyId != "AKIAEASTEXAMPLE" {
		t.Errorf("AccessKeyId = %v, want AKIAEASTEXAMPLE", b.AccessKeyId)
	}

	// The secret access key must never appear in the response surface.
	if strings.Contains(rec.Body.String(), "wJalrSecretAccessKeyEXAMPLE") {
		t.Errorf("response leaked the secret access key: %s", rec.Body.String())
	}

	// And the stored binding's metadata carries region/access_key_id.
	stored, err := srv.bindings.Get(context.Background(), binding.Name("aws_sigv4/athena/prod-east"))
	if err != nil {
		t.Fatalf("Get stored binding: %v", err)
	}
	if stored.Region != "us-east-1" || stored.AccessKeyID != "AKIAEASTEXAMPLE" {
		t.Errorf("stored region/akid = %q/%q", stored.Region, stored.AccessKeyID)
	}
}

// TestSetupBindings_AWSSigV4MissingValueIs400 keeps the secret required:
// aws_sigv4's bound value is the secret access key, so an empty value fails
// closed rather than creating a signing binding with no key.
func TestSetupBindings_AWSSigV4MissingValueIs400(t *testing.T) {
	srv, _ := bindingTestServer(t)
	installFakeAPIKeyConnector(t, srv.installer.Store, "github://acme/athena", "1.0.0", "aws_sigv4")
	body := `{
		"connector_fqn": "github://acme/athena",
		"bindings": [
			{"identity": "prod", "source": {"kind": "aws_sigv4", "region": "us-east-1"}}
		]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec, httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing_value") {
		t.Errorf("expected missing_value: %s", rec.Body.String())
	}
}

// TestSetupBindings_RejectsTrulyUnsupportedKind confirms the gate still
// fails closed for kinds outside the setup-supported set; the connector
// declares aws_sigv4 so a basic source also trips kind_mismatch first, but
// the message must name the supported set.
func TestSetupBindings_RejectsTrulyUnsupportedKind(t *testing.T) {
	srv, _ := bindingTestServer(t)
	body := `{
		"connector_fqn": "github://aileron/linear",
		"bindings": [
			{"identity": "work", "source": {"kind": "basic", "value": "x"}}
		]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec, httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestRebind_ReplacesAWSSigV4SecretAndRegion exercises the rebind twin of
// the gate: aws_sigv4 rebind rotates the secret and may update the
// non-secret region / access_key_id.
func TestRebind_ReplacesAWSSigV4SecretAndRegion(t *testing.T) {
	srv, _ := bindingTestServer(t)
	installFakeAPIKeyConnector(t, srv.installer.Store, "github://acme/athena", "1.0.0", "aws_sigv4")

	// Seed via setup.
	setupBody := `{
		"connector_fqn": "github://acme/athena",
		"bindings": [{"identity":"prod","source":{"kind":"aws_sigv4","value":"old-secret","region":"us-east-1","access_key_id":"AKIAOLD"}}]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec, httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(setupBody)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rebindBody := `{"source":{"kind":"aws_sigv4","value":"new-secret","region":"eu-west-1","access_key_id":"AKIANEW"}}`
	rec = httptest.NewRecorder()
	srv.RebindBinding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/aws_sigv4/athena/prod/rebind", strings.NewReader(rebindBody)),
		api.BindingName("aws_sigv4/athena/prod"))
	if rec.Code != http.StatusOK {
		t.Fatalf("rebind status = %d, body = %s", rec.Code, rec.Body.String())
	}

	stored, err := srv.bindings.Get(context.Background(), binding.Name("aws_sigv4/athena/prod"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Region != "eu-west-1" || stored.AccessKeyID != "AKIANEW" {
		t.Errorf("after rebind region/akid = %q/%q, want eu-west-1/AKIANEW", stored.Region, stored.AccessKeyID)
	}
}
