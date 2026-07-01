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
	return AuditRecord{ActionRef: d.ActionRef, Fields: fields, Sink: d.Sink}
}

// emitAudit records every per-action audit record plus one per-launch summary
// record through the sink, returning the minted record ids in order. The sink
// is the customer-owned audit store (wired by the CLI). A nil sink emits
// nothing and returns no ids.
func emitAudit(ctx context.Context, sink AuditSink, st execState) []string {
	if sink == nil {
		return nil
	}
	var ids []string
	for _, d := range st.dispatches {
		ids = append(ids, sink.Record(ctx, buildActionRecord(d)))
	}
	// Per-launch summary: resolved inputs → materialized artifacts, by
	// reference. Data inputs are recorded by their source binding, never the
	// dataset inline.
	ids = append(ids, sink.Record(ctx, buildLaunchRecord(st)))
	return ids
}

// buildLaunchRecord builds the per-launch resolved-inputs→outputs record. It
// references source-input reads by their resolved binding and records each
// materialized output by name, path, and content digest, never inline data.
// The sha256 digest is the ADR-0027 snapshot identifier: it binds the output
// name to the exact bytes the run produced so a past launch is independently
// verifiable (hash the loose output file, compare to the recorded digest)
// without duplicating the dataset in the audit.
func buildLaunchRecord(st execState) AuditRecord {
	artifacts := make([]map[string]any, 0, len(st.artifacts))
	for _, a := range st.artifacts {
		artifacts = append(artifacts, map[string]any{
			"name":   a.Name,
			"path":   a.Path,
			"sha256": a.Digest,
		})
	}
	sources := map[string]any{}
	for name, sb := range st.inputs.SourceBindings {
		sources[name] = map[string]any{"actionRef": sb.ActionRef, "select": sb.Select}
	}
	return AuditRecord{
		Fields: map[string]any{
			"sourceInputBindings": sources,
			"materializedOutputs": artifacts,
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
