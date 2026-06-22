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
	got := EnrichValidateError(base, TierDevcontainer, agent, version, workDir, false)
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
		got := EnrichValidateError(base, tier, "claude", "1.2.3", "/home/dev/project", false)
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
	got := EnrichValidateError(base, TierDevcontainer, agent, "1.2.3", "/home/dev/project", false)
	if got != base {
		t.Errorf("unpublished agent: error was enriched, want unchanged: %s", got.Error())
	}
}

func TestEnrichValidateErrorUnrelatedFailureUnchanged(t *testing.T) {
	base := errors.New("validate sandbox image: image must support /bin/sh command execution")
	got := EnrichValidateError(base, TierDevcontainer, "claude", "1.2.3", "/home/dev/project", false)
	if got != base {
		t.Errorf("unrelated failure: error was enriched, want unchanged: %s", got.Error())
	}
}

const workspaceNotWritableErr = "validate sandbox image img: sandbox workspace is not writable at /home/agent/workspace: exit status 3"

// TestEnrichValidateErrorWorkspaceNotWritableSELinux verifies that when the
// `:z` SELinux relabel was actually applied (selinuxRelabelActive=true), the
// opaque "exit status 3" is enriched with the SELinux root cause, the
// automatic-relabel note, and the manual `chcon` fallback. This is the
// SELinux-enforcing-host branch.
func TestEnrichValidateErrorWorkspaceNotWritableSELinux(t *testing.T) {
	const workDir = "/home/dev/project"
	base := errors.New(workspaceNotWritableErr)
	got := EnrichValidateError(base, TierBase, "claude", "1.2.3", workDir, true)
	msg := got.Error()

	for _, want := range []string{
		"SELinux",       // names the root cause
		"enforcing",     // names the enforcing-mode condition
		"automatically", // states Aileron applies the relabel itself
		"`:z`",          // names the relabel mount option
		"chcon",         // gives the manual fallback command
		workDir,         // names the operator's workspace
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("enriched writability error missing %q: %s", want, msg)
		}
	}
	// The original error must remain wrapped so callers' %w chains still match.
	if !errors.Is(got, base) {
		t.Errorf("enriched error does not wrap the original validate error")
	}
}

// TestEnrichValidateErrorWorkspaceNotWritableUIDMismatch is the regression test
// for issue #1461: when the `:z` relabel was NOT applied (the daemon lacks
// SELinux support, or SELinux is not enforcing — selinuxRelabelActive=false),
// the real cause is a DAC uid mismatch, not SELinux. The enriched message must
// name the uid mismatch and the remap remediation, and must NOT claim Aileron
// applied a relabel or point at SELinux/chcon. Before the fix this branch
// emitted the SELinux wording unconditionally; this fails before and passes
// after.
func TestEnrichValidateErrorWorkspaceNotWritableUIDMismatch(t *testing.T) {
	const workDir = "/home/dev/project"
	base := errors.New(workspaceNotWritableErr)
	got := EnrichValidateError(base, TierBase, "claude", "1.2.3", workDir, false)
	msg := got.Error()

	for _, want := range []string{
		"uid",                     // names the DAC uid mismatch
		"aileron-remap-agent-uid", // names the remap remediation
		workDir,                   // names the operator's workspace
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("DAC-mismatch error missing %q: %s", want, msg)
		}
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

// TestEnrichValidateErrorWorkspaceNotWritableAllTiers verifies the writability
// enrichment is tier-independent: every sandbox tier bind-mounts the workspace,
// so the denial can occur regardless of which image was selected. Exercised on
// the DAC-mismatch branch (the common non-SELinux case).
func TestEnrichValidateErrorWorkspaceNotWritableAllTiers(t *testing.T) {
	base := errors.New(workspaceNotWritableErr)
	for _, tier := range []Tier{TierBase, TierPublished, TierBYOImage, TierDevcontainer} {
		got := EnrichValidateError(base, tier, "claude", "1.2.3", "/home/dev/project", false)
		if !strings.Contains(got.Error(), "uid") {
			t.Errorf("tier %q: writability error was not enriched: %s", tier, got.Error())
		}
	}
}

// TestEnrichWorkspaceNotWritableEmptyWorkDirDefaultsToCwd covers the empty
// workDir default ("." substitution) on both writability branches.
func TestEnrichWorkspaceNotWritableEmptyWorkDirDefaultsToCwd(t *testing.T) {
	base := errors.New(workspaceNotWritableErr)
	for _, selinux := range []bool{true, false} {
		got := EnrichValidateError(base, TierBase, "claude", "1.2.3", "", selinux)
		if !strings.Contains(got.Error(), "workspace .") {
			t.Errorf("selinux=%v: empty workDir not defaulted to '.': %s", selinux, got.Error())
		}
	}
}

func TestEnrichValidateErrorNilUnchanged(t *testing.T) {
	if got := EnrichValidateError(nil, TierDevcontainer, "claude", "1.2.3", "/home/dev/project", false); got != nil {
		t.Errorf("nil error: got %v, want nil", got)
	}
}

func TestEnrichValidateErrorEmptyWorkDirDefaultsToCwd(t *testing.T) {
	base := agentNotFoundErr("claude")
	got := EnrichValidateError(base, TierDevcontainer, "claude", "1.2.3", "", false)
	// filepath.Join(".", path) cleans to the bare relative path.
	wantPath := DefaultDevcontainerPath
	if !strings.Contains(got.Error(), wantPath) {
		t.Errorf("empty workDir: enriched error does not name %q: %s", wantPath, got.Error())
	}
}
