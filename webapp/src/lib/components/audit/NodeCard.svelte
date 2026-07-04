<script lang="ts">
	import * as Card from '$lib/components/ui/card';
	import { Badge } from '$lib/components/ui/badge';
	import type { ProvenanceNode } from '$lib/audit/provenance';

	// One node in the provenance DAG. Cards are one-line summaries only:
	// title + subtitle. Full record fields live in the side panel, opened
	// by clicking the card (progressive disclosure — #1891 key decision 2).
	let { node, onselect }: { node: ProvenanceNode; onselect: (n: ProvenanceNode) => void } =
		$props();

	const kindLabel: Record<ProvenanceNode['kind'], string> = {
		artifact: 'Artifact',
		step: 'Step',
		literal: 'Literal input',
		plan_input: 'Plan input',
		launch: 'Launch'
	};

	const kindVariant: Record<ProvenanceNode['kind'], 'default' | 'secondary' | 'outline'> = {
		artifact: 'default',
		step: 'secondary',
		literal: 'outline',
		plan_input: 'outline',
		launch: 'outline'
	};
</script>

<Card.Root
	data-testid="provenance-node"
	data-node-kind={node.kind}
	data-node-id={node.id}
	class="w-64 cursor-pointer transition-colors hover:border-primary {node.dangling
		? 'border-destructive/60'
		: ''}"
>
	<button
		type="button"
		class="w-full cursor-pointer text-left"
		aria-label="Open details for {node.title}"
		onclick={() => onselect(node)}
	>
		<Card.Header>
			<div class="flex items-center justify-between gap-2">
				<span class="truncate font-semibold" title={node.title}>{node.title}</span>
				<Badge variant={kindVariant[node.kind]}>{kindLabel[node.kind]}</Badge>
			</div>
		</Card.Header>
		{#if node.subtitle}
			<Card.Content>
				<p class="truncate text-xs text-muted-foreground" title={node.subtitle}>
					{node.subtitle}
				</p>
				{#if node.dangling}
					<p class="mt-1 text-xs text-destructive">unresolved upstream</p>
				{/if}
			</Card.Content>
		{/if}
	</button>
</Card.Root>
