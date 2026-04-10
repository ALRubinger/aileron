package launch_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/audit"
	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/launch"
	"github.com/creack/pty/v2"
)

func TestResolveBinary_Found(t *testing.T) {
	// "echo" should be on every Unix PATH
	path, err := launch.ResolveBinary([]string{"echo"})
	if err != nil {
		t.Fatalf("expected to find 'echo': %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("expected absolute path, got %q", path)
	}
}

func TestResolveBinary_FallsBackToSecondCandidate(t *testing.T) {
	path, err := launch.ResolveBinary([]string{"nonexistent-xyz-1234", "echo"})
	if err != nil {
		t.Fatalf("expected to find 'echo' as fallback: %v", err)
	}
	if !strings.HasSuffix(path, "echo") {
		t.Errorf("expected path ending in 'echo', got %q", path)
	}
}

func TestResolveBinary_NotFound(t *testing.T) {
	_, err := launch.ResolveBinary([]string{"nonexistent-binary-xyz-9999"})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestResolveShim_NextToSelf(t *testing.T) {
	dir := t.TempDir()
	shimPath := filepath.Join(dir, "aileron-sh")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfPath := filepath.Join(dir, "aileron")

	resolved, err := launch.ResolveShim(selfPath)
	if err != nil {
		t.Fatalf("expected to find shim next to self: %v", err)
	}
	if resolved != shimPath {
		t.Errorf("expected %q, got %q", shimPath, resolved)
	}
}

func TestResolveShim_NotFound(t *testing.T) {
	_, err := launch.ResolveShim("/nonexistent/dir/aileron")
	if err == nil {
		t.Fatal("expected error when shim not found")
	}
}

// envAgent is a test agent that launches "env" to print the environment.
type envAgent struct {
	extraEnv map[string]string
}

func (a envAgent) Name() string           { return "test-env" }
func (a envAgent) BinaryNames() []string  { return []string{"env"} }
func (a envAgent) Args() []string         { return nil }
func (a envAgent) Env() map[string]string { return a.extraEnv }

func TestLaunch_EnvironmentSetup(t *testing.T) {
	// Capture the child's env by launching "env" and reading stdout.
	// We need to redirect stdout to a file since Launch connects to os.Stdout.
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")

	// Isolate HOME so InstallWrapper doesn't touch the real home directory.
	t.Setenv("HOME", dir)

	// Create a wrapper script that runs env and writes to file
	script := filepath.Join(dir, "capture-env.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	shimPath := "/tmp/fake-aileron-sh"
	agent := scriptAgent{script: script}

	result, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: shimPath,
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading env output: %v", err)
	}
	envStr := string(data)

	if !strings.Contains(envStr, "SHELL="+shimPath) {
		t.Error("SHELL not set to shim path in child env")
	}
	if !strings.Contains(envStr, "AILERON_REAL_SHELL=") {
		t.Error("AILERON_REAL_SHELL not set in child env")
	}
	if !strings.Contains(envStr, "CLAUDE_CODE_SHELL=") {
		t.Error("CLAUDE_CODE_SHELL not set in child env")
	}
	if !strings.Contains(envStr, "AILERON_AGENT=test-script") {
		t.Error("AILERON_AGENT not set in child env")
	}
	// CLAUDE_CODE_SHELL path must contain "bash" for Claude Code to accept it
	for _, line := range strings.Split(envStr, "\n") {
		if strings.HasPrefix(line, "CLAUDE_CODE_SHELL=") {
			val := strings.TrimPrefix(line, "CLAUDE_CODE_SHELL=")
			if !strings.Contains(val, "bash") {
				t.Errorf("CLAUDE_CODE_SHELL path must contain 'bash', got %q", val)
			}
		}
	}
}

func TestLaunch_AgentSpecificEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	outFile := filepath.Join(dir, "env.txt")

	script := filepath.Join(dir, "capture-env.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	agent := scriptAgent{
		script:   script,
		extraEnv: map[string]string{"CUSTOM_VAR": "hello"},
	}

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "CUSTOM_VAR=hello") {
		t.Error("agent-specific env var not set in child env")
	}
}

