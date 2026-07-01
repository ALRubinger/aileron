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
	// One per-output provenance record per materialized artifact, whether the
	// materializing step was an action-call or a transform (#1752).
	for _, o := range st.outputs {
		ids = append(ids, sink.Record(ctx, buildOutputRecord(o, prov)))
	}
	// Per-launch summary: resolved input source bindings, by reference. Data
	// inputs are recorded by their source binding, never the dataset inline.
	ids = append(ids, sink.Record(ctx, buildLaunchRecord(st)))
	return ids
}

// buildOutputRecord builds one per-output provenance record (#1752) for a
// materialized artifact. The flat `aileron.*` field map carries the output's
// identity (name, mime, content hash, byte count), the originating step's
// provenance (id, kind, and — for a transform — the transform applied), and the
// launch's plan/invocation identity. The content hash is Artifact.Digest
// verbatim (the same `sha256:<hex>` printed at launch), so the audited hash
// equals the stdout digest for free. The transform key is present only for a
// transform step, so an action-call output does not carry an empty transform.
func buildOutputRecord(o materializedOutput, prov launchProvenance) AuditRecord {
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
	return AuditRecord{Kind: RecordKindOutput, Fields: fields}
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
