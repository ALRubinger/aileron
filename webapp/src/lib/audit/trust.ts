// Chain-of-custody trust classification over a provenance graph.
//
// The `/audit` graph already carries every trust field per node (each
// trust-bearing node embeds an `AuditEvent` whose payload holds the plan
// skill, signer fingerprint, signature status, and acting identity — see
// payload.ts). This module is the pure, fixture-tested layer that turns those
// per-node fields into the two things the render surface needs:
//
//   - `nodeTrust(node)` — a per-node classification the card chrome keys off.
//   - `chainTrustSummary(graph)` — the whole-graph rollup the header renders
//     ("2 of 5 steps unverified" / "Chain verified", the de-duplicated signer
//     set, and a single `fullyVerified` flag).
//
// It imports nothing from Svelte so the counting logic is unit-testable
// against graph fixtures without rendering (repo testing philosophy: test the
// contract, not the render). "Verified" is not redefined here; it routes
// through `signatureVerified` in payload.ts, which mirrors the daemon.

import type { ProvenanceGraph, ProvenanceNode } from './provenance';
import { signatureVerified, planSignedBy } from './payload';

/** Per-node chain-of-custody classification.
 *
 *  - `verified`   — a trust-bearing node (`step`/`launch`) whose backing
 *                   event's plan signature verified.
 *  - `unverified` — a trust-bearing node with a backing event whose plan
 *                   signature did not verify (unsigned, failed, or unknown).
 *  - `gap`        — a node whose upstream producer could not be resolved
 *                   (a `dangling` provenance gap). Takes precedence: a gap is
 *                   a hole in the chain regardless of kind.
 *  - `none`       — a node that carries no trust chrome (`literal` inputs and
 *                   artifact nodes that are neither dangling nor trust-bearing).
 */
export type NodeTrust = 'verified' | 'unverified' | 'gap' | 'none';

/** Kinds whose embedded event carries plan/actor provenance. Artifact nodes
 *  surface a gap marker when dangling but otherwise carry no signature chrome;
 *  literal nodes never do. */
const TRUST_BEARING_KINDS: ReadonlySet<ProvenanceNode['kind']> = new Set(['step', 'launch']);

export function isTrustBearing(node: ProvenanceNode): boolean {
	return TRUST_BEARING_KINDS.has(node.kind) && node.event !== undefined;
}

export function nodeTrust(node: ProvenanceNode): NodeTrust {
	// A dangling node is a provenance gap first, whatever its kind.
	if (node.dangling) return 'gap';
	if (isTrustBearing(node) && node.event) {
		return signatureVerified(node.event) ? 'verified' : 'unverified';
	}
	return 'none';
}

/** Whole-graph chain-of-custody rollup for the header.
 *
 *  - `signers`           — de-duplicated, non-empty signer fingerprints across
 *                          trust-bearing nodes, in first-seen order.
 *  - `trustBearingCount` — number of `step`/`launch` nodes with a backing event.
 *  - `unverifiedCount`   — trust-bearing nodes whose signature did not verify.
 *  - `gapCount`          — dangling nodes (unresolved upstream producers).
 *  - `fullyVerified`     — at least one trust-bearing node and no unverified
 *                          nodes and no gaps.
 *  - `label`             — human summary: "Chain verified" when clean,
 *                          otherwise the gap/unverified breakdown, e.g.
 *                          "2 of 5 steps unverified" or
 *                          "1 of 3 steps unverified · 1 provenance gap".
 */
export type ChainTrustSummary = {
	signers: string[];
	fullyVerified: boolean;
	trustBearingCount: number;
	unverifiedCount: number;
	gapCount: number;
	label: string;
};

export function chainTrustSummary(graph: ProvenanceGraph): ChainTrustSummary {
	const signers: string[] = [];
	const seenSigners = new Set<string>();
	let trustBearingCount = 0;
	let unverifiedCount = 0;
	let gapCount = 0;

	for (const node of graph.nodes) {
		if (node.dangling) gapCount++;
		if (!isTrustBearing(node) || !node.event) continue;
		trustBearingCount++;
		if (!signatureVerified(node.event)) unverifiedCount++;
		const signer = planSignedBy(node.event);
		if (signer && !seenSigners.has(signer)) {
			seenSigners.add(signer);
			signers.push(signer);
		}
	}

	const fullyVerified = trustBearingCount > 0 && unverifiedCount === 0 && gapCount === 0;

	return {
		signers,
		fullyVerified,
		trustBearingCount,
		unverifiedCount,
		gapCount,
		label: summaryLabel({ trustBearingCount, unverifiedCount, gapCount, fullyVerified })
	};
}

function summaryLabel(s: {
	trustBearingCount: number;
	unverifiedCount: number;
	gapCount: number;
	fullyVerified: boolean;
}): string {
	if (s.fullyVerified) return 'Chain verified';
	const parts: string[] = [];
	if (s.unverifiedCount > 0) {
		parts.push(`${s.unverifiedCount} of ${s.trustBearingCount} steps unverified`);
	}
	if (s.gapCount > 0) {
		parts.push(`${s.gapCount} provenance ${s.gapCount === 1 ? 'gap' : 'gaps'}`);
	}
	// No trust-bearing nodes at all and no gaps: nothing to attest.
	if (parts.length === 0) return 'No signed steps recorded';
	return parts.join(' · ');
}
