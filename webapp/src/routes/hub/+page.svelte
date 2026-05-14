<script lang="ts">
	import { listHubConnectors, type HubConnectorEntry } from '$lib/api';
	import { onMount } from 'svelte';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import HubInstallModal from '$lib/components/HubInstallModal.svelte';

	// Hub browsing page (ADR-0013, #488). Reads from the daemon's
	// `/v1/hub/connectors` endpoint; the daemon shallow-clones the
	// public `aileron-connectors-hub` repo per query (no persisted
	// cache, per #486).
	//
	// The page is the webapp half of the umbrella's acceptance
	// criteria 1-3: search by keyword, browse list with FQN +
	// description, click an entry to see fingerprint and install. The
	// install action opens HubInstallModal which carries the publisher
	// trust decision.

	let entries = $state<HubConnectorEntry[]>([]);
	let loading = $state(true);
	let error = $state('');
	let query = $state('');
	let selectedFQN = $state<string | null>(null);

	async function refresh(q: string) {
		loading = true;
		error = '';
		try {
			const resp = await listHubConnectors(q);
			entries = resp.connectors ?? [];
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
			entries = [];
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		void refresh('');
	});

	let searchHandle: ReturnType<typeof setTimeout> | undefined;
	function onSearchInput(value: string) {
		query = value;
		// Debounce — every keystroke would shallow-clone the Hub repo
		// daemon-side. 250ms aligns with the typing pause that humans
		// reach for an interactive search box.
		if (searchHandle !== undefined) clearTimeout(searchHandle);
		searchHandle = setTimeout(() => {
			void refresh(query);
		}, 250);
	}
</script>

<svelte:head>
	<title>Hub — Aileron</title>
</svelte:head>

<h1 class="mb-4 text-3xl font-extrabold tracking-tight">Connector Hub</h1>
<p class="ink-bleed mb-4 text-sm text-muted-foreground">
	Community-published connectors. Browse, search by keyword, and install with
	one click. The daemon writes publisher trust to your keyring per repo on
	confirm.
</p>

<div class="mb-4">
	<Input
		type="text"
		placeholder="Search by FQN or description…"
		value={query}
		oninput={(e) => onSearchInput((e.currentTarget as HTMLInputElement).value)}
		data-testid="hub-search-input"
	/>
</div>

{#if loading}
	<p class="text-muted-foreground">Loading Hub entries…</p>
{:else if error}
	<p class="text-destructive" data-testid="hub-list-error">{error}</p>
{:else if entries.length === 0}
	<p class="text-muted-foreground" data-testid="hub-empty-state">
		{query.trim()
			? `No Hub connectors match "${query.trim()}".`
			: 'No connectors published to the Hub.'}
	</p>
{:else}
	<div class="flex flex-col gap-3" data-testid="hub-list">
		{#each entries as entry (entry.fqn)}
			<Card.Root data-testid="hub-entry-card" data-fqn={entry.fqn}>
				<Card.Header>
					<div class="flex flex-wrap items-start justify-between gap-2">
						<div class="space-y-1">
							<Card.Title class="font-mono text-sm break-all">
								{entry.fqn}
							</Card.Title>
							<Card.Description>{entry.description}</Card.Description>
						</div>
						<Button
							variant="default"
							onclick={() => (selectedFQN = entry.fqn)}
							data-testid="hub-install-cta"
						>
							Install
						</Button>
					</div>
				</Card.Header>
				<Card.Content class="text-xs text-muted-foreground">
					<span>Publisher: {entry.publisher_github}</span>
					<span class="ml-3">Release pattern: <code>{entry.release_pattern}</code></span>
				</Card.Content>
			</Card.Root>
		{/each}
	</div>
{/if}

<HubInstallModal
	fqn={selectedFQN}
	onClose={() => (selectedFQN = null)}
/>
