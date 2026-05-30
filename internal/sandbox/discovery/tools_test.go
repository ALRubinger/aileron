package discovery

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
)

func TestToolsTextRendersConnectorsFromActions(t *testing.T) {
	got := string(ToolsText([]action.LoadedAction{
		{Manifest: &action.Manifest{
			Name: "send-email",
			Requires: action.Requires{Connectors: []action.RequiresConnector{
				{Name: "github://ALRubinger/aileron-connector-google"},
			}},
		}},
		{Manifest: &action.Manifest{
			Name: "move-file",
			Requires: action.Requires{Connectors: []action.RequiresConnector{
				{Name: "github://ALRubinger/aileron-connector-google"},
				{Name: "hub://acme/internal/release-tool"},
			}},
		}},
	}))

	for _, want := range []string{
		"google\tgithub://ALRubinger/aileron-connector-google -- installed Aileron connector used by actions: move-file, send-email\n",
		"release-tool\thub://acme/internal/release-tool -- installed Aileron connector used by actions: move-file\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("tools.txt missing %q:\n%s", want, got)
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
