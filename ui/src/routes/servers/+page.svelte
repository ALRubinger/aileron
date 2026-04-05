<script lang="ts">
	import { listMCPServers, deleteMCPServer } from '$lib/api';
	import { onMount } from 'svelte';
	import { serverStatusColor, modeColor } from '$lib/colors';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';

	let servers = $state<any[]>([]);
	let loading = $state(true);
	let error = $state('');
	let deleting = $state<string | null>(null);

	async function load() {
		try {
			const data = await listMCPServers();
			servers = data.items || [];
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

	async function handleDelete(e: Event, id: string, name: string) {
		e.preventDefault();
		e.stopPropagation();
		if (!confirm(`Remove server "${name}"? This cannot be undone.`)) return;
		deleting = id;
		try {
			await deleteMCPServer(id);
			await load();
		} catch (err: any) {
			error = err.message;
		} finally {
			deleting = null;
		}
	}
</script>

<svelte:head>
	<title>Servers - Aileron</title>
</svelte:head>

<h1 class="text-xl font-semibold mb-4">Installed Servers</h1>

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else if error}
	<p class="text-destructive">{error}</p>
{:else if servers.length === 0}
	<p class="text-muted-foreground">No MCP servers installed. <a href="/marketplace" class="text-status-blue">Browse the marketplace</a> to get started.</p>
{:else}
	<div class="flex flex-col gap-3">
		{#each servers as server}
			<a href="/servers/{server.id}" class="no-underline text-foreground">
				<Card.Root class="hover:bg-muted/50 transition-colors">
					<Card.Header>
						<div class="flex justify-between items-center">
							<div>
								<span class="font-semibold">{server.name}</span>
								{#if server.version}
									<span class="text-xs text-muted-foreground ml-2">v{server.version}</span>
								{/if}
							</div>
							<div class="flex gap-2 items-center">
								{#if server.source === 'enterprise'}
									<span class="rounded border px-2 py-0.5 text-xs font-semibold uppercase border-purple-500 text-purple-500">
										Enterprise
									</span>
								{:else}
									<span class="rounded border px-2 py-0.5 text-xs font-semibold uppercase border-sky-500 text-sky-500">
										Personal
									</span>
								{/if}
								<span class="rounded border px-2 py-0.5 text-xs font-semibold uppercase" style="color: {serverStatusColor(server.status)}; border-color: {serverStatusColor(server.status)}">
									{server.status || 'stopped'}
								</span>
								<span class="rounded border px-2 py-0.5 text-xs font-semibold uppercase" style="color: {modeColor(server.mode)}; border-color: {modeColor(server.mode)}">
									{server.mode || 'local'}
								</span>
							</div>
						</div>
					</Card.Header>
					<Card.Content>
						{#if server.description}
							<div class="text-sm text-muted-foreground mb-2">{server.description}</div>
						{/if}
						<div class="flex justify-between items-center text-xs text-muted-foreground">
							<div>
								{#if server.registry_id}
									<span class="font-mono">From: {server.registry_id}</span>
								{/if}
								{#if server.created_at}
									<span class="ml-3">Created: {new Date(server.created_at).toLocaleDateString()}</span>
								{/if}
							</div>
							{#if server.source !== 'enterprise'}
								<Button
									variant="destructive"
									size="xs"
									onclick={(e: Event) => handleDelete(e, server.id, server.name)}
									disabled={deleting === server.id}
								>
									{deleting === server.id ? 'Removing...' : 'Remove'}
								</Button>
							{:else}
								<span class="text-xs text-muted-foreground italic">Managed by organization</span>
							{/if}
						</div>
					</Card.Content>
				</Card.Root>
			</a>
		{/each}
	</div>
{/if}