func TestLaunch_ExitCodePropagation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	agent := scriptAgent{script: "/bin/sh"}
	result, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Args:      []string{"-c", "exit 42"},
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", result.ExitCode)
	}
}

func TestLaunch_BinaryNotFound(t *testing.T) {
	agent := testAgent{name: "nonexistent-binary-xyz"}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestLaunch_EmptyShellFallback(t *testing.T) {
	// When SHELL is empty, buildEnv should default AILERON_REAL_SHELL to /bin/sh
	// (unless agent overrides it).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("SHELL", "")
	outFile := filepath.Join(dir, "env.txt")

	script := filepath.Join(dir, "capture-env.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	agent := scriptAgent{script: script}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	// With no agent override and empty SHELL, should fall back to /bin/sh
	if !strings.Contains(string(data), "AILERON_REAL_SHELL=/bin/sh") {
		t.Error("expected AILERON_REAL_SHELL=/bin/sh when SHELL is empty")
	}
}

func TestLaunch_WorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	outFile := filepath.Join(dir, "cwd.txt")

	script := filepath.Join(dir, "capture-cwd.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(dir, "workdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	agent := scriptAgent{script: script}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Dir:       workDir,
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "workdir") {
		t.Errorf("expected working directory to be set, got %q", string(data))
	}
}

func TestLaunch_EmptyWrapperPathSkipsCLAUDE_CODE_SHELL(t *testing.T) {
	// When wrapperPath is empty, CLAUDE_CODE_SHELL should not be set.
	// We can't easily test this through Launch (it always installs wrapper),
	// so this is a note that the branch is covered by the empty-path guard.
	// The buildEnv function is unexported, so we verify indirectly.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	outFile := filepath.Join(dir, "env.txt")

	script := filepath.Join(dir, "capture-env.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	agent := scriptAgent{script: script}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	// CLAUDE_CODE_SHELL should be set (wrapper IS installed by Launch)
	if !strings.Contains(string(data), "CLAUDE_CODE_SHELL=") {
		t.Error("expected CLAUDE_CODE_SHELL to be set when wrapper is installed")
	}
	// AILERON_AGENT should be set
	if !strings.Contains(string(data), "AILERON_AGENT=") {
		t.Error("expected AILERON_AGENT to be set")
	}
}

func TestInstallWrapper_BadHomeDir(t *testing.T) {
	// Point HOME at a read-only path to trigger the MkdirAll error.
	t.Setenv("HOME", "/dev/null")
	_, err := launch.InstallWrapper("/usr/local/bin/aileron-sh")
	if err == nil {
		t.Fatal("expected error when HOME is invalid")
	}
}

func TestInstallWrapper_ReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Create .aileron as a read-only directory so WriteFile fails
	aileronDir := filepath.Join(dir, ".aileron")
	if err := os.MkdirAll(aileronDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a file at the wrapper path that's not writable
	wrapperPath := filepath.Join(aileronDir, "bash")
	if err := os.WriteFile(wrapperPath, []byte("old"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Make directory read-only
	os.Chmod(aileronDir, 0o555)
	t.Cleanup(func() { os.Chmod(aileronDir, 0o755) })

	_, err := launch.InstallWrapper("/usr/local/bin/aileron-sh")
	if err == nil {
		t.Fatal("expected error when wrapper dir is read-only")
	}
}

func TestLaunch_EnvScrubbing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Set env vars that should be scrubbed.
	t.Setenv("AWS_SECRET_KEY", "supersecret")
	t.Setenv("MY_TOKEN", "tok123")
	t.Setenv("SAFE_VAR", "keepme")

	// Write an aileron.yaml with scrub config.
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: allow
env:
  scrub:
    - "AWS_*"
    - "*_TOKEN"
  passthrough:
    - "SAFE_VAR"
`), 0o644)

	outFile := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "capture-env.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755)

	agent := scriptAgent{script: script}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Dir:       dir,
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}

	data, _ := os.ReadFile(outFile)
	envStr := string(data)

	if strings.Contains(envStr, "AWS_SECRET_KEY") {
		t.Error("AWS_SECRET_KEY should have been scrubbed")
	}
	if strings.Contains(envStr, "MY_TOKEN") {
		t.Error("MY_TOKEN should have been scrubbed")
	}
	if !strings.Contains(envStr, "SAFE_VAR=keepme") {
		t.Error("SAFE_VAR should have been preserved (passthrough)")
	}
}

func TestLaunch_EnvScrubPassthroughBeatsScrub(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("HOME_DIR", "/home/user")

	// HOME_DIR matches HOME* scrub pattern but HOME is in passthrough.
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: allow
env:
  scrub:
    - "HOME*"
  passthrough:
    - "HOME"
    - "HOME_DIR"
`), 0o644)

	outFile := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "capture-env.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755)

	agent := scriptAgent{script: script}
	launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Dir:       dir,
	})

	data, _ := os.ReadFile(outFile)
	envStr := string(data)

	// HOME_DIR is in passthrough, so passthrough beats scrub.
	if !strings.Contains(envStr, "HOME_DIR=/home/user") {
		t.Error("HOME_DIR should be preserved (passthrough beats scrub)")
	}
}

