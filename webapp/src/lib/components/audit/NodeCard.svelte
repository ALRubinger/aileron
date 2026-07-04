<script lang="ts">
	import * as Card from '$lib/components/ui/card';
	import { Badge } from '$lib/components/ui/badge';
	import type { ProvenanceNode } from '$lib/audit/provenance';
	import * as p from '$lib/audit/payload';

	// One node in the provenance DAG. Cards are one-line summaries plus a
	// terse chain-of-custody strip (signature badge, signer, acting identity)
	// for trust-bearing nodes; full record fields live in the side panel,
	// opened by clicking the card (progressive disclosure — #1891 key
	// decision 2). Dangling nodes surface a first-class "provenance gap"
	// affordance distinct from an unverified-signature warning.
	let { node, onselect }: { node: ProvenanceNode; onselect: (n: ProvenanceNode) => void } =
		$props();

	const kindLabel: Record<ProvenanceNode['kind'], string> = {
		artifact: 'Artifact',
		step: 'Step',
		literal: 'Literal input',
		launch: 'Launch'
	};

	const kindVariant: Record<ProvenanceNode['kind'], 'default' | 'secondary' | 'outline'> = {
		artifact: 'default',
		step: 'secondary',
		literal: 'outline',
		launch: 'outline'
	};

	// Trust chrome renders only on the trust-bearing nodes (step/launch),
	// whose embedded event carries plan/actor provenance.
	const trustBearing = $derived(
		node.event !== undefined && (node.kind === 'step' || node.kind === 'launch')
	);
	const verified = $derived(node.event ? p.signatureVerified(node.event) : false);
	const sigStatus = $derived(node.event ? p.planSignatureStatus(node.event) : undefined);
	const signer = $derived(node.event ? p.planSignedBy(node.event) : undefined);
	const identity = $derived(node.event ? p.actorIdentityLabel(node.event) : undefined);
	const consent = $derived(node.event ? p.consentDecision(node.event) : undefined);
</script>

<Card.Root
	data-testid="provenance-node"
	data-node-kind={node.kind}
	data-node-id={node.id}
	data-node-trust={node.dangling ? 'gap' : trustBearing ? (verified ? 'verified' : 'unverified') : 'none'}
	class="w-64 cursor-pointer transition-colors hover:border-primary {node.dangling
		? 'border-destructive/60'
		: trustBearing && !verified
			? 'border-destructive/40'
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
		<Card.Content>
			{#if node.subtitle}
				<p class="truncate text-xs text-muted-foreground" title={node.subtitle}>
					{node.subtitle}
				</p>
			{/if}

			{#if node.dangling}
				<p class="mt-1 text-xs font-medium text-destructive" data-testid="node-provenance-gap">
					Provenance gap — unresolved upstream
				</p>
			{:else if trustBearing}
				<div class="mt-2 flex flex-col gap-1">
					{#if verified}
						<Badge variant="default" data-testid="node-signature-badge">Signature verified</Badge>
					{:else}
						<Badge variant="destructive" data-testid="node-unverified-warning">
							{sigStatus ? `Signature ${sigStatus}` : 'Unsigned'}
						</Badge>
					{/if}
					{#if signer}
						<p
							class="truncate font-mono text-xs text-muted-foreground"
							title={signer}
							data-testid="node-signer"
						>
							{p.shortHash(signer)}
						</p>
					{/if}
					{#if identity}
						<p class="truncate text-xs text-muted-foreground" title={identity} data-testid="node-identity">
							{identity}{#if consent}<span class="text-muted-foreground/70"> · consent {consent}</span>{/if}
						</p>
					{/if}
				</div>
			{/if}
		</Card.Content>
	</button>
</Card.Root>
