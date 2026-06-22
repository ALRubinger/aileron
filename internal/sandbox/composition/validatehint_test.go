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
	got := EnrichValidateError(base, TierDevcontainer, agent, version, workDir)
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
		got := EnrichValidateError(base, tier, "claude", "1.2.3", "/home/dev/project")
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
	got := EnrichValidateError(base, TierDevcontainer, agent, "1.2.3", "/home/dev/project")
	if got != base {
		t.Errorf("unpublished agent: error was enriched, want unchanged: %s", got.Error())
	}
}

func TestEnrichValidateErrorUnrelatedFailureUnchanged(t *testing.T) {
	base := errors.New("validate sandbox image: image must support /bin/sh command execution")
	got := EnrichValidateError(base, TierDevcontainer, "claude", "1.2.3", "/home/dev/project")
	if got != base {
		t.Errorf("unrelated failure: error was enriched, want unchanged: %s", got.Error())
	}
}

// TestEnrichValidateErrorWorkspaceNotWritable verifies the opaque writability
// failure (which surfaces to the operator as a bare "exit status 3") is enriched
// with the SELinux root cause, the automatic-relabel note, and the manual
// fallback. This fails before the fix (the raw error explains none of those) and
// passes after.
func TestEnrichValidateErrorWorkspaceNotWritable(t *testing.T) {
	const workDir = "/home/dev/project"
	base := errors.New("validate sandbox image img: sandbox workspace is not writable at /home/agent/workspace: exit status 3")
	got := EnrichValidateError(base, TierBase, "claude", "1.2.3", workDir)
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

// TestEnrichValidateErrorWorkspaceNotWritableAllTiers verifies the writability
// enrichment is tier-independent: every sandbox tier bind-mounts the workspace,
// so the SELinux denial can occur regardless of which image was selected.
func TestEnrichValidateErrorWorkspaceNotWritableAllTiers(t *testing.T) {
	base := errors.New("validate sandbox image img: sandbox workspace is not writable at /home/agent/workspace: exit status 3")
	for _, tier := range []Tier{TierBase, TierPublished, TierBYOImage, TierDevcontainer} {
		got := EnrichValidateError(base, tier, "claude", "1.2.3", "/home/dev/project")
		if !strings.Contains(got.Error(), "SELinux") {
			t.Errorf("tier %q: writability error was not enriched: %s", tier, got.Error())
		}
	}
}

func TestEnrichValidateErrorNilUnchanged(t *testing.T) {
	if got := EnrichValidateError(nil, TierDevcontainer, "claude", "1.2.3", "/home/dev/project"); got != nil {
		t.Errorf("nil error: got %v, want nil", got)
	}
}

func TestEnrichValidateErrorEmptyWorkDirDefaultsToCwd(t *testing.T) {
	base := agentNotFoundErr("claude")
	got := EnrichValidateError(base, TierDevcontainer, "claude", "1.2.3", "")
	// filepath.Join(".", path) cleans to the bare relative path.
	wantPath := DefaultDevcontainerPath
	if !strings.Contains(got.Error(), wantPath) {
		t.Errorf("empty workDir: enriched error does not name %q: %s", wantPath, got.Error())
	}
}
