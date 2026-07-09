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

// buildReachRecord builds one per-tool-step reach record (#1784, enforcement
// truth from #1829) from a captured reachRecord. The flat `aileron.*` field
// map carries the step's id, the declared operation effect, the hosts the
// step ran under, and the truthful `aileron.reach.enforced` marker:
// enforced:true means the hosts are the verified lock's sealed reach and the
// step's subprocess ran under a step-scoped proxy credential restricted to
// exactly them; enforced:false means the step declared a contract with no
// sealed entry (only reachable outside the verified load path, which refuses
// that shape). enforced:true is a proxy-credential guarantee, not a
// network-layer egress guarantee: it attests the step ran under a step-scoped
// proxy credential restricted to its sealed reach, not that a non-cooperative
// subprocess ignoring HTTPS_PROXY (dialing a raw socket) was blocked — that is
// out of scope for this marker.
func buildReachRecord(r reachRecord, prov launchProvenance) AuditRecord {
	fields := map[string]any{
		"aileron.step.id":        r.StepID,
		"aileron.reach.effect":   string(r.Effect),
		"aileron.reach.hosts":    r.Hosts,
		"aileron.reach.enforced": r.Enforced,
	}
	for k, v := range provenanceFields(prov) {
		fields[k] = v
	}
	return AuditRecord{Kind: RecordKindReach, Fields: fields}
}

