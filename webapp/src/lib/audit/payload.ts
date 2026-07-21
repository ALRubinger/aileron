// Typed accessors over the flat `aileron.*` provenance keys that live
// inside an audit event's free-form `payload` map.
//
// The runtime emits provenance as flat string-keyed payload entries (see
// internal/flightplan/runtime/audit.go). Rather than pretend the wire
// shape is strongly typed, the webapp reads the keys it needs through
// these small getters, each of which is defensive about a missing or
// wrong-typed value. This keeps the provenance-graph module (which is
// pure and heavily unit-tested) free of `unknown`-narrowing noise.

import type { AuditEvent } from '$lib/api';

/** One `aileron.step.inputs[]` entry: a producing binding, the reference
 *  it resolved, and (for a hashed input) the content hash of the value it
 *  carried. A literal input has no `content_hash`. */
export type StepInput = {
	binding: string;
	source?: string;
	content_hash?: string;
	query_execution_id?: string;
};

function str(payload: Record<string, unknown>, key: string): string | undefined {
	const v = payload[key];
	return typeof v === 'string' && v.length > 0 ? v : undefined;
}

function num(payload: Record<string, unknown>, key: string): number | undefined {
	const v = payload[key];
	return typeof v === 'number' ? v : undefined;
}

export function outputName(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.output.name');
}
export function outputMime(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.output.mime');
}
export function outputContentHash(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.output.content_hash');
}
export function outputBytes(e: AuditEvent): number | undefined {
	return num(e.payload, 'aileron.output.bytes');
}
export function outputPath(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.output.path');
}

export function stepId(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.step.id');
}
export function stepKind(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.step.kind');
}
export function stepTransform(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.step.transform');
}
export function stepCommand(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.step.command');
}

/** Reads the `aileron.step.inputs[]` walk-back list, coercing each entry
 *  defensively. A non-array or malformed value yields an empty list. */
export function stepInputs(e: AuditEvent): StepInput[] {
	const raw = e.payload['aileron.step.inputs'];
	if (!Array.isArray(raw)) return [];
	const out: StepInput[] = [];
	for (const item of raw) {
		if (item === null || typeof item !== 'object') continue;
		const obj = item as Record<string, unknown>;
		// A step input without a real binding name is not a usable walk-back
		// entry; skip it alongside the other malformed shapes.
		if (typeof obj.binding !== 'string' || obj.binding.length === 0) continue;
		const entry: StepInput = { binding: obj.binding };
		if (typeof obj.source === 'string') entry.source = obj.source;
		if (typeof obj.content_hash === 'string') entry.content_hash = obj.content_hash;
		if (typeof obj.query_execution_id === 'string')
			entry.query_execution_id = obj.query_execution_id;
		out.push(entry);
	}
	return out;
}

/** One `aileron.resolved_inputs` entry: the launch-config input's binding
 *  name, the JS type of its resolved value, a size measure (character length
 *  of the value's JSON serialization), and the resolved value itself rendered
 *  as a display string. Source-resolved dataset inputs are excluded upstream
 *  (ADR-0027 audit boundary), so every entry here is a literal or dynamic
 *  launch-config value; secrets never enter `aileron.resolved_inputs`
 *  (launchConfigInputs, internal/flightplan/runtime/audit.go), so surfacing
 *  the value carries no sensitive data. A large value stays summarized by
 *  `size` alone and is revealed only on demand at the render layer. */
export type ResolvedInput = {
	name: string;
	type: string;
	size: number;
	value: string;
};

/** Reads the `aileron.resolved_inputs` map (name -> resolved value) that
 *  every `output.materialized` record carries and returns a list of
 *  name/type/size/value descriptors, sorted by name for stable rendering. A
 *  missing or non-object value yields an empty list. A scalar or small value
 *  is meant to be shown inline; a large one (`size` past the render layer's
 *  threshold) stays collapsed behind its descriptor and is revealed on
 *  demand. */
