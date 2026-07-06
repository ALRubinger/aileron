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
// access_key_id is persisted on the binding. The source carries no region:
// the signing region and service are derived from the resolved upstream host
// at egress (#1978), so there is no operator-supplied region to persist.
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
	if b.AccessKeyId == nil || *b.AccessKeyId != "AKIAEASTEXAMPLE" {
		t.Errorf("AccessKeyId = %v, want AKIAEASTEXAMPLE", b.AccessKeyId)
	}

	// The response no longer carries a region field at all.
	if strings.Contains(rec.Body.String(), `"region"`) {
		t.Errorf("response still carries a region field: %s", rec.Body.String())
	}

	// The secret access key must never appear in the response surface.
	if strings.Contains(rec.Body.String(), "wJalrSecretAccessKeyEXAMPLE") {
		t.Errorf("response leaked the secret access key: %s", rec.Body.String())
	}

	// And the stored binding's metadata carries the access key id.
	stored, err := srv.bindings.Get(context.Background(), binding.Name("aws_sigv4/athena/prod-east"))
	if err != nil {
		t.Fatalf("Get stored binding: %v", err)
	}
	if stored.AccessKeyID != "AKIAEASTEXAMPLE" {
		t.Errorf("stored akid = %q, want AKIAEASTEXAMPLE", stored.AccessKeyID)
	}
}

// TestSetupBindings_RejectsLegacyRegion is the #1978 regression: an aws_sigv4
// setup payload that still carries a `region` field is rejected with a
// structured 400 rather than silently accepted with the stray field dropped.
// The operator surface no longer has any place to supply a region, so no
// second copy of the region can drift from the host being signed for.
func TestSetupBindings_RejectsLegacyRegion(t *testing.T) {
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
					"access_key_id": "AKIAEASTEXAMPLE",
					"region": "us-east-1"
				}
			}
		]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec, httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Errorf("expected invalid_request for legacy region field: %s", rec.Body.String())
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
			{"identity": "prod", "source": {"kind": "aws_sigv4", "access_key_id": "AKIAEASTEXAMPLE"}}
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

// TestRebind_ReplacesAWSSigV4SecretAndAccessKeyID exercises the rebind twin of
// the gate: aws_sigv4 rebind rotates the secret and may update the non-secret
// access_key_id. The source carries no region (#1978).
func TestRebind_ReplacesAWSSigV4SecretAndAccessKeyID(t *testing.T) {
	srv, _ := bindingTestServer(t)
	installFakeAPIKeyConnector(t, srv.installer.Store, "github://acme/athena", "1.0.0", "aws_sigv4")

	// Seed via setup.
	setupBody := `{
		"connector_fqn": "github://acme/athena",
		"bindings": [{"identity":"prod","source":{"kind":"aws_sigv4","value":"old-secret","access_key_id":"AKIAOLD"}}]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec, httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(setupBody)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rebindBody := `{"source":{"kind":"aws_sigv4","value":"new-secret","access_key_id":"AKIANEW"}}`
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
	if stored.AccessKeyID != "AKIANEW" {
		t.Errorf("after rebind akid = %q, want AKIANEW", stored.AccessKeyID)
	}
}

// TestRebind_RejectsLegacyRegion is the rebind twin of the #1978 regression:
// a rebind payload that still carries a `region` field is rejected with a
// structured 400 rather than silently accepted.
func TestRebind_RejectsLegacyRegion(t *testing.T) {
	srv, _ := bindingTestServer(t)
	installFakeAPIKeyConnector(t, srv.installer.Store, "github://acme/athena", "1.0.0", "aws_sigv4")

	setupBody := `{
		"connector_fqn": "github://acme/athena",
		"bindings": [{"identity":"prod","source":{"kind":"aws_sigv4","value":"old-secret","access_key_id":"AKIAOLD"}}]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec, httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(setupBody)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rebindBody := `{"source":{"kind":"aws_sigv4","value":"new-secret","access_key_id":"AKIANEW","region":"eu-west-1"}}`
	rec = httptest.NewRecorder()
	srv.RebindBinding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/aws_sigv4/athena/prod/rebind", strings.NewReader(rebindBody)),
		api.BindingName("aws_sigv4/athena/prod"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rebind status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Errorf("expected invalid_request for legacy region field: %s", rec.Body.String())
	}
}
