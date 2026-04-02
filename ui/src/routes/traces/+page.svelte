<script lang="ts">
	import { listTraces } from '$lib/api';
	import { onMount } from 'svelte';
	import { eventColor } from '$lib/colors';
	import * as Card from '$lib/components/ui/card';

	let traces = $state<any[]>([]);
	let loading = $state(true);
	let error = $state('');
	let expandedTrace = $state<string | null>(null);

	async function load() {
		try {
			const data = await listTraces();
			traces = data.items || [];
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		load();
		const interval = setInterval(load, 5000);
		return () => clearInterval(interval);
	});
</script>

<svelte:head>
	<title>Traces - Aileron</title>
</svelte:head>

<h1 class="text-xl font-semibold mb-4">Audit Traces</h1>

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else if error}
	<p class="text-destructive">{error}</p>
{:else if traces.length === 0}
	<p class="text-muted-foreground">No traces yet.</p>
{:else}
	<div class="flex flex-col gap-3">
		{#each traces as trace}
			<Card.Root class="overflow-hidden">
				<button
					onclick={() => expandedTrace = expandedTrace === trace.trace_id ? null : trace.trace_id}
					class="w-full px-4 py-3 bg-transparent border-none text-foreground text-left cursor-pointer flex justify-between items-center"
				>
					<div>
						<span class="font-semibold font-mono text-sm">{trace.intent_id}</span>
						<span class="text-muted-foreground ml-2 text-sm">{trace.events?.length || 0} events</span>
					</div>
					<span class="text-muted-foreground">{expandedTrace === trace.trace_id ? '\u2212' : '+'}</span>
				</button>

				{#if expandedTrace === trace.trace_id && trace.events}
					<div class="border-t border-border px-4 py-3">
						{#each trace.events as event, i}
							<div class="flex gap-4 items-start py-2 {i > 0 ? 'border-t border-border' : ''}">
								<div class="w-1.5 h-1.5 rounded-full mt-[0.45rem] shrink-0" style="background: {eventColor(event.event_type)}"></div>
								<div class="flex-1 min-w-0">
									<div class="flex justify-between items-baseline">
										<span class="font-semibold text-sm" style="color: {eventColor(event.event_type)}">{event.event_type}</span>
										<span class="text-xs text-muted-foreground">{new Date(event.timestamp).toLocaleTimeString()}</span>
									</div>
									<div class="text-xs text-muted-foreground">
										{event.actor.type}: {event.actor.id}
									</div>
									{#if event.payload}
										<pre class="text-xs text-muted-foreground mt-1 whitespace-pre-wrap break-all">{JSON.stringify(event.payload, null, 2)}</pre>
									{/if}
								</div>
							</div>
						{/each}
					</div>
				{/if}
			</Card.Root>
		{/each}
	</div>
{/if}
