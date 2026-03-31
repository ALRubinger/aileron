<script lang="ts">
	import { listPolicies } from '$lib/api';
	import { onMount } from 'svelte';

	let policies = $state<any[]>([]);
	let loading = $state(true);
	let error = $state('');

	async function load() {
		try {
			const data = await listPolicies();
			policies = data.items || [];
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	onMount(() => { load(); });

	function effectColor(effect: string) {
		switch (effect) {
			case 'allow': return 'var(--green)';
			case 'deny': return 'var(--red)';
			case 'require_approval': return 'var(--yellow)';
			case 'allow_with_modification': return 'var(--orange)';
			default: return 'var(--text-muted)';
		}
	}
</script>

<svelte:head>
	<title>Policies - Aileron</title>
</svelte:head>

<h1 style="font-size: 1.3rem; margin-bottom: 1rem;">Policies</h1>

{#if loading}
	<p style="color: var(--text-muted);">Loading...</p>
{:else if error}
	<p style="color: var(--red);">{error}</p>
{:else if policies.length === 0}
	<p style="color: var(--text-muted);">No policies configured.</p>
{:else}
	<div style="display: flex; flex-direction: column; gap: 0.75rem;">
		{#each policies as policy}
			<div style="border: 1px solid var(--border); border-radius: 8px; padding: 1rem; background: var(--bg-card);">
				<div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 0.75rem;">
					<div style="font-weight: 600;">{policy.name}</div>
					<span style="font-size: 0.8rem; color: var(--text-muted); text-transform: uppercase;">{policy.status}</span>
				</div>
				{#if policy.description}
					<p style="font-size: 0.85rem; color: var(--text-muted); margin: 0 0 0.75rem;">{policy.description}</p>
				{/if}
				{#each policy.rules as rule}
					<div style="padding: 0.5rem 0.75rem; margin-top: 0.5rem; border: 1px solid var(--border); border-radius: 6px; font-size: 0.85rem;">
						<div style="display: flex; justify-content: space-between; align-items: center;">
							<span style="color: {effectColor(rule.effect)}; font-weight: 600; text-transform: uppercase;">{rule.effect}</span>
							{#if rule.priority != null}
								<span style="color: var(--text-muted); font-size: 0.8rem;">Priority: {rule.priority}</span>
							{/if}
						</div>
						{#if rule.description}
							<div style="color: var(--text-muted); margin-top: 0.25rem;">{rule.description}</div>
						{/if}
						{#if rule.conditions}
							<div style="margin-top: 0.5rem;">
								{#each rule.conditions as cond}
									<div style="font-family: monospace; font-size: 0.8rem; color: var(--text-muted);">
										{cond.field} {cond.operator} {JSON.stringify(cond.value)}
									</div>
								{/each}
							</div>
						{/if}
					</div>
				{/each}
			</div>
		{/each}
	</div>
{/if}
