package composition

import (
	"errors"
	"strings"
	"testing"
)

// agentNotFoundErr mimics the validate failure the in-container script produces
// when the agent CLI is not on PATH (see runtime.go validationScript).
func agentNotFoundErr(agent string) error {
	return errors.New("validate sandbox image: agent command not found in sandbox image: " + agent)
}

// TestEnrichValidateErrorDevcontainerMismatch reproduces the CWD-determined-tier
// failure: a .devcontainer/ in workDir selected the devcontainer tier, its
// project image lacks the requested agent CLI, and a published per-agent image
// exists. The enriched error must name the tier, the discovered devcontainer
// path/CWD, and the published image. This fails before the fix (the raw error
// names none of those) and passes after.
func TestEnrichValidateErrorDevcontainerMismatch(t *testing.T) {
	const (
		agent   = "claude"
		version = "1.2.3"
		workDir = "/home/dev/project"
	)
	if !PublishedAgentExists(agent) {
		t.Fatalf("precondition: %q must have a published image", agent)
	}
	base := agentNotFoundErr(agent)
	got := EnrichValidateError(base, TierDevcontainer, agent, version, workDir, "linux", false)
	msg := got.Error()

	if !strings.Contains(msg, string(TierDevcontainer)) {
		t.Errorf("enriched error does not name the tier %q: %s", TierDevcontainer, msg)
	}
	wantPath := workDir + "/" + DefaultDevcontainerPath
	if !strings.Contains(msg, wantPath) {
		t.Errorf("enriched error does not name the devcontainer path %q: %s", wantPath, msg)
	}
	if !strings.Contains(msg, workDir) {
		t.Errorf("enriched error does not name the CWD %q: %s", workDir, msg)
	}
	wantImage := PublishedAgentImage(agent, version)
	if !strings.Contains(msg, wantImage) {
		t.Errorf("enriched error does not name the published image %q: %s", wantImage, msg)
	}
	// The original error must remain wrapped so callers' %w chains still match.
	if !errors.Is(got, base) {
		t.Errorf("enriched error does not wrap the original validate error")
	}
}

func TestEnrichValidateErrorNonDevcontainerTierUnchanged(t *testing.T) {
	base := agentNotFoundErr("claude")
	for _, tier := range []Tier{TierBase, TierPublished, TierBYOImage} {
		got := EnrichValidateError(base, tier, "claude", "1.2.3", "/home/dev/project", "linux", false)
		if got != base {
			t.Errorf("tier %q: error was enriched, want unchanged: %s", tier, got.Error())
		}
	}
}

func TestEnrichValidateErrorUnpublishedAgentUnchanged(t *testing.T) {
	const agent = "no-such-agent"
	if PublishedAgentExists(agent) {
		t.Fatalf("precondition: %q must not have a published image", agent)
	}
	base := agentNotFoundErr(agent)
	got := EnrichValidateError(base, TierDevcontainer, agent, "1.2.3", "/home/dev/project", "linux", false)
	if got != base {
		t.Errorf("unpublished agent: error was enriched, want unchanged: %s", got.Error())
	}
}

func TestEnrichValidateErrorUnrelatedFailureUnchanged(t *testing.T) {
	base := errors.New("validate sandbox image: image must support /bin/sh command execution")
	got := EnrichValidateError(base, TierDevcontainer, "claude", "1.2.3", "/home/dev/project", "linux", false)
	if got != base {
		t.Errorf("unrelated failure: error was enriched, want unchanged: %s", got.Error())
	}
}

const workspaceNotWritableErr = "validate sandbox image img: sandbox workspace is not writable at /home/agent/workspace: exit status 3"

