import { describe, it, expect } from 'vitest';
import { assembleProvenance, type ResolveByHash } from './provenance';
import type { AuditEvent } from '$lib/api';

// Fixture builder for a materialized-output audit event. Only the flat
// `aileron.*` payload keys the walk-back reads are set.
function output(opts: {
	hash: string;
	name?: string;
	mime?: string;
	bytes?: number;
	stepId?: string;
	kind?: string;
	transform?: string;
	command?: string;
	skill?: string;
	actor?: string;
	inputs?: Array<{ binding: string; source?: string; content_hash?: string; query_execution_id?: string }>;
}): AuditEvent {
	const payload: Record<string, unknown> = {
		'aileron.output.name': opts.name ?? 'artifact',
		'aileron.output.content_hash': opts.hash,
		'aileron.output.mime': opts.mime ?? 'text/csv',
		'aileron.output.bytes': opts.bytes ?? 1024,
		'aileron.step.id': opts.stepId ?? 'step-x',
		'aileron.step.kind': opts.kind ?? 'action_call'
	};
	if (opts.transform) payload['aileron.step.transform'] = opts.transform;
	if (opts.command) payload['aileron.step.command'] = opts.command;
	if (opts.skill) payload['aileron.plan.skill'] = opts.skill;
	if (opts.actor) payload['aileron.actor.identity_label'] = opts.actor;
	if (opts.inputs) payload['aileron.step.inputs'] = opts.inputs;
	return {
		audit_id: `evt-${opts.hash}`,
		event_type: 'output.materialized',
		timestamp: '2026-07-04T00:00:00Z',
		payload
	};
}

function mapResolver(events: AuditEvent[]): ResolveByHash {
	const byHash = new Map<string, AuditEvent>();
	for (const e of events) {
		const h = e.payload['aileron.output.content_hash'];
		if (typeof h === 'string') byHash.set(h, e);
	}
	return async (hash) => byHash.get(hash) ?? null;
}

const kinds = (g: { nodes: Array<{ kind: string }> }) => g.nodes.map((n) => n.kind).sort();

describe('assembleProvenance — linear chain', () => {
	it('walks artifact → step → upstream artifact → step → launch', async () => {
		const upstream = output({ hash: 'sha256:up', name: 'raw.json', skill: 'ingest' });
		const root = output({
			hash: 'sha256:root',
			name: 'report.csv',
			skill: 'ingest',
			actor: 'analyst',
			inputs: [{ binding: 'data', source: 'steps.q1.rows', content_hash: 'sha256:up' }]
		});

		const g = await assembleProvenance(root, mapResolver([upstream, root]));

		expect(g.rootId).toBe('artifact:sha256:root');
		// two artifacts, two steps, one launch
		const artifactNodes = g.nodes.filter((n) => n.kind === 'artifact');
		expect(artifactNodes.map((n) => n.contentHash)).toEqual(
			expect.arrayContaining(['sha256:root', 'sha256:up'])
		);
		expect(g.nodes.filter((n) => n.kind === 'step')).toHaveLength(2);
		expect(g.nodes.filter((n) => n.kind === 'launch')).toHaveLength(1);

		// The root artifact's step is wired to the upstream artifact.
		expect(g.edges).toContainEqual({ from: 'artifact:sha256:root', to: 'step:artifact:sha256:root' });
		expect(g.edges).toContainEqual({ from: 'step:artifact:sha256:root', to: 'artifact:sha256:up' });
		// terminal launch node hangs off the root step
		expect(g.edges).toContainEqual({ from: 'step:artifact:sha256:root', to: 'launch' });
		expect(g.nodes.find((n) => n.kind === 'launch')?.title).toContain('ingest');
	});
});

describe('assembleProvenance — branching', () => {
	it('walks both hashed upstreams of a two-input step', async () => {
		const a = output({ hash: 'sha256:a', name: 'a.json' });
		const b = output({ hash: 'sha256:b', name: 'b.json' });
		const root = output({
			hash: 'sha256:merged',
			name: 'merged.csv',
			kind: 'transform',
			transform: 'join',
			inputs: [
				{ binding: 'left', content_hash: 'sha256:a' },
				{ binding: 'right', content_hash: 'sha256:b' }
			]
		});

		const g = await assembleProvenance(root, mapResolver([a, b, root]));

		const hashes = g.nodes.filter((n) => n.kind === 'artifact').map((n) => n.contentHash);
		expect(hashes).toEqual(expect.arrayContaining(['sha256:merged', 'sha256:a', 'sha256:b']));
		expect(g.edges).toContainEqual({ from: 'step:artifact:sha256:merged', to: 'artifact:sha256:a' });
		expect(g.edges).toContainEqual({ from: 'step:artifact:sha256:merged', to: 'artifact:sha256:b' });
	});
});

describe('assembleProvenance — literal input', () => {
	it('emits a literal node and does not recurse', async () => {
		const root = output({
			hash: 'sha256:root',
			inputs: [{ binding: 'threshold', source: 'inputs.threshold' }]
		});

		const g = await assembleProvenance(root, mapResolver([root]));

		const literals = g.nodes.filter((n) => n.kind === 'literal');
		expect(literals).toHaveLength(1);
		expect(literals[0].literal).toEqual({ binding: 'threshold', source: 'inputs.threshold' });
		// only the one root artifact — no upstream artifact was fetched
		expect(g.nodes.filter((n) => n.kind === 'artifact')).toHaveLength(1);
	});
});

describe('assembleProvenance — dangling upstream', () => {
	it('marks an unresolvable hash and does not throw', async () => {
		const root = output({
			hash: 'sha256:root',
			inputs: [{ binding: 'data', content_hash: 'sha256:missing' }]
		});
		// resolver knows only the root, returns null for the upstream
		const g = await assembleProvenance(root, mapResolver([root]));

		const dangling = g.nodes.find((n) => n.dangling);
		expect(dangling).toBeTruthy();
		expect(dangling?.contentHash).toBe('sha256:missing');
		expect(g.edges).toContainEqual({
			from: 'step:artifact:sha256:root',
			to: 'artifact:sha256:missing'
		});
	});
});

describe('assembleProvenance — cycle safety', () => {
	it('terminates when an upstream points back to an already-seen artifact', async () => {
		// root -> back references root's own hash as an input (degenerate cycle)
		const root = output({
			hash: 'sha256:cyc',
			inputs: [{ binding: 'self', content_hash: 'sha256:cyc' }]
		});

		const g = await assembleProvenance(root, mapResolver([root]));

		// Exactly one artifact node for the cyclic hash — never expanded twice.
		expect(g.nodes.filter((n) => n.contentHash === 'sha256:cyc')).toHaveLength(1);
		// The self-edge is still recorded so the cycle is visible.
		expect(g.edges).toContainEqual({
			from: 'step:artifact:sha256:cyc',
			to: 'artifact:sha256:cyc'
		});
		expect(kinds(g)).toEqual(['artifact', 'launch', 'step']);
	});
});
