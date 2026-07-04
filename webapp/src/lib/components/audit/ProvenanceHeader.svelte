<script lang="ts">
	import { Badge } from '$lib/components/ui/badge';
	import type { AuditEvent } from '$lib/api';
	import type { ProvenanceGraph } from '$lib/audit/provenance';
	import * as p from '$lib/audit/payload';
	import { chainTrustSummary } from '$lib/audit/trust';

	// Header for a provenance graph: the actor, the root artifact's
	// verified-signature badge and skill, and a whole-chain trust rollup
	// (chain-verified badge + gap/unsigned count + the de-duplicated signer
	// set) computed across every trust-bearing node in the graph.
	let { root, graph }: { root: AuditEvent; graph: ProvenanceGraph } = $props();

	const skill = $derived(p.planSkill(root));
	const actor = $derived(p.actorIdentityLabel(root));
	const status = $derived(p.planSignatureStatus(root));
	const verified = $derived(p.signatureVerified(root));

	const summary = $derived(chainTrustSummary(graph));
</script>

<div class="mb-4 rounded-lg border border-border bg-card p-4" data-testid="provenance-header">
	<div class="flex flex-wrap items-center gap-3">
		{#if skill}
			<span class="text-lg font-bold" data-testid="header-skill">{skill}</span>
		{/if}
		{#if status}
			<Badge
				variant={verified ? 'default' : 'destructive'}
				data-testid="header-signature-badge"
			>
				{verified ? 'Signature verified' : `Signature ${status}`}
			</Badge>
		{/if}
		<Badge
			variant={summary.fullyVerified ? 'default' : 'destructive'}
			data-testid="header-chain-badge"
		>
			{summary.label}
		</Badge>
	</div>
	<dl class="mt-2 grid grid-cols-1 gap-x-6 gap-y-1 text-xs text-muted-foreground sm:grid-cols-2">
		{#if actor}
			<div class="flex gap-2">
				<dt class="font-medium">Actor</dt>
				<dd data-testid="header-actor">{actor}</dd>
			</div>
		{/if}
		{#if summary.signers.length > 0}
			<div class="flex gap-2">
				<dt class="font-medium">Signed by</dt>
				<dd class="break-all font-mono" data-testid="header-signed-by">
					{summary.signers.join(', ')}
				</dd>
			</div>
		{/if}
	</dl>
</div>
