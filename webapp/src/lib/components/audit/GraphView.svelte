<script lang="ts">
	import NodeCard from './NodeCard.svelte';
	import type { ProvenanceGraph, ProvenanceNode } from '$lib/audit/provenance';

	// Hand-rolled top-down DAG (no d3/elkjs/cytoscape — #1891 key decision
	// 3). Provenance DAGs are tiny, so we bucket nodes by depth into
	// stacked rows and render CSS connector rails between rows. The root
	// artifact sits at depth 0; upstreams and the terminal launch node
	// stack below it.
	let {
		graph,
		onselect
	}: { graph: ProvenanceGraph; onselect: (n: ProvenanceNode) => void } = $props();

	// Group nodes into depth rows, sorted shallow → deep.
	const rows = $derived.by(() => {
		const byDepth = new Map<number, ProvenanceNode[]>();
		for (const n of graph.nodes) {
			const bucket = byDepth.get(n.depth) ?? [];
			bucket.push(n);
			byDepth.set(n.depth, bucket);
		}
		return [...byDepth.entries()]
			.sort((a, b) => a[0] - b[0])
			.map(([depth, nodes]) => ({ depth, nodes }));
	});
</script>

<div class="flex flex-col items-center gap-0" data-testid="provenance-graph">
	{#each rows as row, i (row.depth)}
		<div class="flex flex-wrap justify-center gap-4">
			{#each row.nodes as node (node.id)}
				<NodeCard {node} {onselect} />
			{/each}
		</div>
		{#if i < rows.length - 1}
			<!-- Connector rail between depth rows. The small-N case makes a
			     single vertical rail legible without per-edge routing. -->
			<div class="h-6 w-px bg-border" aria-hidden="true"></div>
		{/if}
	{/each}
</div>
