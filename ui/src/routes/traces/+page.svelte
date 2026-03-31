<script lang="ts">
	import { listTraces } from '$lib/api';
	import { onMount } from 'svelte';

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

	function eventColor(eventType: string) {
		if (eventType.includes('submitted')) return 'var(--accent)';
		if (eventType.includes('approved')) return 'var(--green)';
		if (eventType.includes('denied')) return 'var(--red)';
		if (eventType.includes('succeeded')) return 'var(--green)';
		if (eventType.includes('failed')) return 'var(--red)';
		if (eventType.includes('granted') || eventType.includes('issued')) return 'var(--green)';
		return 'var(--text-muted)';
	}
</script>

<svelte:head>
	<title>Traces - Aileron</title>
</svelte:head>

<h1 style="font-size: 1.3rem; margin-bottom: 1rem;">Audit Traces</h1>

{#if loading}
	<p style="color: var(--text-muted);">Loading...</p>
{:else if error}
	<p style="color: var(--red);">{error}</p>
{:else if traces.length === 0}
	<p style="color: var(--text-muted);">No traces yet.</p>
{:else}
	<div style="display: flex; flex-direction: column; gap: 0.75rem;">
		{#each traces as trace}
			<div style="border: 1px solid var(--border); border-radius: 8px; background: var(--bg-card); overflow: hidden;">
				<button
					onclick={() => expandedTrace = expandedTrace === trace.trace_id ? null : trace.trace_id}
					style="width: 100%; padding: 1rem; background: none; border: none; color: var(--text); text-align: left; cursor: pointer; display: flex; justify-content: space-between; align-items: center;"
				>
					<div>
						<span style="font-weight: 600; font-family: monospace; font-size: 0.85rem;">{trace.intent_id}</span>
						<span style="color: var(--text-muted); margin-left: 0.5rem; font-size: 0.85rem;">{trace.events?.length || 0} events</span>
					</div>
					<span style="color: var(--text-muted);">{expandedTrace === trace.trace_id ? '−' : '+'}</span>
				</button>

				{#if expandedTrace === trace.trace_id && trace.events}
					<div style="border-top: 1px solid var(--border); padding: 1rem;">
						{#each trace.events as event, i}
							<div style="display: flex; gap: 1rem; align-items: flex-start; padding: 0.5rem 0; {i > 0 ? 'border-top: 1px solid var(--border);' : ''}">
								<div style="width: 6px; height: 6px; border-radius: 50%; background: {eventColor(event.event_type)}; margin-top: 0.45rem; flex-shrink: 0;"></div>
								<div style="flex: 1; min-width: 0;">
									<div style="display: flex; justify-content: space-between; align-items: baseline;">
										<span style="font-weight: 600; font-size: 0.85rem; color: {eventColor(event.event_type)};">{event.event_type}</span>
										<span style="font-size: 0.75rem; color: var(--text-muted);">{new Date(event.timestamp).toLocaleTimeString()}</span>
									</div>
									<div style="font-size: 0.8rem; color: var(--text-muted);">
										{event.actor.type}: {event.actor.id}
									</div>
									{#if event.payload}
										<pre style="font-size: 0.75rem; color: var(--text-muted); margin: 0.25rem 0 0; white-space: pre-wrap; word-break: break-all;">{JSON.stringify(event.payload, null, 2)}</pre>
									{/if}
								</div>
							</div>
						{/each}
					</div>
				{/if}
			</div>
		{/each}
	</div>
{/if}
