<script lang="ts">
	import { Badge } from '$lib/components/ui/badge';
	import type { AuditEvent } from '$lib/api';
	import * as p from '$lib/audit/payload';

	// Header for a provenance graph: the actor, a verified-signature badge,
	// the signer fingerprint, and the plan/skill name — all read off the
	// root artifact record.
	let { root }: { root: AuditEvent } = $props();

	const skill = $derived(p.planSkill(root));
	const actor = $derived(p.actorIdentityLabel(root));
	const signedBy = $derived(p.planSignedBy(root));
	const status = $derived(p.planSignatureStatus(root));
	const verified = $derived(status === 'verified');
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
	</div>
	<dl class="mt-2 grid grid-cols-1 gap-x-6 gap-y-1 text-xs text-muted-foreground sm:grid-cols-2">
		{#if actor}
			<div class="flex gap-2">
				<dt class="font-medium">Actor</dt>
				<dd data-testid="header-actor">{actor}</dd>
			</div>
		{/if}
		{#if signedBy}
			<div class="flex gap-2">
				<dt class="font-medium">Signed by</dt>
				<dd class="break-all font-mono" data-testid="header-signed-by">{signedBy}</dd>
			</div>
		{/if}
	</dl>
</div>
