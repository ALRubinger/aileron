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
		const binding = typeof obj.binding === 'string' ? obj.binding : '';
		const entry: StepInput = { binding };
		if (typeof obj.source === 'string') entry.source = obj.source;
		if (typeof obj.content_hash === 'string') entry.content_hash = obj.content_hash;
		if (typeof obj.query_execution_id === 'string')
			entry.query_execution_id = obj.query_execution_id;
		out.push(entry);
	}
	return out;
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

/** Actor identity label: prefers the normalized `actor.identity_label`,
 *  falling back to the flat payload key. */
export function actorIdentityLabel(e: AuditEvent): string | undefined {
	return e.actor?.identity_label ?? str(e.payload, 'aileron.actor.identity_label');
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