// TestEnrichValidateErrorWorkspaceNotWritableSELinux verifies that when the
// `:z` SELinux relabel was actually applied (selinuxRelabelActive=true), the
// opaque "exit status 3" leads with the actionable not-writable headline and
// then appends the SELinux deeper diagnostic: the automatic-relabel note and the
// manual `chcon` fallback. This is the SELinux-enforcing-host branch.
func TestEnrichValidateErrorWorkspaceNotWritableSELinux(t *testing.T) {
	const workDir = "/home/dev/project"
	base := errors.New(workspaceNotWritableErr)
	got := EnrichValidateError(base, TierBase, "claude", "1.2.3", workDir, "linux", true)
	msg := got.Error()

	for _, want := range []string{
		"not writable",      // leads with the actionable problem
		"directory you own", // states the simplest fix
		"SELinux",           // names the root cause
		"enforcing",         // names the enforcing-mode condition
		"automatically",     // states Aileron applies the relabel itself
		"`:z`",              // names the relabel mount option
		"chcon",             // gives the manual fallback command
		workDir,             // names the operator's workspace
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("enriched writability error missing %q: %s", want, msg)
		}
	}
	// The actionable headline must come before the SELinux deeper diagnostic.
	if strings.Index(msg, "not writable") > strings.Index(msg, "SELinux") {
		t.Errorf("actionable headline must precede the SELinux diagnostic: %s", msg)
	}
	// The original error must remain wrapped so callers' %w chains still match.
	if !errors.Is(got, base) {
		t.Errorf("enriched error does not wrap the original validate error")
	}
}

// TestEnrichValidateErrorWorkspaceNotWritableUIDMismatch is the regression test
// for issue #1461 (kept current for #1495): on Linux + Docker when the `:z`
// relabel was NOT applied (the daemon lacks SELinux support, or SELinux is not
// enforcing — selinuxRelabelActive=false), the real cause is a DAC uid mismatch,
// not SELinux. The enriched message must lead with the actionable not-writable
// headline and then append the uid-mismatch + remap remediation as the deeper
// diagnostic, and must NOT claim Aileron applied a relabel or point at
// SELinux/chcon.
func TestEnrichValidateErrorWorkspaceNotWritableUIDMismatch(t *testing.T) {
	const workDir = "/home/dev/project"
	base := errors.New(workspaceNotWritableErr)
	got := EnrichValidateError(base, TierBase, "claude", "1.2.3", workDir, "linux", false)
	msg := got.Error()

	for _, want := range []string{
		"not writable",            // leads with the actionable problem
		"directory you own",       // states the simplest fix
		"uid",                     // names the DAC uid mismatch
		"aileron-remap-agent-uid", // names the remap remediation
		workDir,                   // names the operator's workspace
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("DAC-mismatch error missing %q: %s", want, msg)
		}
	}
	// The actionable headline must precede the uid-remap deeper diagnostic — the
	// internals are no longer the lead (issue #1495).
	if strings.Index(msg, "not writable") > strings.Index(msg, "aileron-remap-agent-uid") {
		t.Errorf("actionable headline must precede the uid-remap diagnostic: %s", msg)
	}
	// The misleading SELinux wording must be gone in this branch.
	for _, absent := range []string{
		"SELinux",
		"chcon",
		"`:z`",
		"automatically", // must not claim Aileron applied a relabel it didn't
	} {
		if strings.Contains(msg, absent) {
			t.Errorf("DAC-mismatch error must not mention %q (no relabel was applied): %s", absent, msg)
		}
	}
	if !errors.Is(got, base) {
		t.Errorf("enriched error does not wrap the original validate error")
	}
}

// TestEnrichValidateErrorWorkspaceNotWritableWindows is the regression test for
// issue #1495: on Windows the Linux uid-remap machinery does not apply (Docker
// Desktop translates uids at the file-sharing boundary), so the enriched message
// must NOT lead with — or even mention — the uid-remap internals. It must lead
// with the actionable not-writable headline and call out the
// C:\WINDOWS\system32 PowerShell default-CWD trap with the `cd`-to-home remedy.
func TestEnrichValidateErrorWorkspaceNotWritableWindows(t *testing.T) {
	const workDir = `C:\WINDOWS\system32`
	base := errors.New(workspaceNotWritableErr)
	got := EnrichValidateError(base, TierBase, "claude", "1.2.3", workDir, "windows", false)
	msg := got.Error()

	for _, want := range []string{
		"not writable",        // leads with the actionable problem
		"directory you own",   // states the simplest fix
		`C:\WINDOWS\system32`, // names the PowerShell default-CWD trap
		"cd",                  // gives the one-step remedy
		"home",                // points at the user home
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("Windows writability error missing %q: %s", want, msg)
		}
	}
	// The Linux-only uid-remap internals must NOT appear on Windows: the helper
	// does not run there, so naming it would point the user at a mechanism that
	// does not apply (the core complaint of #1495). SELinux must be absent too.
	for _, absent := range []string{
		"aileron-remap-agent-uid",
		"in-container uid",
		"SELinux",
		"chcon",
	} {
		if strings.Contains(msg, absent) {
			t.Errorf("Windows writability error must not mention Linux-only %q: %s", absent, msg)
		}
	}
	if !errors.Is(got, base) {
		t.Errorf("enriched error does not wrap the original validate error")
	}
}

