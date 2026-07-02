package runtime

import (
	"context"
	"errors"
	"testing"
)

// fakeDispatcher records calls and returns a canned result. It is the action
// boundary stand-in; secrets never reach it because bindings are references.
// provenance, when set, is copied onto the returned DispatchResult so tests can
// assert the enforcer threads the daemon's actor provenance through (#1753).
type fakeDispatcher struct {
	calls      []dispatchCall
	result     map[string]any
	failErr    error
	provenance DispatchResult
}

type dispatchCall struct {
	ref  string
	args map[string]any
}

func (f *fakeDispatcher) Dispatch(_ context.Context, ref string, args map[string]any) (DispatchResult, error) {
	f.calls = append(f.calls, dispatchCall{ref: ref, args: args})
	if f.failErr != nil {
		return DispatchResult{}, f.failErr
	}
	res := f.result
	if res == nil {
		res = map[string]any{"ok": true}
	}
	return DispatchResult{
		Output:            res,
		ConnectorVersion:  f.provenance.ConnectorVersion,
		ConnectorHash:     f.provenance.ConnectorHash,
		IdentityLabel:     f.provenance.IdentityLabel,
		CredentialBinding: f.provenance.CredentialBinding,
		ConsentDecision:   f.provenance.ConsentDecision,
	}, nil
}

// fakeApprover returns a fixed decision and records the requests it saw.
type fakeApprover struct {
	decision Decision
	err      error
	requests []ApprovalRequest
}

func (f *fakeApprover) Approve(_ context.Context, req ApprovalRequest) (Decision, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return Decision{}, f.err
	}
	return f.decision, nil
}

func readAction(ref string) Action {
	return Action{Ref: ref, TrustContract: TrustContract{Effect: EffectRead, Hosts: []string{"h"}}}
}

func writeAction(ref string, safeToRetry, idemKey bool) Action {
	return Action{Ref: ref, TrustContract: TrustContract{
		Effect:      EffectWrite,
		Hosts:       []string{"h"},
		Idempotency: Idempotency{SafeToRetry: safeToRetry, IdempotencyKey: idemKey},
	}}
}

func TestDispatch_ReadRunsWithoutApproval(t *testing.T) {
	disp := &fakeDispatcher{}
	app := &fakeApprover{}
	e := &enforcer{dispatcher: disp, approver: app}
	out, err := e.dispatch(context.Background(), "test", readAction("aileron:m.read"), map[string]any{"a": 1}, 1)
	if err != nil {
		t.Fatalf("read dispatch: %v", err)
	}
	if out.ApprovalRequested {
		t.Error("a read must not request approval")
	}
	if len(app.requests) != 0 {
		t.Error("approver must not be called for a read")
	}
	if len(disp.calls) != 1 {
		t.Fatalf("want 1 dispatch, got %d", len(disp.calls))
	}
}

func TestDispatch_WriteBlocksThenProceedsOnApprove(t *testing.T) {
	disp := &fakeDispatcher{}
	app := &fakeApprover{decision: Decision{Approved: true}}
	e := &enforcer{dispatcher: disp, approver: app}
	out, err := e.dispatch(context.Background(), "test", writeAction("aileron:t.write", false, false), nil, 1)
	if err != nil {
		t.Fatalf("write dispatch: %v", err)
	}
	if !out.ApprovalRequested || !out.Approved {
		t.Error("a write must request approval and proceed on approve")
	}
	if len(disp.calls) != 1 {
		t.Error("an approved write must dispatch")
	}
}

func TestDispatch_DeleteAbortsOnDeny(t *testing.T) {
	disp := &fakeDispatcher{}
	app := &fakeApprover{decision: Decision{Approved: false, Reason: "no"}}
	del := Action{Ref: "aileron:t.delete", TrustContract: TrustContract{Effect: EffectDelete, Hosts: []string{"h"}}}
	e := &enforcer{dispatcher: disp, approver: app}
	_, err := e.dispatch(context.Background(), "test", del, nil, 1)
	var de *DenyError
	if !errors.As(err, &de) {
		t.Fatalf("a denied delete must return a *DenyError, got %v", err)
	}
	if len(disp.calls) != 0 {
		t.Error("a denied action must NOT dispatch")
	}
}

func TestDispatch_NonRetryableNotReissued(t *testing.T) {
	disp := &fakeDispatcher{}
	app := &fakeApprover{decision: Decision{Approved: true}}
	e := &enforcer{dispatcher: disp, approver: app}
	// attempt 2 of a safeToRetry:false action must be refused.
	_, err := e.dispatch(context.Background(), "test", writeAction("aileron:t.write", false, false), nil, 2)
	if err == nil {
		t.Fatal("retry of a non-idempotent action must be refused")
	}
	if len(disp.calls) != 0 {
		t.Error("a refused retry must not dispatch")
	}
}

