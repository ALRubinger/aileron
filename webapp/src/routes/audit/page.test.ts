import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	listRecentMaterialized: vi.fn(),
	getAuditByContentHash: vi.fn(),
	getAuditByInvocation: vi.fn(),
	EVENT_OUTPUT_MATERIALIZED: 'output.materialized'
}));

import Page from './+page.svelte';
import {
	listRecentMaterialized,
	getAuditByContentHash,
	getAuditByInvocation,
	type AuditEvent
} from '$lib/api';

// --- fixtures ---

function materialized(opts: {
	hash: string;
	name?: string;
	skill?: string;
	actor?: string;
	invocation?: string;
	inputs?: Array<{ binding: string; source?: string; content_hash?: string }>;
	sigStatus?: string;
	signedBy?: string;
	timestamp?: string;
}): AuditEvent {
	const payload: Record<string, unknown> = {
		'aileron.output.name': opts.name ?? 'report.csv',
		'aileron.output.content_hash': opts.hash,
		'aileron.output.mime': 'text/csv',
		'aileron.output.bytes': 2048,
		'aileron.step.id': 'step-1',
		'aileron.step.kind': 'action_call'
	};
	if (opts.skill) payload['aileron.plan.skill'] = opts.skill;
	if (opts.actor) payload['aileron.actor.identity_label'] = opts.actor;
	if (opts.invocation) payload['aileron.invocation.id'] = opts.invocation;
	if (opts.inputs) payload['aileron.step.inputs'] = opts.inputs;
	if (opts.sigStatus) payload['aileron.plan.signature_status'] = opts.sigStatus;
	if (opts.signedBy) payload['aileron.plan.signed_by'] = opts.signedBy;
	return {
		audit_id: `evt-${opts.hash}`,
		event_type: 'output.materialized',
		timestamp: opts.timestamp ?? '2026-07-04T00:00:00Z',
		payload
	};
}

function setLocationSearch(search: string) {
	Object.defineProperty(window, 'location', {
		writable: true,
		value: { ...window.location, search }
	});
}

beforeEach(() => {
	vi.mocked(listRecentMaterialized).mockReset();
	vi.mocked(getAuditByContentHash).mockReset();
	vi.mocked(getAuditByInvocation).mockReset();
	setLocationSearch('');
});

afterEach(() => {
	setLocationSearch('');
});

describe('Audit page — landing', () => {
	it('renders recent materialized artifacts', async () => {
		vi.mocked(listRecentMaterialized).mockResolvedValue([
			materialized({ hash: 'sha256:a', name: 'sales.csv', skill: 'ingest', actor: 'analyst' })
		]);

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('recent-list')).toBeInTheDocument();
			expect(screen.getByText('sales.csv')).toBeInTheDocument();
		});
		expect(vi.mocked(listRecentMaterialized)).toHaveBeenCalled();
	});

	it('shows the empty state when nothing is materialized', async () => {
		vi.mocked(listRecentMaterialized).mockResolvedValue([]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('recent-empty')).toBeInTheDocument();
		});
	});

	it('surfaces a listing error', async () => {
		vi.mocked(listRecentMaterialized).mockRejectedValue(new Error('audit down'));
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('recent-error')).toHaveTextContent('audit down');
		});
	});
});

describe('Audit page — deep link (#1894 affordance)', () => {
	it('resolves and renders the artifact graph on load, skipping the landing', async () => {
		setLocationSearch('?content_hash=sha256:root');
		const root = materialized({
			hash: 'sha256:root',
			name: 'final.csv',
			skill: 'pipeline',
			signedBy: 'sha256:key',
			sigStatus: 'verified'
		});
		vi.mocked(getAuditByContentHash).mockResolvedValue([root]);

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('provenance-graph')).toBeInTheDocument();
		});
		// The landing must not render.
		expect(screen.queryByTestId('artifact-lookup')).not.toBeInTheDocument();
		expect(vi.mocked(listRecentMaterialized)).not.toHaveBeenCalled();
		// Header shows the verified signature badge and skill.
		expect(screen.getByTestId('header-signature-badge')).toHaveTextContent(/verified/i);
		expect(screen.getByTestId('header-skill')).toHaveTextContent('pipeline');
	});

	it('shows a friendly message when the deep-linked hash resolves to nothing', async () => {
		setLocationSearch('?content_hash=sha256:missing');
		vi.mocked(getAuditByContentHash).mockResolvedValue([]);

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('graph-error')).toHaveTextContent(/no artifact found/i);
		});
	});
});

