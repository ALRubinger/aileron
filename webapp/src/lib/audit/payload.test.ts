import { describe, it, expect } from 'vitest';
import { shortHash, formatBytes, stepInputs } from './payload';
import type { AuditEvent } from '$lib/api';

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
	it('skips malformed entries', () => {
		expect(stepInputs(ev({ 'aileron.step.inputs': [null, 42, { binding: 'ok' }] }))).toEqual([
			{ binding: 'ok' }
		]);
	});
});
