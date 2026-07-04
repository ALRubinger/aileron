// Provenance graph assembly — a pure, DOM-free walk-back that turns a
// starting materialized-artifact record into a node/edge graph.
//
// The walk starts at an `output.materialized` event and follows its
// `aileron.step.inputs[]` upstream: for each input carrying a
// `content_hash`, it resolves the record that produced that hash and
// recurses. It terminates at a literal input (no `content_hash`), at a
// dangling hash (resolver returns null), and at the top with a launch /
// actor node derived from the artifact's `aileron.plan.*` / `aileron.actor.*`.
//
// Provenance DAGs are tiny (a handful of nodes), so this is deliberately
// simple. It is a pure module so it is unit-testable against fixtures
// (repo testing philosophy: test the contract, not the render).

import type { AuditEvent } from '$lib/api';
import * as p from './payload';

export type ProvenanceNodeKind = 'artifact' | 'step' | 'literal' | 'launch';

export type ProvenanceNode = {
	/** Stable node id, unique within the graph. */
	id: string;
	kind: ProvenanceNodeKind;
	/** One-line summary fields the cards render. */
	title: string;
	subtitle?: string;
	/** Distance from the root artifact (0 = root). Assigned during assembly. */
	depth: number;
	/** The backing audit event, when the node was derived from one. A
	 *  literal input has no backing record. */
	event?: AuditEvent;
	/** For an artifact node, the content hash it materialized. */
	contentHash?: string;
	/** For a literal node, the input binding/source it stands for. */
	literal?: { binding: string; source?: string };
	/** True when this node's content hash could not be resolved to a
	 *  producing record (dangling upstream). */
	dangling?: boolean;
};

export type ProvenanceEdge = {
	/** Parent (downstream/consumer) node id. */
	from: string;
	/** Child (upstream/producer) node id. */
	to: string;
};

export type ProvenanceGraph = {
	rootId: string;
	nodes: ProvenanceNode[];
	edges: ProvenanceEdge[];
};

/** Async resolver: given a content hash, return the materialized-output
 *  event that produced it, or null when unknown. Production passes a
 *  wrapper over `getAuditByContentHash`; tests pass a fixture map. */
export type ResolveByHash = (hash: string) => Promise<AuditEvent | null>;

const MAX_DEPTH = 32;

function artifactTitle(e: AuditEvent): string {
	return p.outputName(e) ?? '(unnamed artifact)';
}

function artifactSubtitle(e: AuditEvent): string {
	const mime = p.outputMime(e);
	const size = p.formatBytes(p.outputBytes(e));
	const hash = p.shortHash(p.outputContentHash(e));
	return [mime, size, hash].filter((s) => s && s.length > 0).join(' · ');
}

function stepSubtitle(e: AuditEvent): string {
	const kind = p.stepKind(e);
	const detail = p.stepTransform(e) ?? p.stepCommand(e);
	return [kind, detail].filter((s) => s && s.length > 0).join(' · ');
}

/** Builds the terminal launch/actor node from the root artifact's
 *  plan/actor provenance. */
function launchNode(root: AuditEvent, depth: number): ProvenanceNode {
	const skill = p.planSkill(root);
	const actor = p.actorIdentityLabel(root);
	const subtitleParts = [
		skill ? `skill ${skill}` : undefined,
		actor ? `by ${actor}` : undefined
	].filter((s): s is string => !!s);
	return {
		id: 'launch',
		kind: 'launch',
		title: skill ? `Launch: ${skill}` : 'Launch',
		subtitle: subtitleParts.join(' · ') || undefined,
		depth,
		event: root
	};
}

/**
 * Assembles the provenance graph for a starting materialized artifact.
 *
 * @param start   the `output.materialized` event to walk back from.
 * @param resolve async resolver from content hash to producing event.
 */
export async function assembleProvenance(
	start: AuditEvent,
	resolve: ResolveByHash
): Promise<ProvenanceGraph> {
	const nodes: ProvenanceNode[] = [];
	const edges: ProvenanceEdge[] = [];
	// Guard against cycles: an artifact is keyed by its content hash, so a
	// hash we have already expanded is never expanded twice.
	const seenHashes = new Set<string>();
	let literalSeq = 0;

	// walk expands one artifact event: emits its step node, its input
	// nodes/edges, and recurses into hashed upstreams. Returns the id of
	// the artifact node it created so callers can wire an edge to it.
	async function walk(event: AuditEvent, depth: number): Promise<string> {
		const hash = p.outputContentHash(event);
		const artifactId = hash ? `artifact:${hash}` : `artifact:node${nodes.length}`;

		nodes.push({
			id: artifactId,
			kind: 'artifact',
			title: artifactTitle(event),
			subtitle: artifactSubtitle(event),
			depth,
			event,
			contentHash: hash
		});

		if (hash) seenHashes.add(hash);

		// The materializing step node hangs directly off the artifact.
		const sid = p.stepId(event);
		const stepNodeId = `step:${artifactId}`;
		nodes.push({
			id: stepNodeId,
			kind: 'step',
			title: sid ? `Step ${sid}` : 'Step',
			subtitle: stepSubtitle(event) || undefined,
			depth: depth + 1,
			event
		});
		edges.push({ from: artifactId, to: stepNodeId });

		if (depth + 2 > MAX_DEPTH) {
			return artifactId;
		}

		const inputs = p.stepInputs(event);
		for (const input of inputs) {
			if (!input.content_hash) {
				// Literal input: a terminal leaf, never recursed.
				const litId = `literal:${literalSeq++}`;
				nodes.push({
					id: litId,
					kind: 'literal',
					title: input.binding || '(literal)',
					subtitle: input.source,
					depth: depth + 2,
					literal: { binding: input.binding, source: input.source }
				});
				edges.push({ from: stepNodeId, to: litId });
				continue;
			}

			if (seenHashes.has(input.content_hash)) {
				// Already expanded upstream (or in-flight): wire the edge to
				// the existing artifact node and do not recurse (cycle guard).
				edges.push({ from: stepNodeId, to: `artifact:${input.content_hash}` });
				continue;
			}
			// Reserve the hash before awaiting so a concurrent/cyclic path
			// cannot re-enter it.
			seenHashes.add(input.content_hash);

			const upstream = await resolve(input.content_hash);
			if (!upstream) {
				// Dangling upstream: mark it and stop, do not throw.
				const danglingId = `artifact:${input.content_hash}`;
				nodes.push({
					id: danglingId,
					kind: 'artifact',
					title: '(unresolved artifact)',
					subtitle: p.shortHash(input.content_hash),
					depth: depth + 2,
					contentHash: input.content_hash,
					dangling: true
				});
				edges.push({ from: stepNodeId, to: danglingId });
				continue;
			}

			const upstreamId = await walk(upstream, depth + 2);
			edges.push({ from: stepNodeId, to: upstreamId });
		}

		return artifactId;
	}

	const rootId = await walk(start, 0);

	// Terminal launch/actor node, attached below the root artifact's step.
	const maxDepth = nodes.reduce((m, n) => Math.max(m, n.depth), 0);
	const launch = launchNode(start, maxDepth + 1);
	nodes.push(launch);
	edges.push({ from: `step:${rootId}`, to: launch.id });

	return { rootId, nodes, edges };
}
