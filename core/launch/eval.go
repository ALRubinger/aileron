package launch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/policy"
	launchpolicy "github.com/ALRubinger/aileron/core/policy/launch"
	"github.com/ALRubinger/aileron/core/store/mem"
)

// EvalResult holds the outcome of evaluating a command against policy.
type EvalResult struct {
	Disposition model.Disposition
	Reason      string
}

// EvaluateCommand loads policy from the given file (or uses an empty default)
// and evaluates the command against it.
func EvaluateCommand(policyPath, command, workingDir string) EvalResult {
	pf := loadPolicyFileFrom(policyPath)
	rules := pf.ToEngineRules()

	store := mem.NewPolicyStore()
	ctx := context.Background()

	active := policy.ActiveStatus()
	if err := store.Create(ctx, policy.MakePolicy("launch", "default", rules, active)); err != nil {
		return EvalResult{model.DispositionRequireApproval, "policy load error"}
	}

	engine := policy.NewRuleEngine(store)

	parts := strings.Fields(command)
	binaryName := ""
	argsStr := ""
	if len(parts) > 0 {
		binaryName = parts[0]
	}
	if len(parts) > 1 {
		argsStr = strings.Join(parts[1:], " ")
	}

	decision, err := engine.Evaluate(ctx, policy.EvaluationRequest{
		WorkspaceID: "default",
		Action: model.ActionIntent{
			Type:    "shell.exec",
			Summary: command,
			Metadata: map[string]any{
				"shell.command":     command,
				"shell.binary":      binaryName,
				"shell.args":        argsStr,
				"shell.working_dir": workingDir,
			},
		},
	})
	if err != nil {
		return EvalResult{model.DispositionRequireApproval, "policy evaluation error"}
	}

	return EvalResult{decision.Disposition, decision.DenialReason}
}

func loadPolicyFileFrom(path string) *launchpolicy.PolicyFile {
	if path == "" {
		// No project policy file — still load user settings so personal
		// rules apply even in repos without an aileron.yaml.
		pf, err := launchpolicy.LoadUserSettings()
		if err != nil {
			return &launchpolicy.PolicyFile{Version: 1}
		}
		return pf
	}
	pf, err := launchpolicy.LoadWithProfiles(path, profileDirs())
	if err != nil {
		return &launchpolicy.PolicyFile{Version: 1}
	}
	return pf
}

// profileDirs returns the standard directories to search for policy profiles.
func profileDirs() []string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}
	return []string{filepath.Join(home, ".aileron", "profiles")}
}

// FindPolicyFile searches for aileron.yaml in the given directory and parent
// directories up to the filesystem root. Returns empty string if not found.
func FindPolicyFile(startDir string) string {
	dir := startDir
	for {
		candidate := dir + "/aileron.yaml"
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := dir[:strings.LastIndex(dir, "/")]
		if parent == dir || parent == "" {
			return ""
		}
		dir = parent
	}
}

// WriteDeny writes a deny message to the writer with ANSI color.
func WriteDeny(w io.Writer, command, reason string) {
	fmt.Fprintf(w, "\033[31m  ✗ aileron: denied\033[0m %s\n", command)
	if reason != "" {
		fmt.Fprintf(w, "    %s\n", reason)
	}
}

// WriteDenyByUser writes a user-denied message to the writer.
func WriteDenyByUser(w io.Writer, command string) {
	fmt.Fprintf(w, "\033[33m  ✗ aileron: denied by user\033[0m %s\n", command)
}