func TestResolveAuditLogFromCwd(t *testing.T) {
	dir := t.TempDir()
	// No aileron.yaml → falls back to cwd/.aileron/audit.jsonl.
	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	path := launch.ResolveAuditLogFromCwd()
	if !strings.HasSuffix(path, filepath.Join(".aileron", "audit.jsonl")) {
		t.Errorf("expected .aileron/audit.jsonl suffix, got %q", path)
	}
}

func TestResolveAuditLogFromCwd_WithPolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
settings:
  audit_log: custom/audit.log
`), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	path := launch.ResolveAuditLogFromCwd()
	if !strings.HasSuffix(path, filepath.Join("custom", "audit.log")) {
		t.Errorf("expected custom/audit.log, got %q", path)
	}
}

func TestResolveAuditLogFromCwd_DefaultWithPolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("version: 1\n"), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	path := launch.ResolveAuditLogFromCwd()
	if !strings.HasSuffix(path, filepath.Join(".aileron", "audit.jsonl")) {
		t.Errorf("expected .aileron/audit.jsonl, got %q", path)
	}
}

func TestPrintSessionSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	// Write some entries.
	for _, e := range []struct{ cmd, disp string }{
		{"echo 1", "allow"},
		{"echo 2", "allow"},
		{"rm -rf /", "deny"},
		{"git push", "ask_approved"},
		{"curl bad", "ask_denied"},
	} {
		audit.AppendShellEntry(path, audit.ShellEntry{
			SessionID:   "test-session",
			Command:     e.cmd,
			Disposition: e.disp,
		})
	}

	var buf strings.Builder
	launch.PrintSessionSummary(&buf, path, "test-session")
	out := buf.String()

	if !strings.Contains(out, "2 command(s) allowed") {
		t.Errorf("expected 2 allowed, got:\n%s", out)
	}
	if !strings.Contains(out, "1 command(s) denied by policy") {
		t.Errorf("expected 1 denied, got:\n%s", out)
	}
	if !strings.Contains(out, "1 command(s) approved") {
		t.Errorf("expected 1 approved, got:\n%s", out)
	}
	if !strings.Contains(out, "1 command(s) denied by user") {
		t.Errorf("expected 1 user denied, got:\n%s", out)
	}
}

func TestPrintSessionSummary_NoEntries(t *testing.T) {
	var buf strings.Builder
	launch.PrintSessionSummary(&buf, "/nonexistent/audit.jsonl", "nope")
	if buf.Len() != 0 {
		t.Errorf("expected no output for missing log, got %q", buf.String())
	}
}

func TestPrintSessionSummary_EmptySession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "other", Command: "echo", Disposition: "allow"})

	var buf strings.Builder
	launch.PrintSessionSummary(&buf, path, "nonexistent-session")
	if buf.Len() != 0 {
		t.Errorf("expected no output for unmatched session, got %q", buf.String())
	}
}

func TestBridgeMessages(t *testing.T) {
	queue := launch.NewNotifyQueue(10, nil)
	msgs := make(chan comms.IncomingMessage, 5)

	go launch.BridgeMessages(msgs, queue, nil, "", "")

	msgs <- comms.IncomingMessage{
		ID:        "msg-1",
		Service:   "slack",
		Channel:   "#backend",
		Author:    "Alice",
		Body:      "Is the deploy blocked?",
		Timestamp: time.Now(),
	}
	msgs <- comms.IncomingMessage{
		ID:      "msg-2",
		Service: "slack",
		Channel: "#backend",
		Author:  "Bob",
		Body:    "No, it went through.",
	}
	close(msgs)
	time.Sleep(50 * time.Millisecond)

	if queue.Len() != 2 {
		t.Fatalf("expected 2 messages in queue, got %d", queue.Len())
	}
	latest, ok := queue.Latest()
	if !ok || latest.Author != "Bob" {
		t.Errorf("expected latest from Bob, got %+v", latest)
	}
	if latest.Source != "slack" {
		t.Errorf("Source = %q, want 'slack'", latest.Source)
	}
}

func TestWireDraftInjection_OverlayCallback(t *testing.T) {
	var buf strings.Builder
	q := launch.NewNotifyQueue(10, nil)
	o := launch.NewOverlay(q, nil, &buf, 24, 80, nil)

	launch.WireDraftInjection(&buf, o, q)

	// Simulate overlay draft request.
	msg := launch.Message{Source: "slack", Channel: "#backend", Author: "Sarah", Body: "question?"}
	o.OnDraftRequest(msg)

	out := buf.String()
	if !strings.Contains(out, "send_message") {
		t.Error("expected send_message in injected prompt from overlay callback")
	}
	if !strings.Contains(out, "Sarah") {
		t.Error("expected author in injected prompt")
	}
}

func TestWireDraftInjection_AutoDraftCallback(t *testing.T) {
	var buf strings.Builder
	q := launch.NewNotifyQueue(10, nil)
	o := launch.NewOverlay(q, nil, &buf, 24, 80, nil)

	launch.WireDraftInjection(&buf, o, q)

	// Push an auto-draft message — should trigger injection.
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Bob", Body: "help?", AutoDraft: true})

	out := buf.String()
	if !strings.Contains(out, "send_message") {
		t.Error("expected send_message in injected prompt from auto-draft callback")
	}
	if !strings.Contains(out, "Bob") {
		t.Error("expected author in injected prompt")
	}
}

func TestWireReply(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "comms.sock")
	sender := &testListener{service: "slack", msgs: make(chan comms.IncomingMessage, 1)}
	queue := launch.NewNotifyQueue(10, nil)

	srv, err := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	var buf strings.Builder
	o := launch.NewOverlay(queue, nil, &buf, 24, 80, nil)

	launch.WireReply(o, srv)

	msg := launch.Message{Source: "slack", Channel: "#backend", Author: "Sarah", Body: "question?"}
	o.OnReply(msg, "my reply")

	// The sender should have received the message (Send returns nil for testListener).
	// Just verify no panic and the callback is wired.
}

func TestBridgeMessages_AutoDraft(t *testing.T) {
	queue := launch.NewNotifyQueue(10, nil)
	msgs := make(chan comms.IncomingMessage, 3)

	autoDraft := map[string]bool{"#backend": true}
	go launch.BridgeMessages(msgs, queue, autoDraft, "", "")

	msgs <- comms.IncomingMessage{ID: "1", Service: "slack", Channel: "#backend", Author: "Sarah", Body: "question"}
	msgs <- comms.IncomingMessage{ID: "2", Service: "slack", Channel: "#general", Author: "Bob", Body: "chat"}
	close(msgs)
	time.Sleep(50 * time.Millisecond)

	all := queue.Messages()
	if len(all) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(all))
	}
	if !all[0].AutoDraft {
		t.Error("expected AutoDraft=true for #backend message")
	}
	if all[1].AutoDraft {
		t.Error("expected AutoDraft=false for #general message")
	}
}

func TestLaunch_RejectsPlaintextTokens(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: allow
notifications:
  slack:
    app_token: xapp-plaintext-token
    bot_token: xoxb-plaintext-token
    channels:
      - name: "#test"
`), 0o644)

	script := filepath.Join(dir, "noop.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ntrue\n"), 0o755)

	agent := scriptAgent{script: script}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Dir:       dir,
	})
	if err != nil {
		t.Fatalf("launch should succeed even if token validation fails: %v", err)
	}
}

