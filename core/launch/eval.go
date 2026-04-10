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
	RuleID      string // which policy rule matched
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
		return EvalResult{model.DispositionRequireApproval, "policy load error", ""}
	}

	engine := policy.NewRuleEngine(store)

	parts := strings.Fields(command)
	binaryName := ""
	argsStr := ""
	if len(parts) > 0 {
		binaryName = filepath.Base(parts[0])
	}
	if len(parts) > 1 {
		argsStr = strings.Join(parts[1:], " ")
	}

	// Rebuild the command with the normalized binary name so that
	// rules like "ls *" match even when the agent calls "/bin/ls -la ...".
	normalizedCommand := command
	if binaryName != "" && len(parts) > 0 && parts[0] != binaryName {
		normalizedCommand = binaryName
		if argsStr != "" {
			normalizedCommand += " " + argsStr
		}
	}

	decision, err := engine.Evaluate(ctx, policy.EvaluationRequest{
		WorkspaceID: "default",
		Action: model.ActionIntent{
			Type:    "shell.exec",
			Summary: command,
			Metadata: map[string]any{
				"shell.command":     normalizedCommand,
				"shell.binary":      binaryName,
				"shell.args":        argsStr,
				"shell.working_dir": workingDir,
			},
		},
	})
	if err != nil {
		return EvalResult{model.DispositionRequireApproval, "policy evaluation error", ""}
	}

	ruleID := ""
	if len(decision.MatchedPolicies) > 0 {
		ruleID = decision.MatchedPolicies[0].RuleID
	}

	return EvalResult{decision.Disposition, decision.DenialReason, ruleID}
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
	pf, err := launchpolicy.LoadWithProfiles(path)
	if err != nil {
		return &launchpolicy.PolicyFile{Version: 1}
	}
	return pf
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

// WriteDeny writes a deny message to the writer.
func WriteDeny(w io.Writer, command, reason, ruleID, policyPath string) {
	fmt.Fprintf(w, "[✈️ Aileron] Denied ⛔: %s\n", command)
	if reason != "" {
		fmt.Fprintf(w, "Reason: %s\n", reason)
	}
	if source := describeRuleSource(ruleID, policyPath); source != "" {
		fmt.Fprintf(w, "Policy: %s\n", source)
	}
}

// WriteDenyByUser writes a user-denied message to the writer.
func WriteDenyByUser(w io.Writer, command string) {
	fmt.Fprintf(w, "[✈️ Aileron] Denied ⛔: %s\n", command)
}

// describeRuleSource returns a human-readable description of where a
// policy rule came from.
func describeRuleSource(ruleID, policyPath string) string {
	if strings.HasPrefix(ruleID, "builtin_") {
		return "built-in rule"
	}
	if policyPath == "" {
		return ""
	}
	// Shorten the path for display.
	if strings.Contains(policyPath, "/.aileron/settings.yaml") {
		return "personal settings (~/.aileron/settings.yaml)"
	}
	return policyPath
}
