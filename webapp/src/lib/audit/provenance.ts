// Provenance render model + a pure map from the server-assembled trace.
//
// The `/audit` graph is assembled server-side by `GET /v1/audit/trace`
// (#1913): one round-trip returns the whole node/edge DAG that the
// webapp used to walk client-side. This module keeps the render model
// (`ProvenanceGraph` / `ProvenanceNode` / `ProvenanceEdge`) the graph
// components consume and provides `mapTraceResponse`, a pure snake→camel
// map from the wire shape (`AuditTraceResponse`) into that model.
//
// The wire and render models differ only cosmetically: the wire node
// carries `content_hash` (snake) where the render node carries
// `contentHash` (camel); `root_id` → `rootId`. Everything else — kinds,
// titles, subtitles, depth, embedded `event`, `literal`, `dangling`, and
// edges — passes through unchanged. It is a pure module so it is
// unit-testable against fixtures (repo testing philosophy: test the
// contract, not the render).

import type { AuditEvent, AuditTraceResponse } from '$lib/api';

export type ProvenanceNodeKind = 'artifact' | 'step' | 'literal' | 'plan_input' | 'launch';

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

/** Maps a server-assembled provenance trace into the render model.
 *
 * A pure snake→camel projection: the wire node's `content_hash` becomes
 * `contentHash`, `root_id` becomes `rootId`, and every other field
 * (`kind`, `title`, `subtitle`, `depth`, `event`, `literal`, `dangling`)
 * and every edge pass through unchanged. The daemon already performed the
 * walk-back (steps, literal leaves, upstream artifacts, terminal launch
 * node, dangling/cycle termination), so there is nothing to traverse here.
 */
export function mapTraceResponse(resp: AuditTraceResponse): ProvenanceGraph {
	const nodes: ProvenanceNode[] = resp.nodes.map((n) => ({
		id: n.id,
		kind: n.kind,
		title: n.title,
		subtitle: n.subtitle,
		depth: n.depth,
		event: n.event,
		contentHash: n.content_hash,
		literal: n.literal,
		dangling: n.dangling
	}));
	const edges: ProvenanceEdge[] = resp.edges.map((e) => ({ from: e.from, to: e.to }));
	return { rootId: resp.root_id, nodes, edges };
}
