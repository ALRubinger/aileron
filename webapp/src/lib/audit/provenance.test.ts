import { describe, it, expect } from 'vitest';
import { mapTraceResponse } from './provenance';
import type { AuditEvent, AuditTraceNode, AuditTraceResponse } from '$lib/api';

// The provenance render model is now assembled server-side and mapped
// here (#1930). These tests pin the pure snake→camel projection:
// `content_hash` → `contentHash`, `root_id` → `rootId`, with every other
// field (kind, title, subtitle, depth, embedded event, literal, dangling)
// and every edge passing through verbatim. This is the shape `GraphView`
// consumes, so the mapping is the whole contract.

function eventFor(hash: string): AuditEvent {
	return {
		audit_id: `evt-${hash}`,
		event_type: 'output.materialized',
		timestamp: '2026-07-04T00:00:00Z',
		payload: { 'aileron.output.content_hash': hash }
	};
}

function traceResponse(overrides?: Partial<AuditTraceResponse>): AuditTraceResponse {
	const artifactEvent = eventFor('sha256:root');
	const nodes: AuditTraceNode[] = [
		{
			id: 'artifact:sha256:root',
			kind: 'artifact',
			title: 'report.csv',
			subtitle: 'text/csv · 1 KB',
			depth: 0,
			content_hash: 'sha256:root',
			event: artifactEvent
		},
		{
			id: 'step:artifact:sha256:root',
			kind: 'step',
			title: 'Step step-1',
			depth: 1,
			event: artifactEvent
		},
		{
			id: 'literal:0',
			kind: 'literal',
			title: 'threshold',
			subtitle: 'inputs.threshold',
			depth: 2,
			literal: { binding: 'threshold', source: 'inputs.threshold' }
		},
		{
			id: 'artifact:sha256:missing',
			kind: 'artifact',
			title: '(unresolved artifact)',
			subtitle: 'sha256:missing',
			depth: 2,
			content_hash: 'sha256:missing',
			dangling: true
		},
		{
			id: 'launch',
			kind: 'launch',
			title: 'Launch: ingest',
			depth: 3,
			event: artifactEvent
		}
	];
	return {
		root_id: 'artifact:sha256:root',
		nodes,
		edges: [
			{ from: 'artifact:sha256:root', to: 'step:artifact:sha256:root' },
			{ from: 'step:artifact:sha256:root', to: 'literal:0' },
			{ from: 'step:artifact:sha256:root', to: 'artifact:sha256:missing' },
			{ from: 'step:artifact:sha256:root', to: 'launch' }
		],
		...overrides
	};
}

describe('mapTraceResponse — snake→camel projection', () => {
	it('maps root_id to rootId', () => {
		const g = mapTraceResponse(traceResponse());
		expect(g.rootId).toBe('artifact:sha256:root');
		// The mapped rootId resolves to a node in the graph.
		expect(g.nodes.find((n) => n.id === g.rootId)).toBeTruthy();
	});

	it('maps an artifact node content_hash to contentHash and drops the snake key', () => {
		const g = mapTraceResponse(traceResponse());
		const root = g.nodes.find((n) => n.id === 'artifact:sha256:root')!;
		expect(root.contentHash).toBe('sha256:root');
		expect((root as Record<string, unknown>).content_hash).toBeUndefined();
	});

	it('carries the embedded event through unchanged', () => {
		const g = mapTraceResponse(traceResponse());
		const root = g.nodes.find((n) => n.id === 'artifact:sha256:root')!;
		expect(root.event?.audit_id).toBe('evt-sha256:root');
		expect(root.event?.payload['aileron.output.content_hash']).toBe('sha256:root');
	});

	it('preserves a literal node binding/source', () => {
		const g = mapTraceResponse(traceResponse());
		const lit = g.nodes.find((n) => n.kind === 'literal')!;
		expect(lit.literal).toEqual({ binding: 'threshold', source: 'inputs.threshold' });
	});

	it('preserves the dangling flag on an unresolved upstream', () => {
		const g = mapTraceResponse(traceResponse());
		const dangling = g.nodes.find((n) => n.dangling);
		expect(dangling?.contentHash).toBe('sha256:missing');
		expect(dangling?.kind).toBe('artifact');
	});

	it('maps a plan_input node through non-red, preserving hash and literal', () => {
		// #1927: a plan-provided root input (source `inputs.*`) carries a
		// content hash but has no producing record by design. The server
		// emits it as a terminal `plan_input` leaf (not the red dangling
		// artifact); the pure map must carry that kind through unchanged and
		// never set `dangling`.
		const resp = traceResponse({
			nodes: [
				...traceResponse().nodes,
				{
					id: 'plan_input:0',
					kind: 'plan_input',
					title: 'region',
					subtitle: 'sha256:reg',
					depth: 2,
					content_hash: 'sha256:region',
					literal: { binding: 'region', source: 'inputs.region' }
				}
			]
		});
		const g = mapTraceResponse(resp);
		const pi = g.nodes.find((n) => n.kind === 'plan_input')!;
		expect(pi.contentHash).toBe('sha256:region');
		expect(pi.literal).toEqual({ binding: 'region', source: 'inputs.region' });
		expect(pi.dangling).toBeFalsy();
	});

	it('passes title, subtitle, depth, and kind through verbatim', () => {
		const g = mapTraceResponse(traceResponse());
		const step = g.nodes.find((n) => n.kind === 'step')!;
		expect(step.title).toBe('Step step-1');
		expect(step.depth).toBe(1);
		const launch = g.nodes.find((n) => n.kind === 'launch')!;
		expect(launch.title).toBe('Launch: ingest');
	});

	it('preserves edges verbatim', () => {
		const resp = traceResponse();
		const g = mapTraceResponse(resp);
		expect(g.edges).toEqual(resp.edges);
	});

	it('produces the kinds GraphView renders (artifact, step, literal, launch)', () => {
		const g = mapTraceResponse(traceResponse());
		const kinds = new Set(g.nodes.map((n) => n.kind));
		expect(kinds).toEqual(new Set(['artifact', 'step', 'literal', 'launch']));
	});

	it('maps an empty graph (no nodes/edges) without throwing', () => {
		const g = mapTraceResponse({ root_id: '', nodes: [], edges: [] });
		expect(g).toEqual({ rootId: '', nodes: [], edges: [] });
	});
});