export function resolvedInputs(e: AuditEvent): ResolvedInput[] {
	const raw = e.payload['aileron.resolved_inputs'];
	if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) return [];
	const obj = raw as Record<string, unknown>;
	const out: ResolvedInput[] = [];
	for (const name of Object.keys(obj)) {
		const v = obj[name];
		out.push({
			name,
			type: resolvedInputType(v),
			size: resolvedInputSize(v),
			value: resolvedInputValue(v)
		});
	}
	out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
	return out;
}

/** Classifies a resolved-input value into a compact display type. Arrays
 *  and null narrow out of the bare `typeof object` bucket so the descriptor
 *  reads naturally. */
function resolvedInputType(v: unknown): string {
	if (v === null) return 'null';
	if (Array.isArray(v)) return 'array';
	return typeof v;
}

/** Size of a resolved-input value as the character length of its JSON
 *  serialization. A value that cannot be serialized (should not occur for
 *  JSON-carried payloads) reports 0. */
function resolvedInputSize(v: unknown): number {
	try {
		return JSON.stringify(v)?.length ?? 0;
	} catch {
		return 0;
	}
}

/** Renders a resolved-input value as a display string. A string is shown
 *  verbatim (no JSON quoting) so `region = us-east-1` reads naturally;
 *  every other value falls back to its JSON serialization. A value that
 *  cannot be serialized reports the empty string. */
function resolvedInputValue(v: unknown): string {
	if (typeof v === 'string') return v;
	try {
		return JSON.stringify(v) ?? '';
	} catch {
		return '';
	}
}

export function planSkill(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.plan.skill');
}
export function planSignedBy(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.plan.signed_by');
}
export function planSignatureStatus(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.plan.signature_status');
}
export function planContentHash(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.plan.content_hash');
}

/** The single `aileron.plan.signature_status` value the daemon writes for a
 *  plan whose signature actually verified. Mirrors the daemon constant
 *  `signatureStatusVerified` (internal/flightplan/runtime/audit.go) verbatim,
 *  so the webapp's notion of "verified" cannot drift from the runtime's. */
export const SIGNATURE_STATUS_VERIFIED = 'verified';

/** True only when an event's plan signature verified. This is the one
 *  truthiness test for "verified" on the client; the header, node cards, and
 *  chain rollup all route through it rather than inlining a string compare, so
 *  there is a single source of truth mirroring the daemon. */
export function signatureVerified(e: AuditEvent): boolean {
	return planSignatureStatus(e) === SIGNATURE_STATUS_VERIFIED;
}

/** Actor identity label: prefers the normalized `actor.identity_label`,
 *  falling back to the flat payload key. This is the acting credential's
 *  identity label specifically; it is intentionally blank when no
 *  credential backs the action, which the chain-of-custody Actor row treats
 *  as truthful. Use `actorLabel` for the "who launched" surfaces that must
 *  never render blank. */
export function actorIdentityLabel(e: AuditEvent): string | undefined {
	return e.actor?.identity_label ?? str(e.payload, 'aileron.actor.identity_label');
}

/** "Who launched" label for the header, landing cards, and the graph's
 *  launch node. Falls back identity_label -> display_name -> id so a
 *  human-launched or transform-produced artifact (which carries no
 *  credential identity label) still names its actor instead of rendering
 *  blank. Mirrors the server-side `actorLabel` (internal/audit/trace_payload.go). */
export function actorLabel(e: AuditEvent): string | undefined {
	return (
		actorIdentityLabel(e) ??
		(e.actor?.display_name && e.actor.display_name.length > 0
			? e.actor.display_name
			: undefined) ??
		(e.actor?.id && e.actor.id.length > 0 ? e.actor.id : undefined)
	);
}
export function actorCredentialBinding(e: AuditEvent): string | undefined {
	return e.actor?.credential_binding ?? str(e.payload, 'aileron.actor.credential_binding');
}
export function actorConnectorVersion(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.actor.connector_version');
}
export function actorConnectorHash(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.actor.connector_hash');
}
export function consentDecision(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.consent.decision');
}

export function invocationId(e: AuditEvent): string | undefined {
	return str(e.payload, 'aileron.invocation.id');
}

