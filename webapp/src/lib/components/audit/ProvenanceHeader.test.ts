import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import ProvenanceHeader from './ProvenanceHeader.svelte';
import type { AuditEvent } from '$lib/api';
import type { ProvenanceGraph, ProvenanceNode } from '$lib/audit/provenance';

function event(opts: {
	skill?: string;
	actor?: string;
	sigStatus?: string;
	signedBy?: string;
}): AuditEvent {
	const payload: Record<string, unknown> = {};
	if (opts.skill) payload['aileron.plan.skill'] = opts.skill;
	if (opts.actor) payload['aileron.actor.identity_label'] = opts.actor;
	if (opts.sigStatus) payload['aileron.plan.signature_status'] = opts.sigStatus;
	if (opts.signedBy) payload['aileron.plan.signed_by'] = opts.signedBy;
	return { audit_id: 'evt', event_type: 'output.materialized', timestamp: 't', payload };
}

function step(id: string, e: AuditEvent): ProvenanceNode {
	return { id, kind: 'step', title: id, depth: 1, event: e };
}

function graphOf(root: AuditEvent, nodes: ProvenanceNode[]): ProvenanceGraph {
	return { rootId: 'root', nodes, edges: [] };
}

describe('ProvenanceHeader — chain-level trust rollup', () => {
	it('shows a green chain-verified rollup for an all-verified graph', () => {
		const root = event({ skill: 'pipeline', actor: 'analyst', sigStatus: 'verified', signedBy: 'sha256:k' });
		const graph = graphOf(root, [step('s1', root), step('s2', root)]);
		render(ProvenanceHeader, { root, graph });
		expect(screen.getByTestId('header-skill')).toHaveTextContent('pipeline');
		expect(screen.getByTestId('header-signature-badge')).toHaveTextContent(/verified/i);
		expect(screen.getByTestId('header-chain-badge')).toHaveTextContent('Chain verified');
	});

	it('surfaces the gap count when an upstream is dangling', () => {
		const root = event({ skill: 'etl', sigStatus: 'verified', signedBy: 'sha256:k' });
		const graph = graphOf(root, [
			step('s1', root),
			{ id: 'artifact:missing', kind: 'artifact', title: '(unresolved)', depth: 2, dangling: true }
		]);
		render(ProvenanceHeader, { root, graph });
		expect(screen.getByTestId('header-chain-badge')).toHaveTextContent(/provenance gap/i);
	});

	it('surfaces the unverified count and a warning chain badge', () => {
		const root = event({ skill: 'etl', sigStatus: 'unverified' });
		const graph = graphOf(root, [
			step('s1', root),
			step('s2', event({ sigStatus: 'verified', signedBy: 'sha256:k' }))
		]);
		render(ProvenanceHeader, { root, graph });
		expect(screen.getByTestId('header-chain-badge')).toHaveTextContent('1 of 2 steps unverified');
		// Root signature status also reflects the unverified root.
		expect(screen.getByTestId('header-signature-badge')).toHaveTextContent(/unverified/i);
	});

	it('lists the de-duplicated signer set across the chain', () => {
		const root = event({ skill: 'etl', sigStatus: 'verified', signedBy: 'sha256:alice' });
		const graph = graphOf(root, [
			step('s1', root),
			step('s2', event({ sigStatus: 'verified', signedBy: 'sha256:bob' })),
			step('s3', event({ sigStatus: 'verified', signedBy: 'sha256:alice' }))
		]);
		render(ProvenanceHeader, { root, graph });
		const signedBy = screen.getByTestId('header-signed-by');
		expect(signedBy).toHaveTextContent('sha256:alice');
		expect(signedBy).toHaveTextContent('sha256:bob');
	});
});
