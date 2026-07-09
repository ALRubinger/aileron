package runtime

import (
	"context"
	"fmt"
)

// DenyError reports that an effect-gated action was denied at the approval
// channel. The step aborts and the run fails; the denial is audited.
type DenyError struct {
	ActionRef string
	Reason    string
}

func (e *DenyError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("flightplan: action %q denied at approval: %s", e.ActionRef, e.Reason)
	}
	return fmt.Sprintf("flightplan: action %q denied at approval", e.ActionRef)
}

// enforcer wraps the action-dispatch seam with trust-contract enforcement
// (#1507): effect-routed approval before dispatch, idempotency-aware retry,
// and redaction of the result before it surfaces. It is the single chokepoint
// every action-call (and source-input read) passes through, so the
// trust-contract rules are applied in exactly one place.
type enforcer struct {
	dispatcher ActionDispatcher
	approver   Approver
}

// dispatchOutcome carries the redacted result plus the approval decision (for
// the audit record).
type dispatchOutcome struct {
	// Result is the redacted dispatch result that surfaces into the graph.
	Result map[string]any
	// Approved is the approval decision; true for an unattended read.
	Approved bool
	// ApprovalRequested reports whether the action routed through approval.
	ApprovalRequested bool

	// The following carry the non-secret actor provenance the dispatcher
	// surfaced for this call (issue #1753): the connector build and the
	// identity/binding it used, plus the consent posture. They are copied
	// straight from the DispatchResult and recorded on the actionDispatch so
	// a materialized output's audit record can attribute the produced
	// artifact to the connector version+hash and identity that produced it.
	// Zero on a deny or a dispatcher error, where no call reached a
	// connector.
	ConnectorVersion  string
	ConnectorHash     string
	IdentityLabel     string
	CredentialBinding string
	ConsentDecision   string
}

// dispatch runs one action through the enforcement pipeline. action is the
// decoded trust contract; args are the resolved binding values. callID
// identifies this specific call site (the step id, or a source-input marker)
// so the idempotency key is stable per call and never collides when the same
// action ref runs at two call sites. attempt is the 1-based call attempt so a
// retry of a non-idempotent action is refused rather than silently re-issued.
func (e *enforcer) dispatch(ctx context.Context, callID string, action Action, args map[string]any, attempt int) (dispatchOutcome, error) {
	tc := action.TrustContract

	// Idempotency: a retried dispatch of an action declared not safe-to-retry
	// is refused rather than re-issued, so a write is never silently doubled.
	if attempt > 1 && !tc.Idempotency.SafeToRetry {
		return dispatchOutcome{}, fmt.Errorf("flightplan: action %q is not safe to retry (attempt %d); refusing to re-issue", action.Ref, attempt)
	}

	out := dispatchOutcome{Approved: true}

	// Effect-routed approval (ADR-0009). A read runs unattended; everything
	// else blocks on the out-of-band decision.
	if requiresApproval(tc.Effect) {
		out.ApprovalRequested = true
		if e.approver == nil {
			return dispatchOutcome{}, fmt.Errorf("flightplan: action %q has effect %q and needs approval but no approver is configured", action.Ref, tc.Effect)
		}
		argsSummary := approvalArgsSummary(args)
		decision, err := e.approver.Approve(ctx, ApprovalRequest{
			ActionRef: action.Ref,
			Effect:    tc.Effect,
			Args:      argsSummary,
		})
		if err != nil {
			return dispatchOutcome{}, fmt.Errorf("flightplan: approval for %q failed: %w", action.Ref, err)
		}
		// Pending is the third outcome (#2100): no decision yet, so the run
		// SUSPENDS at this step rather than blocking. It is checked BEFORE the
		// approve/deny branches and short-circuits before Dispatch, so the effect
		// never fires. The sentinel carries the same redacted args the approver
		// saw so the suspend result presents the request without re-deriving it.
		// A pending decision that also (incoherently) sets Approved is treated as
		// pending: the third outcome wins, so no effect ever fires on a pending.
		if decision.Pending {
			return dispatchOutcome{ApprovalRequested: true},
				&PendingApprovalError{ActionRef: action.Ref, Effect: tc.Effect, Args: argsSummary}
		}
		out.Approved = decision.Approved
		if !decision.Approved {
			return dispatchOutcome{Approved: false, ApprovalRequested: true}, &DenyError{ActionRef: action.Ref, Reason: decision.Reason}
		}
	}

	// Thread a stable idempotency key when the contract declares one so the
	// upstream dedups a retried write. The key is derived from the action ref
	// and never carries a secret.
	dispatchArgs := args
	if tc.Idempotency.IdempotencyKey {
		dispatchArgs = withIdempotencyKey(args, callID, action.Ref)
	}

	res, err := e.dispatcher.Dispatch(ctx, action.Ref, dispatchArgs)
	if err != nil {
		return dispatchOutcome{Approved: out.Approved, ApprovalRequested: out.ApprovalRequested}, fmt.Errorf("flightplan: dispatch %q: %w", action.Ref, err)
	}

	// Redaction runs on the dispatch result BEFORE it enters the graph or any
	// audit summary (#1507). The original result is never surfaced unredacted.
	out.Result = applyRedaction(res.Output, tc.Redaction)
	// Carry the non-secret actor provenance the dispatcher surfaced (issue
	// #1753) so the recorded dispatch can attribute a materialized output to
	// the connector build and identity that produced it. Redaction does not
	// touch these: they are references (version, hash, label, binding name,
	// consent), never dataset fields.
	out.ConnectorVersion = res.ConnectorVersion
	out.ConnectorHash = res.ConnectorHash
	out.IdentityLabel = res.IdentityLabel
	out.CredentialBinding = res.CredentialBinding
	out.ConsentDecision = res.ConsentDecision
	return out, nil
}

// approvalArgsSummary builds the redacted args summary presented to the
// approver. Args are already resolved binding values (never secrets), but we
// pass a copy so the approver can never mutate the live args.
func approvalArgsSummary(args map[string]any) map[string]any {
	return deepCopyMap(args)
}

// idempotencyKeyField is the conventional arg key the runtime threads a stable
// idempotency key through when the contract declares idempotencyKey:true.
const idempotencyKeyField = "idempotencyKey"

// withIdempotencyKey returns a copy of args with a stable idempotency key
// added, derived from the call site (callID) and the action ref. The key lets
// the upstream deduplicate a retried write. It is deterministic for a given
// call so a retry of that call threads the same key, while two distinct call
// sites of the same action ref get distinct keys (no cross-call collision).
func withIdempotencyKey(args map[string]any, callID, ref string) map[string]any {
	out := deepCopyMap(args)
	if _, present := out[idempotencyKeyField]; !present {
		out[idempotencyKeyField] = "aileron-idem-" + callID + "-" + ref
	}
	return out
}
