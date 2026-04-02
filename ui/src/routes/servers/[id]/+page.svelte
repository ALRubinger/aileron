<script lang="ts">
	import { page } from '$app/stores';
	import { goto } from '$app/navigation';
	import { getMCPServer, deleteMCPServer, setMCPServerCredential } from '$lib/api';
	import { onMount } from 'svelte';
	import { serverStatusColor, modeColor } from '$lib/colors';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';

	let server = $state<any>(null);
	let loading = $state(true);
	let error = $state('');
	let deleting = $state(false);

	let credEnvVar = $state('');
	let credValue = $state('');
	let credSaving = $state(false);
	let credResult = $state('');

	let id = $derived($page.params.id ?? '');

	async function load() {
		try {
			server = await getMCPServer(id);
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		load();
	});

	async function handleSetCredential() {
		if (!credEnvVar.trim() || !credValue.trim()) return;
		credSaving = true;
		credResult = '';
		try {
			await setMCPServerCredential(id, credEnvVar.trim(), credValue);
			credResult = `Stored ${credEnvVar.trim()} in vault.`;
			credEnvVar = '';
			credValue = '';
			await load();
		} catch (e: any) {
			credResult = `Error: ${e.message}`;
		} finally {
			credSaving = false;
		}
	}

	async function handleDelete() {
		if (!confirm(`Remove server "${server?.name}"? This cannot be undone.`)) return;
		deleting = true;
		try {
			await deleteMCPServer(id);
			goto('/servers');
		} catch (e: any) {
			error = e.message;
			deleting = false;
		}
	}
</script>

<svelte:head>
	<title>{server?.name || 'Server'} - Aileron</title>
</svelte:head>

<a href="/servers" class="text-muted-foreground no-underline text-sm">&larr; Back to Servers</a>

{#if loading}
	<p class="text-muted-foreground mt-4">Loading...</p>
{:else if error}
	<p class="text-destructive mt-4">{error}</p>
{:else if server}
	<div class="flex items-center gap-3 mt-4 mb-6">
		<h1 class="text-xl font-semibold m-0">{server.name}</h1>
		{#if server.version}
			<span class="text-sm text-muted-foreground">v{server.version}</span>
		{/if}
		<span class="rounded border px-2 py-0.5 text-xs font-semibold uppercase" style="color: {serverStatusColor(server.status)}; border-color: {serverStatusColor(server.status)}">
			{server.status || 'stopped'}
		</span>
		<span class="rounded border px-2 py-0.5 text-xs font-semibold uppercase" style="color: {modeColor(server.mode)}; border-color: {modeColor(server.mode)}">
			{server.mode || 'local'}
		</span>
	</div>

	<Card.Root class="mb-4">
		<Card.Header>
			<Card.Title>Configuration</Card.Title>
		</Card.Header>
		<Card.Content>
			<div class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
				<span class="text-muted-foreground">ID</span>
				<span class="font-mono">{server.id}</span>

				{#if server.description}
					<span class="text-muted-foreground">Description</span>
					<span>{server.description}</span>
				{/if}

				<span class="text-muted-foreground">Command</span>
				<span class="font-mono">{(server.command || []).join(' ')}</span>

				<span class="text-muted-foreground">Mode</span>
				<span>{server.mode || 'local'}</span>

				{#if server.version}
					<span class="text-muted-foreground">Version</span>
					<span>{server.version}</span>
				{/if}

				{#if server.registry_id}
					<span class="text-muted-foreground">Registry ID</span>
					<span class="font-mono">{server.registry_id}</span>
				{/if}

				{#if server.policy_mapping?.tool_prefix}
					<span class="text-muted-foreground">Tool Prefix</span>
					<span class="font-mono">{server.policy_mapping.tool_prefix}</span>
				{/if}

				{#if server.created_at}
					<span class="text-muted-foreground">Created</span>
					<span>{new Date(server.created_at).toLocaleString()}</span>
				{/if}

				{#if server.updated_at}
					<span class="text-muted-foreground">Updated</span>
					<span>{new Date(server.updated_at).toLocaleString()}</span>
				{/if}
			</div>
		</Card.Content>
	</Card.Root>

	{#if server.env && Object.keys(server.env).length > 0}
		<Card.Root class="mb-4">
			<Card.Header>
				<Card.Title>Environment Variables</Card.Title>
			</Card.Header>
			<Card.Content>
				<div class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
					{#each Object.entries(server.env) as [key, value]}
						<span class="font-mono">{key}</span>
						<span class="font-mono text-muted-foreground">
							{#if typeof value === 'string' && value.startsWith('vault://')}
								<span title={String(value)}>&#128274; vault-backed</span>
							{:else}
								{value || '(empty)'}
							{/if}
						</span>
					{/each}
				</div>
			</Card.Content>
		</Card.Root>
	{/if}

	<Card.Root class="mb-4">
		<Card.Header>
			<Card.Title>Set Credential</Card.Title>
			<Card.Description>Store a secret in the vault for this server.</Card.Description>
		</Card.Header>
		<Card.Content>
			<div class="flex gap-2 items-end flex-wrap">
				<div>
					<label for="cred-env-var" class="block text-xs text-muted-foreground mb-1">Env var name</label>
					<Input
						id="cred-env-var"
						type="text"
						bind:value={credEnvVar}
						placeholder="e.g. API_KEY"
						class="font-mono"
					/>
				</div>
				<div>
					<label for="cred-secret" class="block text-xs text-muted-foreground mb-1">Secret value</label>
					<Input
						id="cred-secret"
						type="password"
						bind:value={credValue}
						placeholder="Enter secret..."
					/>
				</div>
				<Button
					onclick={handleSetCredential}
					disabled={credSaving}
				>
					{credSaving ? 'Saving...' : 'Save'}
				</Button>
			</div>
			{#if credResult}
				<p class="mt-2 text-xs" style="color: {credResult.startsWith('Error') ? 'var(--color-status-red)' : 'var(--color-status-green)'}">{credResult}</p>
			{/if}
		</Card.Content>
	</Card.Root>

	<Card.Root class="border-destructive">
		<Card.Header>
			<Card.Title class="text-destructive">Danger Zone</Card.Title>
		</Card.Header>
		<Card.Content>
			<Button
				variant="destructive"
				onclick={handleDelete}
				disabled={deleting}
			>
				{deleting ? 'Removing...' : 'Delete Server'}
			</Button>
		</Card.Content>
	</Card.Root>
{/if}
