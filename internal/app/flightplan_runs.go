package app

import (
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// flightPlanRunTTL bounds how long a suspended run lingers in the registry
// without being touched (launched-then-abandoned by a crashed or idle agent).
// A resume touches the record, so an actively-driven multi-suspend run never
// expires mid-flight; only a run the agent walks away from is reaped. This
// bounds the registry's memory over a long-lived daemon session — the runs are
// already restart-scoped, so an hour is generous headroom for a human approval.
const flightPlanRunTTL = time.Hour

// flightPlanRunRegistry is the daemon's in-memory, session-scoped store of
// suspended Flight Plan runs (#2101). A launch that suspends (an unfulfilled
// llm-seam or a gated action) parks its record here keyed by the runtime-minted
// run id; the resume endpoint reads it back, replays the run with the
// accumulated memo, and either re-parks (a further suspend) or deletes it (a
// terminal completion or a denied approval).
//
// It deliberately mirrors internal/approval.PersistedRecord's field discipline
// (registration fields immutable, accumulated state mutated per transition)
// WITHOUT any persistence: the registry is a plain mutex-guarded map, lost on
// daemon restart. A restart mid-suspend orphans the run (the accepted gap in
// #2101's readiness brief); the agent re-launches. The runtime stays stateless
// across HTTP calls — this registry is the ONLY cross-call state the
// suspend/resume handshake needs.
type flightPlanRunRegistry struct {
	mu   sync.Mutex
	runs map[string]*flightPlanRunRecord
	// ttl bounds how long an un-touched run lingers before lazy expiry; now is
	// injectable so tests can drive expiry deterministically. Both default via
	// newFlightPlanRunRegistry.
	ttl time.Duration
	now func() time.Time
}

// flightPlanRunRecord is one suspended run's re-launch state: enough to replay
// the plan deterministically from the top, plus the per-run approval linkage
// for an in-flight gated step.
type flightPlanRunRecord struct {
	// Name + Version select the frozen unit to replay (immutable per run).
	Name    string
	Version string
	// Inputs are the literal launch overrides, replayed verbatim on every
	// resume so Phase A resolves identically (immutable per run).
	Inputs runtime.LaunchArgs
	// Outputs is the accumulated memo (stepId → its named outputs) at the point
	// of the latest suspend. On resume it is replayed as Options.ResumeOutputs so
	// every already-computed step is injected without re-execution (exactly-once
	// for effects). Mutated on each suspend via MergeOutputs.
	Outputs map[string]map[string]any
	// Approvals maps a gated action ref to the approval-queue entry id registered
	// for it in THIS run. The resume-path approver reads it to look up the
	// recorded decision (approved → replay the action; denied → fail closed)
	// without re-registering. Nil until the run first suspends on an approval.
	Approvals map[string]string
	// AuditedSeams records the seam step ids already written to the
	// flightplan.launch.seam audit trail in THIS run (#2119). The daemon emits
	// one seam record per distinct seam step at the suspend point and stamps the
	// step id here, so an empty-body re-suspend of the same still-unfulfilled seam
	// (which re-walks and re-suspends the same step) does not double-audit. A
	// fulfilled seam is memoized and never re-suspends. Nil until the run first
	// suspends on a seam.
	AuditedSeams map[string]bool
	// touched is the last time the record was created/updated/read, driving lazy
	// TTL expiry so an abandoned suspended run is eventually reaped.
	touched time.Time
}

// newFlightPlanRunRegistry returns an empty registry ready to accept runs, with
// the default TTL and a real clock.
func newFlightPlanRunRegistry() *flightPlanRunRegistry {
	return &flightPlanRunRegistry{
		runs: map[string]*flightPlanRunRecord{},
		ttl:  flightPlanRunTTL,
		now:  time.Now,
	}
}

// clock returns the registry's time source, defaulting to time.Now so a
// zero-value registry (defensive) still works.
func (r *flightPlanRunRegistry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// reapExpiredLocked drops every run untouched for longer than the TTL. Called
// under the lock from Put/Get so expiry is lazy (no background goroutine). A
// non-positive TTL disables expiry.
func (r *flightPlanRunRegistry) reapExpiredLocked(nowT time.Time) {
	if r.ttl <= 0 {
		return
	}
	for id, rec := range r.runs {
		if nowT.Sub(rec.touched) > r.ttl {
			delete(r.runs, id)
		}
	}
}

// Put stores (or replaces) the record for runID. The registry owns the stored
// pointer afterwards; callers must not mutate a record after Put except through
// the registry's own methods.
func (r *flightPlanRunRegistry) Put(runID string, rec *flightPlanRunRecord) {
	if r == nil || runID == "" || rec == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.Outputs == nil {
		rec.Outputs = map[string]map[string]any{}
	}
	if rec.Approvals == nil {
		rec.Approvals = map[string]string{}
	}
	if rec.AuditedSeams == nil {
		rec.AuditedSeams = map[string]bool{}
	}
	nowT := r.clock()
	rec.touched = nowT
	r.runs[runID] = rec
	r.reapExpiredLocked(nowT)
}

// Get returns the record for runID and whether it was found. The returned
// pointer is the live registry entry; callers read it under the assumption that
// the handler serializing this run is the only writer (the run is single-flight
// across the suspend/resume handshake).
func (r *flightPlanRunRegistry) Get(runID string) (*flightPlanRunRecord, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	nowT := r.clock()
	r.reapExpiredLocked(nowT)
	rec, ok := r.runs[runID]
	if ok {
		// A read is a touch: an actively-driven run never expires mid-flight.
		rec.touched = nowT
	}
	return rec, ok
}

// MergeOutputs folds a suspend's accumulated memo into the stored record,
// replacing each step's outputs by id (last-write-wins per step, matching the
// runtime's memoized-replay contract where a later suspend's memo is a superset
// of the earlier one). A missing run id is a no-op. Returns whether the run was
// found so a caller can distinguish "merged" from "unknown run".
func (r *flightPlanRunRegistry) MergeOutputs(runID string, outputs map[string]map[string]any) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.runs[runID]
	if !ok {
		return false
	}
	if rec.Outputs == nil {
		rec.Outputs = map[string]map[string]any{}
	}
	for id, named := range outputs {
		rec.Outputs[id] = named
	}
	rec.touched = r.clock()
	return true
}

// RecordApproval links a gated action ref to its registered approval-queue entry
// id for this run, so the resume-path approver looks it up instead of
// re-registering. A missing run id is a no-op.
func (r *flightPlanRunRegistry) RecordApproval(runID, actionRef, approvalID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.runs[runID]
	if !ok {
		return
	}
	if rec.Approvals == nil {
		rec.Approvals = map[string]string{}
	}
	rec.Approvals[actionRef] = approvalID
	rec.touched = r.clock()
}

// Delete removes the record for runID (a terminal completion or a denied
// approval). Idempotent: deleting an unknown id is a no-op.
func (r *flightPlanRunRegistry) Delete(runID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.runs, runID)
}