describe('Audit page — graph + side panel', () => {
	it('renders node cards for a linked fixture and opens the side panel on click', async () => {
		vi.mocked(listRecentMaterialized).mockResolvedValue([
			materialized({ hash: 'sha256:root', name: 'joined.csv' })
		]);
		const upstream = materialized({ hash: 'sha256:up', name: 'raw.json' });
		const root = materialized({
			hash: 'sha256:root',
			name: 'joined.csv',
			skill: 'etl',
			inputs: [{ binding: 'data', source: 'steps.q.rows', content_hash: 'sha256:up' }]
		});
		vi.mocked(getAuditByContentHash).mockImplementation(async (h: string) => {
			if (h === 'sha256:root') return [root];
			if (h === 'sha256:up') return [upstream];
			return [];
		});

		render(Page);

		const card = await screen.findByText('joined.csv');
		await fireEvent.click(card);

		await waitFor(() => {
			expect(screen.getByTestId('provenance-graph')).toBeInTheDocument();
		});
		// Both artifacts appear as node cards.
		await waitFor(() => {
			expect(screen.getAllByTestId('provenance-node').length).toBeGreaterThanOrEqual(2);
		});

		// Clicking a node opens the side panel with full fields.
		const nodeButtons = screen.getAllByRole('button', { name: /open details/i });
		await fireEvent.click(nodeButtons[0]);
		await waitFor(() => {
			expect(screen.getByTestId('side-panel-title')).toBeInTheDocument();
		});
	});
});

describe('Audit page — timeline toggle', () => {
	it('switches to timeline and fetches by invocation id in time order', async () => {
		setLocationSearch('?content_hash=sha256:root');
		const root = materialized({
			hash: 'sha256:root',
			name: 'final.csv',
			invocation: 'inv-9'
		});
		vi.mocked(getAuditByContentHash).mockResolvedValue([root]);
		vi.mocked(getAuditByInvocation).mockResolvedValue([
			materialized({ hash: 'sha256:root', name: 'final.csv', invocation: 'inv-9', timestamp: '2026-07-04T00:00:02Z' }),
			materialized({ hash: 'sha256:up', name: 'raw.json', invocation: 'inv-9', timestamp: '2026-07-04T00:00:01Z' })
		]);

		render(Page);

		const timelineBtn = await screen.findByTestId('view-timeline');
		await fireEvent.click(timelineBtn);

		await waitFor(() => {
			expect(vi.mocked(getAuditByInvocation)).toHaveBeenCalledWith('inv-9');
			expect(screen.getByTestId('provenance-timeline')).toBeInTheDocument();
		});
		const rows = screen.getAllByTestId('timeline-row');
		expect(rows.length).toBe(2);
	});
});

describe('Audit page — collapse behavior for large literal input', () => {
	it('renders a large literal collapsed with a view toggle that reveals the full value', async () => {
		const bigValue = 'x'.repeat(300);
		setLocationSearch('?content_hash=sha256:root');
		const root = materialized({
			hash: 'sha256:root',
			name: 'out.csv',
			inputs: [{ binding: 'blob', source: bigValue }]
		});
		vi.mocked(getAuditByContentHash).mockResolvedValue([root]);

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('provenance-graph')).toBeInTheDocument();
		});

		// Find and click the literal node card.
		const literalNode = await waitFor(() => {
			const nodes = screen.getAllByTestId('provenance-node');
			const lit = nodes.find((n) => n.getAttribute('data-node-kind') === 'literal');
			expect(lit).toBeTruthy();
			return lit!;
		});
		const litButton = literalNode.querySelector('button')!;
		await fireEvent.click(litButton);

		// The descriptor renders collapsed: the view toggle is present but the
		// full value is hidden until expanded (the collapsible keeps content
		// mounted-but-hidden, so assert visibility, not presence).
		await waitFor(() => {
			expect(screen.getByTestId('literal-descriptor')).toBeInTheDocument();
			expect(screen.getByTestId('literal-view-toggle')).toBeInTheDocument();
		});
		const collapsed = screen.queryByTestId('literal-full-value');
		// Either fully unmounted or mounted-but-hidden while collapsed.
		if (collapsed) expect(collapsed).not.toBeVisible();

		await fireEvent.click(screen.getByTestId('literal-view-toggle'));
		await waitFor(() => {
			const full = screen.getByTestId('literal-full-value');
			expect(full).toBeVisible();
			expect(full).toHaveTextContent(bigValue);
		});
	});
});
