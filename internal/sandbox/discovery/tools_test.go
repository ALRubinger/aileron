package discovery

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
)

func TestToolsTextRendersConnectorsFromActions(t *testing.T) {
	got := string(ToolsText(testActions()))

	for _, want := range []string{
		"google\tgithub://ALRubinger/aileron-connector-google -- installed Aileron connector used by actions: move-file, send-email\n",
		"release-tool\thub://acme/internal/release-tool -- installed Aileron connector used by actions: move-file\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("tools.txt missing %q:\n%s", want, got)
		}
	}
}

func TestConnectorToolsRendersActionHelp(t *testing.T) {
	tools := ConnectorTools(testActions())
	if len(tools) != 2 {
		t.Fatalf("ConnectorTools len = %d, want 2: %#v", len(tools), tools)
	}
	google := tools[0]
	if google.Name != "google" || google.FQN != "github://ALRubinger/aileron-connector-google" {
		t.Fatalf("first tool = %#v, want google connector", google)
	}
	if len(google.Actions) != 2 {
		t.Fatalf("google actions = %#v, want two actions", google.Actions)
	}
	if got, want := google.Actions[1].Inputs[0].Name, "to"; got != want {
		t.Fatalf("google send-email input = %q, want %q", got, want)
	}
}

func TestShimScriptsRenderHelpAndFailClosed(t *testing.T) {
	scripts := ShimScripts(testActions())
	script := string(scripts["google"])
	for _, want := range []string{
		"#!/bin/sh\n",
		"Aileron connector shim: google\n",
		"send-email - send email\n",
		"--to <string> (required) - recipient\n",
		"execution is not wired yet",
		"exit 64\n",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("shim script missing %q:\n%s", want, script)
		}
	}
}

func TestToolsTextSkipsEmptyInputs(t *testing.T) {
	got := ToolsText([]action.LoadedAction{
		{},
		{Manifest: &action.Manifest{Name: "no-connectors"}},
		{Manifest: &action.Manifest{
			Name: "blank-connector",
			Requires: action.Requires{Connectors: []action.RequiresConnector{
				{Name: " "},
			}},
		}},
	})
	if got != nil {
		t.Fatalf("ToolsText = %q, want nil", got)
	}
}

func TestToolNameSanitizesFallbackFQN(t *testing.T) {
	if got := toolName("not a valid fqn"); got != "not-a-valid-fqn" {
		t.Fatalf("toolName fallback = %q", got)
	}
}

func TestConnectorToolsDisambiguatesToolNames(t *testing.T) {
	tools := ConnectorTools([]action.LoadedAction{
		{Manifest: &action.Manifest{
			Name: "one",
			Requires: action.Requires{Connectors: []action.RequiresConnector{
				{Name: "github://acme/aileron-connector-google"},
			}},
		}},
		{Manifest: &action.Manifest{
			Name: "two",
			Requires: action.Requires{Connectors: []action.RequiresConnector{
				{Name: "hub://acme/connector-google"},
			}},
		}},
	})
	if len(tools) != 2 {
		t.Fatalf("ConnectorTools len = %d, want 2: %#v", len(tools), tools)
	}
	if tools[0].Name != "google" || tools[1].Name != "google-2" {
		t.Fatalf("tool names = %q, %q; want google, google-2", tools[0].Name, tools[1].Name)
	}
}

func testActions() []action.LoadedAction {
	return []action.LoadedAction{
		{Manifest: &action.Manifest{
			Name: "send-email",
			Match: action.Match{
				Intent: "send email",
			},
			Inputs: []action.Input{{
				Name:        "to",
				Type:        "string",
				Description: "recipient",
			}},
			Requires: action.Requires{Connectors: []action.RequiresConnector{
				{Name: "github://ALRubinger/aileron-connector-google"},
			}},
		}},
		{Manifest: &action.Manifest{
			Name: "move-file",
			Match: action.Match{
				Intent: "move file",
			},
			Requires: action.Requires{Connectors: []action.RequiresConnector{
				{Name: "github://ALRubinger/aileron-connector-google"},
				{Name: "hub://acme/internal/release-tool"},
			}},
		}},
	}
}
