<script lang="ts">
	import { searchMarketplace, installMarketplaceServer, setMCPServerCredential } from '$lib/api';
	import { onMount } from 'svelte';

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

<h1 style="font-size: 1.3rem; margin-bottom: 1rem;">Marketplace</h1>

<input
	type="text"
	placeholder="Search servers..."
	bind:value={query}
	oninput={handleInput}
	style="width: 100%; padding: 0.6rem 0.75rem; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text); font-size: 0.9rem; margin-bottom: 1.5rem; box-sizing: border-box;"
/>

{#if installResult}
	<div style="padding: 1.25rem; border: 1px solid var(--accent); border-radius: 8px; background: var(--bg-card); margin-bottom: 1.5rem;">
		<div style="font-weight: 600; margin-bottom: 0.75rem;">Configure Credentials for {installResult.server.name}</div>
		<p style="font-size: 0.85rem; color: var(--text-muted); margin-bottom: 1rem;">This server requires credentials to function. You can configure them now or later from the server detail page.</p>
		{#each installResult.required_credentials as cred}
			<div style="margin-bottom: 0.75rem;">
				<!-- svelte-ignore a11y_label_has_associated_control -->
				<label style="display: block; font-size: 0.85rem; font-weight: 600; margin-bottom: 0.25rem;">
					{cred.name}
					{#if cred.required}<span style="color: var(--red);">*</span>{/if}
				</label>
				{#if cred.description}
					<div style="font-size: 0.8rem; color: var(--text-muted); margin-bottom: 0.25rem;">{cred.description}</div>
				{/if}
				<input
					type="password"
					bind:value={credentialValues[cred.name]}
					placeholder="Enter value..."
					style="width: 100%; padding: 0.5rem 0.6rem; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text); font-size: 0.85rem; box-sizing: border-box;"
				/>
			</div>
		{/each}
		{#if credError}
			<p style="color: var(--red); font-size: 0.85rem; margin-bottom: 0.75rem;">{credError}</p>
		{/if}
		<div style="display: flex; gap: 0.75rem; margin-top: 0.5rem;">
			<button
				onclick={handleSaveCredentials}
				disabled={credSaving}
				style="padding: 0.5rem 1rem; background: var(--accent); color: #fff; border: none; border-radius: 6px; cursor: pointer; font-size: 0.85rem;"
			>
				{credSaving ? 'Saving...' : 'Save Credentials'}
			</button>
			<button
				onclick={handleSkipCredentials}
				style="padding: 0.5rem 1rem; background: transparent; color: var(--text-muted); border: 1px solid var(--border); border-radius: 6px; cursor: pointer; font-size: 0.85rem;"
			>
				Skip for now
			</button>
		</div>
	</div>
{/if}

{#if loading}
	<p style="color: var(--text-muted);">Loading...</p>
{:else if error}
	<p style="color: var(--red);">{error}</p>
{:else if servers.length === 0}
	<p style="color: var(--text-muted);">No servers found.{query ? ' Try a different search.' : ''}</p>
{:else}
	<div style="display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 1rem;">
		{#each servers as server}
			{@const versions = server.versions || []}
			{@const latestVersion = versions[0]?.version}
			{@const versionCount = versions.length}
			<div style="padding: 1.25rem; border: 1px solid var(--border); border-radius: 8px; background: var(--bg-card); display: flex; flex-direction: column;">
				<div style="font-weight: 600; margin-bottom: 0.25rem;">{server.name}</div>
				{#if server.description}
					<div style="font-size: 0.85rem; color: var(--text-muted); margin-bottom: 0.75rem; flex: 1;">{server.description}</div>
				{/if}
				<div style="display: flex; align-items: center; justify-content: flex-end; gap: 0.4rem; margin-top: auto;">
					{#if server.installed}
						<span style="color: var(--green); border: 1px solid var(--green); border-radius: 4px; padding: 0.2rem 0.6rem; font-size: 0.8rem; font-weight: 600;">Installed</span>
					{:else}
						<button
							onclick={() => handleInstall(server.registry_id)}
							disabled={installing === server.registry_id}
							style="padding: 0.35rem 0.7rem; background: var(--accent); color: #fff; border: none; border-radius: 6px; cursor: pointer; font-size: 0.8rem; font-weight: 600; white-space: nowrap;"
						>
							{installing === server.registry_id ? 'Installing...' : `Install v${latestVersion}`}
						</button>
						{#if versionCount > 1}
							<button
								onclick={() => { expandedInstall = expandedInstall === server.registry_id ? null : server.registry_id; if (!selectedVersions[server.registry_id]) selectedVersions[server.registry_id] = versions[0].version; }}
								style="padding: 0.35rem 0.7rem; background: transparent; color: var(--text-muted); border: 1px solid var(--border); border-radius: 6px; cursor: pointer; font-size: 0.8rem;"
							>
								Select version
							</button>
						{/if}
					{/if}
				</div>
				{#if expandedInstall === server.registry_id}
					<div style="margin-top: 0.75rem; padding-top: 0.75rem; border-top: 1px solid var(--border);">
						<div style="display: flex; gap: 0.5rem; align-items: center;">
							<select
								bind:value={selectedVersions[server.registry_id]}
								style="flex: 1; padding: 0.4rem 0.5rem; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text); font-size: 0.8rem;"
							>
								{#each versions as ver}
									<option value={ver.version}>v{ver.version}</option>
								{/each}
							</select>
							<button
								onclick={() => handleInstall(server.registry_id)}
								disabled={installing === server.registry_id}
								style="padding: 0.4rem 0.75rem; background: var(--accent); color: #fff; border: none; border-radius: 6px; cursor: pointer; font-size: 0.8rem; font-weight: 600; white-space: nowrap;"
							>
								Install
							</button>
						</div>
					</div>
				{/if}
			</div>
		{/each}
	</div>
{/if}
