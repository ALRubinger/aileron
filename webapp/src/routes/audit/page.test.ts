import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	listRecentMaterialized: vi.fn(),
	getAuditTrace: vi.fn(),
	getAuditByContentHash: vi.fn(),
	getAuditByInvocation: vi.fn(),
	EVENT_OUTPUT_MATERIALIZED: 'output.materialized'
}));

import Page from './+page.svelte';
import {
	listRecentMaterialized,
	getAuditTrace,
	getAuditByContentHash,
	getAuditByInvocation,
	type AuditEvent,
	type AuditTraceNode,
	type AuditTraceResponse
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

// Assembles the server-assembled trace response the daemon would return
// for a root event, mirroring `GET /v1/audit/trace` (#1913): the root
// artifact, its step, literal-input leaves, recursively-resolved upstream
// artifacts (from `upstreams`), dangling markers for unresolved hashes,
// and a terminal launch node. This stands in for the server walk so the
// page tests exercise the single-round-trip contract against a realistic
// wire shape.
function traceFor(root: AuditEvent, upstreams: Record<string, AuditEvent> = {}): AuditTraceResponse {
	const nodes: AuditTraceNode[] = [];
	const edges: AuditTraceResponse['edges'] = [];
	const seen = new Set<string>();
	let litSeq = 0;

	function walk(event: AuditEvent, depth: number): string {
		const hash = event.payload['aileron.output.content_hash'] as string | undefined;
		const artifactId = hash ? `artifact:${hash}` : `artifact:node${nodes.length}`;
		nodes.push({
			id: artifactId,
			kind: 'artifact',
			title: (event.payload['aileron.output.name'] as string) ?? '(unnamed artifact)',
			subtitle: 'text/csv',
			depth,
			content_hash: hash,
			event
		});
		if (hash) seen.add(hash);
		const stepId = `step:${artifactId}`;
		nodes.push({ id: stepId, kind: 'step', title: 'Step step-1', depth: depth + 1, event });
		edges.push({ from: artifactId, to: stepId });

		const inputs = (event.payload['aileron.step.inputs'] as
			| Array<{ binding: string; source?: string; content_hash?: string }>
			| undefined) ?? [];
		for (const input of inputs) {
			if (!input.content_hash) {
				const litId = `literal:${litSeq++}`;
				nodes.push({
					id: litId,
					kind: 'literal',
					title: input.binding || '(literal)',
					subtitle: input.source,
					depth: depth + 2,
					literal: { binding: input.binding, source: input.source }
				});
				edges.push({ from: stepId, to: litId });
				continue;
			}
			if (seen.has(input.content_hash)) {
				edges.push({ from: stepId, to: `artifact:${input.content_hash}` });
				continue;
			}
			const up = upstreams[input.content_hash];
			if (!up) {
				const dId = `artifact:${input.content_hash}`;
				nodes.push({
					id: dId,
					kind: 'artifact',
					title: '(unresolved artifact)',
					subtitle: input.content_hash,
					depth: depth + 2,
					content_hash: input.content_hash,
					dangling: true
				});
				edges.push({ from: stepId, to: dId });
				continue;
			}
			seen.add(input.content_hash);
			const upId = walk(up, depth + 2);
			edges.push({ from: stepId, to: upId });
		}
		return artifactId;
	}

	const rootId = walk(root, 0);
	const maxDepth = nodes.reduce((m, n) => Math.max(m, n.depth), 0);
	const skill = root.payload['aileron.plan.skill'] as string | undefined;
	nodes.push({
		id: 'launch',
		kind: 'launch',
		title: skill ? `Launch: ${skill}` : 'Launch',
		depth: maxDepth + 1,
		event: root
	});
	edges.push({ from: `step:${rootId}`, to: 'launch' });
	return { root_id: rootId, nodes, edges };
}

function setLocationSearch(search: string) {
	Object.defineProperty(window, 'location', {
		writable: true,
		value: { ...window.location, search }
	});
}

beforeEach(() => {
	vi.mocked(listRecentMaterialized).mockReset();
	vi.mocked(getAuditTrace).mockReset();
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

	it('labels a landing card by the credential identity when present', async () => {
		vi.mocked(listRecentMaterialized).mockResolvedValue([
			materialized({ hash: 'sha256:a', name: 'sales.csv', actor: 'analyst@corp' })
		]);
		render(Page);
		const card = await screen.findByTestId('recent-artifact');
		expect(card).toHaveTextContent('by analyst@corp');
	});

	it('falls back to the actor id on a landing card for a human-launched artifact', async () => {
		// Regression for the blank-actor defect: a human launch carries only
		// actor.id/type, so the "by ..." line must render the id rather than
		// dropping out.
		const event = materialized({ hash: 'sha256:a', name: 'sales.csv' });
		event.actor = { id: 'alr@host', type: 'human' };
		vi.mocked(listRecentMaterialized).mockResolvedValue([event]);
		render(Page);
		const card = await screen.findByTestId('recent-artifact');
		expect(card).toHaveTextContent('by alr@host');
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
	it('resolves and renders the artifact graph on load in a single trace call, skipping the landing', async () => {
		setLocationSearch('?content_hash=sha256:root');
		const root = materialized({
			hash: 'sha256:root',
			name: 'final.csv',
			skill: 'pipeline',
			signedBy: 'sha256:key',
			sigStatus: 'verified'
		});
		vi.mocked(getAuditTrace).mockResolvedValue(traceFor(root));

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('provenance-graph')).toBeInTheDocument();
		});
		// The landing must not render.
		expect(screen.queryByTestId('artifact-lookup')).not.toBeInTheDocument();
		expect(vi.mocked(listRecentMaterialized)).not.toHaveBeenCalled();
		// Exactly one round-trip, rooted at the deep-linked hash — no N+1
		// per-input fan-out (the old getAuditByContentHash walk is gone).
		expect(vi.mocked(getAuditTrace)).toHaveBeenCalledTimes(1);
		expect(vi.mocked(getAuditTrace)).toHaveBeenCalledWith({ contentHash: 'sha256:root' });
		expect(vi.mocked(getAuditByContentHash)).not.toHaveBeenCalled();
		// Header shows the verified signature badge and skill from the root event.
		expect(screen.getByTestId('header-signature-badge')).toHaveTextContent(/verified/i);
		expect(screen.getByTestId('header-skill')).toHaveTextContent('pipeline');
		// The chain-of-custody rollup reads clean and per-node trust badges render.
		expect(screen.getByTestId('header-chain-badge')).toHaveTextContent('Chain verified');
		expect(screen.getAllByTestId('node-signature-badge').length).toBeGreaterThanOrEqual(1);
	});

	it('shows a friendly message when the trace resolves to null (unknown hash 404)', async () => {
		setLocationSearch('?content_hash=sha256:missing');
		vi.mocked(getAuditTrace).mockResolvedValue(null);

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('graph-error')).toHaveTextContent(/no artifact found/i);
		});
	});
});

