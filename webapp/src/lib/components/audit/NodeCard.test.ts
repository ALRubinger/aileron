import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import NodeCard from './NodeCard.svelte';
import type { AuditEvent } from '$lib/api';
import type { ProvenanceNode } from '$lib/audit/provenance';

function event(payload: Record<string, unknown>): AuditEvent {
	return { audit_id: 'evt', event_type: 'output.materialized', timestamp: 't', payload };
}

const noop = () => {};

describe('NodeCard — per-node trust chrome', () => {
	it('renders a verified badge, signer, and identity for a verified step', () => {
		const node: ProvenanceNode = {
			id: 's1',
			kind: 'step',
			title: 'Step step-1',
			depth: 1,
			event: event({
				'aileron.plan.signature_status': 'verified',
				'aileron.plan.signed_by': 'sha256:0123456789abcdef0123456789abcdef',
				'aileron.actor.identity_label': 'analyst@corp'
			})
		};
		render(NodeCard, { node, onselect: noop });
		expect(screen.getByTestId('node-signature-badge')).toHaveTextContent(/verified/i);
		expect(screen.getByTestId('node-signer')).toBeInTheDocument();
		expect(screen.getByTestId('node-identity')).toHaveTextContent('analyst@corp');
		expect(screen.queryByTestId('node-unverified-warning')).not.toBeInTheDocument();
	});

	it('renders the acting identity with its consent decision', () => {
		const node: ProvenanceNode = {
			id: 'launch',
			kind: 'launch',
			title: 'Launch: ingest',
			depth: 3,
			event: event({
				'aileron.plan.signature_status': 'verified',
				'aileron.actor.identity_label': 'svc-bot',
				'aileron.consent.decision': 'granted'
			})
		};
		render(NodeCard, { node, onselect: noop });
		expect(screen.getByTestId('node-identity')).toHaveTextContent(/svc-bot/);
		expect(screen.getByTestId('node-identity')).toHaveTextContent(/consent granted/);
	});

	it('renders the unverified warning and no verified badge for an unverified step', () => {
		const node: ProvenanceNode = {
			id: 's2',
			kind: 'step',
			title: 'Step step-2',
			depth: 1,
			event: event({ 'aileron.plan.signature_status': 'failed' })
		};
		render(NodeCard, { node, onselect: noop });
		const warning = screen.getByTestId('node-unverified-warning');
		expect(warning).toHaveTextContent(/failed/i);
		expect(screen.queryByTestId('node-signature-badge')).not.toBeInTheDocument();
	});

	it('labels an unverified step with no status as "Unsigned"', () => {
		const node: ProvenanceNode = {
			id: 's3',
			kind: 'step',
			title: 'Step step-3',
			depth: 1,
			event: event({})
		};
		render(NodeCard, { node, onselect: noop });
		expect(screen.getByTestId('node-unverified-warning')).toHaveTextContent(/unsigned/i);
	});

	it('renders a provenance-gap affordance for a dangling artifact and no signature chrome', () => {
		const node: ProvenanceNode = {
			id: 'artifact:missing',
			kind: 'artifact',
			title: '(unresolved artifact)',
			subtitle: 'sha256:missing',
			depth: 2,
			dangling: true
		};
		render(NodeCard, { node, onselect: noop });
		expect(screen.getByTestId('node-provenance-gap')).toBeInTheDocument();
		expect(screen.queryByTestId('node-signature-badge')).not.toBeInTheDocument();
		expect(screen.queryByTestId('node-unverified-warning')).not.toBeInTheDocument();
	});

	it('renders no trust chrome for a literal node', () => {
		const node: ProvenanceNode = {
			id: 'literal:0',
			kind: 'literal',
			title: 'threshold',
			depth: 2,
			literal: { binding: 'threshold', source: 'inputs.threshold' }
		};
		render(NodeCard, { node, onselect: noop });
		expect(screen.queryByTestId('node-signature-badge')).not.toBeInTheDocument();
		expect(screen.queryByTestId('node-unverified-warning')).not.toBeInTheDocument();
		expect(screen.queryByTestId('node-signer')).not.toBeInTheDocument();
		expect(screen.queryByTestId('node-provenance-gap')).not.toBeInTheDocument();
	});

	it('renders no trust chrome for a non-dangling artifact node', () => {
		const node: ProvenanceNode = {
			id: 'artifact:root',
			kind: 'artifact',
			title: 'report.csv',
			depth: 0,
			event: event({ 'aileron.plan.signature_status': 'verified' })
		};
		render(NodeCard, { node, onselect: noop });
		expect(screen.queryByTestId('node-signature-badge')).not.toBeInTheDocument();
	});

	it('calls onselect when the card is clicked', async () => {
		const onselect = vi.fn();
		const node: ProvenanceNode = {
			id: 's1',
			kind: 'step',
			title: 'Step step-1',
			depth: 1,
			event: event({ 'aileron.plan.signature_status': 'verified' })
		};
		render(NodeCard, { node, onselect });
		await screen.getByRole('button', { name: /open details/i }).click();
		expect(onselect).toHaveBeenCalledWith(node);
	});
});