func TestDispatch_IdempotencyKeyThreaded(t *testing.T) {
	disp := &fakeDispatcher{}
	app := &fakeApprover{decision: Decision{Approved: true}}
	e := &enforcer{dispatcher: disp, approver: app}
	_, err := e.dispatch(context.Background(), "test", writeAction("aileron:t.write", true, true), map[string]any{"body": "x"}, 1)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got := disp.calls[0].args[idempotencyKeyField]
	if got == nil || got == "" {
		t.Error("an idempotencyKey:true contract must thread a stable key into dispatch args")
	}
}

func TestDispatch_NoApproverForWriteErrors(t *testing.T) {
	e := &enforcer{dispatcher: &fakeDispatcher{}, approver: nil}
	if _, err := e.dispatch(context.Background(), "test", writeAction("aileron:t.write", true, false), nil, 1); err == nil {
		t.Fatal("a write with no approver configured must error, not run unattended")
	}
}

func TestDispatch_RedactsBeforeSurfacing(t *testing.T) {
	disp := &fakeDispatcher{result: map[string]any{"email": "a@b.com", "id": 7}}
	app := &fakeApprover{}
	act := readAction("aileron:m.read")
	act.TrustContract.Redaction = []RedactionRule{{Field: "email", Rule: RedactDrop}}
	e := &enforcer{dispatcher: disp, approver: app}
	out, err := e.dispatch(context.Background(), "test", act, nil, 1)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, present := out.Result["email"]; present {
		t.Error("redaction must drop the declared field before it surfaces")
	}
	if out.Result["id"] != 7 {
		t.Error("non-redacted fields must survive")
	}
}

func TestDispatch_IdempotencyKeyDistinctPerCallSite(t *testing.T) {
	disp := &fakeDispatcher{}
	app := &fakeApprover{decision: Decision{Approved: true}}
	e := &enforcer{dispatcher: disp, approver: app}
	act := writeAction("aileron:t.write", true, true)
	if _, err := e.dispatch(context.Background(), "step:a", act, map[string]any{}, 1); err != nil {
		t.Fatalf("dispatch a: %v", err)
	}
	if _, err := e.dispatch(context.Background(), "step:b", act, map[string]any{}, 1); err != nil {
		t.Fatalf("dispatch b: %v", err)
	}
	ka := disp.calls[0].args[idempotencyKeyField]
	kb := disp.calls[1].args[idempotencyKeyField]
	if ka == kb {
		t.Error("two call sites of the same action ref must get distinct idempotency keys")
	}
}

func TestDispatch_DispatcherErrorSurfaces(t *testing.T) {
	disp := &fakeDispatcher{failErr: errors.New("boom")}
	e := &enforcer{dispatcher: disp, approver: &fakeApprover{}}
	if _, err := e.dispatch(context.Background(), "test", readAction("aileron:m.read"), nil, 1); err == nil {
		t.Fatal("a dispatcher error must surface")
	}
}

func TestDispatch_CarriesActorProvenance(t *testing.T) {
	// The enforcer threads the daemon's non-secret actor provenance from the
	// DispatchResult onto the dispatchOutcome so the recorded dispatch can
	// attribute a materialized output to the connector build and identity
	// that produced it (#1753).
	disp := &fakeDispatcher{provenance: DispatchResult{
		ConnectorVersion:  "2.3.1",
		ConnectorHash:     "sha256:conn",
		IdentityLabel:     "work",
		CredentialBinding: "aws_sigv4/athena/work",
		ConsentDecision:   "unattended",
	}}
	e := &enforcer{dispatcher: disp, approver: &fakeApprover{}}
	out, err := e.dispatch(context.Background(), "test", readAction("aileron:athena.query"), nil, 1)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if out.ConnectorVersion != "2.3.1" || out.ConnectorHash != "sha256:conn" {
		t.Errorf("connector build = %q/%q, want 2.3.1/sha256:conn", out.ConnectorVersion, out.ConnectorHash)
	}
	if out.IdentityLabel != "work" || out.CredentialBinding != "aws_sigv4/athena/work" {
		t.Errorf("identity/binding = %q/%q", out.IdentityLabel, out.CredentialBinding)
	}
	if out.ConsentDecision != "unattended" {
		t.Errorf("consent = %q, want unattended", out.ConsentDecision)
	}
}

func TestDispatch_DenyLeavesProvenanceZero(t *testing.T) {
	// A denied action never reaches a connector, so the outcome carries no
	// actor provenance — the zero value, not a stale/guessed one.
	disp := &fakeDispatcher{provenance: DispatchResult{IdentityLabel: "should-not-appear"}}
	app := &fakeApprover{decision: Decision{Approved: false, Reason: "no"}}
	del := Action{Ref: "aileron:t.delete", TrustContract: TrustContract{Effect: EffectDelete, Hosts: []string{"h"}}}
	e := &enforcer{dispatcher: disp, approver: app}
	out, err := e.dispatch(context.Background(), "test", del, nil, 1)
	var de *DenyError
	if !errors.As(err, &de) {
		t.Fatalf("a denied delete must return a *DenyError, got %v", err)
	}
	if out.IdentityLabel != "" || out.ConnectorVersion != "" || out.CredentialBinding != "" {
		t.Errorf("a denied action must leave actor provenance zero, got %+v", out)
	}
}
