package discovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestShimScriptsRenderHelpAndDispatch(t *testing.T) {
	scripts := ShimScripts(testActions())
	script := string(scripts["google"])
	for _, want := range []string{
		"#!/bin/sh\n",
		"Aileron connector shim: google\n",
		"send-email - send email\n",
		"--to <string> (required) - recipient\n",
		"Run `google <action-name> --args '<json-object>'`",
		"--post-data \"$body\" \"$url\"",
		"X-Aileron-Session-Id: $AILERON_SESSION_ID",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("shim script missing %q:\n%s", want, script)
		}
	}
}

func TestShimScriptExecutesInstalledActionThroughDaemonAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generated shim execution is POSIX shell behavior")
	}
	scripts := ShimScripts(testActions())
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

	cmd := exec.Command("/bin/sh", shimPath, "send-email", "--args", `{"to":"a@example.com"}`, "--json")
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
		"--post-data\n{\"args\":{\"to\":\"a@example.com\"}}\n",
		"http://host.docker.internal:48123/v1/actions/send-email/run\n",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("wget args missing %q:\n%s", want, args)
		}
	}
}

func TestShimScriptExecutesInstalledActionWithoutSessionID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generated shim execution is POSIX shell behavior")
	}
	scripts := ShimScripts(testActions())
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

	cmd := exec.Command("/bin/sh", shimPath, "send-email", "--args", `{"to":"a@example.com"}`, "--json")
	cmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"AILERON_API_URL=http://host.docker.internal:48123/v1",
		"AILERON_TOKEN=test-token",
	}
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
	if args := string(captured); strings.Contains(args, "X-Aileron-Session-Id") {
		t.Fatalf("wget args included session header without session id:\n%s", args)
	}
}

func TestShimScriptRejectsUnknownAction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generated shim execution is POSIX shell behavior")
	}
	scripts := ShimScripts(testActions())
	dir := t.TempDir()
	shimPath := filepath.Join(dir, "google")
	if err := os.WriteFile(shimPath, scripts["google"], 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", shimPath, "missing-action")
	cmd.Env = append(os.Environ(), "AILERON_API_URL=http://host.docker.internal:48123/v1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("shim succeeded unexpectedly:\n%s", out)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 64 {
		t.Fatalf("shim exit = %v, want 64; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "Unknown action for google: missing-action") {
		t.Fatalf("unknown action output = %s", out)
	}
}

func TestShimScriptFailsWithoutAILERONAPIURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generated shim execution is POSIX shell behavior")
	}
	scripts := ShimScripts(testActions())
	dir := t.TempDir()
	shimPath := filepath.Join(dir, "google")
	if err := os.WriteFile(shimPath, scripts["google"], 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", shimPath, "send-email")
	cmd.Env = append(os.Environ(), "AILERON_API_URL=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("shim succeeded unexpectedly:\n%s", out)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 69 {
		t.Fatalf("shim exit = %v, want 69; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "AILERON_API_URL") {
		t.Fatalf("missing API URL output = %s", out)
	}
}

func TestShimScriptFailsWithoutWget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generated shim execution is POSIX shell behavior")
	}
	scripts := ShimScripts(testActions())
	dir := t.TempDir()
	shimPath := filepath.Join(dir, "google")
	if err := os.WriteFile(shimPath, scripts["google"], 0o755); err != nil {
		t.Fatal(err)
	}
	emptyPathDir := filepath.Join(dir, "empty-path")
	if err := os.Mkdir(emptyPathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", shimPath, "send-email")
	cmd.Env = append(os.Environ(),
		"PATH="+emptyPathDir,
		"AILERON_API_URL=http://host.docker.internal:48123/v1",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("shim succeeded unexpectedly:\n%s", out)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 69 {
		t.Fatalf("shim exit = %v, want 69; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "wget") {
		t.Fatalf("missing wget output = %s", out)
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
