package action

import (
	"errors"
	"reflect"
	"testing"
)

func capabilityManifest() *Manifest {
	return &Manifest{
		Name:    "ship-update",
		Version: "1.0.0",
		Requires: Requires{
			Connectors: []RequiresConnector{
				{
					Name:         "github://aileron/slack",
					Version:      "1.2.0",
					Hash:         "sha256:abc",
					Capabilities: []string{"chat:write", "channels:read"},
				},
				{
					Name:         "github://aileron/git",
					Version:      "2.1.0",
					Hash:         "sha256:def",
					Capabilities: []string{"read"},
				},
			},
		},
	}
}

func TestEnforceCapability_DeclaredAllowed(t *testing.T) {
	m := capabilityManifest()
	if err := EnforceCapability(m, "github://aileron/slack", "chat:write"); err != nil {
		t.Errorf("EnforceCapability(declared) error = %v, want nil", err)
	}
	if err := EnforceCapability(m, "github://aileron/git", "read"); err != nil {
		t.Errorf("EnforceCapability(read) error = %v, want nil", err)
	}
}

func TestEnforceCapability_OutOfSubsetDenied(t *testing.T) {
	m := capabilityManifest()
	err := EnforceCapability(m, "github://aileron/slack", "users:read")
	if err == nil {
		t.Fatal("EnforceCapability succeeded; want capability_denied")
	}
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if aerr.Class != ClassCapabilityDenied {
		t.Errorf("Class = %s, want %s", aerr.Class, ClassCapabilityDenied)
	}
	if aerr.Boundary != BoundaryAction {
		t.Errorf("Boundary = %s, want action", aerr.Boundary)
	}
	if aerr.Details["requested"] != "users:read" {
		t.Errorf("Details.requested = %v", aerr.Details["requested"])
	}
	if !reflect.DeepEqual(aerr.Details["declared_subset"], []string{"chat:write", "channels:read"}) {
		t.Errorf("Details.declared_subset = %v", aerr.Details["declared_subset"])
	}
}

func TestEnforceCapability_UndeclaredConnectorDenied(t *testing.T) {
	m := capabilityManifest()
	err := EnforceCapability(m, "github://other/foo", "do:thing")
	if err == nil {
		t.Fatal("EnforceCapability succeeded; want capability_denied")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassCapabilityDenied {
		t.Fatalf("expected ClassCapabilityDenied, got %v", err)
	}
	if aerr.Message == "" || aerr.Details["connector"] != "github://other/foo" {
		t.Errorf("expected denial details to name the connector; got %+v", aerr.Details)
	}
}

func TestEnforceCapability_NilManifestDenies(t *testing.T) {
	err := EnforceCapability(nil, "github://aileron/slack", "chat:write")
	if err == nil {
		t.Fatal("EnforceCapability(nil) succeeded; want denial")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassCapabilityDenied {
		t.Errorf("expected capability_denied, got %v", err)
	}
}
