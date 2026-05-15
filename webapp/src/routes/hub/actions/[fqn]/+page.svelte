<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { getHubAction, type HubActionEntry } from '$lib/api';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import HubCompositeInstallModal from '$lib/components/HubCompositeInstallModal.svelte';

	// Detail page for a single Hub action entry (#709). The route param
	// `[fqn]` carries the action FQN URL-encoded; SvelteKit decodes once
	// before handing it to us, so `page.params.fqn` is the canonical
	// string like `github://acme/conn-google/actions/draft-email`.
	//
	// Rendered alongside the connector dependency, intents, and category.
	// The Install button opens the existing HubInstallModal targeted at
	// the action's own FQN; the modal will fetch the composite action
	// install-decision when its composite extension lands.

	let entry = $state<HubActionEntry | null>(null);
	let loading = $state(true);
	let error = $state('');
	let installFqn = $state<string | null>(null);

	const fqn = $derived(page.params.fqn ?? '');

	onMount(async () => {
		try {
			entry = await getHubAction(fqn);
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	});
</script>

<svelte:head>
	<title>{entry?.fqn ?? 'Action'} — Hub — Aileron</title>
</svelte:head>

<div class="mb-4">
	<Button
		variant="ghost"
		onclick={() => goto('/hub')}
		data-testid="hub-detail-back"
	>
		← Back to Hub
	</Button>
</div>

{#if loading}
	<p class="text-muted-foreground">Loading action…</p>
{:else if error}
	<p class="text-destructive" data-testid="hub-detail-error">{error}</p>
{:else if entry}
	<h1 class="mb-2 font-mono text-2xl font-bold break-all">{entry.fqn}</h1>
	<p class="ink-bleed mb-6 text-base text-muted-foreground">
		{entry.description}
	</p>

	<Card.Root class="mb-4">
		<Card.Header>
			<Card.Title>Action metadata</Card.Title>
		</Card.Header>
		<Card.Content class="space-y-2 text-sm">
			<div>
				<span class="font-medium">Publisher:</span>
				<span class="ml-2 font-mono">{entry.publisher_github}</span>
			</div>
			<div>
				<span class="font-medium">Required connector:</span>
				<a
					href="/hub/connectors/{encodeURIComponent(entry.connector_fqn)}"
					class="ml-2 font-mono underline-offset-2 hover:underline"
					data-testid="hub-detail-connector-link"
				>
					{entry.connector_fqn}
				</a>
			</div>
			{#if entry.category}
				<div>
					<span class="font-medium">Category:</span>
					<span class="ml-2">{entry.category}</span>
				</div>
			{/if}
			{#if entry.intents && entry.intents.length > 0}
				<div>
					<span class="font-medium">Intents:</span>
					<ul
						class="mt-1 ml-2 list-disc pl-4"
						data-testid="hub-detail-intents"
					>
						{#each entry.intents as intent}
							<li><code>{intent}</code></li>
						{/each}
					</ul>
				</div>
			{/if}
		</Card.Content>
	</Card.Root>

	<Button
		variant="default"
		onclick={() => (installFqn = entry!.fqn)}
		data-testid="hub-detail-install-cta"
	>
		Install action
	</Button>

	<HubCompositeInstallModal
		fqn={installFqn}
		kind={installFqn ? 'action' : null}
		onClose={() => (installFqn = null)}
	/>
{/if}
