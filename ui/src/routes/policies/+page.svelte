<script lang="ts">
	import { listPolicies } from '$lib/api';
	import { onMount } from 'svelte';
	import { effectColor } from '$lib/colors';
	import * as Card from '$lib/components/ui/card';
	import { Badge } from '$lib/components/ui/badge';

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
</script>

<svelte:head>
	<title>Policies - Aileron</title>
</svelte:head>

<h1 class="text-xl font-semibold mb-4">Policies</h1>

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else if error}
	<p class="text-destructive">{error}</p>
{:else if policies.length === 0}
	<p class="text-muted-foreground">No policies configured.</p>
{:else}
	<div class="flex flex-col gap-3">
		{#each policies as policy}
			<Card.Root>
				<Card.Header>
					<div class="flex justify-between items-center">
						<Card.Title>{policy.name}</Card.Title>
						<span class="text-xs text-muted-foreground uppercase">{policy.status}</span>
					</div>
					{#if policy.description}
						<Card.Description>{policy.description}</Card.Description>
					{/if}
				</Card.Header>
				<Card.Content>
					{#each policy.rules as rule}
						<div class="px-3 py-2 mt-2 border border-border rounded-md text-sm">
							<div class="flex justify-between items-center">
								<span class="font-semibold uppercase" style="color: {effectColor(rule.effect)}">{rule.effect}</span>
								{#if rule.priority != null}
									<span class="text-muted-foreground text-xs">Priority: {rule.priority}</span>
								{/if}
							</div>
							{#if rule.description}
								<div class="text-muted-foreground mt-1">{rule.description}</div>
							{/if}
							{#if rule.conditions}
								<div class="mt-2">
									{#each rule.conditions as cond}
										<div class="font-mono text-xs text-muted-foreground">
											{cond.field} {cond.operator} {JSON.stringify(cond.value)}
										</div>
									{/each}
								</div>
							{/if}
						</div>
					{/each}
				</Card.Content>
			</Card.Root>
		{/each}
	</div>
{/if}
