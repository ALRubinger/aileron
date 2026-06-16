package discovery

import (
	"strings"
	"testing"

	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
)

func TestSpecConnectorToolsDerivesOperations(t *testing.T) {
	tools, err := SpecConnectorTools(testSpecs())
	if err != nil {
		t.Fatalf("SpecConnectorTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want one tool", tools)
	}
	tool := tools[0]
	if tool.Name != "google" || tool.FQN != "github://acme/aileron-connector-google" {
		t.Fatalf("tool = %#v, want google connector", tool)
	}
	if tool.Description != "Google APIs" {
		t.Fatalf("description = %q, want %q", tool.Description, "Google APIs")
	}
	if len(tool.Operations) != 2 {
		t.Fatalf("operations = %#v, want two", tool.Operations)
	}
	// Operations are sorted by name.
	if tool.Operations[0].Name != "gmail.messages.search" || tool.Operations[1].Name != "gmail.messages.send" {
		t.Fatalf("operation order = %q, %q; want search before send", tool.Operations[0].Name, tool.Operations[1].Name)
	}
	send := tool.Operations[1]
	if send.Method != "POST" || send.Path != "/gmail/v1/users/me/messages/send" {
		t.Fatalf("send target = %s %s", send.Method, send.Path)
	}
	if send.Approval != "required" || send.Credential != "oauth2" || send.Idempotency != "not_idempotent" {
		t.Fatalf("send policy = %#v", send)
	}
	if len(send.Inputs) != 1 || send.Inputs[0].Name != "to" || !send.Inputs[0].Required {
		t.Fatalf("send inputs = %#v, want required 'to'", send.Inputs)
	}
}

func TestSpecConnectorToolsRejectsConflictingNames(t *testing.T) {
	_, err := SpecConnectorTools([]connectorspec.Spec{
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: "github://acme/one"},
			Tools:         []connectorspec.Tool{{Name: "google", Operations: []connectorspec.Operation{{Name: "one"}}}},
		},
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: "github://acme/two"},
			Tools:         []connectorspec.Tool{{Name: "google", Operations: []connectorspec.Operation{{Name: "two"}}}},
		},
	})
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), `tool name conflict "google"`) {
		t.Fatalf("error = %v", err)
	}
}

func testSpecs() []connectorspec.Spec {
	return []connectorspec.Spec{{
		SchemaVersion: connectorspec.SchemaVersion,
		Connector: connectorspec.Connector{
			FQN:     "github://acme/aileron-connector-google",
			Version: "1.2.3",
		},
		Tools: []connectorspec.Tool{{
			Name:        "google",
			Description: "Google APIs",
			Operations: []connectorspec.Operation{
				{
					Name:        "gmail.messages.search",
					Summary:     "Search Gmail messages",
					Method:      "GET",
					Path:        "/gmail/v1/users/me/messages",
					Idempotency: "idempotent",
					Credential:  "oauth2",
				},
				{
					Name:        "gmail.messages.send",
					Summary:     "Send a Gmail message",
					Method:      "POST",
					Path:        "/gmail/v1/users/me/messages/send",
					Idempotency: "not_idempotent",
					Approval:    "required",
					Credential:  "oauth2",
					Inputs: []connectorspec.Input{{
						Name:        "to",
						Type:        "string",
						Required:    true,
						Description: "recipient",
					}},
				},
			},
		}},
	}}
}