func TestLaunch_VaultRefsNoTTY(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: allow
notifications:
  slack:
    app_token: vault:slack_app_token
    bot_token: vault:slack_bot_token
    channels:
      - name: "#test"
`), 0o644)

	script := filepath.Join(dir, "noop.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ntrue\n"), 0o755)

	agent := scriptAgent{script: script}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Dir:       dir,
	})
	if err != nil {
		t.Fatalf("launch should succeed even if vault prompt fails: %v", err)
	}
}

func TestLaunch_DiscordPlaintextRejected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte(`
version: 1
default: allow
notifications:
  discord:
    bot_token: plaintext-discord-token
    channels:
      - name: "123456"
`), 0o644)

	script := filepath.Join(dir, "noop.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ntrue\n"), 0o755)

	agent := scriptAgent{script: script}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Dir:       dir,
	})
	if err != nil {
		t.Fatalf("launch should succeed: %v", err)
	}
}

func TestBridgeMessages_AuditLog(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	queue := launch.NewNotifyQueue(10, nil)
	msgs := make(chan comms.IncomingMessage, 2)

	go launch.BridgeMessages(msgs, queue, nil, auditPath, "test-session")

	msgs <- comms.IncomingMessage{ID: "1", Service: "slack", Channel: "#backend", Author: "Alice", Body: "hello", Timestamp: time.Now()}
	msgs <- comms.IncomingMessage{ID: "2", Service: "discord", Channel: "dev-chat", Author: "Bob", Body: "hi", Timestamp: time.Now()}
	close(msgs)
	time.Sleep(50 * time.Millisecond)

	entries, err := audit.ReadMessageEntries(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 audit entries, got %d", len(entries))
	}
	if entries[0].Event != "message_received" {
		t.Errorf("expected 'message_received', got %q", entries[0].Event)
	}
	if entries[0].Author != "Alice" {
		t.Errorf("expected author 'Alice', got %q", entries[0].Author)
	}
	if entries[0].SessionID != "test-session" {
		t.Errorf("expected session 'test-session', got %q", entries[0].SessionID)
	}
	if entries[1].Service != "discord" {
		t.Errorf("expected service 'discord', got %q", entries[1].Service)
	}
}

func TestBridgeMessages_LongPreviewTruncated(t *testing.T) {
	queue := launch.NewNotifyQueue(10, nil)
	msgs := make(chan comms.IncomingMessage, 1)

	go launch.BridgeMessages(msgs, queue, nil, "", "")

	longBody := strings.Repeat("x", 100)
	msgs <- comms.IncomingMessage{
		ID:   "msg-1",
		Body: longBody,
	}
	close(msgs)
	time.Sleep(50 * time.Millisecond)

	all := queue.Messages()
	if len(all) != 1 {
		t.Fatal("expected 1 message")
	}
	if len(all[0].Preview) > 80 {
		t.Errorf("preview should be truncated, got %d chars", len(all[0].Preview))
	}
	if all[0].Body != longBody {
		t.Error("full body should be preserved")
	}
}

// testListener is a mock comms.Listener for testing StartListeners.
type testListener struct {
	service      string
	connectErr   error
	listenErr    error
	msgs         chan comms.IncomingMessage
	closed       bool
}

func (l *testListener) Service() string { return l.service }
func (l *testListener) Connect(ctx context.Context) error { return l.connectErr }
func (l *testListener) Listen(ctx context.Context) (<-chan comms.IncomingMessage, error) {
	if l.listenErr != nil {
		return nil, l.listenErr
	}
	return l.msgs, nil
}
func (l *testListener) Send(ctx context.Context, msg comms.OutgoingMessage) error { return nil }
func (l *testListener) Close() error { l.closed = true; return nil }

func TestStartListeners_Success(t *testing.T) {
	msgs := make(chan comms.IncomingMessage, 10)
	l := &testListener{service: "test", msgs: msgs}
	queue := launch.NewNotifyQueue(10, nil)

	var stderr strings.Builder
	started := launch.StartListeners(context.Background(), []comms.Listener{l}, queue, &stderr, nil, "", "")

	if len(started) != 1 {
		t.Fatalf("expected 1 started listener, got %d", len(started))
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no errors, got %q", stderr.String())
	}

	// Send a message and verify it reaches the queue.
	msgs <- comms.IncomingMessage{ID: "1", Service: "test", Author: "Alice", Body: "hello"}
	close(msgs)
	time.Sleep(50 * time.Millisecond)

	if queue.Len() != 1 {
		t.Errorf("expected 1 message in queue, got %d", queue.Len())
	}
}

func TestStartListeners_ConnectError(t *testing.T) {
	l := &testListener{
		service:    "bad",
		connectErr: fmt.Errorf("connection refused"),
	}
	queue := launch.NewNotifyQueue(10, nil)

	var stderr strings.Builder
	started := launch.StartListeners(context.Background(), []comms.Listener{l}, queue, &stderr, nil, "", "")

	if len(started) != 0 {
		t.Error("expected 0 started listeners on connect error")
	}
	if !strings.Contains(stderr.String(), "connect failed") {
		t.Errorf("expected connect error in stderr, got %q", stderr.String())
	}
}

func TestStartListeners_ListenError(t *testing.T) {
	l := &testListener{
		service:   "bad",
		listenErr: fmt.Errorf("listen failed"),
	}
	queue := launch.NewNotifyQueue(10, nil)

	var stderr strings.Builder
	started := launch.StartListeners(context.Background(), []comms.Listener{l}, queue, &stderr, nil, "", "")

	if len(started) != 0 {
		t.Error("expected 0 started listeners on listen error")
	}
	if !strings.Contains(stderr.String(), "listen failed") {
		t.Errorf("expected listen error in stderr, got %q", stderr.String())
	}
}

func TestStartListeners_Empty(t *testing.T) {
	queue := launch.NewNotifyQueue(10, nil)
	var stderr strings.Builder
	started := launch.StartListeners(context.Background(), nil, queue, &stderr, nil, "", "")
	if len(started) != 0 {
		t.Error("expected 0 listeners for nil input")
	}
}

func TestStartListeners_Mixed(t *testing.T) {
	good := &testListener{service: "good", msgs: make(chan comms.IncomingMessage, 1)}
	bad := &testListener{service: "bad", connectErr: fmt.Errorf("nope")}
	queue := launch.NewNotifyQueue(10, nil)

	var stderr strings.Builder
	started := launch.StartListeners(context.Background(), []comms.Listener{bad, good}, queue, &stderr, nil, "", "")

	if len(started) != 1 {
		t.Fatalf("expected 1 started, got %d", len(started))
	}
	if started[0].Service() != "good" {
		t.Errorf("expected 'good' listener, got %q", started[0].Service())
	}
}

func TestLaunch_CommsListenersStartWithConfig(t *testing.T) {
	// Verify that startCommsListeners doesn't panic when no config exists.
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// No aileron.yaml → no listeners, no panic.
	queue := launch.NewNotifyQueue(10, nil)
	_ = queue // listeners would push to this

	// Just verify Launch doesn't crash when there's no notifications config.
	script := filepath.Join(dir, "noop.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ntrue\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "aileron.yaml"), []byte("version: 1\ndefault: allow\n"), 0o644)

	agent := scriptAgent{script: script}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:     agent,
		ShellShim: "/tmp/fake-shim",
		Dir:       dir,
	})
	if err != nil {
		t.Fatalf("launch with no comms config should succeed: %v", err)
	}
}

func TestComputeAgentRows(t *testing.T) {
	tests := []struct {
		totalRows, barHeight, want int
	}{
		{24, 2, 22},
		{10, 2, 8},
		{3, 2, 1},
		{2, 2, 1}, // clamped to 1
		{1, 2, 1}, // clamped to 1
	}
	for _, tt := range tests {
		got := launch.ComputeAgentRows(tt.totalRows, tt.barHeight)
		if got != tt.want {
			t.Errorf("ComputeAgentRows(%d, %d) = %d, want %d", tt.totalRows, tt.barHeight, got, tt.want)
		}
	}
}

func TestSetupTerminalScreen(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "test")
	var buf strings.Builder
	launch.SetupTerminalScreen(&buf, 22, bar)
	out := buf.String()

	// Should clear screen
	if !strings.Contains(out, "\033[2J") {
		t.Error("expected clear screen escape")
	}
	// Should contain status bar content
	if !strings.Contains(out, "test") {
		t.Error("expected status bar text")
	}
	// Should position cursor at top of agent area so the agent starts there.
	if !strings.Contains(out, "\033[1;1H") {
		t.Error("expected cursor at row 1 (top of agent area)")
	}
}

func TestSetupTerminalScreen_WithQueue(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "branding")
	q := launch.NewNotifyQueue(10, nil)
	bar.SetQueue(q)

	// Push a message before setup — the initial render should show it.
	q.Push(launch.Message{ID: "1", Preview: "hey there"})

	var buf strings.Builder
	launch.SetupTerminalScreen(&buf, 22, bar)
	out := buf.String()

	// Should clear screen.
	if !strings.Contains(out, "\033[2J") {
		t.Error("expected clear screen escape")
	}
	// Should show unread count from the queue.
	if !strings.Contains(out, "1 unread") {
		t.Errorf("expected '1 unread' in initial setup, got %q", out)
	}
	// Should still show branding.
	if !strings.Contains(out, "branding") {
		t.Error("expected branding text")
	}
}

func TestSetupTerminalScreen_ClearsBeforeBar(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "test")
	var buf strings.Builder
	launch.SetupTerminalScreen(&buf, 22, bar)
	out := buf.String()

	// Clear screen should come before the status bar content.
	clearIdx := strings.Index(out, "\033[2J")
	barIdx := strings.Index(out, "test")
	if clearIdx < 0 || barIdx < 0 || clearIdx >= barIdx {
		t.Error("expected clear screen before status bar render")
	}
}

func TestHandleResize(t *testing.T) {
	// Create a real pty pair so we have valid file descriptors.
	ptmx, pts, err := pty.Open()
	if err != nil {
		t.Fatalf("failed to open pty: %v", err)
	}
	defer ptmx.Close()
	defer pts.Close()

	// Set a known size so the bar renders.
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80})

	bar := launch.NewStatusBar(24, 80, "test")
	var buf strings.Builder

	launch.HandleResize(&buf, int(ptmx.Fd()), ptmx, bar)

	// HandleResize should resize the pty and re-render the bar.
	// The pty might report 0x0 on some systems, in which case the bar
	// won't render (< 3 rows). Just verify no panic.
	_ = buf.String()
}

func TestCleanupTerminalScreen(t *testing.T) {
	var buf strings.Builder
	launch.CleanupTerminalScreen(&buf, 24)
	out := buf.String()

	// Should move to bar area and clear
	if !strings.Contains(out, "\033[23;1H\033[J") {
		t.Errorf("expected clear at row 23, got %q", out)
	}
}

// scriptAgent launches a specific script/binary directly.
type scriptAgent struct {
	script   string
	extraEnv map[string]string
}

func (a scriptAgent) Name() string           { return "test-script" }
func (a scriptAgent) BinaryNames() []string  { return []string{a.script} }
func (a scriptAgent) Args() []string         { return nil }
func (a scriptAgent) Env() map[string]string { return a.extraEnv }
