import { describe, it, expect } from 'vitest';
import {
	shortHash,
	formatBytes,
	stepInputs,
	signatureVerified,
	actorLabel,
	resolvedInputs,
	auditPayloadSummary,
	payloadName,
	payloadConnector,
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

describe('audit event summary (CLI parity)', () => {
	// Mirrors the Go cases in cmd/aileron/main_test.go
	// (TestAuditPayloadSummary_PerEventShape) plus the approval shape called
	// out in #2162, so the webapp feed and the `aileron audit` CLI render the
	// identical summary line for the same event.
	function ev(eventType: string, payload: Record<string, unknown>): AuditEvent {
		return { audit_id: 'x', event_type: eventType, timestamp: 't', payload };
	}

	describe('payloadName', () => {
		it('prefers the action name', () => {
			expect(payloadName(ev('action.installed', { 'aileron.action.name': 'ship-update' }))).toBe(
				'ship-update'
			);
		});
		it('falls back to the binding name', () => {
			expect(payloadName(ev('binding.created', { 'aileron.binding.name': 'slack-prod' }))).toBe(
				'slack-prod'
			);
		});
		it('is empty when neither key is present', () => {
			expect(payloadName(ev('unknown', {}))).toBe('');
		});
	});

	describe('payloadConnector', () => {
		it('prefers the connector fqn', () => {
			expect(
				payloadConnector(ev('binding.created', { 'aileron.connector.fqn': 'github://aileron/slack' }))
			).toBe('github://aileron/slack');
		});
		it('falls back to the action fqn', () => {
			expect(
				payloadConnector(ev('action.installed', { 'aileron.action.fqn': 'github://aileron/ship' }))
			).toBe('github://aileron/ship');
		});
		it('reads the nested failure-details connector as a last resort', () => {
			expect(
				payloadConnector(
					ev('execution.failed', { 'aileron.failure.details': { connector: 'github://aileron/x' } })
				)
			).toBe('github://aileron/x');
		});
		it('is empty when no connector key resolves', () => {
			expect(payloadConnector(ev('unknown', {}))).toBe('');
			expect(payloadConnector(ev('execution.failed', { 'aileron.failure.details': 'nope' }))).toBe(
				''
			);
		});
	});

	describe('auditPayloadSummary', () => {
		it('renders an action.installed event as name + connector (CLI parity)', () => {
			expect(
				auditPayloadSummary(
					ev('action.installed', {
						'aileron.action.name': 'ship-update',
						'aileron.action.fqn': 'github://aileron/ship-update'
					})
				)
			).toBe('name=ship-update connector=github://aileron/ship-update');
		});
		it('renders a binding event as connector only (CLI parity)', () => {
			expect(
				auditPayloadSummary(ev('binding.created', { 'aileron.connector.fqn': 'github://aileron/slack' }))
			).toBe('connector=github://aileron/slack');
		});
		it('renders an empty summary for an event with no useful keys (CLI parity)', () => {
			expect(auditPayloadSummary(ev('unknown', {}))).toBe('');
		});
		it('renders execution.failed as class (+ connector when present)', () => {
			expect(
				auditPayloadSummary(ev('execution.failed', { 'aileron.failure.class': 'timeout' }))
			).toBe('class=timeout');
			expect(
				auditPayloadSummary(
					ev('execution.failed', {
						'aileron.failure.class': 'auth',
						'aileron.failure.details': { connector: 'github://aileron/slack' }
					})
				)
			).toBe('class=auth connector=github://aileron/slack');
		});
		it('summarizes an approval.approved connector-action event by name + connector', () => {
			// An approved connector-action approval carries the gated action's
			// identity in the OTel-namespaced keys; the feed reads it the same
			// way the CLI does, yielding a name+connector one-liner.
			expect(
				auditPayloadSummary(
					ev('approval.approved', {
						'aileron.action.name': 'send_email',
						'aileron.connector.fqn': 'github://aileron/google'
					})
				)
			).toBe('name=send_email connector=github://aileron/google');
		});
	});
});

describe('resolvedInputs', () => {
	function ev(payload: Record<string, unknown>): AuditEvent {
		return { audit_id: 'x', event_type: 'output.materialized', timestamp: 't', payload };
	}
	it('returns name/type/size/value descriptors sorted by name', () => {
		expect(
			resolvedInputs(
				ev({
					'aileron.resolved_inputs': { region: 'us-east-1', limit: 10, flags: ['a', 'b'] }
				})
			)
		).toEqual([
			{ name: 'flags', type: 'array', size: JSON.stringify(['a', 'b']).length, value: '["a","b"]' },
			{ name: 'limit', type: 'number', size: 2, value: '10' },
			{
				name: 'region',
				type: 'string',
				size: JSON.stringify('us-east-1').length,
				value: 'us-east-1'
			}
		]);
	});
	it('carries a string value verbatim, without JSON quoting', () => {
		const [region] = resolvedInputs(ev({ 'aileron.resolved_inputs': { region: 'us-east-1' } }));
		expect(region.value).toBe('us-east-1');
	});
	it('carries a large value in full so the render layer can reveal it on demand', () => {
		const big = 'x'.repeat(200);
		const [blob] = resolvedInputs(ev({ 'aileron.resolved_inputs': { blob: big } }));
		expect(blob.value).toBe(big);
		expect(blob.size).toBe(202);
	});
	it('classifies null and object values distinctly from bare typeof', () => {
		expect(
			resolvedInputs(ev({ 'aileron.resolved_inputs': { a: null, b: { k: 1 } } }))
		).toEqual([
			{ name: 'a', type: 'null', size: 4, value: 'null' },
			{ name: 'b', type: 'object', size: JSON.stringify({ k: 1 }).length, value: '{"k":1}' }
		]);
	});
	it('returns [] for a missing, non-object, or array value', () => {
		expect(resolvedInputs(ev({}))).toEqual([]);
		expect(resolvedInputs(ev({ 'aileron.resolved_inputs': 'nope' }))).toEqual([]);
		expect(resolvedInputs(ev({ 'aileron.resolved_inputs': ['a', 'b'] }))).toEqual([]);
		expect(resolvedInputs(ev({ 'aileron.resolved_inputs': null }))).toEqual([]);
	});
});
