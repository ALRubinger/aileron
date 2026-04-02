<script lang="ts">
	import { searchMarketplace, installMarketplaceServer, setMCPServerCredential } from '$lib/api';
	import { onMount } from 'svelte';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Badge } from '$lib/components/ui/badge';

	function clickOutside(node: HTMLElement, callback: () => void) {
		function handleClick(e: MouseEvent) {
			if (!node.contains(e.target as Node)) callback();
		}
		document.addEventListener('click', handleClick, true);
		return { destroy() { document.removeEventListener('click', handleClick, true); } };
	}

	let servers = $state<any[]>([]);
	let loading = $state(true);
	let error = $state('');
	let query = $state('');
	let installing = $state<string | null>(null);
	let installResult = $state<any>(null);
	let credentialValues = $state<Record<string, string>>({});
	let credError = $state('');
	let credSaving = $state(false);
	let debounceTimer: ReturnType<typeof setTimeout>;
	let selectedVersions = $state<Record<string, string>>({});
	let expandedInstall = $state<string | null>(null);

	async function load(q?: string) {
		try {
			const data = await searchMarketplace(q || undefined);
			servers = data.items || [];
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	function handleInput() {
		clearTimeout(debounceTimer);
		debounceTimer = setTimeout(() => load(query), 300);
	}

	async function handleInstall(registryId: string) {
		installing = registryId;
		expandedInstall = null;
		try {
			const result = await installMarketplaceServer(registryId);
			if (result.required_credentials?.length > 0) {
				installResult = result;
				credentialValues = {};
			} else {
				await load(query || undefined);
			}
		} catch (e: any) {
			error = e.message;
		} finally {
			installing = null;
		}
	}

	async function handleSaveCredentials() {
		credSaving = true;
		credError = '';
		try {
			for (const cred of installResult.required_credentials) {
				const val = credentialValues[cred.name];
				if (val) {
					await setMCPServerCredential(installResult.server.id, cred.name, val);
				}
			}
			installResult = null;
			credentialValues = {};
			await load(query || undefined);
		} catch (e: any) {
			credError = e.message;
		} finally {
			credSaving = false;
		}
	}

	function handleSkipCredentials() {
		installResult = null;
		credentialValues = {};
		load(query || undefined);
	}

	onMount(() => {
		load();
	});
</script>

<svelte:head>
	<title>Marketplace - Aileron</title>
</svelte:head>

<h1 class="text-xl font-semibold mb-4">Marketplace</h1>

<Input
	type="text"
	placeholder="Search servers..."
	bind:value={query}
	oninput={handleInput}
	class="mb-6"
/>

{#if installResult}
	<Card.Root class="mb-6 border-status-blue">
		<Card.Header>
			<Card.Title>Configure Credentials for {installResult.server.name}</Card.Title>
			<Card.Description>This server requires credentials to function. You can configure them now or later from the server detail page.</Card.Description>
		</Card.Header>
		<Card.Content>
			{#each installResult.required_credentials as cred}
				<div class="mb-3">
					<!-- svelte-ignore a11y_label_has_associated_control -->
					<label class="block text-sm font-semibold mb-1">
						{cred.name}
						{#if cred.required}<span class="text-destructive">*</span>{/if}
					</label>
					{#if cred.description}
						<div class="text-xs text-muted-foreground mb-1">{cred.description}</div>
					{/if}
					<Input
						type="password"
						bind:value={credentialValues[cred.name]}
						placeholder="Enter value..."
					/>
				</div>
			{/each}
			{#if credError}
				<p class="text-destructive text-sm mb-3">{credError}</p>
			{/if}
			<div class="flex gap-3 mt-2">
				<Button
					onclick={handleSaveCredentials}
					disabled={credSaving}
				>
					{credSaving ? 'Saving...' : 'Save Credentials'}
				</Button>
				<Button
					variant="outline"
					onclick={handleSkipCredentials}
				>
					Skip for now
				</Button>
			</div>
		</Card.Content>
	</Card.Root>
{/if}

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else if error}
	<p class="text-destructive">{error}</p>
{:else if servers.length === 0}
	<p class="text-muted-foreground">No servers found.{query ? ' Try a different search.' : ''}</p>
{:else}
	<div class="grid grid-cols-[repeat(auto-fill,minmax(280px,1fr))] gap-4">
		{#each servers as server}
			{@const versions = server.versions || []}
			{@const latestVersion = versions[0]?.version}
			{@const versionCount = versions.length}
			<Card.Root class="relative flex flex-col">
				<Card.Header class="flex-1">
					<Card.Title>{server.name}</Card.Title>
					{#if server.description}
						<Card.Description>{server.description}</Card.Description>
					{/if}
				</Card.Header>
				<Card.Content>
					<div class="flex items-center justify-between">
						<div>
							{#if !server.installed && versionCount > 1}
								<Button
									variant="outline"
									size="xs"
									onclick={() => { expandedInstall = expandedInstall === server.registry_id ? null : server.registry_id; if (!selectedVersions[server.registry_id]) selectedVersions[server.registry_id] = versions[0].version; }}
								>
									Select version
								</Button>
							{/if}
						</div>
						{#if server.installed}
							<Badge variant="outline" class="text-status-green border-status-green">Installed</Badge>
						{:else}
							<Button
								size="sm"
								onclick={() => handleInstall(server.registry_id)}
								disabled={installing === server.registry_id}
							>
								{installing === server.registry_id ? 'Installing...' : `Install v${selectedVersions[server.registry_id] || latestVersion}`}
							</Button>
						{/if}
					</div>
				</Card.Content>
				{#if expandedInstall === server.registry_id}
					<!-- svelte-ignore a11y_no_static_element_interactions -->
					<div
						use:clickOutside={() => { expandedInstall = null; }}
						class="absolute left-0 right-0 bottom-0 px-4 pb-4 pt-3 bg-card border border-border rounded-b-xl z-10"
					>
						<div class="flex gap-2 items-center">
							<select
								bind:value={selectedVersions[server.registry_id]}
								class="flex-1 h-8 rounded-lg border border-input bg-transparent px-2.5 py-1 text-sm text-foreground"
							>
								{#each versions as ver}
									<option value={ver.version}>v{ver.version}</option>
								{/each}
							</select>
							<Button
								size="sm"
								onclick={() => handleInstall(server.registry_id)}
								disabled={installing === server.registry_id}
							>
								Install
							</Button>
						</div>
					</div>
				{/if}
			</Card.Root>
		{/each}
	</div>
{/if}
