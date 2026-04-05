<script lang="ts">
	import { onMount } from 'svelte';
	import {
		listEnterpriseMCPServers,
		createEnterpriseMCPServer,
		deleteEnterpriseMCPServer,
		updateEnterpriseMCPServer
	} from '$lib/api';
	import { getUser } from '$lib/auth.svelte.js';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Button } from '$lib/components/ui/button';

	let servers = $state<any[]>([]);
	let loading = $state(true);
	let error = $state('');
	let deleting = $state<string | null>(null);

	let showAddForm = $state(false);
	let newName = $state('');
	let newCommand = $state('');
	let newDescription = $state('');
	let newAutoEnabled = $state(false);
	let adding = $state(false);

	const canEdit = $derived(
		(() => {
			const user = getUser();
			return user?.role === 'owner' || user?.role === 'admin';
		})()
	);

	async function load() {
		try {
			const data = await listEnterpriseMCPServers();
			servers = data.items || [];
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		load();
	});

	async function handleAdd() {
		if (!newName.trim() || !newCommand.trim()) return;
		adding = true;
		error = '';
		try {
			const command = newCommand
				.trim()
				.split(/\s+/)
				.filter((s: string) => s);
			await createEnterpriseMCPServer({
				name: newName.trim(),
				command,
				description: newDescription.trim() || undefined,
				auto_enabled: newAutoEnabled
			});
			newName = '';
			newCommand = '';
			newDescription = '';
			newAutoEnabled = false;
			showAddForm = false;
			await load();
		} catch (e: any) {
			error = e.message;
		} finally {
			adding = false;
		}
	}

	async function handleDelete(id: string, name: string) {
		if (!confirm(`Remove "${name}" from the enterprise approved list?`)) return;
		deleting = id;
		try {
			await deleteEnterpriseMCPServer(id);
			await load();
		} catch (e: any) {
			error = e.message;
		} finally {
			deleting = null;
		}
	}

	async function handleToggleAutoEnabled(server: any) {
		try {
			await updateEnterpriseMCPServer(server.id, {
				name: server.name,
				command: server.command,
				description: server.description,
				env: server.env,
				mode: server.mode,
				auto_enabled: !server.auto_enabled
			});
			await load();
		} catch (e: any) {
			error = e.message;
		}
	}
</script>

<svelte:head>
	<title>MCP Servers - Settings - Aileron</title>
</svelte:head>

<div class="flex justify-between items-center mb-4">
	<div>
		<h2 class="text-lg font-semibold">Enterprise MCP Servers</h2>
		<p class="text-sm text-muted-foreground">
			Manage the approved MCP server list for your organization. Auto-enabled servers are
			automatically active for all members.
		</p>
	</div>
	{#if canEdit}
		<Button size="sm" onclick={() => (showAddForm = !showAddForm)}>
			{showAddForm ? 'Cancel' : 'Add Server'}
		</Button>
	{/if}
</div>

{#if error}
	<p class="text-destructive text-sm mb-4">{error}</p>
{/if}

{#if showAddForm && canEdit}
	<Card.Root class="mb-4">
		<Card.Header>
			<Card.Title>Add Enterprise Server</Card.Title>
		</Card.Header>
		<Card.Content>
			<div class="flex flex-col gap-3 max-w-lg">
				<div>
					<label for="new-name" class="block text-xs text-muted-foreground mb-1">Name</label>
					<Input id="new-name" bind:value={newName} placeholder="e.g. filesystem" />
				</div>
				<div>
					<label for="new-command" class="block text-xs text-muted-foreground mb-1"
						>Command</label
					>
					<Input
						id="new-command"
						bind:value={newCommand}
						placeholder="e.g. npx -y @modelcontextprotocol/server-filesystem"
						class="font-mono"
					/>
				</div>
				<div>
					<label for="new-description" class="block text-xs text-muted-foreground mb-1"
						>Description (optional)</label
					>
					<Input id="new-description" bind:value={newDescription} placeholder="What this server does" />
				</div>
				<label class="flex items-center gap-2">
					<input type="checkbox" bind:checked={newAutoEnabled} class="h-4 w-4 rounded border-border" />
					<span class="text-sm">Auto-enable for all members</span>
				</label>
				<div>
					<Button onclick={handleAdd} disabled={adding} size="sm">
						{adding ? 'Adding...' : 'Add Server'}
					</Button>
				</div>
			</div>
		</Card.Content>
	</Card.Root>
{/if}

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else if servers.length === 0}
	<p class="text-muted-foreground">
		No enterprise MCP servers configured.
		{#if canEdit}
			Click "Add Server" to get started.
		{/if}
	</p>
{:else}
	<div class="flex flex-col gap-3">
		{#each servers as server}
			<Card.Root>
				<Card.Header>
					<div class="flex justify-between items-center">
						<div>
							<span class="font-semibold">{server.name}</span>
							{#if server.version}
								<span class="text-xs text-muted-foreground ml-2">v{server.version}</span>
							{/if}
						</div>
						<div class="flex gap-2 items-center">
							{#if server.auto_enabled}
								<span
									class="rounded border px-2 py-0.5 text-xs font-semibold uppercase border-green-500 text-green-500"
								>
									Auto-enabled
								</span>
							{:else}
								<span
									class="rounded border px-2 py-0.5 text-xs font-semibold uppercase border-muted-foreground text-muted-foreground"
								>
									Approved
								</span>
							{/if}
						</div>
					</div>
				</Card.Header>
				<Card.Content>
					{#if server.description}
						<p class="text-sm text-muted-foreground mb-2">{server.description}</p>
					{/if}
					<p class="text-xs font-mono text-muted-foreground mb-3">
						{(server.command || []).join(' ')}
					</p>
					{#if canEdit}
						<div class="flex gap-2 items-center">
							<Button
								variant="outline"
								size="xs"
								onclick={() => handleToggleAutoEnabled(server)}
							>
								{server.auto_enabled ? 'Disable auto-enable' : 'Auto-enable for all'}
							</Button>
							<Button
								variant="destructive"
								size="xs"
								onclick={() => handleDelete(server.id, server.name)}
								disabled={deleting === server.id}
							>
								{deleting === server.id ? 'Removing...' : 'Remove'}
							</Button>
						</div>
					{/if}
				</Card.Content>
			</Card.Root>
		{/each}
	</div>
{/if}
