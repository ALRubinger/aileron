import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import RecordSidePanel from './RecordSidePanel.svelte';
import type { AuditEvent } from '$lib/api';
import type { ProvenanceNode } from '$lib/audit/provenance';

function event(payload: Record<string, unknown>): AuditEvent {
	return { audit_id: 'evt', event_type: 'output.materialized', timestamp: 't', payload };
}

describe('RecordSidePanel — chain-of-custody section', () => {
	it('groups signer, identity, binding, and consent under the custody section for a verified node', () => {
		const node: ProvenanceNode = {
			id: 's1',
			kind: 'step',
			title: 'Step step-1',
			depth: 1,
			event: event({
				'aileron.plan.signed_by': 'sha256:key',
				'aileron.plan.signature_status': 'verified',
				'aileron.actor.identity_label': 'analyst@corp',
				'aileron.actor.credential_binding': 'vault://cred/x',
				'aileron.consent.decision': 'granted'
			})
		};
		render(RecordSidePanel, { node });
		const section = screen.getByTestId('custody-section');
		expect(section).toHaveTextContent('sha256:key');
		expect(section).toHaveTextContent('analyst@corp');
		expect(section).toHaveTextContent('vault://cred/x');
		expect(section).toHaveTextContent('granted');
		expect(screen.getByTestId('custody-indicator')).toHaveTextContent('Verified');
	});

	it('shows a warning indicator for an unverified node', () => {
		const node: ProvenanceNode = {
			id: 's2',
			kind: 'step',
			title: 'Step step-2',
			depth: 1,
			event: event({
				'aileron.plan.signature_status': 'unverified',
				'aileron.actor.identity_label': 'analyst@corp'
			})
		};
		render(RecordSidePanel, { node });
		expect(screen.getByTestId('custody-indicator')).toHaveTextContent('Unverified');
	});

	it('shows a provenance-gap note for a dangling node', () => {
		const node: ProvenanceNode = {
			id: 'artifact:missing',
			kind: 'artifact',
			title: '(unresolved artifact)',
			subtitle: 'sha256:missing',
			depth: 2,
			dangling: true
		};
		render(RecordSidePanel, { node });
		expect(screen.getByTestId('custody-gap-note')).toHaveTextContent(/provenance gap/i);
	});

	it('does not render the custody section for a literal node', () => {
		const node: ProvenanceNode = {
			id: 'literal:0',
			kind: 'literal',
			title: 'threshold',
			depth: 2,
			literal: { binding: 'threshold', source: 'inputs.threshold' }
		};
		render(RecordSidePanel, { node });
		expect(screen.queryByTestId('custody-section')).not.toBeInTheDocument();
	});
});
