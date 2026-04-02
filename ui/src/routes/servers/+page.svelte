<script lang="ts">
	import { listMCPServers, deleteMCPServer } from '$lib/api';
	import { onMount } from 'svelte';

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

	function statusColor(status: string) {
		switch (status) {
			case 'running': return 'var(--green)';
			case 'error': return 'var(--red)';
			default: return 'var(--text-muted)';
		}
	}

	function modeColor(mode: string) {
		return mode === 'remote' ? 'var(--orange)' : 'var(--accent)';
	}

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

<h1 style="font-size: 1.3rem; margin-bottom: 1rem;">Installed Servers</h1>

{#if loading}
	<p style="color: var(--text-muted);">Loading...</p>
{:else if error}
	<p style="color: var(--red);">{error}</p>
{:else if servers.length === 0}
	<p style="color: var(--text-muted);">No MCP servers installed. <a href="/marketplace" style="color: var(--accent);">Browse the marketplace</a> to get started.</p>
{:else}
	<div style="display: flex; flex-direction: column; gap: 0.75rem;">
		{#each servers as server}
			<a href="/servers/{server.id}" style="display: block; padding: 1rem; border: 1px solid var(--border); border-radius: 8px; text-decoration: none; color: var(--text); background: var(--bg-card);">
				<div style="display: flex; justify-content: space-between; align-items: center;">
					<div style="font-weight: 600;">{server.name}</div>
					<div style="display: flex; gap: 0.5rem; align-items: center;">
						<span style="color: {statusColor(server.status)}; border: 1px solid {statusColor(server.status)}; border-radius: 4px; padding: 0.15rem 0.5rem; font-size: 0.75rem; font-weight: 600; text-transform: uppercase;">
							{server.status || 'stopped'}
						</span>
						<span style="color: {modeColor(server.mode)}; border: 1px solid {modeColor(server.mode)}; border-radius: 4px; padding: 0.15rem 0.5rem; font-size: 0.75rem; font-weight: 600; text-transform: uppercase;">
							{server.mode || 'local'}
						</span>
					</div>
				</div>
				{#if server.description}
					<div style="margin-top: 0.4rem; font-size: 0.85rem; color: var(--text-muted);">{server.description}</div>
				{/if}
				<div style="margin-top: 0.5rem; display: flex; justify-content: space-between; align-items: center; font-size: 0.8rem; color: var(--text-muted);">
					<div>
						{#if server.registry_id}
							<span style="font-family: monospace;">From: {server.registry_id}</span>
						{/if}
						{#if server.created_at}
							<span style="margin-left: 0.75rem;">Created: {new Date(server.created_at).toLocaleDateString()}</span>
						{/if}
					</div>
					<button
						onclick={(e) => handleDelete(e, server.id, server.name)}
						disabled={deleting === server.id}
						style="padding: 0.25rem 0.6rem; background: transparent; color: var(--red); border: 1px solid var(--red); border-radius: 4px; cursor: pointer; font-size: 0.75rem;"
					>
						{deleting === server.id ? 'Removing...' : 'Remove'}
					</button>
				</div>
			</a>
		{/each}
	</div>
{/if}
