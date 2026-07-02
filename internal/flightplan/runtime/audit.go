package runtime

import "context"

// auditFieldValue computes the value for one declared audit field from an
// action dispatch. The closed field set mirrors the schema's auditStructure
// enum. Data reads are referenced by resolved binding (a result/snapshot
// summary), never the dataset inline (ADR-0027 audit boundary, #1523). A
// field the runtime cannot populate from the available context is omitted
// rather than guessed.
func auditFieldValue(field string, d actionDispatch) (any, bool) {
	switch field {
	case "operation-effect":
		return string(d.Effect), true
	case "approval-decision":
		if !d.ApprovalRequested {
			return "unattended", true
		}
		if d.Approved {
			return "approved", true
		}
		return "denied", true
	case "result":
		// A result/snapshot summary, never the full dataset: record the set of
		// top-level result keys so the audit references the shape, not the data.
		return resultSummary(d.Result), true
	case "network-target":
		return d.ActionRef, true
	default:
		// connector-hash, action-manifest-version, credential-binding,
		// identity-label, approved-input, request-summary, response-summary
		// are populated by the daemon-backed sink at the action boundary in
		// production; the runtime core does not synthesize them. Omitted here
		// so the record carries only fields it can populate truthfully.
		return nil, false
	}
}

// resultSummary returns an audit-safe summary of a dispatch result: the sorted
// top-level keys, never the values. This is the "result or snapshot identifier
// with a summary" the audit boundary permits.
func resultSummary(result map[string]any) map[string]any {
	keys := make([]string, 0, len(result))
	for k := range result {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return map[string]any{"fields": keys, "fieldCount": len(keys)}
}

// buildActionRecord builds the AuditRecord for one action dispatch from its
// declared audit fields. Only declared fields are emitted (ADR-0027).
func buildActionRecord(d actionDispatch) AuditRecord {
	fields := map[string]any{}
	for _, f := range d.AuditFields {
		if v, ok := auditFieldValue(f, d); ok {
			fields[f] = v
		}
	}
	return AuditRecord{Kind: RecordKindAction, ActionRef: d.ActionRef, Fields: fields, Sink: d.Sink}
}

// signatureStatusVerified is the single `aileron.plan.signature_status` value
// the runtime records. Reaching runPlan means LoadVerified → freeze.VerifyFrozen
// succeeded (both the ed25519 signature and the content-hash gate passed), so a
// plan that runs is, by construction, verified. Single-sourced here so the
// constant is not restated at each call site.
const signatureStatusVerified = "verified"

// launchProvenance is the per-launch identity threaded onto each
// output.materialized record: the frozen skill name, its verified content hash,
// the verified signer key fingerprint, the signature status, and a
// launch-scoped invocation id that correlates every record from one launch.
type launchProvenance struct {
	// Skill is the frozen skill name (plan.Name).
	Skill string
	// ContentHash is the verified `sha256:<hex>` content hash of the frozen unit.
	ContentHash string
	// SignedBy is the `sha256:<hex>` fingerprint of the verified author key.
	SignedBy string
	// SignatureStatus is the verification status ("verified").
	SignatureStatus string
	// InvocationID is the launch-scoped uuid correlating this launch's records.
	InvocationID string
}

// emitAudit records every per-action audit record, then one output.materialized
// record per materialized artifact, then one per-launch summary record through
// the sink, returning the minted record ids in order. The sink is the
// customer-owned audit store (wired by the CLI). A nil sink emits nothing and
// returns no ids.
func emitAudit(ctx context.Context, sink AuditSink, st execState, prov launchProvenance) []string {
	if sink == nil {
		return nil
	}
	var ids []string
	for _, d := range st.dispatches {
		ids = append(ids, sink.Record(ctx, buildActionRecord(d)))
	}
	// Index the dispatches by producing step id so an output record can resolve
	// the actor for its materializing step (an action-call attributes its own
	// dispatch; a transform walks its BindStep inputs to the producing
	// dispatches). Built once per launch, read per output (issue #1753).
	dispatchByStep := make(map[string]actionDispatch, len(st.dispatches))
	for _, d := range st.dispatches {
		dispatchByStep[d.StepID] = d
	}
	// The launch-config inputs (literal + dynamic only, source datasets
	// excluded) surface on each output record's `aileron.resolved_inputs` so a
	// reader sees the config the run used without inlining a source-resolved
	// dataset (ADR-0027 audit boundary). Computed once per launch.
	resolvedInputs := launchConfigInputs(st.inputs)
	// One per-output provenance record per materialized artifact, whether the
	// materializing step was an action-call or a transform (#1752).
	for _, o := range st.outputs {
		ids = append(ids, sink.Record(ctx, buildOutputRecord(o, prov, dispatchByStep, resolvedInputs)))
	}
	// Per-launch summary: resolved input source bindings, by reference. Data
	// inputs are recorded by their source binding, never the dataset inline.
	ids = append(ids, sink.Record(ctx, buildLaunchRecord(st)))
	return ids
}

// buildOutputRecord builds one per-output provenance record (#1752, enriched by
// #1753) for a materialized artifact. The flat `aileron.*` field map carries:
//   - the output's identity (name, mime, content hash, byte count);
//   - the originating step's provenance (id, kind, and — for a transform — the
//     transform applied);
//   - the input walk-back (`aileron.step.inputs`): per producing binding, the
//     reference it resolved and the `content_hash` of the value it carried
//     (plus a `query_execution_id` for a query-shaped input), so the output
//     resolves to the exact inputs by hash, never by inlining the dataset;
//   - the launch-config inputs (`aileron.resolved_inputs`): literal + dynamic
//     resolved values only, source datasets excluded (ADR-0027 audit boundary);
//   - the actor provenance (`aileron.actor.*`, `aileron.consent.decision`):
//     the connector build and identity that produced the artifact, resolved
//     from the materializing step's own dispatch (action-call) or by walking a
//     transform's upstreams to a single agreed identity — omitted, never
//     guessed, on divergence or absence; and
//   - the launch's plan/invocation identity.
//
// The content hash is Artifact.Digest verbatim (the same `sha256:<hex>` printed
// at launch), so the audited hash equals the stdout digest for free. The
// transform key is present only for a transform step, so an action-call output
// does not carry an empty transform.
func buildOutputRecord(o materializedOutput, prov launchProvenance, dispatchByStep map[string]actionDispatch, resolvedInputs map[string]any) AuditRecord {
	fields := map[string]any{
		"aileron.output.name": o.Artifact.Name,
		// Path is the artifact's declared on-disk path (empty for a retained
		// target:none output). It carries the write-location provenance the old
		// launch-summary array used to hold, so removing that array loses nothing.
		"aileron.output.path":           o.Artifact.Path,
		"aileron.output.mime":           o.Artifact.MimeType,
		"aileron.output.content_hash":   o.Artifact.Digest,
		"aileron.output.bytes":          len(o.Artifact.Content),
		"aileron.step.id":               o.StepID,
		"aileron.step.kind":             string(o.StepKind),
		"aileron.plan.skill":            prov.Skill,
		"aileron.plan.content_hash":     prov.ContentHash,
		"aileron.plan.signed_by":        prov.SignedBy,
		"aileron.plan.signature_status": prov.SignatureStatus,
		"aileron.invocation.id":         prov.InvocationID,
	}
	// The transform name is meaningful only for a transform step; omit it for an
	// action-call so the record does not carry an empty value.
	if o.StepKind == KindTransform && o.Transform != "" {
		fields["aileron.step.transform"] = o.Transform
	}
	// The input walk-back: the producing step's bindings, each hashed. Present
	// only when the step bound at least one input (a materializing step always
	// does in practice; guard so a no-binding step carries no empty key).
	if inputs := buildStepInputs(o); len(inputs) > 0 {
		fields["aileron.step.inputs"] = inputs
	}
	// Launch config the run used (literal + dynamic only). Always present as a
	// (possibly empty) object so a reader can tell "no launch config" from a
	// missing key.
	fields["aileron.resolved_inputs"] = resolvedInputs
	// Actor provenance, resolved truthfully or omitted (never guessed).
	for k, v := range resolveActor(o, dispatchByStep) {
		fields[k] = v
	}
	return AuditRecord{Kind: RecordKindOutput, Fields: fields}
}

// buildStepInputs builds the `aileron.step.inputs` list for a materialized
// output: one entry per producing binding, sorted by the step's input name for
// determinism. Each entry carries the input name (`binding`), the reference it
// resolved (`source`, the raw `inputs.<name>` / `steps.<id>.<out>` string), and
// the `content_hash` of the value it carried — the input-side snapshot
// identifier that makes the output walk back to its exact inputs by hash,
// never by inlining the dataset (ADR-0027 audit boundary). A `query_execution_id`
// is lifted (not synthesized) only when the resolved value is a JSON object
// carrying a `QueryExecutionId` string, mirroring auditFieldValue's
// omit-rather-than-guess discipline for non-query inputs. A value that cannot
// be canonicalized to a digest is recorded by reference alone (no content_hash)
// rather than dropping the whole entry.
func buildStepInputs(o materializedOutput) []map[string]any {
	names := make([]string, 0, len(o.Binds))
	for name := range o.Binds {
		names = append(names, name)
	}
	sortStrings(names)
	inputs := make([]map[string]any, 0, len(names))
	for _, name := range names {
		b := o.Binds[name]
		entry := map[string]any{"binding": name, "source": b.Raw}
		value := o.Resolved[name]
		if digest, err := canonicalValueDigest(value); err == nil {
			entry["content_hash"] = digest
		}
		if qid := queryExecutionID(value); qid != "" {
			entry["query_execution_id"] = qid
		}
		inputs = append(inputs, entry)
	}
	return inputs
}

// queryExecutionID extracts a query-execution id from a resolved binding value
// when the value is a JSON object carrying a `QueryExecutionId` string (the
// shape an Athena-style query action returns). It returns "" for any other
// shape — the honest omission auditFieldValue uses, never a synthesized id.
func queryExecutionID(value any) string {
	obj, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	qid, ok := obj["QueryExecutionId"].(string)
	if !ok {
		return ""
	}
	return qid
}

// resolveActor resolves the `aileron.actor.*` / `aileron.consent.decision`
// fields for a materialized output, or returns an empty map when it cannot
// populate them truthfully (issue #1753). Two cases:
//
//   - An action-call materializing step attributes its own dispatch: the actor
//     is the connector+identity that ran that step.
//   - A transform materializing step has no dispatch of its own, so it walks
//     its BindStep inputs back to the producing action-call dispatches. When
//     every data-producing upstream agrees on a single identity_label AND
//     credential_binding (the canonical single-connector case), that agreed
//     actor populates the record. On divergence (a genuinely multi-identity
//     transform) or absence, the actor keys are omitted — the walk-back to
//     identity still resolves through `aileron.step.inputs`, so the record
//     stays truthful without guessing one actor for many.
//
// Only non-empty actor fields are emitted, so a dispatch that resolved a
// connector build but no identity still surfaces the build without an empty
// identity key.
func resolveActor(o materializedOutput, dispatchByStep map[string]actionDispatch) map[string]any {
	switch o.StepKind {
	case KindActionCall:
		if d, ok := dispatchByStep[o.StepID]; ok {
			return actorFields(d)
		}
		return map[string]any{}
	case KindTransform:
		d, ok := agreedUpstreamActor(o, dispatchByStep)
		if !ok {
			return map[string]any{}
		}
		return actorFields(d)
	default:
		return map[string]any{}
	}
}

// agreedUpstreamActor walks a transform output's BindStep inputs to their
// producing dispatches and returns the single agreed actor when every
// data-producing upstream shares one non-empty identity_label and one
// credential_binding. It returns ok=false when there is no such agreement (no
// upstream dispatch, no identity resolved, or a divergence across upstreams),
// so the caller omits the actor keys rather than attributing the output to one
// of several identities.
func agreedUpstreamActor(o materializedOutput, dispatchByStep map[string]actionDispatch) (actionDispatch, bool) {
	var chosen actionDispatch
	found := false
	for _, b := range o.Binds {
		if b.Kind != BindStep {
			continue
		}
		d, ok := dispatchByStep[b.StepID]
		if !ok || d.IdentityLabel == "" {
			// An upstream that produced no dispatch or resolved no identity is
			// not a data-producing action-call with an attributable actor.
			continue
		}
		if !found {
			chosen = d
			found = true
			continue
		}
		if d.IdentityLabel != chosen.IdentityLabel || d.CredentialBinding != chosen.CredentialBinding {
			// Divergent identities across upstreams: no single truthful actor.
			return actionDispatch{}, false
		}
	}
	return chosen, found
}

// actorFields renders the non-empty actor provenance of one dispatch into the
// flat `aileron.actor.*` / `aileron.consent.decision` field map. Each key is
// emitted only when its source value is non-empty, so a dispatch missing (say)
// an identity still surfaces the connector build without an empty identity key.
func actorFields(d actionDispatch) map[string]any {
	out := map[string]any{}
	if d.IdentityLabel != "" {
		out["aileron.actor.identity_label"] = d.IdentityLabel
	}
	if d.CredentialBinding != "" {
		out["aileron.actor.credential_binding"] = d.CredentialBinding
	}
	if d.ConnectorVersion != "" {
		out["aileron.actor.connector_version"] = d.ConnectorVersion
	}
	if d.ConnectorHash != "" {
		out["aileron.actor.connector_hash"] = d.ConnectorHash
	}
	if d.ConsentDecision != "" {
		out["aileron.consent.decision"] = d.ConsentDecision
	}
	return out
}

// launchConfigInputs returns the launch-config inputs recorded on each output
// record: the literal + dynamic resolved values only, source-resolved datasets
// excluded. A source input is referenced by binding on the launch record and by
// content_hash / query_execution_id in `aileron.step.inputs`; it is never
// inlined here, honoring the ADR-0027 audit boundary (issue #1753).
func launchConfigInputs(ri ResolvedInputs) map[string]any {
	out := map[string]any{}
	for name, v := range ri.Values {
		if _, isSource := ri.SourceBindings[name]; isSource {
			continue
		}
		out[name] = v
	}
	return out
}

// buildLaunchRecord builds the per-launch resolved-inputs record. It references
// source-input reads by their resolved binding, never the dataset inline. The
// per-materialized-output provenance (name, hash, bytes, step) now lives on the
// individual output.materialized records (#1752), so the summary no longer
// lumps the outputs into an array; it keeps the input source bindings the
// per-output records do not carry.
func buildLaunchRecord(st execState) AuditRecord {
	sources := map[string]any{}
	for name, sb := range st.inputs.SourceBindings {
		sources[name] = map[string]any{"actionRef": sb.ActionRef, "select": sb.Select}
	}
	return AuditRecord{
		Kind: RecordKindLaunch,
		Fields: map[string]any{
			"sourceInputBindings": sources,
		},
	}
}

// sortStrings sorts in place without importing sort at every call site.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