// --- one-line event summary (parity with the CLI) ---
//
// The `aileron audit` CLI renders each event as a single
// timestamp/audit-id/event-type/summary row. The summary column is a
// human-readable hint pulled from the most identifying payload fields of
// each event shape. The three functions below are a faithful port of
// `auditPayloadSummary`, `payloadName`, and `payloadConnector` from
// cmd/aileron/main.go, so the "Audit events" webapp feed reads the same
// line the CLI prints for the same event. Payload keys follow the
// OTel-namespaced audit schema (issue #390).

/** Reads a raw string payload value with no non-empty coercion, matching
 *  the Go type-assertion semantics the CLI relies on (an empty string is
 *  a present-but-empty value, distinct from a missing key). Used only by
 *  the summary port; the accessors above intentionally treat "" as
 *  absent. */
function rawStr(payload: Record<string, unknown>, key: string): string {
	const v = payload[key];
	return typeof v === 'string' ? v : '';
}

/** Pulls a human-identifying name from the namespaced payload. Action and
 *  binding events surface different keys; either resolves to the same
 *  single-line summary slot. Mirrors `payloadName` in cmd/aileron/main.go. */
export function payloadName(e: AuditEvent): string {
	const action = rawStr(e.payload, 'aileron.action.name');
	if (action) return action;
	const binding = rawStr(e.payload, 'aileron.binding.name');
	if (binding) return binding;
	return '';
}

/** Pulls the connector FQN backing an event, checking connector, action,
 *  and nested failure-detail keys in the CLI's order. Mirrors
 *  `payloadConnector` in cmd/aileron/main.go. */
export function payloadConnector(e: AuditEvent): string {
	const conn = rawStr(e.payload, 'aileron.connector.fqn');
	if (conn) return conn;
	const actionFqn = rawStr(e.payload, 'aileron.action.fqn');
	if (actionFqn) return actionFqn;
	const details = e.payload['aileron.failure.details'];
	if (details !== null && typeof details === 'object' && !Array.isArray(details)) {
		const c = (details as Record<string, unknown>)['connector'];
		if (typeof c === 'string' && c.length > 0) return c;
	}
	return '';
}

/** Renders a one-line, human-readable hint about an event, pulling the
 *  most identifying fields from each event shape the recorder emits.
 *  Faithful port of `auditPayloadSummary` in cmd/aileron/main.go — an
 *  `execution.failed` event summarizes its class (and connector, if any);
 *  every other event prefers a name, then a connector, then blank. */
export function auditPayloadSummary(e: AuditEvent): string {
	if (e.event_type === 'execution.failed') {
		const cls = rawStr(e.payload, 'aileron.failure.class');
		const conn = payloadConnector(e);
		if (conn) return `class=${cls} connector=${conn}`;
		return `class=${cls}`;
	}
	const name = payloadName(e);
	if (name) {
		const conn = payloadConnector(e);
		if (conn) return `name=${name} connector=${conn}`;
		return `name=${name}`;
	}
	const conn = payloadConnector(e);
	if (conn) return `connector=${conn}`;
	return '';
}

// --- formatting helpers (reused by cards) ---

/** Shortens a `sha256:<hex>` (or bare hex) content hash for card display,
 *  keeping the algorithm prefix and the first/last few hex chars. */
export function shortHash(hash: string | undefined): string {
	if (!hash) return '';
	const [algo, hex] = hash.includes(':') ? hash.split(':', 2) : ['', hash];
	if (hex.length <= 12) return hash;
	const abbreviated = `${hex.slice(0, 8)}…${hex.slice(-4)}`;
	return algo ? `${algo}:${abbreviated}` : abbreviated;
}

/** Human-readable byte-size formatter (B / KB / MB / GB, base-1024). */
export function formatBytes(bytes: number | undefined): string {
	if (bytes === undefined || bytes < 0) return '';
	if (bytes < 1024) return `${bytes} B`;
	const units = ['KB', 'MB', 'GB', 'TB'];
	let value = bytes / 1024;
	let unit = 0;
	while (value >= 1024 && unit < units.length - 1) {
		value /= 1024;
		unit++;
	}
	const rounded = value < 10 ? value.toFixed(1) : Math.round(value).toString();
	return `${rounded} ${units[unit]}`;
}
