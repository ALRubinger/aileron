import { describe, it, expect } from 'vitest';
import { nodeTrust, chainTrustSummary, isTrustBearing } from './trust';
import type { AuditEvent } from '$lib/api';
import type { ProvenanceGraph, ProvenanceNode } from './provenance';

// Fixtures mirror the `ProvenanceGraph` render shape (see provenance.test.ts).
// A trust-bearing node (step/launch) carries an embedded event whose payload
// holds the signature status and signer; artifact/literal nodes do not.

function event(opts: { sigStatus?: string; signedBy?: string; skill?: string }): AuditEvent {
	const payload: Record<string, unknown> = {};
	if (opts.sigStatus) payload['aileron.plan.signature_status'] = opts.sigStatus;
	if (opts.signedBy) payload['aileron.plan.signed_by'] = opts.signedBy;
	if (opts.skill) payload['aileron.plan.skill'] = opts.skill;
	return { audit_id: 'evt', event_type: 'output.materialized', timestamp: 't', payload };
}

function step(id: string, opts: Parameters<typeof event>[0]): ProvenanceNode {
	return { id, kind: 'step', title: id, depth: 1, event: event(opts) };
}

function graphOf(nodes: ProvenanceNode[]): ProvenanceGraph {
	return { rootId: nodes[0]?.id ?? '', nodes, edges: [] };
}

describe('nodeTrust', () => {
	it('classifies a verified step as verified', () => {
		expect(nodeTrust(step('s1', { sigStatus: 'verified', signedBy: 'sha256:k' }))).toBe('verified');
	});
	it('classifies a launch node with a verified event as verified', () => {
		const launch: ProvenanceNode = {
			id: 'launch',
			kind: 'launch',
			title: 'Launch',
			depth: 3,
			event: event({ sigStatus: 'verified' })
		};
		expect(nodeTrust(launch)).toBe('verified');
	});
	it('classifies a trust-bearing node with an unverified signature as unverified', () => {
		expect(nodeTrust(step('s1', { sigStatus: 'unverified' }))).toBe('unverified');
		expect(nodeTrust(step('s2', {}))).toBe('unverified');
	});
	it('classifies a dangling node as a gap regardless of kind', () => {
		const dangling: ProvenanceNode = {
			id: 'artifact:missing',
			kind: 'artifact',
			title: '(unresolved artifact)',
			depth: 2,
			dangling: true
		};
		expect(nodeTrust(dangling)).toBe('gap');
		// Even a step marked dangling reads as a gap first.
		const danglingStep: ProvenanceNode = { ...step('s', { sigStatus: 'verified' }), dangling: true };
		expect(nodeTrust(danglingStep)).toBe('gap');
	});
	it('classifies literal and non-dangling artifact nodes as none', () => {
		const literal: ProvenanceNode = {
			id: 'literal:0',
			kind: 'literal',
			title: 'threshold',
			depth: 2,
			literal: { binding: 'threshold' }
		};
		const artifact: ProvenanceNode = {
			id: 'artifact:root',
			kind: 'artifact',
			title: 'report.csv',
			depth: 0,
			event: event({ sigStatus: 'verified' })
		};
		expect(nodeTrust(literal)).toBe('none');
		expect(nodeTrust(artifact)).toBe('none');
	});
});

describe('isTrustBearing', () => {
	it('is true for step/launch nodes with an event', () => {
		expect(isTrustBearing(step('s', { sigStatus: 'verified' }))).toBe(true);
	});
	it('is false for a step without an event', () => {
		expect(isTrustBearing({ id: 's', kind: 'step', title: 's', depth: 1 })).toBe(false);
	});
	it('is false for artifact and literal kinds', () => {
		expect(isTrustBearing({ id: 'a', kind: 'artifact', title: 'a', depth: 0 })).toBe(false);
	});
});

describe('chainTrustSummary', () => {
	it('reports a fully verified chain with zero counts', () => {
		const g = graphOf([
			step('s1', { sigStatus: 'verified', signedBy: 'sha256:k' }),
			step('s2', { sigStatus: 'verified', signedBy: 'sha256:k' })
		]);
		const s = chainTrustSummary(g);
		expect(s.fullyVerified).toBe(true);
		expect(s.unverifiedCount).toBe(0);
		expect(s.gapCount).toBe(0);
		expect(s.trustBearingCount).toBe(2);
		expect(s.label).toBe('Chain verified');
	});

	it('counts a dangling upstream as a gap and is not fully verified', () => {
		const g = graphOf([
			step('s1', { sigStatus: 'verified', signedBy: 'sha256:k' }),
			{ id: 'artifact:missing', kind: 'artifact', title: '(unresolved)', depth: 2, dangling: true }
		]);
		const s = chainTrustSummary(g);
		expect(s.gapCount).toBe(1);
		expect(s.fullyVerified).toBe(false);
		expect(s.label).toContain('1 provenance gap');
	});

	it('counts an unverified step and renders the "N of M steps unverified" label', () => {
		const g = graphOf([
			step('s1', { sigStatus: 'verified', signedBy: 'sha256:k' }),
			step('s2', { sigStatus: 'unverified' }),
			step('s3', { sigStatus: 'verified', signedBy: 'sha256:k' })
		]);
		const s = chainTrustSummary(g);
		expect(s.unverifiedCount).toBe(1);
		expect(s.trustBearingCount).toBe(3);
		expect(s.fullyVerified).toBe(false);
		expect(s.label).toBe('1 of 3 steps unverified');
	});

	it('combines the unverified and gap counts in the label', () => {
		const g = graphOf([
			step('s1', { sigStatus: 'unverified' }),
			step('s2', { sigStatus: 'verified', signedBy: 'sha256:k' }),
			{ id: 'artifact:missing', kind: 'artifact', title: '(unresolved)', depth: 2, dangling: true }
		]);
		const s = chainTrustSummary(g);
		expect(s.label).toBe('1 of 2 steps unverified · 1 provenance gap');
	});

	it('de-duplicates signers across trust-bearing nodes in first-seen order', () => {
		const g = graphOf([
			step('s1', { sigStatus: 'verified', signedBy: 'sha256:alice' }),
			step('s2', { sigStatus: 'verified', signedBy: 'sha256:bob' }),
			step('s3', { sigStatus: 'verified', signedBy: 'sha256:alice' })
		]);
		expect(chainTrustSummary(g).signers).toEqual(['sha256:alice', 'sha256:bob']);
	});

	it('reports not-fully-verified with no attestation when there are no trust-bearing nodes', () => {
		const g = graphOf([
			{ id: 'artifact:root', kind: 'artifact', title: 'report.csv', depth: 0 },
			{ id: 'literal:0', kind: 'literal', title: 'threshold', depth: 1 }
		]);
		const s = chainTrustSummary(g);
		expect(s.fullyVerified).toBe(false);
		expect(s.trustBearingCount).toBe(0);
		expect(s.label).toBe('No signed steps recorded');
	});
});
