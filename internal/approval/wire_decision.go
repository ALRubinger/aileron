package approval

// Wire decision strings the policy-enforced shell understands. The
// `aileron-sh` shim and the daemon's shell-approval handler agree on
// this vocabulary verbatim — the four buttons on the webapp approval
// card map one-to-one. Any error path (timeout, context cancelled,
// wrong shape on the wire) collapses to [DecisionDeny] so the shell
// fails closed.
const (
	DecisionAllowOnce    = "allow_once"
	DecisionDeny         = "deny"
	DecisionAllowProject = "allow_project"
	DecisionAllowUser    = "allow_user"
)

// SavePolicyKey is the [ActionDecision.EditedPayload] key the webapp
// sets when a shell approval should also save the rule. Values are
// `""` (or absent — approve once), `"project"`, or `"user"`. The
// daemon translates that into the wire string `aileron-sh` expects;
// any other value degrades to [DecisionAllowOnce] rather than
// failing the whole approval.
const SavePolicyKey = "save_policy"

// WireDecision maps an [ActionDecision] + [ActionApproval.Wait] error
// into the four-option vocabulary `aileron-sh` understands. waitErr
// non-nil (timeout, ctx cancelled, etc.) and !d.Approved both collapse
// to [DecisionDeny]. The "save and approve" verdicts ride through
// EditedPayload under [SavePolicyKey]; this function is the single
// canonical translator so the shell-approval handler and any future
// CLI consumer agree on the mapping without copy-pasting.
func WireDecision(d ActionDecision, waitErr error) string {
	if waitErr != nil {
		return DecisionDeny
	}
	if !d.Approved {
		return DecisionDeny
	}
	raw, ok := d.EditedPayload[SavePolicyKey]
	if !ok {
		return DecisionAllowOnce
	}
	switch v, _ := raw.(string); v {
	case "project":
		return DecisionAllowProject
	case "user":
		return DecisionAllowUser
	default:
		return DecisionAllowOnce
	}
}
