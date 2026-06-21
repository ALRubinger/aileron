<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { getHubSuite, type HubSuiteEntry } from '$lib/api';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import HubCompositeInstallModal from '$lib/components/HubCompositeInstallModal.svelte';

	// Detail page for a single Hub suite entry (#709). Same routing
	// pattern as the action detail page: `[fqn]` is URL-encoded in the
	// link and SvelteKit decodes once. Renders the member-action list
	// and the connectors_required closure so the operator can see what
	// the suite will install before clicking through.

	let entry = $state<HubSuiteEntry | null>(null);
	let loading = $state(true);
	let error = $state('');
	let installFqn = $state<string | null>(null);

	const fqn = $derived(page.params.fqn ?? '');

	onMount(async () => {
		try {
			entry = await getHubSuite(fqn);
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	});
</script>

<svelte:head>
	<title>{entry?.fqn ?? 'Suite'} — Hub — Aileron</title>
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
	<p class="text-muted-foreground">Loading suite…</p>
{:else if error}
	<p class="text-destructive" data-testid="hub-detail-error">{error}</p>
{:else if entry}
	<h1 class="mb-2 font-mono text-2xl font-bold break-all">{entry.fqn}</h1>
	<p class="mb-6 text-base text-muted-foreground">
		{entry.description}
	</p>

	<Card.Root class="mb-4">
		<Card.Header>
			<Card.Title>Suite metadata</Card.Title>
		</Card.Header>
		<Card.Content class="space-y-2 text-sm">
			<div>
				<span class="font-medium">Publisher:</span>
				<span class="ml-2 font-mono">{entry.publisher_github}</span>
			</div>
			{#if entry.category}
				<div>
					<span class="font-medium">Category:</span>
					<span class="ml-2">{entry.category}</span>
				</div>
			{/if}
			<div>
				<span class="font-medium">Member actions ({entry.member_actions.length}):</span>
				<ul
					class="mt-1 ml-2 list-disc pl-4"
					data-testid="hub-detail-members"
				>
					{#each entry.member_actions as action}
						<li>
							<a
								href="/hub/actions/{encodeURIComponent(action)}"
								class="font-mono underline-offset-2 hover:underline"
							>
								{action}
							</a>
						</li>
					{/each}
				</ul>
			</div>
			{#if entry.connectors_required && entry.connectors_required.length > 0}
				<div>
					<span class="font-medium">
						Connectors required ({entry.connectors_required.length}):
					</span>
					<ul
						class="mt-1 ml-2 list-disc pl-4"
						data-testid="hub-detail-connectors"
					>
						{#each entry.connectors_required as conn}
							<li>
								<a
									href="/hub/connectors/{encodeURIComponent(conn)}"
									class="font-mono underline-offset-2 hover:underline"
								>
									{conn}
								</a>
							</li>
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
		Install suite
	</Button>

	<HubCompositeInstallModal
		fqn={installFqn}
		kind={installFqn ? 'suite' : null}
		onClose={() => (installFqn = null)}
	/>
{/if}