describe('Audit page — graph + side panel', () => {
	it('renders node cards for a linked fixture in one trace call and opens the side panel on click', async () => {
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
		vi.mocked(getAuditTrace).mockResolvedValue(traceFor(root, { 'sha256:up': upstream }));

		render(Page);

		const card = await screen.findByText('joined.csv');
		await fireEvent.click(card);

		await waitFor(() => {
			expect(screen.getByTestId('provenance-graph')).toBeInTheDocument();
		});
		// Both artifacts appear as node cards (parity with the old client walk).
		await waitFor(() => {
			expect(screen.getAllByTestId('provenance-node').length).toBeGreaterThanOrEqual(2);
		});
		// A single trace round-trip served the whole graph — no per-hash fan-out.
		expect(vi.mocked(getAuditTrace)).toHaveBeenCalledTimes(1);
		expect(vi.mocked(getAuditByContentHash)).not.toHaveBeenCalled();

		// Clicking a node opens the side panel with full fields.
		const nodeButtons = screen.getAllByRole('button', { name: /open details/i });
		await fireEvent.click(nodeButtons[0]);
		await waitFor(() => {
			expect(screen.getByTestId('side-panel-title')).toBeInTheDocument();
		});
	});

	it('renders a dangling upstream node rather than dropping it', async () => {
		setLocationSearch('?content_hash=sha256:root');
		const root = materialized({
			hash: 'sha256:root',
			name: 'out.csv',
			inputs: [{ binding: 'data', content_hash: 'sha256:missing' }]
		});
		// No upstream provided for sha256:missing → server marks it dangling.
		vi.mocked(getAuditTrace).mockResolvedValue(traceFor(root));

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('provenance-graph')).toBeInTheDocument();
		});
		// The dangling node card is present and shows the unresolved-upstream cue.
		const dangling = await waitFor(() => {
			const nodes = screen.getAllByTestId('provenance-node');
			const node = nodes.find((n) => n.textContent?.includes('unresolved artifact'));
			expect(node).toBeTruthy();
			return node!;
		});
		expect(dangling).toHaveTextContent(/unresolved upstream/i);
	});

	it('surfaces the header gap count and a per-node provenance-gap affordance for a dangling upstream', async () => {
		setLocationSearch('?content_hash=sha256:root');
		const root = materialized({
			hash: 'sha256:root',
			name: 'out.csv',
			skill: 'etl',
			sigStatus: 'verified',
			signedBy: 'sha256:key',
			inputs: [{ binding: 'data', content_hash: 'sha256:missing' }]
		});
		vi.mocked(getAuditTrace).mockResolvedValue(traceFor(root));

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('provenance-graph')).toBeInTheDocument();
		});
		// Header rollup reports the gap; a node carries the first-class gap marker.
		expect(screen.getByTestId('header-chain-badge')).toHaveTextContent(/provenance gap/i);
		expect(screen.getAllByTestId('node-provenance-gap').length).toBeGreaterThanOrEqual(1);
	});

	it('surfaces the header unverified count and a per-node warning for an unsigned plan', async () => {
		setLocationSearch('?content_hash=sha256:root');
		const root = materialized({
			hash: 'sha256:root',
			name: 'out.csv',
			skill: 'etl',
			sigStatus: 'unverified'
		});
		vi.mocked(getAuditTrace).mockResolvedValue(traceFor(root));

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('provenance-graph')).toBeInTheDocument();
		});
		expect(screen.getByTestId('header-chain-badge')).toHaveTextContent(/unverified/i);
		expect(screen.getAllByTestId('node-unverified-warning').length).toBeGreaterThanOrEqual(1);
	});
});

describe('Audit page — timeline toggle', () => {
	it('switches to timeline and fetches by invocation id (flat, time order) — not the trace endpoint', async () => {
		setLocationSearch('?content_hash=sha256:root');
		const root = materialized({
			hash: 'sha256:root',
			name: 'final.csv',
			invocation: 'inv-9'
		});
		vi.mocked(getAuditTrace).mockResolvedValue(traceFor(root));
		vi.mocked(getAuditByInvocation).mockResolvedValue([
			materialized({
				hash: 'sha256:root',
				name: 'final.csv',
				invocation: 'inv-9',
				timestamp: '2026-07-04T00:00:02Z'
			}),
			materialized({
				hash: 'sha256:up',
				name: 'raw.json',
				invocation: 'inv-9',
				timestamp: '2026-07-04T00:00:01Z'
			})
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
		vi.mocked(getAuditTrace).mockResolvedValue(traceFor(root));

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
