<script lang="ts">
	import {
		listHubConnectors,
		listHubActions,
		listHubSuites,
		type HubConnectorEntry,
		type HubActionEntry,
		type HubSuiteEntry,
	} from '$lib/api';
	import { onMount } from 'svelte';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import HubInstallModal from '$lib/components/HubInstallModal.svelte';

	// Hub browsing page (ADR-0013, action-first amendment / #709). The
	// catalog has three entry types — Suites, Actions, Connectors — and
	// the default tab is Suites (intent-first browse). The Providers tab
	// preserves the original connector-only view from the pre-amendment
	// page so trust-first browsers and "show me everything for Slack"
	// flows keep working unchanged.
	//
	// Each tab queries its own daemon endpoint and renders a card list.
	// Search is per-tab — typing in the search input filters whichever
	// catalog is currently active. Per-tab daemon clones are fast
	// (shallow clone, no GitHub API; per #486).

	type Tab = 'suites' | 'actions' | 'providers';

	let activeTab = $state<Tab>('suites');
	let query = $state('');

	let suites = $state<HubSuiteEntry[]>([]);
	let actions = $state<HubActionEntry[]>([]);
	let connectors = $state<HubConnectorEntry[]>([]);
	let loading = $state(true);
	let error = $state('');
	let selectedFQN = $state<string | null>(null);

	async function refresh() {
		loading = true;
		error = '';
		const q = query.trim();
		try {
			if (activeTab === 'suites') {
				const resp = await listHubSuites(q || undefined);
				suites = resp.suites ?? [];
			} else if (activeTab === 'actions') {
				const resp = await listHubActions(q || undefined);
				actions = resp.actions ?? [];
			} else {
				const resp = await listHubConnectors(q || undefined);
				connectors = resp.connectors ?? [];
			}
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		void refresh();
	});

	let searchHandle: ReturnType<typeof setTimeout> | undefined;
	function onSearchInput(value: string) {
		query = value;
		// 250ms debounce — each query is a fresh shallow clone of the
		// Hub repo daemon-side, so we don't want a fetch per keystroke.
		if (searchHandle !== undefined) clearTimeout(searchHandle);
		searchHandle = setTimeout(() => {
			void refresh();
		}, 250);
	}

	function switchTab(tab: Tab) {
		if (tab === activeTab) return;
		activeTab = tab;
		// Re-fetch immediately; per-tab search state is preserved (the
		// same `query` string applies, since the daemon's filter is
		// substring-match on FQN + description in every catalog).
		void refresh();
	}

	function placeholderForTab(tab: Tab): string {
		switch (tab) {
			case 'suites':
				return 'Search suites by name or description…';
			case 'actions':
				return 'Search actions by name, description, or intent…';
			case 'providers':
				return 'Search providers by FQN or description…';
		}
	}

	function emptyStateMessage(tab: Tab, q: string): string {
		const trimmed = q.trim();
		if (tab === 'suites') {
			return trimmed
				? `No Hub suites match "${trimmed}".`
				: 'No suites published to the Hub yet.';
		}
		if (tab === 'actions') {
			return trimmed
				? `No Hub actions match "${trimmed}".`
				: 'No actions published to the Hub yet.';
		}
		return trimmed
			? `No Hub connectors match "${trimmed}".`
			: 'No connectors published to the Hub.';
	}
</script>

<svelte:head>
	<title>Hub — Aileron</title>
</svelte:head>

<h1 class="mb-4 text-3xl font-extrabold tracking-tight">Hub</h1>
<p class="ink-bleed mb-4 text-sm text-muted-foreground">
	Browse community-published suites, actions, and providers. The daemon
	writes publisher trust to your keyring per repo on confirm. Default
	view leads with intent — start at Suites if you're shopping for a
	capability; switch to Providers if you want to see everything from a
	specific publisher.
</p>

{#snippet tab(id: Tab, label: string, testid: string)}
	<button
		type="button"
		role="tab"
		aria-selected={activeTab === id}
		class="rounded-t-md border-b-2 border-transparent px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground {activeTab ===
		id
			? 'border-foreground text-foreground'
			: ''}"
		onclick={() => switchTab(id)}
		data-testid={testid}
	>
		{label}
	</button>
{/snippet}

<div class="mb-4 flex gap-2 border-b" data-testid="hub-tabs" role="tablist">
	{@render tab('suites', 'Suites', 'hub-tab-suites')}
	{@render tab('actions', 'Actions', 'hub-tab-actions')}
	{@render tab('providers', 'Providers', 'hub-tab-providers')}
</div>

<div class="mb-4">
	<Input
		type="text"
		placeholder={placeholderForTab(activeTab)}
		value={query}
		oninput={(e) => onSearchInput((e.currentTarget as HTMLInputElement).value)}
		data-testid="hub-search-input"
	/>
</div>

{#if loading}
	<p class="text-muted-foreground">Loading Hub entries…</p>
{:else if error}
	<p class="text-destructive" data-testid="hub-list-error">{error}</p>
{:else if activeTab === 'suites'}
	{#if suites.length === 0}
		<p class="text-muted-foreground" data-testid="hub-empty-state">
			{emptyStateMessage('suites', query)}
		</p>
	{:else}
		<div class="flex flex-col gap-3" data-testid="hub-list" data-tab="suites">
			{#each suites as entry (entry.fqn)}
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
								Install suite
							</Button>
						</div>
					</Card.Header>
					<Card.Content class="text-xs text-muted-foreground">
						<span>Publisher: {entry.publisher_github}</span>
						<span class="ml-3">
							Actions: {entry.member_actions?.length ?? 0}
						</span>
						{#if entry.category}
							<span class="ml-3">Category: {entry.category}</span>
						{/if}
					</Card.Content>
				</Card.Root>
			{/each}
		</div>
	{/if}
{:else if activeTab === 'actions'}
	{#if actions.length === 0}
		<p class="text-muted-foreground" data-testid="hub-empty-state">
			{emptyStateMessage('actions', query)}
		</p>
	{:else}
		<div class="flex flex-col gap-3" data-testid="hub-list" data-tab="actions">
			{#each actions as entry (entry.fqn)}
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
								Install action
							</Button>
						</div>
					</Card.Header>
					<Card.Content class="text-xs text-muted-foreground">
						<span>Publisher: {entry.publisher_github}</span>
						<span class="ml-3">
							Requires: <code>{entry.connector_fqn}</code>
						</span>
						{#if entry.category}
							<span class="ml-3">Category: {entry.category}</span>
						{/if}
						{#if entry.intents && entry.intents.length > 0}
							<div class="mt-1">
								<span>Intents:</span>
								{#each entry.intents as intent}
									<code class="ml-1">{intent}</code>
								{/each}
							</div>
						{/if}
					</Card.Content>
				</Card.Root>
			{/each}
		</div>
	{/if}
{:else}
	{#if connectors.length === 0}
		<p class="text-muted-foreground" data-testid="hub-empty-state">
			{emptyStateMessage('providers', query)}
		</p>
	{:else}
		<div class="flex flex-col gap-3" data-testid="hub-list" data-tab="providers">
			{#each connectors as entry (entry.fqn)}
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
{/if}

<HubInstallModal fqn={selectedFQN} onClose={() => (selectedFQN = null)} />
