<script lang="ts">
	import * as Card from '$lib/components/ui/card';
	import { Badge } from '$lib/components/ui/badge';
	import type { AuditEvent } from '$lib/api';
	import * as p from '$lib/audit/payload';

	// Timeline (secondary) view: the launch's records in explicit time
	// order. Ordering is oldest-first so a reader follows the launch as it
	// unfolded (the daemon returns newest-first, so we reverse).
	let { events }: { events: AuditEvent[] } = $props();

	const ordered = $derived(
		[...events].sort((a, b) => a.timestamp.localeCompare(b.timestamp))
	);
</script>

<div class="flex flex-col gap-3" data-testid="provenance-timeline">
	{#if ordered.length === 0}
		<p class="text-sm text-muted-foreground">No records for this launch.</p>
	{:else}
		{#each ordered as event (event.audit_id)}
			<Card.Root data-testid="timeline-row">
				<Card.Header>
					<div class="flex items-center justify-between gap-2">
						<span class="font-mono text-xs text-muted-foreground">{event.timestamp}</span>
						<Badge variant="outline">{event.event_type}</Badge>
					</div>
				</Card.Header>
				<Card.Content>
					<p class="text-sm">
						{p.outputName(event) ?? p.stepId(event) ?? event.audit_id}
					</p>
					{#if p.actorIdentityLabel(event)}
						<p class="text-xs text-muted-foreground">by {p.actorIdentityLabel(event)}</p>
					{/if}
				</Card.Content>
			</Card.Root>
		{/each}
	{/if}
</div>