// TestEnrichValidateErrorWorkspaceNotWritableMacOS verifies the macOS (and other
// non-Linux, non-Windows) branch: Docker Desktop hides the uid mismatch at the
// file-sharing boundary, so the actionable headline stands alone with no
// Linux-only uid-remap internals and no SELinux/Windows specifics.
func TestEnrichValidateErrorWorkspaceNotWritableMacOS(t *testing.T) {
	const workDir = "/Users/dev/project"
	base := errors.New(workspaceNotWritableErr)
	got := EnrichValidateError(base, TierBase, "claude", "1.2.3", workDir, "darwin", false)
	msg := got.Error()

	for _, want := range []string{
		"not writable",      // leads with the actionable problem
		"directory you own", // states the simplest fix
		workDir,             // names the operator's workspace
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("macOS writability error missing %q: %s", want, msg)
		}
	}
	for _, absent := range []string{
		"aileron-remap-agent-uid",
		"in-container uid",
		"SELinux",
		"chcon",
		`C:\WINDOWS\system32`,
	} {
		if strings.Contains(msg, absent) {
			t.Errorf("macOS writability error must not mention %q: %s", absent, msg)
		}
	}
	if !errors.Is(got, base) {
		t.Errorf("enriched error does not wrap the original validate error")
	}
}

// TestEnrichValidateErrorWorkspaceNotWritableAllTiers verifies the writability
// enrichment is tier-independent: every sandbox tier bind-mounts the workspace,
// so the denial can occur regardless of which image was selected. The actionable
// headline must appear on every tier; exercised on the Linux DAC-mismatch branch
// (the common non-SELinux case).
func TestEnrichValidateErrorWorkspaceNotWritableAllTiers(t *testing.T) {
	base := errors.New(workspaceNotWritableErr)
	for _, tier := range []Tier{TierBase, TierPublished, TierBYOImage, TierDevcontainer} {
		got := EnrichValidateError(base, tier, "claude", "1.2.3", "/home/dev/project", "linux", false)
		if !strings.Contains(got.Error(), "not writable") {
			t.Errorf("tier %q: writability error was not enriched: %s", tier, got.Error())
		}
	}
}

// TestEnrichWorkspaceNotWritableEmptyWorkDirDefaultsToCwd covers the empty
// workDir default ("." substitution) on every writability branch.
func TestEnrichWorkspaceNotWritableEmptyWorkDirDefaultsToCwd(t *testing.T) {
	base := errors.New(workspaceNotWritableErr)
	cases := []struct {
		hostOS  string
		selinux bool
	}{
		{"linux", true},    // SELinux branch
		{"linux", false},   // Linux DAC-mismatch branch
		{"windows", false}, // Windows branch
		{"darwin", false},  // macOS / default branch
	}
	for _, tc := range cases {
		got := EnrichValidateError(base, TierBase, "claude", "1.2.3", "", tc.hostOS, tc.selinux)
		if !strings.Contains(got.Error(), "working directory . is not writable") {
			t.Errorf("hostOS=%s selinux=%v: empty workDir not defaulted to '.': %s", tc.hostOS, tc.selinux, got.Error())
		}
	}
}

func TestEnrichValidateErrorNilUnchanged(t *testing.T) {
	if got := EnrichValidateError(nil, TierDevcontainer, "claude", "1.2.3", "/home/dev/project", "linux", false); got != nil {
		t.Errorf("nil error: got %v, want nil", got)
	}
}

func TestEnrichValidateErrorEmptyWorkDirDefaultsToCwd(t *testing.T) {
	base := agentNotFoundErr("claude")
	got := EnrichValidateError(base, TierDevcontainer, "claude", "1.2.3", "", "linux", false)
	// filepath.Join(".", path) cleans to the bare relative path.
	wantPath := DefaultDevcontainerPath
	if !strings.Contains(got.Error(), wantPath) {
		t.Errorf("empty workDir: enriched error does not name %q: %s", wantPath, got.Error())
	}
}
