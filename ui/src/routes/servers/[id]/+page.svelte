<script lang="ts">
	import { page } from '$app/stores';
	import { goto } from '$app/navigation';
	import { getMCPServer, deleteMCPServer, setMCPServerCredential } from '$lib/api';
	import { onMount } from 'svelte';

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

<a href="/servers" style="color: var(--text-muted); text-decoration: none; font-size: 0.85rem;">&larr; Back to Servers</a>

{#if loading}
	<p style="color: var(--text-muted); margin-top: 1rem;">Loading...</p>
{:else if error}
	<p style="color: var(--red); margin-top: 1rem;">{error}</p>
{:else if server}
	<div style="display: flex; align-items: center; gap: 0.75rem; margin-top: 1rem; margin-bottom: 1.5rem;">
		<h1 style="font-size: 1.3rem; margin: 0;">{server.name}</h1>
		{#if server.version}
			<span style="font-size: 0.85rem; color: var(--text-muted);">v{server.version}</span>
		{/if}
		<span style="color: {statusColor(server.status)}; border: 1px solid {statusColor(server.status)}; border-radius: 4px; padding: 0.15rem 0.5rem; font-size: 0.75rem; font-weight: 600; text-transform: uppercase;">
			{server.status || 'stopped'}
		</span>
		<span style="color: {modeColor(server.mode)}; border: 1px solid {modeColor(server.mode)}; border-radius: 4px; padding: 0.15rem 0.5rem; font-size: 0.75rem; font-weight: 600; text-transform: uppercase;">
			{server.mode || 'local'}
		</span>
	</div>

	<!-- Configuration -->
	<div style="padding: 1rem; border: 1px solid var(--border); border-radius: 8px; background: var(--bg-card); margin-bottom: 1rem;">
		<div style="font-weight: 600; margin-bottom: 0.75rem;">Configuration</div>
		<div style="display: grid; grid-template-columns: auto 1fr; gap: 0.4rem 1rem; font-size: 0.85rem;">
			<span style="color: var(--text-muted);">ID</span>
			<span style="font-family: monospace;">{server.id}</span>

			{#if server.description}
				<span style="color: var(--text-muted);">Description</span>
				<span>{server.description}</span>
			{/if}

			<span style="color: var(--text-muted);">Command</span>
			<span style="font-family: monospace;">{(server.command || []).join(' ')}</span>

			<span style="color: var(--text-muted);">Mode</span>
			<span>{server.mode || 'local'}</span>

			{#if server.version}
				<span style="color: var(--text-muted);">Version</span>
				<span>{server.version}</span>
			{/if}

			{#if server.registry_id}
				<span style="color: var(--text-muted);">Registry ID</span>
				<span style="font-family: monospace;">{server.registry_id}</span>
			{/if}

			{#if server.policy_mapping?.tool_prefix}
				<span style="color: var(--text-muted);">Tool Prefix</span>
				<span style="font-family: monospace;">{server.policy_mapping.tool_prefix}</span>
			{/if}

			{#if server.created_at}
				<span style="color: var(--text-muted);">Created</span>
				<span>{new Date(server.created_at).toLocaleString()}</span>
			{/if}

			{#if server.updated_at}
				<span style="color: var(--text-muted);">Updated</span>
				<span>{new Date(server.updated_at).toLocaleString()}</span>
			{/if}
		</div>
	</div>

	<!-- Environment Variables -->
	{#if server.env && Object.keys(server.env).length > 0}
		<div style="padding: 1rem; border: 1px solid var(--border); border-radius: 8px; background: var(--bg-card); margin-bottom: 1rem;">
			<div style="font-weight: 600; margin-bottom: 0.75rem;">Environment Variables</div>
			<div style="display: grid; grid-template-columns: auto 1fr; gap: 0.4rem 1rem; font-size: 0.85rem;">
				{#each Object.entries(server.env) as [key, value]}
					<span style="font-family: monospace;">{key}</span>
					<span style="font-family: monospace; color: var(--text-muted);">
						{#if typeof value === 'string' && value.startsWith('vault://')}
							<span title={String(value)}>&#128274; vault-backed</span>
						{:else}
							{value || '(empty)'}
						{/if}
					</span>
				{/each}
			</div>
		</div>
	{/if}

	<!-- Set Credential -->
	<div style="padding: 1rem; border: 1px solid var(--border); border-radius: 8px; background: var(--bg-card); margin-bottom: 1rem;">
		<div style="font-weight: 600; margin-bottom: 0.75rem;">Set Credential</div>
		<p style="font-size: 0.8rem; color: var(--text-muted); margin-bottom: 0.75rem;">Store a secret in the vault for this server.</p>
		<div style="display: flex; gap: 0.5rem; align-items: flex-end; flex-wrap: wrap;">
			<div>
				<label for="cred-env-var" style="display: block; font-size: 0.8rem; color: var(--text-muted); margin-bottom: 0.2rem;">Env var name</label>
				<input
					id="cred-env-var"
					type="text"
					bind:value={credEnvVar}
					placeholder="e.g. API_KEY"
					style="padding: 0.45rem 0.6rem; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text); font-size: 0.85rem; font-family: monospace;"
				/>
			</div>
			<div>
				<label for="cred-secret" style="display: block; font-size: 0.8rem; color: var(--text-muted); margin-bottom: 0.2rem;">Secret value</label>
				<input
					id="cred-secret"
					type="password"
					bind:value={credValue}
					placeholder="Enter secret..."
					style="padding: 0.45rem 0.6rem; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text); font-size: 0.85rem;"
				/>
			</div>
			<button
				onclick={handleSetCredential}
				disabled={credSaving}
				style="padding: 0.45rem 0.85rem; background: var(--accent); color: #fff; border: none; border-radius: 6px; cursor: pointer; font-size: 0.85rem;"
			>
				{credSaving ? 'Saving...' : 'Save'}
			</button>
		</div>
		{#if credResult}
			<p style="margin-top: 0.5rem; font-size: 0.8rem; color: {credResult.startsWith('Error') ? 'var(--red)' : 'var(--green)'};">{credResult}</p>
		{/if}
	</div>

	<!-- Danger Zone -->
	<div style="padding: 1rem; border: 1px solid var(--red); border-radius: 8px; background: var(--bg-card);">
		<div style="font-weight: 600; margin-bottom: 0.5rem; color: var(--red);">Danger Zone</div>
		<button
			onclick={handleDelete}
			disabled={deleting}
			style="padding: 0.4rem 0.85rem; background: var(--red); color: #fff; border: none; border-radius: 6px; cursor: pointer; font-size: 0.85rem;"
		>
			{deleting ? 'Removing...' : 'Delete Server'}
		</button>
	</div>
{/if}
