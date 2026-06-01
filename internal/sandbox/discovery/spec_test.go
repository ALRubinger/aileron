package discovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
)

func TestSpecToolsTextRendersConnectorOperations(t *testing.T) {
	got, err := SpecToolsText(testSpecs())
	if err != nil {
		t.Fatalf("SpecToolsText: %v", err)
	}
	want := "google\tgithub://acme/aileron-connector-google -- Aileron connector operations: gmail.messages.search, gmail.messages.send\n"
	if string(got) != want {
		t.Fatalf("tools.txt = %q, want %q", got, want)
	}
}

func TestSpecShimScriptsRenderHelpAndDispatch(t *testing.T) {
	scripts, err := SpecShimScripts(testSpecs())
	if err != nil {
		t.Fatalf("SpecShimScripts: %v", err)
	}
	script := string(scripts["google"])
	for _, want := range []string{
		"#!/bin/sh\n",
		"Aileron connector shim: google\n",
		"Connector: github://acme/aileron-connector-google\n",
		"gmail.messages.send - Send a Gmail message\n",
		"target: POST /gmail/v1/users/me/messages/send\n",
		"policy: credential=oauth2 approval=required idempotency=not_idempotent\n",
		"--to <string> (required) - recipient",
		"/connector-operations/run",
		"X-Aileron-Session-Id: $AILERON_SESSION_ID",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func TestSpecShimScriptExecutesOperationThroughDaemonAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generated shim execution is POSIX shell behavior")
	}
	scripts, err := SpecShimScripts(testSpecs())
	if err != nil {
		t.Fatalf("SpecShimScripts: %v", err)
	}
	dir := t.TempDir()
	shimPath := filepath.Join(dir, "google")
	if err := os.WriteFile(shimPath, scripts["google"], 0o755); err != nil {
		t.Fatal(err)
	}
	capturePath := filepath.Join(dir, "wget-args.txt")
	wgetPath := filepath.Join(dir, "wget")
	if err := os.WriteFile(wgetPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+capturePath+"\nprintf '{\"ok\":true}\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/sh", shimPath, "gmail.messages.send", "--args", `{"to":"a@example.com"}`, "--json")
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AILERON_API_URL=http://host.docker.internal:48123/v1",
		"AILERON_TOKEN=test-token",
		"AILERON_SESSION_ID=sess-sandbox",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim execution failed: %v\n%s", err, out)
	}
	if got := string(out); got != "{\"ok\":true}\n" {
		t.Fatalf("shim output = %q", got)
	}
	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	args := string(captured)
	for _, want := range []string{
		"--header\nContent-Type: application/json\n",
		"--header\nAuthorization: Bearer test-token\n",
		"--header\nX-Aileron-Session-Id: sess-sandbox\n",
		"--post-data\n{\"connector_fqn\":\"github://acme/aileron-connector-google\",\"tool\":\"google\",\"operation\":\"gmail.messages.send\",\"args\":{\"to\":\"a@example.com\"}}\n",
		"http://host.docker.internal:48123/v1/connector-operations/run\n",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("wget args missing %q:\n%s", want, args)
		}
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