// provenanceFields returns the flat plan/invocation provenance keys carried on
// every record from one launch: the launch-scoped invocation id that correlates
// the launch's records and the frozen plan's verified identity (skill name,
// content hash, signer fingerprint, signature status). These are the keys the
// invocation filter (internal/audit/mem.go) and the webapp Timeline accessor
// (webapp/src/lib/audit/payload.ts) read at the top level, so stamping them on
// the reach and launch records (not only the output records) keeps
// flightplan.launch and flightplan.launch.reach visible under
// GET /v1/audit?invocation_id=<id> (#1928). Each key is emitted only when its
// source value is non-empty, mirroring the omit-rather-than-guess discipline the
// actor fields use.
func provenanceFields(prov launchProvenance) map[string]any {
	out := map[string]any{}
	if prov.Skill != "" {
		out["aileron.plan.skill"] = prov.Skill
	}
	if prov.ContentHash != "" {
		out["aileron.plan.content_hash"] = prov.ContentHash
	}
	if prov.SignedBy != "" {
		out["aileron.plan.signed_by"] = prov.SignedBy
	}
	if prov.SignatureStatus != "" {
		out["aileron.plan.signature_status"] = prov.SignatureStatus
	}
	if prov.InvocationID != "" {
		out["aileron.invocation.id"] = prov.InvocationID
	}
	return out
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

// emitStepAudit records every per-step audit record for the steps that ran this
// call — the per-action records, the reach records, and the per-output
// provenance records — but NOT the per-launch summary. It is the shared body of
// emitAudit and the audit path a SUSPEND takes (#2100): a suspended call is not
// a terminal launch, so it emits its step records now and defers the launch
// summary to the completing resume. Because a memo-injected (replayed) step
// never appends to st.dispatches, st.reaches, or st.outputs, this emits nothing
// for an injected step, so a multi-resume sequence records each real execution
// exactly once. A nil sink emits nothing and returns no ids.
func emitStepAudit(ctx context.Context, sink AuditSink, st execState, prov launchProvenance) []string {
	if sink == nil {
		return nil
	}
	var ids []string
	for _, d := range st.dispatches {
		ids = append(ids, sink.Record(ctx, buildActionRecord(d)))
	}
	// One reach record per tool step that carried a trust contract
	// (#1784/#1829), marked enforced truthfully per record.
	for _, r := range st.reaches {
		ids = append(ids, sink.Record(ctx, buildReachRecord(r, prov)))
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
	return ids
}

// emitAudit records every per-step audit record (emitStepAudit) then one
// per-launch summary record through the sink, returning the minted record ids
// in order. It is the terminal-launch audit path: a completed run (or a hard
// error) emits the launch summary; a SUSPEND uses emitStepAudit and defers the
// summary to the completing resume (#2100). A nil sink emits nothing and returns
// no ids.
func emitAudit(ctx context.Context, sink AuditSink, st execState, prov launchProvenance) []string {
	if sink == nil {
		return nil
	}
	ids := emitStepAudit(ctx, sink, st, prov)
	// Per-launch summary: resolved input source bindings, by reference. Data
	// inputs are recorded by their source binding, never the dataset inline.
	ids = append(ids, sink.Record(ctx, buildLaunchRecord(st, prov)))
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
		"aileron.output.path":         o.Artifact.Path,
		"aileron.output.mime":         o.Artifact.MimeType,
		"aileron.output.content_hash": o.Artifact.Digest,
		"aileron.output.bytes":        len(o.Artifact.Content),
		"aileron.step.id":             o.StepID,
		"aileron.step.kind":           string(o.StepKind),
	}
	// The plan/invocation provenance keys (skill, content hash, signer,
	// signature status, invocation id) are shared with the reach and launch
	// records; single-sourced through provenanceFields so all three carry the
	// same flat keys the invocation filter reads (#1928).
	for k, v := range provenanceFields(prov) {
		fields[k] = v
	}
	// A tool-materialized output (#1829) records the executed argv: `kind:
	// tool` is a first-class step kind, so aileron.step.kind is already the
	// literal "tool" from StepKind above, and the discriminator here keys off
	// the kind. The argv is the executed-command identity; the environment
	// identity is the plan's single composed pin, already on the launch
	// record, so no per-step aileron.step.image exists. aileron.step.calls[]
	// (per-egress attribution, Half B) is out of scope and omitted entirely.
	if o.StepKind == KindTool {
		fields["aileron.step.command"] = o.Command
		// The input-derived index marker (#1958): which argv positions were
		// instantiated from `{{ inputs.<name> }}` interpolation. Present only
		// when non-empty, so a token-free tool command records no marker and its
		// record is byte-identical to the pre-interpolation shape.
		if len(o.CommandDerived) > 0 {
			fields["aileron.step.command_derived"] = o.CommandDerived
		}
	}
	// The transform name is meaningful only for a transform step; omit it for
	// an action-call or tool step so the record does not carry an empty value.
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
// never by inlining the dataset (ADR-0027 audit boundary). For a file-map
// carrier the `content_hash` is the digest over its carried content bytes — the
// same digest-space as the producer's `aileron.output.content_hash` — so the
// input links to the producing `output.materialized` record (#1891/#1912); a
// plain-data input keeps the whole-value-object digest, which already matches
// the producer's digest for a plain-data carrier. A `query_execution_id`
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
		if digest, err := inputContentDigest(value); err == nil {
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
// producing dispatches and returns the single agreed actor only when EVERY
// BindStep upstream carries complete actor provenance and they all agree on the
// full tuple (identity_label, credential_binding, connector_version,
// connector_hash, consent). It returns ok=false the moment any upstream is
// missing its dispatch, resolved no identity/binding, or diverges from the
// others on any tuple field — so a transform that mixes an attributable read
// with an unattributable or differently-authorized one is never mislabeled with
// a single actor. The walk-back to identity still resolves through
// aileron.step.inputs, so omitting the actor keeps the record truthful rather
// than guessing one actor for a genuinely multi-source transform.
func agreedUpstreamActor(o materializedOutput, dispatchByStep map[string]actionDispatch) (actionDispatch, bool) {
	var chosen actionDispatch
	found := false
	for _, b := range o.Binds {
		if b.Kind != BindStep {
			// A non-step binding (a launch input) is not a data-producing actor.
			continue
		}
		d, ok := dispatchByStep[b.StepID]
		if !ok || d.IdentityLabel == "" || d.CredentialBinding == "" {
			// A data-producing upstream with no dispatch or incomplete actor
			// provenance means there is no single truthful actor for the output.
			return actionDispatch{}, false
		}
		if !found {
			chosen = d
			found = true
			continue
		}
		if d.IdentityLabel != chosen.IdentityLabel ||
			d.CredentialBinding != chosen.CredentialBinding ||
			d.ConnectorVersion != chosen.ConnectorVersion ||
			d.ConnectorHash != chosen.ConnectorHash ||
			d.ConsentDecision != chosen.ConsentDecision {
			// Divergent provenance across upstreams: no single truthful actor.
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
func buildLaunchRecord(st execState, prov launchProvenance) AuditRecord {
	sources := map[string]any{}
	for name, sb := range st.inputs.SourceBindings {
		sources[name] = map[string]any{"actionRef": sb.ActionRef, "select": sb.Select}
	}
	fields := map[string]any{
		"sourceInputBindings": sources,
	}
	// Stamp the same flat plan/invocation provenance the output and reach
	// records carry, so the per-launch summary is visible under an
	// invocation-filtered query and in the /audit Timeline (#1928).
	for k, v := range provenanceFields(prov) {
		fields[k] = v
	}
	return AuditRecord{Kind: RecordKindLaunch, Fields: fields}
}

// sortStrings sorts in place without importing sort at every call site.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
