import { describe, it, expect } from 'vitest';
import {
	shortHash,
	formatBytes,
	stepInputs,
	signatureVerified,
	actorLabel,
	resolvedInputs,
	SIGNATURE_STATUS_VERIFIED
} from './payload';
import type { AuditEvent } from '$lib/api';

function eventWith(payload: Record<string, unknown>): AuditEvent {
	return { audit_id: 'x', event_type: 'output.materialized', timestamp: 't', payload };
}

describe('signatureVerified', () => {
	it('mirrors the daemon constant', () => {
		expect(SIGNATURE_STATUS_VERIFIED).toBe('verified');
	});
	it('is true only when the plan signature status is "verified"', () => {
		expect(signatureVerified(eventWith({ 'aileron.plan.signature_status': 'verified' }))).toBe(true);
	});
	it('is false for a non-verified status', () => {
		expect(signatureVerified(eventWith({ 'aileron.plan.signature_status': 'unverified' }))).toBe(
			false
		);
		expect(signatureVerified(eventWith({ 'aileron.plan.signature_status': 'failed' }))).toBe(false);
	});
	it('is false when the status is missing or empty', () => {
		expect(signatureVerified(eventWith({}))).toBe(false);
		expect(signatureVerified(eventWith({ 'aileron.plan.signature_status': '' }))).toBe(false);
	});
});

describe('shortHash', () => {
	it('abbreviates a sha256 hash keeping the algorithm prefix', () => {
		expect(shortHash('sha256:0123456789abcdef0123456789abcdef')).toBe('sha256:01234567…cdef');
	});
	it('returns a short hash unchanged', () => {
		expect(shortHash('sha256:abc')).toBe('sha256:abc');
	});
	it('handles a bare hex digest with no prefix', () => {
		expect(shortHash('0123456789abcdef0123')).toBe('01234567…0123');
	});
	it('returns empty for undefined', () => {
		expect(shortHash(undefined)).toBe('');
	});
});

describe('formatBytes', () => {
	it('renders bytes below 1KB verbatim', () => {
		expect(formatBytes(512)).toBe('512 B');
	});
	it('renders KB with one decimal below 10', () => {
		expect(formatBytes(1536)).toBe('1.5 KB');
	});
	it('renders MB', () => {
		expect(formatBytes(5 * 1024 * 1024)).toBe('5.0 MB');
	});
	it('returns empty for undefined', () => {
		expect(formatBytes(undefined)).toBe('');
	});
});

describe('stepInputs', () => {
	function ev(payload: Record<string, unknown>): AuditEvent {
		return { audit_id: 'x', event_type: 'output.materialized', timestamp: 't', payload };
	}
	it('coerces well-formed entries', () => {
		const got = stepInputs(
			ev({
				'aileron.step.inputs': [
					{ binding: 'a', source: 's', content_hash: 'sha256:h' },
					{ binding: 'b', source: 'lit' }
				]
			})
		);
		expect(got).toEqual([
			{ binding: 'a', source: 's', content_hash: 'sha256:h' },
			{ binding: 'b', source: 'lit' }
		]);
	});
	it('returns [] for a non-array value', () => {
		expect(stepInputs(ev({ 'aileron.step.inputs': 'nope' }))).toEqual([]);
		expect(stepInputs(ev({}))).toEqual([]);
	});
	it('skips malformed entries, including objects without a real binding', () => {
		expect(
			stepInputs(ev({ 'aileron.step.inputs': [null, 42, {}, { binding: '' }, { binding: 'ok' }] }))
		).toEqual([{ binding: 'ok' }]);
	});
});

describe('actorLabel', () => {
	function evActor(
		actor: AuditEvent['actor'],
		payload: Record<string, unknown> = {}
	): AuditEvent {
		return { audit_id: 'x', event_type: 'output.materialized', timestamp: 't', payload, actor };
	}
	it('prefers the credential identity label', () => {
		expect(
			actorLabel(
				evActor({ id: 'runtime', type: 'agent', display_name: 'Bot', identity_label: 'analytics@corp' })
			)
		).toBe('analytics@corp');
	});
	it('falls back to display name when no identity label', () => {
		expect(actorLabel(evActor({ id: 'runtime', type: 'agent', display_name: 'Analytics Bot' }))).toBe(
			'Analytics Bot'
		);
	});
	it('falls back to id when no identity label or display name (human-launched)', () => {
		expect(actorLabel(evActor({ id: 'alr@host', type: 'human' }))).toBe('alr@host');
	});
	it('is undefined when the actor carries nothing usable', () => {
		expect(actorLabel(evActor(undefined))).toBeUndefined();
		expect(actorLabel(evActor({ id: '', type: 'human', display_name: '' }))).toBeUndefined();
	});
	it('still reads the flat payload identity label when the normalized field is absent', () => {
		expect(
			actorLabel(evActor(undefined, { 'aileron.actor.identity_label': 'flat@corp' }))
		).toBe('flat@corp');
	});
});

describe('resolvedInputs', () => {
	function ev(payload: Record<string, unknown>): AuditEvent {
		return { audit_id: 'x', event_type: 'output.materialized', timestamp: 't', payload };
	}
	it('returns name/type/size descriptors sorted by name', () => {
		expect(
			resolvedInputs(
				ev({
					'aileron.resolved_inputs': { region: 'us-east-1', limit: 10, flags: ['a', 'b'] }
				})
			)
		).toEqual([
			{ name: 'flags', type: 'array', size: JSON.stringify(['a', 'b']).length },
			{ name: 'limit', type: 'number', size: 2 },
			{ name: 'region', type: 'string', size: JSON.stringify('us-east-1').length }
		]);
	});
	it('classifies null and object values distinctly from bare typeof', () => {
		expect(
			resolvedInputs(ev({ 'aileron.resolved_inputs': { a: null, b: { k: 1 } } }))
		).toEqual([
			{ name: 'a', type: 'null', size: 4 },
			{ name: 'b', type: 'object', size: JSON.stringify({ k: 1 }).length }
		]);
	});
	it('returns [] for a missing, non-object, or array value', () => {
		expect(resolvedInputs(ev({}))).toEqual([]);
		expect(resolvedInputs(ev({ 'aileron.resolved_inputs': 'nope' }))).toEqual([]);
		expect(resolvedInputs(ev({ 'aileron.resolved_inputs': ['a', 'b'] }))).toEqual([]);
		expect(resolvedInputs(ev({ 'aileron.resolved_inputs': null }))).toEqual([]);
	});
});
